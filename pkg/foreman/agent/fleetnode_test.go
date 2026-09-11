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
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := foremanv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("foreman scheme: %v", err)
	}
	return s
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&foremanv1alpha1.FleetNode{}).
		Build()
}

// fixedCapability is a deterministic CapabilityProvider so heartbeat-
// patch assertions don't depend on the host's live sysctl / vm_stat.
type fixedCapability struct {
	cap foremanv1alpha1.FleetNodeCapability
}

func (f *fixedCapability) Capability() foremanv1alpha1.FleetNodeCapability { return f.cap }

// fixedSupervisionCapacity is a deterministic SupervisionCapacityProvider so
// the heartbeat-publish assertions do not depend on any in-process state.
type fixedSupervisionCapacity struct {
	current int32
	maximum *int32
}

func (f fixedSupervisionCapacity) SupervisionCapacity() (int32, *int32) {
	return f.current, f.maximum
}

func TestSpecEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b foremanv1alpha1.FleetNodeSpec
		want bool
	}{
		{"both_zero", foremanv1alpha1.FleetNodeSpec{}, foremanv1alpha1.FleetNodeSpec{}, true},
		{
			"identical_fully_populated",
			foremanv1alpha1.FleetNodeSpec{NodeName: "m5", TailscaleAddr: "ts", Roles: []string{"worker", "verifier"}},
			foremanv1alpha1.FleetNodeSpec{NodeName: "m5", TailscaleAddr: "ts", Roles: []string{"worker", "verifier"}},
			true,
		},
		{
			"different_node_name",
			foremanv1alpha1.FleetNodeSpec{NodeName: "m5"},
			foremanv1alpha1.FleetNodeSpec{NodeName: "m6"},
			false,
		},
		{
			"different_tailscale_addr",
			foremanv1alpha1.FleetNodeSpec{NodeName: "m5", TailscaleAddr: "a"},
			foremanv1alpha1.FleetNodeSpec{NodeName: "m5", TailscaleAddr: "b"},
			false,
		},
		{
			"different_roles_length",
			foremanv1alpha1.FleetNodeSpec{Roles: []string{"worker"}},
			foremanv1alpha1.FleetNodeSpec{Roles: []string{"worker", "verifier"}},
			false,
		},
		{
			"role_value_mismatch",
			foremanv1alpha1.FleetNodeSpec{Roles: []string{"worker"}},
			foremanv1alpha1.FleetNodeSpec{Roles: []string{"verifier"}},
			false,
		},
		{
			"role_order_matters",
			foremanv1alpha1.FleetNodeSpec{Roles: []string{"worker", "verifier"}},
			foremanv1alpha1.FleetNodeSpec{Roles: []string{"verifier", "worker"}},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := specEqual(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("specEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestRegistrar_Upsert_CreatesIfMissing(t *testing.T) {
	kc := newFakeClient(t)
	r := &Registrar{
		Client:   kc,
		NodeName: "m5-max",
		Spec: foremanv1alpha1.FleetNodeSpec{
			NodeName: "m5-max",
			Roles:    []string{"worker"},
		},
		Provider: &fixedCapability{},
	}
	if err := r.Upsert(context.Background()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "m5-max"}, &got); err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if got.Spec.NodeName != "m5-max" {
		t.Errorf("Spec.NodeName = %q, want %q", got.Spec.NodeName, "m5-max")
	}
	if len(got.Spec.Roles) != 1 || got.Spec.Roles[0] != "worker" {
		t.Errorf("Spec.Roles = %v, want [worker]", got.Spec.Roles)
	}
}

func TestRegistrar_Upsert_UpdatesIfSpecChanged(t *testing.T) {
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "m5-max"},
		Spec: foremanv1alpha1.FleetNodeSpec{
			NodeName: "m5-max",
			Roles:    []string{"worker"},
		},
	}
	kc := newFakeClient(t, existing)
	r := &Registrar{
		Client:   kc,
		NodeName: "m5-max",
		Spec: foremanv1alpha1.FleetNodeSpec{
			NodeName:      "m5-max",
			TailscaleAddr: "m5-max.tail-scale.ts.net",
			Roles:         []string{"worker", "verifier"},
		},
		Provider: &fixedCapability{},
	}
	if err := r.Upsert(context.Background()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "m5-max"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.TailscaleAddr != "m5-max.tail-scale.ts.net" {
		t.Errorf("TailscaleAddr not updated: got %q", got.Spec.TailscaleAddr)
	}
	if len(got.Spec.Roles) != 2 {
		t.Errorf("Roles not updated: got %v", got.Spec.Roles)
	}
}

func TestRegistrar_Upsert_NoopIfSpecUnchanged(t *testing.T) {
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "m5-max", ResourceVersion: "1"},
		Spec: foremanv1alpha1.FleetNodeSpec{
			NodeName: "m5-max",
			Roles:    []string{"worker"},
		},
	}
	kc := newFakeClient(t, existing)
	r := &Registrar{
		Client:   kc,
		NodeName: "m5-max",
		Spec: foremanv1alpha1.FleetNodeSpec{
			NodeName: "m5-max",
			Roles:    []string{"worker"},
		},
		Provider: &fixedCapability{},
	}
	if err := r.Upsert(context.Background()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "m5-max"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The fake client bumps resourceVersion on every Update. A noop
	// Upsert leaves it where it was.
	if got.ResourceVersion != "1" {
		t.Errorf("ResourceVersion changed from %q to %q (expected noop on identical spec)",
			"1", got.ResourceVersion)
	}
}

