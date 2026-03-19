package externalmodel

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// BuildExternalNameService creates a Kubernetes ExternalName Service that maps
// an in-cluster DNS name to the external FQDN. This allows HTTPRoute backendRefs
// to reference external hosts via standard k8s Service names.
//
// No OwnerReferences are set because the MaaSModelRef lives in a different
// namespace. Cleanup is handled by the finalizer in the reconciler.
func BuildExternalNameService(spec ExternalModelSpec, namespace string, labels map[string]string) *corev1.Service {
	svcName, _, _, _ := ResourceNames(spec.Provider + "-" + sanitize(spec.Endpoint))
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: spec.Endpoint,
			Ports: []corev1.ServicePort{
				{
					Port:       spec.Port,
					TargetPort: intstr.FromInt32(spec.Port),
				},
			},
		},
	}
}

// BuildServiceEntry creates an Istio ServiceEntry that registers the external
// FQDN in the mesh service registry. Required when outboundTrafficPolicy is
// REGISTRY_ONLY.
func BuildServiceEntry(spec ExternalModelSpec, namespace string, labels map[string]string) *unstructured.Unstructured {
	_, seName, _, _ := ResourceNames(spec.Provider + "-" + sanitize(spec.Endpoint))

	protocol := "HTTPS"
	portName := "https"
	if !spec.TLS {
		protocol = "HTTP"
		portName = "http"
	}

	se := &unstructured.Unstructured{}
	se.SetAPIVersion("networking.istio.io/v1")
	se.SetKind("ServiceEntry")
	se.SetName(seName)
	se.SetNamespace(namespace)
	se.SetLabels(labels)

	se.Object["spec"] = map[string]interface{}{
		"hosts":      []interface{}{spec.Endpoint},
		"location":   "MESH_EXTERNAL",
		"resolution": "DNS",
		"ports": []interface{}{
			map[string]interface{}{
				"number":   int64(spec.Port),
				"name":     portName,
				"protocol": protocol,
			},
		},
	}
	return se
}

// BuildDestinationRule creates an Istio DestinationRule that configures TLS
// origination for the external host. Skipped when TLS is false.
func BuildDestinationRule(spec ExternalModelSpec, namespace string, labels map[string]string) *unstructured.Unstructured {
	_, _, drName, _ := ResourceNames(spec.Provider + "-" + sanitize(spec.Endpoint))

	dr := &unstructured.Unstructured{}
	dr.SetAPIVersion("networking.istio.io/v1")
	dr.SetKind("DestinationRule")
	dr.SetName(drName)
	dr.SetNamespace(namespace)
	dr.SetLabels(labels)

	dr.Object["spec"] = map[string]interface{}{
		"host": spec.Endpoint,
		"trafficPolicy": map[string]interface{}{
			"tls": map[string]interface{}{
				"mode": "SIMPLE",
			},
		},
	}
	return dr
}

// BuildHTTPRoute creates a Gateway API HTTPRoute that routes requests matching
// the path prefix to the ExternalName Service backend and sets the Host header.
func BuildHTTPRoute(spec ExternalModelSpec, namespace, gatewayName, gatewayNamespace string, labels map[string]string) *gatewayapiv1.HTTPRoute {
	svcName, _, _, hrName := ResourceNames(spec.Provider + "-" + sanitize(spec.Endpoint))

	pathPrefix := spec.PathPrefix
	if pathPrefix == "" {
		// Sanitize provider for use in URL path to prevent path traversal
		safeProvider := sanitize(spec.Provider)
		pathPrefix = "/external/" + safeProvider + "/"
	}

	gwNamespace := gatewayapiv1.Namespace(gatewayNamespace)
	pathType := gatewayapiv1.PathMatchPathPrefix
	port := gatewayapiv1.PortNumber(spec.Port)

	// Build header modifiers
	headers := []gatewayapiv1.HTTPHeader{
		{
			Name:  "Host",
			Value: spec.Endpoint,
		},
	}
	for k, v := range spec.ExtraHeaders {
		headers = append(headers, gatewayapiv1.HTTPHeader{
			Name:  gatewayapiv1.HTTPHeaderName(k),
			Value: v,
		})
	}

	// Build filters
	filters := []gatewayapiv1.HTTPRouteFilter{
		{
			Type: gatewayapiv1.HTTPRouteFilterURLRewrite,
			URLRewrite: &gatewayapiv1.HTTPURLRewriteFilter{
				Path: &gatewayapiv1.HTTPPathModifier{
					Type:               gatewayapiv1.PrefixMatchHTTPPathModifier,
					ReplacePrefixMatch: strPtr("/"),
				},
			},
		},
		{
			Type: gatewayapiv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayapiv1.HTTPHeaderFilter{
				Set: headers,
			},
		},
	}

	timeout := gatewayapiv1.Duration("300s")

	return &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hrName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{
					{
						Name:      gatewayapiv1.ObjectName(gatewayName),
						Namespace: &gwNamespace,
					},
				},
			},
			Rules: []gatewayapiv1.HTTPRouteRule{
				{
					Matches: []gatewayapiv1.HTTPRouteMatch{
						{
							Path: &gatewayapiv1.HTTPPathMatch{
								Type:  &pathType,
								Value: &pathPrefix,
							},
						},
					},
					BackendRefs: []gatewayapiv1.HTTPBackendRef{
						{
							BackendRef: gatewayapiv1.BackendRef{
								BackendObjectReference: gatewayapiv1.BackendObjectReference{
									Name: gatewayapiv1.ObjectName(svcName),
									Port: &port,
								},
							},
						},
					},
					Filters:  filters,
					Timeouts: &gatewayapiv1.HTTPRouteTimeouts{Request: &timeout},
				},
			},
		},
	}
}

func sanitize(s string) string {
	// Convert to lowercase and replace non-alphanumeric characters with dashes
	// for RFC 1123 DNS label compatibility.
	var result []byte
	for _, c := range []byte(strings.ToLower(s)) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result = append(result, c)
		} else {
			result = append(result, '-')
		}
	}
	// Trim leading/trailing dashes
	return strings.Trim(string(result), "-")
}

func strPtr(s string) *string {
	return &s
}

// FormatResourceSummary returns a human-readable summary of resources that would be created.
func FormatResourceSummary(spec ExternalModelSpec, namespace string) string {
	svcName, seName, drName, hrName := ResourceNames(spec.Provider + "-" + sanitize(spec.Endpoint))
	summary := fmt.Sprintf("ExternalName Service: %s/%s\n", namespace, svcName)
	summary += fmt.Sprintf("ServiceEntry:         %s/%s\n", namespace, seName)
	if spec.TLS {
		summary += fmt.Sprintf("DestinationRule:      %s/%s\n", namespace, drName)
	}
	summary += fmt.Sprintf("HTTPRoute:            %s/%s\n", namespace, hrName)
	return summary
}
