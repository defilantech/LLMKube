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

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// recordingExecutor records starts and stops without spawning anything.
// onStart, when set, runs inside StartProcess (after the agent's pre-start
// checks), so a test can change the cluster mid-start. startErr, when set,
// makes StartProcess fail with that error instead of succeeding, so a test
// can drive reconcileProcess's post-StartProcess error handling (e.g. a
// *ModelDigestMismatchError from a real executor's ensureModel) without a
// real download.
type recordingExecutor struct {
	starts   int
	lastCfg  ExecutorConfig
	stopped  []int
	onStart  func()
	startErr error
	// stopErr is what StopProcess returns when set, e.g. a
	// *KilledAfterGraceError standing in for the grace-period SIGKILL path.
	stopErr error
}

func (e *recordingExecutor) StartProcess(_ context.Context, cfg ExecutorConfig) (*ManagedProcess, error) {
	e.starts++
	e.lastCfg = cfg
	if e.onStart != nil {
		e.onStart()
	}
	if e.startErr != nil {
		return nil, e.startErr
	}
	return &ManagedProcess{Name: cfg.Name, Namespace: cfg.Namespace, PID: 4242, Port: 18080, Healthy: true}, nil
}

func (e *recordingExecutor) StopProcess(pid int) error {
	e.stopped = append(e.stopped, pid)
	if e.stopErr != nil {
		return e.stopErr
	}
	return nil
}

// runtimeObject is client.Object; aliased to keep the fixture signature short.
type runtimeObject = client.Object

// refusalFixture wires an agent with a fake client, a fake recorder and a
// recordingExecutor for the llama.cpp runtime. objs are seeded into the client.
func refusalFixture(
	t *testing.T, cfg MetalAgentConfig, objs ...runtimeObject,
) (*MetalAgent, *recordingExecutor, *record.FakeRecorder) {
	t.Helper()
	return refusalFixtureWithInterceptor(t, cfg, nil, objs...)
}

// refusalFixtureWithInterceptor is refusalFixture with optional interceptor
// funcs installed on the fake client.
func refusalFixtureWithInterceptor(
	t *testing.T, cfg MetalAgentConfig, funcs *interceptor.Funcs, objs ...runtimeObject,
) (*MetalAgent, *recordingExecutor, *record.FakeRecorder) {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{})
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	cfg.K8sClient = b.Build()
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.ModelStorePath == "" {
		cfg.ModelStorePath = t.TempDir()
	}
	cfg.MemoryCheckMode = MemoryCheckModeWarn
	rec := record.NewFakeRecorder(16)
	cfg.EventRecorder = rec
	a := NewMetalAgent(cfg)
	ex := &recordingExecutor{}
	a.executors[runtimeLlamaServer] = ex
	a.executors[runtimeLlamaCPP] = ex
	a.registry = NewServiceRegistry(cfg.K8sClient, "10.0.0.5", newNopLogger(), "")
	return a, ex, rec
}

// refusalModelName is the Model every refusal fixture InferenceService uses.
const refusalModelName = "m"

func refusalISVC(name string) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: refusalModelName},
	}
}

// refusalModel returns the Metal GGUF Model refusal fixtures reference.
func refusalModel() *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: refusalModelName, Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{Source: "https://example.invalid/m.gguf", Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"}},
	}
}

// drainEvents is defined in pressure_test.go and reused here.

// foreignAPIServerService returns a Service shaped like the cluster's own
// "kubernetes" Service: no managed-by label, no owner.
func foreignAPIServerService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default",
			Labels: map[string]string{"component": "apiserver"}},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{Name: "https", Port: 443}},
		},
	}
}

func getService(t *testing.T, c client.Client, name string) *corev1.Service {
	t.Helper()
	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, svc); err != nil {
		t.Fatalf("get Service %s: %v", name, err)
	}
	return svc
}

func assertConflictRefusal(t *testing.T, a *MetalAgent, rec *record.FakeRecorder, err error, name string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), EventReasonEndpointNameConflict) {
		t.Fatalf("ensureProcess error = %v, want %s", err, EventReasonEndpointNameConflict)
	}
	events := drainEvents(rec)
	if !strings.Contains(strings.Join(events, "\n"), "Warning "+EventReasonEndpointNameConflict) {
		t.Errorf("events = %v, want a Warning %s", events, EventReasonEndpointNameConflict)
	}
	got := &inferencev1alpha1.InferenceService{}
	if gErr := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: name, Namespace: "default"}, got); gErr != nil {
		t.Fatalf("get InferenceService: %v", gErr)
	}
	if got.Status.SchedulingStatus != EventReasonEndpointNameConflict {
		t.Errorf("status.schedulingStatus = %q, want %q", got.Status.SchedulingStatus, EventReasonEndpointNameConflict)
	}
}

// An InferenceService whose name is taken by an unowned Service is refused
// before the engine starts: nothing is spawned, a Warning Event and the status
// carry the reason, the foreign Service is untouched and no EndpointSlice is
// created.
func TestEnsureProcess_EndpointNameConflictRefusedBeforeStart(t *testing.T) {
	isvc := refusalISVC("kubernetes")
	a, ex, rec := refusalFixture(t, MetalAgentConfig{},
		isvc, refusalModel(), foreignAPIServerService())
	before := getService(t, a.config.K8sClient, "kubernetes")

	err := a.ensureProcess(context.Background(), isvc)

	assertConflictRefusal(t, a, rec, err, "kubernetes")
	if ex.starts != 0 {
		t.Errorf("engine started %d time(s); a taken endpoint name must be refused before start", ex.starts)
	}
	if after := getService(t, a.config.K8sClient, "kubernetes"); !reflect.DeepEqual(before, after) {
		t.Errorf("foreign Service was modified:\nbefore %+v\nafter  %+v", before, after)
	}
	if gErr := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: "kubernetes", Namespace: "default"},
		&discoveryv1.EndpointSlice{}); !apierrors.IsNotFound(gErr) {
		t.Errorf("EndpointSlice get err = %v, want NotFound", gErr)
	}
}