func TestMergeManagedLabels(t *testing.T) {
	cases := []struct {
		name        string
		existing    map[string]string
		managed     map[string]string
		wantChanged bool
		wantKV      map[string]string // subset that must be present
	}{
		{
			name: "no managed labels", existing: map[string]string{"a": "b"},
			managed: nil, wantChanged: false, wantKV: map[string]string{"a": "b"},
		},
		{
			name: "add to nil existing", existing: nil,
			managed: map[string]string{"coder-pool": "amd"}, wantChanged: true,
			wantKV: map[string]string{"coder-pool": "amd"},
		},
		{
			name: "add missing key", existing: map[string]string{"a": "b"},
			managed: map[string]string{"coder-pool": "amd"}, wantChanged: true,
			wantKV: map[string]string{"a": "b", "coder-pool": "amd"},
		},
		{
			name: "overwrite changed value", existing: map[string]string{"coder-pool": "metal"},
			managed: map[string]string{"coder-pool": "amd"}, wantChanged: true,
			wantKV: map[string]string{"coder-pool": "amd"},
		},
		{
			name: "no change when present+equal", existing: map[string]string{"coder-pool": "amd"},
			managed: map[string]string{"coder-pool": "amd"}, wantChanged: false,
			wantKV: map[string]string{"coder-pool": "amd"},
		},
		{
			name: "preserve unmanaged labels", existing: map[string]string{"keep": "me"},
			managed: map[string]string{"coder-pool": "amd"}, wantChanged: true,
			wantKV: map[string]string{"keep": "me", "coder-pool": "amd"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := mergeManagedLabels(tc.existing, tc.managed)
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			for k, v := range tc.wantKV {
				if got[k] != v {
					t.Errorf("result[%q] = %q, want %q (full: %v)", k, got[k], v, got)
				}
			}
		})
	}
}

func TestRegistrar_Upsert_SetsLabelsOnCreate(t *testing.T) {
	kc := newFakeClient(t)
	r := &Registrar{
		Client:   kc,
		NodeName: "incluster-agent",
		Spec:     foremanv1alpha1.FleetNodeSpec{NodeName: "incluster-agent", Roles: []string{"worker", "coder"}},
		Labels:   map[string]string{"coder-pool": "amd"},
		Provider: &fixedCapability{},
	}
	if err := r.Upsert(context.Background()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "incluster-agent"}, &got); err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if got.Labels["coder-pool"] != "amd" {
		t.Errorf("Labels = %v, want coder-pool=amd", got.Labels)
	}
}

