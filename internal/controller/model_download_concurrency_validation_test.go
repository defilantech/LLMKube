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

	It("admits downloadConcurrency with files and mmproj", func() {
		m := newModel("dlc-mmproj", 2, []string{"a.gguf"}, "proj.gguf")
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
	})

	// The downloader never runs for these, so the field would be ignored. An
	// mmproj with no files has no primary file to stage.
	It("rejects downloadConcurrency where the downloader does not stage spec.files", func() {
		for _, tc := range []struct {
			name, source, mmproj string
			files                []string
		}{
			{"dlc-single", "https://example.com/model.gguf", "", nil},
			{"dlc-mmproj-only", "hf://org/repo", "proj.gguf", nil},
			{"dlc-pvc", "pvc://claim/path", "", []string{"a.gguf", "b.gguf"}},
			{"dlc-oci", "oci://registry.example.com/models/m:1", "", []string{"a.gguf", "b.gguf"}},
		} {
			m := newModel(tc.name, 4, tc.files, tc.mmproj)
			m.Spec.Source = tc.source
			err := k8sClient.Create(ctx, m)
			Expect(err).To(HaveOccurred(), tc.name)
			Expect(err.Error()).To(ContainSubstring("downloadConcurrency sets how many spec.files the downloader stages at once"), tc.name)
		}
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

	It("admits downloadMaxInFlightSize with downloadConcurrency above 1", func() {
		for _, size := range []string{"90Gi", "999999Ti", "999999999999999"} {
			m := newModel("dlc-cap", 4, []string{"a.safetensors"}, "")
			m.Spec.DownloadMaxInFlightSize = size
			Expect(k8sClient.Create(ctx, m)).To(Succeed(), size)
			Expect(k8sClient.Delete(ctx, m)).To(Succeed(), size)
		}
	})

	It("rejects downloadMaxInFlightSize without downloadConcurrency above 1", func() {
		for name, concurrency := range map[string]int32{"dlc-cap-unset": 0, "dlc-cap-one": 1} {
			m := newModel(name, concurrency, []string{"a.safetensors"}, "")
			m.Spec.DownloadMaxInFlightSize = "90Gi"
			err := k8sClient.Create(ctx, m)
			Expect(err).To(HaveOccurred(), name)
			Expect(err.Error()).To(ContainSubstring("downloadMaxInFlightSize caps what the downloadConcurrency workers hold at once"), name)
		}
	})

	It("rejects a downloadMaxInFlightSize that is not a positive whole quantity", func() {
		// The bound keeps every admitted value inside int64: 10E parses to 0,
		// 99999999999999999999 wraps, and 8Ei saturates.
		for _, size := range []string{"0", "1.5Gi", "-1Gi", "90GiB", "10E", "8Ei", "1Pi", "1000000Ti", "99999999999999999999"} {
			m := newModel("dlc-cap-bad", 4, []string{"a.safetensors"}, "")
			m.Spec.DownloadMaxInFlightSize = size
			err := k8sClient.Create(ctx, m)
			Expect(err).To(HaveOccurred(), size)
			Expect(err.Error()).To(ContainSubstring("spec.downloadMaxInFlightSize"), size)
		}
	})
})
