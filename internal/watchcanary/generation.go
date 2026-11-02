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

package watchcanary

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/metrics"
)

// The kind label values of the generation-lag gauges.
const (
	KindInferenceService = "InferenceService"
	KindModel            = "Model"
)

// GenerationLagOptions configures a GenerationLagChecker.
type GenerationLagOptions struct {
	// Interval is how often the checker lists each kind. Must be positive.
	Interval time.Duration
	// Threshold is how long an object's generation may run ahead of its
	// conditions before it counts. Must be positive; keep it above the
	// operator's --reconcile-timeout so a reconcile that runs to its deadline
	// does not count.
	Threshold time.Duration
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// Log receives the checker's lifecycle and per-object lag logs.
	Log logr.Logger
}

// APIReaderSource is the part of a controller-runtime manager the checker
// needs: a reader that goes to the API server, never the manager's cache.
type APIReaderSource interface {
	GetAPIReader() client.Reader
}

// GenerationLagChecker reports InferenceServices and Models whose spec change
// has gone unreconciled: metadata.generation ahead of the highest
// observedGeneration on their status conditions (#2308). The watch canary
// proves the Lease informer is alive, but informers are per resource type, so
// one kind's watch can die alone (kubernetes-sigs/controller-runtime#3428).
// It is an alert source only: a stuck object is not always a dead watch (a
// reconcile error retried forever looks the same), so nothing here restarts
// the operator.
type GenerationLagChecker struct {
	opts   GenerationLagOptions
	log    logr.Logger
	reader client.Reader

	mu sync.Mutex
	// lagging holds, per kind, when each lagging object was first seen
	// lagging. An entry leaves when the object catches up or is gone.
	lagging map[string]map[types.UID]*lagEntry
}

type lagEntry struct {
	since    time.Time
	reported bool
}

// lagObject pairs an object with its status conditions, which the typed
// lists hold in different structs.
type lagObject struct {
	obj        client.Object
	conditions []metav1.Condition
}

type lagKind struct {
	name string
	list func(ctx context.Context, r client.Reader) ([]lagObject, error)
}

var lagKinds = []lagKind{
	{name: KindInferenceService, list: listInferenceServices},
	{name: KindModel, list: listModels},
}

// NewGenerationLagChecker validates the options and builds a checker that
// lists through mgr's API reader. The reader is chosen here, not by the
// caller, because it is the point of the check: a dead informer's cache is
// frozen at the generation it last delivered, so a cached List would show the
// object current and the lag would hide itself.
func NewGenerationLagChecker(mgr APIReaderSource, o GenerationLagOptions) (*GenerationLagChecker, error) {
	if mgr == nil {
		return nil, fmt.Errorf("unreconciled-generation checker needs the manager's API reader")
	}
	return newGenerationLagChecker(mgr.GetAPIReader(), o)
}

func newGenerationLagChecker(reader client.Reader, o GenerationLagOptions) (*GenerationLagChecker, error) {
	if o.Interval <= 0 {
		return nil, fmt.Errorf("unreconciled-generation interval must be positive, got %s", o.Interval)
	}
	if o.Threshold <= 0 {
		return nil, fmt.Errorf("unreconciled-generation threshold must be positive, got %s", o.Threshold)
	}
	if reader == nil {
		return nil, fmt.Errorf("unreconciled-generation checker needs a live reader")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	log := o.Log
	if log.GetSink() == nil {
		log = logf.Log.WithName("unreconciled-generation")
	}
	return &GenerationLagChecker{
		opts:    o,
		log:     log,
		reader:  reader,
		lagging: map[string]map[types.UID]*lagEntry{},
	}, nil
}

// NeedLeaderElection keeps the checker on the leader: the leader is the pod
// whose informers drive the controllers, and one List per kind per interval
// is the whole cost.
func (c *GenerationLagChecker) NeedLeaderElection() bool { return true }

// Start runs a pass immediately and then every Interval until ctx ends.
func (c *GenerationLagChecker) Start(ctx context.Context) error {
	c.log.Info("unreconciled-generation checker started",
		"interval", c.opts.Interval.String(), "threshold", c.opts.Threshold.String())
	c.check(ctx, c.opts.Now())
	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.check(ctx, c.opts.Now())
		}
	}
}