// The race backstop: the name is free at the pre-start check but a foreign
// Service appears while the engine is starting. Registration conflicts, so the
// engine is stopped, the refusal is reported, and the foreign Service is left
// as its creator wrote it.
func TestEnsureProcess_EndpointNameConflictBackstopStopsEngine(t *testing.T) {
	isvc := refusalISVC("kubernetes")
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel())
	ex.onStart = func() {
		if err := a.config.K8sClient.Create(context.Background(), foreignAPIServerService()); err != nil {
			t.Errorf("create foreign Service mid-start: %v", err)
		}
	}

	err := a.ensureProcess(context.Background(), isvc)

	assertConflictRefusal(t, a, rec, err, "kubernetes")
	if ex.starts != 1 {
		t.Errorf("starts = %d, want 1 (the pre-check saw a free name)", ex.starts)
	}
	if !reflect.DeepEqual(ex.stopped, []int{4242}) {
		t.Errorf("stopped = %v, want [4242]", ex.stopped)
	}
	after := getService(t, a.config.K8sClient, "kubernetes")
	if after.Labels[managedByLabel] != "" || after.Labels["component"] != "apiserver" ||
		len(after.Spec.Ports) != 1 || after.Spec.Ports[0].Port != 443 {
		t.Errorf("foreign Service was modified: %+v", after)
	}
}

// Re-evaluating a refused InferenceService (the watcher re-delivers it on
// every resourceVersion change) must not flap the status through "" and must
// never start the engine.
func TestEnsureProcess_EndpointNameConflictNoStatusFlap(t *testing.T) {
	var written []string
	funcs := &interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string,
			obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if is, ok := obj.(*inferencev1alpha1.InferenceService); ok && sub == "status" {
				written = append(written, is.Status.SchedulingStatus)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	isvc := refusalISVC("kubernetes")
	// A sized model under a generous absolute budget, so memory admission
	// passes: its success path clears a non-empty SchedulingStatus, which is
	// the flap this test guards against.
	model := refusalModel()
	model.Spec.Hardware.MemoryBudget = "1Ti"
	model.Status.Size = "1 GiB"
	a, ex, _ := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, funcs,
		isvc, model, foreignAPIServerService())

	for i := range 2 {
		cur := &inferencev1alpha1.InferenceService{}
		if err := a.config.K8sClient.Get(context.Background(),
			types.NamespacedName{Name: "kubernetes", Namespace: "default"}, cur); err != nil {
			t.Fatal(err)
		}
		if err := a.ensureProcess(context.Background(), cur); err == nil {
			t.Fatalf("call %d: ensureProcess succeeded, want a conflict refusal", i+1)
		}
	}

	if ex.starts != 0 {
		t.Errorf("starts = %d after two refused evaluations, want 0", ex.starts)
	}
	if len(written) == 0 {
		t.Fatal("no status writes recorded; the refusal must be reported on the status")
	}
	for i, s := range written {
		if s != EventReasonEndpointNameConflict {
			t.Errorf("status write %d set schedulingStatus = %q, want %q (writes: %q)",
				i, s, EventReasonEndpointNameConflict, written)
		}
	}
}

// Switching an InferenceService from a CUDA Model to a Metal Model leaves the
// Service the controller created for it (controller ownerReference to the
// same InferenceService, no managed-by label). The agent takes it over
// instead of refusing the migration.
func TestEnsureProcess_ControllerOwnedServiceIsTakenOver(t *testing.T) {
	isvc := refusalISVC("migrated")
	isvc.UID = "isvc-uid-migrated"
	owned := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "migrated", Namespace: "default",
			Labels: map[string]string{"app": "migrated"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: inferencev1alpha1.GroupVersion.String(), Kind: "InferenceService",
				Name: "migrated", UID: isvc.UID, Controller: ptr.To(true),
			}}},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "migrated"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 8080}},
		},
	}
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel(), owned)

	if err := a.ensureProcess(context.Background(), isvc); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	if ex.starts != 1 {
		t.Errorf("starts = %d, want 1", ex.starts)
	}
	if len(ex.stopped) != 0 {
		t.Errorf("stopped = %v, want none", ex.stopped)
	}
	svc := getService(t, a.config.K8sClient, "migrated")
	if svc.Labels[managedByLabel] != managedByValue {
		t.Errorf("Service labels = %v, want %s=%s", svc.Labels, managedByLabel, managedByValue)
	}
}

// With no roots configured, the model store is the only root.
func TestNewMetalAgent_DefaultRootIsModelStore(t *testing.T) {
	store := t.TempDir()
	a := NewMetalAgent(MetalAgentConfig{ModelStorePath: store})
	want, _ := filepath.EvalSymlinks(store)
	if got := a.roots.Dirs(); len(got) != 1 || got[0] != want {
		t.Errorf("roots = %v, want [%s]", got, want)
	}
}

// The model store is always a root: configuring AllowedModelRoots adds to it,
// it never replaces it, so downloads that land in the store keep passing.
func TestNewMetalAgent_ConfiguredRoots(t *testing.T) {
	store, extra := t.TempDir(), t.TempDir()
	a := NewMetalAgent(MetalAgentConfig{ModelStorePath: store, AllowedModelRoots: []string{extra}})
	wantStore, _ := filepath.EvalSymlinks(store)
	wantExtra, _ := filepath.EvalSymlinks(extra)
	got := a.roots.Dirs()
	if len(got) != 2 {
		t.Fatalf("roots = %v, want the model store and the configured extra root", got)
	}
	want := map[string]bool{wantStore: true, wantExtra: true}
	for _, d := range got {
		if !want[d] {
			t.Errorf("roots = %v, unexpected entry %q", got, d)
		}
	}
}

// Listing the model store again in AllowedModelRoots does not duplicate it.
func TestNewMetalAgent_ConfiguredRootsDedupesModelStore(t *testing.T) {
	store, extra := t.TempDir(), t.TempDir()
	a := NewMetalAgent(MetalAgentConfig{ModelStorePath: store, AllowedModelRoots: []string{store, extra}})
	if got := a.roots.Dirs(); len(got) != 2 {
		t.Errorf("roots = %v, want 2 (the model store deduped against AllowedModelRoots)", got)
	}
}

