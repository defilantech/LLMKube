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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestRuntimeCacheDirs(t *testing.T) {
	vllm := (&VLLMBackend{}).RuntimeCacheDirs()
	wantVLLM := []RuntimeCacheDir{
		{Env: "VLLM_CACHE_ROOT", Value: "vllm", Dir: "vllm", Default: "/root/.cache/vllm"},
		{Env: "TRITON_CACHE_DIR", Value: "triton", Dir: "triton", Default: "/root/.triton/cache"},
		{Env: "TILELANG_CACHE_DIR", Value: "tilelang", Dir: "tilelang", Default: "/root/.tilelang/cache"},
		{Env: "TORCHINDUCTOR_CACHE_DIR", Value: "torchinductor", Dir: "torchinductor", Default: "/tmp/torchinductor_root"},
		{Env: "FLASHINFER_WORKSPACE_BASE", Value: "flashinfer", Dir: "flashinfer/.cache/flashinfer", Default: "/root/.cache/flashinfer"},
	}
	if len(vllm) != len(wantVLLM) {
		t.Fatalf("vLLM declares %d cache dirs (%v), want %d", len(vllm), vllm, len(wantVLLM))
	}
	for i, want := range wantVLLM {
		if vllm[i] != want {
			t.Errorf("vLLM cache dir %d = %+v, want %+v", i, vllm[i], want)
		}
	}
	// The issue's measurement is a torch.compile-disabled start, so the root
	// alone is not enough: the inductor and triton variables must be declared.
	for _, want := range []string{"TORCHINDUCTOR_CACHE_DIR", "TRITON_CACHE_DIR"} {
		found := false
		for _, d := range vllm {
			if d.Env == want {
				found = true
			}
		}
		if !found {
			t.Errorf("vLLM cache dirs are missing %s", want)
		}
	}

	sglang := (&SGLangBackend{}).RuntimeCacheDirs()
	wantSGLang := []RuntimeCacheDir{{Env: "SGLANG_CACHE_DIR", Value: "", Dir: "", Default: "/root/.cache/sglang"}}
	if len(sglang) != 1 || sglang[0] != wantSGLang[0] {
		t.Errorf("SGLang cache dirs = %+v, want exactly %+v", sglang, wantSGLang)
	}

	// Every other runtime keeps its container exactly as before: it declares
	// no cache for the operator to relocate.
	for _, backend := range []RuntimeBackend{
		&LlamaCppBackend{}, &LlamaCppRouterBackend{}, &TGIBackend{}, &GenericBackend{}, &PersonaPlexBackend{},
	} {
		if _, ok := backend.(RuntimeCacheProvider); ok {
			t.Errorf("%T implements RuntimeCacheProvider; only vllm and sglang declare a cache", backend)
		}
	}
}

