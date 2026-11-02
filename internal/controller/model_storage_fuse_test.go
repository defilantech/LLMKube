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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeStatScript is a stat that answers -f as the named filesystem would and
// defers everything else to the real stat ($REAL_STAT). "unavailable" is an
// init image whose stat has no -f.
func fakeStatScript(fs string) string {
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
	return stub + "; else exec \"$REAL_STAT\" \"$@\"; fi\n"
}

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
				storage := buildModelStorageConfig(model, nil, resolveEffectiveModelCache(nil, model), "default", true, ModelCacheModeShared, "", defaultPrefetchImage, 0, ModelCachePrepAlways, DefaultInitContainerUser, nil, "")
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
				if err := os.WriteFile(filepath.Join(bin, "stat"), []byte(fakeStatScript(fs)), 0o755); err != nil {
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
				// A stat without -f stages under the lock, as before FUSE
				// detection, rather than failing on every filesystem (#2253).
				if !prefetch && fs != "ext4" && fs != "unavailable" {
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
					if fs == "unavailable" {
						if !strings.Contains(string(out), "WARNING: cannot determine the cache filesystem") ||
							strings.Contains(string(out), "ModelCacheLockUnsupported") {
							t.Errorf("stat without -f: want the warning and no ModelCacheLockUnsupported diagnosis: %s", out)
						}
						if _, err := os.Stat(filepath.Join(dir, ".llmkube-download.lock")); err != nil {
							t.Errorf("stat without -f must stage under the directory lock: %v", err)
						}
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
	// The concurrent pool (spec.downloadConcurrency) must keep every guarantee
	// the serial loop gives a serving pod on FUSE, and stage nothing.
	for _, concurrent := range []bool{false, true} {
		for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
			for _, s3 := range []bool{false, true} {
				for _, state := range []string{"offline", "matching", "missing", "truncated", "changed", "changed-recorded"} {
					t.Run(fmt.Sprintf("concurrent-%v/%s/s3-%v/%s", concurrent, policy, s3, state), func(t *testing.T) {
						runMultiFileFUSEWarmCacheCase(t, realStat, policy, s3, concurrent, state)
					})
				}
			}
		}
	}
}

func runMultiFileFUSEWarmCacheCase(t *testing.T, realStat, policy string, s3, concurrent bool, state string) {
	t.Helper()
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			gets.Add(1)
		}
		if strings.HasPrefix(state, "changed") {
			w.Header().Set("Content-Length", "100")
		} else {
			w.Header().Set("Content-Length", "6")
		}
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
	if state == "truncated" || state == "changed-recorded" {
		if err := os.WriteFile(dest+".llmkube-size", []byte("6"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := filepath.Join(dir, "orphan.tmp")
	if err := os.WriteFile(sentinel, []byte("untouched"), 0o444); err != nil {
		t.Fatal(err)
	}
	env := []string{"MODEL_FILES=model.safetensors", "PATH=" + bin + ":" + os.Getenv("PATH"), "REAL_STAT=" + realStat,
		"MODEL_DOWNLOAD_CONCURRENCY=2", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=1048576"}
	if s3 {
		env = append(env, s3TestEnv(srv.URL)...)
	}
	out, err := runVerifyScriptEnv(t, buildMultiFileInitCommand(true, s3, false, false, concurrent, policy), srv.URL, dest, "", env...)
	if strings.Contains(out, "Staging up to") {
		t.Errorf("a serving pod on FUSE, which stages nothing, announced a download pool: %s", out)
	}
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
	} else {
		assertFUSEWarmCacheLog(t, out, policy, state)
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
	if state == "truncated" || state == "changed-recorded" {
		if got := readOrFail(t, dest+".llmkube-size"); got != "6" {
			t.Errorf("serving pod changed size record to %q", got)
		}
	} else if _, err := os.Stat(dest + ".llmkube-size"); !os.IsNotExist(err) {
		t.Error("serving pod wrote a size record on FUSE")
	}
}

// assertFUSEWarmCacheLog checks a warm FUSE start says what happened to the
// copy it kept: neither a refusal nor an unreachable origin when the origin
// answered (#2255).
func assertFUSEWarmCacheLog(t *testing.T, out, policy, state string) {
	t.Helper()
	if strings.Contains(out, "ModelCacheLockUnsupported") {
		t.Errorf("a warm FUSE cache start logged a refusal:\n%s", out)
	}
	if policy != RefreshPolicyOnChange {
		return
	}
	want := map[string]string{
		"offline":          "Revalidation unreachable for model.safetensors; kept cached copy",
		"matching":         "Model artifact model.safetensors revalidated (unchanged, skipped download)",
		"changed":          "Upstream changed for model.safetensors; a serving pod does not stage on a FUSE cache, so it kept the cached copy",
		"changed-recorded": "Upstream changed for model.safetensors; a serving pod does not stage on a FUSE cache, so it kept the cached copy",
	}[state]
	if !strings.Contains(out, want) {
		t.Errorf("log does not say %q:\n%s", want, out)
	}
	if state != "offline" && strings.Contains(out, "unreachable") {
		t.Errorf("the origin answered, but the log calls it unreachable:\n%s", out)
	}
}

// Feed the downloader's actual failure into the same condition path that
// handles an init container's termination message on a serving pod, and check
// the condition repeats the remedy the downloader printed (#2251).
func assertCacheLockCondition(t *testing.T, output string) {
	t.Helper()
	condition := cacheLockCondition(t, output)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ModelCacheLockUnsupported" {
		t.Fatalf("FUSE staging failure has no actionable condition: %+v", condition)
	}
	remedy := printedCacheLockRemedy(t, output)
	if !strings.HasSuffix(condition.Message, ". "+remedy) {
		t.Errorf("the condition's remedy differs from the one the downloader printed:\nlog remedy: %s\ncondition:  %s", remedy, condition.Message)
	}
}

// cacheLockCondition runs output, as a failed model-downloader's termination
// message, through reconcileModelTransferCondition and returns the condition.
func cacheLockCondition(t *testing.T, output string) *metav1.Condition {
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
	return meta.FindStatusCondition(isvc.Status.Conditions, ConditionModelTransferHealthy)
}

// printedCacheLockRemedy returns what the downloader printed after the
// ModelCacheLockUnsupported refusal's "<file>; ".
func printedCacheLockRemedy(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if _, rest, ok := strings.Cut(line, "ModelCacheLockUnsupported: "); ok {
			if _, remedy, ok := strings.Cut(rest, "; "); ok && remedy != "" {
				return remedy
			}
		}
	}
	t.Fatalf("the downloader printed no ModelCacheLockUnsupported remedy:\n%s", output)
	return ""
}

// downloaderOf returns the model-downloader container of a storage config.
func downloaderOf(t *testing.T, containers []corev1.Container) *corev1.Container {
	t.Helper()
	for i := range containers {
		if containers[i].Name == "model-downloader" {
			return &containers[i]
		}
	}
	t.Fatal("missing model-downloader container")
	return nil
}

// runDownloaderOnFS runs a generated downloader container's command under sh
// with the container's own env, its cache directory moved to dir, and a stat
// that answers -f as fs would (see fakeStatScript). envFrom stands in for the
// container's EnvFrom: the explicit Env is applied after it and wins, as in a
// pod.
func runDownloaderOnFS(t *testing.T, c *corev1.Container, dir, fs string, envFrom ...string) (string, error) {
	t.Helper()
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "stat"), []byte(fakeStatScript(fs)), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", c.Command[2])
	cmd.Env = append(os.Environ(), envFrom...)
	for _, env := range c.Env {
		switch env.Name {
		case "CACHE_DIR":
			env.Value = dir
		case "MODEL_PATH":
			env.Value = filepath.Join(dir, filepath.Base(env.Value))
		}
		cmd.Env = append(cmd.Env, env.Name+"="+env.Value)
	}
	cmd.Env = append(cmd.Env, "PATH="+bin+":"+os.Getenv("PATH"), "REAL_STAT="+realStat)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestCacheLockRemedyLogMatchesCondition: a serving pod that refuses to stage
// on a FUSE cache prints a remedy, and the ModelTransferHealthy condition the
// operator derives from that output must give the same one, for a source that
// can prefetch and one that cannot (#2251). The output is the real
// downloader's, run from the container the storage builder renders.
func TestCacheLockRemedyLogMatchesCondition(t *testing.T) {
	requireInitShellEnvironment(t)
	for _, tc := range []struct {
		name   string
		source string
		env    []string
		want   string
	}{
		{"http", "https://example.invalid/org/repo", nil, cacheLockRemedyPrefetch},
		{"s3", "s3://bucket/prefix", s3TestEnv("http://127.0.0.1:1"), cacheLockRemedyS3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "weights", Namespace: "default"},
				Spec:       inferencev1alpha1.ModelSpec{Source: tc.source, Format: "safetensors", Files: []string{"model.safetensors"}},
				Status:     inferencev1alpha1.ModelStatus{CacheKey: "cache"},
			}
			storage := buildModelStorageConfig(model, nil, resolveEffectiveModelCache(nil, model), "default", true, ModelCacheModeShared, "", defaultPrefetchImage, 0, ModelCachePrepAlways, DefaultInitContainerUser, nil, "")
			out, err := runDownloaderOnFS(t, downloaderOf(t, storage.initContainers), t.TempDir(), "fuse", tc.env...)
			if err == nil {
				t.Fatalf("a serving pod staged on FUSE: %s", out)
			}
			if got := printedCacheLockRemedy(t, out); got != tc.want {
				t.Errorf("the downloader printed remedy %q, want %q", got, tc.want)
			}
			assertCacheLockCondition(t, out)
		})
	}
}