// A "~"-prefixed AllowedModelRoots entry expands against the process's home
// directory, the same way NewMetalAgent resolves it via os.UserHomeDir().
// --allowed-model-roots ~/llmkube-models must not be rejected as relative
// (the pre-fix behavior, which made cmd/metal-agent exit 1 and launchd
// restart-loop it).
func TestNewMetalAgent_ConfiguredRootsExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := t.TempDir()
	mkTestFile(t, filepath.Join(home, "llmkube-models", "placeholder"))

	a := NewMetalAgent(MetalAgentConfig{ModelStorePath: store, AllowedModelRoots: []string{"~/llmkube-models"}})

	if a.home != home {
		t.Errorf("a.home = %q, want %q", a.home, home)
	}
	wantStore, _ := filepath.EvalSymlinks(store)
	wantExtra, _ := filepath.EvalSymlinks(filepath.Join(home, "llmkube-models"))
	got := a.roots.Dirs()
	if len(got) != 2 {
		t.Fatalf("roots = %v, want the model store and the expanded ~/llmkube-models root", got)
	}
	want := map[string]bool{wantStore: true, wantExtra: true}
	for _, d := range got {
		if !want[d] {
			t.Errorf("roots = %v, unexpected entry %q", got, d)
		}
	}
}

// mkTestFile writes a small stub file, creating its parent directory.
func mkTestFile(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A local model source outside every allowed root is refused before the
// engine starts.
func TestEnsureProcess_RefusesLocalSourceOutsideRoots(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "m.gguf")
	mkTestFile(t, outside)
	isvc := refusalISVC("svc")
	model := refusalModel()
	model.Spec.Source = outside
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, model)

	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), EventReasonModelSourceNotAllowed) {
		t.Fatalf("error = %v, want %s", err, EventReasonModelSourceNotAllowed)
	}
	if ex.starts != 0 {
		t.Errorf("engine started %d times; want 0", ex.starts)
	}
	if !strings.Contains(strings.Join(drainEvents(rec), "\n"), "Warning "+EventReasonModelSourceNotAllowed) {
		t.Error("no ModelSourceNotAllowed Warning event")
	}
}

// A Model the controller marked Failed is refused; the agent must not serve a
// Model the controller already rejected.
func TestEnsureProcess_RefusesFailedModel(t *testing.T) {
	isvc := refusalISVC("svc")
	model := refusalModel()
	model.Status.Phase = inferencev1alpha1.PhaseFailed
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, isvc, model)
	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), EventReasonModelSourceNotAllowed) || ex.starts != 0 {
		t.Fatalf("error = %v starts = %d, want refusal and no start", err, ex.starts)
	}
}

// A Failed Model refusal happens before memory admission ever runs: two
// ensureProcess calls in a row must not flap the status between the refusal
// reason and "" (which the memory-admission success path would otherwise
// clear on an admitted model). The model here is sized and budgeted so it
// WOULD pass memory admission if checkModelPaths ran after it, and every
// status write in between the two calls is captured via an interceptor so a
// clear-then-set within a single call cannot hide behind the final read.
func TestEnsureProcess_RefusesFailedModelNoFlap(t *testing.T) {
	var written []string
	funcs := &interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string,
			obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if is, ok := obj.(*inferencev1alpha1.InferenceService); ok && sub == "status" {
				written = append(written, is.Status.SchedulingStatus)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	isvc := refusalISVC("svc")
	model := refusalModel()
	model.Spec.Hardware.MemoryBudget = "1Ti"
	model.Status.Size = "1 GiB"
	model.Status.Phase = inferencev1alpha1.PhaseFailed
	a, ex, _ := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, funcs, isvc, model)

	for i := range 2 {
		cur := &inferencev1alpha1.InferenceService{}
		if err := a.config.K8sClient.Get(context.Background(),
			types.NamespacedName{Name: "svc", Namespace: "default"}, cur); err != nil {
			t.Fatal(err)
		}
		if err := a.ensureProcess(context.Background(), cur); err == nil {
			t.Fatalf("call %d: ensureProcess succeeded, want a refusal", i+1)
		}
	}

	if ex.starts != 0 {
		t.Errorf("starts = %d after two refused evaluations, want 0", ex.starts)
	}
	if len(written) == 0 {
		t.Fatal("no status writes recorded; the refusal must be reported on the status")
	}
	for i, s := range written {
		if s != EventReasonModelSourceNotAllowed {
			t.Errorf("status write %d set schedulingStatus = %q, want %q (writes: %q)",
				i, s, EventReasonModelSourceNotAllowed, written)
		}
	}
}

// A local source inside a root starts, and the engine receives the path as
// written (unresolved), preserving split-GGUF sibling lookup (#1920).
func TestEnsureProcess_LocalSourceInsideSymlinkedRootStarts(t *testing.T) {
	store, hf := t.TempDir(), t.TempDir()
	mkTestFile(t, filepath.Join(hf, "snap", "m.gguf"))
	link := filepath.Join(store, "linked")
	if err := os.Symlink(filepath.Join(hf, "snap"), link); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(link, "m.gguf")
	isvc := refusalISVC("svc")
	model := refusalModel()
	model.Spec.Source = src
	a, ex, _ := refusalFixture(t, MetalAgentConfig{ModelStorePath: store, AllowedModelRoots: []string{store, hf}},
		isvc, model)
	if err := a.ensureProcess(context.Background(), isvc); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	if ex.starts != 1 || ex.lastCfg.ModelSource != src {
		t.Errorf("starts=%d ModelSource=%q, want 1 start with the unresolved %q", ex.starts, ex.lastCfg.ModelSource, src)
	}
}

// oMLX's pagedSSDCacheDir is checked against the allowed roots just like a
// local model source, even when the model itself streams from a remote
// source.
func TestEnsureProcess_RefusesPagedSSDCacheDirOutsideRoots(t *testing.T) {
	isvc := refusalISVC("svc")
	isvc.Spec.Runtime = runtimeOMLX
	isvc.Spec.PagedSSDCacheDir = ptr.To(t.TempDir())
	model := refusalModel()
	model.Spec.Format = formatMLX
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, model)
	a.executors[runtimeOMLX] = ex

	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), EventReasonModelSourceNotAllowed) {
		t.Fatalf("error = %v, want %s", err, EventReasonModelSourceNotAllowed)
	}
	if ex.starts != 0 {
		t.Errorf("engine started %d times; want 0", ex.starts)
	}
	if !strings.Contains(strings.Join(drainEvents(rec), "\n"), "Warning "+EventReasonModelSourceNotAllowed) {
		t.Error("no ModelSourceNotAllowed Warning event")
	}
}

