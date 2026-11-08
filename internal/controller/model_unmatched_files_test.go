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

package controller

import (
	"fmt"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestReconcile_UnmatchedFilePatterns(t *testing.T) {
	for _, prefetch := range []bool{false, true} {
		t.Run(fmt.Sprintf("prefetch=%v", prefetch), func(t *testing.T) {
			model := listingModel()
			model.Spec.SourceSecretRef = nil
			model.Spec.Prefetch = prefetch
			model.Spec.Files = []string{"config.json", "*.json", "*.nomatch", "weights-?.bin"}
			g := newListingRig(t, model)
			recorder := events.NewFakeRecorder(20)
			g.r.Recorder = recorder
			reconcile := func() {
				t.Helper()
				if _, err := g.r.Reconcile(g.ctx, glmRequest); err != nil {
					t.Fatal(err)
				}
			}
			reconcile()
			g.finish(0, mustEncode(t, repoListingResult{Files: []repoListedFile{{Path: "config.json", Size: 100}}}), false)
			reconcile()
			if prefetch {
				stored := getModel(t, g.r, model.Name)
				var job batchv1.Job
				if err := g.r.Get(g.ctx, types.NamespacedName{Name: prefetchJobName(stored), Namespace: model.Namespace}, &job); err != nil {
					t.Fatal(err)
				}
				if got := prefetchJobFiles(&job); strings.TrimSpace(got) != "config.json" {
					t.Fatalf("Job files = %q", got)
				}
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
				if err := g.r.Status().Update(g.ctx, &job); err != nil {
					t.Fatal(err)
				}
				reconcile()
			}
			for i := 0; i < 2; i++ {
				stored := getModel(t, g.r, model.Name)
				if stored.Status.Phase != PhaseReady || strings.Join(stored.Status.StagedFiles, " ") != "config.json" {
					t.Fatalf("phase = %q, staged files = %v; want Ready with config.json", stored.Status.Phase, stored.Status.StagedFiles)
				}
				c := meta.FindStatusCondition(stored.Status.Conditions, "UnmatchedFilePatterns")
				if c == nil || c.Status != metav1.ConditionTrue || c.ObservedGeneration != model.Generation || !strings.Contains(c.Message, "*.nomatch") || !strings.Contains(c.Message, "weights-?.bin") || strings.Contains(c.Message, "*.json") {
					t.Fatalf("unmatched condition = %+v; want both unmatched patterns, excluding matched *.json", c)
				}
				reconcile()
			}
			warnings := 0
			for len(recorder.Events) > 0 {
				event := <-recorder.Events
				if strings.Contains(event, "Warning UnmatchedFilePatterns") && strings.Contains(event, "*.nomatch") && strings.Contains(event, "weights-?.bin") {
					warnings++
				}
			}
			if warnings != 1 {
				t.Fatalf("unmatched warning events = %d; want 1 across repeated reconciles", warnings)
			}

			stored := getModel(t, g.r, model.Name)
			stored.Spec.Files = []string{"config.json"}
			stored.Generation++ // The fake client does not increment generation on spec updates.
			if err := g.r.Update(g.ctx, stored); err != nil {
				t.Fatal(err)
			}
			reconcile()
			stored = getModel(t, g.r, model.Name)
			if c := meta.FindStatusCondition(stored.Status.Conditions, "UnmatchedFilePatterns"); c != nil {
				t.Fatalf("warning remains after correcting spec.files: %+v", c)
			}
		})
	}
}

func TestReconcile_UnmatchedFilePatternsOCI(t *testing.T) {
	model := listingModel()
	model.Spec.SourceSecretRef = nil
	model.Spec.Prefetch = true
	model.Spec.PrefetchOCI = &inferencev1alpha1.OCIPrefetchSpec{Image: "registry.example.com/models/test:v1", EphemeralStorage: "2Gi", PushSecretRef: &corev1.LocalObjectReference{Name: "registry"}}
	model.Spec.Files = []string{"config.json", "*.nomatch"}
	g := newListingRig(t, model)
	g.r.OCIPublisherImage = "operator:test"
	recorder := events.NewFakeRecorder(10)
	g.r.Recorder = recorder
	if _, err := g.r.Reconcile(g.ctx, glmRequest); err != nil {
		t.Fatal(err)
	}
	g.finish(0, mustEncode(t, repoListingResult{Files: []repoListedFile{{Path: "config.json"}}}), false)
	if _, err := g.r.Reconcile(g.ctx, glmRequest); err != nil {
		t.Fatal(err)
	}
	stored := getModel(t, g.r, model.Name)
	c := meta.FindStatusCondition(stored.Status.Conditions, "UnmatchedFilePatterns")
	if c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "*.nomatch") {
		t.Fatalf("unmatched condition = %+v", c)
	}
	var job batchv1.Job
	if err := g.r.Get(g.ctx, types.NamespacedName{Name: prefetchJobNameFor(model.Name, "-prefetch-oci"), Namespace: model.Namespace}, &job); err != nil {
		t.Fatal(err)
	}
	if got := job.Annotations[annotationOCIFiles]; got != `["config.json"]` {
		t.Fatalf("OCI Job files = %q", got)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Warning UnmatchedFilePatterns") || !strings.Contains(event, "*.nomatch") {
			t.Fatalf("event = %q", event)
		}
	default:
		t.Fatal("missing unmatched-pattern warning event")
	}
}
