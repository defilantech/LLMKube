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
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// memoryGuard returns the pod's multinode-memory-guard init container and its
// index, or -1.
func memoryGuard(p *corev1.Pod) (corev1.Container, int) {
	for i, c := range p.Spec.InitContainers {
		if c.Name == multiNodeMemoryGuardContainer {
			return c, i
		}
	}
	return corev1.Container{}, -1
}

func TestConstructMemberPodsMemoryGuard(t *testing.T) {
	isvc, model := multiNodeFixture()
	isvc.Spec.MultiNode.Memory = &inferencev1alpha1.MultiNodeMemory{
		DropPageCacheBeforeBoot: true,
		FlushBelowMemFree:       "4Gi",
		SuspendBelowMemFree:     "1536Mi",
	}
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService, InitContainerImage: "mirror.example/curl:1"}
	pods, _, err := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	for _, p := range pods {
		c, i := memoryGuard(p)
		if i < 0 {
			t.Fatalf("%s: no %s sidecar: %v", p.Name, multiNodeMemoryGuardContainer, p.Spec.InitContainers)
		}
		if _, prep := memoryPrep(p); prep < 0 || prep > i {
			t.Errorf("%s: guard at %d, prep at %d; the guard must start after the prep", p.Name, i, prep)
		}
		if i != len(p.Spec.InitContainers)-1 {
			t.Errorf("%s: guard is init container %d of %d; it must be last so it starts after staging", p.Name, i, len(p.Spec.InitContainers))
		}
		if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			t.Errorf("%s: guard must be a native sidecar (restartPolicy Always): %v", p.Name, c.RestartPolicy)
		}
		if !isSidecar(p, multiNodeMemoryGuardContainer) {
			t.Errorf("%s: isSidecar does not see the guard", p.Name)
		}
		if c.Image != "mirror.example/curl:1" {
			t.Errorf("%s: image %q, want the operator's init container image", p.Name, c.Image)
		}
		sc := c.SecurityContext
		if sc == nil || sc.Privileged == nil || !*sc.Privileged || sc.RunAsUser == nil || *sc.RunAsUser != 0 {
			t.Errorf("%s: guard must be privileged and root to write drop_caches: %+v", p.Name, sc)
		}
		if c.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
			t.Errorf("%s: termination message policy %q; the controller reads the guard's message from the file", p.Name, c.TerminationMessagePolicy)
		}
		// sh -c <script> <$0> <flush MiB> <suspend MiB>
		want := []string{"sh", "-c", multiNodeMemoryGuardScript, multiNodeMemoryGuardContainer, "4096", "1536"}
		if strings.Join(c.Command, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s: command args = %q, want flush 4096 and suspend 1536", p.Name, c.Command[3:])
		}
	}

	// Flush alone gives a guard that never suspends.
	isvc.Spec.MultiNode.Memory.SuspendBelowMemFree = ""
	pods, _, err = r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	if c, i := memoryGuard(pods[0]); i < 0 || c.Command[5] != "0" {
		t.Errorf("flush only: guard index %d, args %q; want suspend 0", i, c.Command)
	}

	// Neither threshold: no sidecar, so the boot-only block keeps its pods.
	isvc.Spec.MultiNode.Memory.FlushBelowMemFree = ""
	pods, _, err = r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	for _, p := range pods {
		if _, i := memoryGuard(p); i >= 0 {
			t.Errorf("%s: guard sidecar without a threshold", p.Name)
		}
	}
}

func TestValidateMultiNodeMemoryGuardFloors(t *testing.T) {
	for _, tc := range []struct {
		name         string
		flush, floor string
		wantFlush    int64
		wantFloor    int64
		wantErr      string
	}{
		{name: "unset"},
		{name: "both", flush: "4Gi", floor: "1536Mi", wantFlush: 4096, wantFloor: 1536},
		{name: "decimal units", flush: "4G", wantFlush: 3814},
		{name: "not a quantity", flush: "lots", wantErr: "flushBelowMemFree"},
		{name: "rounds to zero", floor: "512Ki", wantErr: "at least 1Mi"},
		{name: "zero", floor: "0", wantErr: "at least 1Mi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := &inferencev1alpha1.MultiNodeMemory{FlushBelowMemFree: tc.flush, SuspendBelowMemFree: tc.floor}
			flush, floor, err := multiNodeMemoryGuardFloors(mem)
			verr := validateMultiNodeMemory(mem)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
				}
				if verr == nil {
					t.Fatalf("validateMultiNodeMemory accepted %+v", mem)
				}
				return
			}
			if err != nil || verr != nil {
				t.Fatalf("unexpected errors: %v / %v", err, verr)
			}
			if flush != tc.wantFlush || floor != tc.wantFloor {
				t.Errorf("floors = %d, %d MiB, want %d, %d", flush, floor, tc.wantFlush, tc.wantFloor)
			}
		})
	}
}

