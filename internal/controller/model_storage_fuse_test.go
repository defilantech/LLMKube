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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestMultiFileFUSEStaging(t *testing.T) {
	requireInitShellEnvironment(t)
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Fatal(err)
	}
	for _, fs := range []string{"fuse", "fuseblk", "busybox-fuse", "ext4", "unavailable"} {
		for _, prefetch := range []bool{false, true} {
			name := fs + "/serving"
			if prefetch {
				name = fs + "/prefetch"
			}
			t.Run(name, func(t *testing.T) {
				origin := newRangeOrigin(t, true)
				model := &inferencev1alpha1.Model{
					ObjectMeta: metav1.ObjectMeta{Name: "weights", Namespace: "default"},
					Spec:       inferencev1alpha1.ModelSpec{Source: origin.srv.URL, Format: "safetensors", Files: []string{"model.safetensors"}},
					Status:     inferencev1alpha1.ModelStatus{CacheKey: "cache"},
				}
				storage := buildModelStorageConfig(model, nil, "default", true, ModelCacheModeShared, "", defaultPrefetchImage, 0, nil, "")
				containers := storage.initContainers
				if prefetch {
					r := &ModelReconciler{InitContainerImage: defaultPrefetchImage}
					job, err := r.buildPrefetchJob(context.Background(), model, "", "")
					if err != nil {
						t.Fatal(err)
					}
					if job.Spec.PodReplacementPolicy == nil || *job.Spec.PodReplacementPolicy != batchv1.Failed {
						t.Error("prefetch must wait for a failed pod before replacing it")
					}
					containers = job.Spec.Template.Spec.InitContainers
				}
				var downloader *corev1.Container
				for i := range containers {
					if containers[i].Name == "model-downloader" {
						downloader = &containers[i]
					}
				}
				if downloader == nil {
					t.Fatal("missing downloader")
				}
				dir := t.TempDir()
				bin := t.TempDir()
				stub := "#!/bin/sh\nif [ \"$1\" = -f ]; then "
				if fs == "unavailable" {
					stub += "exit 1"
				} else {
					// BusyBox does not name FUSE, but both implementations
					// report the Linux filesystem magic through %t.
					magic, name := "65735546", "fuseblk"
					switch fs {
					case "fuse":
						name = "fuse"
					case "busybox-fuse":
						name = "UNKNOWN"
					case "ext4":
						magic, name = "ef53", "ext2/ext3"
					}
					stub += "if [ \"$3\" = %t ]; then echo " + magic + "; else echo " + name + "; fi"
				}
				stub += "; else exec \"$REAL_STAT\" \"$@\"; fi\n"
				if err := os.WriteFile(filepath.Join(bin, "stat"), []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
				sentinel := filepath.Join(dir, "orphan.tmp")
				if err := os.WriteFile(sentinel, []byte("untouched"), 0o644); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("sh", "-c", downloader.Command[2])
				cmd.Env = os.Environ()
				// EnvFrom cannot override the explicit serving-pod safety setting.
				cmd.Env = append(cmd.Env, "LLMKUBE_PREFETCH_STAGER=true")
				for _, env := range downloader.Env {
					cmd.Env = append(cmd.Env, env.Name+"="+env.Value)
				}
				cmd.Env = append(cmd.Env, "CACHE_DIR="+dir, "PATH="+bin+":"+os.Getenv("PATH"), "REAL_STAT="+realStat)
				out, err := cmd.CombinedOutput()
				if (!prefetch && fs != "ext4") || fs == "unavailable" {
					if err == nil {
						t.Fatalf("unsafe staging succeeded: %s", out)
					}
					if got := origin.fullFromZero.Load(); got != 0 {
						t.Errorf("unsafe staging issued %d downloads", got)
					}
					if got := readOrFail(t, sentinel); got != "untouched" {
						t.Errorf("guard modified cache: %q", got)
					}
					if _, err := os.Stat(filepath.Join(dir, "model.safetensors")); !os.IsNotExist(err) {
						t.Error("unsafe staging published a file")
					}
					if !strings.Contains(string(out), "ERROR:") {
						t.Errorf("missing diagnostic: %s", out)
					}
				} else {
					if err != nil {
						t.Fatalf("prefetch failed: %v\n%s", err, out)
					}
					if got := readOrFail(t, filepath.Join(dir, "model.safetensors")); got != string(origin.content()) {
						t.Error("prefetch published incorrect bytes")
					}
				}
			})
		}
	}
}

