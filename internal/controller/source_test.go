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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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
	// URL schemes are case-insensitive (RFC 3986); a case-variant file://
	// must still classify as local so it cannot dodge the hostPath
	// allowlist check (GHSA-jw3m-8q7m-f35r).
	It("should return true for case-variant FILE:// prefix", func() {
		Expect(isLocalSource("FILE:///mnt/models/test.gguf")).To(BeTrue())
		Expect(isLocalSource("File:///mnt/models/test.gguf")).To(BeTrue())
	})
})

var _ = Describe("getLocalPath (source.go)", func() {
	It("should strip file:// prefix", func() {
		Expect(getLocalPath("file:///mnt/models/test.gguf")).To(Equal("/mnt/models/test.gguf"))
	})
	It("should return absolute path as-is", func() {
		Expect(getLocalPath("/mnt/models/test.gguf")).To(Equal("/mnt/models/test.gguf"))
	})
	It("should strip a case-variant FILE:// prefix, agreeing with isLocalSource", func() {
		Expect(getLocalPath("FILE:///mnt/models/test.gguf")).To(Equal("/mnt/models/test.gguf"))
	})
})

var _ = Describe("isHFRepoSource (source.go)", func() {
	It("should return true for TinyLlama repo ID", func() {
		Expect(isHFRepoSource("TinyLlama/TinyLlama-1.1B-Chat-v1.0")).To(BeTrue())
	})
	It("should return true for Qwen repo ID", func() {
		Expect(isHFRepoSource("Qwen/Qwen3.6-35B-A3B")).To(BeTrue())
	})
	It("should return true for bartowski repo ID", func() {
		Expect(isHFRepoSource("bartowski/Qwen_Qwen3.6-35B-A3B-GGUF")).To(BeTrue())
	})
	It("should return true for hf:// prefixed repo ID", func() {
		Expect(isHFRepoSource("hf://unsloth/gemma-4-31B-it-GGUF")).To(BeTrue())
	})
	It("should return true for hf:// with multi-part path", func() {
		Expect(isHFRepoSource("hf://org/deep/nested/repo")).To(BeTrue())
	})
	It("should return false for https URL", func() {
		Expect(isHFRepoSource("https://example.com/model.gguf")).To(BeFalse())
	})
	It("should return false for http URL", func() {
		Expect(isHFRepoSource("http://example.com/model.gguf")).To(BeFalse())
	})
	It("should return false for absolute path", func() {
		Expect(isHFRepoSource("/models/local.gguf")).To(BeFalse())
	})
	It("should return false for file:// URL", func() {
		Expect(isHFRepoSource("file:///models/local.gguf")).To(BeFalse())
	})
	It("should return false for PVC source", func() {
		Expect(isHFRepoSource("pvc://my-claim/model.gguf")).To(BeFalse())
	})
	It("should return false for filename without slash", func() {
		Expect(isHFRepoSource("just-a-filename")).To(BeFalse())
	})
	It("should return false for hf:// without slash (bare name)", func() {
		Expect(isHFRepoSource("hf://just-a-name")).To(BeFalse())
	})
	It("should return false for empty string", func() {
		Expect(isHFRepoSource("")).To(BeFalse())
	})
	It("should return true for multi-part nested path", func() {
		Expect(isHFRepoSource("multi/part/path/thing")).To(BeTrue())
	})
})

var _ = Describe("validateHFRepoSource (source.go)", func() {
	It("should return nil for valid bare repo ID", func() {
		Expect(validateHFRepoSource("org/repo")).To(Succeed())
	})
	It("should return nil for valid hf:// prefixed repo ID", func() {
		Expect(validateHFRepoSource("hf://org/repo")).To(Succeed())
	})
	It("should return error for hf:// with @rev", func() {
		err := validateHFRepoSource("hf://org/repo@main")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("@rev"))
	})
	It("should return error for bare repo ID with @rev", func() {
		err := validateHFRepoSource("org/repo@v1.0")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("@rev"))
	})
})

var _ = Describe("normalizeHFSource (source.go)", func() {
	It("should strip hf:// prefix", func() {
		Expect(normalizeHFSource("hf://org/repo")).To(Equal("org/repo"))
	})
	It("should leave bare repo ID unchanged", func() {
		Expect(normalizeHFSource("org/repo")).To(Equal("org/repo"))
	})
	It("should leave non-hf source unchanged", func() {
		Expect(normalizeHFSource("https://example.com/model.gguf")).To(Equal("https://example.com/model.gguf"))
	})
})

