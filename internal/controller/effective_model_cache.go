/*
Copyright 2026.

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

package controller

import (
	"fmt"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// effectiveModelCache is the cache configuration one InferenceService's pods
// get: the service's own spec.modelCache when it has one, else the Model's.
// The Model's block is the default for the services that reference it
// (api/v1alpha1/model_types.go, Model.Spec.ModelCache), and its prefetch field
// is the Model's own spec.prefetch, which holds no service, so it is cleared.
//
// The value is taken once per reconcile and passed to every reader. Reading
// isvc.Spec.ModelCache directly is not equivalent: a status write decodes the
// stored object back into the same pointer, which drops a block that was
// written into the spec in memory and never persisted.
type effectiveModelCache struct {
	spec        *inferencev1alpha1.ModelCacheSpec
	serviceName string
}

// resolveEffectiveModelCache returns the cache configuration isvc's pods get.
func resolveEffectiveModelCache(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) effectiveModelCache {
	cache := effectiveModelCache{}
	if isvc != nil {
		cache.serviceName = isvc.Name
		if isvc.Spec.ModelCache != nil {
			cache.spec = isvc.Spec.ModelCache.DeepCopy()
			return cache
		}
	}
	if model != nil && model.Spec.ModelCache != nil {
		inherited := model.Spec.ModelCache.DeepCopy()
		inherited.Prefetch = false
		cache.spec = inherited
	}
	return cache
}

// claimName returns the user-supplied cache PVC name, or "" when the service
// does not override the operator-global cache mode.
func (e effectiveModelCache) claimName() string {
	if e.spec == nil {
		return ""
	}
	return e.spec.ClaimName
}

// storageClassName returns the user-supplied cache storage class, or "" when
// the service does not override the operator-global --model-cache-storage-class.
func (e effectiveModelCache) storageClassName() string {
	if e.spec == nil {
		return ""
	}
	return e.spec.StorageClassName
}

// size returns the user-supplied cache claim size, or "" when the service does
// not override the operator-global --model-cache-size.
func (e effectiveModelCache) size() string {
	if e.spec == nil {
		return ""
	}
	return e.spec.Size
}

// isEphemeral reports whether the service declined the cache (#1451). Only an
// explicit Ephemeral opts out: an absent block and an empty persistence both
// resolve to Cached.
func (e effectiveModelCache) isEphemeral() bool {
	return e.spec != nil && e.spec.Persistence == inferencev1alpha1.ModelCachePersistenceEphemeral
}

// prefetchRequested reports whether the service asks for its Model to be
// prefetched into the cache claim its pods mount. The request is the service's
// own: an inherited block drops the Model's prefetch setting.
func (e effectiveModelCache) prefetchRequested() bool {
	return e.spec != nil && e.spec.Prefetch && !e.isEphemeral()
}

// pvcName returns the name of the model cache PVC for the given mode. An
// explicit claimName override (#928) wins over the operator-global mode.
// Otherwise, in shared mode (the default, and the resolution of an empty mode)
// this is the single cluster-wide PVC; in perService mode it is the
// per-InferenceService PVC "<isvc>-model-cache". A zero value (a builder
// exercised without a service) falls back to the shared name.
func (e effectiveModelCache) pvcName(mode string) string {
	if claim := e.claimName(); claim != "" {
		return claim
	}
	if e.serviceName == "" || resolveCacheMode(mode) == ModelCacheModeShared {
		return ModelCachePVCName
	}
	return fmt.Sprintf("%s-model-cache", e.serviceName)
}
