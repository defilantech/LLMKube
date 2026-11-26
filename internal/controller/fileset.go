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
	"path"

	"github.com/defilantech/llmkube/pkg/fileset"
)

// File-set resolution lives in pkg/fileset so `llmkube cache prune` resolves a
// Model's files with the code staging runs (#2170). These names keep the
// controller's call sites unchanged.

// StagingPlan describes the resolved set of model files to stage from a source.
type StagingPlan = fileset.StagingPlan

// ResolveFileSet turns declared spec.files and spec.mmproj into a staging plan.
func ResolveFileSet(files []string, mmproj string, repoFileList []string) (*StagingPlan, error) {
	return fileset.ResolveFileSet(files, mmproj, repoFileList)
}

// stagedCachePath returns the absolute cache path for a repo-relative file.
func stagedCachePath(modelDir string, repoRelative string) string {
	return path.Join(modelDir, repoRelative)
}

func filesHaveGlob(files []string) bool {
	return fileset.FilesHaveGlob(files)
}

// fileEntryPath preserves HF repository paths and S3 keys; other HTTP files
// use their basename.
func fileEntryPath(entry string) (string, error) {
	return fileset.EntryPath(entry)
}

func hasGlob(value string) bool {
	return fileset.HasGlob(value)
}

func validateRepoRelativePath(p string) error {
	return fileset.ValidateRepoRelativePath(p)
}

func expandFilePattern(entry string, repoFileList []string) ([]string, error) {
	return fileset.ExpandFilePattern(entry, repoFileList)
}
