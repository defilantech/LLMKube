//go:build darwin

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
	"os"
	"strings"
	"testing"
)

func TestParsePowermetricsStream_Fixture(t *testing.T) {
	f, err := os.Open("testdata/powermetrics_sample.txt")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	var samples []applePowerSample
	if err := parsePowermetricsStream(f, func(s applePowerSample) {
		samples = append(samples, s)
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(samples) == 0 {
		t.Fatal("expected at least one sample, got 0")
	}

	for i, s := range samples {
		if s.CombinedWatts <= 0 {
			t.Errorf("sample %d: CombinedWatts %.3f, want >0", i, s.CombinedWatts)
		}
		// Combined Power = CPU + GPU + ANE per powermetrics docs. Allow 50 mW
		// rounding slop because powermetrics rounds each component to whole
		// milliwatts independently.
		sum := s.CPUWatts + s.GPUWatts + s.ANEWatts
		if diff := s.CombinedWatts - sum; diff > 0.05 || diff < -0.05 {
			t.Errorf("sample %d: combined %.3f != cpu+gpu+ane %.3f", i, s.CombinedWatts, sum)
		}
		if s.CPUWatts < 0 || s.GPUWatts < 0 || s.ANEWatts < 0 {
			t.Errorf("sample %d: negative component: cpu=%.3f gpu=%.3f ane=%.3f",
				i, s.CPUWatts, s.GPUWatts, s.ANEWatts)
		}
	}
}

// TestParsePowermetricsStream_DuplicateGPULineIgnored guards the behaviour
// described in power_darwin.go: powermetrics emits "GPU Power: <n> mW" twice
// per sample (once in cpu_power, once in gpu_power) and the parser must take
// the first occurrence so the value matches what was summed into Combined.
func TestParsePowermetricsStream_DuplicateGPULineIgnored(t *testing.T) {
	input := strings.Join([]string{
		"*** Sampled system activity (header)",
		"CPU Power: 100 mW",
		"GPU Power: 50 mW",
		"ANE Power: 0 mW",
		"Combined Power (CPU + GPU + ANE): 150 mW",
		"GPU Power: 9999 mW", // duplicate from the gpu_power section
		"",
	}, "\n")

	var got []applePowerSample
	if err := parsePowermetricsStream(strings.NewReader(input), func(s applePowerSample) {
		got = append(got, s)
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want 1 sample, got %d", len(got))
	}
	if got[0].GPUWatts != 0.05 {
		t.Errorf("want GPUWatts 0.05 (first occurrence), got %.3f", got[0].GPUWatts)
	}
	if got[0].CombinedWatts != 0.15 {
		t.Errorf("want CombinedWatts 0.15, got %.3f", got[0].CombinedWatts)
	}
}

// TestParsePowermetricsStream_PartialSampleNotEmitted ensures that a sample
// missing its Combined Power line never emits — partial parses would publish
// stale-zero gauges and corrupt InferCost's per-token math.
func TestParsePowermetricsStream_PartialSampleNotEmitted(t *testing.T) {
	input := strings.Join([]string{
		"*** Sampled system activity (header 1)",
		"CPU Power: 100 mW",
		"GPU Power: 50 mW",
		// no ANE, no Combined — process killed mid-write
		"*** Sampled system activity (header 2)",
		"CPU Power: 200 mW",
		"GPU Power: 80 mW",
		"ANE Power: 0 mW",
		"Combined Power (CPU + GPU + ANE): 280 mW",
		"",
	}, "\n")

	var got []applePowerSample
	if err := parsePowermetricsStream(strings.NewReader(input), func(s applePowerSample) {
		got = append(got, s)
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want 1 sample (only the complete one), got %d", len(got))
	}
	if got[0].CombinedWatts != 0.28 {
		t.Errorf("want CombinedWatts 0.28, got %.3f", got[0].CombinedWatts)
	}
}

func TestStderrCapture_TruncatesAtMax(t *testing.T) {
	var sb strings.Builder
	c := stderrCapture{w: &sb, max: 10}
	n, err := c.Write([]byte("12345"))
	if err != nil || n != 5 {
		t.Fatalf("first write: n=%d err=%v", n, err)
	}
	n, err = c.Write([]byte("67890ABCDE"))
	if err != nil || n != 10 {
		t.Fatalf("second write: n=%d err=%v", n, err)
	}
	if got := sb.String(); got != "1234567890" {
		t.Errorf("captured %q, want %q", got, "1234567890")
	}
	// Further writes are silently swallowed.
	n, err = c.Write([]byte("XYZ"))
	if err != nil || n != 3 {
		t.Fatalf("post-cap write: n=%d err=%v", n, err)
	}
	if got := sb.String(); got != "1234567890" {
		t.Errorf("after overflow %q, want %q", got, "1234567890")
	}
}
