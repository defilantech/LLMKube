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

package main

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/defilantech/llmkube/internal/platform"
)

func TestCheckHostPlatform(t *testing.T) {
	origCaps, origGOOS, origGlob := detectCapabilities, hostGOOS, globDevices
	t.Cleanup(func() { detectCapabilities, hostGOOS, globDevices = origCaps, origGOOS, origGlob })

	mac := platform.Capabilities{Metal: true, GPUCores: 40, OS: "darwin", Arch: "arm64"}
	linuxHost := platform.Capabilities{OS: "linux", Arch: "arm64"}
	hailoDevice := func(string) ([]string, error) { return []string{"/dev/hailo0"}, nil }
	noDevice := func(string) ([]string, error) { return nil, nil }

	for _, tc := range []struct {
		name, goos string
		caps       platform.Capabilities
		glob       func(string) ([]string, error)
		wantErr    bool
		// wantLogs are messages logged exactly once; noLogs are never logged.
		wantLogs, noLogs []string
	}{
		{
			name: "mac with Metal", goos: "darwin", caps: mac, glob: hailoDevice,
			wantLogs: []string{"Metal support detected"},
			noLogs:   []string{"experimental", "Hailo device detected", "no Hailo device found"},
		},
		{
			name: "linux with a Hailo device", goos: "linux", caps: linuxHost, glob: hailoDevice,
			wantLogs: []string{"Metal not available; Linux host support is experimental", "Hailo device detected"},
			noLogs:   []string{"no Hailo device found"},
		},
		{
			name: "linux without a device still starts", goos: "linux", caps: linuxHost, glob: noDevice,
			wantLogs: []string{"Metal not available; Linux host support is experimental", "no Hailo device found"},
			noLogs:   []string{"Hailo device detected"},
		},
		{
			name: "mac without Metal", goos: "darwin", caps: platform.Capabilities{OS: "darwin"}, glob: noDevice,
			wantErr: true, noLogs: []string{"experimental"},
		},
		{
			name: "other OS", goos: "freebsd", caps: platform.Capabilities{OS: "freebsd"}, glob: hailoDevice,
			wantErr: true, noLogs: []string{"experimental", "Hailo device detected"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostGOOS = tc.goos
			detectCapabilities = func() platform.Capabilities { return tc.caps }
			globDevices = tc.glob
			core, logs := observer.New(zapcore.DebugLevel)
			caps, err := checkHostPlatform(zap.New(core).Sugar())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err == nil && caps != tc.caps {
				t.Errorf("caps = %+v, want the detected %+v passed through", caps, tc.caps)
			}
			for _, msg := range tc.wantLogs {
				if n := logs.FilterMessage(msg).Len(); n != 1 {
					t.Errorf("%q logged %d times, want once", msg, n)
				}
			}
			for _, snippet := range tc.noLogs {
				if n := logs.FilterMessageSnippet(snippet).Len(); n != 0 {
					t.Errorf("%q logged %d times, want never", snippet, n)
				}
			}
		})
	}
}

func TestLogHailoDevicesNamesTheDevices(t *testing.T) {
	origGlob := globDevices
	t.Cleanup(func() { globDevices = origGlob })
	var pattern string
	globDevices = func(p string) ([]string, error) {
		pattern = p
		return []string{"/dev/hailo0", "/dev/hailo1"}, nil
	}
	core, logs := observer.New(zapcore.InfoLevel)
	logHailoDevices(zap.New(core).Sugar())
	if pattern != hailoDeviceGlob {
		t.Errorf("globbed %q, want %q", pattern, hailoDeviceGlob)
	}
	entries := logs.FilterMessage("Hailo device detected").All()
	if len(entries) != 1 {
		t.Fatalf("Hailo device detected entries = %d, want 1", len(entries))
	}
	got, _ := entries[0].ContextMap()["devices"].([]interface{})
	if len(got) != 2 || got[0] != "/dev/hailo0" || got[1] != "/dev/hailo1" {
		t.Errorf("devices = %v, want both device nodes", entries[0].ContextMap()["devices"])
	}
}

func TestCheckHostRuntimes(t *testing.T) {
	for _, tc := range []struct {
		name, goos string
		cfg        AgentConfig
		wantErr    []string
	}{
		{name: "darwin accepts mlx-server", goos: "darwin", cfg: AgentConfig{Runtime: "mlx-server"}},
		{name: "darwin accepts the binary flags", goos: "darwin", cfg: AgentConfig{Runtime: "llama-server",
			OMLXBin: "/opt/homebrew/bin/omlx", VLLMSwiftBin: "/x", MLXServerBin: "/y", TensorFoldBin: "/z"}},
		{name: "linux accepts llama-server", goos: "linux", cfg: AgentConfig{Runtime: "llama-server"}},
		{name: "linux accepts ollama", goos: "linux", cfg: AgentConfig{Runtime: "ollama"}},
		{
			name: "linux refuses --runtime mlx-server", goos: "linux", cfg: AgentConfig{Runtime: "mlx-server"},
			wantErr: []string{
				"--runtime mlx-server", "executor runs only on macOS",
				"--runtime llama-server", "--runtime ollama",
			},
		},
		{name: "linux refuses --runtime omlx", goos: "linux", cfg: AgentConfig{Runtime: "omlx"},
			wantErr: []string{"--runtime omlx"}},
		{name: "linux refuses --runtime vllm-swift", goos: "linux", cfg: AgentConfig{Runtime: "vllm-swift"},
			wantErr: []string{"--runtime vllm-swift"}},
		{name: "linux refuses --runtime tensorfold", goos: "linux", cfg: AgentConfig{Runtime: "tensorfold"},
			wantErr: []string{"--runtime tensorfold", "executor runs only on macOS"}},
		{
			name: "linux refuses Apple-Silicon-only executor binary flags", goos: "linux",
			cfg:     AgentConfig{Runtime: "ollama", OMLXBin: "/usr/local/bin/omlx", MLXServerBin: "/usr/local/bin/mlx-server"},
			wantErr: []string{"--mlx-server-bin (mlx-server)", "--omlx-bin (omlx)", "on linux"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkHostRuntimes(&tc.cfg, tc.goos)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("checkHostRuntimes = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("checkHostRuntimes = nil, want a refusal")
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			if strings.Contains(err.Error(), "brew") {
				t.Errorf("error %q gives a brew hint off macOS", err)
			}
		})
	}
}