// A scheme-less relative model source (no "://", no leading "/") that
// contains a ".." component is refused before the engine starts. Every
// executor's resolveModelPath joins a relative ModelSource onto the model
// store with filepath.Join, which cleans ".." out of the joined path, so an
// unchecked value like this would otherwise resolve outside the store.
func TestEnsureProcess_RefusesRelativeSourceEscape(t *testing.T) {
	isvc := refusalISVC("svc")
	isvc.Spec.Runtime = runtimeMLXServer
	model := refusalModel()
	model.Spec.Format = formatMLX
	model.Spec.Source = "a/../../outside"
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, model)
	a.executors[runtimeMLXServer] = ex

	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), EventReasonModelSourceNotAllowed) {
		t.Fatalf("error = %v, want %s", err, EventReasonModelSourceNotAllowed)
	}
	if ex.starts != 0 {
		t.Errorf("engine started %d times; want 0", ex.starts)
	}
	if !strings.Contains(strings.Join(drainEvents(rec), "\n"), "Warning "+EventReasonModelSourceNotAllowed) {
		t.Error("no ModelSourceNotAllowed Warning event")
	}
}

// A relative source that is a plain repository id (no ".." component, no
// scheme) resolves under the model store even though nothing exists there
// yet, so it is not refused: it starts like any other not-yet-downloaded
// source.
func TestEnsureProcess_RelativeRepoIDStillStarts(t *testing.T) {
	isvc := refusalISVC("svc")
	isvc.Spec.Runtime = runtimeMLXServer
	model := refusalModel()
	model.Spec.Format = formatMLX
	model.Spec.Source = "lmstudio-community/Qwen3.8-27B-MLX-4bit"
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, isvc, model)
	a.executors[runtimeMLXServer] = ex

	if err := a.ensureProcess(context.Background(), isvc); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	if ex.starts != 1 {
		t.Errorf("starts = %d, want 1", ex.starts)
	}
}

// A relative source resolved against a symlink inside the store that points
// outside every root is refused: ResolvePath resolves the symlink component
// before the root containment check runs.
func TestEnsureProcess_RefusesRelativeSourceThroughSymlinkEscape(t *testing.T) {
	store, outside := t.TempDir(), t.TempDir()
	mkTestFile(t, filepath.Join(outside, "model", "weights.gguf"))
	if err := os.Symlink(filepath.Join(outside, "model"), filepath.Join(store, "linked")); err != nil {
		t.Fatal(err)
	}
	isvc := refusalISVC("svc")
	model := refusalModel()
	model.Spec.Source = "linked/weights.gguf"
	a, ex, rec := refusalFixture(t, MetalAgentConfig{ModelStorePath: store}, isvc, model)

	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), EventReasonModelSourceNotAllowed) {
		t.Fatalf("error = %v, want %s", err, EventReasonModelSourceNotAllowed)
	}
	if ex.starts != 0 {
		t.Errorf("engine started %d times; want 0", ex.starts)
	}
	if !strings.Contains(strings.Join(drainEvents(rec), "\n"), "Warning "+EventReasonModelSourceNotAllowed) {
		t.Error("no ModelSourceNotAllowed Warning event")
	}
}

// A ".." path segment in the model source is refused regardless of scheme.
// mlx-server, vllm-swift and tensorfold's resolveModelPath all filepath.Join
// a non-absolute ModelSource onto the model store even when it carries a
// scheme (only an absolute local path is left as-is), so a value like
// "https://../../../../Users/victim/x" walks the joined path back out of the
// store. The CRD's Source pattern allows any character after a scheme, and
// the controller marks a remote-looking source Ready (never Failed), so
// neither the pattern nor the Failed-Model check catches this.
func TestEnsureProcess_RefusesDotDotInAnySource(t *testing.T) {
	cases := []string{
		"https://../../../../Users/x",
		"hf://a/../../../x",
		"s3://bucket/../../x",
		"pvc://claim/../../x",
	}
	for _, source := range cases {
		t.Run(source, func(t *testing.T) {
			isvc := refusalISVC("svc")
			isvc.Spec.Runtime = runtimeMLXServer
			model := refusalModel()
			model.Spec.Format = formatMLX
			model.Spec.Source = source
			a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, model)
			a.executors[runtimeMLXServer] = ex

			err := a.ensureProcess(context.Background(), isvc)
			if err == nil || !strings.Contains(err.Error(), EventReasonModelSourceNotAllowed) {
				t.Fatalf("error = %v, want %s", err, EventReasonModelSourceNotAllowed)
			}
			if ex.starts != 0 {
				t.Errorf("engine started %d times; want 0", ex.starts)
			}
			if !strings.Contains(strings.Join(drainEvents(rec), "\n"), "Warning "+EventReasonModelSourceNotAllowed) {
				t.Error("no ModelSourceNotAllowed Warning event")
			}
		})
	}
}

// A normal remote source with no ".." segment is not refused by the ".."
// rule. Status.Size is set so the pre-flight memory check resolves from it
// instead of issuing a real HEAD request against the (fake) URL.
func TestEnsureProcess_NormalRemoteSourceNotRefusedByDotDotRule(t *testing.T) {
	isvc := refusalISVC("svc")
	isvc.Spec.Runtime = runtimeLlamaCPP
	model := refusalModel()
	model.Spec.Source = "https://huggingface.co/org/repo/resolve/main/m.gguf"
	model.Status.Size = "1 GiB"
	a, _, _ := refusalFixture(t, MetalAgentConfig{}, isvc, model)

	if err := a.ensureProcess(context.Background(), isvc); err != nil &&
		strings.Contains(err.Error(), EventReasonModelSourceNotAllowed) {
		t.Fatalf("ensureProcess refused a normal remote source: %v", err)
	}
}

// A dangerous extraArgs flag (llama-server's --path serves the model store
// over HTTP) is refused before the engine starts.
func TestEnsureProcess_RefusesDangerousExtraArgs(t *testing.T) {
	isvc := refusalISVC("svc")
	isvc.Spec.ExtraArgs = []string{"--path", "/"}
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel())

	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), EventReasonExtraArgsRejected) || ex.starts != 0 {
		t.Fatalf("error = %v starts = %d, want ExtraArgsRejected and no start", err, ex.starts)
	}
	if !strings.Contains(strings.Join(drainEvents(rec), "\n"), `Warning ExtraArgsRejected extraArgs flag "--path"`) {
		t.Error("event does not name the refused flag")
	}
}