// singleFileFUSEModel is a single-file cached Model on source, which the tests
// point at a stub origin.
func singleFileFUSEModel(source, policy string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "weights", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: source, Format: "gguf", RefreshPolicy: policy},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: "cache"},
	}
}

// TestSingleFileFUSEStaging: a single-file cached Model stages on FUSE only
// from the prefetch stager, as the multi-file builder does (#2254). A serving
// pod on a cold FUSE cache refuses with ModelCacheLockUnsupported before it
// writes anything; other filesystems, and a stat that cannot tell, stage under
// the directory lock as before.
func TestSingleFileFUSEStaging(t *testing.T) {
	requireInitShellEnvironment(t)
	for _, kind := range []string{"http-IfNotPresent", "http-OnChange", "s3"} {
		for _, fs := range []string{"fuse", "fuseblk", "busybox-fuse", "ext4", "unavailable"} {
			for _, prefetch := range []bool{false, true} {
				role := "serving"
				if prefetch {
					role = "prefetch"
				}
				t.Run(kind+"/"+fs+"/"+role, func(t *testing.T) {
					runSingleFileFUSEStagingCase(t, kind, fs, prefetch)
				})
			}
		}
	}
}

func runSingleFileFUSEStagingCase(t *testing.T, kind, fs string, prefetch bool) {
	t.Helper()
	origin := newRangeOrigin(t, true)
	source, policy, extraEnv := origin.srv.URL+"/org/repo/model.gguf", RefreshPolicyIfNotPresent, []string(nil)
	switch kind {
	case "http-OnChange":
		policy = RefreshPolicyOnChange
	case "s3":
		source = "s3://bucket/model.gguf"
		extraEnv = s3TestEnv(origin.srv.URL)
	}
	model := singleFileFUSEModel(source, policy)
	containers := buildModelStorageConfig(model, nil, resolveEffectiveModelCache(nil, model), "default", true, ModelCacheModeShared, "", defaultPrefetchImage, 0, ModelCachePrepAlways, DefaultInitContainerUser, nil, "").initContainers
	if prefetch {
		job, err := (&ModelReconciler{InitContainerImage: defaultPrefetchImage}).buildPrefetchJob(context.Background(), model, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if job.Spec.PodReplacementPolicy == nil || *job.Spec.PodReplacementPolicy != batchv1.Failed {
			t.Error("a single-file prefetch stager must wait for a failed pod before replacing it")
		}
		containers = job.Spec.Template.Spec.InitContainers
	}
	downloader := downloaderOf(t, containers)
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "orphan.tmp")
	if err := os.WriteFile(sentinel, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	// EnvFrom cannot override the explicit serving-pod setting, which the
	// container's own Env carries after this.
	out, err := runDownloaderOnFS(t, downloader, dir, fs, append([]string{"LLMKUBE_PREFETCH_STAGER=true"}, extraEnv...)...)
	published := filepath.Join(dir, singleFileStagedBasename(model))
	if !prefetch && fs != "ext4" && fs != "unavailable" {
		if err == nil {
			t.Fatalf("a serving pod staged on FUSE: %s", out)
		}
		if got := origin.fullFromZero.Load() + origin.rangeRequests.Load(); got != 0 {
			t.Errorf("a serving pod on FUSE issued %d downloads", got)
		}
		if got := readOrFail(t, sentinel); got != "untouched" {
			t.Errorf("a serving pod on FUSE swept the cache: %q", got)
		}
		if _, err := os.Stat(published); !os.IsNotExist(err) {
			t.Error("a serving pod on FUSE published a file")
		}
		if _, err := os.Stat(filepath.Join(dir, ".llmkube-download.lock")); !os.IsNotExist(err) {
			t.Error("a serving pod on FUSE created a lock file")
		}
		assertCacheLockCondition(t, out)
		return
	}
	if err != nil {
		t.Fatalf("staging failed: %v\n%s", err, out)
	}
	if got := readOrFail(t, published); got != string(origin.content()) {
		t.Errorf("staging published %d bytes, want the origin's %d", len(got), len(origin.content()))
	}
	if strings.Contains(out, "ModelCacheLockUnsupported") {
		t.Errorf("a pod allowed to stage logged a refusal: %s", out)
	}
	lockFile := filepath.Join(dir, ".llmkube-download.lock")
	if fs == "ext4" || fs == "unavailable" {
		if _, err := os.Stat(lockFile); err != nil {
			t.Errorf("staging off FUSE must hold the directory lock: %v", err)
		}
	} else if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("the FUSE stager must not rely on flock")
	}
}