func TestMultiFileFUSEWarmCache(t *testing.T) {
	requireInitShellEnvironment(t)
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
		for _, s3 := range []bool{false, true} {
			for _, state := range []string{"offline", "matching", "missing", "truncated"} {
				t.Run(fmt.Sprintf("%s/s3-%v/%s", policy, s3, state), func(t *testing.T) {
					var gets atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != http.MethodHead {
							gets.Add(1)
						}
						w.Header().Set("Content-Length", "6")
					}))
					t.Cleanup(srv.Close)
					if state == "offline" {
						srv.Close()
					}
					dir := t.TempDir()
					bin := t.TempDir()
					stub := "#!/bin/sh\nif [ \"$1\" = -f ]; then echo 65735546; else exec \"$REAL_STAT\" \"$@\"; fi\n"
					if err := os.WriteFile(filepath.Join(bin, "stat"), []byte(stub), 0o755); err != nil {
						t.Fatal(err)
					}
					cached := "cached"
					if state == "truncated" {
						cached = "short"
					}
					dest := filepath.Join(dir, "model.safetensors")
					if state != "missing" {
						if err := os.WriteFile(dest, []byte(cached), 0o444); err != nil {
							t.Fatal(err)
						}
					}
					sentinel := filepath.Join(dir, "orphan.tmp")
					if err := os.WriteFile(sentinel, []byte("untouched"), 0o444); err != nil {
						t.Fatal(err)
					}
					env := []string{"MODEL_FILES=model.safetensors", "PATH=" + bin + ":" + os.Getenv("PATH"), "REAL_STAT=" + realStat}
					if s3 {
						env = append(env, s3TestEnv(srv.URL)...)
					}
					out, err := runVerifyScriptEnv(t, buildMultiFileInitCommand(true, s3, false, false, policy), srv.URL, dest, "", env...)
					if state == "missing" || state == "truncated" {
						if err == nil {
							t.Fatalf("unsafe FUSE staging succeeded: %s", out)
						}
						assertCacheLockCondition(t, out)
						if s3 && (!strings.Contains(out, "s3:// sources do not support prefetch") || strings.Contains(out, "set spec.modelCache.prefetch")) {
							t.Errorf("S3 staging error gives an unsupported remedy: %s", out)
						}
					} else if err != nil {
						t.Fatalf("warm FUSE cache failed: %v\n%s", err, out)
					}
					if gets.Load() != 0 {
						t.Errorf("serving pod attempted %d downloads on FUSE", gets.Load())
					}
					if state != "missing" && readOrFail(t, dest) != cached {
						t.Error("serving pod modified the FUSE cache")
					}
					if readOrFail(t, sentinel) != "untouched" {
						t.Error("serving pod swept the FUSE cache")
					}
					if _, err := os.Stat(filepath.Join(dir, ".llmkube-download.lock")); !os.IsNotExist(err) {
						t.Error("serving pod created a lock file on FUSE")
					}
					if _, err := os.Stat(dest + ".llmkube-size"); !os.IsNotExist(err) {
						t.Error("serving pod wrote a size record on FUSE")
					}
				})
			}
		}
	}
}

// Feed the downloader's actual failure into the same condition path that
// handles an init container's termination message on a serving pod.
func assertCacheLockCondition(t *testing.T, output string) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "default", Labels: map[string]string{"app": isvc.Name, "inference.llmkube.dev/service": isvc.Name}},
		Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
			Name: "model-downloader", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: output}},
		}}},
	}
	r := &InferenceServiceReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()}
	r.reconcileModelTransferCondition(t.Context(), isvc)
	condition := meta.FindStatusCondition(isvc.Status.Conditions, ConditionModelTransferHealthy)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ModelCacheLockUnsupported" {
		t.Fatalf("FUSE staging failure has no actionable condition: %+v", condition)
	}
	if !strings.Contains(condition.Message, "pvc://") {
		t.Errorf("cache-lock condition lacks a pre-staging remedy: %s", condition.Message)
	}
}
