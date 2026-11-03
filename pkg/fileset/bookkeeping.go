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

import "path"

// BookkeepingName is one family of files the staging scripts in
// internal/controller/model_storage.go write into a cache entry next to the
// artifacts. Pattern is a path.Match pattern on the file's base name.
type BookkeepingName struct {
	Pattern   string
	WrittenBy string
}

// StagingBookkeeping lists every file name staging writes into a cache entry
// besides the artifacts. Nothing that removes files from a live entry may touch
// a match: staging reads stamps, markers and size records back on the next
// start, resumes into partials, and locks the lockfile. A file a Model lists is
// an artifact even when its name matches.
//
// internal/controller's TestStagingBookkeepingCoversEveryStagedName scans the
// generated init commands for every name they write and fails when one is not
// matched here, so a new bookkeeping file cannot be missed silently.
var StagingBookkeeping = []BookkeepingName{
	{".llmkube-download.lock", "cacheDirLock: the per-entry download lock (fd 9), never deleted"},
	{"*.sha256", "llmkube_stamp_sha256: <file>.sha256 digest stamp"},
	{"*.sha256-rejected", "llmkube_publish_sha256: <file>.<digest>.sha256-rejected mismatch marker"},
	{"*.llmkube-size", "publishFile and the size backfill: <file>.llmkube-size size record"},
	{"*.tmp", "resume partials <file>.<validator>.tmp and <file>.tmp, swept by staging itself"},
	{"*.llmkube-tmp.??????", "llmkube_atomic_write temp file (<dest>.llmkube-tmp.XXXXXX)"},
	{"*.llmkube-size.??????", "size-record temp of the writer before .llmkube-tmp (#2255)"},
	{"*.sha256-rejected.??????", "marker temp of the writer before .llmkube-tmp (#2255)"},
	{"*.sha256.??????", "stamp temp of the writer before .llmkube-tmp; staging's own sweep leaves it alone"},
	// Staging names its own files .llmkube-*. Reserving the whole namespace
	// keeps a future bookkeeping name safe before this list learns it.
	{"*.llmkube-*", "any other name in the .llmkube-* namespace staging reserves"},
}

// IsStagingBookkeeping reports whether rel, a path relative to a cache entry,
// names a file staging writes for its own use rather than an artifact.
func IsStagingBookkeeping(rel string) bool {
	base := path.Base(rel)
	for _, name := range StagingBookkeeping {
		if ok, _ := path.Match(name.Pattern, base); ok {
			return true
		}
	}
	return false
}