// Tuning flags the policy allows (a plain value flag and a negative-number
// value) are not treated as paths or bind flags and start normally, with the
// args passed to the executor unchanged.
func TestEnsureProcess_TuningExtraArgsStillPass(t *testing.T) {
	isvc := refusalISVC("svc")
	isvc.Spec.ExtraArgs = []string{"--spec-type", "draft-mtp", "--cache-ram", "-1"}
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel())

	if err := a.ensureProcess(context.Background(), isvc); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	if ex.starts != 1 || !reflect.DeepEqual(ex.lastCfg.ExtraArgs, isvc.Spec.ExtraArgs) {
		t.Errorf("starts=%d extraArgs=%v, want 1 start with the args unchanged", ex.starts, ex.lastCfg.ExtraArgs)
	}
}

// Re-evaluating a refused InferenceService must not flap the status through ""
// between calls, mirroring TestEnsureProcess_EndpointNameConflictNoStatusFlap:
// the model here is sized and budgeted so it WOULD pass memory admission if
// checkExtraArgs ran after it instead (the flap this test guards against), and
// every status write across two ensureProcess calls is captured so a
// clear-then-set within a single call cannot hide behind the final read.
func TestEnsureProcess_ExtraArgsRejectedNoStatusFlap(t *testing.T) {
	var written []string
	funcs := &interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string,
			obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if is, ok := obj.(*inferencev1alpha1.InferenceService); ok && sub == "status" {
				written = append(written, is.Status.SchedulingStatus)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	isvc := refusalISVC("svc")
	isvc.Spec.ExtraArgs = []string{"--path", "/"}
	model := refusalModel()
	model.Spec.Hardware.MemoryBudget = "1Ti"
	model.Status.Size = "1 GiB"
	a, ex, _ := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, funcs, isvc, model)

	for i := range 2 {
		cur := &inferencev1alpha1.InferenceService{}
		if err := a.config.K8sClient.Get(context.Background(),
			types.NamespacedName{Name: "svc", Namespace: "default"}, cur); err != nil {
			t.Fatal(err)
		}
		if err := a.ensureProcess(context.Background(), cur); err == nil {
			t.Fatalf("call %d: ensureProcess succeeded, want a refusal", i+1)
		}
	}

	if ex.starts != 0 {
		t.Errorf("starts = %d after two refused evaluations, want 0", ex.starts)
	}
	if len(written) == 0 {
		t.Fatal("no status writes recorded; the refusal must be reported on the status")
	}
	for i, s := range written {
		if s != EventReasonExtraArgsRejected {
			t.Errorf("status write %d set schedulingStatus = %q, want %q (writes: %q)",
				i, s, EventReasonExtraArgsRejected, written)
		}
	}
}

// --allow-unsafe-extra-args relaxes a Rule 1 refused-but-not-bind flag
// (--log-file writes a file) but never a bind flag (--host sets the bind
// address): the hatch cannot be used to move where the engine listens.
func TestEnsureProcess_AllowUnsafeExtraArgsRelaxesButNotBind(t *testing.T) {
	t.Run("relaxed flag passes", func(t *testing.T) {
		isvc := refusalISVC("svc")
		isvc.Spec.ExtraArgs = []string{"--log-file", "/tmp/x"}
		a, ex, _ := refusalFixture(t, MetalAgentConfig{AllowUnsafeExtraArgs: true}, isvc, refusalModel())

		if err := a.ensureProcess(context.Background(), isvc); err != nil {
			t.Fatalf("ensureProcess: %v", err)
		}
		if ex.starts != 1 {
			t.Errorf("starts = %d, want 1 (AllowUnsafe should let --log-file through)", ex.starts)
		}
	})

	t.Run("bind flag still refused", func(t *testing.T) {
		isvc := refusalISVC("svc")
		isvc.Spec.ExtraArgs = []string{"--host", "0.0.0.0"}
		a, ex, _ := refusalFixture(t, MetalAgentConfig{AllowUnsafeExtraArgs: true}, isvc, refusalModel())

		err := a.ensureProcess(context.Background(), isvc)
		if err == nil || !strings.Contains(err.Error(), EventReasonExtraArgsRejected) || ex.starts != 0 {
			t.Fatalf("error = %v starts = %d, want ExtraArgsRejected and no start even with AllowUnsafe",
				err, ex.starts)
		}
	})
}

// A SHA256 mismatch surfaces from the real executor as a *ModelDigestMismatchError
// wrapped inside StartProcess's "failed to ensure model" and "failed to start
// process" errors (ensureModel -> StartProcess); reconcileProcess must unwrap
// it with errors.As and route it through refuseStart, so the mismatch shows
// as an Event and a status field, exactly like the *EndpointNameConflictError
// handling a few lines below the StartProcess call. Every other StartProcess failure keeps
// the old plain-wrapped-error, log-only behavior (TestEnsureProcess_
// EndpointNameConflictBackstopStopsEngine and friends above cover that this
// branch was not broadly widened).
func TestEnsureProcess_ModelDigestMismatchRefusedWithEvent(t *testing.T) {
	isvc := refusalISVC("svc")
	digestErr := &ModelDigestMismatchError{
		Path:     "/models/m/model.gguf",
		Expected: "aaaa",
		Computed: "bbbb",
	}
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel())
	ex.startErr = fmt.Errorf("failed to ensure model: %w", digestErr)

	err := a.ensureProcess(context.Background(), isvc)

	if err == nil || !strings.Contains(err.Error(), EventReasonModelDigestMismatch) {
		t.Fatalf("ensureProcess error = %v, want it to name %s", err, EventReasonModelDigestMismatch)
	}
	events := drainEvents(rec)
	if !strings.Contains(strings.Join(events, "\n"), "Warning "+EventReasonModelDigestMismatch) {
		t.Errorf("events = %v, want a Warning %s", events, EventReasonModelDigestMismatch)
	}
	got := &inferencev1alpha1.InferenceService{}
	if gErr := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: "svc", Namespace: "default"}, got); gErr != nil {
		t.Fatalf("get InferenceService: %v", gErr)
	}
	if got.Status.SchedulingStatus != EventReasonModelDigestMismatch {
		t.Errorf("status.schedulingStatus = %q, want %q", got.Status.SchedulingStatus, EventReasonModelDigestMismatch)
	}
	if ex.starts != 1 {
		t.Errorf("starts = %d, want 1 (the executor was invoked and itself reported the mismatch)", ex.starts)
	}
}