// A running guard sidecar must not make the member look like it is still
// staging, and its restarts are not download restarts.
func TestMemberStagingIgnoresGuardSidecar(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	p := &corev1.Pod{
		Spec: corev1.PodSpec{InitContainers: []corev1.Container{
			{Name: "model-downloader"},
			{Name: multiNodeMemoryGuardContainer, RestartPolicy: &always},
		}},
		Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
			{Name: "model-downloader", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: multiNodeMemoryGuardContainer, RestartCount: 7, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}},
	}
	if memberStaging(p) {
		t.Errorf("memberStaging = true with only the guard running")
	}
	if !memberStaged(p) {
		t.Errorf("memberStaged = false with the downloader done and the guard running")
	}
	if n := initRestartCount(p); n != 0 {
		t.Errorf("initRestartCount = %d, want 0: guard restarts are not staging restarts", n)
	}

	// The skip is for sidecars only: a running ordinary init container is
	// still staging.
	p.Spec.InitContainers[1].RestartPolicy = nil
	if !memberStaging(p) {
		t.Errorf("memberStaging = false with an ordinary init container running")
	}
}

func TestMemoryGuardTripped(t *testing.T) {
	formed := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
	const hash = "h1"
	guardExit := func(code int32, at time.Time, last bool) *corev1.Pod {
		term := &corev1.ContainerStateTerminated{ExitCode: code, FinishedAt: metav1.NewTime(at), Message: "MemFree 900 MiB stayed under 1536 MiB for 10s after a page cache drop\n"}
		cs := corev1.ContainerStatus{Name: multiNodeMemoryGuardContainer}
		if last {
			cs.LastTerminationState.Terminated = term
			cs.State.Running = &corev1.ContainerStateRunning{}
		} else {
			cs.State.Terminated = term
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "ring-mn-1", Annotations: map[string]string{AnnotationMultiNodeGroupHash: hash}},
			Status:     corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{cs}},
		}
	}
	obs := func(p *corev1.Pod) []memberObservation {
		head := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationMultiNodeGroupHash: hash}}}
		return []memberObservation{{rank: 0, existing: head}, {rank: 1, existing: p}}
	}

	p, msg := memoryGuardTripped(obs(guardExit(42, formed.Add(time.Minute), true)), hash, formed)
	if p == nil || p.Name != "ring-mn-1" {
		t.Fatalf("restarted guard after formation: trigger = %v", p)
	}
	if msg != "MemFree 900 MiB stayed under 1536 MiB for 10s after a page cache drop" {
		t.Errorf("message = %q", msg)
	}
	if p, _ := memoryGuardTripped(obs(guardExit(42, formed.Add(time.Minute), false)), hash, formed); p == nil {
		t.Errorf("guard terminated (not yet restarted) after formation was not seen")
	}
	if p, _ := memoryGuardTripped(obs(guardExit(42, formed.Add(-time.Minute), true)), hash, formed); p != nil {
		t.Errorf("an exit from before formation tripped the guard")
	}
	if p, _ := memoryGuardTripped(obs(guardExit(42, formed.Add(time.Minute), true)), hash, time.Time{}); p != nil {
		t.Errorf("an unformed group tripped the guard")
	}
	if p, _ := memoryGuardTripped(obs(guardExit(1, formed.Add(time.Minute), true)), hash, formed); p != nil {
		t.Errorf("an exit other than %d tripped the guard", multiNodeMemoryGuardExitCode)
	}

	// A member on its way out does not speak for the group: one that is
	// terminating, and one left over from another spec.
	gone := guardExit(42, formed.Add(time.Minute), true)
	now := metav1.NewTime(formed.Add(2 * time.Minute))
	gone.DeletionTimestamp = &now
	if p, _ := memoryGuardTripped(obs(gone), hash, formed); p != nil {
		t.Errorf("a terminating member tripped the guard")
	}
	if p, _ := memoryGuardTripped(obs(guardExit(42, formed.Add(time.Minute), true)), "h2", formed); p != nil {
		t.Errorf("a member from another group hash tripped the guard")
	}
}