func TestRuntimeCacheWhyNot(t *testing.T) {
	const cacheKey = "0123456789abcdef"
	newModel := func(mutate func(*inferencev1alpha1.Model)) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/models/llama.gguf", Format: "gguf"},
			Status:     inferencev1alpha1.ModelStatus{CacheKey: cacheKey},
		}
		if mutate != nil {
			mutate(m)
		}
		return m
	}
	prefetched := func() *inferencev1alpha1.Model {
		return runtimeCachePrefetchedModel()
	}
	enabled := func() *inferencev1alpha1.ModelCacheSpec {
		return &inferencev1alpha1.ModelCacheSpec{
			RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: true},
		}
	}
	prefetching := func() *inferencev1alpha1.ModelCacheSpec {
		return runtimeCacheTestEnabled(true)
	}
	tests := []struct {
		name          string
		runtime       string
		cache         *inferencev1alpha1.ModelCacheSpec
		model         *inferencev1alpha1.Model
		mutateService func(*inferencev1alpha1.InferenceService)
		reconciler    *InferenceServiceReconciler
		wantApplied   bool
		wantReason    string
	}{
		{
			name:        "field not set",
			runtime:     RuntimeVLLM,
			wantApplied: false,
			wantReason:  "",
		},
		{
			name:        "vllm",
			runtime:     RuntimeVLLM,
			cache:       enabled(),
			wantApplied: true,
		},
		{
			name:        "sglang",
			runtime:     RuntimeSGLANG,
			cache:       enabled(),
			wantApplied: true,
		},
		{
			name:        "llamacpp has no persistent cache",
			runtime:     "llamacpp",
			cache:       enabled(),
			wantApplied: false,
			wantReason:  ReasonRuntimeCacheUnsupported,
		},
		{
			name:        "tgi has no persistent cache",
			runtime:     "tgi",
			cache:       enabled(),
			wantApplied: false,
			wantReason:  ReasonRuntimeCacheUnsupported,
		},
		{
			name:    "a metal model runs no serving container",
			runtime: RuntimeVLLM,
			cache:   enabled(),
			model: newModel(func(m *inferencev1alpha1.Model) {
				m.Spec.Hardware = &inferencev1alpha1.HardwareSpec{Accelerator: "metal"}
			}),
			wantApplied: false,
			wantReason:  ReasonRuntimeCacheUnsupported,
		},
		{
			name:        "the cache prep is turned off",
			runtime:     RuntimeVLLM,
			cache:       enabled(),
			reconciler:  &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService, ModelCachePrep: ModelCachePrepNever},
			wantApplied: false,
			wantReason:  ReasonRuntimeCacheNeedsPrepare,
		},
		{
			name:    "skipModelInit mounts no cache claim",
			runtime: RuntimeVLLM,
			cache:   enabled(),
			mutateService: func(s *inferencev1alpha1.InferenceService) {
				skip := true
				s.Spec.SkipModelInit = &skip
			},
			wantApplied: false,
			wantReason:  ReasonRuntimeCacheUnsupported,
		},
		{
			name:        "a pvc:// source is pre-staged",
			runtime:     RuntimeVLLM,
			cache:       enabled(),
			model:       newModel(func(m *inferencev1alpha1.Model) { m.Spec.Source = "pvc://weights/llama.gguf" }),
			wantApplied: false,
			wantReason:  ReasonRuntimeCacheUnsupported,
		},
		{
			name:        "an oci:// source is pre-staged",
			runtime:     RuntimeVLLM,
			cache:       enabled(),
			model:       newModel(func(m *inferencev1alpha1.Model) { m.Spec.Source = "oci://registry.example.com/weights:1" }),
			wantApplied: false,
			wantReason:  ReasonRuntimeCacheUnsupported,
		},
		{
			name:        "model caching is disabled on the operator",
			runtime:     RuntimeVLLM,
			cache:       enabled(),
			reconciler:  &InferenceServiceReconciler{},
			wantApplied: false,
			wantReason:  ReasonRuntimeCacheUnsupported,
		},
		{
			name:        "persistence Ephemeral mounts an emptyDir",
			runtime:     RuntimeVLLM,
			cache:       &inferencev1alpha1.ModelCacheSpec{Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral, RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: true}},
			wantApplied: false,
			wantReason:  ReasonRuntimeCacheUnsupported,
		},
		{
			// Transient: the key is derived when the Model reconciles, and the
			// next pass applies the request, so there is nothing to warn about.
			name:        "no cache key yet is declined without a warning",
			runtime:     RuntimeVLLM,
			cache:       enabled(),
			model:       newModel(func(m *inferencev1alpha1.Model) { m.Status.CacheKey = "" }),
			wantApplied: false,
			wantReason:  "",
		},
		{
			name:    "the user already mounts the operator path",
			runtime: RuntimeVLLM,
			cache:   enabled(),
			mutateService: func(s *inferencev1alpha1.InferenceService) {
				s.Spec.ExtraVolumeMounts = []corev1.VolumeMount{{Name: "user-cache", MountPath: "/runtime-cache"}}
			},
			wantApplied: false,
			wantReason:  "",
		},
		{
			// A prefetched claim is written by the Job in the operator's group:
			// a service whose own fsGroup differs would make kubelet re-own
			// every staged file each start, because the runtime cache mounts
			// the claim writable.
			name:    "a prefetched service with another fsGroup is declined",
			runtime: RuntimeVLLM,
			cache:   prefetching(),
			model:   prefetched(),
			mutateService: func(s *inferencev1alpha1.InferenceService) {
				fsGroup := int64(3000)
				s.Spec.PodSecurityContext = &corev1.PodSecurityContext{FSGroup: &fsGroup}
			},
			wantApplied: false,
			wantReason:  ReasonRuntimeCacheFSGroupMismatch,
		},
		{
			name:    "a prefetched service with the operator's group keeps the cache",
			runtime: RuntimeVLLM,
			cache:   prefetching(),
			model:   prefetched(),
			mutateService: func(s *inferencev1alpha1.InferenceService) {
				fsGroup := int64(102)
				s.Spec.PodSecurityContext = &corev1.PodSecurityContext{FSGroup: &fsGroup}
			},
			wantApplied: true,
		},
		{
			// The fsGroup walk only re-owns a volume the pod mounts writable. A
			// service that is not prefetched mounts its own claim, which was
			// writable before the runtime cache existed.
			name:    "a service that is not prefetched keeps another fsGroup",
			runtime: RuntimeVLLM,
			cache:   enabled(),
			mutateService: func(s *inferencev1alpha1.InferenceService) {
				fsGroup := int64(3000)
				s.Spec.PodSecurityContext = &corev1.PodSecurityContext{FSGroup: &fsGroup}
			},
			wantApplied: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.reconciler
			if r == nil {
				r = &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService, DefaultFSGroup: 102}
			}
			model := tc.model
			if model == nil {
				model = newModel(nil)
			}
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime:    tc.runtime,
					ModelCache: tc.cache,
				},
			}
			if tc.mutateService != nil {
				tc.mutateService(isvc)
			}
			cache := resolveEffectiveModelCache(isvc, model)
			plan := r.runtimeCachePlan(isvc, model, cache, resolveBackend(isvc))
			if plan.applied() != tc.wantApplied {
				t.Fatalf("runtimeCachePlan applied = %v (reason %q), want %v", plan.applied(), plan.reason, tc.wantApplied)
			}
			if plan.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", plan.reason, tc.wantReason)
			}
			if plan.reason != "" && plan.message == "" {
				t.Errorf("reason %q carries no message for the warning event", plan.reason)
			}
			render := r.constructDeploymentForCache(isvc, model, cache, nil, 1, "", "").Spec.Template.Spec
			if plan.applied() != (prepInitContainer(render) != nil) {
				t.Errorf("the plan applied = %v but the rendered pod has no prep container", plan.applied())
			}
		})
	}
}

