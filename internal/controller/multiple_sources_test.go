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
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestMultipleSourcesDownloader(t *testing.T) {
	requireInitShellEnvironment(t)
	for _, policy := range []string{"IfNotPresent", "OnChange"} {
		for _, withSource := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/source=%t", policy, withSource), func(t *testing.T) {
				const body = "model bytes"
				mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer secret" {
						t.Errorf("mirror auth = %q", r.Header.Get("Authorization"))
					}
					if r.URL.Path != "/org/repo/resolve/main/weights.gguf" {
						t.Errorf("mirror path = %s", r.URL.Path)
					}
					w.Header().Set("Content-Length", fmt.Sprint(len(body)))
					if r.Method != http.MethodHead {
						_, _ = w.Write([]byte(body))
					}
				}))
				defer mirror.Close()
				external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "" {
						t.Errorf("leaked credentials to external host: %q", r.Header.Get("Authorization"))
					}
					if r.URL.Path != "/other/encoder.bin" {
						t.Errorf("external path = %s", r.URL.Path)
					}
					w.Header().Set("Content-Length", fmt.Sprint(len(body)))
					if r.Method != http.MethodHead {
						_, _ = w.Write([]byte(body))
					}
				}))
				defer external.Close()
				objectStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
						t.Errorf("S3 auth = %q", r.Header.Get("Authorization"))
					}
					if r.URL.Path != "/bucket/vae/vae.bin" {
						t.Errorf("S3 path = %s", r.URL.Path)
					}
					w.Header().Set("Content-Length", fmt.Sprint(len(body)))
					if r.Method != http.MethodHead {
						_, _ = w.Write([]byte(body))
					}
				}))
				defer objectStore.Close()
				files := []string{mirror.URL + "/org/repo/resolve/main/weights.gguf", external.URL + "/other/encoder.bin?download=1", "s3://bucket/vae/vae.bin"}
				source := ""
				if withSource {
					source = "hf://org/repo"
					files[0] = "weights.gguf"
				}
				model := &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{Source: source, Files: files, RefreshPolicy: policy,
					FileSHA256: map[string]inferencev1alpha1.SHA256Digest{files[1]: inferencev1alpha1.SHA256Digest(fmt.Sprintf("%x", sha256.Sum256([]byte(body))))}}}
				isvc := &inferencev1alpha1.InferenceService{}
				cfg := buildModelStorageConfig(model, isvc, "default", true, ModelCacheModeShared, "", "img:latest", 0, ModelCachePrepNever, DefaultInitContainerUser, nil, mirror.URL)
				container, _ := downloaderFrom(cfg)
				if !strings.HasSuffix(cfg.modelPath, "/weights.gguf") {
					t.Fatalf("runtime model path=%s", cfg.modelPath)
				}
				dir := t.TempDir()
				for attempt := 0; attempt < 2; attempt++ {
					out, err := runDownloaderOnFS(t, &container, dir, "ext4", "HF_TOKEN=secret", "AWS_ACCESS_KEY_ID=access", "AWS_SECRET_ACCESS_KEY=key", "AWS_REGION=us-east-1", "AWS_ENDPOINT_URL="+objectStore.URL)
					if err != nil {
						t.Fatalf("download: %v\n%s", err, out)
					}
					for _, name := range []string{"weights.gguf", "encoder.bin", "vae/vae.bin"} {
						got, err := os.ReadFile(filepath.Join(dir, name))
						if err != nil || string(got) != body {
							t.Errorf("%s: bytes=%q err=%v", name, got, err)
						}
					}
				}
			})
		}
	}
}

func TestMultipleSourcesRejectInvalidPaths(t *testing.T) {
	for _, files := range [][]string{
		{"https://example.com/a.bin", "https://other.com/a.bin"},
		{"a.bin", "https://example.com/a.bin"},
		{"./a.bin", "https://example.com/a.bin"},
		{"https://huggingface.co/org/repo/resolve/main/%2e%2e/escape.bin"},
		{"https://example.com/%0ainjected"},
		{"file:///tmp/model.bin"},
		{"s3://bucket/"},
	} {
		if _, err := ResolveFileSet(files, "", nil); err == nil {
			t.Errorf("accepted %v", files)
		}
	}
}

