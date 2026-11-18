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
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// hailoISVC is an ollama InferenceService over the refusal fixtures' Model.
func hailoISVC(name string) *inferencev1alpha1.InferenceService {
	isvc := refusalISVC(name)
	isvc.Spec.Runtime = runtimeOllama
	return isvc
}

// hailoDeviceFixture is a refusal fixture with a hailo:// Model, ollama
// InferenceServices svc-a and svc-b over it, and the recording executor
// behind the ollama runtime.
func hailoDeviceFixture(t *testing.T) (*MetalAgent, *recordingExecutor, *record.FakeRecorder) {
	t.Helper()
	model := refusalModel()
	model.Spec.Source = "hailo://qwen2.5-instruct:1.5b"
	model.Spec.Format = ""
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, hailoISVC("svc-a"), hailoISVC("svc-b"), model)
	a.executors[runtimeOllama] = ex
	return a, ex, rec
}

// getISVC reads InferenceService name back from the fixture's client.
func getISVC(t *testing.T, a *MetalAgent, name string) *inferencev1alpha1.InferenceService {
	t.Helper()
	got := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: name, Namespace: "default"}, got); err != nil {
		t.Fatalf("get InferenceService %s: %v", name, err)
	}
	return got
}

// mustStart runs ensureProcess for name and fails unless it served.
func mustStart(t *testing.T, a *MetalAgent, name string) {
	t.Helper()
	if err := a.ensureProcess(context.Background(), getISVC(t, a, name)); err != nil {
		t.Fatalf("ensureProcess(%s): %v", name, err)
	}
	a.mu.RLock()
	_, ok := a.processes["default/"+name]
	a.mu.RUnlock()
	if !ok {
		t.Fatalf("%s: no process stored", name)
	}
}

// wantHailoRefusal checks that ensureProcess err, the status of svc-b and the
// recorded Events refuse svc-b with HailoDeviceInUse naming svc-a.
func wantHailoRefusal(t *testing.T, a *MetalAgent, rec *record.FakeRecorder, err error) {
	t.Helper()
	const name, holder = "svc-b", "default/svc-a"
	if err == nil || !strings.Contains(err.Error(), EventReasonHailoDeviceInUse) {
		t.Fatalf("ensureProcess(%s) error = %v, want %s", name, err, EventReasonHailoDeviceInUse)
	}
	got := getISVC(t, a, name)
	if got.Status.SchedulingStatus != EventReasonHailoDeviceInUse ||
		!strings.Contains(got.Status.SchedulingMessage, holder) {
		t.Errorf("status = %q / %q, want %s naming %s",
			got.Status.SchedulingStatus, got.Status.SchedulingMessage, EventReasonHailoDeviceInUse, holder)
	}
	if events := warningEvents(rec, EventReasonHailoDeviceInUse); len(events) != 1 {
		t.Errorf("Warning %s events = %v, want one", EventReasonHailoDeviceInUse, events)
	}
	a.mu.RLock()
	_, ok := a.processes["default/"+name]
	a.mu.RUnlock()
	if ok {
		t.Errorf("%s: a refused start stored a process", name)
	}
}

func TestHailoDevice_SecondServiceRefused(t *testing.T) {
	a, ex, rec := hailoDeviceFixture(t)
	mustStart(t, a, "svc-a")

	err := a.ensureProcess(context.Background(), getISVC(t, a, "svc-b"))
	wantHailoRefusal(t, a, rec, err)
	if ex.starts != 1 {
		t.Errorf("starts = %d, want 1", ex.starts)
	}
	a.mu.RLock()
	_, served := a.processes["default/svc-a"]
	a.mu.RUnlock()
	if !served {
		t.Error("svc-a stopped serving after svc-b's refusal")
	}
}

func TestHailoDevice_SameServiceNotRefused(t *testing.T) {
	a, ex, _ := hailoDeviceFixture(t)
	mustStart(t, a, "svc-a")

	// A reconcile of the running spec.
	mustStart(t, a, "svc-a")
	if ex.starts != 1 {
		t.Fatalf("starts after a no-op reconcile = %d, want 1", ex.starts)
	}

	// A spec change restarts the service.
	isvc := getISVC(t, a, "svc-a")
	isvc.Spec.ContextSize = ptr.To(int32(4096))
	if err := a.config.K8sClient.Update(context.Background(), isvc); err != nil {
		t.Fatal(err)
	}
	mustStart(t, a, "svc-a")
	if ex.starts != 2 {
		t.Errorf("starts after a spec change = %d, want 2", ex.starts)
	}
	if got := getISVC(t, a, "svc-a"); got.Status.SchedulingStatus == EventReasonHailoDeviceInUse {
		t.Errorf("svc-a refused itself: %q", got.Status.SchedulingMessage)
	}
}

