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

package changepolicy

import (
	"testing"
)

func TestClassify(t *testing.T) {
	policy := defaultPolicy{}

	tests := []struct {
		name    string
		changed map[string]int
		want    WorkClass
	}{
		{
			name:    "ci-policy: .github/workflows",
			changed: map[string]int{".github/workflows/ci.yml": 10},
			want:    workClassCIPolicy,
		},
		{
			name:    "ci-policy: .github/actions",
			changed: map[string]int{".github/actions/lint/action.yml": 5},
			want:    workClassCIPolicy,
		},
		{
			name:    "release-policy: .goreleaser",
			changed: map[string]int{".goreleaser.yaml": 8},
			want:    workClassReleasePolicy,
		},
		{
			name:    "release-policy: release-please",
			changed: map[string]int{"release-please-config.json": 3},
			want:    workClassReleasePolicy,
		},
		{
			name:    "packaging: Formula",
			changed: map[string]int{"Formula/llmkube.rb": 12},
			want:    workClassPackaging,
		},
		{
			name:    "packaging: Dockerfile",
			changed: map[string]int{"Dockerfile": 20},
			want:    workClassPackaging,
		},
		{
			name:    "packaging: charts",
			changed: map[string]int{"charts/llmkube/values.yaml": 15},
			want:    workClassPackaging,
		},
		{
			name:    "packaging: *.spec",
			changed: map[string]int{"pkg/llmkube.spec": 6},
			want:    workClassPackaging,
		},
		{
			name:    "packaging: hack/publish-*",
			changed: map[string]int{"hack/publish-release.sh": 9},
			want:    workClassPackaging,
		},
		{
			name:    "docs: *.md",
			changed: map[string]int{"README.md": 7},
			want:    workClassDocs,
		},
		{
			name:    "docs: docs/**",
			changed: map[string]int{"docs/guides/install.md": 11},
			want:    workClassDocs,
		},
		{
			name:    "docs: examples/**",
			changed: map[string]int{"examples/basic.yaml": 4},
			want:    workClassDocs,
		},
		{
			name:    "config: *.yaml",
			changed: map[string]int{"config/rbac/role.yaml": 14},
			want:    workClassConfig,
		},
		{
			name:    "config: *.yml",
			changed: map[string]int{"config/kustomization.yml": 2},
			want:    workClassConfig,
		},
		{
			name:    "config: *.toml",
			changed: map[string]int{"config/settings.toml": 3},
			want:    workClassConfig,
		},
		{
			name:    "config: *.json",
			changed: map[string]int{"config/config.json": 5},
			want:    workClassConfig,
		},
		{
			name:    "code-fix: Go source",
			changed: map[string]int{"pkg/agent/loop.go": 25},
			want:    workClassCodeFix,
		},
		{
			name:    "code-fix: empty changed",
			changed: map[string]int{},
			want:    workClassCodeFix,
		},
		{
			name: "mixed: no dominant class",
			changed: map[string]int{
				"pkg/agent/loop.go": 10,
				"README.md":         10,
			},
			want: workClassMixed,
		},
		{
			name: "mixed: zero-line diffs",
			changed: map[string]int{
				"pkg/agent/loop.go": 0,
				"README.md":         0,
			},
			want: workClassMixed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := policy.Classify(tt.changed)
			if got != tt.want {
				t.Errorf("Classify() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequiresHumanReview(t *testing.T) {
	policy := defaultPolicy{}

	tests := []struct {
		name         string
		changedPaths []string
		selfGO       []string
		want         bool
	}{
		{
			name:         "ci-policy change with selfGO missing ci-policy",
			changedPaths: []string{".github/workflows/ci.yml"},
			selfGO:       []string{"code-fix", "docs", "config"},
			want:         true,
		},
		{
			name:         "ci-policy change with selfGO including ci-policy",
			changedPaths: []string{".github/workflows/ci.yml"},
			selfGO:       []string{"ci-policy", "code-fix", "docs", "config"},
			want:         false,
		},
		{
			name:         "release-policy change with selfGO missing release-policy",
			changedPaths: []string{".goreleaser.yaml"},
			selfGO:       []string{"code-fix", "docs", "config"},
			want:         true,
		},
		{
			name:         "release-policy change with selfGO including release-policy",
			changedPaths: []string{".goreleaser.yaml"},
			selfGO:       []string{"release-policy", "code-fix", "docs", "config"},
			want:         false,
		},
		{
			name:         "code-fix change with selfGO including code-fix",
			changedPaths: []string{"pkg/agent/loop.go"},
			selfGO:       []string{"code-fix", "docs", "config"},
			want:         false,
		},
		{
			name:         "code-fix change with selfGO missing code-fix",
			changedPaths: []string{"pkg/agent/loop.go"},
			selfGO:       []string{"docs", "config"},
			want:         true,
		},
		{
			name:         "docs change with selfGO including docs",
			changedPaths: []string{"README.md"},
			selfGO:       []string{"code-fix", "docs", "config"},
			want:         false,
		},
		{
			name:         "docs change with selfGO missing docs",
			changedPaths: []string{"README.md"},
			selfGO:       []string{"code-fix", "config"},
			want:         true,
		},
		{
			name:         "mixed change with selfGO missing mixed",
			changedPaths: []string{"pkg/agent/loop.go", "README.md"},
			selfGO:       []string{"code-fix", "docs", "config"},
			want:         true,
		},
		{
			name:         "mixed change with selfGO including mixed",
			changedPaths: []string{"pkg/agent/loop.go", "README.md"},
			selfGO:       []string{"mixed", "code-fix", "docs", "config"},
			want:         false,
		},
		{
			name:         "empty changed paths",
			changedPaths: []string{},
			selfGO:       []string{"code-fix"},
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := policy.RequiresHumanReview(tt.changedPaths, tt.selfGO)
			if got != tt.want {
				t.Errorf("RequiresHumanReview() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyCiPolicy(t *testing.T) {
	// Regression test: .github/workflows must classify as ci-policy.
	policy := defaultPolicy{}
	got := policy.Classify(map[string]int{".github/workflows/ci.yml": 10})
	if got != workClassCIPolicy {
		t.Errorf("Classify(ci.yml) = %q, want %q", got, workClassCIPolicy)
	}
}

func TestClassifyCiPolicyActions(t *testing.T) {
	// Regression test: .github/actions must classify as ci-policy.
	policy := defaultPolicy{}
	got := policy.Classify(map[string]int{".github/actions/lint/action.yml": 5})
	if got != workClassCIPolicy {
		t.Errorf("Classify(actions/lint/action.yml) = %q, want %q", got, workClassCIPolicy)
	}
}

func TestRequiresHumanReviewCiPolicyGate(t *testing.T) {
	// Regression test: RequiresHumanReview returns true when
	// a ci-policy change is not in the selfGO list.
	policy := defaultPolicy{}
	got := policy.RequiresHumanReview(
		[]string{".github/workflows/ci.yml"},
		[]string{"code-fix", "docs", "config"},
	)
	if !got {
		t.Error("RequiresHumanReview(ci.yml) = false, want true")
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		name string
		glob string
		path string
		want bool
	}{
		{
			name: "exact match",
			glob: ".goreleaser.yaml",
			path: ".goreleaser.yaml",
			want: true,
		},
		{
			name: "base name match",
			glob: ".goreleaser*",
			path: ".goreleaser.yaml",
			want: true,
		},
		{
			name: "prefix /** match",
			glob: ".github/workflows/**",
			path: ".github/workflows/ci.yml",
			want: true,
		},
		{
			name: "prefix /** no match",
			glob: ".github/workflows/**",
			path: ".github/actions/lint/action.yml",
			want: false,
		},
		{
			name: "no match",
			glob: "*.yaml",
			path: "pkg/agent/loop.go",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchGlob(tt.glob, tt.path)
			if got != tt.want {
				t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.glob, tt.path, got, tt.want)
			}
		})
	}
}

func TestClassifyFile(t *testing.T) {
	tests := []struct {
		name string
		path string
		want WorkClass
	}{
		{
			name: "ci-policy: .github/workflows/ci.yml",
			path: ".github/workflows/ci.yml",
			want: workClassCIPolicy,
		},
		{
			name: "ci-policy: .github/actions/lint/action.yml",
			path: ".github/actions/lint/action.yml",
			want: workClassCIPolicy,
		},
		{
			name: "release-policy: .goreleaser.yaml",
			path: ".goreleaser.yaml",
			want: workClassReleasePolicy,
		},
		{
			name: "release-policy: release-please-config.json",
			path: "release-please-config.json",
			want: workClassReleasePolicy,
		},
		{
			name: "packaging: Formula/llmkube.rb",
			path: "Formula/llmkube.rb",
			want: workClassPackaging,
		},
		{
			name: "packaging: Dockerfile",
			path: "Dockerfile",
			want: workClassPackaging,
		},
		{
			name: "packaging: charts/llmkube/values.yaml",
			path: "charts/llmkube/values.yaml",
			want: workClassPackaging,
		},
		{
			name: "packaging: pkg/llmkube.spec",
			path: "pkg/llmkube.spec",
			want: workClassPackaging,
		},
		{
			name: "packaging: hack/publish-release.sh",
			path: "hack/publish-release.sh",
			want: workClassPackaging,
		},
		{
			name: "docs: README.md",
			path: "README.md",
			want: workClassDocs,
		},
		{
			name: "docs: docs/guides/install.md",
			path: "docs/guides/install.md",
			want: workClassDocs,
		},
		{
			name: "docs: examples/basic.yaml",
			path: "examples/basic.yaml",
			want: workClassDocs,
		},
		{
			name: "config: config/rbac/role.yaml",
			path: "config/rbac/role.yaml",
			want: workClassConfig,
		},
		{
			name: "config: config/kustomization.yml",
			path: "config/kustomization.yml",
			want: workClassConfig,
		},
		{
			name: "config: config/settings.toml",
			path: "config/settings.toml",
			want: workClassConfig,
		},
		{
			name: "config: config/config.json",
			path: "config/config.json",
			want: workClassConfig,
		},
		{
			name: "code-fix: pkg/agent/loop.go",
			path: "pkg/agent/loop.go",
			want: workClassCodeFix,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyFile(tt.path)
			if got != tt.want {
				t.Errorf("classifyFile(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestClassifyFootprint(t *testing.T) {
	tests := []struct {
		name    string
		changed map[string]int
		want    WorkClass
	}{
		{
			name:    "single ci-policy file",
			changed: map[string]int{".github/workflows/ci.yml": 10},
			want:    workClassCIPolicy,
		},
		{
			name:    "single code-fix file",
			changed: map[string]int{"pkg/agent/loop.go": 25},
			want:    workClassCodeFix,
		},
		{
			name:    "mixed: no dominant class",
			changed: map[string]int{"pkg/agent/loop.go": 10, "README.md": 10},
			want:    workClassMixed,
		},
		{
			name:    "mixed: zero-line diffs",
			changed: map[string]int{"pkg/agent/loop.go": 0, "README.md": 0},
			want:    workClassMixed,
		},
		{
			name:    "empty changed",
			changed: map[string]int{},
			want:    workClassCodeFix,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyFootprint(tt.changed)
			if got != tt.want {
				t.Errorf("classifyFootprint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkClassInList(t *testing.T) {
	tests := []struct {
		name  string
		class WorkClass
		list  []string
		want  bool
	}{
		{
			name:  "ci-policy in list",
			class: workClassCIPolicy,
			list:  []string{"ci-policy", "code-fix"},
			want:  true,
		},
		{
			name:  "ci-policy not in list",
			class: workClassCIPolicy,
			list:  []string{"code-fix", "docs"},
			want:  false,
		},
		{
			name:  "empty list",
			class: workClassCodeFix,
			list:  []string{},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workClassInList(tt.class, tt.list)
			if got != tt.want {
				t.Errorf("workClassInList(%q, %v) = %v, want %v", tt.class, tt.list, got, tt.want)
			}
		})
	}
}
