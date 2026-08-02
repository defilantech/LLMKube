package controller

import (
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestModelNeedsCachePVC guards the fix for the unused cache PVC on pvc://
// sources: a pre-staged pvc:// model is mounted read-only (no download), so the
// operator must not provision a per-ISVC cache PVC for it. Each model below
// sets Status.CacheKey so effectiveModelCacheKey is non-empty -- the pvc:// case
// is therefore false ONLY because of the isPVCSource guard. Remove that guard
// and the pvc:// case flips to true, failing this test.
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
		{"caching disabled (empty cache path) -> no cache PVC", withKey("https://example.com/model.gguf"), "", false},
		{"no cache key -> no cache PVC", &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"}}, cachePath, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelNeedsCachePVC(tc.model, tc.cachePath); got != tc.want {
				t.Errorf("modelNeedsCachePVC(source=%q, cachePath=%q) = %v, want %v",
					tc.model.Spec.Source, tc.cachePath, got, tc.want)
			}
		})
	}
}