// A guard that trips on rank 0 of a starved node usually finds the runtime
// starved too: the head is not Ready and its runtime container is not ready
// either. On a group that has served longer than the startup budget
// groupNeedsRecreate answers HeadNotReady for that same observation, so the
// reconcile has to ask memoryGuardTripped first. This pins both answers, and
// the envtest spec "suspends, not recreates, when the guard trips on rank 0"
// pins the order.
func TestMemoryGuardTripOnHeadAlsoReadsAsHeadNotReady(t *testing.T) {
	const hash = "h1"
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	formed := now.Add(-26 * time.Hour)
	always := corev1.ContainerRestartPolicyAlways
	head := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ring-mn-0", Annotations: map[string]string{AnnotationMultiNodeGroupHash: hash}},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: multiNodeMemoryGuardContainer, RestartPolicy: &always}},
			Containers:     []corev1.Container{{Name: "vllm"}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: multiNodeMemoryGuardContainer, RestartCount: 1,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: multiNodeMemoryGuardExitCode, FinishedAt: metav1.NewTime(now.Add(-5 * time.Second)),
					Message: "MemFree 6416 MiB stayed under 8192 MiB for 10s after a page cache drop",
				}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "vllm", Ready: false,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(formed)}},
			}},
		},
	}
	obs := []memberObservation{{rank: 0, desired: head, existing: head}}

	if reason, _, _ := groupNeedsRecreate(obs, hash, now, formed, formed); reason != multiNodeReasonHeadNotReady {
		t.Fatalf("groupNeedsRecreate reason = %q, want %q: this is the case the order protects against", reason, multiNodeReasonHeadNotReady)
	}
	p, msg := memoryGuardTripped(obs, hash, formed)
	if p == nil || p.Name != "ring-mn-0" {
		t.Fatalf("the trip on rank 0 was not seen: %v", p)
	}
	if msg != "MemFree 6416 MiB stayed under 8192 MiB for 10s after a page cache drop" {
		t.Errorf("message = %q", msg)
	}
}

// guardRun is one run of the guard script against a fake meminfo.
type guardRun struct {
	memFreeMiB, cachedMiB int
	shmemMiB              int  // Shmem line; < 0 leaves the line out
	noMemFree             bool // leave the MemFree line out
	flush, floor          string
	gapS                  string // GUARD_DROP_GAP_S; "" is 0
	// afterDropMemFreeMiB > 0 makes the drop help: a fake sync, which the
	// script runs right before it writes drop_caches, rewrites the meminfo
	// to this MemFree with a small Cached, as a real drop would.
	afterDropMemFreeMiB int
	shell               []string      // default {"sh"}
	timeout             time.Duration // the run is killed after this
	termAfter           time.Duration // > 0: send SIGTERM after this instead
}

func guardMeminfo(memFreeMiB, cachedMiB, shmemMiB int, noMemFree bool) string {
	content := "MemTotal:       127498412 kB\n"
	if !noMemFree {
		content += "MemFree:        " + strconv.Itoa(memFreeMiB*1024) + " kB\n"
	}
	content += "MemAvailable:   " + strconv.Itoa((memFreeMiB+cachedMiB)*1024) + " kB\n" +
		"Buffers:           10240 kB\n" +
		"Cached:         " + strconv.Itoa(cachedMiB*1024) + " kB\n" +
		"SwapCached:            0 kB\n"
	if shmemMiB >= 0 {
		content += "Shmem:          " + strconv.Itoa(shmemMiB*1024) + " kB\n"
	}
	return content
}

// runGuard runs the guard script against a fake meminfo and returns its
// output, exit code (-1 when killed by the timeout) and the files it wrote.
func runGuard(t *testing.T, g guardRun) (out string, code int, dropped, term string) {
	t.Helper()
	shell := g.shell
	if len(shell) == 0 {
		shell = []string{"sh"}
	}
	bin, err := exec.LookPath(shell[0])
	if err != nil {
		t.Skipf("no %s on PATH", shell[0])
	}
	dir := t.TempDir()
	meminfo := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(meminfo, []byte(guardMeminfo(g.memFreeMiB, g.cachedMiB, g.shmemMiB, g.noMemFree)), 0o644); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	if g.afterDropMemFreeMiB > 0 {
		after := guardMeminfo(g.afterDropMemFreeMiB, 300, g.shmemMiB, false)
		if err := os.WriteFile(meminfo+".after", []byte(after), 0o644); err != nil {
			t.Fatal(err)
		}
		fakeBin := filepath.Join(dir, "bin")
		if err := os.Mkdir(fakeBin, 0o755); err != nil {
			t.Fatal(err)
		}
		fakeSync := "#!/bin/sh\ncat \"$GUARD_MEMINFO.after\" > \"$GUARD_MEMINFO\"\n"
		if err := os.WriteFile(filepath.Join(fakeBin, "sync"), []byte(fakeSync), 0o755); err != nil {
			t.Fatal(err)
		}
		path = fakeBin + string(os.PathListSeparator) + path
	}
	gap := g.gapS
	if gap == "" {
		gap = "0"
	}
	dropf := filepath.Join(dir, "drop_caches")
	termf := filepath.Join(dir, "termination-log")
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()
	args := append(append([]string{}, shell[1:]...), "-c", multiNodeMemoryGuardScript, multiNodeMemoryGuardContainer, g.flush, g.floor)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"GUARD_MEMINFO="+meminfo, "GUARD_DROP_CACHES="+dropf, "GUARD_TERMINATION_LOG="+termf,
		"GUARD_POLL_S=1", "GUARD_HOLD_S=1", "GUARD_DROP_GAP_S="+gap)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if g.termAfter > 0 {
		time.Sleep(g.termAfter)
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("signal: %v", err)
		}
	}
	err = cmd.Wait()
	code = 0
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil && !errors.Is(err, exec.ErrWaitDelay):
		t.Fatalf("run: %v", err)
	}
	d, _ := os.ReadFile(dropf)
	m, _ := os.ReadFile(termf)
	return buf.String(), code, strings.TrimSpace(string(d)), strings.TrimSpace(string(m))
}

