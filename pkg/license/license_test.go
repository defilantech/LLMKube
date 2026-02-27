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

package license

import (
	"testing"
)

func TestGet(t *testing.T) {
	tests := []struct {
		id       string
		wantNil  bool
		wantName string
	}{
		{"apache-2.0", false, "Apache License 2.0"},
		{"mit", false, "MIT License"},
		{"llama-3.1-community", false, "Llama 3.1 Community License Agreement"},
		{"llama-3.2-community", false, "Llama 3.2 Community License Agreement"},
		{"llama-3.3-community", false, "Llama 3.3 Community License Agreement"},
		{"gemma", false, "Gemma Terms of Use"},
		{"unknown-license", true, ""},
		{"", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got := Get(tt.id)
			if tt.wantNil {
				if got != nil {
					t.Errorf("Get(%q) = %+v, want nil", tt.id, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("Get(%q) = nil, want non-nil", tt.id)
			}
			if got.Name != tt.wantName {
				t.Errorf("Get(%q).Name = %q, want %q", tt.id, got.Name, tt.wantName)
			}
			if got.ID != tt.id {
				t.Errorf("Get(%q).ID = %q, want %q", tt.id, got.ID, tt.id)
			}
		})
	}
}

func TestAll(t *testing.T) {
	licenses := All()

	if len(licenses) != 6 {
		t.Errorf("All() returned %d licenses, want 6", len(licenses))
	}

	for i, l := range licenses {
		if l.ID == "" {
			t.Errorf("licenses[%d].ID is empty", i)
		}
		if l.Name == "" {
			t.Errorf("licenses[%d].Name is empty", i)
		}
		if l.URL == "" {
			t.Errorf("licenses[%d].URL is empty", i)
		}
	}

	// Verify sorted order
	for i := 1; i < len(licenses); i++ {
		if licenses[i].ID < licenses[i-1].ID {
			t.Errorf("All() not sorted: %q appears after %q", licenses[i].ID, licenses[i-1].ID)
		}
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"Apache-2.0", "apache-2.0"},
		{"apache-2.0", "apache-2.0"},
		{"MIT", "mit"},
		{"mit", "mit"},
		{"llama 3.1 community", "llama-3.1-community"},
		{"Llama-3.1-Community", "llama-3.1-community"},
		{"llama 3.2 community", "llama-3.2-community"},
		{"llama 3.3 community", "llama-3.3-community"},
		{"Gemma Terms", "gemma"},
		{"unknown-xyz", "unknown-xyz"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got := Normalize(tt.raw)
			if got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestLicenseProperties(t *testing.T) {
	apache := Get("apache-2.0")
	if !apache.CommercialUse {
		t.Error("Apache-2.0 should allow commercial use")
	}
	if !apache.Attribution {
		t.Error("Apache-2.0 should require attribution")
	}
	if len(apache.Restrictions) != 0 {
		t.Errorf("Apache-2.0 should have no restrictions, got %v", apache.Restrictions)
	}

	llama := Get("llama-3.1-community")
	if !llama.CommercialUse {
		t.Error("Llama 3.1 should allow commercial use")
	}
	if len(llama.Restrictions) != 1 {
		t.Fatalf("Llama 3.1 should have 1 restriction, got %d", len(llama.Restrictions))
	}
	if llama.Restrictions[0] != "700M monthly active users limit" {
		t.Errorf("unexpected restriction: %q", llama.Restrictions[0])
	}

	gemma := Get("gemma")
	if len(gemma.Restrictions) != 1 {
		t.Fatalf("Gemma should have 1 restriction, got %d", len(gemma.Restrictions))
	}
}