// A symlink in a downloaded source's cache slot (<store>/<model>/<file>) is
// refused before memory admission with ModelSourceNotAllowed, as a status
// field and a Warning Event naming the supported replacement, and the
// executor is never invoked.
func TestEnsureProcess_SymlinkedCacheSlotRefusedBeforeStart(t *testing.T) {
	isvc := refusalISVC("svc")
	store := t.TempDir()
	a, ex, rec := refusalFixture(t, MetalAgentConfig{ModelStorePath: store}, isvc, refusalModel())
	slotDir := filepath.Join(store, refusalModelName)
	if err := os.Mkdir(slotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	handPlaced := filepath.Join(t.TempDir(), "hand-placed.gguf")
	if err := os.WriteFile(handPlaced, []byte("gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(handPlaced, filepath.Join(slotDir, "m.gguf")); err != nil {
		t.Fatal(err)
	}

	err := a.ensureProcess(context.Background(), isvc)

	if err == nil || !strings.Contains(err.Error(), EventReasonModelSourceNotAllowed) {
		t.Fatalf("ensureProcess error = %v, want it to name %s", err, EventReasonModelSourceNotAllowed)
	}
	for _, want := range []string{"file://", "--allowed-model-roots"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the remedy %q", err, want)
		}
	}
	events := drainEvents(rec)
	if !strings.Contains(strings.Join(events, "\n"), "Warning "+EventReasonModelSourceNotAllowed) {
		t.Errorf("events = %v, want a Warning %s", events, EventReasonModelSourceNotAllowed)
	}
	got := &inferencev1alpha1.InferenceService{}
	if gErr := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: "svc", Namespace: "default"}, got); gErr != nil {
		t.Fatalf("get InferenceService: %v", gErr)
	}
	if got.Status.SchedulingStatus != EventReasonModelSourceNotAllowed {
		t.Errorf("status.schedulingStatus = %q, want %q", got.Status.SchedulingStatus, EventReasonModelSourceNotAllowed)
	}
	if ex.starts != 0 {
		t.Errorf("starts = %d, want 0 (refused before the executor ran)", ex.starts)
	}
}

// If the cache slot becomes a symlink after the pre-flight check, the
// executor's *ModelCacheEntryNotRegularError still reaches refuseStart with
// ModelSourceNotAllowed rather than the log-only plain-failure path.
func TestEnsureProcess_CacheSlotErrorFromStartRefused(t *testing.T) {
	isvc := refusalISVC("svc")
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel())
	ex.startErr = fmt.Errorf("failed to ensure model: %w",
		&ModelCacheEntryNotRegularError{Path: "/models/m/m.gguf", Mode: os.ModeSymlink | 0o755})

	err := a.ensureProcess(context.Background(), isvc)

	if err == nil || !strings.Contains(err.Error(), EventReasonModelSourceNotAllowed) {
		t.Fatalf("ensureProcess error = %v, want it to name %s", err, EventReasonModelSourceNotAllowed)
	}
	if events := drainEvents(rec); !strings.Contains(strings.Join(events, "\n"),
		"Warning "+EventReasonModelSourceNotAllowed) {
		t.Errorf("events = %v, want a Warning %s", events, EventReasonModelSourceNotAllowed)
	}
	if ex.starts != 1 {
		t.Errorf("starts = %d, want 1", ex.starts)
	}
}

// A plain (non-digest) StartProcess failure must NOT be routed through
// refuseStart: a transient network or process-spawn failure is not a refusal
// to park behind a refusal reason. It is reported as StartFailed, which the
// agent clears itself once a retry starts the engine, and retried on the
// restart backoff rather than the refusal re-check (#2404). This test used to
// require no Event and an empty status, which was the #2404 bug.
func TestEnsureProcess_PlainStartFailureNotRefused(t *testing.T) {
	isvc := refusalISVC("svc")
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel())
	ex.startErr = fmt.Errorf("failed to ensure model: connection refused")

	err := a.ensureProcess(context.Background(), isvc)

	if err == nil || strings.Contains(err.Error(), EventReasonModelDigestMismatch) {
		t.Fatalf("ensureProcess error = %v, want a plain failure not naming %s", err, EventReasonModelDigestMismatch)
	}
	events := drainEvents(rec)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning "+EventReasonStartFailed+" ") {
		t.Errorf("events = %v, want only a Warning %s", events, EventReasonStartFailed)
	}
	got := &inferencev1alpha1.InferenceService{}
	if gErr := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: "svc", Namespace: "default"}, got); gErr != nil {
		t.Fatalf("get InferenceService: %v", gErr)
	}
	if got.Status.SchedulingStatus != EventReasonStartFailed {
		t.Errorf("status.schedulingStatus = %q, want %s for a plain start failure",
			got.Status.SchedulingStatus, EventReasonStartFailed)
	}
	a.refusals.mu.Lock()
	_, refused := a.refusals.records["default/svc"]
	a.refusals.mu.Unlock()
	if refused {
		t.Errorf("a plain start failure was recorded as a refusal")
	}
	if !restartPending(a, "default/svc") {
		t.Errorf("a plain start failure was not queued for a retry")
	}
}