// guardScriptHealthy fails the test when the script did not get as far as its
// loop or tripped over its own syntax. Without it a test that only asserts
// "nothing was dropped" would also pass for a script that died on line one.
func guardScriptHealthy(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "start: MemFree ") {
		t.Errorf("the script never printed its start line:\n%s", out)
	}
	for _, bad := range []string{"syntax error", "arithmetic", "not found", "unbound", "parameter not set", "integer expression", "bad number"} {
		if strings.Contains(out, bad) {
			t.Errorf("the script reported %q:\n%s", bad, out)
		}
	}
}

func runGuardScript(t *testing.T, memFreeMiB, cachedMiB int, flush, floor string, timeout time.Duration) (out string, code int, dropped, term string) {
	t.Helper()
	return runGuardScriptShmem(t, memFreeMiB, cachedMiB, -1, flush, floor, timeout)
}

// runGuardScriptShmem is runGuardScript with a Shmem line in the fake
// meminfo; shmemMiB < 0 leaves the line out, as on a kernel that has none.
func runGuardScriptShmem(t *testing.T, memFreeMiB, cachedMiB, shmemMiB int, flush, floor string, timeout time.Duration) (out string, code int, dropped, term string) {
	t.Helper()
	out, code, dropped, term = runGuard(t, guardRun{memFreeMiB: memFreeMiB, cachedMiB: cachedMiB, shmemMiB: shmemMiB, flush: flush, floor: floor, timeout: timeout})
	guardScriptHealthy(t, out)
	return out, code, dropped, term
}

func TestMultiNodeMemoryGuardScriptFlushes(t *testing.T) {
	// MemFree 2 GiB under a 4 GiB flush floor with 8 GiB of cache, no suspend
	// floor: the loop drops and keeps running until killed.
	out, code, dropped, term := runGuardScript(t, 2048, 8192, "4096", "0", 2500*time.Millisecond)
	if dropped != "3" {
		t.Fatalf("drop_caches = %q, want 3; output:\n%s", dropped, out)
	}
	if !strings.Contains(out, "drop (MemFree < 4096 MiB): MemFree 2048 -> 2048 MiB, Cached 8192 -> 8192 MiB") {
		t.Errorf("no audit line for the drop:\n%s", out)
	}
	if code == multiNodeMemoryGuardExitCode || term != "" {
		t.Errorf("a guard with no suspend floor exited %d with message %q", code, term)
	}
}

func TestMultiNodeMemoryGuardScriptLeavesSmallCache(t *testing.T) {
	// Under the flush floor but with only 256 MiB of cache: nothing to gain,
	// so no drop.
	out, _, dropped, _ := runGuardScript(t, 2048, 256, "4096", "0", 1500*time.Millisecond)
	if dropped != "" {
		t.Errorf("dropped with 256 MiB of cache:\n%s", out)
	}
}

// Cached counts tmpfs and shared memory, which drop_caches cannot free. A
// node whose Cached is nearly all Shmem has nothing to drop, so the flusher
// must leave it alone instead of syncing and dropping every few seconds.
func TestMultiNodeMemoryGuardScriptDoesNotCountShmem(t *testing.T) {
	// 8 GiB Cached of which 7.8 GiB is Shmem: 200 MiB of real page cache.
	out, _, dropped, _ := runGuardScriptShmem(t, 2048, 8192, 7992, "4096", "0", 2500*time.Millisecond)
	if dropped != "" {
		t.Errorf("dropped with 200 MiB of page cache behind 7.8 GiB of Shmem:\n%s", out)
	}
	// The same node with 2 GiB of real page cache beside the Shmem drops.
	out, _, dropped, _ = runGuardScriptShmem(t, 2048, 8192, 6144, "4096", "0", 2500*time.Millisecond)
	if dropped != "3" {
		t.Errorf("no drop with 2 GiB of page cache beside 6 GiB of Shmem:\n%s", out)
	}
	if strings.Contains(out, "syntax error") || strings.Contains(out, "arithmetic") {
		t.Errorf("the script failed on the Shmem arithmetic:\n%s", out)
	}
}