func TestRegistrar_Upsert_ReappliesLabelsOnExisting(t *testing.T) {
	// Simulate a FleetNode that lost its managed label (e.g. a hand-applied
	// label dropped on pod recreate) while carrying an unrelated label. Upsert
	// must re-add the managed label without disturbing the unmanaged one, even
	// though the spec is unchanged.
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "incluster-agent",
			ResourceVersion: "1",
			Labels:          map[string]string{"unmanaged": "keep"},
		},
		Spec: foremanv1alpha1.FleetNodeSpec{NodeName: "incluster-agent", Roles: []string{"worker", "coder"}},
	}
	kc := newFakeClient(t, existing)
	r := &Registrar{
		Client:   kc,
		NodeName: "incluster-agent",
		Spec:     foremanv1alpha1.FleetNodeSpec{NodeName: "incluster-agent", Roles: []string{"worker", "coder"}},
		Labels:   map[string]string{"coder-pool": "amd"},
		Provider: &fixedCapability{},
	}
	if err := r.Upsert(context.Background()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "incluster-agent"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Labels["coder-pool"] != "amd" {
		t.Errorf("managed label not re-applied: %v", got.Labels)
	}
	if got.Labels["unmanaged"] != "keep" {
		t.Errorf("unmanaged label not preserved: %v", got.Labels)
	}
}

func TestRegistrar_PatchHeartbeat_WritesPhaseAndCapability(t *testing.T) {
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "m5-max"},
		Spec:       foremanv1alpha1.FleetNodeSpec{NodeName: "m5-max"},
	}
	kc := newFakeClient(t, existing)
	cap := foremanv1alpha1.FleetNodeCapability{
		Accelerator:      foremanv1alpha1.FleetNodeAccelerator("metal"),
		TotalRAMGB:       128,
		AvailableRAMGB:   64,
		MaxContextTokens: 131072,
		TokensPerSecond:  47,
	}
	r := &Registrar{
		Client:   kc,
		NodeName: "m5-max",
		Provider: &fixedCapability{cap: cap},
	}
	before := time.Now().Add(-time.Second)
	if _, err := r.PatchHeartbeat(context.Background(), foremanv1alpha1.FleetNodePhaseReady); err != nil {
		t.Fatalf("PatchHeartbeat: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "m5-max"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != foremanv1alpha1.FleetNodePhaseReady {
		t.Errorf("Phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.LastHeartbeatTime == nil {
		t.Fatal("LastHeartbeatTime is nil")
	}
	if got.Status.LastHeartbeatTime.Time.Before(before) {
		t.Errorf("LastHeartbeatTime %v is before %v", got.Status.LastHeartbeatTime.Time, before)
	}
	if got.Status.Capability.TotalRAMGB != 128 {
		t.Errorf("Capability.TotalRAMGB = %d, want 128", got.Status.Capability.TotalRAMGB)
	}
	if got.Status.Capability.AvailableRAMGB != 64 {
		t.Errorf("Capability.AvailableRAMGB = %d, want 64", got.Status.Capability.AvailableRAMGB)
	}
	if got.Status.Capability.MaxContextTokens != 131072 {
		t.Errorf("Capability.MaxContextTokens = %d, want 131072", got.Status.Capability.MaxContextTokens)
	}
	if got.Status.Capability.TokensPerSecond != 47 {
		t.Errorf("Capability.TokensPerSecond = %d, want 47", got.Status.Capability.TokensPerSecond)
	}
}

func TestRegistrar_PatchHeartbeat_StampsVersionAndKind(t *testing.T) {
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "studio"},
		Spec:       foremanv1alpha1.FleetNodeSpec{NodeName: "studio"},
	}
	kc := newFakeClient(t, existing)
	r := &Registrar{
		Client:   kc,
		NodeName: "studio",
		Provider: &fixedCapability{},
		Version:  "v0.9.0",
		Kind:     "foreman-agent",
	}
	if _, err := r.PatchHeartbeat(context.Background(), foremanv1alpha1.FleetNodePhaseReady); err != nil {
		t.Fatalf("PatchHeartbeat: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "studio"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.AgentVersion != "v0.9.0" {
		t.Errorf("AgentVersion = %q, want %q", got.Status.AgentVersion, "v0.9.0")
	}
	if got.Status.AgentKind != foremanv1alpha1.FleetNodeAgentKind("foreman-agent") {
		t.Errorf("AgentKind = %q, want %q", got.Status.AgentKind, "foreman-agent")
	}
}