func TestRuntimeCachePrepCommand(t *testing.T) {
	dirs := (&VLLMBackend{}).RuntimeCacheDirs()
	const claimDir = "_runtime-cache/default/svc"

	// Configures the per-service directory and the runtime's caches under it.
	// The chown/chmod address those paths and nothing below them, so a restart
	// never walks a cache the runtime has been growing.
	t.Run("fsGroup set: group-owned, non-recursive", func(t *testing.T) {
		cmd := runtimeCachePrepCommand(claimDir, dirs, 102, 101)
		if strings.Contains(cmd, "-R") {
			t.Errorf("prep command recurses: %s", cmd)
		}
		if !strings.Contains(cmd, "chown 0:102 ") || !strings.Contains(cmd, "chmod g+rwX ") {
			t.Errorf("prep command = %q, want chown 0:102 and chmod g+rwX", cmd)
		}
		root := t.TempDir()
		runMkdirPrefix(t, cmd, root)
		for _, rel := range []string{"", "vllm", "triton", "tilelang", "torchinductor", "flashinfer/.cache/flashinfer"} {
			p := filepath.Join(root, claimDir, rel)
			if _, err := os.Stat(p); err != nil {
				t.Errorf("prep did not create %s: %v", p, err)
			}
		}
	})

	// fsGroup disabled (OpenShift, --default-fsgroup=0): the downloader's uid
	// owns the directory, group 0 stays reachable for an SCC-assigned uid.
	t.Run("fsGroup disabled: downloader-owned, non-recursive", func(t *testing.T) {
		cmd := runtimeCachePrepCommand(claimDir, dirs, 0, 101)
		if strings.Contains(cmd, "-R") {
			t.Errorf("prep command recurses: %s", cmd)
		}
		if !strings.Contains(cmd, "chown 101:0 ") || !strings.Contains(cmd, "chmod 770 ") {
			t.Errorf("prep command = %q, want chown 101:0 and chmod 770", cmd)
		}
	})

	// SGLang's cache is the per-service directory itself, so there is nothing
	// below it to create.
	t.Run("a runtime whose cache is its own root creates only the per-service dir", func(t *testing.T) {
		cmd := runtimeCachePrepCommand(claimDir, (&SGLangBackend{}).RuntimeCacheDirs(), 102, 101)
		root := t.TempDir()
		runMkdirPrefix(t, cmd, root)
		entries, err := os.ReadDir(filepath.Join(root, claimDir))
		if err != nil {
			t.Fatalf("per-service directory missing: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("prep created %d entries under the per-service dir, want none: %v", len(entries), entries)
		}
	})
}

// runMkdirPrefix executes the directory-creation half of a prep command against
// a temporary root. The chown half needs root, so it is not run here.
func runMkdirPrefix(t *testing.T, cmd, root string) {
	t.Helper()
	mkdir, _, ok := strings.Cut(cmd, " && chown")
	if !ok {
		t.Fatalf("prep command %q has no chown half", cmd)
	}
	if !strings.Contains(mkdir, "mkdir -p ") {
		t.Fatalf("prep command %q does not create its directories", cmd)
	}
	out, err := exec.Command("sh", "-c", strings.ReplaceAll(mkdir, runtimeCacheMountPath, root)).CombinedOutput()
	if err != nil {
		t.Fatalf("executing %q: %v: %s", mkdir, err, out)
	}
}

func TestRuntimeCacheMountNeverSharesAPath(t *testing.T) {
	a := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns1"}}
	b := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns1"}}
	c := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns2"}}

	mountA := runtimeCacheMount(a, "model-cache")
	mountB := runtimeCacheMount(b, "model-cache")
	mountC := runtimeCacheMount(c, "model-cache")
	envA := runtimeCacheEnvVars((&VLLMBackend{}).RuntimeCacheDirs())
	if mountA == nil || mountB == nil || mountC == nil {
		t.Fatal("runtimeCacheMount returned no mount")
	}
	if mountA.SubPath != "_runtime-cache/ns1/a" {
		t.Errorf("SubPath = %q, want _runtime-cache/ns1/a", mountA.SubPath)
	}
	if mountA.SubPath == mountB.SubPath {
		t.Errorf("two services share SubPath %q; the cache is a security boundary", mountA.SubPath)
	}
	if mountA.SubPath == mountC.SubPath {
		t.Errorf("the same name in two namespaces shares SubPath %q", mountA.SubPath)
	}
	if mountA.Name != "model-cache" || mountA.MountPath != "/runtime-cache" {
		t.Errorf("mount = %+v, want the cache volume at /runtime-cache", mountA)
	}
	for _, e := range envA {
		if !strings.HasPrefix(e.Value, mountA.MountPath) {
			t.Errorf("env %s = %q, want a path under %s", e.Name, e.Value, mountA.MountPath)
		}
	}
}

const runtimeCacheTestKey = "0123456789abcdef"

// runtimeCacheTestModel is a ready single-file Model whose weights the pod
// stages, so the rendered pod mounts the cache claim.
func runtimeCacheTestModel(mutate func(*inferencev1alpha1.Model)) *inferencev1alpha1.Model {
	m := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/models/llama.gguf", Format: "gguf"},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: runtimeCacheTestKey},
	}
	if mutate != nil {
		mutate(m)
	}
	return m
}

func runtimeCacheTestEnabled(prefetch bool) *inferencev1alpha1.ModelCacheSpec {
	return &inferencev1alpha1.ModelCacheSpec{
		Prefetch:     prefetch,
		RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: true},
	}
}

func runtimeCacheTestService(runtime string, cache *inferencev1alpha1.ModelCacheSpec, mutate func(*inferencev1alpha1.InferenceServiceSpec)) *inferencev1alpha1.InferenceService {
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:    runtime,
			ModelCache: cache,
		},
	}
	if mutate != nil {
		mutate(&isvc.Spec)
	}
	return isvc
}

func runtimeCacheTestReconciler() *InferenceServiceReconciler {
	return &InferenceServiceReconciler{
		ModelCachePath:     DefaultModelCachePath,
		ModelCacheMode:     ModelCacheModePerService,
		InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		DefaultFSGroup:     102,
	}
}

func renderRuntimeCachePod(isvc *inferencev1alpha1.InferenceService, m *inferencev1alpha1.Model) corev1.PodSpec {
	return renderRuntimeCachePodWith(runtimeCacheTestReconciler(), isvc, m)
}

func renderRuntimeCachePodWith(r *InferenceServiceReconciler, isvc *inferencev1alpha1.InferenceService, m *inferencev1alpha1.Model) corev1.PodSpec {
	return r.constructDeploymentForCache(isvc, m, resolveEffectiveModelCache(isvc, m), nil, 1, "", "").Spec.Template.Spec
}

// runtimeCacheVLLMEnv is the env the correction turns every vLLM cache into.
var runtimeCacheVLLMEnv = map[string]string{
	"VLLM_CACHE_ROOT":           "/runtime-cache/vllm",
	"TRITON_CACHE_DIR":          "/runtime-cache/triton",
	"TILELANG_CACHE_DIR":        "/runtime-cache/tilelang",
	"TORCHINDUCTOR_CACHE_DIR":   "/runtime-cache/torchinductor",
	"FLASHINFER_WORKSPACE_BASE": "/runtime-cache/flashinfer",
}

