package externalmodel

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	finalizerName = "maas.opendatahub.io/external-model-cleanup"

	// Annotation keys for ExternalModel spec (until CRD is updated)
	AnnProvider     = "maas.opendatahub.io/provider"
	AnnEndpoint     = "maas.opendatahub.io/endpoint"
	AnnExtraHeaders = "maas.opendatahub.io/extra-headers"
	AnnPort         = "maas.opendatahub.io/port"
	AnnTLS          = "maas.opendatahub.io/tls"
	AnnPathPrefix   = "maas.opendatahub.io/path-prefix"

	// MaaSModelRef GVK
	maasGroup    = "maas.opendatahub.io"
	maasVersion  = "v1alpha1"
	maasResource = "maasmodelrefs"
	maasKind     = "MaaSModelRef"

	// Default gateway (matches MaaS controller defaults)
	defaultGatewayName      = "maas-default-gateway"
	defaultGatewayNamespace = "openshift-ingress"
)

// Reconciler watches MaaSModelRef CRs with kind=ExternalModel and creates
// the Istio resources needed to route to the external provider.
//
// Resources are created in the gateway namespace (typically openshift-ingress),
// which is a different namespace from the MaaSModelRef. Because Kubernetes
// garbage collection does not work across namespaces, the reconciler uses a
// finalizer to explicitly delete managed resources when the CR is removed.
type Reconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Log              logr.Logger
	GatewayName      string
	GatewayNamespace string
}

func (r *Reconciler) gatewayName() string {
	if r.GatewayName != "" {
		return r.GatewayName
	}
	return defaultGatewayName
}

func (r *Reconciler) gatewayNamespace() string {
	if r.GatewayNamespace != "" {
		return r.GatewayNamespace
	}
	return defaultGatewayNamespace
}