func TestRegistrar_PatchHeartbeat_EmptyVersionOmitted(t *testing.T) {
	// When Version and Kind are not set, the status fields must remain empty
	// (not overwrite a value set by a newer agent restart with blank).
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "studio2"},
		Spec:       foremanv1alpha1.FleetNodeSpec{NodeName: "studio2"},
	}
	kc := newFakeClient(t, existing)
	r := &Registrar{
		Client:   kc,
		NodeName: "studio2",
		Provider: &fixedCapability{},
		// Version and Kind intentionally zero
	}
	if _, err := r.PatchHeartbeat(context.Background(), foremanv1alpha1.FleetNodePhaseReady); err != nil {
		t.Fatalf("PatchHeartbeat: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "studio2"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.AgentVersion != "" {
		t.Errorf("AgentVersion = %q, want empty (version not set)", got.Status.AgentVersion)
	}
	if got.Status.AgentKind != "" {
		t.Errorf("AgentKind = %q, want empty (kind not set)", got.Status.AgentKind)
	}
}

// TestRegistrar_Run_SelfUpdateDrainsAndReturnsRestartError verifies that
// when an UpdateApplier signals Restarting=true, Run:
//  1. emits a final Draining heartbeat so the operator stops routing tasks,
//  2. returns ErrSelfUpdateRestart (not nil, not a generic error).
func TestRegistrar_Run_SelfUpdateDrainsAndReturnsRestartError(t *testing.T) {
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "studio-update"},
		Spec:       foremanv1alpha1.FleetNodeSpec{NodeName: "studio-update"},
		Status: foremanv1alpha1.FleetNodeStatus{
			UpdateRequest: &foremanv1alpha1.FleetNodeUpdateRequest{
				TargetVersion: "v0.9.0",
				URL:           "http://example.com/foreman-agent-v0.9.0",
				SHA256:        "a" + strings.Repeat("b", 63),
			},
		},
	}
	kc := newFakeClient(t, existing)

	// Fake applier: signals restarting=true on the first call.
	applierCalled := false
	applier := UpdateApplierFunc(func(_, _, _ string) (bool, error) {
		applierCalled = true
		return true, nil
	})

	r := &Registrar{
		Client:   kc,
		NodeName: "studio-update",
		Provider: &fixedCapability{},
		Interval: 20 * time.Millisecond,
		Version:  "v0.8.4",
		Updater:  applier,
	}

	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrSelfUpdateRestart) {
			t.Fatalf("Run returned %v, want ErrSelfUpdateRestart", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s after self-update trigger")
	}

	if !applierCalled {
		t.Error("UpdateApplier was never called")
	}

	// Final phase must be Draining (agent told operator it is going away).
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "studio-update"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != foremanv1alpha1.FleetNodePhaseDraining {
		t.Errorf("final phase = %q, want Draining", got.Status.Phase)
	}
}

// TestRegistrar_Run_SelfUpdateNilUpdaterNoRestart verifies that when Updater
// is nil (pre-PR4 / disabled) Run keeps heartbeating normally.
func TestRegistrar_Run_SelfUpdateNilUpdaterNoRestart(t *testing.T) {
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "studio-noupdate"},
		Spec:       foremanv1alpha1.FleetNodeSpec{NodeName: "studio-noupdate"},
		Status: foremanv1alpha1.FleetNodeStatus{
			UpdateRequest: &foremanv1alpha1.FleetNodeUpdateRequest{
				TargetVersion: "v0.9.0",
				URL:           "http://example.com/bin",
				SHA256:        "a" + strings.Repeat("b", 63),
			},
		},
	}
	kc := newFakeClient(t, existing)
	r := &Registrar{
		Client:   kc,
		NodeName: "studio-noupdate",
		Provider: &fixedCapability{},
		Interval: 20 * time.Millisecond,
		// Updater intentionally nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Let a couple of ticks fire without triggering self-update.
	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil (Updater=nil skips update)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRegistrar_Run_DrainsAndExitsOnCancel(t *testing.T) {
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "m5-max"},
		Spec:       foremanv1alpha1.FleetNodeSpec{NodeName: "m5-max"},
	}
	kc := newFakeClient(t, existing)
	r := &Registrar{
		Client:   kc,
		NodeName: "m5-max",
		Provider: &fixedCapability{cap: foremanv1alpha1.FleetNodeCapability{TotalRAMGB: 128}},
		Interval: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Let the initial heartbeat + at least one ticker firing happen.
	time.Sleep(75 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}

	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "m5-max"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != foremanv1alpha1.FleetNodePhaseDraining {
		t.Errorf("final phase = %q, want Draining", got.Status.Phase)
	}
}

