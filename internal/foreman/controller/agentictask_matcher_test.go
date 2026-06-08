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

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func nodeWithCapability(cap foremanv1alpha1.FleetNodeCapability) *foremanv1alpha1.FleetNode {
	n := &foremanv1alpha1.FleetNode{}
	n.Status.Phase = foremanv1alpha1.FleetNodePhaseReady
	// Default to a fresh heartbeat so capability tests are not accidentally
	// gated out by the staleness check.
	now := metav1.Now()
	n.Status.LastHeartbeatTime = &now
	n.Status.Capability = cap
	return n
}

// A node that still reads Phase=Ready but has not heart-beat within the
// staleness window must be treated as ineligible by the scheduler: the
// FleetAgent on it may be dead, so dispatching there is a black hole.
// Defense-in-depth for defilantech/LLMKube#627.
func TestNodeSchedulable_StaleHeartbeatExcludedDespiteReady(t *testing.T) {
	now := time.Now()
	stale := metav1.NewTime(now.Add(-2 * foremanv1alpha1.FleetNodeHeartbeatTimeout))
	node := nodeWithCapability(foremanv1alpha1.FleetNodeCapability{Accelerator: "metal"})
	node.Status.LastHeartbeatTime = &stale

	if nodeSchedulable(node, now) {
		t.Fatalf("expected stale-heartbeat node to be unschedulable despite Phase=Ready")
	}
}

// A fresh Ready node is schedulable.
func TestNodeSchedulable_FreshReadyNode(t *testing.T) {
	now := time.Now()
	fresh := metav1.NewTime(now.Add(-1 * time.Second))
	node := nodeWithCapability(foremanv1alpha1.FleetNodeCapability{Accelerator: "metal"})
	node.Status.LastHeartbeatTime = &fresh

	if !nodeSchedulable(node, now) {
		t.Fatalf("expected fresh Ready node to be schedulable")
	}
}

// A node that has never heart-beat (nil LastHeartbeatTime) is not
// schedulable: we have no evidence the agent is alive.
func TestNodeSchedulable_NeverHeartbeat(t *testing.T) {
	node := nodeWithCapability(foremanv1alpha1.FleetNodeCapability{Accelerator: "metal"})
	node.Status.LastHeartbeatTime = nil

	if nodeSchedulable(node, time.Now()) {
		t.Fatalf("expected node with no heartbeat to be unschedulable")
	}
}

// A reviewer Agent's model is already loaded on the node, so the loop
// needs ~0 additional RAM. With RequiresModelInstalled set, the matcher
// must treat installedModels membership as sufficient and ignore the
// minRAMGB gate (which is sized for the cold-load path).
// Regression for defilantech/LLMKube#579.
func TestCapabilitySatisfies_RequiresModelInstalledBypassesRAMGate(t *testing.T) {
	req := foremanv1alpha1.RequiredCapability{
		Accelerator:            "metal",
		MinRAMGB:               8, // higher than the node's available RAM
		RequiresModelInstalled: true,
	}
	node := nodeWithCapability(foremanv1alpha1.FleetNodeCapability{
		Accelerator:     "metal",
		AvailableRAMGB:  7, // below MinRAMGB on purpose
		InstalledModels: []string{"devstral-24b"},
	})

	if !capabilitySatisfies(req, "devstral-24b", node, false) {
		t.Fatalf("expected match: model loaded on node, RAM gate should be bypassed")
	}
}

// RequiresModelInstalled with the required model absent from the node
// must NOT match: the whole point is to pin the task to the model's home.
func TestCapabilitySatisfies_RequiresModelInstalledButModelAbsent(t *testing.T) {
	req := foremanv1alpha1.RequiredCapability{
		Accelerator:            "metal",
		RequiresModelInstalled: true,
	}
	node := nodeWithCapability(foremanv1alpha1.FleetNodeCapability{
		Accelerator:     "metal",
		AvailableRAMGB:  64,
		InstalledModels: []string{"qwen3-coder-30b"},
	})

	if capabilitySatisfies(req, "devstral-24b", node, false) {
		t.Fatalf("expected no match: required model not in node's installedModels")
	}
}

// RequiresModelInstalled with no resolvable model name is a
// misconfiguration; the matcher cannot confirm residency, so it must
// not match rather than silently bypassing the gate.
func TestCapabilitySatisfies_RequiresModelInstalledWithEmptyModel(t *testing.T) {
	req := foremanv1alpha1.RequiredCapability{
		Accelerator:            "metal",
		RequiresModelInstalled: true,
	}
	node := nodeWithCapability(foremanv1alpha1.FleetNodeCapability{
		Accelerator:     "metal",
		AvailableRAMGB:  64,
		InstalledModels: []string{"devstral-24b"},
	})

	if capabilitySatisfies(req, "", node, false) {
		t.Fatalf("expected no match: no resolvable model to confirm residency")
	}
}

// Default path (RequiresModelInstalled unset) keeps enforcing minRAMGB.
func TestCapabilitySatisfies_DefaultStillEnforcesRAMGate(t *testing.T) {
	req := foremanv1alpha1.RequiredCapability{
		Accelerator: "metal",
		MinRAMGB:    32,
	}
	node := nodeWithCapability(foremanv1alpha1.FleetNodeCapability{
		Accelerator:    "metal",
		AvailableRAMGB: 16, // below MinRAMGB
	})

	if capabilitySatisfies(req, "", node, false) {
		t.Fatalf("expected no match: 16 < 32 and RequiresModelInstalled not set")
	}
}
