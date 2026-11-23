package controller

import (
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestModelNeedsCachePVC guards the fix for the unused cache PVC on pre-staged
// sources: a pvc:// model or an oci:// ImageVolume is mounted read-only (no
// download), so the operator must not provision a cache PVC for either. Each
// model below sets Status.CacheKey so effectiveModelCacheKey is non-empty -- the
// pvc:// and oci:// cases are therefore false ONLY because of the isPVCSource /
// isOCISource guards. Remove either guard and its row flips to true, failing
// this test.
func TestModelNeedsCachePVC(t *testing.T) {
	withKey := func(source string) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			Spec:   inferencev1alpha1.ModelSpec{Source: source},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "deadbeef"},
		}
	}
	const cachePath = "/models"

	tests := []struct {
		name      string
		model     *inferencev1alpha1.Model
		cachePath string
		want      bool
	}{
		{"https source is downloaded -> needs a cache", withKey("https://example.com/model.gguf"), cachePath, true},
		{"http source is downloaded -> needs a cache", withKey("http://example.com/model.gguf"), cachePath, true},
		{"s3 source is downloaded -> needs a cache", withKey("s3://bucket/model.gguf"), cachePath, true},
		{"pvc:// source is pre-staged/read-only -> no cache PVC", withKey("pvc://my-models/model.gguf"), cachePath, false},
		{"oci:// source is pre-staged/read-only -> no cache PVC", withKey("oci://registry.defilan.net/models/qwen3-32b@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), cachePath, false},
		{"caching disabled (empty cache path) -> no cache PVC", withKey("https://example.com/model.gguf"), "", false},
		{"no cache key -> no cache PVC", &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"}}, cachePath, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// nil isvc: these cases predate spec.modelCache.persistence and
			// assert the operator-level behaviour, which a nil isvc preserves
			// (no opt-out). Ephemeral is covered in model_cache_ephemeral_test.go.
			if got := modelNeedsCachePVC(tc.model, resolveEffectiveModelCache(nil, nil), tc.cachePath); got != tc.want {
				t.Errorf("modelNeedsCachePVC(source=%q, cachePath=%q) = %v, want %v",
					tc.model.Spec.Source, tc.cachePath, got, tc.want)
			}
		})
	}
}

// TestMetalAgentServes pins which services the host metal-agent serves with no
// pod, so they need no model cache claim (#2511): a Metal model outside a
// multiNode group. A multiNode group runs member pods even for a Metal model,
// because reconcileWorkload checks multiNode before the Metal path, so it keeps
// its claim.
func TestMetalAgentServes(t *testing.T) {
	withAccel := func(accel string) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: accel},
		}}
	}
	single := &inferencev1alpha1.InferenceService{}
	group := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{
		MultiNode: &inferencev1alpha1.MultiNodeSpec{},
	}}

	tests := []struct {
		name  string
		isvc  *inferencev1alpha1.InferenceService
		model *inferencev1alpha1.Model
		want  bool
	}{
		{"metal model, single service -> the agent serves it, no pod", single, withAccel(acceleratorMetal), true},
		{"metal model in a multiNode group -> member pods, keeps its claim", group, withAccel(acceleratorMetal), false},
		{"cuda model -> pods", single, withAccel("cuda"), false},
		{"cpu model -> pods", single, withAccel("cpu"), false},
		{"no hardware block -> pods", single, &inferencev1alpha1.Model{}, false},
		{"no model -> pods", single, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := metalAgentServes(tc.isvc, tc.model); got != tc.want {
				t.Errorf("metalAgentServes() = %v, want %v", got, tc.want)
			}
		})
	}
}