// The #1640 decision: FleetNode identity is the agent process, and the
// physical machine is a reported PROPERTY. status.kubernetesNode is where
// that property lands for in-cluster agents (fed from the FLEET_NODE_NAME
// downward-API env var, which was previously set by the chart but read by
// nothing).
func TestRegistrar_PatchHeartbeat_StampsKubernetesNode(t *testing.T) {
	kc := newFakeClient(t)
	r := &Registrar{
		Client:   kc,
		NodeName: "coder-agent-abc12",
		Spec: foremanv1alpha1.FleetNodeSpec{
			NodeName: "coder-agent-abc12",
			Roles:    []string{"coder"},
		},
		Provider:       &fixedCapability{},
		KubernetesNode: "ahazidgx1",
	}
	if err := r.Upsert(context.Background()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := r.PatchHeartbeat(context.Background(), foremanv1alpha1.FleetNodePhaseReady); err != nil {
		t.Fatalf("PatchHeartbeat: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "coder-agent-abc12"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.KubernetesNode != "ahazidgx1" {
		t.Errorf("Status.KubernetesNode = %q, want %q", got.Status.KubernetesNode, "ahazidgx1")
	}
}

// Off-cluster agents (the metal Macs) have no Kubernetes node, so a node
// that has never been stamped stays empty. This case alone cannot tell the
// r.KubernetesNode != "" guard apart from its absence (an unconditional ""
// write is indistinguishable on a fresh node); the sticky-value test below
// is the one that pins the guard down.
func TestRegistrar_PatchHeartbeat_OmitsKubernetesNodeWhenUnset(t *testing.T) {
	kc := newFakeClient(t)
	r := &Registrar{
		Client:   kc,
		NodeName: "mac-studio-reviewer",
		Spec: foremanv1alpha1.FleetNodeSpec{
			NodeName: "mac-studio-reviewer",
			Roles:    []string{"reviewer"},
		},
		Provider: &fixedCapability{},
	}
	if err := r.Upsert(context.Background()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := r.PatchHeartbeat(context.Background(), foremanv1alpha1.FleetNodePhaseReady); err != nil {
		t.Fatalf("PatchHeartbeat: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "mac-studio-reviewer"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.KubernetesNode != "" {
		t.Errorf("Status.KubernetesNode = %q, want empty", got.Status.KubernetesNode)
	}
}

// A Registrar with KubernetesNode empty must not erase a value that is
// already on status. This is the case that separates the
// r.KubernetesNode != "" guard from its absence: without the guard, an
// unconditional "" assignment plus the field's json omitempty tag makes
// the MergeFrom patch emit kubernetesNode: null and the stored value is
// lost. The guard makes the property sticky, matching how OS and Arch
// already behave, so a stamped node keeps its last known machine even if
// the agent restarts without the FLEET_NODE_NAME downward-API env var.
func TestRegistrar_PatchHeartbeat_KeepsExistingKubernetesNodeWhenUnset(t *testing.T) {
	kc := newFakeClient(t)
	spec := foremanv1alpha1.FleetNodeSpec{
		NodeName: "coder-agent-abc12",
		Roles:    []string{"coder"},
	}
	stamped := &Registrar{
		Client:         kc,
		NodeName:       "coder-agent-abc12",
		Spec:           spec,
		Provider:       &fixedCapability{},
		KubernetesNode: "ahazidgx1",
	}
	if err := stamped.Upsert(context.Background()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := stamped.PatchHeartbeat(context.Background(), foremanv1alpha1.FleetNodePhaseReady); err != nil {
		t.Fatalf("PatchHeartbeat (stamped): %v", err)
	}

	// A second Registrar for the same FleetNode with the env var absent.
	unset := &Registrar{
		Client:   kc,
		NodeName: "coder-agent-abc12",
		Spec:     spec,
		Provider: &fixedCapability{},
	}
	if _, err := unset.PatchHeartbeat(context.Background(), foremanv1alpha1.FleetNodePhaseReady); err != nil {
		t.Fatalf("PatchHeartbeat (unset): %v", err)
	}

	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "coder-agent-abc12"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.KubernetesNode != "ahazidgx1" {
		t.Errorf("Status.KubernetesNode = %q, want %q (empty Registrar must not clear a stamped value)",
			got.Status.KubernetesNode, "ahazidgx1")
	}
}

// TestRegistrar_PatchHeartbeat_PublishesSupervisionCapacity verifies that the
// Job-mode supervision budget the watcher holds in-process (#1639) lands on
// FleetNode.status on the heartbeat patch, on the same patch as the phase and
// heartbeat time -- not a second write path.
func TestRegistrar_PatchHeartbeat_PublishesSupervisionCapacity(t *testing.T) {
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "m5-max"},
		Spec:       foremanv1alpha1.FleetNodeSpec{NodeName: "m5-max"},
	}
	kc := newFakeClient(t, existing)
	max := int32(4)
	r := &Registrar{
		Client:   kc,
		NodeName: "m5-max",
		Provider: &fixedCapability{},
		Watcher:  fixedSupervisionCapacity{current: 3, maximum: &max},
	}
	if _, err := r.PatchHeartbeat(context.Background(), foremanv1alpha1.FleetNodePhaseReady); err != nil {
		t.Fatalf("PatchHeartbeat: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "m5-max"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.SupervisionCapacity == nil {
		t.Fatal("SupervisionCapacity is nil, want published budget")
	}
	if got.Status.SupervisionCapacity.Current != 3 {
		t.Errorf("SupervisionCapacity.Current = %d, want 3", got.Status.SupervisionCapacity.Current)
	}
	if got.Status.SupervisionCapacity.Maximum == nil || *got.Status.SupervisionCapacity.Maximum != 4 {
		t.Errorf("SupervisionCapacity.Maximum = %v, want 4", got.Status.SupervisionCapacity.Maximum)
	}
}

// TestRegistrar_PatchHeartbeat_SupervisionCapacityZeroMaximum verifies the
// "agent full" case: a node whose bound is 0 is at capacity and declines
// further Job-mode tasks. A zero Maximum must be written verbatim -- it is
// distinguishable from an absent capacity only because the Maximum is a
// pointer on the API and the Registrar writes it whenever a bound is
// reported.
func TestRegistrar_PatchHeartbeat_SupervisionCapacityZeroMaximum(t *testing.T) {
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "m5-full"},
		Spec:       foremanv1alpha1.FleetNodeSpec{NodeName: "m5-full"},
	}
	kc := newFakeClient(t, existing)
	zero := int32(0)
	r := &Registrar{
		Client:   kc,
		NodeName: "m5-full",
		Provider: &fixedCapability{},
		Watcher:  fixedSupervisionCapacity{current: 0, maximum: &zero},
	}
	if _, err := r.PatchHeartbeat(context.Background(), foremanv1alpha1.FleetNodePhaseReady); err != nil {
		t.Fatalf("PatchHeartbeat: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "m5-full"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.SupervisionCapacity == nil {
		t.Fatal("SupervisionCapacity is nil, want published zero-Maximum budget")
	}
	if got.Status.SupervisionCapacity.Maximum == nil {
		t.Fatal("SupervisionCapacity.Maximum is nil, want 0 (agent full)")
	}
	if *got.Status.SupervisionCapacity.Maximum != 0 {
		t.Errorf("SupervisionCapacity.Maximum = %d, want 0", *got.Status.SupervisionCapacity.Maximum)
	}
}

// TestRegistrar_PatchHeartbeat_SupervisionCapacityAbsentWhenNoProvider
// verifies the "older agent / no bound" case: with no Watcher wired the field
// stays absent rather than being written as a zero Maximum, which would
// otherwise read as "unbounded" -- a different state from "not reported".
//
// This is the mutation that must fail: stop writing the capacity fields on the
// heartbeat patch and the first test fails. A test that only asserts the API
// field exists passes with the feature entirely unwired, so this absent-case
// guard is what pins the write path down.
func TestRegistrar_PatchHeartbeat_SupervisionCapacityAbsentWhenNoProvider(t *testing.T) {
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "studio-old"},
		Spec:       foremanv1alpha1.FleetNodeSpec{NodeName: "studio-old"},
	}
	kc := newFakeClient(t, existing)
	r := &Registrar{
		Client:   kc,
		NodeName: "studio-old",
		Provider: &fixedCapability{},
		// Watcher intentionally nil (older agent, no bound to report).
	}
	if _, err := r.PatchHeartbeat(context.Background(), foremanv1alpha1.FleetNodePhaseReady); err != nil {
		t.Fatalf("PatchHeartbeat: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "studio-old"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.SupervisionCapacity != nil {
		t.Errorf("SupervisionCapacity = %+v, want absent (Watcher nil)", *got.Status.SupervisionCapacity)
	}
}

// TestRegistrar_PatchHeartbeat_SupervisionCapacityAbsentPreservesExisting
// verifies that a heartbeat from a node with no bound to report does not
// clobber a value a newer agent already published: the MergeFrom patch must
// leave the field where it was, not emit null.
func TestRegistrar_PatchHeartbeat_SupervisionCapacityAbsentPreservesExisting(t *testing.T) {
	max := int32(4)
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "studio-upgrade"},
		Spec:       foremanv1alpha1.FleetNodeSpec{NodeName: "studio-upgrade"},
		Status: foremanv1alpha1.FleetNodeStatus{
			SupervisionCapacity: &foremanv1alpha1.SupervisionCapacity{
				Current: 2,
				Maximum: &max,
			},
		},
	}
	kc := newFakeClient(t, existing)
	r := &Registrar{
		Client:   kc,
		NodeName: "studio-upgrade",
		Provider: &fixedCapability{},
		// Watcher nil: the upgraded agent reports nothing, so the existing
		// budget must survive.
	}
	if _, err := r.PatchHeartbeat(context.Background(), foremanv1alpha1.FleetNodePhaseReady); err != nil {
		t.Fatalf("PatchHeartbeat: %v", err)
	}
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "studio-upgrade"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.SupervisionCapacity == nil {
		t.Fatal("SupervisionCapacity lost: a nil-Watcher heartbeat must not clobber an existing budget")
	}
	if got.Status.SupervisionCapacity.Current != 2 ||
		got.Status.SupervisionCapacity.Maximum == nil ||
		*got.Status.SupervisionCapacity.Maximum != 4 {
		t.Errorf("SupervisionCapacity = %+v, want {Current:2, Maximum:4}", got.Status.SupervisionCapacity)
	}
}