// Reconcile handles create/update/delete of MaaSModelRef CRs with kind=ExternalModel.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("maasmodelref", req.NamespacedName)

	// Fetch the MaaSModelRef (unstructured since we don't import the MaaS types)
	model := &unstructured.Unstructured{}
	model.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   maasGroup,
		Version: maasVersion,
		Kind:    maasKind,
	})
	if err := r.Get(ctx, req.NamespacedName, model); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Only handle ExternalModel kind
	kind, _, _ := unstructured.NestedString(model.Object, "spec", "modelRef", "kind")
	if kind != "ExternalModel" {
		return ctrl.Result{}, nil
	}

	// Handle deletion — must explicitly delete cross-namespace resources
	if !model.GetDeletionTimestamp().IsZero() {
		return r.handleDeletion(ctx, log, model)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(model, finalizerName) {
		controllerutil.AddFinalizer(model, finalizerName)
		if err := r.Update(ctx, model); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Parse the ExternalModel spec from annotations (until CRD is enriched)
	spec, err := specFromAnnotations(model)
	if err != nil {
		log.Error(err, "Failed to parse ExternalModel spec from annotations")
		return r.setStatus(ctx, model, "Failed", fmt.Sprintf("invalid spec: %v", err))
	}

	log.Info("Reconciling ExternalModel",
		"provider", spec.Provider,
		"endpoint", spec.Endpoint,
		"tls", spec.TLS,
	)

	gwNamespace := r.gatewayNamespace()
	gwName := r.gatewayName()
	labels := CommonLabels(model.GetName())
	resourceKey := spec.Provider + "-" + sanitize(spec.Endpoint)

	// NOTE: No OwnerReferences on these resources. The MaaSModelRef lives in a
	// different namespace (e.g. opendatahub) than the gateway namespace (e.g.
	// openshift-ingress). Kubernetes GC does not follow cross-namespace
	// OwnerReferences. Instead, the finalizer on the MaaSModelRef triggers
	// explicit deletion in handleDeletion().

	// 1. ExternalName Service
	svc := BuildExternalNameService(spec, gwNamespace, labels)
	if err := r.applyService(ctx, log, svc); err != nil {
		return r.setStatus(ctx, model, "Failed", fmt.Sprintf("failed to create Service: %v", err))
	}

	// 2. ServiceEntry
	se := BuildServiceEntry(spec, gwNamespace, labels)
	if err := r.applyUnstructured(ctx, log, se); err != nil {
		return r.setStatus(ctx, model, "Failed", fmt.Sprintf("failed to create ServiceEntry: %v", err))
	}

	// 3. DestinationRule (only if TLS; delete stale DR when TLS is disabled)
	if spec.TLS {
		dr := BuildDestinationRule(spec, gwNamespace, labels)
		if err := r.applyUnstructured(ctx, log, dr); err != nil {
			return r.setStatus(ctx, model, "Failed", fmt.Sprintf("failed to create DestinationRule: %v", err))
		}
	} else {
		// Clean up any stale DestinationRule from when TLS was previously enabled
		_, _, drName, _ := ResourceNames(resourceKey)
		if err := r.deleteIfExists(ctx, log, "DestinationRule", drName, gwNamespace, schema.GroupVersionKind{
			Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule",
		}); err != nil {
			log.Error(err, "Failed to delete stale DestinationRule", "name", drName)
		}
	}

	// 4. HTTPRoute
	hr := BuildHTTPRoute(spec, gwNamespace, gwName, gwNamespace, labels)
	if err := r.applyHTTPRoute(ctx, log, hr); err != nil {
		return r.setStatus(ctx, model, "Failed", fmt.Sprintf("failed to create HTTPRoute: %v", err))
	}

	log.Info("ExternalModel resources reconciled successfully",
		"service", svc.Name,
		"serviceEntry", se.GetName(),
		"httpRoute", hr.Name,
	)

	// Build endpoint URL from the MaaS gateway (not the provider URL).
	// MaaS clients discover models via status.endpoint which must point to
	// the MaaS gateway, not directly to the external provider.
	endpoint, err := r.resolveGatewayEndpoint(ctx, gwName, gwNamespace, spec.Provider)
	if err != nil {
		log.Error(err, "Failed to resolve gateway endpoint, using fallback")
		endpoint = fmt.Sprintf("/external/%s/", spec.Provider)
	}

	return r.setStatusReady(ctx, model, endpoint, hr.Name, gwName, gwNamespace)
}

// resolveGatewayEndpoint reads the Gateway resource to get the listener hostname
// and constructs the MaaS-facing endpoint URL.
func (r *Reconciler) resolveGatewayEndpoint(ctx context.Context, gwName, gwNamespace, provider string) (string, error) {
	gw := &gatewayapiv1.Gateway{}
	if err := r.Get(ctx, types.NamespacedName{Name: gwName, Namespace: gwNamespace}, gw); err != nil {
		return "", fmt.Errorf("failed to get Gateway %s/%s: %w", gwNamespace, gwName, err)
	}

	// Use the first listener hostname
	for _, listener := range gw.Spec.Listeners {
		if listener.Hostname != nil && *listener.Hostname != "" {
			scheme := "https"
			if listener.Protocol == gatewayapiv1.HTTPProtocolType {
				scheme = "http"
			}
			return fmt.Sprintf("%s://%s/external/%s/", scheme, *listener.Hostname, provider), nil
		}
	}

	// Fallback: check status addresses
	for _, addr := range gw.Status.Addresses {
		if addr.Value != "" {
			return fmt.Sprintf("http://%s/external/%s/", addr.Value, provider), nil
		}
	}

	return "", fmt.Errorf("no hostname or address found on Gateway %s/%s", gwNamespace, gwName)
}

// handleDeletion explicitly deletes all managed resources in the gateway
// namespace, then removes the finalizer only if all deletions succeed.
// This prevents orphaned resources when deletion fails (e.g., network
// partition, RBAC issue). The controller will requeue and retry.
func (r *Reconciler) handleDeletion(ctx context.Context, log logr.Logger, model *unstructured.Unstructured) (ctrl.Result, error) {
	log.Info("Handling deletion of ExternalModel, cleaning up cross-namespace resources")

	spec, err := specFromAnnotations(model)
	if err != nil {
		log.Error(err, "Failed to parse spec for cleanup, removing finalizer anyway")
	} else {
		gwNamespace := r.gatewayNamespace()
		resourceKey := spec.Provider + "-" + sanitize(spec.Endpoint)
		svcName, seName, drName, hrName := ResourceNames(resourceKey)

		var cleanupErrs []error

		// Delete in reverse creation order
		if err := r.deleteIfExists(ctx, log, "HTTPRoute", hrName, gwNamespace, schema.GroupVersionKind{
			Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute",
		}); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
		if err := r.deleteIfExists(ctx, log, "DestinationRule", drName, gwNamespace, schema.GroupVersionKind{
			Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule",
		}); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
		if err := r.deleteIfExists(ctx, log, "ServiceEntry", seName, gwNamespace, schema.GroupVersionKind{
			Group: "networking.istio.io", Version: "v1", Kind: "ServiceEntry",
		}); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
		// Delete ExternalName Service
		svc := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{Name: svcName, Namespace: gwNamespace}, svc); err == nil {
			log.Info("Deleting Service", "name", svcName)
			if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to delete Service", "name", svcName)
				cleanupErrs = append(cleanupErrs, fmt.Errorf("failed to delete Service %s/%s: %w", gwNamespace, svcName, err))
			}
		} else if !apierrors.IsNotFound(err) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("failed to get Service %s/%s: %w", gwNamespace, svcName, err))
		}

		if len(cleanupErrs) > 0 {
			// Requeue — do NOT remove finalizer so cleanup is retried
			return ctrl.Result{}, fmt.Errorf("cleanup incomplete, will retry: %v", cleanupErrs)
		}
	}

	if controllerutil.ContainsFinalizer(model, finalizerName) {
		controllerutil.RemoveFinalizer(model, finalizerName)
		if err := r.Update(ctx, model); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// deleteIfExists deletes an unstructured resource if it exists.
// Returns an error only for real failures; NotFound is treated as success.
func (r *Reconciler) deleteIfExists(ctx context.Context, log logr.Logger, kind, name, namespace string, gvk schema.GroupVersionKind) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get %s %s/%s: %w", kind, namespace, name, err)
	}
	log.Info("Deleting resource", "kind", kind, "name", name)
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "Failed to delete resource", "kind", kind, "name", name)
		return fmt.Errorf("failed to delete %s %s/%s: %w", kind, namespace, name, err)
	}
	return nil
}

