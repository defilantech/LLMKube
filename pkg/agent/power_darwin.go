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
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// DefaultPowermetricsBin is the canonical macOS powermetrics binary path. It
// requires root, which the agent reaches via a NOPASSWD sudoers entry rather
// than by running as root itself. See deployment/macos/sudoers.d/llmkube-powermetrics.
const DefaultPowermetricsBin = "/usr/bin/powermetrics"

// DefaultApplePowerInterval is the powermetrics sampling cadence. 1s matches
// the granularity needed for InferCost to attribute decode-vs-idle power on
// short bursts; tighter intervals don't add value because powermetrics already
// rounds to whole milliseconds.
const DefaultApplePowerInterval = time.Second

// powermetrics emits "<label>: <integer> mW" lines after each sample header.
// Within one sample the cpu_power section emits CPU/GPU/ANE/Combined and the
// gpu_power section then emits GPU again — we only update gauges on the
// *first* occurrence of each label between sample boundaries so the duplicate
// GPU line in the gpu_power section is ignored.
var (
	powerSampleHeaderRE = regexp.MustCompile(`^\*\*\* Sampled system activity`)
	powerLineRE         = regexp.MustCompile(
		`^(CPU Power|GPU Power|ANE Power|Combined Power[^:]*): (\d+) mW`,
	)
)

// applePowerSample is one parsed powermetrics window. Watts are floats because
// powermetrics reports milliwatts as integers and we divide by 1000.
type applePowerSample struct {
	CPUWatts      float64
	GPUWatts      float64
	ANEWatts      float64
	CombinedWatts float64
}

// ApplePowerSampler runs `sudo powermetrics --samplers cpu_power,gpu_power -i
// <interval>` as a long-lived child process and updates the apple_power_*
// Prometheus gauges from each emitted sample. Cancel ctx in Run to stop the
// child cleanly.
type ApplePowerSampler struct {
	bin      string
	interval time.Duration
	logger   *zap.SugaredLogger

	// disabled is true when NewApplePowerSampler refused to construct a real
	// sampler (e.g. because --powermetrics-bin was overridden). Run becomes a
	// no-op so the agent keeps running without power data rather than crashing.
	disabled bool

	// commandFactory builds the exec.Cmd. Overridden in tests so the parser
	// can be exercised without spawning powermetrics.
	commandFactory func(ctx context.Context, bin string, interval time.Duration) *exec.Cmd
}

// NewApplePowerSampler constructs a sampler. Empty bin or zero interval fall
// back to DefaultPowermetricsBin / DefaultApplePowerInterval.
//
// SECURITY: bin is locked to DefaultPowermetricsBin. The shipped sudoers entry
// grants NOPASSWD only for that exact path; allowing an arbitrary --powermetrics-bin
// would let an operator point the agent at a binary the sudoers spec doesn't
// match (silently broken, no power data) — or worse, line up a path that
// happens to match a future, looser sudoers rule. We refuse to construct a
// real sampler when bin diverges and return a no-op instead so the agent keeps
// running without power data rather than failing closed.
func NewApplePowerSampler(bin string, interval time.Duration, logger *zap.SugaredLogger) *ApplePowerSampler {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	if bin == "" {
		bin = DefaultPowermetricsBin
	}
	if bin != DefaultPowermetricsBin {
		logger.Warnw("apple-power: refusing to launch with non-default powermetrics bin — "+
			"the shipped sudoers entry is pinned to "+DefaultPowermetricsBin+
			"; remove --powermetrics-bin or rebuild from source if you really need a different path",
			"requestedBin", bin, "expectedBin", DefaultPowermetricsBin)
		return &ApplePowerSampler{disabled: true, logger: logger}
	}
	if interval <= 0 {
		interval = DefaultApplePowerInterval
	}
	return &ApplePowerSampler{
		bin:            bin,
		interval:       interval,
		logger:         logger,
		commandFactory: defaultPowermetricsCommand,
	}
}

// Run launches powermetrics under sudo and parses its stdout until ctx is
// cancelled or the child exits. It logs and returns on terminal errors but
// does not panic the agent — Apple power data is purely additive.
func (s *ApplePowerSampler) Run(ctx context.Context) {
	if s.disabled {
		s.logger.Infow("apple-power: sampler disabled (see prior warning); skipping")
		return
	}
	cmd := s.commandFactory(ctx, s.bin, s.interval)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.logger.Warnw("apple-power: stdout pipe failed", "error", err)
		return
	}
	stderrBuf := &strings.Builder{}
	cmd.Stderr = stderrCapture{w: stderrBuf, max: 4096}

	if err := cmd.Start(); err != nil {
		s.logger.Warnw("apple-power: failed to start powermetrics — "+
			"is the NOPASSWD sudoers entry installed?",
			"bin", s.bin, "error", err)
		return
	}
	s.logger.Infow("apple-power: sampling started",
		"bin", s.bin, "interval", s.interval, "pid", cmd.Process.Pid)

	parseDone := make(chan struct{})
	go func() {
		defer close(parseDone)
		if perr := parsePowermetricsStream(stdout, applyPowerSample); perr != nil {
			s.logger.Warnw("apple-power: parser error", "error", perr)
		}
	}()

	// Wait for either ctx cancellation or the powermetrics child to exit on
	// its own. On cancellation we send SIGTERM to give powermetrics a chance
	// to flush its sampler before the parent reaps it.
	select {
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	case <-parseDone:
	}

	waitErr := cmd.Wait()
	<-parseDone
	if waitErr != nil && ctx.Err() == nil {
		s.logger.Warnw("apple-power: powermetrics exited unexpectedly",
			"error", waitErr, "stderr", strings.TrimSpace(stderrBuf.String()))
	} else {
		s.logger.Infow("apple-power: sampling stopped")
	}
}

