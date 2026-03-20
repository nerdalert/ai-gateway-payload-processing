/*
Copyright 2026 The opendatahub.io Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api_translation

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/external-model/provider"
	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/external-model/state"
	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/api-translation/providers"
	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/api-translation/providers/anthropic"
)

const (
	APITranslationPluginType = "api-translation"
)

// compile-time type validation
var _ framework.RequestProcessor = &APITranslationPlugin{}
var _ framework.ResponseProcessor = &APITranslationPlugin{}

// APITranslationFactory defines the factory function for APITranslationPlugin.
func APITranslationFactory(name string, _ json.RawMessage, _ framework.Handle) (framework.BBRPlugin, error) {
	return NewAPITranslationPlugin().WithName(name), nil
}

// NewAPITranslationPlugin creates a new plugin instance with all registered providers.
func NewAPITranslationPlugin() *APITranslationPlugin {
	return &APITranslationPlugin{
		typedName: plugin.TypedName{
			Type: APITranslationPluginType,
			Name: APITranslationPluginType,
		},
		providers: map[string]providers.Provider{
			provider.Anthropic: anthropic.NewAnthropicProvider(),
		},
	}
}

// APITranslationPlugin translates inference API requests and responses between
// OpenAI Chat Completions format and provider-native formats (e.g., Anthropic Messages API).
type APITranslationPlugin struct {
	typedName plugin.TypedName
	providers map[string]providers.Provider
}

// TypedName returns the type and name tuple of this plugin instance.
func (p *APITranslationPlugin) TypedName() plugin.TypedName {
	return p.typedName
}

// WithName sets the name of the plugin instance.
func (p *APITranslationPlugin) WithName(name string) *APITranslationPlugin {
	p.typedName.Name = name
	return p
}

// ProcessRequest reads the provider from CycleState (set by an upstream plugin) and translates
// the request body from OpenAI format to the provider's native format if needed.
func (p *APITranslationPlugin) ProcessRequest(ctx context.Context, cycleState *framework.CycleState, request *framework.InferenceRequest) error {
	if request == nil || request.Headers == nil || request.Body == nil {
		return fmt.Errorf("invalid inference request: request/headers/body must be non-nil")
	}

	providerName, err := framework.ReadCycleStateKey[string](cycleState, state.ProviderKey) // err if not found
	if err != nil || providerName == "" || providerName == "openai" {                       // empty provider means no translation needed
		return nil
	}

	translator, ok := p.providers[providerName]
	if !ok {
		return fmt.Errorf("unsupported provider %q", providerName)
	}

	// Store model in CycleState for response use
	if model, ok := request.Body["model"].(string); ok {
		cycleState.Write(state.ModelKey, model)
	}

	translatedBody, headers, headersToRemove, err := translator.TranslateRequest(request.Body)
	if err != nil {
		return fmt.Errorf("request translation failed for provider %q: %w", providerName, err)
	}

	if translatedBody != nil {
		request.SetBody(translatedBody)
	}

	for k, v := range headers {
		request.SetHeader(k, v)
	}
	for _, k := range headersToRemove {
		request.RemoveHeader(k)
	}

	return nil
}

// ProcessResponse reads the provider from CycleState and translates the response
// back to OpenAI Chat Completions format if needed.
func (p *APITranslationPlugin) ProcessResponse(ctx context.Context, cycleState *framework.CycleState, response *framework.InferenceResponse) error {
	if response == nil || response.Headers == nil || response.Body == nil {
		return fmt.Errorf("invalid inference response: response/headers/body must be non-nil")
	}

	providerName, _ := framework.ReadCycleStateKey[string](cycleState, state.ProviderKey)
	if providerName == "" || providerName == "openai" {
		return nil
	}

	translator, ok := p.providers[providerName]
	if !ok {
		return nil
	}

	model, _ := framework.ReadCycleStateKey[string](cycleState, state.ModelKey)

	translatedBody, err := translator.TranslateResponse(response.Body, model)
	if err != nil {
		return fmt.Errorf("response translation failed for provider %q: %w", providerName, err)
	}

	if translatedBody != nil {
		response.SetBody(translatedBody)
	}

	return nil
}
