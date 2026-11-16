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
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
			ObjectMeta: metav1.ObjectMeta{Name: "ring-mn-1"},
			Status:     corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{cs}},
		}
	}
	obs := func(p *corev1.Pod) []memberObservation {
		return []memberObservation{{rank: 0, existing: &corev1.Pod{}}, {rank: 1, existing: p}}
	}

	p, msg := memoryGuardTripped(obs(guardExit(42, formed.Add(time.Minute), true)), formed)
	if p == nil || p.Name != "ring-mn-1" {
		t.Fatalf("restarted guard after formation: trigger = %v", p)
	}
	if msg != "MemFree 900 MiB stayed under 1536 MiB for 10s after a page cache drop" {
		t.Errorf("message = %q", msg)
	}
	if p, _ := memoryGuardTripped(obs(guardExit(42, formed.Add(time.Minute), false)), formed); p == nil {
		t.Errorf("guard terminated (not yet restarted) after formation was not seen")
	}
	if p, _ := memoryGuardTripped(obs(guardExit(42, formed.Add(-time.Minute), true)), formed); p != nil {
		t.Errorf("an exit from before formation tripped the guard")
	}
	if p, _ := memoryGuardTripped(obs(guardExit(42, formed.Add(time.Minute), true)), time.Time{}); p != nil {
		t.Errorf("an unformed group tripped the guard")
	}
	if p, _ := memoryGuardTripped(obs(guardExit(1, formed.Add(time.Minute), true)), formed); p != nil {
		t.Errorf("an exit other than %d tripped the guard", multiNodeMemoryGuardExitCode)
	}
}

// runGuardScript runs the guard script under sh against a fake meminfo and
// returns its output, exit code and the files it wrote.
func runGuardScript(t *testing.T, memFreeMiB, cachedMiB int, flush, floor string, timeout time.Duration) (out string, code int, dropped, term string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	dir := t.TempDir()
	meminfo := filepath.Join(dir, "meminfo")
	content := "MemTotal:       127498412 kB\n" +
		"MemFree:        " + strconv.Itoa(memFreeMiB*1024) + " kB\n" +
		"MemAvailable:   " + strconv.Itoa((memFreeMiB+cachedMiB)*1024) + " kB\n" +
		"Cached:         " + strconv.Itoa(cachedMiB*1024) + " kB\n"
	if err := os.WriteFile(meminfo, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	dropf := filepath.Join(dir, "drop_caches")
	termf := filepath.Join(dir, "termination-log")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh, "-c", multiNodeMemoryGuardScript, multiNodeMemoryGuardContainer, flush, floor)
	cmd.Env = append(os.Environ(),
		"GUARD_MEMINFO="+meminfo, "GUARD_DROP_CACHES="+dropf, "GUARD_TERMINATION_LOG="+termf,
		"GUARD_POLL_S=1", "GUARD_HOLD_S=1", "GUARD_DROP_GAP_S=0")
	b, err := cmd.CombinedOutput()
	code = 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	d, _ := os.ReadFile(dropf)
	m, _ := os.ReadFile(termf)
	return string(b), code, strings.TrimSpace(string(d)), strings.TrimSpace(string(m))
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
