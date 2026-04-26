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
	"bufio"
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
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

// TestDefaultPowermetricsCommand_MatchesSudoersSpec is the regression guard
// that keeps the agent's argv aligned with the shipped sudoers fragment.
// If you change defaultPowermetricsCommand without updating
// deployment/macos/sudoers.d/llmkube-powermetrics, this test will fail —
// preventing a release where the gauges silently stop publishing because
// sudo refuses the new argv.
func TestDefaultPowermetricsCommand_MatchesSudoersSpec(t *testing.T) {
	cmd := defaultPowermetricsCommand(context.Background(), DefaultPowermetricsBin, time.Second)

	// argv[0] must be absolute /usr/bin/sudo so a $PATH attacker can't
	// substitute their own sudo wrapper to capture our (root) NOPASSWD call.
	if cmd.Path != defaultSudoBin {
		t.Errorf("sudo path = %q, want %q", cmd.Path, defaultSudoBin)
	}
	wantArgs := []string{
		defaultSudoBin, "-n",
		DefaultPowermetricsBin,
		"--samplers", "cpu_power,gpu_power",
		"-i", "1000",
	}
	if len(cmd.Args) != len(wantArgs) {
		t.Fatalf("argv = %v (len %d), want %v (len %d)",
			cmd.Args, len(cmd.Args), wantArgs, len(wantArgs))
	}
	for i, want := range wantArgs {
		if cmd.Args[i] != want {
			t.Errorf("argv[%d] = %q, want %q", i, cmd.Args[i], want)
		}
	}

	// Cross-check against the actual sudoers fragment we ship. Strip the
	// `User Host = (root) NOPASSWD:` prefix and what's left must match the
	// post-binary portion of our argv with the dynamic interval rendered.
	specPattern := readShippedSudoersSpec(t)
	postBinary := strings.Join(cmd.Args[3:], " ") // drop sudo, -n, /usr/bin/powermetrics
	// sudoers escapes the comma in cpu_power\,gpu_power; strip the backslash
	// before matching against the rendered argv.
	specPattern = strings.ReplaceAll(specPattern, `\,`, ",")
	// "[0-9]*" in the sudoers spec is shell-glob, not regex. Translate.
	regexPattern := "^" + strings.ReplaceAll(regexp.QuoteMeta(specPattern), `\[0-9\]\*`, `[0-9]+`) + "$"
	if !regexp.MustCompile(regexPattern).MatchString(postBinary) {
		t.Errorf("agent argv %q does not satisfy shipped sudoers spec %q (regex %q)",
			postBinary, specPattern, regexPattern)
	}
}

// TestNewApplePowerSampler_RejectsNonDefaultBin guards H-2 from the security
// audit: the --powermetrics-bin override flag must not produce a working
// sampler against a path the shipped sudoers entry doesn't match.
func TestNewApplePowerSampler_RejectsNonDefaultBin(t *testing.T) {
	logger := zap.NewNop().Sugar()
	s := NewApplePowerSampler("/tmp/not-the-real-powermetrics", time.Second, logger)
	if !s.disabled {
		t.Fatal("expected sampler to be disabled when bin diverges from default")
	}
	// Run must be a fast no-op — if the disable check is missed it would try
	// to fork sudo, which we definitely don't want in unit tests.
	done := make(chan struct{})
	go func() {
		s.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disabled sampler Run did not return promptly")
	}
}

// readShippedSudoersSpec extracts the actual command-spec from the file we
// ship at deployment/macos/sudoers.d/llmkube-powermetrics. Skipping comments
// and the user/host preamble, we keep just what comes after `NOPASSWD:`.
func readShippedSudoersSpec(t *testing.T) string {
	t.Helper()
	const path = "../../deployment/macos/sudoers.d/llmkube-powermetrics"
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open shipped sudoers fragment %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "NOPASSWD:")
		if idx < 0 {
			continue
		}
		spec := strings.TrimSpace(line[idx+len("NOPASSWD:"):])
		// Drop the binary path itself; we only need the arg portion.
		spec = strings.TrimPrefix(spec, DefaultPowermetricsBin+" ")
		return spec
	}
	t.Fatalf("no NOPASSWD line found in %s", path)
	return ""
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