// TestSingleFileFUSEWarmCache: a serving pod on FUSE still starts from a warm
// single-file cache (#2254), whatever the origin says, and changes nothing on
// the claim: no lock file, no sweep, no transfer.
func TestSingleFileFUSEWarmCache(t *testing.T) {
	requireInitShellEnvironment(t)
	for _, tc := range []struct {
		policy, origin string
		pinned         bool
		wantLog        string
	}{
		{RefreshPolicyIfNotPresent, "matching", false, "Model already cached, skipping download"},
		{RefreshPolicyIfNotPresent, "offline", false, "Model already cached, skipping download"},
		{RefreshPolicyIfNotPresent, "matching", true, "Model already cached, skipping download"},
		{RefreshPolicyOnChange, "matching", false, "Model revalidated (unchanged, skipped download)"},
		{RefreshPolicyOnChange, "offline", false, "Revalidation unreachable for model.gguf; kept cached copy"},
		{RefreshPolicyOnChange, "changed", false, "Upstream changed for model.gguf; a serving pod does not stage on a FUSE cache, so it kept the cached copy"},
		{RefreshPolicyOnChange, "changed", true, "Upstream changed for model.gguf; a serving pod does not stage on a FUSE cache, so it kept the cached copy"},
	} {
		t.Run(fmt.Sprintf("%s/%s/pinned-%v", tc.policy, tc.origin, tc.pinned), func(t *testing.T) {
			const cached = "cached"
			var gets atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					gets.Add(1)
				}
				size := len(cached)
				if tc.origin == "changed" {
					size = 100
				}
				w.Header().Set("Content-Length", fmt.Sprint(size))
			}))
			t.Cleanup(srv.Close)
			if tc.origin == "offline" {
				srv.Close()
			}
			model := singleFileFUSEModel(srv.URL+"/org/repo/model.gguf", tc.policy)
			model.Status.Path = "/models/cache/model.gguf"
			if tc.pinned {
				model.Spec.SHA256 = sha256Hex([]byte(cached))
			}
			storage := buildModelStorageConfig(model, nil, resolveEffectiveModelCache(nil, model), "default", true, ModelCacheModeShared, "", defaultPrefetchImage, 0, ModelCachePrepAlways, DefaultInitContainerUser, nil, "")
			dir := t.TempDir()
			dest := filepath.Join(dir, "model.gguf")
			if err := os.WriteFile(dest, []byte(cached), 0o444); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(dir, "orphan.tmp")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0o444); err != nil {
				t.Fatal(err)
			}
			out, err := runDownloaderOnFS(t, downloaderOf(t, storage.initContainers), dir, "fuse")
			if err != nil {
				t.Fatalf("a warm FUSE cache did not start: %v\n%s", err, out)
			}
			if !strings.Contains(out, tc.wantLog) {
				t.Errorf("log does not say %q:\n%s", tc.wantLog, out)
			}
			if strings.Contains(out, "ModelCacheLockUnsupported") {
				t.Errorf("a warm cache start logged a refusal:\n%s", out)
			}
			if gets.Load() != 0 {
				t.Errorf("a serving pod on FUSE issued %d downloads", gets.Load())
			}
			if readOrFail(t, dest) != cached {
				t.Error("a serving pod modified the FUSE cache")
			}
			if readOrFail(t, sentinel) != "untouched" {
				t.Error("a serving pod swept the FUSE cache")
			}
			if _, err := os.Stat(filepath.Join(dir, ".llmkube-download.lock")); !os.IsNotExist(err) {
				t.Error("a serving pod created a lock file on FUSE")
			}
		})
	}
}

