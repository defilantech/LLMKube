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
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/metrics"
)

func cond(t string, gen int64) metav1.Condition {
	return metav1.Condition{Type: t, Status: metav1.ConditionTrue, Reason: "Test", ObservedGeneration: gen}
}

func isvcAt(name string, uid types.UID, gen int64, conds ...metav1.Condition) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: uid, Generation: gen},
		Status:     inferencev1alpha1.InferenceServiceStatus{Conditions: conds},
	}
}

func lagOf(o *inferencev1alpha1.InferenceService) lagObject {
	return lagObject{obj: o, conditions: o.Status.Conditions}
}

func testLagChecker(t *testing.T, reader client.Reader, threshold time.Duration) *GenerationLagChecker {
	t.Helper()
	c, err := newGenerationLagChecker(reader, GenerationLagOptions{
		Interval:  time.Minute,
		Threshold: threshold,
		Log:       logr.Discard(),
	})
	if err != nil {
		t.Fatalf("new checker: %v", err)
	}
	return c
}

// TestGenerationUnreconciled pins the lag rule: generation ahead of the
// HIGHEST observedGeneration across conditions. The stale-secondary case is
// the issue's falsification: reading the first condition (an old GPUAvailable
// the current path no longer touches) reports a fully reconciled object as
// lagging.
func TestGenerationUnreconciled(t *testing.T) {
	deleting := metav1.Now()
	cases := []struct {
		name string
		obj  *inferencev1alpha1.InferenceService
		want bool
	}{
		{
			name: "every condition at the current generation",
			obj:  isvcAt("a", "a", 3, cond("Available", 3), cond("Suspended", 3)),
			want: false,
		},
		{
			name: "stale secondary condition first, current Available after it",
			obj:  isvcAt("a", "a", 2, cond("GPUAvailable", 1), cond("Available", 2)),
			want: false,
		},
		{
			name: "every condition behind the generation",
			obj:  isvcAt("a", "a", 4, cond("Available", 3), cond("Suspended", 2)),
			want: true,
		},
		{
			name: "no conditions at all on a created object",
			obj:  isvcAt("a", "a", 1),
			want: true,
		},
		{
			name: "being deleted is skipped even when behind",
			obj: func() *inferencev1alpha1.InferenceService {
				o := isvcAt("a", "a", 5, cond("Available", 1))
				o.DeletionTimestamp = &deleting
				return o
			}(),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GenerationUnreconciled(tc.obj, tc.obj.Status.Conditions); got != tc.want {
				t.Fatalf("GenerationUnreconciled = %v, want %v (generation %d, max observed %d)",
					got, tc.want, tc.obj.Generation, MaxObservedGeneration(tc.obj.Status.Conditions))
			}
		})
	}
}

// TestObserveTracksLagPerObject drives the first-seen clock: an object counts
// only once its lag has lasted longer than the threshold, the oldest age is
// reported from the first sighting, catching up clears it, and a later lag on
// the same object starts a new clock rather than inheriting the old one.
func TestObserveTracksLagPerObject(t *testing.T) {
	const threshold = 20 * time.Minute
	c := testLagChecker(t, fake.NewClientBuilder().Build(), threshold)
	t0 := time.Unix(1750000000, 0)

	lagging := isvcAt("lagging", "uid-lag", 2, cond("Available", 1))
	healthy := isvcAt("healthy", "uid-ok", 2, cond("Available", 2))

	steps := []struct {
		at         time.Duration
		objs       []lagObject
		wantCount  int
		wantOldest time.Duration
	}{
		// First sighting: lagging, but for zero seconds.
		{0, []lagObject{lagOf(lagging), lagOf(healthy)}, 0, 0},
		// Under the threshold: reported as an age, not counted.
		{10 * time.Minute, []lagObject{lagOf(lagging), lagOf(healthy)}, 0, 10 * time.Minute},
		// Exactly at the threshold is not "longer than" it.
		{threshold, []lagObject{lagOf(lagging), lagOf(healthy)}, 0, threshold},
		// Past it: counted.
		{threshold + time.Minute, []lagObject{lagOf(lagging), lagOf(healthy)}, 1, threshold + time.Minute},
	}
	for _, s := range steps {
		count, oldest := c.observe(KindInferenceService, s.objs, t0.Add(s.at))
		if count != s.wantCount || oldest != s.wantOldest {
			t.Fatalf("at +%s: got (%d, %s), want (%d, %s)", s.at, count, oldest, s.wantCount, s.wantOldest)
		}
	}

	// Caught up: the controller stamped the current generation.
	caughtUp := isvcAt("lagging", "uid-lag", 2, cond("Available", 2))
	if count, oldest := c.observe(KindInferenceService, []lagObject{lagOf(caughtUp)}, t0.Add(30*time.Minute)); count != 0 || oldest != 0 {
		t.Fatalf("after catch-up: got (%d, %s), want (0, 0s)", count, oldest)
	}
	if n := len(c.lagging[KindInferenceService]); n != 0 {
		t.Fatalf("expected the caught-up entry dropped, %d remain", n)
	}

	// A new spec change later: the clock starts again from this sighting.
	again := isvcAt("lagging", "uid-lag", 3, cond("Available", 2))
	if count, oldest := c.observe(KindInferenceService, []lagObject{lagOf(again)}, t0.Add(40*time.Minute)); count != 0 || oldest != 0 {
		t.Fatalf("new lag after catch-up: got (%d, %s), want (0, 0s)", count, oldest)
	}
}