// applyService creates or updates a Service.
func (r *Reconciler) applyService(ctx context.Context, log logr.Logger, desired *corev1.Service) error {
	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		log.Info("Creating Service", "name", desired.Name)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		existing.Spec = desired.Spec
		existing.Labels = desired.Labels
		log.Info("Updating Service", "name", desired.Name)
		return r.Update(ctx, existing)
	}
	return nil
}

// applyUnstructured creates or updates an unstructured resource (ServiceEntry, DestinationRule).
func (r *Reconciler) applyUnstructured(ctx context.Context, log logr.Logger, desired *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err := r.Get(ctx, types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()}, existing)
	if apierrors.IsNotFound(err) {
		log.Info("Creating resource", "kind", desired.GetKind(), "name", desired.GetName())
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	log.Info("Updating resource", "kind", desired.GetKind(), "name", desired.GetName())
	return r.Update(ctx, desired)
}

// applyHTTPRoute creates or updates an HTTPRoute.
func (r *Reconciler) applyHTTPRoute(ctx context.Context, log logr.Logger, desired *gatewayapiv1.HTTPRoute) error {
	existing := &gatewayapiv1.HTTPRoute{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		log.Info("Creating HTTPRoute", "name", desired.Name)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	log.Info("Updating HTTPRoute", "name", desired.Name)
	return r.Update(ctx, existing)
}

// setStatus updates the MaaSModelRef status phase and conditions.
func (r *Reconciler) setStatus(ctx context.Context, model *unstructured.Unstructured, phase, message string) (ctrl.Result, error) {
	status, _ := model.Object["status"].(map[string]interface{})
	if status == nil {
		status = map[string]interface{}{}
	}
	status["phase"] = phase
	now := metav1.Now().Format("2006-01-02T15:04:05Z")
	status["conditions"] = []interface{}{
		map[string]interface{}{
			"type":               "Ready",
			"status":             "False",
			"lastTransitionTime": now,
			"reason":             phase,
			"message":            message,
		},
	}
	model.Object["status"] = status

	if err := r.Status().Update(ctx, model); err != nil {
		r.Log.Error(err, "Failed to update status", "phase", phase)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// setStatusReady updates the MaaSModelRef status to Ready with the MaaS gateway endpoint.
func (r *Reconciler) setStatusReady(ctx context.Context, model *unstructured.Unstructured, endpoint, httpRouteName, gwName, gwNamespace string) (ctrl.Result, error) {
	status, _ := model.Object["status"].(map[string]interface{})
	if status == nil {
		status = map[string]interface{}{}
	}
	status["phase"] = "Ready"
	status["endpoint"] = endpoint
	status["httpRouteName"] = httpRouteName
	status["httpRouteGatewayName"] = gwName
	status["httpRouteGatewayNamespace"] = gwNamespace
	now := metav1.Now().Format("2006-01-02T15:04:05Z")
	status["conditions"] = []interface{}{
		map[string]interface{}{
			"type":               "Ready",
			"status":             "True",
			"lastTransitionTime": now,
			"reason":             "Reconciled",
			"message":            "Istio resources created successfully",
		},
	}
	model.Object["status"] = status

	if err := r.Status().Update(ctx, model); err != nil {
		r.Log.Error(err, "Failed to update status to Ready")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// specFromAnnotations parses ExternalModelSpec from MaaSModelRef annotations.
// This is a bridge until the CRD is enriched with the required fields.
func specFromAnnotations(model *unstructured.Unstructured) (ExternalModelSpec, error) {
	ann := model.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}

	spec := ExternalModelSpec{
		Provider:   ann[AnnProvider],
		Endpoint:   ann[AnnEndpoint],
		PathPrefix: ann[AnnPathPrefix],
		TLS:        true,
		Port:       443,
	}

	// Fall back to modelRef.name for provider if annotation not set
	if spec.Provider == "" {
		name, _, _ := unstructured.NestedString(model.Object, "spec", "modelRef", "name")
		spec.Provider = name
	}

	if spec.Endpoint == "" {
		return spec, fmt.Errorf("annotation %s is required", AnnEndpoint)
	}

	// Parse port with range validation
	if portStr, ok := ann[AnnPort]; ok {
		p, err := strconv.ParseInt(portStr, 10, 32)
		if err != nil {
			return spec, fmt.Errorf("invalid port %q: %v", portStr, err)
		}
		if p < 1 || p > 65535 {
			return spec, fmt.Errorf("port %d out of range (1-65535)", p)
		}
		spec.Port = int32(p)
	}

	// Parse TLS
	if tlsStr, ok := ann[AnnTLS]; ok {
		parsed, err := strconv.ParseBool(tlsStr)
		if err != nil {
			return spec, fmt.Errorf("invalid tls value %q: %v", tlsStr, err)
		}
		spec.TLS = parsed
	}

	// Parse extra headers (format: "key1=value1,key2=value2")
	if extraStr, ok := ann[AnnExtraHeaders]; ok && extraStr != "" {
		spec.ExtraHeaders = map[string]string{}
		for _, pair := range strings.Split(extraStr, ",") {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) == 2 {
				spec.ExtraHeaders[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
			}
		}
	}

	return spec, nil
}

// SetupWithManager registers the reconciler to watch MaaSModelRef CRs.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	modelRef := &unstructured.Unstructured{}
	modelRef.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   maasGroup,
		Version: maasVersion,
		Kind:    maasKind,
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(modelRef).
		Named("external-model-reconciler").
		Complete(r)
}
