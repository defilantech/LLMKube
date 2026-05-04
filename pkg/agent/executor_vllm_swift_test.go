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
	"encoding/json"
	"testing"
	"time"
)

func TestNewVLLMSwiftExecutor(t *testing.T) {
	executor := NewVLLMSwiftExecutor("/opt/homebrew/bin/vllm-swift", "/models", newNopLogger())

	if executor.bin != "/opt/homebrew/bin/vllm-swift" {
		t.Errorf("bin = %q, want %q", executor.bin, "/opt/homebrew/bin/vllm-swift")
	}
	if executor.modelStorePath != "/models" {
		t.Errorf("modelStorePath = %q, want %q", executor.modelStorePath, "/models")
	}
	if executor.startupTimeout != DefaultVLLMSwiftStartupTimeout {
		t.Errorf("default startupTimeout = %v, want %v",
			executor.startupTimeout, DefaultVLLMSwiftStartupTimeout)
	}
}

func TestVLLMSwiftSetStartupTimeout(t *testing.T) {
	executor := NewVLLMSwiftExecutor("/bin/vllm-swift", "/models", newNopLogger())

	executor.SetStartupTimeout(180 * time.Second)
	if executor.startupTimeout != 180*time.Second {
		t.Errorf("after Set(180s) = %v, want 180s", executor.startupTimeout)
	}

	// Non-positive values coerce back to default.
	executor.SetStartupTimeout(0)
	if executor.startupTimeout != DefaultVLLMSwiftStartupTimeout {
		t.Errorf("after Set(0) = %v, want default %v",
			executor.startupTimeout, DefaultVLLMSwiftStartupTimeout)
	}
	executor.SetStartupTimeout(-5 * time.Second)
	if executor.startupTimeout != DefaultVLLMSwiftStartupTimeout {
		t.Errorf("after Set(-5s) = %v, want default %v",
			executor.startupTimeout, DefaultVLLMSwiftStartupTimeout)
	}
}

func TestVLLMSwiftStopProcess_InvalidPID(t *testing.T) {
	executor := NewVLLMSwiftExecutor("/bin/vllm-swift", "/models", newNopLogger())

	err := executor.StopProcess(-99999)
	if err == nil {
		t.Error("StopProcess with invalid PID should return error")
	}
}

func TestTurboQuantConfig(t *testing.T) {
	tests := []struct {
		cacheType  string
		wantScheme string
		wantBits   int
	}{
		{"turbo4v2", "turbo4v2", 4},
		{"turbo4", "turbo4", 4},
		{"turbo3", "turbo3", 3},
		{"turbo2", "turbo2", 2},
		{"", "", 0},
		{"f16", "", 0},
		{"q8_0", "", 0},
		{"iq4_nl", "", 0},
		{"TURBO4V2", "", 0}, // case-sensitive, upstream uses lowercase
	}

	for _, tc := range tests {
		t.Run(tc.cacheType, func(t *testing.T) {
			scheme, bits := turboQuantConfig(tc.cacheType)
			if scheme != tc.wantScheme {
				t.Errorf("scheme = %q, want %q", scheme, tc.wantScheme)
			}
			if bits != tc.wantBits {
				t.Errorf("bits = %d, want %d", bits, tc.wantBits)
			}
		})
	}
}

func TestBuildVLLMSwiftArgs_Defaults(t *testing.T) {
	args := buildVLLMSwiftArgs("/models/Qwen3-4B-4bit", 8080, ExecutorConfig{
		ContextSize: 32768,
	})

	// Positional model path comes after "serve" subcommand.
	if len(args) < 2 || args[0] != "serve" {
		t.Fatalf("first arg must be \"serve\" subcommand, got: %v", args)
	}
	if args[1] != "/models/Qwen3-4B-4bit" {
		t.Errorf("model path = %q, want %q (full args: %v)",
			args[1], "/models/Qwen3-4B-4bit", args)
	}

	want := map[string]string{
		"--port":          "8080",
		"--max-model-len": "32768",
	}
	for flag, expected := range want {
		if got := flagValue(args, flag); got != expected {
			t.Errorf("%s = %q, want %q (full args: %v)", flag, got, expected, args)
		}
	}

	// Defaults must NOT inject TurboQuant or seq concurrency.
	for _, unwanted := range []string{
		"--additional-config", "--max-num-seqs", "--gpu-memory-utilization",
	} {
		if hasFlag(args, unwanted) {
			t.Errorf("unexpected flag %q in default args: %v", unwanted, args)
		}
	}
}

