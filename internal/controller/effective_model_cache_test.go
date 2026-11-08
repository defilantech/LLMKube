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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestResolveEffectiveModelCache(t *testing.T) {
	modelWith := func(cache *inferencev1alpha1.ModelCacheSpec) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
		m.Spec.ModelCache = cache
		return m
	}
	svcWith := func(name string, cache *inferencev1alpha1.ModelCacheSpec) *inferencev1alpha1.InferenceService {
		s := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		s.Spec.ModelCache = cache
		return s
	}

	cases := []struct {
		name        string
		isvc        *inferencev1alpha1.InferenceService
		model       *inferencev1alpha1.Model
		wantClaim   string
		wantClass   string
		wantSize    string
		wantEphem   bool
		wantPrefech bool
	}{
		{
			name:      "the service's own block wins over the Model's",
			isvc:      svcWith("svc", &inferencev1alpha1.ModelCacheSpec{ClaimName: "own"}),
			model:     modelWith(&inferencev1alpha1.ModelCacheSpec{ClaimName: "from-model"}),
			wantClaim: "own",
		},
		{
			name:      "a service with no block inherits the Model's claim",
			isvc:      svcWith("svc", nil),
			model:     modelWith(&inferencev1alpha1.ModelCacheSpec{ClaimName: "from-model"}),
			wantClaim: "from-model",
		},
		{
			name:  "an inherited block keeps the Model's storage class",
			isvc:  svcWith("svc", nil),
			model: modelWith(&inferencev1alpha1.ModelCacheSpec{ClaimName: "from-model", StorageClassName: "fast"}),
			// The Model's block is copied whole, as before #2142.
			wantClaim: "from-model",
			wantClass: "fast",
		},
		{
			name:     "a service with no block inherits the Model's size",
			isvc:     svcWith("svc", nil),
			model:    modelWith(&inferencev1alpha1.ModelCacheSpec{Size: "1Gi"}),
			wantSize: "1Gi",
		},
		{
			// Whole-struct precedence: the service's block replaces the
			// Model's, so the Model's size does not fill in a field the
			// service left unset (#2448).
			name:      "the service's own block drops the Model's size",
			isvc:      svcWith("svc", &inferencev1alpha1.ModelCacheSpec{StorageClassName: "fast"}),
			model:     modelWith(&inferencev1alpha1.ModelCacheSpec{Size: "1Gi"}),
			wantClass: "fast",
		},
		{
			name:        "an inherited block drops the Model's prefetch request",
			isvc:        svcWith("svc", nil),
			model:       modelWith(&inferencev1alpha1.ModelCacheSpec{ClaimName: "from-model", Prefetch: true}),
			wantClaim:   "from-model",
			wantPrefech: false,
		},
		{
			name:        "the service's own prefetch request is kept",
			isvc:        svcWith("svc", &inferencev1alpha1.ModelCacheSpec{ClaimName: "own", Prefetch: true}),
			model:       modelWith(nil),
			wantClaim:   "own",
			wantPrefech: true,
		},
		{
			name:      "neither names a claim",
			isvc:      svcWith("svc", nil),
			model:     modelWith(nil),
			wantClaim: "",
		},
		{
			name:      "an ephemeral opt-out is kept as written",
			isvc:      svcWith("svc", &inferencev1alpha1.ModelCacheSpec{Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral}),
			wantClaim: "",
			wantEphem: true,
		},
		{
			name:      "an inherited block keeps the Model's persistence",
			isvc:      svcWith("svc", nil),
			model:     modelWith(&inferencev1alpha1.ModelCacheSpec{Persistence: inferencev1alpha1.ModelCachePersistenceCached}),
			wantClaim: "",
		},
		{
			name:      "a nil service resolves nothing but stays usable",
			isvc:      nil,
			model:     modelWith(&inferencev1alpha1.ModelCacheSpec{ClaimName: "from-model"}),
			wantClaim: "from-model",
		},
		{
			name:      "a nil model and service resolve nothing",
			isvc:      nil,
			model:     nil,
			wantClaim: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveEffectiveModelCache(tc.isvc, tc.model)
			if got.claimName() != tc.wantClaim {
				t.Errorf("claimName() = %q, want %q", got.claimName(), tc.wantClaim)
			}
			if got.storageClassName() != tc.wantClass {
				t.Errorf("storageClassName() = %q, want %q", got.storageClassName(), tc.wantClass)
			}
			if got.size() != tc.wantSize {
				t.Errorf("size() = %q, want %q", got.size(), tc.wantSize)
			}
			if got.isEphemeral() != tc.wantEphem {
				t.Errorf("isEphemeral() = %v, want %v", got.isEphemeral(), tc.wantEphem)
			}
			if got.prefetchRequested() != tc.wantPrefech {
				t.Errorf("prefetchRequested() = %v, want %v", got.prefetchRequested(), tc.wantPrefech)
			}
		})
	}
}

