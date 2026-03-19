// Package externalmodel implements a reconciler that watches MaaSModelRef CRs
// with kind=ExternalModel and creates the Istio resources required to route
// traffic to an external AI model provider:
//
//  1. ExternalName Service   - DNS bridge for HTTPRoute backendRef
//  2. ServiceEntry           - Registers external host in Istio mesh
//  3. DestinationRule        - TLS origination (HTTP -> HTTPS)
//  4. HTTPRoute              - Routes requests and sets Host header
//
// Resources are created in the gateway namespace (cross-namespace from the CR).
// A finalizer on the MaaSModelRef handles cleanup since cross-namespace
// OwnerReferences are not supported by Kubernetes garbage collection.
package externalmodel

import "strings"

// ExternalModelSpec holds the configuration for routing to an external model.
// These fields are read from MaaSModelRef annotations until the CRD is enriched.
//
// Required annotations:
//
//	maas.opendatahub.io/endpoint: "api.openai.com"
//
// Optional annotations:
//
//	maas.opendatahub.io/provider: "openai"
//	maas.opendatahub.io/extra-headers: "anthropic-version=2023-06-01"
//	maas.opendatahub.io/port: "443"
//	maas.opendatahub.io/tls: "true"
//	maas.opendatahub.io/path-prefix: "/external/openai/"
type ExternalModelSpec struct {
	// Provider identifies the API format (e.g. "openai", "anthropic", "vllm")
	Provider string
	// Endpoint is the external FQDN (e.g. "api.openai.com")
	Endpoint string
	// ExtraHeaders are additional headers to set (e.g. "anthropic-version=2023-06-01")
	ExtraHeaders map[string]string
	// Port is the external service port (default 443)
	Port int32
	// TLS indicates whether TLS origination is needed (default true)
	TLS bool
	// PathPrefix is the path prefix to match (default "/external/<provider>/")
	PathPrefix string
}

// ResourceNames returns consistent names for the resources created per external model.
// Names are truncated to 63 characters (Kubernetes limit for DNS labels).
func ResourceNames(modelName string) (svcName, seName, drName, hrName string) {
	svcName = truncateName(modelName, "-external")
	seName = truncateName(modelName, "-serviceentry")
	drName = truncateName(modelName, "-destinationrule")
	hrName = truncateName(modelName, "-httproute")
	return
}

// truncateName ensures base + suffix fits within 63 characters.
func truncateName(base, suffix string) string {
	const maxLen = 63
	max := maxLen - len(suffix)
	if max < 1 {
		max = 1
	}
	if len(base) > max {
		base = base[:max]
		// Trim trailing dashes from truncation
		base = strings.TrimRight(base, "-")
	}
	return base + suffix
}

// CommonLabels returns labels applied to all managed resources.
func CommonLabels(modelName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":       "maas-external-model-reconciler",
		"maas.opendatahub.io/external-model": modelName,
	}
}