// TestObserveDropsDeletedObjects: an object that disappears from the List (or
// starts terminating) stops counting and leaves no entry behind, so a
// recreated object with the same name starts clean.
func TestObserveDropsDeletedObjects(t *testing.T) {
	c := testLagChecker(t, fake.NewClientBuilder().Build(), time.Minute)
	t0 := time.Unix(1750000000, 0)
	lagging := isvcAt("gone", "uid-gone", 2, cond("Available", 1))

	if count, _ := c.observe(KindInferenceService, []lagObject{lagOf(lagging)}, t0); count != 0 {
		t.Fatalf("first sighting counted: %d", count)
	}
	if count, _ := c.observe(KindInferenceService, []lagObject{lagOf(lagging)}, t0.Add(5*time.Minute)); count != 1 {
		t.Fatalf("expected the lag counted past the threshold, got %d", count)
	}

	terminating := lagging.DeepCopy()
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	if count, oldest := c.observe(KindInferenceService, []lagObject{lagOf(terminating)}, t0.Add(6*time.Minute)); count != 0 || oldest != 0 {
		t.Fatalf("terminating object still counted: (%d, %s)", count, oldest)
	}
	if count, oldest := c.observe(KindInferenceService, nil, t0.Add(7*time.Minute)); count != 0 || oldest != 0 {
		t.Fatalf("deleted object still counted: (%d, %s)", count, oldest)
	}
	if n := len(c.lagging[KindInferenceService]); n != 0 {
		t.Fatalf("expected no tracked entries after deletion, %d remain", n)
	}
}

// TestCheckPublishesPerKindGauges runs a full pass through a reader and reads
// the exported gauges, per kind. A failed List leaves that kind's previous
// values and tracking alone instead of reading as "caught up".
func TestCheckPublishesPerKindGauges(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	isvc := isvcAt("lagging-isvc", "uid-isvc", 2, cond("Available", 1))
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "current-model", UID: "uid-model", Generation: 4},
		Status:     inferencev1alpha1.ModelStatus{Conditions: []metav1.Condition{cond("Available", 4)}},
	}
	failModels := false
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(isvc, model).
		WithStatusSubresource(isvc, model).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*inferencev1alpha1.ModelList); ok && failModels {
					return errors.New("injected list failure")
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	c := testLagChecker(t, reader, 10*time.Minute)
	t0 := time.Unix(1750000000, 0)
	ctx := context.Background()

	c.check(ctx, t0)
	c.check(ctx, t0.Add(11*time.Minute))
	if got := testutil.ToFloat64(metrics.UnreconciledGenerationObjects.WithLabelValues(KindInferenceService)); got != 1 {
		t.Fatalf("InferenceService objects = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.UnreconciledGenerationOldestSeconds.WithLabelValues(KindInferenceService)); got != 660 {
		t.Fatalf("InferenceService oldest = %v, want 660", got)
	}
	if got := testutil.ToFloat64(metrics.UnreconciledGenerationObjects.WithLabelValues(KindModel)); got != 0 {
		t.Fatalf("Model objects = %v, want 0 for a Model reconciled at its generation", got)
	}

	// Put a Model behind, record one sighting, then fail its List: the
	// previous Model values stand and the first-seen clock is kept.
	stale := &inferencev1alpha1.Model{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(model), stale); err != nil {
		t.Fatalf("get model: %v", err)
	}
	stale.Status.Conditions = []metav1.Condition{cond("Available", 3)}
	if err := reader.Status().Update(ctx, stale); err != nil {
		t.Fatalf("make model stale: %v", err)
	}
	c.check(ctx, t0.Add(12*time.Minute))
	failModels = true
	c.check(ctx, t0.Add(30*time.Minute))
	if got := testutil.ToFloat64(metrics.UnreconciledGenerationOldestSeconds.WithLabelValues(KindModel)); got != 0 {
		t.Fatalf("Model oldest moved on a failed List: %v", got)
	}
	failModels = false
	c.check(ctx, t0.Add(31*time.Minute))
	if got := testutil.ToFloat64(metrics.UnreconciledGenerationObjects.WithLabelValues(KindModel)); got != 1 {
		t.Fatalf("Model objects = %v, want 1: the clock from before the failed List must survive it", got)
	}
}

func TestNewGenerationLagCheckerValidates(t *testing.T) {
	reader := fake.NewClientBuilder().Build()
	cases := []struct {
		name string
		o    GenerationLagOptions
	}{
		{"zero interval", GenerationLagOptions{Interval: 0, Threshold: time.Minute}},
		{"zero threshold", GenerationLagOptions{Interval: time.Minute, Threshold: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newGenerationLagChecker(reader, tc.o); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
	if _, err := NewGenerationLagChecker(nil, GenerationLagOptions{Interval: time.Minute, Threshold: time.Minute}); err == nil {
		t.Fatal("expected an error without an API reader source")
	}
	c, err := newGenerationLagChecker(reader, GenerationLagOptions{Interval: time.Minute, Threshold: time.Minute})
	if err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	if !c.NeedLeaderElection() {
		t.Fatal("expected the checker to need leader election")
	}
}
