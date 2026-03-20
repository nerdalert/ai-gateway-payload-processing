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
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// ModelInfo holds the provider and credential reference for an external model.
type ModelInfo struct {
	Provider               string
	CredentialRefName      string
	CredentialRefNamespace string
}

// modelStore is a thread-safe in-memory store that maps model names to their provider info.
// The reconciler writes to it; the plugin reads from it during request processing.
type modelStore struct {
	mu     sync.RWMutex
	models map[string]ModelInfo
	// modelToResource tracks which MaaSModelRef resource added each model entry,
	// so we can clean up when the resource is deleted.
	modelToResource map[string]types.NamespacedName
}

func newModelStore() *modelStore {
	return &modelStore{
		models:          make(map[string]ModelInfo),
		modelToResource: make(map[string]types.NamespacedName),
	}
}

// getProvider returns the ModelInfo for a model name, or empty if not found.
func (s *modelStore) getProvider(modelName string) (ModelInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info, ok := s.models[modelName]
	return info, ok
}

// setModel stores the model→provider mapping and records which resource it came from.
func (s *modelStore) setModel(modelName string, info ModelInfo, resourceKey types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models[modelName] = info
	s.modelToResource[modelName] = resourceKey
}

// deleteByResource removes the model entry that was added by the given MaaSModelRef resource.
func (s *modelStore) deleteByResource(resourceKey types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for modelName, key := range s.modelToResource {
		if key == resourceKey {
			delete(s.models, modelName)
			delete(s.modelToResource, modelName)
			return
		}
	}
}
