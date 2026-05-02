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

package agent

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func newPressureTestAgent(t *testing.T, isvcs ...*inferencev1alpha1.InferenceService) *MetalAgent {
	t.Helper()
	scheme := newTestScheme()
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&inferencev1alpha1.InferenceService{})
	for _, isvc := range isvcs {
		builder = builder.WithObjects(isvc)
	}
	k8sClient := builder.Build()

	return NewMetalAgent(MetalAgentConfig{
		K8sClient: k8sClient,
		Logger:    newNopLogger(),
	})
}

// nolint:unparam // name is parameterized for clarity at call sites even though existing tests all use "svc-a"
func newPressureTestISvc(name, priority string) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: name, Priority: priority},
	}
}

// TestHandleMemoryPressure_ConditionPatchedOnTransition verifies that a
// transition from Normal to Warning patches the MemoryPressure condition on
// the running InferenceService so operators can see why their workload is
// degraded.
func TestHandleMemoryPressure_ConditionPatchedOnTransition(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "normal")
	a := newPressureTestAgent(t, isvc)

	a.processes["default/svc-a"] = &ManagedProcess{
		Name: "svc-a", Namespace: "default", ModelPath: "/m.gguf",
		Priority: "normal", StartedAt: time.Now(),
	}

	a.handleMemoryPressure(context.Background(), MemoryPressureWarning, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 25 << 30,
	})

	got := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "svc-a"}, got); err != nil {
		t.Fatalf("get isvc: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionMemoryPressure)
	if cond == nil {
		t.Fatal("expected MemoryPressure condition to be set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("status = %v, want True", cond.Status)
	}
	if cond.Reason != ReasonMemoryWarning {
		t.Errorf("reason = %q, want %q", cond.Reason, ReasonMemoryWarning)
	}
}

// TestHandleMemoryPressure_NoEvictionAtWarning ensures that under Warning
// pressure we surface the condition but do NOT kill any processes — the user
// should get a warning before we start tearing things down.
func TestHandleMemoryPressure_NoEvictionAtWarning(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "low")
	a := newPressureTestAgent(t, isvc)

	a.processes["default/svc-a"] = &ManagedProcess{
		Name: "svc-a", Namespace: "default", ModelPath: "/m.gguf",
		Priority: "low", StartedAt: time.Now(),
	}

	a.handleMemoryPressure(context.Background(), MemoryPressureWarning, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 80 << 30,
	})

	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.pressureBlocked) != 0 {
		t.Errorf("Warning level must not block any process, got %d", len(a.pressureBlocked))
	}
	if _, still := a.processes["default/svc-a"]; !still {
		t.Error("process map should still contain svc-a; eviction is critical-only")
	}
}

// TestHandleMemoryPressure_EvictionBlockedBelowGuard verifies the
// friendly-fire guard: even under Critical pressure, we will not evict if
// LLMKube is using less than 50% of system RSS.
func TestHandleMemoryPressure_EvictionBlockedBelowGuard(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "low")
	a := newPressureTestAgent(t, isvc)

	a.processes["default/svc-a"] = &ManagedProcess{
		Name: "svc-a", Namespace: "default", ModelPath: "/m.gguf",
		Priority: "low", StartedAt: time.Now(),
	}

	// 30% of total — pressure is from somewhere else, not us.
	a.handleMemoryPressure(context.Background(), MemoryPressureCritical, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 30 << 30,
	})

	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.pressureBlocked) != 0 {
		t.Errorf("must not evict below 50%% RSS guard, got %d blocked", len(a.pressureBlocked))
	}
}

// TestEnsureProcess_BlockedUnderCriticalSkipsRespawn verifies the back-pressure
// guard in ensureProcess: a key in pressureBlocked is a no-op while pressure
// is non-Normal, so the controller's UPDATED-event loop cannot defeat
// eviction by silently respawning.
func TestEnsureProcess_BlockedUnderCriticalSkipsRespawn(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "low")
	a := newPressureTestAgent(t, isvc)
	a.executor = NewMetalExecutor("/fake/llama-server", "/tmp/models", newNopLogger())

	a.pressureBlocked["default/svc-a"] = true
	a.lastPressureLevel = MemoryPressureCritical

	if err := a.ensureProcess(context.Background(), isvc); err != nil {
		t.Fatalf("ensureProcess returned error, expected silent skip: %v", err)
	}
	if _, exists := a.processes["default/svc-a"]; exists {
		t.Error("ensureProcess respawned despite pressureBlocked guard")
	}
}

// TestHandleMemoryPressure_NormalClearsBlockedSet verifies that when pressure
// drops back to Normal, previously evicted services become respawnable again
// (pressureBlocked is reset).
func TestHandleMemoryPressure_NormalClearsBlockedSet(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "low")
	a := newPressureTestAgent(t, isvc)

	a.pressureBlocked["default/svc-a"] = true
	a.lastPressureLevel = MemoryPressureCritical

	a.handleMemoryPressure(context.Background(), MemoryPressureNormal, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 10 << 30,
	})

	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.pressureBlocked) != 0 {
		t.Errorf("Normal pressure should reset pressureBlocked, got %d entries", len(a.pressureBlocked))
	}
	if a.lastPressureLevel != MemoryPressureNormal {
		t.Errorf("lastPressureLevel = %v, want Normal", a.lastPressureLevel)
	}
}
