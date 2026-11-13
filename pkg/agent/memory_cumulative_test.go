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
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// A 64 GiB host at the 0.75 fraction has a 48 GiB budget. A 33 GiB file with no
// GGUF shape estimates at 33 * 1.2 + 0.5 = 40.1 GiB (fallbackEstimate), so one
// such model fits and a second one does not fit next to it (#2080).
const (
	cumulativeTotal  = 64 << 30
	cumulativeBudget = 48 << 30
)

func gib(n float64) uint64 { return uint64(n * (1 << 30)) }

// sizedModel is an admission test Model whose only size signal is status.size.
func sizedModel(size string) *inferencev1alpha1.Model {
	return newAdmissionTestModel("https://model-host.invalid/m.gguf", size)
}

// newCumulativeAgent is an admission agent on a 64 GiB host with a fake event
// recorder; isvcs are stored with the status subresource enabled.
func newCumulativeAgent(
	t *testing.T, cfg MetalAgentConfig, isvcs ...*inferencev1alpha1.InferenceService,
) (*MetalAgent, *record.FakeRecorder) {
	t.Helper()
	b := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{})
	for _, isvc := range isvcs {
		b = b.WithObjects(isvc)
	}
	rec := record.NewFakeRecorder(64)
	cfg.K8sClient = b.Build()
	cfg.EventRecorder = rec
	cfg.MemoryProvider = &mockMemoryProvider{totalBytes: cumulativeTotal}
	cfg.MemoryFraction = 0.75
	cfg.ModelStorePath = t.TempDir()
	return NewMetalAgent(cfg), rec
}

func cumulativeISVC(name string) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "test-model"},
	}
}

func schedulingStatus(t *testing.T, a *MetalAgent, name string) (string, string) {
	t.Helper()
	got := &inferencev1alpha1.InferenceService{}
	key := types.NamespacedName{Namespace: "default", Name: name}
	if err := a.config.K8sClient.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get InferenceService %s: %v", name, err)
	}
	return got.Status.SchedulingStatus, got.Status.SchedulingMessage
}

// The definition of done: with a 40 GiB model running on a 48 GiB budget, a
// second 40 GiB model is refused, and the status and a Warning event name the
// request, the free budget and the service holding the rest.
func TestCheckMemoryAdmission_RefusesWhenRunningModelsFillBudget(t *testing.T) {
	isvc := cumulativeISVC("second")
	a, rec := newCumulativeAgent(t, MetalAgentConfig{}, isvc)
	a.processes["default/first"] = &ManagedProcess{Name: "first", Namespace: "default", AdmittedBytes: gib(40.1)}

	err := a.checkMemoryAdmission(context.Background(), isvc, sizedModel("33.0 GiB"), 2048, "", "")
	if err == nil {
		t.Fatal("second 40 GiB model admitted next to a running 40 GiB model on a 48 GiB budget")
	}
	status, msg := schedulingStatus(t, a, "second")
	if status != EventReasonInsufficientMemory {
		t.Errorf("SchedulingStatus = %q, want %q", status, EventReasonInsufficientMemory)
	}
	for _, want := range []string{"40.1 GB required", "already committed", "default/first", "free", "48.0 GB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("SchedulingMessage %q does not mention %q", msg, want)
		}
	}
	events := drainEvents(rec)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning "+EventReasonInsufficientMemory) ||
		!strings.Contains(events[0], "default/first") {
		t.Errorf("events = %q, want one Warning %s naming default/first", events, EventReasonInsufficientMemory)
	}
	if _, held := a.reservedMemory["default/second"]; held {
		t.Error("a refused admission left a reservation behind")
	}
}

// A single model that fits is admitted exactly as before, and its estimate is
// reserved until the start claims or releases it.
func TestCheckMemoryAdmission_ReservesAdmittedEstimate(t *testing.T) {
	isvc := cumulativeISVC("only")
	a, _ := newCumulativeAgent(t, MetalAgentConfig{}, isvc)

	if err := a.checkMemoryAdmission(context.Background(), isvc, sizedModel("33.0 GiB"), 2048, "", ""); err != nil {
		t.Fatalf("a single 40 GiB model on a 48 GiB budget was refused: %v", err)
	}
	if got := a.reservedMemory["default/only"].bytes; got < gib(40) || got > gib(40.2) {
		t.Errorf("reservation = %d bytes, want the ~40.1 GiB estimate", got)
	}
	if got := testutil.ToFloat64(memoryCommittedBytes); got != float64(a.reservedMemory["default/only"].bytes) {
		t.Errorf("committed gauge = %v, want the reserved estimate", got)
	}
}