func TestHailoDevice_FreedByDelete(t *testing.T) {
	a, _, rec := hailoDeviceFixture(t)
	mustStart(t, a, "svc-a")
	err := a.ensureProcess(context.Background(), getISVC(t, a, "svc-b"))
	wantHailoRefusal(t, a, rec, err)

	if err := a.deleteProcess(context.Background(), "default/svc-a"); err != nil {
		t.Fatalf("deleteProcess: %v", err)
	}
	mustStart(t, a, "svc-b")
}

func TestHailoDevice_FreedByScaleToZero(t *testing.T) {
	a, _, _ := hailoDeviceFixture(t)
	mustStart(t, a, "svc-a")

	isvc := getISVC(t, a, "svc-a")
	isvc.Spec.Replicas = ptr.To(int32(0))
	if err := a.config.K8sClient.Update(context.Background(), isvc); err != nil {
		t.Fatal(err)
	}
	if err := a.ensureProcess(context.Background(), getISVC(t, a, "svc-a")); err != nil {
		t.Fatalf("ensureProcess (scaled to zero): %v", err)
	}
	mustStart(t, a, "svc-b")
}

func TestHailoDevice_HeldWhileStarting(t *testing.T) {
	a, ex, rec := hailoDeviceFixture(t)
	var secondErr error
	ex.onStart = func() {
		ex.onStart = nil
		secondErr = a.ensureProcess(context.Background(), getISVC(t, a, "svc-b"))
	}
	mustStart(t, a, "svc-a")
	wantHailoRefusal(t, a, rec, secondErr)
}

func TestHailoDevice_FreedByFailedStart(t *testing.T) {
	a, ex, _ := hailoDeviceFixture(t)
	ex.startErr = errors.New("hailo-ollama: pull failed")
	if err := a.ensureProcess(context.Background(), getISVC(t, a, "svc-a")); err == nil {
		t.Fatal("ensureProcess(svc-a) with a failing start: want an error")
	}
	ex.startErr = nil
	mustStart(t, a, "svc-b")
}

func TestHailoDevice_HeldAcrossHealthRestart(t *testing.T) {
	a, _, rec := hailoDeviceFixture(t)
	mustStart(t, a, "svc-a")
	a.mu.Lock()
	proc := a.processes["default/svc-a"]
	proc.Healthy = false
	a.mu.Unlock()
	a.restartUnhealthy(context.Background(), "default/svc-a", proc, a.healthFailureThreshold())
	drainEvents(rec)

	err := a.ensureProcess(context.Background(), getISVC(t, a, "svc-b"))
	wantHailoRefusal(t, a, rec, err)

	a.runDueRestarts(context.Background())
	a.mu.RLock()
	_, restarted := a.processes["default/svc-a"]
	a.mu.RUnlock()
	if !restarted {
		t.Error("svc-a's health restart did not start it")
	}
}

// TestHailoDevice_OwnClaimNotAHolder covers each way a service holds the
// device: none of them makes it a holder against itself.
func TestHailoDevice_OwnClaimNotAHolder(t *testing.T) {
	const key = "default/svc-a"
	for name, hold := range map[string]func(a *MetalAgent){
		"starting": func(a *MetalAgent) { a.hailoStarting = key },
		"serving":  func(a *MetalAgent) { a.processes[key] = &ManagedProcess{Hailo: true} },
		"restart":  func(a *MetalAgent) { a.restarts[key] = &restartBackoff{pending: true, hailo: true} },
	} {
		t.Run(name, func(t *testing.T) {
			a, _, _ := hailoDeviceFixture(t)
			hold(a)
			if holder := a.claimHailoDevice(key); holder != "" {
				t.Errorf("claimHailoDevice(%s) = %q, want no holder", key, holder)
			}
			if holder := a.claimHailoDevice("default/svc-b"); holder != key {
				t.Errorf("claimHailoDevice(default/svc-b) = %q, want %s", holder, key)
			}
		})
	}
}

// TestHailoDevice_SettledRestartNotAHolder covers restart state kept for its
// backoff after the restart ran: with no restart owed, it holds nothing.
func TestHailoDevice_SettledRestartNotAHolder(t *testing.T) {
	a, _, _ := hailoDeviceFixture(t)
	a.restarts["default/svc-a"] = &restartBackoff{hailo: true}
	mustStart(t, a, "svc-b")
}

func TestHailoDevice_OtherSourcesUnaffected(t *testing.T) {
	a, ex, _ := hailoDeviceFixture(t)
	mustStart(t, a, "svc-a")

	gguf := refusalModel()
	gguf.Name = "gguf"
	llama := refusalISVC("svc-c")
	llama.Spec.ModelRef = gguf.Name
	for _, o := range []runtimeObject{gguf, llama} {
		if err := a.config.K8sClient.Create(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	mustStart(t, a, "svc-c")
	if ex.starts != 2 {
		t.Errorf("starts = %d, want 2", ex.starts)
	}
}