func TestConstructDeployment_RuntimeCache(t *testing.T) {
	t.Run("enabled renders the mount, the env and the prep container", assertRuntimeCacheRendersEnabled)
	t.Run("no runtime cache renders base", assertRuntimeCacheRendersBase)
	t.Run("sglang points its one variable at the per-service directory", assertRuntimeCacheSGLang)
	t.Run("user env wins", assertRuntimeCacheUserEnvWins)
	t.Run("user mount at vllm's default path wins for that cache only", assertRuntimeCacheUserMountAtDefaultPath)
	t.Run("user mount at the operator's path wins", assertRuntimeCacheUserMountAtOperatorPath)
	t.Run("a prefetched cache is writable while /models stays read-only", assertRuntimeCachePrefetchedWritable)
	t.Run("a prefetched service with another fsGroup is declined", assertRuntimeCacheDeclinesPrefetchedFSGroupMismatch)
	t.Run("a read-only claim is declined", assertRuntimeCacheDeclinesReadOnlyClaim)
	t.Run("a pod with no fsGroup of its own runs in the prep group", assertRuntimeCacheAddsPrepGroup)
	t.Run("a mount above a default path takes no cache over", assertRuntimeCacheIgnoresMountAboveDefault)
	t.Run("every cache taken over injects nothing", assertRuntimeCacheAllCachesTakenOver)
	t.Run("a user fsGroup gets OnRootMismatch on a prefetched runtime-cache pod", assertRuntimeCacheFSGroupPolicy)
	t.Run("a cached pod gets OnRootMismatch without prefetch", assertRuntimeCacheFSGroupPolicyCached)
	t.Run("a user fsGroupChangePolicy is kept", assertRuntimeCacheKeepsUserFSGroupChangePolicy)
	t.Run("without the runtime cache the user context is unchanged", assertRuntimeCacheNoFSGroupPolicyWithoutFeature)
	t.Run("only the service's own request is warned", assertRuntimeCacheWarningScope)
	t.Run("multinode members each carry the mount, env and prep", assertRuntimeCacheMultiNodeMembers)
}

func assertRuntimeCacheRendersEnabled(t *testing.T) {
	t.Helper()
	pod := renderRuntimeCachePod(runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(false), nil), runtimeCacheTestModel(nil))
	mount := singleMountAt(t, pod.Containers[0].VolumeMounts, runtimeCacheMountPath)
	if mount.Name != "model-cache" || mount.SubPath != "_runtime-cache/default/svc" || mount.ReadOnly {
		t.Fatalf("mount = %+v, want the writable cache claim at the per-service subPath", mount)
	}
	for name, want := range runtimeCacheVLLMEnv {
		if got := lastEnvValue(pod.Containers[0].Env, name); got != want {
			t.Errorf("env %s = %q, want %q", name, got, want)
		}
	}
	prep := prepInitContainer(pod)
	if prep == nil {
		t.Fatal("no runtime-cache-prep init container")
	}
	if prep.Image != "docker.io/curlimages/curl:8.18.0" {
		t.Errorf("prep image = %q, want the operator's init image", prep.Image)
	}
	if prep.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("prep termination message policy = %q, want the log tail on failure, as every generated init container has",
			prep.TerminationMessagePolicy)
	}
	if len(prep.VolumeMounts) != 1 || prep.VolumeMounts[0].Name != "model-cache" ||
		prep.VolumeMounts[0].MountPath != runtimeCacheMountPath || prep.VolumeMounts[0].SubPath != "" {
		t.Errorf("prep mounts = %+v, want the claim root at %s (the subPath does not exist yet)",
			prep.VolumeMounts, runtimeCacheMountPath)
	}
	cmd := strings.Join(prep.Command, " ")
	for _, want := range []string{
		"mkdir -p /runtime-cache/_runtime-cache/default/svc",
		"/runtime-cache/_runtime-cache/default/svc/vllm",
		"/runtime-cache/_runtime-cache/default/svc/flashinfer/.cache/flashinfer",
		"chown 0:102 ",
		"chmod g+rwX ",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("prep command %q is missing %q", cmd, want)
		}
	}
	sc := prep.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Fatalf("prep security context = %+v, want root", sc)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("prep allows privilege escalation")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Add) != 2 {
		t.Errorf("prep capabilities = %+v, want CHOWN and FOWNER added", sc.Capabilities)
	}
	if got := singleMountAt(t, pod.Containers[0].VolumeMounts, DefaultModelCachePath); !got.ReadOnly {
		t.Errorf("/models mount is writable: %+v", got)
	}
}

func assertRuntimeCacheRendersBase(t *testing.T) {
	t.Helper()
	tests := []struct {
		name    string
		runtime string
		model   *inferencev1alpha1.Model
	}{
		{"vllm", RuntimeVLLM, runtimeCacheTestModel(nil)},
		{"sglang", RuntimeSGLANG, runtimeCacheTestModel(nil)},
		{"llamacpp", "llamacpp", runtimeCacheTestModel(nil)},
		{"pvc source", RuntimeVLLM, runtimeCacheTestModel(func(m *inferencev1alpha1.Model) {
			m.Spec.Source = "pvc://weights/llama.gguf"
		})},
	}
	for _, tc := range tests {
		pod := renderRuntimeCachePod(runtimeCacheTestService(tc.runtime, nil, nil), tc.model)
		assertNoRuntimeCacheInjections(t, tc.name, pod)
	}
}

// assertNoRuntimeCacheInjections checks the pod carries none of the feature's
// three artifacts.
func assertNoRuntimeCacheInjections(t *testing.T, name string, pod corev1.PodSpec) {
	t.Helper()
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.MountPath == runtimeCacheMountPath {
			t.Errorf("%s: got a %s mount without the field", name, runtimeCacheMountPath)
		}
	}
	for _, envName := range []string{
		"VLLM_CACHE_ROOT", "TRITON_CACHE_DIR", "TILELANG_CACHE_DIR",
		"TORCHINDUCTOR_CACHE_DIR", "FLASHINFER_WORKSPACE_BASE", "SGLANG_CACHE_DIR",
	} {
		for _, e := range pod.Containers[0].Env {
			if e.Name == envName {
				t.Errorf("%s: got env %s=%q without the field", name, e.Name, e.Value)
			}
		}
	}
	if prepInitContainer(pod) != nil {
		t.Errorf("%s: got a runtime-cache-prep container without the field", name)
	}
}

