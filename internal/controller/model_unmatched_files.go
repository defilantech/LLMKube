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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const ConditionUnmatchedFilePatterns = "UnmatchedFilePatterns"

// Use the resolved entries so cached expansions and listing-pod results need no new listing.
func (r *ModelReconciler) setUnmatchedFilePatterns(model *inferencev1alpha1.Model, plan *StagingPlan) {
	var unmatched []string
	if plan != nil {
		entries := plan.Entries()
		for _, pattern := range model.Spec.Files {
			if !hasGlob(pattern) || strings.Contains(pattern, "://") {
				continue
			}
			matches, err := expandFilePattern(pattern, entries)
			if err == nil && len(matches) == 0 {
				unmatched = append(unmatched, pattern)
			}
		}
	}
	if len(unmatched) == 0 {
		meta.RemoveStatusCondition(&model.Status.Conditions, ConditionUnmatchedFilePatterns)
		return
	}
	message := fmt.Sprintf("spec.files patterns matched no repository files: %q; unmatched patterns are skipped", unmatched)
	changed := meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type:               ConditionUnmatchedFilePatterns,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: model.Generation,
		Reason:             "NoMatchingFiles",
		Message:            message,
	})
	if changed && r.Recorder != nil {
		r.Recorder.Eventf(model, nil, corev1.EventTypeWarning, ConditionUnmatchedFilePatterns, "ResolveFiles", "%s", message)
	}
}