// A meminfo without a Shmem line must not break the arithmetic: the missing
// field reads as 0 and the loop behaves as before.
func TestMultiNodeMemoryGuardScriptWithoutShmemLine(t *testing.T) {
	out, code, dropped, _ := runGuardScript(t, 2048, 8192, "4096", "0", 2500*time.Millisecond)
	if dropped != "3" {
		t.Errorf("no drop when meminfo has no Shmem line:\n%s", out)
	}
	if code != -1 && code != 0 {
		t.Errorf("exit %d (want killed by the timeout); output:\n%s", code, out)
	}
	if strings.Contains(out, "syntax error") || strings.Contains(out, "arithmetic") {
		t.Errorf("the script failed without a Shmem line:\n%s", out)
	}
}

// The suspend path's last drop still runs when the shortfall is Shmem, and
// the guard then exits: memory a drop cannot give back is exactly the case
// the suspend floor is for.
func TestMultiNodeMemoryGuardScriptSuspendsWhenCacheIsShmem(t *testing.T) {
	out, code, dropped, term := runGuardScriptShmem(t, 6416, 16688, 16600, "4096", "8192", 10*time.Second)
	if code != multiNodeMemoryGuardExitCode {
		t.Fatalf("exit %d, want %d; output:\n%s", code, multiNodeMemoryGuardExitCode, out)
	}
	if dropped != "3" || !strings.Contains(out, "guard: last drop: MemFree 6416 -> 6416 MiB") {
		t.Errorf("no last drop before the suspend when the cache is Shmem; output:\n%s", out)
	}
	if term != "MemFree 6416 MiB stayed under 8192 MiB for 1s after a page cache drop" {
		t.Errorf("termination message = %q", term)
	}
}

func TestMultiNodeMemoryGuardScriptSuspends(t *testing.T) {
	// MemFree 900 MiB stays under a 1536 MiB floor (the fake meminfo never
	// changes, so the last drop does not help): exit 42 with the reason in
	// the termination message.
	out, code, dropped, term := runGuardScript(t, 900, 4096, "0", "1536", 10*time.Second)
	if code != multiNodeMemoryGuardExitCode {
		t.Fatalf("exit %d, want %d; output:\n%s", code, multiNodeMemoryGuardExitCode, out)
	}
	if dropped != "3" {
		t.Errorf("no last drop before the suspend; output:\n%s", out)
	}
	if term != "MemFree 900 MiB stayed under 1536 MiB for 1s after a page cache drop" {
		t.Errorf("termination message = %q", term)
	}
}

func TestMultiNodeMemoryGuardScriptAboveFloorStays(t *testing.T) {
	out, code, dropped, term := runGuardScript(t, 10240, 4096, "4096", "1536", 2500*time.Millisecond)
	if code == multiNodeMemoryGuardExitCode || dropped != "" || term != "" {
		t.Errorf("MemFree 10 GiB: exit %d, drop %q, message %q; output:\n%s", code, dropped, term, out)
	}
}

// guardHead is a rank 0 that has served for a day and is not Ready because
// its guard sidecar is between an OOM kill (exit 137) and its restart.
func guardHead(now time.Time, runtimeReady bool, guardExitedAgo time.Duration) *corev1.Pod {
	always := corev1.ContainerRestartPolicyAlways
	started := metav1.NewTime(now.Add(-26 * time.Hour))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ring-mn-0", Annotations: map[string]string{AnnotationMultiNodeGroupHash: "h1"}},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: multiNodeMemoryGuardContainer, RestartPolicy: &always}},
			Containers:     []corev1.Container{{Name: "vllm"}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: multiNodeMemoryGuardContainer, RestartCount: 1,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.NewTime(now.Add(-guardExitedAgo)),
				}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "vllm", Ready: runtimeReady,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
			}},
		},
	}
}

