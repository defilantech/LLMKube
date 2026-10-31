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
	"slices"
	"strings"
	"testing"
)

// agentEnvFixture is an agent environment carrying the variables that must
// stay with the agent alongside the ones an engine needs.
var agentEnvFixture = []string{
	"HF_TOKEN=hf_planted",
	"HUGGING_FACE_HUB_TOKEN=hf_planted",
	"AWS_ACCESS_KEY_ID=AKIAPLANTED",
	"AWS_SECRET_ACCESS_KEY=planted",
	"KUBECONFIG=/Users/x/.kube/metal-agent",
	"DYLD_INSERT_LIBRARIES=/x/inject.dylib",
	"PYTHONPATH=/x/site",
	"LLAMA_ARG_HOST=0.0.0.0",
	"LLAMA_API_KEY=planted",
	"HTTPS_PROXY=http://proxy.lan:3128",
	"PATH=/opt/homebrew/bin:/usr/bin:/bin",
	"HOME=/Users/x",
	"USER=x",
	"LOGNAME=x",
	"TMPDIR=/var/folders/xx/T/",
	"LANG=en_US.UTF-8",
	"TZ=Europe/Dublin",
	"HF_HOME=/Users/x/.cache/huggingface",
	"HF_HUB_CACHE=/Volumes/models/hub",
	"LC_ALL=en_US.UTF-8",
	"LC_CTYPE=UTF-8",
	"GGML_METAL_ENABLE=0",
	"MLX_MAX_OPS_PER_BUFFER=4",
	"MLX_MAX_OPS_PER_BUFFER_EXTRA=1",
}

var droppedAgentVars = []string{
	"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
	"KUBECONFIG", "DYLD_INSERT_LIBRARIES", "PYTHONPATH", "LLAMA_ARG_HOST", "LLAMA_API_KEY",
	"HTTPS_PROXY",
}

func envNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	return names
}

func TestBuildEngineEnv_DropsAgentOnlyVariables(t *testing.T) {
	got := buildEngineEnv(agentEnvFixture, nil)
	for _, name := range droppedAgentVars {
		if slices.Contains(envNames(got), name) {
			t.Errorf("engine env carries %s: %q", name, got)
		}
	}
}

func TestBuildEngineEnv_KeepsAllowlistAndLocale(t *testing.T) {
	got := buildEngineEnv(agentEnvFixture, nil)
	want := []string{
		"HF_HOME=/Users/x/.cache/huggingface",
		"HF_HUB_CACHE=/Volumes/models/hub",
		"HOME=/Users/x",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		"LC_CTYPE=UTF-8",
		"LOGNAME=x",
		"PATH=/opt/homebrew/bin:/usr/bin:/bin",
		"TMPDIR=/var/folders/xx/T/",
		"TZ=Europe/Dublin",
		"USER=x",
	}
	if !slices.Equal(got, want) {
		t.Errorf("engine env\n got %q\nwant %q", got, want)
	}
}

// A passthrough name is an exact name: it must not widen into a prefix match
// that would carry MLX_MAX_OPS_PER_BUFFER_EXTRA along with it.
func TestBuildEngineEnv_PassthroughIsExactName(t *testing.T) {
	got := buildEngineEnv(agentEnvFixture, []string{"MLX_MAX_OPS_PER_BUFFER", "NOT_SET_ANYWHERE"})
	names := envNames(got)
	if !slices.Contains(got, "MLX_MAX_OPS_PER_BUFFER=4") {
		t.Errorf("passthrough MLX_MAX_OPS_PER_BUFFER missing: %q", got)
	}
	if slices.Contains(names, "MLX_MAX_OPS_PER_BUFFER_EXTRA") {
		t.Errorf("passthrough matched a longer name as a prefix: %q", got)
	}
	if slices.Contains(names, "NOT_SET_ANYWHERE") {
		t.Errorf("an unset passthrough name must not appear: %q", got)
	}
	for _, name := range droppedAgentVars {
		if slices.Contains(names, name) {
			t.Errorf("passthrough of other names leaked %s: %q", name, got)
		}
	}
}

