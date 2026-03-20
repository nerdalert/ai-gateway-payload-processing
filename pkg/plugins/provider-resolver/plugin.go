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

package provider_resolver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/external-model/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"
)

const (
	// ProviderResolverPluginType is the unique identifier for this plugin, used in CLI flags and registry.
	ProviderResolverPluginType = "provider-resolver"
)

// maasModelRefGVK is the GroupVersionKind for MaaSModelRef CRD.
var maasModelRefGVK = schema.GroupVersionKind{
	Group:   "maas.opendatahub.io",
	Version: "v1alpha1",
	Kind:    "MaaSModelRef",
}

// compile-time type validation
var _ framework.RequestProcessor = &ProviderResolverPlugin{}

// ProviderResolverFactory creates a new ProviderResolverPlugin and registers a MaaSModelRef reconciler
// via the framework Handle. Uses unstructured client to avoid importing MaaS controller types.
func ProviderResolverFactory(name string, _ json.RawMessage, handle framework.Handle) (framework.BBRPlugin, error) {
	store := newModelStore()

	reconciler := &maasModelRefReconciler{
		Reader: handle.ClientReader(),
		store:  store,
	}

	// Watch MaaSModelRef CRDs using unstructured client (no MaaS type dependency)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(maasModelRefGVK)

	if err := handle.ReconcilerBuilder().
		For(obj).
		Complete(reconciler); err != nil {
		return nil, fmt.Errorf("failed to register MaaSModelRef reconciler for plugin '%s' - %w", ProviderResolverPluginType, err)
	}

	p := &ProviderResolverPlugin{
		typedName: plugin.TypedName{
			Type: ProviderResolverPluginType,
			Name: ProviderResolverPluginType,
		},
		store: store,
	}

	return p.WithName(name), nil
}

// ProviderResolverPlugin resolves model names to providers by watching MaaSModelRef CRDs.
// It writes the provider and credential reference to CycleState for downstream plugins
// (api-translation, api-key-injection).
type ProviderResolverPlugin struct {
	typedName plugin.TypedName
	store     *modelStore
}

// TypedName returns the type and name tuple of this plugin instance.
func (p *ProviderResolverPlugin) TypedName() plugin.TypedName {
	return p.typedName
}

// WithName sets the name of the plugin instance.
func (p *ProviderResolverPlugin) WithName(name string) *ProviderResolverPlugin {
	p.typedName.Name = name
	return p
}

// ProcessRequest reads the model name from the request body, resolves the provider
// from the model store (populated by MaaSModelRef reconciler), and writes provider
// and credential reference info to CycleState.
func (p *ProviderResolverPlugin) ProcessRequest(ctx context.Context, cycleState *framework.CycleState, request *framework.InferenceRequest) error {
	if request == nil || request.Headers == nil || request.Body == nil {
		return fmt.Errorf("invalid inference request: request/headers/body must be non-nil")
	}

	model, ok := request.Body["model"].(string)
	if !ok || model == "" {
		return nil
	}

	info, found := p.store.getProvider(model)
	if !found {
		return nil
	}

	cycleState.Write(state.ProviderKey, info.Provider)

	if info.CredentialRefName != "" {
		cycleState.Write(state.CredsRefName, info.CredentialRefName)
		cycleState.Write(state.CredsRefNamespace, info.CredentialRefNamespace)
	}

	return nil
}
