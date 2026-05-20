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
	if err := r.PatchHeartbeat(context.Background(), foremanv1alpha1.FleetNodePhaseReady); err != nil {
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