// TestEnsureProcess_DigestMismatchMemoized_NoRepeatedDownload is the
// regression test for the download-storm fix: without the pre-flight
// checkModelDigestMemo check, a persistent digest mismatch is discovered
// inside StartProcess, which runs AFTER checkMemoryAdmission; admission's
// success path clears Status.SchedulingStatus (a Status().Update) and the
// subsequent refusal writes it back (another Status().Update), bumping the
// resourceVersion each time so the watcher sees UPDATED and reconciles
// again, each pass re-running StartProcess's ensureModel, i.e. re-downloading
// the entire model. With the memo, only the FIRST reconcile reaches StartProcess;
// the next two hit the memo in the pre-flight check and refuse immediately,
// so the executor is invoked exactly once and SchedulingStatus is never
// cleared in between (every write recorded via the interceptor is the same
// refusal reason).
func TestEnsureProcess_DigestMismatchMemoized_NoRepeatedDownload(t *testing.T) {
	var written []string
	funcs := &interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string,
			obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if is, ok := obj.(*inferencev1alpha1.InferenceService); ok && sub == "status" {
				written = append(written, is.Status.SchedulingStatus)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	isvc := refusalISVC("svc")
	model := refusalModel()
	model.Spec.SHA256 = "aaaa"
	a, ex, _ := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, funcs, isvc, model)
	ex.startErr = fmt.Errorf("failed to ensure model: %w",
		&ModelDigestMismatchError{Path: "/models/m/model.gguf", Expected: "aaaa", Computed: "bbbb"})

	for i := range 3 {
		cur := &inferencev1alpha1.InferenceService{}
		if err := a.config.K8sClient.Get(context.Background(),
			types.NamespacedName{Name: "svc", Namespace: "default"}, cur); err != nil {
			t.Fatal(err)
		}
		if err := a.ensureProcess(context.Background(), cur); err == nil {
			t.Fatalf("call %d: ensureProcess succeeded, want a digest-mismatch refusal", i+1)
		}
	}

	if ex.starts != 1 {
		t.Errorf("StartProcess invoked %d time(s) over 3 reconciles of an unchanged mismatching spec, want exactly 1 "+
			"(the pre-flight memo should have refused the other 2 without downloading)", ex.starts)
	}
	if len(written) == 0 {
		t.Fatal("no status writes recorded; the refusal must be reported on the status")
	}
	for i, s := range written {
		if s != EventReasonModelDigestMismatch {
			t.Errorf("status write %d = %q, want %q (writes: %q); a clear-then-set in between would mean "+
				"memory admission's success path ran, which is exactly the status churn this memo prevents",
				i, s, EventReasonModelDigestMismatch, written)
		}
	}
}

// TestEnsureProcess_DigestMismatchMemo_ClearsOnSHA256Change proves the memo
// self-invalidates on a spec edit rather than permanently wedging a Model:
// correcting spec.sha256 to match the file's real digest must retry the
// download, not be blocked by a memo recorded against the old, wrong digest.
func TestEnsureProcess_DigestMismatchMemo_ClearsOnSHA256Change(t *testing.T) {
	isvc := refusalISVC("svc")
	model := refusalModel()
	model.Spec.SHA256 = "aaaa"
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, isvc, model)
	ex.startErr = fmt.Errorf("failed to ensure model: %w",
		&ModelDigestMismatchError{Path: "/models/m/model.gguf", Expected: "aaaa", Computed: "bbbb"})

	get := func() *inferencev1alpha1.InferenceService {
		t.Helper()
		cur := &inferencev1alpha1.InferenceService{}
		if err := a.config.K8sClient.Get(context.Background(),
			types.NamespacedName{Name: "svc", Namespace: "default"}, cur); err != nil {
			t.Fatal(err)
		}
		return cur
	}

	if err := a.ensureProcess(context.Background(), get()); err == nil {
		t.Fatal("first ensureProcess succeeded, want a digest-mismatch refusal")
	}
	if err := a.ensureProcess(context.Background(), get()); err == nil {
		t.Fatal("second ensureProcess succeeded, want the memoized refusal")
	}
	if ex.starts != 1 {
		t.Fatalf("starts after two reconciles of an unchanged mismatching spec = %d, want 1", ex.starts)
	}

	// Correct the Model's sha256 to what the (simulated) file actually
	// hashes to. The memo was keyed on the OLD sha256, so it must not apply.
	curModel := &inferencev1alpha1.Model{}
	if err := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: refusalModelName, Namespace: "default"}, curModel); err != nil {
		t.Fatal(err)
	}
	curModel.Spec.SHA256 = "bbbb"
	if err := a.config.K8sClient.Update(context.Background(), curModel); err != nil {
		t.Fatal(err)
	}
	ex.startErr = nil // the corrected digest verifies successfully this time

	if err := a.ensureProcess(context.Background(), get()); err != nil {
		t.Fatalf("ensureProcess after correcting sha256: %v", err)
	}
	if ex.starts != 2 {
		t.Errorf("starts after correcting sha256 = %d, want 2 (a spec change must not be blocked by the stale memo)",
			ex.starts)
	}
}

// A download over --max-download-size surfaces from the real executor as a
// wrapped *ModelDownloadTooLargeError. It must be refused through refuseStart
// with ModelDownloadTooLarge (status field plus Warning Event) and memoized,
// so later reconciles of the unchanged spec refuse in the pre-flight check
// instead of downloading up to the cap again. The Model has no spec.sha256:
// the memo must not be skipped for it, since an over-cap download needs no
// digest to be refused.
func TestEnsureProcess_DownloadTooLargeRefusedAndMemoized(t *testing.T) {
	var written []string
	funcs := &interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string,
			obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if is, ok := obj.(*inferencev1alpha1.InferenceService); ok && sub == "status" {
				written = append(written, is.Status.SchedulingStatus)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	isvc := refusalISVC("svc")
	a, ex, rec := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, funcs, isvc, refusalModel())
	ex.startErr = fmt.Errorf("failed to ensure model: failed to download model: %w",
		&ModelDownloadTooLargeError{Path: "/models/m/m.gguf", Limit: 1 << 30})

	for i := range 3 {
		cur := &inferencev1alpha1.InferenceService{}
		if err := a.config.K8sClient.Get(context.Background(),
			types.NamespacedName{Name: "svc", Namespace: "default"}, cur); err != nil {
			t.Fatal(err)
		}
		err := a.ensureProcess(context.Background(), cur)
		if err == nil || !strings.Contains(err.Error(), EventReasonModelDownloadTooLarge) {
			t.Fatalf("call %d: ensureProcess error = %v, want it to name %s", i+1, err, EventReasonModelDownloadTooLarge)
		}
	}

	if ex.starts != 1 {
		t.Errorf("StartProcess invoked %d time(s) over 3 reconciles of an unchanged over-cap spec, want exactly 1 "+
			"(the memo should have refused the other 2 without downloading)", ex.starts)
	}
	if len(written) == 0 {
		t.Fatal("no status writes recorded; the refusal must be reported on the status")
	}
	for i, s := range written {
		if s != EventReasonModelDownloadTooLarge {
			t.Errorf("status write %d = %q, want %q (writes: %q)", i, s, EventReasonModelDownloadTooLarge, written)
		}
	}
	events := strings.Join(drainEvents(rec), "\n")
	if !strings.Contains(events, "Warning "+EventReasonModelDownloadTooLarge) ||
		!strings.Contains(events, "--max-download-size") {
		t.Errorf("events = %q, want a Warning %s naming --max-download-size", events, EventReasonModelDownloadTooLarge)
	}
}

