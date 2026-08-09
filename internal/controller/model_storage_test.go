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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var _ = Describe("buildModelInitCommand (s3)", func() {
	It("should emit the --aws-sigv4 curl line for s3 source with cache", func() {
		cmd := buildModelInitCommand(false, true, true, "")
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/${S3_KEY}"))
		Expect(cmd).To(ContainSubstring("Downloading model from S3"))
		Expect(cmd).To(ContainSubstring("Model downloaded successfully"))
		Expect(cmd).To(ContainSubstring("Model already cached, skipping download"))
		Expect(cmd).To(ContainSubstring(`-o "$MODEL_PATH.tmp"`))
		Expect(cmd).To(ContainSubstring(`&& mv "$MODEL_PATH.tmp" "$MODEL_PATH"`))
	})

	It("should emit the --aws-sigv4 curl line for s3 source without cache", func() {
		cmd := buildModelInitCommand(false, true, false, "")
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/${S3_KEY}"))
		Expect(cmd).To(ContainSubstring("Downloading model from S3"))
		Expect(cmd).To(ContainSubstring("Model downloaded successfully"))
		Expect(cmd).To(ContainSubstring("Model already exists, skipping download"))
		Expect(cmd).To(ContainSubstring(`-o "$MODEL_PATH.tmp"`))
		Expect(cmd).To(ContainSubstring(`&& mv "$MODEL_PATH.tmp" "$MODEL_PATH"`))
	})

	It("should NOT emit --aws-sigv4 for non-s3 source", func() {
		cmd := buildModelInitCommand(false, false, true, "")
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
			cmd := buildModelInitCommand(tc[0], tc[1], tc[2], "")
			Expect(cmd).ToNot(ContainSubstring(`-o "$MODEL_PATH" `), cmd)
			Expect(cmd).ToNot(ContainSubstring(`cp /host-model/model.gguf "$MODEL_PATH" `), cmd)
		}
	})

	It("should emit the --aws-sigv4 curl line for s3 source with OnChange refresh", func() {
		cmd := buildModelInitCommand(false, true, true, RefreshPolicyOnChange)
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/${S3_KEY}"))
	})
})

// Issue #1435: interrupted transfers leave orphaned .tmp files that nothing
// cleans up. The init command must remove stale .tmp files before starting a
// new download so they do not accumulate on the shared cache PVC.
var _ = Describe("buildModelInitCommand (orphan .tmp cleanup, #1435)", func() {
	It("should remove stale .tmp before downloading in cached remote path", func() {
		cmd := buildModelInitCommand(false, false, true, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring(`rm -f "$MODEL_PATH.tmp"`))
	})

	It("should remove stale .tmp before downloading in cached S3 path", func() {
		cmd := buildModelInitCommand(false, true, true, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring(`rm -f "$MODEL_PATH.tmp"`))
	})

	It("should remove stale .tmp before downloading in cached local path", func() {
		cmd := buildModelInitCommand(true, false, true, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring(`rm -f "$MODEL_PATH.tmp"`))
	})

	It("should remove stale .tmp before downloading in cached OnChange path", func() {
		cmd := buildModelInitCommand(false, false, true, RefreshPolicyOnChange)
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
		cmd := buildMultiFileInitCommand(true, true, "")
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/"))
		Expect(cmd).To(ContainSubstring("${S3_PREFIX:+${S3_PREFIX}/}"))
		Expect(cmd).To(ContainSubstring("Downloading model artifact"))
		Expect(cmd).To(ContainSubstring("Model artifact"))
		Expect(cmd).To(ContainSubstring(`-o "$dest.tmp"`))
		Expect(cmd).To(ContainSubstring(`&& mv "$dest.tmp" "$dest"`))
	})

	It("should emit --aws-sigv4 for s3 source without cache (emptyDir)", func() {
		cmd := buildMultiFileInitCommand(false, true, "")
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/"))
		Expect(cmd).To(ContainSubstring("${S3_PREFIX:+${S3_PREFIX}/}"))
	})

	It("should emit --aws-sigv4 for s3 source with OnChange refresh", func() {
		cmd := buildMultiFileInitCommand(true, true, RefreshPolicyOnChange)
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/"))
		Expect(cmd).To(ContainSubstring("${S3_PREFIX:+${S3_PREFIX}/}"))
		Expect(cmd).To(ContainSubstring("revalidated"))
	})

	It("should NOT emit --aws-sigv4 for non-s3 source (HTTP regression)", func() {
		cmd := buildMultiFileInitCommand(true, false, "")
		Expect(cmd).ToNot(ContainSubstring("aws-sigv4"))
		Expect(cmd).To(ContainSubstring(`curl -f -L -o "$dest.tmp" "$url"`))
		Expect(cmd).To(ContainSubstring("${SOURCE%/}/$rel"))
	})

	It("should NOT emit --aws-sigv4 for non-s3 source with OnChange (HTTP regression)", func() {
		cmd := buildMultiFileInitCommand(true, false, RefreshPolicyOnChange)
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
