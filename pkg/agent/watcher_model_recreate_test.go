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
	"maps"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// withClock gives the fixture's watcher a fake clock and returns a function
// that moves it forward.
func (f *modelLookupFixture) withClock() func(time.Duration) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	f.watcher.now = func() time.Time { return now }
	return func(d time.Duration) { now = now.Add(d) }
}

// deleteModel deletes the fixture's Model through the base client.
func (f *modelLookupFixture) deleteModel(t *testing.T) {
	t.Helper()
	if err := f.base.Delete(context.Background(), &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: lookupModelName, Namespace: "default"},
	}); err != nil {
		t.Fatalf("delete model: %v", err)
	}
}

// createModel creates the fixture's Model again with source, as a GitOps
// re-create or kubectl replace --force does: a new object with a new UID.
func (f *modelLookupFixture) createModel(t *testing.T, source string) {
	t.Helper()
	if err := f.base.Create(context.Background(), &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: lookupModelName, Namespace: "default", UID: "recreated"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: source, Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}); err != nil {
		t.Fatalf("create model: %v", err)
	}
}

const fixtureModelSource = "https://example.com/lookup.gguf"

// TestPoll_ModelDeletedAndRecreatedKeepsService is #2411: deleting and
// re-creating a Model must not stop the engine of the InferenceService that
// references it. While the Model is missing the watcher emits nothing and
// keeps the service's seen entry; once the same Model is back, nothing
// changes.
func TestPoll_ModelDeletedAndRecreatedKeepsService(t *testing.T) {
	f := newModelLookupFixture(t)
	advance := f.withClock()
	seen := map[string]string{}
	if events := f.pollOnce(t, seen); len(events) != 1 || events[0].Type != EventTypeCreated {
		t.Fatalf("poll 1 events = %v, want one CREATED", eventSummary(events))
	}
	before := maps.Clone(seen)

	f.deleteModel(t)
	if events := f.pollOnce(t, seen); len(events) != 0 {
		t.Fatalf("poll 2 (model deleted) events = %v, want none: the engine must keep running", eventSummary(events))
	}
	advance(time.Minute)
	if events := f.pollOnce(t, seen); len(events) != 0 {
		t.Fatalf("poll 3 (model still missing, inside the grace) events = %v, want none", eventSummary(events))
	}
	if !maps.Equal(seen, before) {
		t.Errorf("seen = %v while the model was missing, want %v", seen, before)
	}

	f.createModel(t, fixtureModelSource)
	if events := f.pollOnce(t, seen); len(events) != 0 {
		t.Errorf("poll 4 (same model re-created) events = %v, want none", eventSummary(events))
	}

	// A later deletion starts a new grace, not the old one's remainder.
	advance(2 * defaultModelGoneGrace)
	f.deleteModel(t)
	if events := f.pollOnce(t, seen); len(events) != 0 {
		t.Errorf("poll 5 (model deleted again much later) events = %v, want none", eventSummary(events))
	}
}

// TestPoll_ModelGoneReleasedAfterGrace: a Model that stays deleted releases
// the service once the grace has passed, as it did at once before #2411.
func TestPoll_ModelGoneReleasedAfterGrace(t *testing.T) {
	f := newModelLookupFixture(t)
	advance := f.withClock()
	seen := map[string]string{}
	f.pollOnce(t, seen)

	f.deleteModel(t)
	if events := f.pollOnce(t, seen); len(events) != 0 {
		t.Fatalf("poll 2 events = %v, want none", eventSummary(events))
	}
	advance(defaultModelGoneGrace - time.Second)
	if events := f.pollOnce(t, seen); len(events) != 0 {
		t.Fatalf("poll 3 (a second before the grace ends) events = %v, want none", eventSummary(events))
	}
	advance(time.Second)
	events := f.pollOnce(t, seen)
	if len(events) != 1 || events[0].Type != EventTypeDeleted || events[0].InferenceService.Name != lookupISVCName {
		t.Fatalf("poll 4 (grace over) events = %v, want one DELETED default/%s", eventSummary(events), lookupISVCName)
	}
	if len(seen) != 0 {
		t.Errorf("seen = %v after the release, want empty", seen)
	}
	// The watcher keeps nothing for a released service, so its records do
	// not grow with every Model ever deleted.
	if len(f.watcher.modelGone) != 0 || len(f.watcher.modelSeen) != 0 {
		t.Errorf("after the release modelGone = %v, modelSeen = %v; want both empty",
			f.watcher.modelGone, f.watcher.modelSeen)
	}

	// Re-created after the release, the service is new again.
	f.createModel(t, fixtureModelSource)
	if events := f.pollOnce(t, seen); len(events) != 1 || events[0].Type != EventTypeCreated {
		t.Errorf("poll 5 (model back after the release) events = %v, want one CREATED", eventSummary(events))
	}
}

// TestPoll_ModelRecreatedWithNewSpecRestarts keeps what a re-create did
// before #2411 when the Model changed: the engine restarts with the new
// Model, whether the re-create took several polls or happened between two.
func TestPoll_ModelRecreatedWithNewSpecRestarts(t *testing.T) {
	for _, gap := range []bool{true, false} {
		name := "re-created between two polls"
		if gap {
			name = "missing for a poll"
		}
		t.Run(name, func(t *testing.T) {
			f := newModelLookupFixture(t)
			f.withClock()
			seen := map[string]string{}
			f.pollOnce(t, seen)

			f.deleteModel(t)
			if gap {
				if events := f.pollOnce(t, seen); len(events) != 0 {
					t.Fatalf("poll 2 (model deleted) events = %v, want none", eventSummary(events))
				}
			}
			f.createModel(t, "https://example.com/other.gguf")

			events := f.pollOnce(t, seen)
			got := eventSummary(events)
			if len(events) != 2 || events[0].Type != EventTypeDeleted || events[1].Type != EventTypeCreated {
				t.Fatalf("events after the changed re-create = %v, want DELETED then CREATED", got)
			}
			if _, ok := seen[watchKey(events[1].InferenceService)]; !ok {
				t.Errorf("seen = %v, want the service tracked again", seen)
			}
			if events := f.pollOnce(t, seen); len(events) != 0 {
				t.Errorf("next poll events = %v, want none", eventSummary(events))
			}
		})
	}
}

// TestPoll_ModelEditedInPlaceDoesNotRestart: an in-place Model edit keeps the
// engine, as before #2411; only a re-created Model with a new spec restarts
// it.
func TestPoll_ModelEditedInPlaceDoesNotRestart(t *testing.T) {
	f := newModelLookupFixture(t)
	seen := map[string]string{}
	f.pollOnce(t, seen)

	ctx := context.Background()
	model := &inferencev1alpha1.Model{}
	if err := f.base.Get(ctx, client.ObjectKey{Namespace: "default", Name: lookupModelName}, model); err != nil {
		t.Fatal(err)
	}
	model.Spec.Source = "https://example.com/edited.gguf"
	if err := f.base.Update(ctx, model); err != nil {
		t.Fatal(err)
	}
	if events := f.pollOnce(t, seen); len(events) != 0 {
		t.Errorf("events after an in-place Model edit = %v, want none", eventSummary(events))
	}
}