func assertRuntimeCacheSGLang(t *testing.T) {
	t.Helper()
	pod := renderRuntimeCachePod(runtimeCacheTestService(RuntimeSGLANG, runtimeCacheTestEnabled(false), nil), runtimeCacheTestModel(nil))
	singleMountAt(t, pod.Containers[0].VolumeMounts, runtimeCacheMountPath)
	if got := lastEnvValue(pod.Containers[0].Env, "SGLANG_CACHE_DIR"); got != runtimeCacheMountPath {
		t.Errorf("SGLANG_CACHE_DIR = %q, want %q", got, runtimeCacheMountPath)
	}
}

func assertRuntimeCacheUserEnvWins(t *testing.T) {
	t.Helper()
	isvc := runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(false), func(s *inferencev1alpha1.InferenceServiceSpec) {
		s.Env = []corev1.EnvVar{{Name: "VLLM_CACHE_ROOT", Value: "/user/cache"}}
	})
	pod := renderRuntimeCachePod(isvc, runtimeCacheTestModel(nil))
	if got := lastEnvValue(pod.Containers[0].Env, "VLLM_CACHE_ROOT"); got != "/user/cache" {
		t.Errorf("VLLM_CACHE_ROOT = %q, want the user's value", got)
	}
	if got := lastEnvValue(pod.Containers[0].Env, "TRITON_CACHE_DIR"); got != "/runtime-cache/triton" {
		t.Errorf("TRITON_CACHE_DIR = %q, want the operator's value", got)
	}
}

func assertRuntimeCacheUserMountAtDefaultPath(t *testing.T) {
	t.Helper()
	isvc := runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(false), func(s *inferencev1alpha1.InferenceServiceSpec) {
		s.ExtraVolumes = []corev1.Volume{{Name: "user-tilelang", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		s.ExtraVolumeMounts = []corev1.VolumeMount{{Name: "user-tilelang", MountPath: "/root/.tilelang/cache"}}
	})
	pod := renderRuntimeCachePod(isvc, runtimeCacheTestModel(nil))
	if got := lastEnvValue(pod.Containers[0].Env, "TILELANG_CACHE_DIR"); got != "" {
		t.Errorf("TILELANG_CACHE_DIR = %q, want the operator to leave the user's mount alone", got)
	}
	for name, want := range runtimeCacheVLLMEnv {
		if name == "TILELANG_CACHE_DIR" {
			continue
		}
		if got := lastEnvValue(pod.Containers[0].Env, name); got != want {
			t.Errorf("env %s = %q, want %q", name, got, want)
		}
	}
}

func assertRuntimeCacheUserMountAtOperatorPath(t *testing.T) {
	t.Helper()
	isvc := runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(false), func(s *inferencev1alpha1.InferenceServiceSpec) {
		s.ExtraVolumes = []corev1.Volume{{Name: "user-cache", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		s.ExtraVolumeMounts = []corev1.VolumeMount{{Name: "user-cache", MountPath: runtimeCacheMountPath}}
	})
	pod := renderRuntimeCachePod(isvc, runtimeCacheTestModel(nil))
	var atPath []corev1.VolumeMount
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.MountPath == runtimeCacheMountPath {
			atPath = append(atPath, m)
		}
	}
	if len(atPath) != 1 || atPath[0].Name != "user-cache" {
		t.Fatalf("mounts at %s = %+v, want exactly the user's", runtimeCacheMountPath, atPath)
	}
	for name := range runtimeCacheVLLMEnv {
		if got := lastEnvValue(pod.Containers[0].Env, name); got != "" {
			t.Errorf("env %s = %q, want none when the user owns the path", name, got)
		}
	}
	if prepInitContainer(pod) != nil {
		t.Error("got a runtime-cache-prep container although the user owns the path")
	}
}

func assertRuntimeCachePrefetchedWritable(t *testing.T) {
	t.Helper()
	pod := renderRuntimeCachePod(runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(true), nil), runtimeCachePrefetchedModel())
	vol := volumeNamed(pod, "model-cache")
	if vol == nil || vol.PersistentVolumeClaim == nil {
		t.Fatal("no model-cache claim volume")
	}
	if vol.PersistentVolumeClaim.ReadOnly {
		t.Error("the prefetched claim is read-only, so the runtime cache mount could not be written")
	}
	if got := singleMountAt(t, pod.Containers[0].VolumeMounts, DefaultModelCachePath); !got.ReadOnly {
		t.Error("/models must stay read-only on the prefetch path")
	}
	singleMountAt(t, pod.Containers[0].VolumeMounts, runtimeCacheMountPath)
	if prepInitContainer(pod) == nil {
		t.Error("no runtime-cache-prep init container on the prefetch path")
	}
}

// runtimeCachePrefetchedModel is the Model a service with modelCache.prefetch:
// true sees once its Job has staged the per-service claim.
func runtimeCachePrefetchedModel() *inferencev1alpha1.Model {
	return runtimeCacheTestModel(func(m *inferencev1alpha1.Model) {
		m.Status.Prefetch = []inferencev1alpha1.PrefetchStatus{{
			ClaimName:          "svc-model-cache",
			CacheKey:           runtimeCacheTestKey,
			Phase:              inferencev1alpha1.PrefetchPhaseSucceeded,
			JobName:            "svc-prefetch",
			ObservedGeneration: m.Generation,
		}}
	})
}