// check runs one pass over every kind. A kind whose List fails keeps its
// tracking and its last published values: an API-server error says nothing
// about whether that kind's objects caught up.
func (c *GenerationLagChecker) check(ctx context.Context, now time.Time) {
	for _, k := range lagKinds {
		objs, err := k.list(ctx, c.reader)
		if err != nil {
			c.log.Error(err, "unreconciled-generation list failed; keeping the previous values", "kind", k.name)
			continue
		}
		objects, oldest := c.observe(k.name, objs, now)
		metrics.SetUnreconciledGeneration(k.name, objects, oldest)
	}
}

// observe folds one List of a kind into the tracking and returns the number
// of objects lagging longer than the threshold and the oldest lag's age.
func (c *GenerationLagChecker) observe(kind string, objs []lagObject, now time.Time) (int, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tracked := c.lagging[kind]
	if tracked == nil {
		tracked = map[types.UID]*lagEntry{}
		c.lagging[kind] = tracked
	}

	seen := make(map[types.UID]struct{}, len(objs))
	objects := 0
	var oldest time.Duration
	for i := range objs {
		o := objs[i]
		if !GenerationUnreconciled(o.obj, o.conditions) {
			continue
		}
		uid := o.obj.GetUID()
		seen[uid] = struct{}{}
		entry, ok := tracked[uid]
		if !ok {
			entry = &lagEntry{since: now}
			tracked[uid] = entry
		}
		age := now.Sub(entry.since)
		if age > oldest {
			oldest = age
		}
		if age <= c.opts.Threshold {
			continue
		}
		objects++
		if !entry.reported {
			entry.reported = true
			c.log.Info("spec change unreconciled past the threshold; the controller for this kind may have stopped receiving events",
				"kind", kind, "namespace", o.obj.GetNamespace(), "name", o.obj.GetName(),
				"generation", o.obj.GetGeneration(), "observedGeneration", MaxObservedGeneration(o.conditions),
				"lagging", age.Truncate(time.Second).String(), "threshold", c.opts.Threshold.String())
		}
	}
	// Caught up, being deleted, or gone: stop tracking. A later lag on the
	// same object starts a new clock.
	for uid := range tracked {
		if _, ok := seen[uid]; !ok {
			delete(tracked, uid)
		}
	}
	return objects, oldest
}

// MaxObservedGeneration returns the highest observedGeneration across
// conditions, 0 when there are none. The highest, not the first or the
// Available one: the controllers stamp the conditions each path writes, and a
// condition no current path touches (a GPUAvailable left from an earlier
// pass) keeps an old generation while the object is fully reconciled.
func MaxObservedGeneration(conditions []metav1.Condition) int64 {
	var maxGen int64
	for i := range conditions {
		if conditions[i].ObservedGeneration > maxGen {
			maxGen = conditions[i].ObservedGeneration
		}
	}
	return maxGen
}

// GenerationUnreconciled reports whether obj's spec generation is ahead of
// every condition the controller stamped. An object being deleted never
// counts: its generation during finalization is not something the
// controllers promise to stamp.
func GenerationUnreconciled(obj client.Object, conditions []metav1.Condition) bool {
	if obj.GetDeletionTimestamp() != nil {
		return false
	}
	return obj.GetGeneration() > MaxObservedGeneration(conditions)
}

func listInferenceServices(ctx context.Context, r client.Reader) ([]lagObject, error) {
	list := &inferencev1alpha1.InferenceServiceList{}
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	out := make([]lagObject, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, lagObject{obj: &list.Items[i], conditions: list.Items[i].Status.Conditions})
	}
	return out, nil
}

func listModels(ctx context.Context, r client.Reader) ([]lagObject, error) {
	list := &inferencev1alpha1.ModelList{}
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	out := make([]lagObject, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, lagObject{obj: &list.Items[i], conditions: list.Items[i].Status.Conditions})
	}
	return out, nil
}