// An explicit variable replaces the inherited value, even one the operator
// passed through, and appears once.
func TestBuildEngineEnv_ExplicitOverridesOnce(t *testing.T) {
	got := buildEngineEnv(agentEnvFixture, []string{"GGML_METAL_ENABLE"},
		"GGML_METAL_ENABLE=1", "GGML_METAL_PATH_RESOURCES=/usr/local/share/llama.cpp", "HOME=/override")
	for _, want := range []string{
		"GGML_METAL_ENABLE=1", "GGML_METAL_PATH_RESOURCES=/usr/local/share/llama.cpp", "HOME=/override",
	} {
		name, _, _ := strings.Cut(want, "=")
		var seen []string
		for _, kv := range got {
			if strings.HasPrefix(kv, name+"=") {
				seen = append(seen, kv)
			}
		}
		if len(seen) != 1 || seen[0] != want {
			t.Errorf("%s entries = %q, want exactly [%q]", name, seen, want)
		}
	}
}

func TestBuildEngineEnv_MalformedEntriesIgnored(t *testing.T) {
	parent := []string{"NOEQUALS", "", "=hidden", "PATH=/bin", "HOME"}
	got := buildEngineEnv(parent, []string{"NOEQUALS"}, "BROKEN", "=x", "GGML_METAL_ENABLE=1")
	want := []string{"GGML_METAL_ENABLE=1", "PATH=/bin"}
	if !slices.Equal(got, want) {
		t.Errorf("engine env = %q, want %q", got, want)
	}
}

// A nil cmd.Env means "inherit everything" to os/exec, so an empty result
// must be an empty slice, never nil.
func TestBuildEngineEnv_EmptyResultIsNotNil(t *testing.T) {
	got := buildEngineEnv([]string{"HF_TOKEN=x"}, nil)
	if got == nil || len(got) != 0 {
		t.Errorf("buildEngineEnv = %#v, want a non-nil empty slice", got)
	}
}

func TestBuildEngineEnv_Deterministic(t *testing.T) {
	reversed := slices.Clone(agentEnvFixture)
	slices.Reverse(reversed)
	a := buildEngineEnv(agentEnvFixture, []string{"MLX_MAX_OPS_PER_BUFFER"}, "GGML_METAL_ENABLE=1")
	b := buildEngineEnv(reversed, []string{"MLX_MAX_OPS_PER_BUFFER"}, "GGML_METAL_ENABLE=1")
	if !slices.Equal(a, b) {
		t.Errorf("parent order changed the result:\n%q\n%q", a, b)
	}
	if !slices.IsSorted(envNames(a)) {
		t.Errorf("result not sorted by name: %q", a)
	}
	// A duplicated name keeps its first value, the one os.Getenv reports.
	dup := buildEngineEnv([]string{"HOME=/first", "HOME=/second"}, nil)
	if !slices.Equal(dup, []string{"HOME=/first"}) {
		t.Errorf("duplicate HOME = %q, want [HOME=/first]", dup)
	}
}

func TestValidateEngineEnvPassthrough(t *testing.T) {
	for _, tc := range []struct {
		names   []string
		wantErr string
	}{
		{names: nil},
		{names: []string{"MLX_MAX_OPS_PER_BUFFER", "HTTPS_PROXY", "no_proxy", "_X1"}},
		{names: []string{"HF_TOKEN=x"}, wantErr: `"HF_TOKEN=x"`},
		{names: []string{"GGML_*"}, wantErr: `"GGML_*"`},
		{names: []string{"1ABC"}, wantErr: `"1ABC"`},
		{names: []string{"A B"}, wantErr: `"A B"`},
		{names: []string{""}, wantErr: `""`},
		{names: []string{"OK", "BAD-NAME", "ALSO=BAD"}, wantErr: `"BAD-NAME", "ALSO=BAD"`},
	} {
		err := ValidateEngineEnvPassthrough(tc.names)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("ValidateEngineEnvPassthrough(%q) = %v, want nil", tc.names, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("ValidateEngineEnvPassthrough(%q) = %v, want an error naming %s", tc.names, err, tc.wantErr)
		}
	}
}