// Editing the Model's source clears the over-cap memo, so a corrected source
// is downloaded again rather than refused for the old one.
func TestEnsureProcess_DownloadTooLargeMemo_ClearsOnSourceChange(t *testing.T) {
	isvc := refusalISVC("svc")
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel())
	ex.startErr = fmt.Errorf("failed to ensure model: %w",
		&ModelDownloadTooLargeError{Path: "/models/m/m.gguf", Limit: 1 << 30})
	get := func() *inferencev1alpha1.InferenceService {
		t.Helper()
		cur := &inferencev1alpha1.InferenceService{}
		if err := a.config.K8sClient.Get(context.Background(),
			types.NamespacedName{Name: "svc", Namespace: "default"}, cur); err != nil {
			t.Fatal(err)
		}
		return cur
	}

	for range 2 {
		if err := a.ensureProcess(context.Background(), get()); err == nil {
			t.Fatal("ensureProcess succeeded, want an over-cap refusal")
		}
	}
	if ex.starts != 1 {
		t.Fatalf("starts after two reconciles of an unchanged over-cap spec = %d, want 1", ex.starts)
	}

	model := &inferencev1alpha1.Model{}
	if err := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: refusalModelName, Namespace: "default"}, model); err != nil {
		t.Fatal(err)
	}
	model.Spec.Source = "https://example.invalid/m-smaller.gguf"
	if err := a.config.K8sClient.Update(context.Background(), model); err != nil {
		t.Fatal(err)
	}
	ex.startErr = nil

	if err := a.ensureProcess(context.Background(), get()); err != nil {
		t.Fatalf("ensureProcess after changing the source: %v", err)
	}
	if ex.starts != 2 {
		t.Errorf("starts after changing the source = %d, want 2", ex.starts)
	}
}

// respawnFixtureProcess seeds a healthy process for default/svc whose spec hash
// no longer matches, so the next ensureProcess takes the spec-change respawn.
func respawnFixtureProcess(a *MetalAgent) {
	a.processes["default/svc"] = &ManagedProcess{
		Name: "svc", Namespace: "default", PID: 1111, Port: 18080,
		Healthy: true, SpecHash: "old-hash", Runtime: runtimeLlamaServer,
	}
}

// TestEnsureProcess_SpecChangeKeepsProcessWhenModelLookupFails is the respawn
// side of #2076: a spec change stops the healthy process only once the Model
// lookup has succeeded, so a transient lookup failure leaves the old process
// serving (nothing else retries until the InferenceService changes again),
// and the next reconcile respawns it as usual.
func TestEnsureProcess_SpecChangeKeepsProcessWhenModelLookupFails(t *testing.T) {
	failModelGet := false
	funcs := &interceptor.Funcs{
		Get: func(
			ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
		) error {
			if _, isModel := obj.(*inferencev1alpha1.Model); isModel && failModelGet {
				failModelGet = false
				return apierrors.NewServiceUnavailable("apiserver restarting")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	isvc := refusalISVC("svc")
	a, ex, _ := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, funcs, isvc, refusalModel())
	respawnFixtureProcess(a)

	failModelGet = true
	if err := a.ensureProcess(context.Background(), isvc); err == nil {
		t.Fatal("ensureProcess with the Model lookup failing returned nil, want the lookup error")
	}
	if len(ex.stopped) != 0 {
		t.Errorf("old process stopped (pids %v) on a failed Model lookup, want it left serving", ex.stopped)
	}
	if p := a.processes["default/svc"]; p == nil || p.PID != 1111 {
		t.Errorf("processes[default/svc] = %+v, want the old process (pid 1111) still tracked", p)
	}

	if err := a.ensureProcess(context.Background(), isvc); err != nil {
		t.Fatalf("ensureProcess after the lookup recovered: %v", err)
	}
	if len(ex.stopped) != 1 || ex.stopped[0] != 1111 || ex.starts != 1 {
		t.Errorf("after the lookup recovered stopped = %v, starts = %d; want pid 1111 stopped once and one start",
			ex.stopped, ex.starts)
	}
}

// TestEnsureProcess_SpecChangeToMemoizedMismatchRefusedWithoutDownload pins
// the order inside a spec-change respawn: the new Model reference is noted
// only after deleteProcess forgets the old one. Noting it first would let
// forgetISVC drop the new Model's digest-mismatch memo entry, and the respawn
// would download a file already known to be bad.
func TestEnsureProcess_SpecChangeToMemoizedMismatchRefusedWithoutDownload(t *testing.T) {
	bad := refusalModel()
	bad.Name = "bad"
	bad.Spec.SHA256 = "aaaa"
	isvc := refusalISVC("svc")
	isvc.Spec.ModelRef = bad.Name
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel(), bad)
	respawnFixtureProcess(a)
	a.digestMismatches.noteRef("default/svc", types.NamespacedName{Namespace: "default", Name: refusalModelName})
	a.digestMismatches.record(types.NamespacedName{Namespace: "default", Name: bad.Name}, bad.Spec.Source, "aaaa",
		&ModelDigestMismatchError{Path: "/models/bad/m.gguf", Expected: "aaaa", Computed: "bbbb"})

	err := a.ensureProcess(context.Background(), isvc)

	if err == nil || !strings.Contains(err.Error(), EventReasonModelDigestMismatch) {
		t.Fatalf("ensureProcess error = %v, want the memoized %s refusal", err, EventReasonModelDigestMismatch)
	}
	if ex.starts != 0 {
		t.Errorf("starts = %d, want 0 (the memo should refuse the bad Model without downloading it)", ex.starts)
	}
	if len(ex.stopped) != 1 {
		t.Errorf("stopped = %v, want the old process stopped for the spec change", ex.stopped)
	}
}

// TestMetalAgentRefusalReasons_CoversEveryAgentRefusalReason (a structural
// AST scan, not a hand-maintained table) lives in
// refusal_reasons_scan_test.go.