// A guard that restarts for any reason other than a trip (an OOM kill at its
// small limit) leaves a serving head not Ready for a few seconds. That must
// not recreate a group that has served past the startup budget; a guard that
// stays down for the whole budget still does; and a head whose runtime is
// itself not ready keeps the ordinary deadline.
func TestGroupNeedsRecreateGuardRestartOnHead(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	formed := now.Add(-26 * time.Hour)
	need := func(p *corev1.Pod) string {
		reason, _, _ := groupNeedsRecreate([]memberObservation{{rank: 0, desired: p, existing: p}}, "h1", now, formed, formed)
		return reason
	}
	if got := need(guardHead(now, true, 5*time.Second)); got != "" {
		t.Errorf("a guard that exited 5 s ago on a serving head recreated the group: %q", got)
	}
	if got := need(guardHead(now, true, multiNodeStartupBudget+time.Minute)); got != multiNodeReasonHeadNotReady {
		t.Errorf("a guard down for longer than the budget: reason %q, want %q", got, multiNodeReasonHeadNotReady)
	}
	if got := need(guardHead(now, false, 5*time.Second)); got != multiNodeReasonHeadNotReady {
		t.Errorf("a head whose runtime is not ready: reason %q, want %q", got, multiNodeReasonHeadNotReady)
	}
	// A guard that is running again is not "down": the ordinary deadline
	// applies to whatever else keeps the head not Ready.
	running := guardHead(now, true, 5*time.Second)
	running.Status.InitContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now)}}
	if _, ok := guardDownSince(running); ok {
		t.Errorf("guardDownSince reported a running guard as down")
	}
	// A non-42 exit is not a trip.
	if p, _ := memoryGuardTripped([]memberObservation{{rank: 0, existing: guardHead(now, true, 5*time.Second)}}, "h1", formed); p != nil {
		t.Errorf("an OOM-killed guard tripped the suspend")
	}
}

// A group that sets neither floor is untouched by this feature: same pods,
// same group hash, so an operator upgrade does not recreate it. Setting a
// floor changes the hash; writing the same floor another way does not.
func TestConstructMemberPodsMemoryGuardUnsetIsInert(t *testing.T) {
	isvc, model := multiNodeFixture()
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	build := func(mem *inferencev1alpha1.MultiNodeMemory) ([]*corev1.Pod, string) {
		t.Helper()
		isvc.Spec.MultiNode.Memory = mem
		pods, hash, err := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
		if err != nil {
			t.Fatalf("constructMemberPods(%+v): %v", mem, err)
		}
		return pods, hash
	}
	basePods, baseHash := build(nil)
	emptyPods, emptyHash := build(&inferencev1alpha1.MultiNodeMemory{})
	if emptyHash != baseHash {
		t.Errorf("an empty memory block changed the group hash: %s vs %s", emptyHash, baseHash)
	}
	for i, p := range emptyPods {
		if _, at := memoryGuard(p); at >= 0 {
			t.Errorf("%s: a guard sidecar with neither floor set", p.Name)
		}
		if len(p.Spec.InitContainers) != len(basePods[i].Spec.InitContainers) {
			t.Errorf("%s: %d init containers, want %d", p.Name, len(p.Spec.InitContainers), len(basePods[i].Spec.InitContainers))
		}
	}
	flushPods, flushHash := build(&inferencev1alpha1.MultiNodeMemory{FlushBelowMemFree: "4Gi"})
	if flushHash == baseHash {
		t.Errorf("setting flushBelowMemFree must change the group hash so the group is recreated with the guard")
	}
	if _, at := memoryGuard(flushPods[0]); at < 0 {
		t.Errorf("flushBelowMemFree alone added no guard")
	}
	if _, sameHash := build(&inferencev1alpha1.MultiNodeMemory{FlushBelowMemFree: "4096Mi"}); sameHash != flushHash {
		t.Errorf("4Gi and 4096Mi are the same floor and must give the same hash: %s vs %s", sameHash, flushHash)
	}
	if _, otherHash := build(&inferencev1alpha1.MultiNodeMemory{FlushBelowMemFree: "4Gi", SuspendBelowMemFree: "8Gi"}); otherHash == flushHash {
		t.Errorf("adding suspendBelowMemFree must change the group hash")
	}
}