// Two starts in flight for different services cannot both pass: the first
// one's reservation counts against the second.
func TestCheckMemoryAdmission_ReservationCountsAgainstConcurrentStart(t *testing.T) {
	first, second := cumulativeISVC("first"), cumulativeISVC("second")
	a, _ := newCumulativeAgent(t, MetalAgentConfig{}, first, second)
	model := sizedModel("33.0 GiB")

	if err := a.checkMemoryAdmission(context.Background(), first, model, 2048, "", ""); err != nil {
		t.Fatalf("first admission refused: %v", err)
	}
	if err := a.checkMemoryAdmission(context.Background(), second, model, 2048, "", ""); err == nil {
		t.Fatal("second admission passed while the first one's estimate was still reserved")
	}

	// Once the first start gives its reservation back, the second fits.
	a.releaseAdmission("default/first")
	if err := a.checkMemoryAdmission(context.Background(), second, model, 2048, "", ""); err != nil {
		t.Fatalf("second admission refused after the first reservation was released: %v", err)
	}
}

// A respawn (spec change, health restart) is admitted against everything but
// its own previous record, so it is not refused for overlapping itself.
func TestCheckMemoryAdmission_RespawnExcludesItsOwnRecord(t *testing.T) {
	isvc := cumulativeISVC("svc")
	a, _ := newCumulativeAgent(t, MetalAgentConfig{}, isvc)
	a.processes["default/svc"] = &ManagedProcess{Name: "svc", Namespace: "default", AdmittedBytes: gib(40.1)}

	if err := a.checkMemoryAdmission(context.Background(), isvc, sizedModel("33.0 GiB"), 2048, "", ""); err != nil {
		t.Fatalf("respawn refused against its own record: %v", err)
	}
}

// A service that leaves the process table (delete, eviction, scale to zero)
// stops counting against the budget.
func TestCheckMemoryAdmission_StoppedProcessFreesBudget(t *testing.T) {
	isvc := cumulativeISVC("second")
	a, _ := newCumulativeAgent(t, MetalAgentConfig{}, isvc)
	a.executors[runtimeLlamaServer] = stubExecutor{}
	a.registry = NewServiceRegistry(a.config.K8sClient, "", newNopLogger(), "")
	a.processes["default/first"] = &ManagedProcess{
		Name: "first", Namespace: "default", PID: 999990, AdmittedBytes: gib(40.1), Runtime: runtimeLlamaServer,
	}
	model := sizedModel("33.0 GiB")

	if err := a.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err == nil {
		t.Fatal("admitted while the first service was running")
	}
	if err := a.deleteProcess(context.Background(), "default/first"); err != nil {
		t.Fatalf("deleteProcess: %v", err)
	}
	if err := a.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err != nil {
		t.Fatalf("still refused after the first service stopped: %v", err)
	}
}

// A Model memoryBudget above the host's budget is clamped to it, with a Warning
// event; --allow-model-memory-budget-override restores the old behavior.
func TestCheckMemoryAdmission_ClampsModelBudgetToHost(t *testing.T) {
	// 50 GiB file: 60.5 GiB estimate, under the Model's 100Gi but over the
	// host's 48 GiB.
	model := sizedModel("50.0 GiB")
	model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{MemoryBudget: "100Gi"}

	t.Run("clamped by default", func(t *testing.T) {
		isvc := cumulativeISVC("big")
		a, rec := newCumulativeAgent(t, MetalAgentConfig{}, isvc)
		if err := a.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err == nil {
			t.Fatal("a Model memoryBudget above the host's budget let a 60 GiB model onto a 48 GiB budget")
		}
		events := strings.Join(drainEvents(rec), "\n")
		if !strings.Contains(events, "Warning "+EventReasonMemoryBudgetClamped) {
			t.Errorf("events = %q, want a Warning %s", events, EventReasonMemoryBudgetClamped)
		}
	})

	t.Run("memoryFraction above the agent's is clamped too", func(t *testing.T) {
		isvc := cumulativeISVC("frac")
		a, rec := newCumulativeAgent(t, MetalAgentConfig{}, isvc)
		one := 1.0
		m := sizedModel("50.0 GiB")
		m.Spec.Hardware = &inferencev1alpha1.HardwareSpec{MemoryFraction: &one}
		if err := a.checkMemoryAdmission(context.Background(), isvc, m, 2048, "", ""); err == nil {
			t.Fatal("memoryFraction 1.0 let a 60 GiB model onto a 48 GiB budget")
		}
		if events := strings.Join(drainEvents(rec), "\n"); !strings.Contains(events, EventReasonMemoryBudgetClamped) {
			t.Errorf("events = %q, want %s", events, EventReasonMemoryBudgetClamped)
		}
	})

	t.Run("override flag allows it", func(t *testing.T) {
		isvc := cumulativeISVC("big")
		a, rec := newCumulativeAgent(t, MetalAgentConfig{AllowModelMemoryBudgetOverride: true}, isvc)
		if err := a.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err != nil {
			t.Fatalf("override flag set, Model budget 100Gi, 60 GiB model refused: %v", err)
		}
		if events := strings.Join(drainEvents(rec), "\n"); strings.Contains(events, EventReasonMemoryBudgetClamped) {
			t.Errorf("clamp event emitted with the override flag set: %q", events)
		}
	})
}

