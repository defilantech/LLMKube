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

package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DrainReapable gates deletion of orphaned Draining FleetNodes. A live agent
// keeps heart-beating while it drains, so reaping only fires once the node has
// been silent past FleetNodeDrainReapTimeout. The nil-heartbeat branch falls
// back to creationTimestamp so a hand-crafted object still gets the grace
// window instead of being reaped on first sight.
func TestDrainReapable(t *testing.T) {
	now := time.Now()
	old := metav1.NewTime(now.Add(-2 * FleetNodeDrainReapTimeout))
	recent := metav1.NewTime(now.Add(-1 * time.Second))

	tests := []struct {
		name      string
		heartbeat *metav1.Time
		created   metav1.Time
		want      bool
	}{
		{"fresh heartbeat is not reapable", &recent, metav1.NewTime(now), false},
		{"stale heartbeat past the window is reapable", &old, metav1.NewTime(now), true},
		{"nil heartbeat with recent creation is not reapable", nil, recent, false},
		{"nil heartbeat with old creation is reapable", nil, old, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := &FleetNode{}
			n.CreationTimestamp = tc.created
			n.Status.LastHeartbeatTime = tc.heartbeat
			if got := n.DrainReapable(now); got != tc.want {
				t.Fatalf("DrainReapable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A nil receiver is never reapable (defensive; the reconciler always has a
// live object).
func TestDrainReapableNilReceiver(t *testing.T) {
	var n *FleetNode
	if n.DrainReapable(time.Now()) {
		t.Fatalf("nil FleetNode must not be reapable")
	}
}
