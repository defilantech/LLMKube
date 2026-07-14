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
	})

	It("should emit the --aws-sigv4 curl line for s3 source without cache", func() {
		cmd := buildModelInitCommand(false, true, false, "")
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/${S3_KEY}"))
		Expect(cmd).To(ContainSubstring("Downloading model from S3"))
		Expect(cmd).To(ContainSubstring("Model downloaded successfully"))
		Expect(cmd).To(ContainSubstring("Model already exists, skipping download"))
	})

	It("should NOT emit --aws-sigv4 for non-s3 source", func() {
		cmd := buildModelInitCommand(false, false, true, "")
		Expect(cmd).ToNot(ContainSubstring("aws-sigv4"))
		Expect(cmd).To(ContainSubstring("curl -f -L -o \"$MODEL_PATH\" \"$MODEL_SOURCE\""))
	})

	It("should emit the --aws-sigv4 curl line for s3 source with OnChange refresh", func() {
		cmd := buildModelInitCommand(false, true, true, RefreshPolicyOnChange)
		Expect(cmd).To(ContainSubstring("curl --aws-sigv4"))
		Expect(cmd).To(ContainSubstring("${AWS_ENDPOINT_URL}/${S3_BUCKET}/${S3_KEY}"))
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