// The start path claims the reservation onto the process record when the
// engine starts and gives it back when the start fails.
func TestEnsureProcess_ClaimsOrReleasesAdmission(t *testing.T) {
	t.Run("started engine carries its admitted bytes", func(t *testing.T) {
		f := newRestartFixture(t, "claim")
		delete(f.agent.processes, f.key)
		if err := f.agent.ensureProcess(context.Background(), f.isvc); err != nil {
			t.Fatalf("ensureProcess: %v", err)
		}
		proc := f.agent.processes[f.key]
		if proc == nil || proc.AdmittedBytes == 0 {
			t.Fatalf("process = %+v, want a record carrying its admitted estimate", proc)
		}
		if _, held := f.agent.reservedMemory[f.key]; held {
			t.Error("reservation still held after the engine started")
		}
	})

	t.Run("failed start releases the reservation", func(t *testing.T) {
		f := newRestartFixture(t, "release")
		delete(f.agent.processes, f.key)
		f.exec.startErr = errors.New("spawn failed")
		_ = f.agent.ensureProcess(context.Background(), f.isvc)
		if _, held := f.agent.reservedMemory[f.key]; held {
			t.Error("a failed start left its reservation behind, holding budget forever")
		}
	})
}

// newCgroupAgent is an admission agent at the 0.75 fraction on a fake 64 GiB
// Linux host whose agent cgroup has memory.max set to cgroupMax ("max" for
// no limit): a 48 GiB host-wide budget and a cgroupMax*0.75 cgroup budget.
func newCgroupAgent(
	t *testing.T, cgroupMax string, isvcs ...*inferencev1alpha1.InferenceService,
) *MetalAgent {
	t.Helper()
	root := writeProcFixture(t, map[string]string{
		"proc/meminfo":            "MemTotal: 67108864 kB\nMemAvailable: 60000000 kB\n",
		"proc/self/cgroup":        "0::/agent\n",
		"cg/agent/memory.max":     cgroupMax + "\n",
		"cg/agent/memory.current": "0\n",
	})
	a, _ := newCumulativeAgent(t, MetalAgentConfig{}, isvcs...)
	a.memoryProvider = &LinuxMemoryProvider{
		ProcRoot:   filepath.Join(root, "proc"),
		CgroupRoot: filepath.Join(root, "cg"),
	}
	return a
}

func runtimeISVC(name, runtime string) *inferencev1alpha1.InferenceService {
	isvc := cumulativeISVC(name)
	isvc.Spec.Runtime = runtime
	return isvc
}

// A 20 GiB Ollama model runs outside a 4 GiB agent cgroup, so it does not
// fill the cgroup's 3 GiB budget; in-cgroup services still share that budget.
func TestCheckMemoryAdmission_HostWideServiceDoesNotFillCgroupBudget(t *testing.T) {
	small := runtimeISVC("small", runtimeLlamaServer)
	second := runtimeISVC("second", runtimeLlamaServer)
	a := newCgroupAgent(t, "4294967296", small, second)
	a.processes["default/ollama"] = &ManagedProcess{
		Name: "ollama", Namespace: "default", Runtime: runtimeOllama, AdmittedBytes: gib(20),
	}

	// 0.4 GiB * 1.2 + 0.5 GiB = 0.98 GiB.
	if err := a.checkMemoryAdmission(context.Background(), small, sizedModel("0.4 GiB"), 2048, "", ""); err != nil {
		t.Fatalf("1 GiB llama-server service refused under an empty 3 GiB cgroup budget: %v", err)
	}
	if got := testutil.ToFloat64(memoryCommittedBytes); got != float64(gib(20)+a.reservedMemory["default/small"].bytes) {
		t.Errorf("committed gauge = %v, want every service's estimate, Ollama's included", got)
	}
	if got := testutil.ToFloat64(memoryBudgetBytes); got != cumulativeBudget {
		t.Errorf("budget gauge = %v, want the %d host-wide budget", got, uint64(cumulativeBudget))
	}

	// 1.8 GiB * 1.2 + 0.5 GiB = 2.66 GiB, over the 2.02 GiB the cgroup has left.
	err := a.checkMemoryAdmission(context.Background(), second, sizedModel("1.8 GiB"), 2048, "", "")
	if err == nil {
		t.Fatal("second llama-server service admitted past the 3 GiB cgroup budget")
	}
	_, msg := schedulingStatus(t, a, "second")
	if !strings.Contains(msg, "3.0 GB budget") || !strings.Contains(msg, "default/small") ||
		strings.Contains(msg, "default/ollama") {
		t.Errorf("SchedulingMessage %q: want the 3.0 GB cgroup budget held by default/small only", msg)
	}
}

