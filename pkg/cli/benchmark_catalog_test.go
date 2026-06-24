package cli

import "testing"

func TestResolveImage(t *testing.T) {
	tests := []struct {
		name        string
		accelerator string
		gpuEnabled  bool
		want        string
	}{
		{name: "cpu when gpu disabled", accelerator: acceleratorCUDA, gpuEnabled: false, want: imageLlamaCppServer},
		{name: "cuda image", accelerator: acceleratorCUDA, gpuEnabled: true, want: imageLlamaCppServerCUDA},
		{name: "intel image", accelerator: acceleratorIntel, gpuEnabled: true, want: imageLlamaCppServerIntel},
		{name: "rocm image", accelerator: acceleratorROCm, gpuEnabled: true, want: imageLlamaCppServerROCm},
		{name: "metal no container image", accelerator: acceleratorMetal, gpuEnabled: true, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveImage(tt.accelerator, tt.gpuEnabled)
			if got != tt.want {
				t.Errorf("resolveImage(%q, %v) = %q, want %q", tt.accelerator, tt.gpuEnabled, got, tt.want)
			}
		})
	}
}

func TestGPUVendor(t *testing.T) {
	tests := []struct {
		name        string
		accelerator string
		want        string
	}{
		{name: "intel accelerator maps intel vendor", accelerator: acceleratorIntel, want: "intel"},
		{name: "rocm accelerator maps amd vendor", accelerator: acceleratorROCm, want: "amd"},
		{name: "metal accelerator maps apple vendor", accelerator: acceleratorMetal, want: "apple"},
		{name: "cuda defaults to nvidia vendor", accelerator: acceleratorCUDA, want: "nvidia"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gpuVendor(tt.accelerator)
			if got != tt.want {
				t.Errorf("gpuVendor(%q) = %q, want %q", tt.accelerator, got, tt.want)
			}
		})
	}
}

func TestParseCatalogModelIDs(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{"single model", "llama-3.1-8b", []string{"llama-3.1-8b"}},
		{"multiple models", "llama-3.1-8b,qwen-2.5-coder-7b,mistral-7b",
			[]string{"llama-3.1-8b", "qwen-2.5-coder-7b", "mistral-7b"}},
		{"with spaces", "llama-3.1-8b , qwen-2.5-coder-7b , mistral-7b",
			[]string{"llama-3.1-8b", "qwen-2.5-coder-7b", "mistral-7b"}},
		{"with leading/trailing spaces", "  llama-3.1-8b  ,  qwen-2.5-coder-7b  ",
			[]string{"llama-3.1-8b", "qwen-2.5-coder-7b"}},
		{"empty string", "", []string{""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCatalogModelIDs(tt.input)
			if len(got) != len(tt.expected) {
				t.Errorf("parseCatalogModelIDs(%q) returned %d items, want %d", tt.input, len(got), len(tt.expected))
				return
			}
			for i, v := range got {
				if v != tt.expected[i] {
					t.Errorf("parseCatalogModelIDs(%q)[%d] = %q, want %q", tt.input, i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestValidateCatalogModels(t *testing.T) {
	tests := []struct {
		name      string
		modelIDs  []string
		wantError bool
	}{
		{"valid models", []string{"llama-3.1-8b", "mistral-7b"}, false},
		{"single valid model", []string{"qwen-2.5-coder-7b"}, false},
		{"invalid model", []string{"nonexistent-model-xyz"}, true},
		{"mixed valid and invalid", []string{"llama-3.1-8b", "nonexistent-model-xyz"}, true},
		{"empty list", []string{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			models, err := validateCatalogModels(tt.modelIDs)
			if tt.wantError && err == nil {
				t.Errorf("validateCatalogModels(%v) = nil error, want error", tt.modelIDs)
			}
			if !tt.wantError && err != nil {
				t.Errorf("validateCatalogModels(%v) = error %v, want nil", tt.modelIDs, err)
			}
			if !tt.wantError && len(models) != len(tt.modelIDs) {
				t.Errorf("validateCatalogModels(%v) returned %d models, want %d", tt.modelIDs, len(models), len(tt.modelIDs))
			}
		})
	}
}
