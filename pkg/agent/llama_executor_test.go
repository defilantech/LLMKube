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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func newAgentWithoutLlamaServer(t *testing.T) *MetalAgent {
	t.Helper()
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient:      fake.NewClientBuilder().WithScheme(newTestScheme()).Build(),
		Namespace:      "test-ns",
		ModelStorePath: "/tmp/test-models",
		Runtime:        runtimeOllama,
	})
	a.buildExecutors()
	return a
}

func TestBuildExecutorsWithoutLlamaServer(t *testing.T) {
	a := newAgentWithoutLlamaServer(t)
	for _, rt := range []string{runtimeLlamaServer, runtimeLlamaCPP} {
		if _, ok := a.executors[rt]; ok {
			t.Errorf("executors[%q] registered with no llama-server binary; want the start refused as unregistered", rt)
		}
	}
	if _, ok := a.executors[runtimeOllama]; !ok {
		t.Errorf("executors[%q] not registered", runtimeOllama)
	}
}

func TestBuildExecutorsWithLlamaServer(t *testing.T) {
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient:      fake.NewClientBuilder().WithScheme(newTestScheme()).Build(),
		Namespace:      "test-ns",
		ModelStorePath: "/tmp/test-models",
		LlamaServerBin: "/usr/bin/llama-server",
	})
	a.buildExecutors()
	for _, rt := range []string{runtimeLlamaServer, runtimeLlamaCPP} {
		if _, ok := a.executors[rt].(*MetalExecutor); !ok {
			t.Errorf("executors[%q] = %T, want *MetalExecutor", rt, a.executors[rt])
		}
	}
}

// noLlamaServerFixture is refusalFixture on an agent whose llama.cpp executor
// is missing, as when llama-server was not found and --runtime is ollama.
func noLlamaServerFixture(t *testing.T, isvc *inferencev1alpha1.InferenceService) (
	*MetalAgent, *recordingExecutor, *record.FakeRecorder,
) {
	t.Helper()
	a, ex, rec := refusalFixture(t, MetalAgentConfig{Runtime: runtimeOllama}, isvc, refusalModel())
	delete(a.executors, runtimeLlamaServer)
	delete(a.executors, runtimeLlamaCPP)
	a.executors[runtimeOllama] = ex
	return a, ex, rec
}

func TestEnsureProcessRefusesLlamaCPPWithoutLlamaServer(t *testing.T) {
	for _, rt := range []string{runtimeLlamaCPP, runtimeLlamaServer} {
		t.Run(rt, func(t *testing.T) {
			isvc := refusalISVC("svc")
			isvc.Spec.Runtime = rt
			a, ex, rec := noLlamaServerFixture(t, isvc)

			err := a.ensureProcess(context.Background(), isvc)
			if err == nil || !strings.Contains(err.Error(), EventReasonRuntimeNotAvailable) {
				t.Fatalf("ensureProcess error = %v, want %s", err, EventReasonRuntimeNotAvailable)
			}
			if ex.starts != 0 {
				t.Errorf("starts = %d, want 0", ex.starts)
			}

			got := &inferencev1alpha1.InferenceService{}
			if gErr := a.config.K8sClient.Get(context.Background(),
				types.NamespacedName{Name: "svc", Namespace: "default"}, got); gErr != nil {
				t.Fatalf("get InferenceService: %v", gErr)
			}
			if got.Status.SchedulingStatus != EventReasonRuntimeNotAvailable {
				t.Errorf("status.schedulingStatus = %q, want %q",
					got.Status.SchedulingStatus, EventReasonRuntimeNotAvailable)
			}
			for _, want := range []string{"llama-server was not found", "--llama-server", "restart the agent"} {
				if !strings.Contains(got.Status.SchedulingMessage, want) {
					t.Errorf("status.schedulingMessage = %q, want it to contain %q", got.Status.SchedulingMessage, want)
				}
			}
			events := warningEvents(rec, EventReasonRuntimeNotAvailable)
			if len(events) != 1 || !strings.Contains(events[0], "llama-server was not found") {
				t.Errorf("Warning %s events = %v, want one naming the missing llama-server",
					EventReasonRuntimeNotAvailable, events)
			}
		})
	}
}

func TestEnsureProcessRuntimeNotAvailableKeepsRunningEngine(t *testing.T) {
	isvc := refusalISVC("svc")
	isvc.Spec.Runtime = runtimeLlamaCPP
	a, ex, _ := noLlamaServerFixture(t, isvc)
	a.processes["default/svc"] = &ManagedProcess{
		Name: "svc", Namespace: "default", PID: 1111, Port: 18080,
		Healthy: true, SpecHash: "old-hash", Runtime: runtimeOllama,
	}

	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), EventReasonRuntimeNotAvailable) {
		t.Fatalf("ensureProcess error = %v, want %s", err, EventReasonRuntimeNotAvailable)
	}
	if len(ex.stopped) != 0 {
		t.Errorf("stopped = %v, want the running engine left serving", ex.stopped)
	}
	if p := a.processes["default/svc"]; p == nil || p.PID != 1111 {
		t.Errorf("processes[default/svc] = %+v, want pid 1111 still tracked", p)
	}
}

func TestStopManagedProcessWithoutExecutor(t *testing.T) {
	a := newAgentWithoutLlamaServer(t)
	for _, tc := range []struct {
		name    string
		process *ManagedProcess
	}{
		{name: "spawned llama.cpp process", process: &ManagedProcess{Runtime: runtimeLlamaCPP, PID: 4242}},
		{name: "adopted process without a PID", process: &ManagedProcess{Runtime: runtimeLlamaCPP, Adopted: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := a.stopManagedProcess(context.Background(), tc.process, true); err == nil {
				t.Error("stopping a llama.cpp process with no executor returned nil; want an error")
			}
		})
	}
}
