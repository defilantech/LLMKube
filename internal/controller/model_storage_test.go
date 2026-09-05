/*
Copyright 2025.

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var _ = Describe("buildModelInitCommand (s3)", func() {
	It("should emit the --aws-sigv4 curl line for s3 source with cache", func() {
		cmd := buildModelInitCommand(false, true, true, false, "")
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/${S3_KEY}"))
		Expect(cmd).To(ContainSubstring("Downloading model from S3"))
		Expect(cmd).To(ContainSubstring("Model downloaded successfully"))
		Expect(cmd).To(ContainSubstring("Model already cached, skipping download"))
		Expect(cmd).To(ContainSubstring(`-o "$MODEL_PATH.tmp"`))
		Expect(cmd).To(ContainSubstring(`&& mv "$MODEL_PATH.tmp" "$MODEL_PATH"`))
	})

	It("should emit the --aws-sigv4 curl line for s3 source without cache", func() {
		cmd := buildModelInitCommand(false, true, false, false, "")
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/${S3_KEY}"))
		Expect(cmd).To(ContainSubstring("Downloading model from S3"))
		Expect(cmd).To(ContainSubstring("Model downloaded successfully"))
		Expect(cmd).To(ContainSubstring("Model already exists, skipping download"))
		Expect(cmd).To(ContainSubstring(`-o "$MODEL_PATH.tmp"`))
		Expect(cmd).To(ContainSubstring(`&& mv "$MODEL_PATH.tmp" "$MODEL_PATH"`))
	})

	It("should NOT emit --aws-sigv4 for non-s3 source", func() {
		cmd := buildModelInitCommand(false, false, true, false, "")
		Expect(cmd).ToNot(ContainSubstring("aws-sigv4"))
		Expect(cmd).To(ContainSubstring(`curl -f -L -o "$MODEL_PATH.tmp" "$MODEL_SOURCE" && mv "$MODEL_PATH.tmp" "$MODEL_PATH"`))
	})

	// A truncated transfer must never be published at $MODEL_PATH: the guard
	// is a bare existence check, so a non-atomic write would be cached as
	// complete forever (#1428).
	It("should never write the download target directly for any non-OnChange variant", func() {
		for _, tc := range [][3]bool{
			{false, false, true},  // https, cached
			{false, false, false}, // https, uncached
			{false, true, true},   // s3, cached
			{false, true, false},  // s3, uncached
			{true, false, true},   // local, cached
		} {
			cmd := buildModelInitCommand(tc[0], tc[1], tc[2], false, "")
			Expect(cmd).ToNot(ContainSubstring(`-o "$MODEL_PATH" `), cmd)
			Expect(cmd).ToNot(ContainSubstring(`cp /host-model/model.gguf "$MODEL_PATH" `), cmd)
		}
	})

	It("should emit the --aws-sigv4 curl line for s3 source with OnChange refresh", func() {
		cmd := buildModelInitCommand(false, true, true, false, RefreshPolicyOnChange)
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/${S3_KEY}"))
	})
})

// Issue #1435: interrupted transfers leave orphaned .tmp files that nothing
// cleans up. The init command must remove stale .tmp files before starting a
// new download so they do not accumulate on the shared cache PVC.
var _ = Describe("buildModelInitCommand (orphan .tmp cleanup, #1435)", func() {
	It("should remove stale .tmp before downloading in cached remote path", func() {
		cmd := buildModelInitCommand(false, false, true, false, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring(`rm -f "$MODEL_PATH.tmp"`))
	})

	It("should remove stale .tmp before downloading in cached S3 path", func() {
		cmd := buildModelInitCommand(false, true, true, false, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring(`rm -f "$MODEL_PATH.tmp"`))
	})

	It("should remove stale .tmp before downloading in cached local path", func() {
		cmd := buildModelInitCommand(true, false, true, false, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring(`rm -f "$MODEL_PATH.tmp"`))
	})

	It("should remove stale .tmp before downloading in cached OnChange path", func() {
		cmd := buildModelInitCommand(false, false, true, false, RefreshPolicyOnChange)
		Expect(cmd).To(ContainSubstring(`rm -f "$MODEL_PATH.tmp"`))
	})
})

var _ = Describe("modelInitEnvVars (s3)", func() {
	It("should include S3_BUCKET and S3_KEY for s3 source", func() {
		envs := modelInitEnvVars("s3://my-bucket/models/model.gguf", "/models/cache", "/models/cache/model.gguf")
		Expect(envs).To(HaveLen(5))
		Expect(envs).To(ContainElement(corev1.EnvVar{Name: "S3_BUCKET", Value: "my-bucket"}))
		Expect(envs).To(ContainElement(corev1.EnvVar{Name: "S3_KEY", Value: "models/model.gguf"}))
		Expect(envs).To(ContainElement(corev1.EnvVar{Name: "MODEL_SOURCE", Value: "s3://my-bucket/models/model.gguf"}))
		Expect(envs).To(ContainElement(corev1.EnvVar{Name: "CACHE_DIR", Value: "/models/cache"}))
		Expect(envs).To(ContainElement(corev1.EnvVar{Name: "MODEL_PATH", Value: "/models/cache/model.gguf"}))
	})

	It("should NOT include S3_BUCKET and S3_KEY for non-s3 source", func() {
		envs := modelInitEnvVars("https://example.com/model.gguf", "/models/cache", "/models/cache/model.gguf")
		Expect(envs).To(HaveLen(3))
		Expect(envs).ToNot(ContainElement(corev1.EnvVar{Name: "S3_BUCKET"}))
		Expect(envs).ToNot(ContainElement(corev1.EnvVar{Name: "S3_KEY"}))
	})
})

var _ = Describe("buildMultiFileInitCommand (s3)", func() {
	It("should emit --aws-sigv4 for s3 source with cache (IfNotPresent)", func() {
		cmd := buildMultiFileInitCommand(true, true, false, "")
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/"))
		Expect(cmd).To(ContainSubstring("${S3_PREFIX:+${S3_PREFIX}/}"))
		Expect(cmd).To(ContainSubstring("Downloading model artifact"))
		Expect(cmd).To(ContainSubstring("Model artifact"))
		Expect(cmd).To(ContainSubstring(`-o "$dest.tmp"`))
		Expect(cmd).To(ContainSubstring(`&& mv "$dest.tmp" "$dest"`))
	})

	It("should emit --aws-sigv4 for s3 source without cache (emptyDir)", func() {
		cmd := buildMultiFileInitCommand(false, true, false, "")
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/"))
		Expect(cmd).To(ContainSubstring("${S3_PREFIX:+${S3_PREFIX}/}"))
	})

	It("should emit --aws-sigv4 for s3 source with OnChange refresh", func() {
		cmd := buildMultiFileInitCommand(true, true, false, RefreshPolicyOnChange)
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/"))
		Expect(cmd).To(ContainSubstring("${S3_PREFIX:+${S3_PREFIX}/}"))
		Expect(cmd).To(ContainSubstring("revalidated"))
	})

	It("should NOT emit --aws-sigv4 for non-s3 source (HTTP regression)", func() {
		cmd := buildMultiFileInitCommand(true, false, false, "")
		Expect(cmd).ToNot(ContainSubstring("aws-sigv4"))
		Expect(cmd).To(ContainSubstring(`curl -f -L -o "$dest.tmp" "$url"`))
		Expect(cmd).To(ContainSubstring("${SOURCE%/}/$rel"))
	})

	It("should NOT emit --aws-sigv4 for non-s3 source with OnChange (HTTP regression)", func() {
		cmd := buildMultiFileInitCommand(true, false, false, RefreshPolicyOnChange)
		Expect(cmd).ToNot(ContainSubstring("aws-sigv4"))
		Expect(cmd).To(ContainSubstring(`curl -fsSL -o "$dest.tmp" "$url"`))
		Expect(cmd).To(ContainSubstring("${SOURCE%/}/$rel"))
	})
})

var _ = Describe("multiFileInitEnvVars (s3)", func() {
	It("should include S3_BUCKET and S3_PREFIX for s3 source", func() {
		envs := multiFileInitEnvVars("s3://my-bucket/models/model.gguf", "/models/cache", []string{"model.gguf", "mmproj.gguf"})
		Expect(envs).To(HaveLen(5))
		Expect(envs).To(ContainElement(corev1.EnvVar{Name: "S3_BUCKET", Value: "my-bucket"}))
		Expect(envs).To(ContainElement(corev1.EnvVar{Name: "S3_PREFIX", Value: "models/model.gguf"}))
		Expect(envs).To(ContainElement(corev1.EnvVar{Name: "MODEL_SOURCE", Value: "s3://my-bucket/models/model.gguf"}))
		Expect(envs).To(ContainElement(corev1.EnvVar{Name: "CACHE_DIR", Value: "/models/cache"}))
		Expect(envs).To(ContainElement(corev1.EnvVar{Name: "MODEL_FILES", Value: "model.gguf\nmmproj.gguf"}))
	})

	It("should NOT include S3_BUCKET and S3_PREFIX for non-s3 source", func() {
		envs := multiFileInitEnvVars("https://example.com/models/", "/models/cache", []string{"model.gguf"})
		Expect(envs).To(HaveLen(3))
		Expect(envs).ToNot(ContainElement(corev1.EnvVar{Name: "S3_BUCKET"}))
		Expect(envs).ToNot(ContainElement(corev1.EnvVar{Name: "S3_PREFIX"}))
	})
})

var _ = Describe("modelEnvFrom", func() {
	It("should return nil when SourceSecretRef is nil", func() {
		model := &inferencev1alpha1.Model{}
		envFrom := modelEnvFrom(model)
		Expect(envFrom).To(BeNil())
	})

	It("should return EnvFrom with SecretRef when SourceSecretRef is set", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				SourceSecretRef: &corev1.LocalObjectReference{Name: "s3-credentials"},
			},
		}
		envFrom := modelEnvFrom(model)
		Expect(envFrom).To(HaveLen(1))
		Expect(envFrom[0].SecretRef).ToNot(BeNil())
		Expect(envFrom[0].SecretRef.Name).To(Equal("s3-credentials"))
	})
})

// These exercise the merge's own logic. The invariants that make the resulting
// pod acceptable to the apiserver are asserted at constructDeployment level in
// deployment_builder_test.go, where the inputs come from the real storage
// builders rather than from fixtures that can invent names production never
// emits.
func TestMergeStorageConfigs(t *testing.T) {
	pvcVolume := func(name, claim string) corev1.Volume {
		return corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			},
		}
	}

	// Both models on the per-service cache: the PVC is mounted once at
	// /models and each model already lives under its own
	// effectiveModelCacheKey subdirectory, so the merged pod must carry ONE
	// model-cache volume and BOTH download init containers.
	t.Run("the same cache volume is shared", func(t *testing.T) {
		cache := pvcVolume("model-cache", "svc-model-cache")
		mount := corev1.VolumeMount{Name: "model-cache", MountPath: "/models"}
		target := modelStorageConfig{
			modelPath:      "/models/target/model.gguf",
			initContainers: []corev1.Container{{Name: "model-downloader", Image: "curl"}},
			volumes:        []corev1.Volume{cache},
			volumeMounts:   []corev1.VolumeMount{mount},
		}
		draft := modelStorageConfig{
			modelPath:      "/models/draft/model.gguf",
			initContainers: []corev1.Container{{Name: "model-downloader", Image: "curl", Env: []corev1.EnvVar{{Name: "MODEL_PATH"}}}},
			volumes:        []corev1.Volume{cache},
			volumeMounts:   []corev1.VolumeMount{mount},
		}

		got, placed := mergeStorageConfigs(target, draft)

		if len(got.volumes) != 1 {
			t.Errorf("volumes = %d, want 1 (the shared model-cache)", len(got.volumes))
		}
		if len(got.volumeMounts) != 1 {
			t.Errorf("volumeMounts = %d, want 1", len(got.volumeMounts))
		}
		if len(got.initContainers) != 2 {
			t.Errorf("initContainers = %d, want 2 (one download per model)", len(got.initContainers))
		}
		if got.initContainers[1].Name != "draft-model-downloader" {
			t.Errorf("draft init container name = %q, want %q (both builders emit the fixed name %q)",
				got.initContainers[1].Name, "draft-model-downloader", "model-downloader")
		}
		if got.modelPath != "/models/target/model.gguf" {
			t.Errorf("modelPath = %q, want the target's path unchanged", got.modelPath)
		}
		if placed.modelPath != "/models/draft/model.gguf" {
			t.Errorf("placed draft modelPath = %q, want it unchanged under the shared mount", placed.modelPath)
		}
	})

	// Two pvc:// models both arrive as a volume named "model-source" mounted
	// at /model-source. Name-only dedup dropped the draft's claim and left -md
	// pointing into the target's.
	t.Run("a colliding volume with a different source is renamed and remounted", func(t *testing.T) {
		target := modelStorageConfig{
			modelPath:    "/model-source/model.gguf",
			volumes:      []corev1.Volume{pvcVolume("model-source", "target-claim")},
			volumeMounts: []corev1.VolumeMount{{Name: "model-source", MountPath: "/model-source"}},
		}
		draft := modelStorageConfig{
			modelPath:    "/model-source/model.gguf",
			volumes:      []corev1.Volume{pvcVolume("model-source", "draft-claim")},
			volumeMounts: []corev1.VolumeMount{{Name: "model-source", MountPath: "/model-source"}},
		}

		got, placed := mergeStorageConfigs(target, draft)

		if len(got.volumes) != 2 {
			t.Fatalf("volumes = %d, want 2 (the claims differ)", len(got.volumes))
		}
		if got.volumes[1].Name != "draft-model-source" {
			t.Errorf("draft volume name = %q, want draft-model-source", got.volumes[1].Name)
		}
		if got.volumes[1].PersistentVolumeClaim.ClaimName != "draft-claim" {
			t.Errorf("draft claim = %q, want draft-claim", got.volumes[1].PersistentVolumeClaim.ClaimName)
		}
		if got.volumeMounts[1].MountPath != "/draft/model-source" {
			t.Errorf("draft mountPath = %q, want /draft/model-source", got.volumeMounts[1].MountPath)
		}
		if placed.modelPath != "/draft/model-source/model.gguf" {
			t.Errorf("placed draft modelPath = %q, want it rewritten onto the new mount", placed.modelPath)
		}
	})

	// A draft whose volume name does not collide but whose mount path does.
	t.Run("a colliding mount path is remounted under /draft", func(t *testing.T) {
		target := modelStorageConfig{
			modelPath:    "/models/target-key/target.gguf",
			volumes:      []corev1.Volume{pvcVolume("model-cache", "svc-model-cache")},
			volumeMounts: []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models"}},
		}
		draft := modelStorageConfig{
			modelPath: "/models/default-dspark.gguf",
			stagedDir: "/models",
			volumes: []corev1.Volume{{
				Name:         "model-storage",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
			volumeMounts: []corev1.VolumeMount{{Name: "model-storage", MountPath: "/models"}},
		}

		got, placed := mergeStorageConfigs(target, draft)

		if len(got.volumes) != 2 {
			t.Fatalf("volumes = %d, want 2", len(got.volumes))
		}
		if got.volumes[1].Name != "model-storage" {
			t.Errorf("draft volume name = %q, want model-storage (no name collision to resolve)", got.volumes[1].Name)
		}
		if got.volumeMounts[1].MountPath != "/draft/models" {
			t.Errorf("draft mountPath = %q, want /draft/models", got.volumeMounts[1].MountPath)
		}
		if placed.modelPath != "/draft/models/default-dspark.gguf" {
			t.Errorf("placed draft modelPath = %q, want it rewritten", placed.modelPath)
		}
		if placed.stagedDir != "/draft/models" {
			t.Errorf("placed draft stagedDir = %q, want /draft/models", placed.stagedDir)
		}
	})

	// A draft from a different source brings its own volume at its own path,
	// which must survive untouched alongside the target's.
	t.Run("distinct volumes both survive unchanged", func(t *testing.T) {
		target := modelStorageConfig{
			volumes:      []corev1.Volume{{Name: "model-cache"}},
			volumeMounts: []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models"}},
		}
		draft := modelStorageConfig{
			modelPath:    "/draft-pvc/model.gguf",
			volumes:      []corev1.Volume{{Name: "draft-pvc"}},
			volumeMounts: []corev1.VolumeMount{{Name: "draft-pvc", MountPath: "/draft-pvc"}},
		}

		got, placed := mergeStorageConfigs(target, draft)

		if len(got.volumes) != 2 {
			t.Errorf("volumes = %d, want 2", len(got.volumes))
		}
		if len(got.volumeMounts) != 2 {
			t.Errorf("volumeMounts = %d, want 2", len(got.volumeMounts))
		}
		if placed.modelPath != "/draft-pvc/model.gguf" {
			t.Errorf("placed draft modelPath = %q, want it unchanged", placed.modelPath)
		}
	})

	// The inputs are the caller's; the merge must not write through them.
	t.Run("neither input is mutated", func(t *testing.T) {
		target := modelStorageConfig{
			initContainers: []corev1.Container{{Name: "model-downloader"}},
			volumes:        []corev1.Volume{pvcVolume("model-source", "target-claim")},
			volumeMounts:   []corev1.VolumeMount{{Name: "model-source", MountPath: "/model-source"}},
		}
		draft := modelStorageConfig{
			modelPath: "/model-source/draft.gguf",
			initContainers: []corev1.Container{{
				Name:         "model-downloader",
				Image:        "curl",
				VolumeMounts: []corev1.VolumeMount{{Name: "model-source", MountPath: "/model-source"}},
			}},
			volumes:      []corev1.Volume{pvcVolume("model-source", "draft-claim")},
			volumeMounts: []corev1.VolumeMount{{Name: "model-source", MountPath: "/model-source"}},
		}

		_, _ = mergeStorageConfigs(target, draft)

		if draft.initContainers[0].Name != "model-downloader" {
			t.Errorf("draft init container was renamed in place: %q", draft.initContainers[0].Name)
		}
		if draft.initContainers[0].VolumeMounts[0].Name != "model-source" {
			t.Errorf("draft init container mount was rewritten in place: %q", draft.initContainers[0].VolumeMounts[0].Name)
		}
		if draft.volumes[0].Name != "model-source" {
			t.Errorf("draft volume was renamed in place: %q", draft.volumes[0].Name)
		}
		if draft.volumeMounts[0].MountPath != "/model-source" {
			t.Errorf("draft mount was remounted in place: %q", draft.volumeMounts[0].MountPath)
		}
		if draft.modelPath != "/model-source/draft.gguf" {
			t.Errorf("draft modelPath was rewritten in place: %q", draft.modelPath)
		}
		if len(target.initContainers) != 1 || len(target.volumes) != 1 || len(target.volumeMounts) != 1 {
			t.Errorf("target was appended to in place")
		}
	})
}
