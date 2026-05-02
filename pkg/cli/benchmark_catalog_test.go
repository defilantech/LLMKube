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
