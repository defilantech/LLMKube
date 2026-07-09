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

package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// defaultReaperInterval is the sweep cadence used when Reaper.Interval
// is left at its zero value. Once an hour is plenty for a record-count
// bounded by CreationTimestamp — the reaper runs through the cache and
// is purely cosmetic in the hot path.
const defaultReaperInterval = time.Hour

// Sweep deletes audit ConfigMaps whose CreationTimestamp is older than
// retention. Returns the count deleted. retention <= 0 is a no-op
// (returns 0, nil) so operators can disable the reaper via
// --audit-retention=0 without redeploying with the feature stripped.
//
// Audit CMs are intentionally owner-unbound — see writer.go. The
// record must outlive the AgenticTask to remain a compliance trail
// after task GC. That design choice leaves them to accumulate
// unbounded in the task namespace over time without this sweep; see
// defilantech/LLMKube#990.
func Sweep(ctx context.Context, c client.Client, retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	var list corev1.ConfigMapList
	// Cluster-wide: audit CMs land in the task's own namespace (or in
	// AuditNamespace, if the operator was configured with one), which
	// the reaper has no upfront knowledge of. The audit-label selector
	// keeps the blast radius to CMs the reaper actually owns.
	//
	// The List is intentionally NOT paginated: the label filter scopes
	// to audit CMs only and the reaper itself self-bounds the count
	// by deleting the aged-out ones each tick, so the un-paginated
	// result stays small in practice. If audit volume ever grows past
	// the apiserver's --max-objects-per-list (default 500) this single
	// call would start to truncate; if that ever happens, switch to
	// client.List with a Continue loop and a per-page list limit.
	if err := c.List(ctx, &list, client.MatchingLabels{AuditLabel: "true"}); err != nil {
		return 0, fmt.Errorf("audit reaper: list: %w", err)
	}
	cutoff := time.Now().Add(-retention)
	deleted := 0
	for i := range list.Items {
		cm := &list.Items[i]
		if cm.CreationTimestamp.Time.After(cutoff) {
			continue
		}
		if err := c.Delete(ctx, cm); err != nil {
			if apierrors.IsNotFound(err) {
				// Race with another replica or a manual delete; benign.
				continue
			}
			return deleted, fmt.Errorf("audit reaper: delete %s/%s: %w",
				cm.Namespace, cm.Name, err)
		}
		deleted++
	}
	return deleted, nil
}

// Reaper runs Sweep on a periodic ticker. Wire via manager.Add so it
// obeys leader election when the operator is deployed with replicas>1:
//
//	mgr.Add(&audit.Reaper{
//	    Client:    mgr.GetClient(),
//	    Retention: 7 * 24 * time.Hour,  // 7 days; 0 disables
//	    Interval:  time.Hour,            // optional; defaults to 1h
//	})
//
// With --leader-elect enabled, controller-runtime only starts the
// reaper on the elected leader, so a HA deployment cannot double-sweep.
// Retention 0 disables the reaper entirely (Start blocks on ctx.Done()
// and returns nil without running Sweep).
type Reaper struct {
	Client    client.Client
	Retention time.Duration
	Interval  time.Duration
	Log       logr.Logger
}

// NeedLeaderElection makes the reaper a leader-election-aware runnable
// (manager.LeaderElectionRunnable): when the operator is deployed with
// --leader-elect=true the reaper only runs on the elected replica. A
// standalone (single-replica) deployment has no leader; the reaper
// runs there regardless. Either way, this matches the intent: only
// one reconciler per cluster should be pruning audit records.
func (r *Reaper) NeedLeaderElection() bool { return true }

// Start runs the periodic sweep until ctx is cancelled, then returns
// nil. A failed sweep is logged and the loop continues (one transient
// apiserver blip does not stop the reaper permanently).
func (r *Reaper) Start(ctx context.Context) error {
	if r.Retention <= 0 {
		<-ctx.Done()
		return nil
	}
	interval := r.Interval
	if interval <= 0 {
		interval = defaultReaperInterval
	}
	log := r.Log
	if log.IsZero() {
		log = logr.Discard()
	}
	// Tick immediately on start so a fresh install with a backlog of
	// stale audit CMs does not wait an hour for the first cleanup
	// pass.
	if deleted, err := Sweep(ctx, r.Client, r.Retention); err != nil {
		log.Error(err, "audit reaper sweep failed (initial)")
	} else if deleted > 0 {
		log.Info("audit reaper swept old records (initial)",
			"deleted", deleted, "retention", r.Retention.String())
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if deleted, err := Sweep(ctx, r.Client, r.Retention); err != nil {
				log.Error(err, "audit reaper sweep failed")
			} else if deleted > 0 {
				log.Info("audit reaper swept old records",
					"deleted", deleted, "retention", r.Retention.String())
			}
		}
	}
}