func TestMultipleSourcesAdmissionAndReconcile(t *testing.T) {
	c, _, _ := sharedTestEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	ns := newTestNamespace(t, c)
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		source string
		files  []string
		valid  bool
	}{
		{"urls-only", "", []string{"https://example.com/model.bin", "s3://bucket/vae/vae.bin"}, true},
		{"mixed", "hf://org/repo", []string{"model.bin", "https://example.com/encoder.bin"}, true},
		{"relative-without-source", "", []string{"model.bin"}, false},
		{"no-source-or-files", "", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: ns}, Spec: inferencev1alpha1.ModelSpec{Source: tc.source, Files: tc.files, Format: "safetensors"}}
			err := c.Create(ctx, m)
			if !tc.valid {
				if err == nil {
					t.Fatal("invalid Model admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r := &ModelReconciler{Client: c, Scheme: c.Scheme()}
			for i := 0; i < 3; i++ {
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(m), m); err != nil {
				t.Fatal(err)
			}
			if m.Status.Phase != PhaseReady {
				t.Fatalf("phase=%s conditions=%+v", m.Status.Phase, m.Status.Conditions)
			}
			plan, err := modelStagingPlan(m)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(m.Status.StagedFiles, plan.Files) {
				t.Fatalf("staged files=%v want=%v", m.Status.StagedFiles, plan.Files)
			}
			if effectiveModelCacheKey(m) != cachekey.ModelSourceKey(m) {
				t.Fatal("cache key does not identify file sources")
			}
		})
	}
}

func TestMultipleSourcesPrefetchAndGlob(t *testing.T) {
	const external = "https://huggingface.co/other/repo/resolve/main/encoders/encoder.bin"
	m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Spec: inferencev1alpha1.ModelSpec{Source: "hf://org/repo", Files: []string{"weights.bin", "*.json", external}}}
	plan, err := ResolveFileSet(m.Spec.Files, "", []string{"weights.bin", "config.json"})
	if err != nil {
		t.Fatal(err)
	}
	m.Status.StagedFiles = plan.Files
	m.Status.Conditions = []metav1.Condition{{Type: ConditionAvailable, ObservedGeneration: 2}}
	for _, model := range []*inferencev1alpha1.Model{m, prefetchJobModel(m, plan)} {
		restored, err := modelStagingPlan(model)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(restored, plan) {
			t.Fatalf("lost explicit source: got=%+v want=%+v", restored, plan)
		}
	}
	before := effectiveModelCacheKey(m)
	m.Spec.Files[2] = "https://huggingface.co/replacement/repo/resolve/main/encoders/encoder.bin"
	if effectiveModelCacheKey(m) == before {
		t.Fatal("changed origin reused cache directory")
	}
}

// Removing the storage-boundary validation must fail this test: malformed or
// unsupported sources must not produce a downloader or a pre-staged mount.
func TestMultipleSourcesStorageRejectsInvalidSources(t *testing.T) {
	cases := []struct {
		name string
		spec inferencev1alpha1.ModelSpec
	}{
		{"relative without source", inferencev1alpha1.ModelSpec{Files: []string{"model.gguf"}}},
		{"mmproj without source", inferencev1alpha1.ModelSpec{Files: []string{"https://example.com/model.gguf"}, Mmproj: "projector.gguf"}},
		{"URL with PVC source", inferencev1alpha1.ModelSpec{Source: "pvc://models/model.gguf", Files: []string{"https://example.com/model.gguf"}}},
		{"URL with OCI source", inferencev1alpha1.ModelSpec{Source: "oci://registry.example.com/model:latest", Files: []string{"https://example.com/model.gguf"}}},
	}
	for _, tc := range cases {
		for _, useCache := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/cache=%t", tc.name, useCache), func(t *testing.T) {
				model := &inferencev1alpha1.Model{Spec: tc.spec}
				cfg := buildModelStorageConfig(model, &inferencev1alpha1.InferenceService{}, "default", useCache, ModelCacheModeShared, "", "img:latest", 0, ModelCachePrepNever, DefaultInitContainerUser, nil, "")
				downloader, _ := downloaderFrom(cfg)
				if len(downloader.Command) != 3 {
					t.Fatal("missing fail-closed init command")
				}
				out, err := exec.Command(downloader.Command[0], downloader.Command[1:]...).CombinedOutput()
				if err == nil || !strings.Contains(string(out), "InvalidFileSet") {
					t.Fatalf("expected InvalidFileSet failure, got err=%v output=%s", err, out)
				}
				for _, volume := range cfg.volumes {
					if volume.EmptyDir == nil {
						t.Fatalf("invalid source mounted volume %+v", volume)
					}
				}
			})
		}
	}
}