// The inherited block must be a copy: the Model is read on every reconcile of
// every service, and a shared pointer would let one service's use of the value
// mutate the Model.
func TestResolveEffectiveModelCacheCopiesTheModelBlock(t *testing.T) {
	model := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	model.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "from-model", Prefetch: true, StorageClassName: "fast"}

	got := resolveEffectiveModelCache(nil, model)
	if got.spec == model.Spec.ModelCache {
		t.Fatal("the resolved value aliases Model.spec.modelCache; it must be a copy")
	}
	if model.Spec.ModelCache.Prefetch != true {
		t.Error("clearing prefetch on the resolved copy must not touch the Model's own block")
	}
}

// The service's own block is copied as well, so the type comment's claim that
// a status write cannot change the value is literally true: the write decodes
// the stored object back into the same pointer, so a held pointer to the
// service's spec would change under a reader.
func TestResolveEffectiveModelCacheCopiesTheServiceBlock(t *testing.T) {
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"}}
	isvc.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "own", StorageClassName: "fast"}

	got := resolveEffectiveModelCache(isvc, nil)
	if got.spec == isvc.Spec.ModelCache {
		t.Fatal("the resolved value aliases isvc.Spec.ModelCache; it must be a copy")
	}
	if got.claimName() != "own" || got.storageClassName() != "fast" {
		t.Errorf("the copy must keep the service's own values, got claim %q class %q", got.claimName(), got.storageClassName())
	}
}

func TestEffectiveModelCachePVCName(t *testing.T) {
	cases := []struct {
		name      string
		cache     effectiveModelCache
		mode      string
		wantClaim string
	}{
		{
			name:      "an explicit claimName wins over every mode",
			cache:     effectiveModelCache{spec: &inferencev1alpha1.ModelCacheSpec{ClaimName: "mine"}, serviceName: "svc"},
			mode:      ModelCacheModePerService,
			wantClaim: "mine",
		},
		{
			name:      "shared mode mounts the cluster-wide claim",
			cache:     effectiveModelCache{serviceName: "svc"},
			mode:      ModelCacheModeShared,
			wantClaim: ModelCachePVCName,
		},
		{
			name:      "an empty mode resolves to shared",
			cache:     effectiveModelCache{serviceName: "svc"},
			mode:      "",
			wantClaim: ModelCachePVCName,
		},
		{
			name:      "perService mode names the service's own claim",
			cache:     effectiveModelCache{serviceName: "svc"},
			mode:      ModelCacheModePerService,
			wantClaim: "svc-model-cache",
		},
		{
			name:      "a zero value falls back to the shared claim, as a nil service did",
			cache:     effectiveModelCache{},
			mode:      ModelCacheModePerService,
			wantClaim: ModelCachePVCName,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cache.pvcName(tc.mode); got != tc.wantClaim {
				t.Errorf("pvcName(%q) = %q, want %q", tc.mode, got, tc.wantClaim)
			}
		})
	}
}
