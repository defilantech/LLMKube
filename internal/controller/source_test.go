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
)

var _ = Describe("isPVCSource", func() {
	It("should return true for pvc:// prefix", func() {
		Expect(isPVCSource("pvc://my-claim/model.gguf")).To(BeTrue())
	})
	It("should return true for pvc:// with nested path", func() {
		Expect(isPVCSource("pvc://claim/deep/nested/path/model.gguf")).To(BeTrue())
	})
	It("should return false for http://", func() {
		Expect(isPVCSource("http://example.com/model.gguf")).To(BeFalse())
	})
	It("should return false for https://", func() {
		Expect(isPVCSource("https://example.com/model.gguf")).To(BeFalse())
	})
	It("should return false for file://", func() {
		Expect(isPVCSource("file:///mnt/models/model.gguf")).To(BeFalse())
	})
	It("should return false for absolute path", func() {
		Expect(isPVCSource("/mnt/models/model.gguf")).To(BeFalse())
	})
	It("should return false for empty string", func() {
		Expect(isPVCSource("")).To(BeFalse())
	})
})

var _ = Describe("parsePVCSource", func() {
	It("should parse simple pvc source", func() {
		claim, path, err := parsePVCSource("pvc://my-claim/model.gguf")
		Expect(err).NotTo(HaveOccurred())
		Expect(claim).To(Equal("my-claim"))
		Expect(path).To(Equal("model.gguf"))
	})

	It("should parse nested path", func() {
		claim, path, err := parsePVCSource("pvc://shared-storage/models/llama/7b/model.gguf")
		Expect(err).NotTo(HaveOccurred())
		Expect(claim).To(Equal("shared-storage"))
		Expect(path).To(Equal("models/llama/7b/model.gguf"))
	})

	It("should error on non-PVC source", func() {
		_, _, err := parsePVCSource("http://example.com/model.gguf")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not a PVC source"))
	})

	It("should error on empty PVC source", func() {
		_, _, err := parsePVCSource("pvc://")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty PVC source"))
	})

	It("should error on missing file path", func() {
		_, _, err := parsePVCSource("pvc://my-claim")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("must include a file path"))
	})

	It("should error on empty claim name", func() {
		_, _, err := parsePVCSource("pvc:///model.gguf")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty claim name"))
	})

	It("should error on trailing slash only (empty path)", func() {
		_, _, err := parsePVCSource("pvc://my-claim/")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty file path"))
	})
})

var _ = Describe("isLocalSource (source.go)", func() {
	It("should return true for file:// prefix", func() {
		Expect(isLocalSource("file:///mnt/models/test.gguf")).To(BeTrue())
	})
	It("should return true for absolute path", func() {
		Expect(isLocalSource("/mnt/models/test.gguf")).To(BeTrue())
	})
	It("should return false for http://", func() {
		Expect(isLocalSource("http://example.com/model.gguf")).To(BeFalse())
	})
	It("should return false for pvc://", func() {
		Expect(isLocalSource("pvc://claim/model.gguf")).To(BeFalse())
	})
	It("should return false for empty string", func() {
		Expect(isLocalSource("")).To(BeFalse())
	})
})

var _ = Describe("getLocalPath (source.go)", func() {
	It("should strip file:// prefix", func() {
		Expect(getLocalPath("file:///mnt/models/test.gguf")).To(Equal("/mnt/models/test.gguf"))
	})
	It("should return absolute path as-is", func() {
		Expect(getLocalPath("/mnt/models/test.gguf")).To(Equal("/mnt/models/test.gguf"))
	})
})