// The runtime cache mounts the claim writable, so kubelet's default
// fsGroupChangePolicy (Always) would re-own the whole claim, staged weights
// included, on every pod start. A context the user supplied is never changed
// in their spec, so the policy goes onto the pod's copy.
func assertRuntimeCacheFSGroupPolicy(t *testing.T) {
	t.Helper()
	fsGroup := int64(102)
	isvc := runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(true), func(s *inferencev1alpha1.InferenceServiceSpec) {
		s.PodSecurityContext = &corev1.PodSecurityContext{FSGroup: &fsGroup}
	})
	pod := renderRuntimeCachePod(isvc, runtimeCachePrefetchedModel())
	if prepInitContainer(pod) == nil {
		t.Fatal("the runtime cache was not applied, so this test pins nothing")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != fsGroup {
		t.Fatalf("pod security context = %+v, want the user's fsGroup %d", pod.SecurityContext, fsGroup)
	}
	if pod.SecurityContext.FSGroupChangePolicy == nil || *pod.SecurityContext.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("fsGroupChangePolicy = %v, want OnRootMismatch next to the user's fsGroup",
			pod.SecurityContext.FSGroupChangePolicy)
	}
	if isvc.Spec.PodSecurityContext.FSGroupChangePolicy != nil {
		t.Error("the operator wrote fsGroupChangePolicy into the InferenceService spec")
	}
}

func assertRuntimeCacheKeepsUserFSGroupChangePolicy(t *testing.T) {
	t.Helper()
	fsGroup := int64(5000)
	always := corev1.FSGroupChangeAlways
	isvc := runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(false), func(s *inferencev1alpha1.InferenceServiceSpec) {
		s.PodSecurityContext = &corev1.PodSecurityContext{FSGroup: &fsGroup, FSGroupChangePolicy: &always}
	})
	pod := renderRuntimeCachePod(isvc, runtimeCacheTestModel(nil))
	if prepInitContainer(pod) == nil {
		t.Fatal("the runtime cache was not applied, so this test pins nothing")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroupChangePolicy == nil ||
		*pod.SecurityContext.FSGroupChangePolicy != corev1.FSGroupChangeAlways {
		t.Errorf("fsGroupChangePolicy = %v, want the user's Always",
			pod.SecurityContext.FSGroupChangePolicy)
	}
}

func assertRuntimeCacheNoFSGroupPolicyWithoutFeature(t *testing.T) {
	t.Helper()
	fsGroup := int64(5000)
	isvc := runtimeCacheTestService(RuntimeVLLM, &inferencev1alpha1.ModelCacheSpec{Prefetch: true}, func(s *inferencev1alpha1.InferenceServiceSpec) {
		s.PodSecurityContext = &corev1.PodSecurityContext{FSGroup: &fsGroup}
	})
	pod := renderRuntimeCachePod(isvc, runtimeCachePrefetchedModel())
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroupChangePolicy != nil {
		t.Errorf("fsGroupChangePolicy = %v, want none without the runtime cache",
			pod.SecurityContext.FSGroupChangePolicy)
	}
	vol := volumeNamed(pod, "model-cache")
	if vol == nil || vol.PersistentVolumeClaim == nil || !vol.PersistentVolumeClaim.ReadOnly {
		t.Error("want the prefetched claim read-only without the runtime cache")
	}
}

// assertRuntimeCacheDeclinesPrefetchedFSGroupMismatch pins the decline for a
// prefetched service whose own fsGroup differs from the group the prefetch
// writes the claim in: the claim must stay read-only, or kubelet would re-own
// every staged file on each pod start.
func assertRuntimeCacheDeclinesPrefetchedFSGroupMismatch(t *testing.T) {
	t.Helper()
	fsGroup := int64(3000)
	isvc := runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(true), func(s *inferencev1alpha1.InferenceServiceSpec) {
		s.PodSecurityContext = &corev1.PodSecurityContext{FSGroup: &fsGroup}
	})
	pod := renderRuntimeCachePod(isvc, runtimeCachePrefetchedModel())
	assertNoRuntimeCacheInjections(t, "prefetched fsGroup mismatch", pod)
	vol := volumeNamed(pod, "model-cache")
	if vol == nil || vol.PersistentVolumeClaim == nil || !vol.PersistentVolumeClaim.ReadOnly {
		t.Errorf("prefetched claim = %+v, want the read-only volume source a declined request leaves", vol)
	}
}

// assertRuntimeCacheDeclinesReadOnlyClaim pins the decline for a claim that
// offers no writable access mode: nothing is injected, and the warning names the
// claim's modes.
func assertRuntimeCacheDeclinesReadOnlyClaim(t *testing.T) {
	t.Helper()
	scheme := apiruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	newClaim := func(name string, modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: modes},
		}
	}
	newReconciler := func(claim *corev1.PersistentVolumeClaim) *InferenceServiceReconciler {
		r := runtimeCacheTestReconciler()
		r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()
		return r
	}
	newService := func(claim string) *inferencev1alpha1.InferenceService {
		return runtimeCacheTestService(RuntimeVLLM, &inferencev1alpha1.ModelCacheSpec{
			ClaimName:    claim,
			RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: true},
		}, nil)
	}
	m := runtimeCacheTestModel(nil)

	r := newReconciler(newClaim("weights-rox", corev1.ReadOnlyMany))
	isvc := newService("weights-rox")
	cache := resolveEffectiveModelCache(isvc, m)
	plan := r.runtimeCachePlanFor(t.Context(), isvc, m, cache, resolveBackend(isvc))
	if plan.applied() || plan.reason != ReasonRuntimeCacheClaimReadOnly {
		t.Fatalf("plan applied = %v (reason %q), want the read-only claim declined", plan.applied(), plan.reason)
	}
	if !strings.Contains(plan.message, "ReadOnlyMany") {
		t.Errorf("message %q does not name the claim's access mode", plan.message)
	}
	pod := r.constructDeploymentForCache(isvc, m, cache, nil, 1, "", "", plan).Spec.Template.Spec
	assertNoRuntimeCacheInjections(t, "read-only claim", pod)

	// A claim that offers a write keeps the feature.
	r = newReconciler(newClaim("weights-rwx", corev1.ReadWriteMany))
	isvc = newService("weights-rwx")
	cache = resolveEffectiveModelCache(isvc, m)
	plan = r.runtimeCachePlanFor(t.Context(), isvc, m, cache, resolveBackend(isvc))
	if !plan.applied() {
		t.Fatalf("plan applied = false (reason %q) on a writable claim", plan.reason)
	}
	pod = r.constructDeploymentForCache(isvc, m, cache, nil, 1, "", "", plan).Spec.Template.Spec
	singleMountAt(t, pod.Containers[0].VolumeMounts, runtimeCacheMountPath)

	// A claim that cannot be read is not a decline: a transient read error must
	// not take the feature away.
	r = runtimeCacheTestReconciler()
	r.Client = fake.NewClientBuilder().WithScheme(scheme).Build()
	isvc = newService("missing")
	cache = resolveEffectiveModelCache(isvc, m)
	if plan = r.runtimeCachePlanFor(t.Context(), isvc, m, cache, resolveBackend(isvc)); !plan.applied() {
		t.Errorf("plan applied = false (reason %q) on a claim that could not be read", plan.reason)
	}
}