var _ = Describe("isRemoteHTTPSource (source.go)", func() {
	// Regression coverage for issue #363: the controller defers HTTP(S)
	// sources to the workload init container so the per-namespace cache PVC
	// is populated. If a future change widens or narrows what this matcher
	// considers HTTP(S), the dispatch in Reconcile() flips silently, so this
	// matcher needs explicit, exhaustive cases.
	It("should return true for https URL", func() {
		Expect(isRemoteHTTPSource("https://huggingface.co/org/repo/resolve/main/model.gguf")).To(BeTrue())
	})
	It("should return true for http URL", func() {
		Expect(isRemoteHTTPSource("http://example.com/model.gguf")).To(BeTrue())
	})
	It("should return true for https URL with port and query", func() {
		Expect(isRemoteHTTPSource("https://my-mirror.local:8443/m.gguf?token=abc")).To(BeTrue())
	})
	It("should return false for HuggingFace repo ID", func() {
		Expect(isRemoteHTTPSource("Qwen/Qwen3.6-35B-A3B")).To(BeFalse())
	})
	It("should return false for file:// URL", func() {
		Expect(isRemoteHTTPSource("file:///mnt/models/local.gguf")).To(BeFalse())
	})
	It("should return false for absolute path", func() {
		Expect(isRemoteHTTPSource("/mnt/models/local.gguf")).To(BeFalse())
	})
	It("should return false for pvc:// source", func() {
		Expect(isRemoteHTTPSource("pvc://my-claim/path/model.gguf")).To(BeFalse())
	})
	It("should return false for empty string", func() {
		Expect(isRemoteHTTPSource("")).To(BeFalse())
	})
	It("should return false for ftp:// URL (out of scope for the workload init container)", func() {
		Expect(isRemoteHTTPSource("ftp://example.com/model.gguf")).To(BeFalse())
	})
	// URL schemes are case-insensitive (RFC 3986) and url.Parse lowercases
	// them, so http.Client happily fetches "HTTP://..." URLs. The classifier
	// must agree or a case-variant scheme dodges the guarded remote-source
	// routing (GHSA-jw3m-8q7m-f35r).
	It("should return true for case-variant HTTP:// scheme", func() {
		Expect(isRemoteHTTPSource("HTTP://x")).To(BeTrue())
	})
	It("should return true for case-variant HtTpS:// scheme", func() {
		Expect(isRemoteHTTPSource("HtTpS://x")).To(BeTrue())
	})
	It("source-type matchers must be mutually exclusive", func() {
		// Architectural invariant: every reachable source falls into exactly
		// one category. If this regresses, Reconcile()'s dispatch order
		// becomes load-bearing and silent bugs creep in.
		cases := []string{
			"https://huggingface.co/org/repo/resolve/main/m.gguf",
			"http://mirror.local/m.gguf",
			"Qwen/Qwen3.6-35B-A3B",
			"hf://org/repo",
			"file:///mnt/models/m.gguf",
			"/mnt/models/m.gguf",
			"pvc://my-claim/path/m.gguf",
		}
		for _, src := range cases {
			matchCount := 0
			if isPVCSource(src) {
				matchCount++
			}
			if isHFRepoSource(src) {
				matchCount++
			}
			if isRemoteHTTPSource(src) {
				matchCount++
			}
			if isLocalSource(src) {
				matchCount++
			}
			Expect(matchCount).To(Equal(1), "source %q must match exactly one category, got %d", src, matchCount)
		}
	})
})

var _ = Describe("isUnrecoverableFetchError (source.go)", func() {
	It("should return false for nil error", func() {
		Expect(isUnrecoverableFetchError(nil)).To(BeFalse())
	})

	It("should return true for fs.ErrNotExist directly", func() {
		Expect(isUnrecoverableFetchError(fs.ErrNotExist)).To(BeTrue())
	})

	It("should return true for fs.ErrPermission directly", func() {
		Expect(isUnrecoverableFetchError(fs.ErrPermission)).To(BeTrue())
	})

	It("should unwrap fmt.Errorf-wrapped fs.ErrNotExist (the #405 path)", func() {
		// This is the exact wrap shape that copyLocalModel produces and
		// that pinned a Mac kind cluster's CPU for 35 hours. If this
		// assertion ever regresses, the hot-spin guard is silently
		// disabled.
		_, openErr := os.Open(filepath.Join(GinkgoT().TempDir(), "definitely-does-not-exist.gguf"))
		Expect(openErr).To(HaveOccurred())
		wrapped := fmt.Errorf("failed to open local model file: %w", openErr)
		Expect(isUnrecoverableFetchError(wrapped)).To(BeTrue())
	})

	It("should return false for a generic non-filesystem error", func() {
		Expect(isUnrecoverableFetchError(errors.New("network timeout"))).To(BeFalse())
	})

	It("should return false for a wrapped non-filesystem error", func() {
		Expect(isUnrecoverableFetchError(fmt.Errorf("download failed: %w", errors.New("503")))).To(BeFalse())
	})

	It("should return true for double-wrapped fs.ErrNotExist", func() {
		// errors.Is walks the wrap chain, so even a deeply-wrapped
		// not-exist error must be detected.
		inner := fmt.Errorf("inner: %w", fs.ErrNotExist)
		outer := fmt.Errorf("outer: %w", inner)
		Expect(isUnrecoverableFetchError(outer)).To(BeTrue())
	})
})
