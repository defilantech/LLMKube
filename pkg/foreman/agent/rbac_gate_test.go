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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustWrite creates intermediate directories and writes content to
// workspace/relPath. Relative path separators are always '/'.
func mustWrite(t *testing.T, workspace, relPath, content string) {
	t.Helper()
	full := filepath.Join(workspace, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mustWrite MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("mustWrite WriteFile: %v", err)
	}
}

// changedGoFilesRunner returns a fake commandRunner that responds to the
// `git status -z` call (used by changedNonTestGoFiles) with a NUL-terminated
// list of workspace-relative changed paths. All other commands return "".
func changedGoFilesRunner(paths ...string) commandRunner {
	// changedNonTestGoFiles calls: run(ctx, workspace, nil, "git", "status", "-z")
	// Each entry is " M <path>", NUL-terminated as a sequence.
	var entries []string
	for _, p := range paths {
		entries = append(entries, " M "+p)
	}
	out := strings.Join(entries, "\x00")

	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" && len(args) > 0 && args[0] == "status" {
			return out, nil
		}
		return "", nil
	}
}

func TestCheckRBACUse_MissingMarkerFails(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "internal/controller/x.go", `package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
)

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get

func (r *R) Do(ctx context.Context) error {
	return r.Create(ctx, &batchv1.Job{})
}
`)
	run := changedGoFilesRunner("internal/controller/x.go")
	failed, out := checkRBACUse(context.Background(), dir, run)
	if !failed || !strings.Contains(out, "batch") || !strings.Contains(out, "jobs") || !strings.Contains(out, "create") {
		t.Fatalf("want failure for missing batch/jobs/create marker, got failed=%v out=%q", failed, out)
	}
}

func TestCheckRBACUse_MarkerPresentPasses(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "internal/controller/x.go", `package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
)

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create

func (r *R) Do(ctx context.Context) error {
	return r.Create(ctx, &batchv1.Job{})
}
`)
	run := changedGoFilesRunner("internal/controller/x.go")
	failed, _ := checkRBACUse(context.Background(), dir, run)
	if failed {
		t.Fatal("present marker should pass")
	}
}

func TestCheckRBACUse_GetMissingMarkerFails(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "internal/controller/x.go", `package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get

func (r *R) Do(ctx context.Context) error {
	return r.Get(ctx, client.ObjectKey{}, &batchv1.Job{})
}
`)
	run := changedGoFilesRunner("internal/controller/x.go")
	failed, out := checkRBACUse(context.Background(), dir, run)
	if !failed || !strings.Contains(out, "batch") || !strings.Contains(out, "jobs") || !strings.Contains(out, "get") {
		t.Fatalf("want failure for missing batch/jobs/get marker, got failed=%v out=%q", failed, out)
	}
}

func TestCheckRBACUse_GetMarkerPresentPasses(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "internal/controller/x.go", `package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get

func (r *R) Do(ctx context.Context) error {
	return r.Get(ctx, client.ObjectKey{}, &batchv1.Job{})
}
`)
	run := changedGoFilesRunner("internal/controller/x.go")
	failed, _ := checkRBACUse(context.Background(), dir, run)
	if failed {
		t.Fatal("present get marker should pass")
	}
}

func TestCheckRBACUse_NonControllerFileSkipped(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "pkg/cli/x.go", "package cli\n")
	run := changedGoFilesRunner("pkg/cli/x.go")
	failed, _ := checkRBACUse(context.Background(), dir, run)
	if failed {
		t.Fatal("non-controller changes should be skipped")
	}
}