// The suspend is written with the optimistic lock: a reconcile holding a copy
// from before another write gets a conflict and requeues, with no second
// event and no second count. A fresh copy suspends, counts once and says why.
func TestSuspendForMemoryGuardConflictAndCount(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	isvc, _ := multiNodeFixture()
	isvc.Name = "guard-conflict"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc).Build()
	recorder := events.NewFakeRecorder(4)
	r := &InferenceServiceReconciler{Client: c, Scheme: scheme, Recorder: recorder}
	ctx := context.Background()
	key := client.ObjectKeyFromObject(isvc)
	counter := llmkubemetrics.MultiNodeGuardSuspendsTotal.WithLabelValues(isvc.Name, isvc.Namespace)
	before := testutil.ToFloat64(counter)
	member := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "guard-conflict-mn-0"}, Spec: corev1.PodSpec{NodeName: "spark-0"}}

	var stale, fresh inferencev1alpha1.InferenceService
	if err := c.Get(ctx, key, &stale); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Annotations = map[string]string{"touched": "by-someone-else"}
	if err := c.Update(ctx, &fresh); err != nil {
		t.Fatal(err)
	}

	_, res, err := r.suspendForMemoryGuard(ctx, &stale, member, "MemFree 900 MiB stayed under 8192 MiB for 10s after a page cache drop")
	if err != nil || res == nil || res.RequeueAfter <= 0 {
		t.Fatalf("stale copy: result %v, err %v; want a quiet requeue", res, err)
	}
	var got inferencev1alpha1.InferenceService
	if err := c.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Suspend {
		t.Errorf("a stale copy suspended the service")
	}
	if n := testutil.ToFloat64(counter); n != before {
		t.Errorf("a conflicting suspend was counted: %v -> %v", before, n)
	}
	select {
	case e := <-recorder.Events:
		t.Errorf("a conflicting suspend raised an event: %s", e)
	default:
	}

	if _, _, err := r.suspendForMemoryGuard(ctx, &got, member, "MemFree 900 MiB stayed under 8192 MiB for 10s after a page cache drop"); err != nil {
		t.Fatalf("fresh copy: %v", err)
	}
	var done inferencev1alpha1.InferenceService
	if err := c.Get(ctx, key, &done); err != nil {
		t.Fatal(err)
	}
	if !done.Spec.Suspend {
		t.Errorf("a fresh copy did not suspend the service")
	}
	if n := testutil.ToFloat64(counter); n != before+1 {
		t.Errorf("suspends counter %v -> %v, want one more", before, n)
	}
	select {
	case e := <-recorder.Events:
		for _, want := range []string{"MultiNodeMemoryGuardSuspended", "guard-conflict-mn-0", "spark-0", "MemFree 900 MiB"} {
			if !strings.Contains(e, want) {
				t.Errorf("event %q lacks %q", e, want)
			}
		}
	default:
		t.Errorf("no event for the suspend")
	}
}

// The 5 s gap holds between two flush drops: with a long gap a node that
// stays under the flush floor is dropped once, not on every pass.
func TestMultiNodeMemoryGuardScriptRespectsDropGap(t *testing.T) {
	out, _, dropped, _ := runGuard(t, guardRun{memFreeMiB: 2048, cachedMiB: 8192, shmemMiB: 100, flush: "4096", floor: "0", gapS: "30", timeout: 3500 * time.Millisecond})
	guardScriptHealthy(t, out)
	if dropped != "3" {
		t.Fatalf("no drop at all:\n%s", out)
	}
	if n := strings.Count(out, "drop (MemFree < 4096 MiB)"); n != 1 {
		t.Errorf("%d drops inside a 30 s gap, want 1:\n%s", n, out)
	}
	out, _, _, _ = runGuard(t, guardRun{memFreeMiB: 2048, cachedMiB: 8192, shmemMiB: 100, flush: "4096", floor: "0", gapS: "0", timeout: 3500 * time.Millisecond})
	if n := strings.Count(out, "drop (MemFree < 4096 MiB)"); n < 2 {
		t.Errorf("%d drops with no gap, want one per pass:\n%s", n, out)
	}
}

// The case the docs lean on: MemFree is under the suspend floor because of
// page cache (a weight load), the guard's own drop brings it back, and the
// group is not suspended. The loop then goes on watching.
func TestMultiNodeMemoryGuardScriptDropThatHelpsDoesNotSuspend(t *testing.T) {
	out, code, dropped, term := runGuard(t, guardRun{memFreeMiB: 7000, cachedMiB: 35000, shmemMiB: 5000, flush: "0", floor: "8192", afterDropMemFreeMiB: 32000, timeout: 4500 * time.Millisecond})
	guardScriptHealthy(t, out)
	if dropped != "3" {
		t.Fatalf("no last drop:\n%s", out)
	}
	if !strings.Contains(out, "guard: last drop: MemFree 7000 -> 32000 MiB") {
		t.Errorf("no audit line showing the drop helped:\n%s", out)
	}
	if code == multiNodeMemoryGuardExitCode || term != "" || strings.Contains(out, "GUARD:") {
		t.Errorf("a drop that brought MemFree back still suspended: exit %d, message %q\n%s", code, term, out)
	}
	if n := strings.Count(out, "guard: last drop"); n != 1 {
		t.Errorf("%d last drops, want 1 (MemFree stayed above the floor afterwards):\n%s", n, out)
	}
}

