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
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The verify helpers are the whole of the #1965 enforcement surface: a
// refactor that flips a withSHA256 argument or drops the digest env turns
// verification off without failing anything else. These table tests pin the
// wiring at the storage-config builders (the pod path) and the prefetch Job
// builder (the eager path) for every branch the builders emit.

const (
	sha256WiredDigestUpper = "D9BA44419F2A73ED1A666885066C65A235AB70F337E2B31CBB3D062A5F5B8D4B"
	sha256WiredDigestLower = "d9ba44419f2a73ed1a666885066c65a235ab70f337e2b31cbb3d062a5f5b8d4b"
)

var sha256HelperTokens = []string{
	"llmkube_sha256_hash",
	"llmkube_precheck_sha256",
	"llmkube_atomic_write",
	"llmkube_marker_hit_sha256",
	"llmkube_check_sha256",
	"llmkube_publish_sha256",
	"llmkube_precheck_file_sha256",
	"llmkube_file_digest",
	"llmkube_accept_file",
	"llmkube_publish_file",
}

func sha256WiringModel(source string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "wired", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: source,
			SHA256: sha256WiredDigestUpper,
		},
	}
}

func downloaderFrom(cfg modelStorageConfig) (corev1.Container, string) {
	for _, c := range cfg.initContainers {
		if c.Name == "model-downloader" && len(c.Command) == 3 {
			return c, c.Command[2]
		}
	}
	return corev1.Container{}, ""
}