// TestMultiFileFUSEStagerBackfillsSizeRecord: a FUSE cache staged before size
// records existed has none, and serving pods on FUSE must not write one. The
// prefetch stager may write there, so its IfNotPresent pass records the cached
// file's size without a transfer, and later starts stop warning (#2255).
func TestMultiFileFUSEStagerBackfillsSizeRecord(t *testing.T) {
	requireInitShellEnvironment(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", "6")
	}))
	t.Cleanup(srv.Close)
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "weights", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: srv.URL, Format: "safetensors", Files: []string{"model.safetensors"}},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: "cache"},
	}
	job, err := (&ModelReconciler{InitContainerImage: defaultPrefetchImage}).buildPrefetchJob(context.Background(), model, "", "")
	if err != nil {
		t.Fatal(err)
	}
	stager := downloaderOf(t, job.Spec.Template.Spec.InitContainers)
	dir := t.TempDir()
	dest := filepath.Join(dir, "model.safetensors")
	if err := os.WriteFile(dest, []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	const noRecord = "no valid size record for model.safetensors"

	out, err := runDownloaderOnFS(t, stager, dir, "fuse")
	if err != nil {
		t.Fatalf("stager failed on a legacy FUSE cache: %v\n%s", err, out)
	}
	if !strings.Contains(out, noRecord) {
		t.Errorf("the first pass should report the missing record:\n%s", out)
	}
	if got := readOrFail(t, dest+".llmkube-size"); got != "6" {
		t.Errorf("size record = %q, want the cached file's size 6", got)
	}
	if readOrFail(t, dest) != "cached" || requests.Load() != 0 {
		t.Errorf("the stager replaced a cached file it should keep (%d requests)", requests.Load())
	}

	out, err = runDownloaderOnFS(t, stager, dir, "fuse")
	if err != nil {
		t.Fatalf("second stager pass failed: %v\n%s", err, out)
	}
	if strings.Contains(out, noRecord) {
		t.Errorf("the backfilled record was not used on the next start:\n%s", out)
	}
}

