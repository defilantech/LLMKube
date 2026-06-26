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
	"reflect"
	"testing"
)

func TestResolveFileSet(t *testing.T) {
	cases := []struct {
		name      string
		files     []string
		mmproj    string
		repoFiles []string
		want      *StagingPlan
		wantErr   bool
	}{
		{name: "empty returns nil"},
		{
			name:      "single explicit file",
			files:     []string{"model.gguf"},
			repoFiles: []string{"model.gguf"},
			want:      &StagingPlan{Primary: "model.gguf", Files: []string{"model.gguf"}},
		},
		{
			name:      "subdirectory preserved",
			files:     []string{"MTP/weights.gguf"},
			repoFiles: []string{"MTP/weights.gguf"},
			want:      &StagingPlan{Primary: "MTP/weights.gguf", Files: []string{"MTP/weights.gguf"}},
		},
		{
			name:      "mmproj appended to union",
			files:     []string{"model.gguf"},
			mmproj:    "mmproj-F16.gguf",
			repoFiles: []string{"model.gguf", "mmproj-F16.gguf"},
			want:      &StagingPlan{Primary: "model.gguf", Files: []string{"model.gguf", "mmproj-F16.gguf"}, Mmproj: "mmproj-F16.gguf"},
		},
		{
			name:      "mmproj deduped",
			files:     []string{"model.gguf", "mmproj-F16.gguf"},
			mmproj:    "mmproj-F16.gguf",
			repoFiles: []string{"model.gguf", "mmproj-F16.gguf"},
			want:      &StagingPlan{Primary: "model.gguf", Files: []string{"model.gguf", "mmproj-F16.gguf"}, Mmproj: "mmproj-F16.gguf"},
		},
		{
			name:      "primary glob matches one",
			files:     []string{"model-00001*.gguf", "model-0000*-of-00004.gguf"},
			repoFiles: []string{"model-00001-of-00004.gguf", "model-00002-of-00004.gguf"},
			want:      &StagingPlan{Primary: "model-00001-of-00004.gguf", Files: []string{"model-00001-of-00004.gguf", "model-00002-of-00004.gguf"}},
		},
		{
			name:      "primary glob matches multiple",
			files:     []string{"model-*.gguf"},
			repoFiles: []string{"model-a.gguf", "model-b.gguf"},
			wantErr:   true,
		},
		{
			name:      "primary glob matches zero",
			files:     []string{"missing*.gguf"},
			repoFiles: []string{"model.gguf"},
			wantErr:   true,
		},
		{
			name:      "mmproj missing from repo list",
			files:     []string{"model.gguf"},
			mmproj:    "missing-mmproj.gguf",
			repoFiles: []string{"model.gguf"},
			wantErr:   true,
		},
		{
			name:    "rejects empty entry",
			files:   []string{""},
			wantErr: true,
		},
		{
			name:    "rejects absolute path",
			files:   []string{"/models/model.gguf"},
			wantErr: true,
		},
		{
			name:    "rejects path escape",
			files:   []string{"../model.gguf"},
			wantErr: true,
		},
		{
			name:      "rejects invalid glob pattern",
			files:     []string{"model[.gguf"},
			repoFiles: []string{"model[.gguf"},
			wantErr:   true,
		},
		// A secondary glob that matches zero repo files is not an error.
		// The user may declare a pattern that applies only to certain repos,
		// and the controller may validate before HF listing completes.
		{
			name:      "secondary glob matching zero is accepted",
			files:     []string{"model.gguf", "shard-*.gguf"},
			repoFiles: []string{"model.gguf"},
			want:      &StagingPlan{Primary: "model.gguf", Files: []string{"model.gguf"}},
		},
		// When repoFileList is nil or empty, existence validation is skipped
		// for exact paths. The controller may call ResolveFileSet without
		// an HF file listing.
		{
			name:  "nil repo list skips exact path validation",
			files: []string{"model.gguf"},
			want:  &StagingPlan{Primary: "model.gguf", Files: []string{"model.gguf"}},
		},
		{
			name:    "mmproj rejects absolute path",
			files:   []string{"model.gguf"},
			mmproj:  "/models/mmproj.gguf",
			wantErr: true,
		},
		{
			name:    "mmproj rejects path escape",
			files:   []string{"model.gguf"},
			mmproj:  "../mmproj.gguf",
			wantErr: true,
		},
		{
			name:      "mmproj accepts empty string with files set",
			files:     []string{"model.gguf"},
			mmproj:    "",
			repoFiles: []string{"model.gguf"},
			want:      &StagingPlan{Primary: "model.gguf", Files: []string{"model.gguf"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveFileSet(tc.files, tc.mmproj, tc.repoFiles)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ResolveFileSet() error = %v; wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ResolveFileSet() = %#v; want %#v", got, tc.want)
			}
		})
	}
}

func TestStagedCachePath(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"model.gguf", "/models/cache/model.gguf"},
		{"MTP/model.gguf", "/models/cache/MTP/model.gguf"},
	}
	for _, tc := range cases {
		if got := stagedCachePath("/models/cache", tc.input); got != tc.want {
			t.Fatalf("stagedCachePath(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}