// Under both floors at once (flush below suspend, as in the ring sample) one
// pass must not sync and drop twice: the flush branch drops, and the guard
// decides on that drop instead of adding its own.
func TestMultiNodeMemoryGuardScriptOneDropWhenUnderBothFloors(t *testing.T) {
	out, code, _, term := runGuard(t, guardRun{memFreeMiB: 2000, cachedMiB: 8192, shmemMiB: 100, flush: "4096", floor: "8192", gapS: "30", timeout: 10 * time.Second})
	guardScriptHealthy(t, out)
	if code != multiNodeMemoryGuardExitCode {
		t.Fatalf("exit %d, want %d:\n%s", code, multiNodeMemoryGuardExitCode, out)
	}
	if n := strings.Count(out, "MemFree 2000 -> 2000 MiB"); n != 1 {
		t.Errorf("%d drops before the suspend, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "guard: last drop") {
		t.Errorf("the guard added its own drop right after the flush drop:\n%s", out)
	}
	if term != "MemFree 2000 MiB stayed under 8192 MiB for 1s after a page cache drop" {
		t.Errorf("termination message = %q", term)
	}
}

// A meminfo with no MemFree line must not read as 0 MiB free and suspend the
// group: the guard says so once and does nothing.
func TestMultiNodeMemoryGuardScriptMissingMemFreeDoesNothing(t *testing.T) {
	out, code, dropped, term := runGuard(t, guardRun{noMemFree: true, cachedMiB: 8192, shmemMiB: 100, flush: "4096", floor: "8192", timeout: 3500 * time.Millisecond})
	if code == multiNodeMemoryGuardExitCode || term != "" {
		t.Fatalf("a missing MemFree suspended the group: exit %d, message %q\n%s", code, term, out)
	}
	if dropped != "" {
		t.Errorf("a missing MemFree dropped the cache:\n%s", out)
	}
	if n := strings.Count(out, "cannot read MemFree"); n != 1 {
		t.Errorf("the warning was printed %d times, want once:\n%s", n, out)
	}
}

// sh is PID 1 in the member pod and ignores a signal it has no handler for.
// The trap makes the loop leave at once on SIGTERM, so deleting a member does
// not wait out the grace period for the guard.
func TestMultiNodeMemoryGuardScriptStopsOnSIGTERM(t *testing.T) {
	start := time.Now()
	out, code, _, term := runGuard(t, guardRun{memFreeMiB: 20000, cachedMiB: 4096, shmemMiB: 100, flush: "4096", floor: "8192", timeout: 20 * time.Second, termAfter: 1500 * time.Millisecond})
	guardScriptHealthy(t, out)
	if code != 0 {
		t.Errorf("exit %d on SIGTERM, want 0:\n%s", code, out)
	}
	if took := time.Since(start); took > 6*time.Second {
		t.Errorf("the loop took %s to leave on SIGTERM", took)
	}
	if term != "" {
		t.Errorf("a SIGTERM wrote a termination message: %q", term)
	}
}

// The member pods run the script under busybox ash (the default init image is
// curlimages/curl), while a developer's sh is dash or bash. Where busybox is
// installed, the same script must behave the same under its sh.
func TestMultiNodeMemoryGuardScriptUnderBusybox(t *testing.T) {
	ash := []string{"busybox", "sh"}
	out, code, _, term := runGuard(t, guardRun{memFreeMiB: 900, cachedMiB: 4096, shmemMiB: 4000, flush: "4096", floor: "1536", shell: ash, timeout: 10 * time.Second})
	guardScriptHealthy(t, out)
	if code != multiNodeMemoryGuardExitCode || term != "MemFree 900 MiB stayed under 1536 MiB for 1s after a page cache drop" {
		t.Errorf("suspend under busybox: exit %d, message %q\n%s", code, term, out)
	}
	out, _, dropped, _ := runGuard(t, guardRun{memFreeMiB: 2048, cachedMiB: 8192, shmemMiB: 7992, flush: "4096", floor: "0", shell: ash, timeout: 2500 * time.Millisecond})
	guardScriptHealthy(t, out)
	if dropped != "" {
		t.Errorf("busybox dropped with 200 MiB of page cache behind Shmem:\n%s", out)
	}
	out, code, _, _ = runGuard(t, guardRun{memFreeMiB: 20000, cachedMiB: 4096, shmemMiB: 100, flush: "4096", floor: "8192", shell: ash, timeout: 20 * time.Second, termAfter: 1500 * time.Millisecond})
	guardScriptHealthy(t, out)
	if code != 0 {
		t.Errorf("busybox: exit %d on SIGTERM, want 0:\n%s", code, out)
	}
}