// assertRuntimeCacheAddsPrepGroup pins that a serving container whose own
// podSecurityContext sets no fsGroup still runs in the group the prep hands the
// per-service directory to, so it can write it.
func assertRuntimeCacheAddsPrepGroup(t *testing.T) {
	t.Helper()
	runAsUser, runAsNonRoot := int64(1000), true
	isvc := runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(false), func(s *inferencev1alpha1.InferenceServiceSpec) {
		s.PodSecurityContext = &corev1.PodSecurityContext{RunAsUser: &runAsUser, RunAsNonRoot: &runAsNonRoot}
	})
	pod := renderRuntimeCachePod(isvc, runtimeCacheTestModel(nil))
	if prepInitContainer(pod) == nil {
		t.Fatal("the runtime cache was not applied, so this test pins nothing")
	}
	if pod.SecurityContext == nil || !slices.Contains(pod.SecurityContext.SupplementalGroups, int64(102)) {
		t.Fatalf("supplementalGroups = %+v, want the prep's group 102", pod.SecurityContext)
	}
	if len(isvc.Spec.PodSecurityContext.SupplementalGroups) != 0 {
		t.Error("the operator wrote supplementalGroups into the InferenceService spec")
	}

	// A context already in the group through its own fsGroup adds nothing: the
	// prep chowns to that group, and kubelet puts the pod in it.
	fsGroup := int64(102)
	same := runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(false), func(s *inferencev1alpha1.InferenceServiceSpec) {
		s.PodSecurityContext = &corev1.PodSecurityContext{FSGroup: &fsGroup}
	})
	if pod := renderRuntimeCachePod(same, runtimeCacheTestModel(nil)); len(pod.SecurityContext.SupplementalGroups) != 0 {
		t.Errorf("supplementalGroups = %v with the context's own fsGroup, want none", pod.SecurityContext.SupplementalGroups)
	}

	// With the operator's fsGroup disabled the prep hands the directory to the
	// downloader's uid with group 0, the convention model-cache-prep uses, so
	// the operator adds no group it cannot name.
	disabled := runtimeCacheTestReconciler()
	disabled.DefaultFSGroup = 0
	pod = renderRuntimeCachePodWith(disabled, isvc, runtimeCacheTestModel(nil))
	if prepInitContainer(pod) == nil {
		t.Fatal("the runtime cache was not applied with the default fsGroup disabled")
	}
	if len(pod.SecurityContext.SupplementalGroups) != 0 {
		t.Errorf("supplementalGroups = %v with the default fsGroup disabled, want none", pod.SecurityContext.SupplementalGroups)
	}
}

// assertRuntimeCacheIgnoresMountAboveDefault pins the exact-path rule: a user
// volume mounted above a cache's default directory leaves that cache to the
// operator.
func assertRuntimeCacheIgnoresMountAboveDefault(t *testing.T) {
	t.Helper()
	withMount := func(p string) func(*inferencev1alpha1.InferenceServiceSpec) {
		return func(s *inferencev1alpha1.InferenceServiceSpec) {
			s.ExtraVolumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
			s.ExtraVolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: p}}
		}
	}

	// An emptyDir at /tmp is the usual read-only-root-filesystem setup.
	pod := renderRuntimeCachePod(runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(false), withMount("/tmp")), runtimeCacheTestModel(nil))
	if got := lastEnvValue(pod.Containers[0].Env, "TORCHINDUCTOR_CACHE_DIR"); got != runtimeCacheVLLMEnv["TORCHINDUCTOR_CACHE_DIR"] {
		t.Errorf("TORCHINDUCTOR_CACHE_DIR = %q under a user mount at /tmp, want %q", got, runtimeCacheVLLMEnv["TORCHINDUCTOR_CACHE_DIR"])
	}

	// /root/.cache is where a user's HF_HOME usually points.
	pod = renderRuntimeCachePod(runtimeCacheTestService(RuntimeSGLANG, runtimeCacheTestEnabled(false), withMount("/root/.cache")), runtimeCacheTestModel(nil))
	if got := lastEnvValue(pod.Containers[0].Env, "SGLANG_CACHE_DIR"); got != runtimeCacheMountPath {
		t.Errorf("SGLANG_CACHE_DIR = %q under a user mount at /root/.cache, want %q", got, runtimeCacheMountPath)
	}
}