// A big in-cgroup service does not block an Ollama service that fits
// host-wide, but it still counts toward the host-wide budget.
func TestCheckMemoryAdmission_CgroupServiceCountsTowardHostBudget(t *testing.T) {
	fits := runtimeISVC("fits", runtimeOllama)
	over := runtimeISVC("over", runtimeOllama)
	a := newCgroupAgent(t, "4294967296", fits, over)
	a.processes["default/llama"] = &ManagedProcess{
		Name: "llama", Namespace: "default", Runtime: runtimeLlamaServer, AdmittedBytes: gib(2.9),
	}

	// 33 GiB -> 40.1 GiB; 2.9 + 40.1 fits the 48 GiB host-wide budget.
	if err := a.checkMemoryAdmission(context.Background(), fits, sizedModel("33.0 GiB"), 2048, "", ""); err != nil {
		t.Fatalf("Ollama service refused although it fits host-wide next to the in-cgroup service: %v", err)
	}
	delete(a.reservedMemory, "default/fits")

	// 38 GiB -> 46.1 GiB fits 48 GiB alone, not next to the 2.9 GiB service.
	err := a.checkMemoryAdmission(context.Background(), over, sizedModel("38.0 GiB"), 2048, "", "")
	if err == nil {
		t.Fatal("Ollama service admitted past the host-wide budget the in-cgroup service shares")
	}
	_, msg := schedulingStatus(t, a, "over")
	if !strings.Contains(msg, "48.0 GB budget") || !strings.Contains(msg, "default/llama") {
		t.Errorf("SchedulingMessage %q: want the 48.0 GB host-wide budget held by default/llama", msg)
	}
}

// Without a cgroup limit, and on macOS, the cgroup and host-wide budgets are
// the same and every service shares one pool: a running Ollama model blocks an
// in-cgroup service that does not fit next to it.
func TestCheckMemoryAdmission_OnePoolWithoutCgroupLimit(t *testing.T) {
	for name, newAgent := range map[string]func(*inferencev1alpha1.InferenceService) *MetalAgent{
		"linux, no memory.max": func(isvc *inferencev1alpha1.InferenceService) *MetalAgent {
			return newCgroupAgent(t, "max", isvc)
		},
		"non-Linux provider (macOS)": func(isvc *inferencev1alpha1.InferenceService) *MetalAgent {
			a, _ := newCumulativeAgent(t, MetalAgentConfig{}, isvc)
			return a
		},
	} {
		t.Run(name, func(t *testing.T) {
			isvc := runtimeISVC("llama", runtimeLlamaServer)
			a := newAgent(isvc)
			a.processes["default/ollama"] = &ManagedProcess{
				Name: "ollama", Namespace: "default", Runtime: runtimeOllama, AdmittedBytes: gib(20),
			}
			err := a.checkMemoryAdmission(context.Background(), isvc, sizedModel("33.0 GiB"), 2048, "", "")
			if err == nil {
				t.Fatal("40 GiB service admitted next to a 20 GiB Ollama model on a 48 GiB budget")
			}
			_, msg := schedulingStatus(t, a, "llama")
			if !strings.Contains(msg, "48.0 GB budget") || !strings.Contains(msg, "default/ollama") {
				t.Errorf("SchedulingMessage %q: want the 48.0 GB budget held by default/ollama", msg)
			}
			if got := testutil.ToFloat64(memoryBudgetBytes); got != cumulativeBudget {
				t.Errorf("budget gauge = %v, want %d", got, uint64(cumulativeBudget))
			}
		})
	}
}