// TestCacheAtomicWriteDebrisSweep: a pod that may write the cache removes the
// temp files a dead pod's llmkube_atomic_write left, once they are old enough
// that no write can still be using them, and nothing else (#2255). A serving
// pod on FUSE writes nothing, so it sweeps nothing either. Both builders open
// with the same prologue; each is run on a warm cache, so no transfer runs.
func TestCacheAtomicWriteDebrisSweep(t *testing.T) {
	requireInitShellEnvironment(t)
	const marker = "model.0123abcd.sha256-rejected"
	for _, builder := range []string{"single-file", "multi-file"} {
		for _, role := range []string{"ext4/serving", "fuse/serving", "fuse/prefetch"} {
			t.Run(builder+"/"+role, func(t *testing.T) {
				fs, prefetch, _ := strings.Cut(role, "/")
				model := singleFileFUSEModel("https://example.invalid/org/repo/model.gguf", RefreshPolicyIfNotPresent)
				model.Status.Path = "/models/cache/model.gguf"
				artifact := "model.gguf"
				if builder == "multi-file" {
					model.Spec.Source, model.Spec.Format, model.Spec.Files = "https://example.invalid/org/repo", "safetensors", []string{"sub/model.safetensors"}
					artifact = "sub/model.safetensors"
				}
				containers := buildModelStorageConfig(model, nil, resolveEffectiveModelCache(nil, model), "default", true, ModelCacheModeShared, "", defaultPrefetchImage, 0, ModelCachePrepAlways, DefaultInitContainerUser, nil, "").initContainers
				if prefetch == "prefetch" {
					job, err := (&ModelReconciler{InitContainerImage: defaultPrefetchImage}).buildPrefetchJob(context.Background(), model, "", "")
					if err != nil {
						t.Fatal(err)
					}
					containers = job.Spec.Template.Spec.InitContainers
				}
				dir := t.TempDir()
				old := time.Now().Add(-time.Hour)
				write := func(name, content string, mtime time.Time) {
					path := filepath.Join(dir, name)
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(path, mtime, mtime); err != nil {
						t.Fatal(err)
					}
				}
				write(artifact, "cached", old)
				write(artifact+".llmkube-size", "6", old)
				debris := []string{
					artifact + ".llmkube-tmp.AbC123",
					artifact + ".llmkube-size.XyZ789",
					marker + ".QwErTy",
				}
				kept := []string{
					artifact + ".llmkube-size",
					artifact + ".sha256",
					marker,
					"notes.sha256.backup",
					artifact + ".llmkube-tmp.FrEsH1",
				}
				for _, name := range append(debris, kept[1:4]...) {
					write(name, "x", old)
				}
				write(kept[4], "x", time.Now())

				out, err := runDownloaderOnFS(t, downloaderOf(t, containers), dir, fs)
				if err != nil {
					t.Fatalf("warm start failed: %v\n%s", err, out)
				}
				swept := role != "fuse/serving"
				for _, name := range debris {
					_, err := os.Stat(filepath.Join(dir, name))
					if gone := os.IsNotExist(err); gone != swept {
						t.Errorf("%s: removed=%v, want %v", name, gone, swept)
					}
				}
				for _, name := range kept {
					if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
						t.Errorf("%s must survive the sweep: %v", name, err)
					}
				}
			})
		}
	}
}