func envValueFor(envs []corev1.EnvVar, name string) (string, bool) {
	for _, e := range envs {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func assertDigestEnv(t *testing.T, envs []corev1.EnvVar) {
	t.Helper()
	got, ok := envValueFor(envs, "MODEL_SHA256")
	if !ok {
		t.Fatalf("MODEL_SHA256 not injected: %v", envs)
	}
	if got != sha256WiredDigestLower {
		t.Errorf("MODEL_SHA256 = %q, want lowercased %q", got, sha256WiredDigestLower)
	}
}

func TestModelStorageConfig_SHA256Wiring(t *testing.T) {
	cases := []struct {
		name     string
		useCache bool
		source   string
		policy   string
		// wantPublish is the exact publish call the branch must make. Asserting
		// the call site, not the helper name, is what catches a branch that
		// stopped calling the gate: the helper definitions are stripped first.
		wantPublish string
	}{
		{"cached-http-ifnotpresent", true, "https://models.example.com/model.gguf", RefreshPolicyIfNotPresent, `llmkube_publish_sha256 "$MODEL_PARTIAL" "$MODEL_PATH"`},
		{"cached-http-onchange", true, "https://models.example.com/model.gguf", RefreshPolicyOnChange, `llmkube_publish_sha256 "$MODEL_PARTIAL" "$MODEL_PATH"`},
		{"cached-s3", true, "s3://models/repo/model.gguf", RefreshPolicyIfNotPresent, `llmkube_publish_sha256 "$MODEL_PATH.tmp" "$MODEL_PATH"`},
		{"cached-hf-ifnotpresent", true, "https://huggingface.co/org/repo/resolve/main/model.gguf", RefreshPolicyIfNotPresent, `llmkube_publish_sha256 "$MODEL_PARTIAL" "$MODEL_PATH"`},
		{"cached-hf-onchange", true, "https://huggingface.co/org/repo/resolve/main/model.gguf", RefreshPolicyOnChange, `llmkube_publish_sha256 "$MODEL_PARTIAL" "$MODEL_PATH"`},
		{"cached-local", true, "file:///data/models/model.gguf", RefreshPolicyIfNotPresent, `llmkube_publish_sha256 "$MODEL_PATH.tmp" "$MODEL_PATH"`},
		{"uncached-http-ifnotpresent", false, "https://models.example.com/model.gguf", RefreshPolicyIfNotPresent, `llmkube_publish_sha256 "$MODEL_PARTIAL" "$MODEL_PATH"`},
		{"uncached-http-onchange", false, "https://models.example.com/model.gguf", RefreshPolicyOnChange, `llmkube_publish_sha256 "$MODEL_PARTIAL" "$MODEL_PATH"`},
		{"uncached-s3", false, "s3://models/repo/model.gguf", RefreshPolicyIfNotPresent, `llmkube_publish_sha256 "$MODEL_PATH.tmp" "$MODEL_PATH"`},
		{"uncached-hf-ifnotpresent", false, "https://huggingface.co/org/repo/resolve/main/model.gguf", RefreshPolicyIfNotPresent, `llmkube_publish_sha256 "$MODEL_PARTIAL" "$MODEL_PATH"`},
		{"uncached-hf-onchange", false, "https://huggingface.co/org/repo/resolve/main/model.gguf", RefreshPolicyOnChange, `llmkube_publish_sha256 "$MODEL_PARTIAL" "$MODEL_PATH"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := sha256WiringModel(tc.source)
			model.Spec.RefreshPolicy = tc.policy
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			}
			cfg := buildModelStorageConfig(model, isvc, "default", tc.useCache, ModelCacheModeShared, "", "img:latest", 0, ModelCachePrepAlways, []string{"/data/models"}, "")

			downloader, cmd := downloaderFrom(cfg)
			if downloader.Name == "" {
				t.Fatalf("no model-downloader container in %+v", cfg.initContainers)
			}
			assertDigestEnv(t, downloader.Env)
			// Strip the helper definitions so the assertions below pin the
			// call sites, not the const that defines every helper.
			calls := strings.TrimPrefix(cmd, sha256VerifyFns)
			if calls == cmd {
				t.Fatalf("sha-enabled command does not begin with the verify fragments:\n%s", cmd)
			}
			for _, want := range []string{
				`llmkube_precheck_sha256 || exit 1`,
				`llmkube_marker_hit_sha256 "$MODEL_PATH"`,
				`llmkube_check_sha256 "$MODEL_PATH"`,
				tc.wantPublish,
			} {
				if !strings.Contains(calls, want) {
					t.Errorf("sha-enabled command is missing the call %q; gates cannot be silently dropped:\n%s", want, calls)
				}
			}
			if strings.Contains(tc.source, "s3://") && !strings.Contains(cmd, "--aws-sigv4") {
				t.Errorf("S3 branch lost its sigv4 transfer")
			}
		})
	}
}

func TestModelStorageConfig_NoDigestStaysUnwired(t *testing.T) {
	for _, source := range []string{
		"https://models.example.com/model.gguf",
		"s3://models/repo/model.gguf",
		"file:///data/models/model.gguf",
		"https://huggingface.co/org/repo/resolve/main/model.gguf",
	} {
		for _, useCache := range []bool{true, false} {
			if !useCache && strings.HasPrefix(source, "file://") {
				continue // no cache to copy into: the error branch, no gates expected
			}
			for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
				model := sha256WiringModel(source)
				model.Spec.SHA256 = ""
				model.Spec.RefreshPolicy = policy
				isvc := &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
				}
				cfg := buildModelStorageConfig(model, isvc, "default", useCache, ModelCacheModeShared, "", "img:latest", 0, ModelCachePrepAlways, []string{"/data/models"}, "")
				downloader, cmd := downloaderFrom(cfg)
				if downloader.Name == "" {
					t.Fatalf("no model-downloader container (source=%s useCache=%v policy=%s)", source, useCache, policy)
				}
				if v, ok := envValueFor(downloader.Env, "MODEL_SHA256"); ok {
					t.Errorf("MODEL_SHA256 = %q without spec.sha256 (source=%s useCache=%v policy=%s)", v, source, useCache, policy)
				}
				for _, tok := range sha256HelperTokens {
					if strings.Contains(cmd, tok) {
						t.Errorf("unhashed command contains %q (source=%s useCache=%v policy=%s)", tok, source, useCache, policy)
					}
				}
			}
		}
	}
}

func TestPrefetchJob_SHA256Wiring(t *testing.T) {
	build := func(t *testing.T, sha256 string) (corev1.Container, string) {
		t.Helper()
		model := sha256WiringModel("https://models.example.com/model.gguf")
		model.Spec.SHA256 = sha256
		seedPrefetchCacheKey(model)
		r := &ModelReconciler{InitContainerImage: "img:latest"}
		job, err := r.buildPrefetchJob(context.Background(), model, ModelCachePVCName, "")
		if err != nil {
			t.Fatalf("buildPrefetchJob: %v", err)
		}
		cfg := modelStorageConfig{initContainers: job.Spec.Template.Spec.InitContainers}
		downloader, cmd := downloaderFrom(cfg)
		if downloader.Name == "" {
			t.Fatalf("prefetch Job has no model-downloader init container")
		}
		return downloader, cmd
	}

	t.Run("digest set", func(t *testing.T) {
		downloader, cmd := build(t, sha256WiredDigestUpper)
		assertDigestEnv(t, downloader.Env)
		for _, tok := range []string{"llmkube_check_sha256", "llmkube_publish_sha256"} {
			if !strings.Contains(cmd, tok) {
				t.Errorf("prefetch command is missing %q:\n%s", tok, cmd)
			}
		}
	})
	t.Run("digest unset", func(t *testing.T) {
		downloader, cmd := build(t, "")
		if _, ok := envValueFor(downloader.Env, "MODEL_SHA256"); ok {
			t.Errorf("prefetch injected MODEL_SHA256 without spec.sha256")
		}
		for _, tok := range sha256HelperTokens {
			if strings.Contains(cmd, tok) {
				t.Errorf("prefetch command of an unhashed Model contains %q", tok)
			}
		}
	})
}

// TestModelStorageConfig_MultiFileDigestWiring pins the per-file gate wiring at
// the storage config builder for a multi-file Model (#1978). It drives every
// {cached, emptyDir} x {http, s3, hf} x {IfNotPresent, OnChange} combination and
// asserts the call sites, not the helper names: the verify consts are stripped
// from the command first, so a branch that stopped calling a gate fails here
// even though the const still defines the helper.
func TestModelStorageConfig_MultiFileDigestWiring(t *testing.T) {
	kinds := []struct {
		name   string
		source string
		isS3   bool
	}{
		{"http", "https://models.example.com/repo", false},
		{"s3", "s3://bucket/repo", true},
		{"hf", "hf://org/repo", false},
	}
	for _, useCache := range []bool{true, false} {
		for _, kind := range kinds {
			for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
				name := "usecache-" + boolLabel(useCache) + "_" + kind.name + "_" + policy
				t.Run(name, func(t *testing.T) {
					model := &inferencev1alpha1.Model{
						ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
						Spec: inferencev1alpha1.ModelSpec{
							Source:        kind.source,
							Files:         []string{"a.gguf", "b.gguf"},
							FileSHA256:    map[string]inferencev1alpha1.SHA256Digest{"a.gguf": inferencev1alpha1.SHA256Digest(sha256WiredDigestUpper)},
							RefreshPolicy: policy,
						},
					}
					isvc := &inferencev1alpha1.InferenceService{
						ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
					}
					cfg := buildModelStorageConfig(model, isvc, "default", useCache, ModelCacheModeShared, "", "img:latest", 0, ModelCachePrepAlways, []string{"/data/models"}, "")
					downloader, cmd := downloaderFrom(cfg)
					if downloader.Name == "" {
						t.Fatalf("no multi-file model-downloader container")
					}
					if got, ok := envValueFor(downloader.Env, "MODEL_FILE_SHA256"); !ok || got != sha256WiredDigestLower+" a.gguf" {
						t.Errorf("MODEL_FILE_SHA256 = %q (found=%v), want %q", got, ok, sha256WiredDigestLower+" a.gguf")
					}
					if got, ok := envValueFor(downloader.Env, "MODEL_DIGEST_FIELD"); !ok || got != "spec.fileSha256" {
						t.Errorf("MODEL_DIGEST_FIELD = %q (found=%v), want %q", got, ok, "spec.fileSha256")
					}
					calls := strings.TrimPrefix(cmd, sha256VerifyFns+sha256MultiFileFns)
					if calls == cmd {
						t.Fatalf("gated multi-file command does not begin with the verify fragments:\n%s", cmd)
					}
					for _, want := range []string{
						`llmkube_precheck_file_sha256 || exit 1`,
						`llmkube_file_digest "$rel"`,
					} {
						if !strings.Contains(calls, want) {
							t.Errorf("gated multi-file command is missing the call %q:\n%s", want, calls)
						}
					}
					wantPublish := `llmkube_publish_file "$MODEL_PARTIAL" "$dest"`
					if kind.isS3 {
						wantPublish = `llmkube_publish_file "$dest.tmp" "$dest"`
					}
					if !strings.Contains(calls, wantPublish) {
						t.Errorf("gated multi-file %s command is missing the publish call %q:\n%s", kind.name, wantPublish, calls)
					}
					for _, banned := range []string{`mv "$dest.tmp" "$dest"`, `mv "$MODEL_PARTIAL" "$dest"`} {
						if strings.Contains(calls, banned) {
							t.Errorf("gated multi-file command contains the unverified publish %q:\n%s", banned, calls)
						}
					}
					if policy == RefreshPolicyOnChange {
						if !strings.Contains(calls, `[ "$remote_size" != "0" ] && llmkube_accept_file "$dest"; then`) {
							t.Errorf("OnChange size-match is not gated by llmkube_accept_file:\n%s", calls)
						}
					} else if !strings.Contains(calls, `if [ -f "$dest" ] && { if [ -n "${MODEL_SHA256:-}" ]; then llmkube_accept_file "$dest"; else llmkube_cached_size_matches; fi; }; then`) {
						t.Errorf("IfNotPresent cache probe must verify pinned digests and check unpinned sizes:\n%s", calls)
					}
				})
			}
		}
	}
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestModelStorageConfig_MultiFileNoDigestStaysUnwired keeps the negative half:
// a multi-file Model with no spec.fileSha256 must not gain a verify helper or a
// MODEL_FILE_SHA256 env, in either storage shape.
func TestModelStorageConfig_MultiFileNoDigestStaysUnwired(t *testing.T) {
	for _, useCache := range []bool{true, false} {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://models.example.com/repo",
				Files:  []string{"a.gguf", "b.gguf"},
			},
		}
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		}
		cfg := buildModelStorageConfig(model, isvc, "default", useCache, ModelCacheModeShared, "", "img:latest", 0, ModelCachePrepAlways, []string{"/data/models"}, "")
		downloader, cmd := downloaderFrom(cfg)
		if downloader.Name == "" {
			t.Fatalf("no multi-file model-downloader container (useCache=%v)", useCache)
		}
		if v, ok := envValueFor(downloader.Env, "MODEL_FILE_SHA256"); ok {
			t.Errorf("MODEL_FILE_SHA256 = %q without spec.fileSha256 (useCache=%v)", v, useCache)
		}
		if v, ok := envValueFor(downloader.Env, "MODEL_DIGEST_FIELD"); ok {
			t.Errorf("MODEL_DIGEST_FIELD = %q without spec.fileSha256 (useCache=%v)", v, useCache)
		}
		for _, tok := range sha256HelperTokens {
			if strings.Contains(cmd, tok) {
				t.Errorf("unhashed multi-file command contains %q (useCache=%v)", tok, useCache)
			}
		}
	}
}

// TestPVCStorageConfig_SHA256Wiring pins the pvc:// verify wiring (#1979): a
// digest adds a verify init container that must not try to stamp the read-only
// mount; no digest adds nothing.
func TestPVCStorageConfig_SHA256Wiring(t *testing.T) {
	build := func(t *testing.T, sha string) modelStorageConfig {
		t.Helper()
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "pvc://models-pvc/model.gguf",
				SHA256: sha,
			},
		}
		return buildModelStorageConfig(model, nil, "default", true, ModelCacheModeShared, "", "img:latest", 0, ModelCachePrepAlways, nil, "")
	}

	t.Run("digest set", func(t *testing.T) {
		cfg := build(t, sha256WiredDigestUpper)
		downloader, cmd := downloaderFrom(cfg)
		if downloader.Name == "" {
			t.Fatalf("pvc:// Model with spec.sha256 has no verify init container")
		}
		assertDigestEnv(t, downloader.Env)
		for _, tok := range []string{"llmkube_precheck_sha256", "llmkube_sha256_hash"} {
			if !strings.Contains(cmd, tok) {
				t.Errorf("pvc verify command is missing %q:\n%s", tok, cmd)
			}
		}
		if strings.Contains(cmd, "llmkube_check_sha256") || strings.Contains(cmd, "llmkube_publish_sha256") {
			t.Errorf("pvc verify command must not try to stamp a read-only mount:\n%s", cmd)
		}
	})

	t.Run("digest unset", func(t *testing.T) {
		cfg := build(t, "")
		if len(cfg.initContainers) != 0 {
			t.Errorf("pvc:// Model without spec.sha256 grew init containers: %+v", cfg.initContainers)
		}
	})
}
