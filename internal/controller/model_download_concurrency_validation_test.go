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
*/package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// spec.downloadConcurrency sizes the multi-file worker pool (#2364). A Model
// that stages a single object has no pool, so the field would be ignored; the
// CEL rule rejects it there instead.
var _ = Describe("Model downloadConcurrency CRD validation", func() {
	ctx := context.Background()

	newModel := func(name string, concurrency int32, files []string, mmproj string) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:              "hf://org/repo",
				Files:               files,
				Mmproj:              mmproj,
				DownloadConcurrency: concurrency,
			},
		}
	}

	It("admits downloadConcurrency with files", func() {
		m := newModel("dlc-files", 4, []string{"a.safetensors", "b.safetensors"}, "")
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
	})

	It("admits downloadConcurrency with only mmproj", func() {
		m := newModel("dlc-mmproj", 2, nil, "proj.gguf")
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
	})

	It("rejects downloadConcurrency on a single-object Model", func() {
		m := newModel("dlc-single", 4, nil, "")
		m.Spec.Source = "https://example.com/model.gguf"
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("downloadConcurrency sets how many spec.files are staged at once"))
	})

	It("rejects downloadConcurrency above 16", func() {
		m := newModel("dlc-max", 17, []string{"a.safetensors"}, "")
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.downloadConcurrency"))
	})

	It("rejects a negative downloadConcurrency", func() {
		m := newModel("dlc-min", -1, []string{"a.safetensors"}, "")
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.downloadConcurrency"))
	})
})