// applyPowerSample writes a parsed sample into the Prometheus gauges. Split
// out so parsePowermetricsStream is testable without touching real metrics.
func applyPowerSample(s applePowerSample) {
	applePowerCPUWatts.Set(s.CPUWatts)
	applePowerGPUWatts.Set(s.GPUWatts)
	applePowerANEWatts.Set(s.ANEWatts)
	applePowerCombinedWatts.Set(s.CombinedWatts)
}

// parsePowermetricsStream reads powermetrics text output line by line, builds
// one applePowerSample per sample-header boundary, and invokes emit when the
// sample's Combined Power line is seen. It returns when r reaches EOF or
// errors. Designed to be driven from a goroutine; never panics on malformed
// input.
func parsePowermetricsStream(r io.Reader, emit func(applePowerSample)) error {
	scanner := bufio.NewScanner(r)
	// powermetrics emits very long CPU-frequency lines (~2-4 KB). Bump the
	// buffer past the bufio default so we don't truncate mid-sample.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		current  applePowerSample
		seenCPU  bool
		seenGPU  bool
		seenANE  bool
		seenComb bool
		inSample bool
	)

	resetSample := func() {
		current = applePowerSample{}
		seenCPU, seenGPU, seenANE, seenComb = false, false, false, false
	}

	for scanner.Scan() {
		line := scanner.Text()
		if powerSampleHeaderRE.MatchString(line) {
			// New sample boundary. Emit the previous sample only if we got
			// the Combined Power reading — anything less is a partial parse
			// and would publish stale zeros.
			if inSample && seenComb {
				emit(current)
			}
			resetSample()
			inSample = true
			continue
		}
		if !inSample {
			continue
		}
		m := powerLineRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		mw, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		watts := float64(mw) / 1000.0
		switch {
		case strings.HasPrefix(m[1], "CPU Power"):
			if !seenCPU {
				current.CPUWatts = watts
				seenCPU = true
			}
		case strings.HasPrefix(m[1], "GPU Power"):
			if !seenGPU {
				current.GPUWatts = watts
				seenGPU = true
			}
		case strings.HasPrefix(m[1], "ANE Power"):
			if !seenANE {
				current.ANEWatts = watts
				seenANE = true
			}
		case strings.HasPrefix(m[1], "Combined Power"):
			if !seenComb {
				current.CombinedWatts = watts
				seenComb = true
			}
		}
	}

	// Flush final sample on EOF.
	if inSample && seenComb {
		emit(current)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("powermetrics scanner: %w", err)
	}
	return nil
}

// defaultSudoBin is the absolute path to sudo. Hard-coded so a $PATH attacker
// can't substitute their own sudo wrapper to capture the (root) NOPASSWD
// invocation. macOS ships sudo at /usr/bin/sudo and SIP keeps it there.
const defaultSudoBin = "/usr/bin/sudo"

// defaultPowermetricsCommand builds the actual sudo invocation. Split out so
// tests can swap in a fixture-replay command.
//
// SECURITY: the argv emitted here is what the shipped sudoers fragment is
// pinned to. If you change flags, update deployment/macos/sudoers.d/llmkube-powermetrics
// in lockstep — and the regression test in power_darwin_test.go that asserts
// the two stay in sync — or the agent will silently lose power data after
// upgrade because sudoers will no longer match the new argv.
func defaultPowermetricsCommand(ctx context.Context, bin string, interval time.Duration) *exec.Cmd {
	intervalMS := int(interval / time.Millisecond)
	if intervalMS < 100 {
		intervalMS = 100
	}
	return exec.CommandContext(ctx,
		defaultSudoBin, "-n", bin,
		"--samplers", "cpu_power,gpu_power",
		"-i", strconv.Itoa(intervalMS),
	)
}

// stderrCapture is an io.Writer that retains at most max bytes of stderr,
// useful for surfacing the actual sudo/powermetrics failure reason in logs
// without unbounded memory growth.
type stderrCapture struct {
	w   *strings.Builder
	max int
}

func (c stderrCapture) Write(p []byte) (int, error) {
	remaining := c.max - c.w.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		c.w.Write(p[:remaining])
	} else {
		c.w.Write(p)
	}
	return len(p), nil
}
