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

package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeTools puts nvidia-smi, rocm-smi and system_profiler on an otherwise
// empty PATH. Each records that it ran in dir; system_profiler also prints an
// Apple Silicon GPU.
func fakeTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	scripts := map[string]string{
		"nvidia-smi": ": > \"$0.ran\"\necho 'NVIDIA GeForce'\n",
		"rocm-smi":   ": > \"$0.ran\"\necho 'GPU[0] AMD'\n",
		"system_profiler": ": > \"$0.ran\"\n" +
			"echo 'Chipset Model: Apple M4 Max'\necho 'Total Number of Cores: 40'\necho 'Metal Support: Metal 4'\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

func ran(dir, tool string) bool {
	_, err := os.Stat(filepath.Join(dir, tool+".ran"))
	return err == nil
}

func TestDetectMetalRunsNoGPUProbe(t *testing.T) {
	dir := fakeTools(t)
	caps := DetectMetal()
	if caps.OS != runtime.GOOS || caps.Arch != runtime.GOARCH {
		t.Errorf("OS/Arch = %s/%s, want %s/%s", caps.OS, caps.Arch, runtime.GOOS, runtime.GOARCH)
	}
	for _, tool := range []string{"nvidia-smi", "rocm-smi"} {
		if ran(dir, tool) {
			t.Errorf("DetectMetal ran %s", tool)
		}
	}
	if caps.CUDA || caps.ROCm {
		t.Errorf("caps = %+v, want no CUDA or ROCm result", caps)
	}
	wantMetal := runtime.GOOS == "darwin"
	if ran(dir, "system_profiler") != wantMetal || caps.Metal != wantMetal {
		t.Errorf("system_profiler ran %v, Metal %v; want both %v on %s",
			ran(dir, "system_profiler"), caps.Metal, wantMetal, runtime.GOOS)
	}
	if wantMetal && (caps.GPUName != "Apple M4 Max" || caps.GPUCores != 40 || caps.MetalVersion != 4) {
		t.Errorf("caps = %+v, want the GPU system_profiler names", caps)
	}
}

// The fakes are found on PATH: on Linux the full probe runs them.
func TestDetectCapabilitiesRunsTheFakeGPUProbes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the CUDA and ROCm probes run on Linux")
	}
	dir := fakeTools(t)
	caps := DetectCapabilities()
	if !ran(dir, "nvidia-smi") || !ran(dir, "rocm-smi") || !caps.CUDA || !caps.ROCm {
		t.Errorf("caps = %+v, nvidia-smi ran %v, rocm-smi ran %v; want both fakes run and detected",
			caps, ran(dir, "nvidia-smi"), ran(dir, "rocm-smi"))
	}
}
