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

package fileset

import (
	"errors"
	"fmt"
	"strings"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// ConditionAvailable is the Model condition whose observedGeneration says
// which spec status.stagedFiles was recorded for. It mirrors
// internal/controller.ConditionAvailable, which a controller test pins.
const ConditionAvailable = "Available"

// ErrStagedFilesNotCurrent reports a glob in spec.files whose expansion the
// Model controller has not recorded for the current generation. It is the one
// staging error whose cause is not in spec.files, so the generated init
// container names the Model's status instead of blaming the file list.
var ErrStagedFilesNotCurrent = errors.New("staged files are not current for the Model's generation")

// HasMultiFileStaging reports whether the model uses multi-file staging via
// spec.files or spec.mmproj.
func HasMultiFileStaging(model *inferencev1alpha1.Model) bool {
	return model != nil && (len(model.Spec.Files) > 0 || model.Spec.Mmproj != "")
}

// AvailableObservedGeneration returns the generation the Model's Available
// condition was written for, and whether the condition exists.
func AvailableObservedGeneration(model *inferencev1alpha1.Model) (int64, bool) {
	for i := range model.Status.Conditions {
		if model.Status.Conditions[i].Type == ConditionAvailable {
			return model.Status.Conditions[i].ObservedGeneration, true
		}
	}
	return 0, false
}

// ModelPlan resolves the model's declared files into the staging plan the
// downloader runs. Returns nil when there is no multi-file staging. Returns an
// error when multi-file fields are set but resolution fails (fail-closed).
func ModelPlan(model *inferencev1alpha1.Model) (*StagingPlan, error) {
	if !HasMultiFileStaging(model) {
		return nil, nil
	}
	files, err := StagedEntries(model)
	if err != nil {
		return nil, fmt.Errorf("invalid fileset: %w", err)
	}
	plan, err := ResolveFileSet(files, model.Spec.Mmproj, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid fileset: %w", err)
	}
	if plan == nil || plan.Primary == "" {
		return nil, nil
	}
	return plan, nil
}

// StagedEntries returns the file entries to stage: spec.files as written,
// or, when it holds a glob, the expansion the Model controller recorded in
// status.stagedFiles. The expansion is trusted only for the current generation;
// one written for an older spec would stage the wrong set.
func StagedEntries(model *inferencev1alpha1.Model) ([]string, error) {
	if !FilesHaveGlob(model.Spec.Files) {
		return model.Spec.Files, nil
	}
	gen, ok := AvailableObservedGeneration(model)
	if len(model.Status.StagedFiles) == 0 || !ok || gen != model.Generation {
		return nil, fmt.Errorf("%w: spec.files has a glob and the Model has not recorded its expansion for generation %d",
			ErrStagedFilesNotCurrent, model.Generation)
	}
	entries := append([]string(nil), model.Status.StagedFiles...)
	for _, entry := range model.Spec.Files {
		if !strings.Contains(entry, "://") {
			continue
		}
		name, err := EntryPath(entry)
		if err != nil {
			return nil, err
		}
		for i, staged := range entries {
			if staged == name {
				entries[i] = entry
			}
		}
	}
	return entries, nil
}