func TestBuildVLLMSwiftArgs_ParallelSlots(t *testing.T) {
	args := buildVLLMSwiftArgs("/m", 8080, ExecutorConfig{
		ContextSize:   4096,
		ParallelSlots: 8,
	})
	if got := flagValue(args, "--max-num-seqs"); got != "8" {
		t.Errorf("--max-num-seqs = %q, want %q (full args: %v)", got, "8", args)
	}
}

func TestBuildVLLMSwiftArgs_ParallelSlotsOneOrZeroOmits(t *testing.T) {
	for _, n := range []int{0, 1} {
		args := buildVLLMSwiftArgs("/m", 8080, ExecutorConfig{
			ContextSize:   4096,
			ParallelSlots: n,
		})
		if hasFlag(args, "--max-num-seqs") {
			t.Errorf("--max-num-seqs must be omitted for ParallelSlots=%d (full args: %v)",
				n, args)
		}
	}
}

func TestBuildVLLMSwiftArgs_TurboQuant(t *testing.T) {
	tests := []struct {
		name       string
		cacheTypeK string
		wantScheme string
		wantBits   int
	}{
		{"turbo4v2_recommended", "turbo4v2", "turbo4v2", 4},
		{"turbo3_max_compression", "turbo3", "turbo3", 3},
		{"turbo4_legacy", "turbo4", "turbo4", 4},
		{"turbo2_aggressive", "turbo2", "turbo2", 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := buildVLLMSwiftArgs("/m", 8080, ExecutorConfig{
				ContextSize: 131072,
				CacheTypeK:  tc.cacheTypeK,
			})
			raw := flagValue(args, "--additional-config")
			if raw == "" {
				t.Fatalf("--additional-config missing for %s (full args: %v)", tc.cacheTypeK, args)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("--additional-config value is not valid JSON: %q (err %v)", raw, err)
			}
			if got["kv_scheme"] != tc.wantScheme {
				t.Errorf("kv_scheme = %v, want %q", got["kv_scheme"], tc.wantScheme)
			}
			// JSON numbers decode to float64.
			if int(got["kv_bits"].(float64)) != tc.wantBits {
				t.Errorf("kv_bits = %v, want %d", got["kv_bits"], tc.wantBits)
			}
		})
	}
}

func TestBuildVLLMSwiftArgs_TurboQuantOmittedForNonTurboCacheTypes(t *testing.T) {
	for _, ct := range []string{"", "f16", "q8_0", "iq4_nl"} {
		args := buildVLLMSwiftArgs("/m", 8080, ExecutorConfig{
			ContextSize: 4096,
			CacheTypeK:  ct,
		})
		if hasFlag(args, "--additional-config") {
			t.Errorf("--additional-config must be omitted for non-turbo CacheTypeK=%q (full args: %v)",
				ct, args)
		}
	}
}

func TestBuildVLLMSwiftArgs_ExtraArgsAppendedLast(t *testing.T) {
	args := buildVLLMSwiftArgs("/m", 8080, ExecutorConfig{
		ContextSize: 4096,
		ExtraArgs:   []string{"--enable-reasoning", "--reasoning-parser", "deepseek_r1"},
	})
	if len(args) < 3 {
		t.Fatalf("args too short: %v", args)
	}
	tail := args[len(args)-3:]
	want := []string{"--enable-reasoning", "--reasoning-parser", "deepseek_r1"}
	for i, w := range want {
		if tail[i] != w {
			t.Errorf("tail[%d] = %q, want %q (full args: %v)", i, tail[i], w, args)
		}
	}
}

func TestBuildVLLMSwiftArgs_TurboQuantPlusParallelSlots(t *testing.T) {
	// Real-world coding-model invocation: TurboQuant for long context PLUS
	// parallel slots for concurrent agent requests. Both must be present.
	args := buildVLLMSwiftArgs("/models/qwen", 8080, ExecutorConfig{
		ContextSize:   131072,
		ParallelSlots: 4,
		CacheTypeK:    "turbo4v2",
	})
	if got := flagValue(args, "--max-num-seqs"); got != "4" {
		t.Errorf("--max-num-seqs = %q, want %q", got, "4")
	}
	if !hasFlag(args, "--additional-config") {
		t.Errorf("--additional-config missing in combined invocation: %v", args)
	}
}
