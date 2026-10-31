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

package agent

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// helpProbeTimeout bounds `llama-server --help`. The probe runs on the spawn
// path, which the agent serializes, so a wedged binary must not stall it. A
// variable so tests can shorten it.
var helpProbeTimeout = 10 * time.Second

// loadModeCache remembers whether the executor's llama-server accepts
// --load-mode. Only a successful probe is cached: a failed one falls back to
// --mlock for that spawn and is retried on the next, so a transient failure
// (a cold disk on first exec, say) does not pin a --load-mode build to the
// flag it rejects for the life of the agent.
type loadModeCache struct {
	mu        sync.Mutex
	probed    bool
	supported bool
	warned    bool
}

// execHelpProbe runs `<bin> --help` with env as its whole environment and
// returns its combined output. The probe runs the operator's llama-server
// binary, so it gets the same environment an engine spawn does, not the
// agent's.
func execHelpProbe(ctx context.Context, bin string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, helpProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--help")
	cmd.Env = env
	// A child that forks and leaves the output pipe open must not hold the
	// probe past its deadline either.
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// helpSupportsLoadMode reports whether llama-server help text lists the
// --load-mode option. It keys on the option itself: 0.5.0's help still says
// "mlock" in the mode descriptions, so the absence of --mlock is no signal.
func helpSupportsLoadMode(help string) bool {
	return strings.Contains(help, "--load-mode")
}

// supportsLoadMode probes the executor's llama-server once and caches the
// answer; the binary path is fixed for the executor's life. A probe failure
// returns false (keep --mlock, which every build before 0.5.0 accepts) and
// logs a warning the first time only.
func (e *MetalExecutor) supportsLoadMode(ctx context.Context) bool {
	c := &e.loadMode
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.probed {
		return c.supported
	}

	help, err := e.helpProbe(ctx, e.llamaServerBin, e.llamaServerEnv())
	if err != nil {
		if !c.warned {
			c.warned = true
			e.logger.Warnw("llama-server --help probe failed; falling back to --mlock",
				"bin", e.llamaServerBin, "error", err)
		}
		return false
	}

	c.probed = true
	c.supported = helpSupportsLoadMode(help)
	e.logger.Infow("detected llama-server load flag",
		"bin", e.llamaServerBin, "loadModeSupported", c.supported)
	return c.supported
}

// llamaServerArgs fills the binary-dependent fields of config and builds the
// command line. buildLlamaServerArgs stays pure; the probe lives here.
func (e *MetalExecutor) llamaServerArgs(
	ctx context.Context,
	modelPath string,
	port int,
	config ExecutorConfig,
) []string {
	if config.Mlock && !hasLoadModeExtraArg(config.ExtraArgs) {
		config.LoadModeSupported = e.supportsLoadMode(ctx)
	}
	return buildLlamaServerArgs(modelPath, port, config)
}
