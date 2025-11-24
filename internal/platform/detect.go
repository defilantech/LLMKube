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

package platform

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Capabilities represents the GPU/accelerator capabilities of the system
type Capabilities struct {
	Metal        bool
	CUDA         bool
	ROCm         bool
	GPUName      string
	GPUCores     int
	MetalVersion int
	OS           string
	Arch         string
}

// DetectCapabilities detects the GPU and accelerator capabilities of the system
func DetectCapabilities() Capabilities {
	caps := Capabilities{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}

	// Detect Metal on macOS
	if runtime.GOOS == "darwin" {
		caps.Metal = detectMetal(&caps)
	}

	// Detect CUDA on Linux/Windows
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		caps.CUDA = detectCUDA(&caps)
	}

	// Detect ROCm on Linux
	if runtime.GOOS == "linux" {
		caps.ROCm = detectROCm(&caps)
	}

	return caps
}

// detectMetal detects Metal support on macOS
func detectMetal(caps *Capabilities) bool {
	// Use system_profiler to get GPU info
	cmd := exec.Command("system_profiler", "SPDisplaysDataType")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	outputStr := string(output)

	// Check for Metal support
	if !strings.Contains(outputStr, "Metal") {
		return false
	}

	// Parse GPU information
	lines := strings.Split(outputStr, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Get chipset model (GPU name)
		if strings.HasPrefix(line, "Chipset Model:") {
			caps.GPUName = strings.TrimSpace(strings.TrimPrefix(line, "Chipset Model:"))
		}

		// Get total number of cores
		if strings.HasPrefix(line, "Total Number of Cores:") {
			coresStr := strings.TrimSpace(strings.TrimPrefix(line, "Total Number of Cores:"))
			if cores, err := strconv.Atoi(coresStr); err == nil {
				caps.GPUCores = cores
			}
		}

		// Get Metal version
		if strings.HasPrefix(line, "Metal Support:") || strings.HasPrefix(line, "Metal:") {
			metalStr := strings.TrimSpace(line)
			if strings.Contains(metalStr, "Metal 4") {
				caps.MetalVersion = 4
			} else if strings.Contains(metalStr, "Metal 3") {
				caps.MetalVersion = 3
			} else if strings.Contains(metalStr, "Metal 2") {
				caps.MetalVersion = 2
			}
		}
	}

	return true
}

// detectCUDA detects NVIDIA CUDA support
func detectCUDA(caps *Capabilities) bool {
	// Check if nvidia-smi is available
	cmd := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	gpuName := strings.TrimSpace(string(output))
	if gpuName != "" {
		caps.GPUName = gpuName
		return true
	}

	return false
}

// detectROCm detects AMD ROCm support
func detectROCm(caps *Capabilities) bool {
	// Check if rocm-smi is available
	cmd := exec.Command("rocm-smi", "--showproductname")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	if strings.Contains(string(output), "GPU") {
		caps.GPUName = strings.TrimSpace(string(output))
		return true
	}

	return false
}

// GetRecommendedAccelerator returns the recommended accelerator for the system
func GetRecommendedAccelerator() string {
	caps := DetectCapabilities()

	if caps.Metal {
		return "metal"
	}
	if caps.CUDA {
		return "cuda"
	}
	if caps.ROCm {
		return "rocm"
	}

	return "cpu"
}

// HasMetalSupport checks if the system supports Metal acceleration
func HasMetalSupport() bool {
	return DetectCapabilities().Metal
}
