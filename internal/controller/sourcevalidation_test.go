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
	"os"
	"strings"
	"testing"
)

// testLocalRoots is the host-path allowlist handed to envtest reconcilers
// whose fixtures use local (file://) sources. It spans os.TempDir() (where
// the MkdirTemp fixtures live) and the fixed fake paths used by the
// unrecoverable-fetch tests, so pre-allowlist test behavior is preserved
// while production defaults stay locked down (GHSA-jw3m-8q7m-f35r).
var testLocalRoots = []string{os.TempDir(), "/nonexistent", "/still", "/models"}

// TestValidateLocalSourceAllowed locks down the host-path allowlist for local
// model sources (GHSA-jw3m-8q7m-f35r). A local source (absolute path or
// file:// URI) must be rejected unless it lies within an operator-configured
// allowed root; the allowlist is empty by default, so local/hostPath sources
// are disabled until the operator opts in.
func TestValidateLocalSourceAllowed(t *testing.T) {
	tests := []struct {
		name        string
		source      string
		roots       []string
		wantErr     bool
		wantErrText string
	}{
		{
			name:   "non-local https passes through with no roots",
			source: "https://x/m.gguf",
			roots:  nil,
		},
		{
			name:   "non-local pvc passes through with no roots",
			source: "pvc://c/m.gguf",
			roots:  nil,
		},
		{
			name:   "non-local hf passes through with no roots",
			source: "hf://org/repo",
			roots:  nil,
		},
		{
			name:   "non-local https passes through even with roots set",
			source: "https://x/m.gguf",
			roots:  []string{"/srv/models"},
		},
		{
			name:        "empty roots reject absolute path",
			source:      "/etc/passwd",
			roots:       nil,
			wantErr:     true,
			wantErrText: "disabled",
		},
		{
			name:        "empty roots reject file:// URI",
			source:      "file:///etc/passwd",
			roots:       nil,
			wantErr:     true,
			wantErrText: "GHSA-jw3m-8q7m-f35r",
		},
		{
			name:   "path under allowed root is allowed",
			source: "/srv/models/m.gguf",
			roots:  []string{"/srv/models"},
		},
		{
			name:   "exact allowed root is allowed",
			source: "/srv/models",
			roots:  []string{"/srv/models"},
		},
		{
			name:   "file:// URI under allowed root is allowed",
			source: "file:///srv/models/m.gguf",
			roots:  []string{"/srv/models"},
		},
		{
			name:        "path outside allowed root is rejected",
			source:      "/etc/passwd",
			roots:       []string{"/srv/models"},
			wantErr:     true,
			wantErrText: "not within any allowed root",
		},
		{
			name:        "dot-dot escape out of allowed root is rejected",
			source:      "/srv/models/../../etc/passwd",
			roots:       []string{"/srv/models"},
			wantErr:     true,
			wantErrText: "GHSA-jw3m-8q7m-f35r",
		},
		{
			name:        "sibling directory sharing the root as a string prefix is rejected",
			source:      "/models-evil/m.gguf",
			roots:       []string{"/models"},
			wantErr:     true,
			wantErrText: "not within any allowed root",
		},
		{
			name:   "trailing slash on the root is cleaned before matching",
			source: "/srv/models/m.gguf",
			roots:  []string{"/srv/models/"},
		},
		{
			name:        "unusable roots (blank, relative) are treated as empty",
			source:      "/srv/models/m.gguf",
			roots:       []string{"  ", "relative/path"},
			wantErr:     true,
			wantErrText: "disabled",
		},
		{
			name:   "prefix mechanism: SA token path allowed only when under an allowlisted root",
			source: "/var/run/secrets/kubernetes.io/serviceaccount/token",
			roots:  []string{"/var/run/secrets/kubernetes.io/serviceaccount"},
		},
		{
			name:        "SA token path rejected when only an unrelated root is allowed",
			source:      "/var/run/secrets/kubernetes.io/serviceaccount/token",
			roots:       []string{"/srv/models"},
			wantErr:     true,
			wantErrText: "not within any allowed root",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLocalSourceAllowed(tt.source, tt.roots)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("validateLocalSourceAllowed(%q, %v) = nil, want error", tt.source, tt.roots)
				}
				if tt.wantErrText != "" && !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("validateLocalSourceAllowed(%q, %v) error %q does not contain %q",
						tt.source, tt.roots, err.Error(), tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateLocalSourceAllowed(%q, %v) = %v, want nil", tt.source, tt.roots, err)
			}
		})
	}
}