// TestRegistrar_Run_ReRegistersWhenFleetNodeDeleted verifies that when a
// running agent's FleetNode CR is deleted/reaped (e.g. while silent or asleep),
// the heartbeat loop detects the NotFound error, re-registers the node via
// Upsert, and restores it to Ready status (#1778).
func TestRegistrar_Run_ReRegistersWhenFleetNodeDeleted(t *testing.T) {
	existing := &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-re-register"},
		Spec: foremanv1alpha1.FleetNodeSpec{
			NodeName: "worker-re-register",
			Roles:    []string{"worker"},
		},
	}
	kc := newFakeClient(t, existing)
	r := &Registrar{
		Client:   kc,
		NodeName: "worker-re-register",
		Spec: foremanv1alpha1.FleetNodeSpec{
			NodeName: "worker-re-register",
			Roles:    []string{"worker"},
		},
		Provider: &fixedCapability{cap: foremanv1alpha1.FleetNodeCapability{TotalRAMGB: 64}},
		Interval: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Let the initial heartbeat complete.
	time.Sleep(30 * time.Millisecond)

	// Simulate the reconciler reaping the FleetNode CR.
	if err := kc.Delete(ctx, existing); err != nil {
		t.Fatalf("delete FleetNode: %v", err)
	}

	// Verify the object is gone.
	var check foremanv1alpha1.FleetNode
	if err := kc.Get(ctx, types.NamespacedName{Name: "worker-re-register"}, &check); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after delete, got %v", err)
	}

	// Wait for the next tick: PatchHeartbeat gets NotFound -> Upsert -> PatchHeartbeat.
	time.Sleep(60 * time.Millisecond)

	// Verify the FleetNode was re-created and is Ready.
	var got foremanv1alpha1.FleetNode
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "worker-re-register"}, &got); err != nil {
		t.Fatalf("Get after re-registration: %v", err)
	}
	if got.Status.Phase != foremanv1alpha1.FleetNodePhaseReady {
		t.Errorf("Phase = %v, want Ready", got.Status.Phase)
	}
	if got.Status.LastHeartbeatTime == nil {
		t.Error("LastHeartbeatTime was not populated after re-registration")
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s")
	}

	// Verify clean drain on shutdown.
	if err := kc.Get(context.Background(), types.NamespacedName{Name: "worker-re-register"}, &got); err != nil {
		t.Fatalf("Get after shutdown: %v", err)
	}
	if got.Status.Phase != foremanv1alpha1.FleetNodePhaseDraining {
		t.Errorf("Phase after cancel = %v, want Draining", got.Status.Phase)
	}
}