// assertRuntimeCacheAllCachesTakenOver pins that a mount at every declared
// default directory means nothing is injected, the mount and prep included.
func assertRuntimeCacheAllCachesTakenOver(t *testing.T) {
	t.Helper()
	isvc := runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(false), func(s *inferencev1alpha1.InferenceServiceSpec) {
		s.ExtraVolumes = []corev1.Volume{{Name: "user-caches", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		for _, p := range []string{
			"/root/.cache/vllm", "/root/.triton/cache", "/root/.tilelang/cache",
			"/tmp/torchinductor_root", "/root/.cache/flashinfer",
		} {
			s.ExtraVolumeMounts = append(s.ExtraVolumeMounts, corev1.VolumeMount{Name: "user-caches", MountPath: p})
		}
	})
	pod := renderRuntimeCachePod(isvc, runtimeCacheTestModel(nil))
	assertNoRuntimeCacheInjections(t, "every cache taken over", pod)
}

// assertRuntimeCacheFSGroupPolicyCached pins OnRootMismatch on a pod the runtime
// cache is applied to that is not prefetched, where the claim is writable only
// because of the runtime cache.
func assertRuntimeCacheFSGroupPolicyCached(t *testing.T) {
	t.Helper()
	fsGroup := int64(5000)
	isvc := runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(false), func(s *inferencev1alpha1.InferenceServiceSpec) {
		s.PodSecurityContext = &corev1.PodSecurityContext{FSGroup: &fsGroup}
	})
	pod := renderRuntimeCachePod(isvc, runtimeCacheTestModel(nil))
	if prepInitContainer(pod) == nil {
		t.Fatal("the runtime cache was not applied, so this test pins nothing")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroupChangePolicy == nil ||
		*pod.SecurityContext.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("fsGroupChangePolicy = %+v, want OnRootMismatch next to the user's fsGroup", pod.SecurityContext)
	}
}

// assertRuntimeCacheWarningScope pins that only the service's own request is
// warned about, and that the transient no-cache-key decline is silent.
func assertRuntimeCacheWarningScope(t *testing.T) {
	t.Helper()
	recorder := events.NewFakeRecorder(8)
	r := runtimeCacheTestReconciler()
	r.Recorder = recorder

	// A Model-level block reaches every referencing service, including ones
	// whose runtime keeps no cache: nothing was written on the service.
	inherited := runtimeCacheTestModel(func(m *inferencev1alpha1.Model) {
		m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{
			RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: true},
		}
	})
	isvc := runtimeCacheTestService("llamacpp", nil, nil)
	r.warnRuntimeCacheNotApplied(t.Context(), isvc, inherited, resolveEffectiveModelCache(isvc, inherited), resolveBackend(isvc))
	if got := drainEvents(recorder); len(got) != 0 {
		t.Errorf("events = %v for an inherited block, want none", got)
	}

	// The service's own request is warned.
	own := runtimeCacheTestService("llamacpp", &inferencev1alpha1.ModelCacheSpec{
		RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: true},
	}, nil)
	r.warnRuntimeCacheNotApplied(t.Context(), own, runtimeCacheTestModel(nil), resolveEffectiveModelCache(own, runtimeCacheTestModel(nil)), resolveBackend(own))
	got := drainEvents(recorder)
	if len(got) != 1 || !strings.Contains(got[0], ReasonRuntimeCacheUnsupported) {
		t.Errorf("events = %v, want one %s event", got, ReasonRuntimeCacheUnsupported)
	}

	// No cache key yet is transient: the next reconcile applies the request.
	noKey := runtimeCacheTestModel(func(m *inferencev1alpha1.Model) { m.Status.CacheKey = "" })
	vllm := runtimeCacheTestService(RuntimeVLLM, &inferencev1alpha1.ModelCacheSpec{
		RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: true},
	}, nil)
	r.warnRuntimeCacheNotApplied(t.Context(), vllm, noKey, resolveEffectiveModelCache(vllm, noKey), resolveBackend(vllm))
	if got := drainEvents(recorder); len(got) != 0 {
		t.Errorf("events = %v before the cache key exists, want none", got)
	}
}

func assertRuntimeCacheMultiNodeMembers(t *testing.T) {
	t.Helper()
	isvc := runtimeCacheTestService(RuntimeVLLM, runtimeCacheTestEnabled(false), func(s *inferencev1alpha1.InferenceServiceSpec) {
		s.MultiNode = &inferencev1alpha1.MultiNodeSpec{
			Members: []inferencev1alpha1.MultiNodeMember{
				{Node: "n1", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.1"}},
				{Node: "n2"},
			},
		}
	})
	m := runtimeCacheTestModel(nil)
	pods, _, err := runtimeCacheTestReconciler().constructMemberPods(isvc, m, resolveEffectiveModelCache(isvc, m), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	if len(pods) != 2 {
		t.Fatalf("got %d member pods, want 2", len(pods))
	}
	for i, pod := range pods {
		mount := singleMountAt(t, pod.Spec.Containers[0].VolumeMounts, runtimeCacheMountPath)
		if mount.SubPath != "_runtime-cache/default/svc" {
			t.Errorf("member %d SubPath = %q, want the service's own directory", i, mount.SubPath)
		}
		if got := lastEnvValue(pod.Spec.Containers[0].Env, "VLLM_CACHE_ROOT"); got != "/runtime-cache/vllm" {
			t.Errorf("member %d VLLM_CACHE_ROOT = %q", i, got)
		}
		if prepInitContainer(pod.Spec) == nil {
			t.Errorf("member %d has no runtime-cache-prep init container", i)
		}
	}
}

// singleMountAt returns the one mount at p, failing when there is none or more
// than one (two mounts at one path make the pod invalid).
func singleMountAt(t *testing.T, mounts []corev1.VolumeMount, p string) corev1.VolumeMount {
	t.Helper()
	var found []corev1.VolumeMount
	for _, m := range mounts {
		if m.MountPath == p {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d mounts at %s, want exactly one: %+v", len(found), p, mounts)
	}
	return found[0]
}

// lastEnvValue returns the value the container ends up with for name: a later
// entry for the same variable wins at runtime.
func lastEnvValue(env []corev1.EnvVar, name string) string {
	value := ""
	for _, e := range env {
		if e.Name == name {
			value = e.Value
		}
	}
	return value
}

func prepInitContainer(pod corev1.PodSpec) *corev1.Container {
	for i := range pod.InitContainers {
		if pod.InitContainers[i].Name == runtimeCachePrepContainerName {
			return &pod.InitContainers[i]
		}
	}
	return nil
}

func volumeNamed(pod corev1.PodSpec, name string) *corev1.Volume {
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == name {
			return &pod.Volumes[i]
		}
	}
	return nil
}
