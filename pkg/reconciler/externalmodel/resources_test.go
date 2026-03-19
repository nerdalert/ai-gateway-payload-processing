package externalmodel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResourceNames(t *testing.T) {
	svc, se, dr, hr := ResourceNames("openai-api-openai-com")
	assert.Equal(t, "openai-api-openai-com-external", svc)
	assert.Equal(t, "openai-api-openai-com-serviceentry", se)
	assert.Equal(t, "openai-api-openai-com-destinationrule", dr)
	assert.Equal(t, "openai-api-openai-com-httproute", hr)
}

func TestSanitize(t *testing.T) {
	assert.Equal(t, "api-openai-com", sanitize("api.openai.com"))
	assert.Equal(t, "vllm-internal", sanitize("vllm.internal"))
	assert.Equal(t, "simple", sanitize("simple"))
	assert.Equal(t, "api-openai-com", sanitize("API.OpenAI.com"))  // uppercase
	assert.Equal(t, "host-8000", sanitize("host:8000"))            // colon
	assert.Equal(t, "my-host", sanitize("my_host"))                // underscore
}

func TestBuildExternalNameService(t *testing.T) {
	spec := ExternalModelSpec{
		Provider: "openai",
		Endpoint: "api.openai.com",
		Port:     443,
		TLS:      true,
	}
	labels := CommonLabels("test-model")

	svc := BuildExternalNameService(spec, "openshift-ingress", labels)

	assert.Equal(t, "openai-api-openai-com-external", svc.Name)
	assert.Equal(t, "openshift-ingress", svc.Namespace)
	assert.Equal(t, "api.openai.com", svc.Spec.ExternalName)
	assert.Equal(t, int32(443), svc.Spec.Ports[0].Port)
	assert.Contains(t, svc.Labels, "maas.opendatahub.io/external-model")
	assert.Empty(t, svc.OwnerReferences, "no OwnerReferences for cross-namespace resources")
}

func TestBuildServiceEntry(t *testing.T) {
	spec := ExternalModelSpec{
		Provider: "openai",
		Endpoint: "api.openai.com",
		Port:     443,
		TLS:      true,
	}
	labels := CommonLabels("test-model")

	se := BuildServiceEntry(spec, "openshift-ingress", labels)

	assert.Equal(t, "ServiceEntry", se.GetKind())
	assert.Equal(t, "networking.istio.io/v1", se.GetAPIVersion())
	assert.Equal(t, "openai-api-openai-com-serviceentry", se.GetName())
	assert.Empty(t, se.GetOwnerReferences(), "no OwnerReferences for cross-namespace resources")

	hosts := se.Object["spec"].(map[string]interface{})["hosts"].([]interface{})
	assert.Equal(t, "api.openai.com", hosts[0])

	ports := se.Object["spec"].(map[string]interface{})["ports"].([]interface{})
	port := ports[0].(map[string]interface{})
	assert.Equal(t, "https", port["name"])
	assert.Equal(t, "HTTPS", port["protocol"])
}

func TestBuildDestinationRule(t *testing.T) {
	spec := ExternalModelSpec{
		Provider: "openai",
		Endpoint: "api.openai.com",
		Port:     443,
		TLS:      true,
	}
	labels := CommonLabels("test-model")

	dr := BuildDestinationRule(spec, "openshift-ingress", labels)

	assert.Equal(t, "DestinationRule", dr.GetKind())
	assert.Equal(t, "networking.istio.io/v1", dr.GetAPIVersion())
	assert.Empty(t, dr.GetOwnerReferences())

	drSpec := dr.Object["spec"].(map[string]interface{})
	assert.Equal(t, "api.openai.com", drSpec["host"])
}

func TestBuildHTTPRoute(t *testing.T) {
	spec := ExternalModelSpec{
		Provider:     "openai",
		Endpoint:     "api.openai.com",
		Port:         443,
		TLS:          true,
		ExtraHeaders: map[string]string{},
	}
	labels := CommonLabels("test-model")

	hr := BuildHTTPRoute(spec, "openshift-ingress", "maas-default-gateway", "openshift-ingress", labels)

	assert.Equal(t, "openai-api-openai-com-httproute", hr.Name)
	assert.Equal(t, "openshift-ingress", hr.Namespace)
	assert.Empty(t, hr.OwnerReferences, "no OwnerReferences for cross-namespace resources")
	assert.Len(t, hr.Spec.ParentRefs, 1)
	assert.Equal(t, "maas-default-gateway", string(hr.Spec.ParentRefs[0].Name))
	assert.Len(t, hr.Spec.Rules, 1)
	assert.Equal(t, "/external/openai/", *hr.Spec.Rules[0].Matches[0].Path.Value)
}

func TestBuildHTTPRouteAnthropic(t *testing.T) {
	spec := ExternalModelSpec{
		Provider: "anthropic",
		Endpoint: "api.anthropic.com",
		Port:     443,
		TLS:      true,
		ExtraHeaders: map[string]string{
			"anthropic-version": "2023-06-01",
		},
	}
	labels := CommonLabels("test-model")

	hr := BuildHTTPRoute(spec, "openshift-ingress", "maas-default-gateway", "openshift-ingress", labels)

	for _, filter := range hr.Spec.Rules[0].Filters {
		if filter.RequestHeaderModifier != nil {
			found := false
			for _, h := range filter.RequestHeaderModifier.Set {
				if string(h.Name) == "anthropic-version" {
					found = true
					assert.Equal(t, "2023-06-01", h.Value)
				}
			}
			assert.True(t, found, "anthropic-version header should be present")
		}
	}
}

func TestBuildNoTLS(t *testing.T) {
	spec := ExternalModelSpec{
		Provider: "vllm",
		Endpoint: "vllm.internal",
		Port:     8000,
		TLS:      false,
	}
	labels := CommonLabels("test-model")

	se := BuildServiceEntry(spec, "default", labels)
	seSpec := se.Object["spec"].(map[string]interface{})
	ports := seSpec["ports"].([]interface{})
	port := ports[0].(map[string]interface{})
	assert.Equal(t, "HTTP", port["protocol"])
	assert.Equal(t, "http", port["name"])
}
