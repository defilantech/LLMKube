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
// isOCISource guards, and the Metal cases ONLY because of the isMetalModel guard
// (a Metal service runs no pod; the agent downloads on the Mac, #2511). Remove
// any guard and its rows flip to true, failing this test.
func TestModelNeedsCachePVC(t *testing.T) {
	withKey := func(source string) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			Spec:   inferencev1alpha1.ModelSpec{Source: source},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "deadbeef"},
		}
	}
	// A Metal model with a remote source carries a cache key too: the Model
	// controller records one for the agent's fetch (#2511).
	metalWithKey := func(source string) *inferencev1alpha1.Model {
		m := withKey(source)
		m.Spec.Hardware = &inferencev1alpha1.HardwareSpec{Accelerator: acceleratorMetal}
		return m
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
		{"metal https source is fetched by the agent on the Mac -> no cache PVC", metalWithKey("https://example.com/model.gguf"), cachePath, false},
		{"metal s3 source is fetched by the agent on the Mac -> no cache PVC", metalWithKey("s3://bucket/model.gguf"), cachePath, false},
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