// TestAtomicWriteRemovesItsTempOnFailure: a write whose rename fails removes
// its temp file instead of leaving it on the claim, and a successful write
// leaves none (#2255).
func TestAtomicWriteRemovesItsTempOnFailure(t *testing.T) {
	requireInitShellEnvironment(t)
	for _, failMv := range []bool{false, true} {
		t.Run(fmt.Sprintf("mv-fails-%v", failMv), func(t *testing.T) {
			dir, bin := t.TempDir(), t.TempDir()
			if failMv {
				if err := os.WriteFile(filepath.Join(bin, "mv"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", "-c", atomicWriteFn+`if llmkube_atomic_write "$DIR/record" 42; then echo written; else echo refused; fi`)
			cmd.Env = append(os.Environ(), "DIR="+dir, "PATH="+bin+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			want := "written"
			if failMv {
				want = "refused"
			}
			if strings.TrimSpace(string(out)) != want {
				t.Errorf("output %q, want %q", out, want)
			}
			if leftovers, _ := filepath.Glob(filepath.Join(dir, "record.llmkube-tmp.*")); len(leftovers) != 0 {
				t.Errorf("temp files left behind: %v", leftovers)
			}
			if !failMv && readOrFail(t, filepath.Join(dir, "record")) != "42" {
				t.Error("the record was not written")
			}
		})
	}
}

// TestAtomicWriteDebrisMatchesTheSweep pins the contract between the writer
// and the sweep: the temp file a pod dying mid-write leaves (here, mv and rm
// that do nothing) is one atomicWriteDebrisSweep removes once it is old.
func TestAtomicWriteDebrisMatchesTheSweep(t *testing.T) {
	requireInitShellEnvironment(t)
	dir, bin := t.TempDir(), t.TempDir()
	for _, stub := range []string{"mv", "rm"} {
		if err := os.WriteFile(filepath.Join(bin, stub), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", atomicWriteFn+`llmkube_atomic_write "$DIR/model.safetensors.llmkube-size" 6; llmkube_atomic_write "$DIR/model.safetensors.sha256" stamp; llmkube_atomic_write "$DIR/model.0123.sha256-rejected" 0123; true`)
	cmd.Env = append(os.Environ(), "DIR="+dir, "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	leftovers, err := os.ReadDir(dir)
	if err != nil || len(leftovers) != 3 {
		t.Fatalf("want the three temp files a dead writer leaves, got %v (%v)", leftovers, err)
	}
	old := time.Now().Add(-time.Hour)
	for _, e := range leftovers {
		if err := os.Chtimes(filepath.Join(dir, e.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
	sweep := exec.Command("sh", "-c", atomicWriteDebrisSweep)
	sweep.Env = append(os.Environ(), "CACHE_DIR="+dir)
	if out, err := sweep.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("the sweep left the writer's temp files: %v", left)
	}
}
