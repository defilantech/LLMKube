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
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

// engineEnvAllowlist names the variables an engine inherits from the agent's
// environment. PATH lets an engine (vllm-swift's Python wrapper, a Homebrew
// shim) find its interpreter and tools; HOME, USER, LOGNAME and TMPDIR are
// the basic process identity and scratch space; HF_HOME and HF_HUB_CACHE
// locate the Hugging Face cache an MLX engine reads drafts and tokenizers
// from; LANG, TZ and every LC_* keep locale and time handling as the operator
// set them. Everything else the agent was started with (credentials, a
// kubeconfig, loader and interpreter overrides, engine argument overrides,
// proxies) stays with the agent unless --engine-env-passthrough names it.
var engineEnvAllowlist = []string{
	"PATH",
	"HOME",
	"USER",
	"LOGNAME",
	"TMPDIR",
	"LANG",
	"TZ",
	"HF_HOME",
	"HF_HUB_CACHE",
}

// engineEnvAllowedPrefix is the one prefix the allowlist matches: the POSIX
// locale categories (LC_ALL, LC_CTYPE, LC_NUMERIC, ...).
const engineEnvAllowedPrefix = "LC_"

// envNameRE is the POSIX portable environment variable name: letters, digits
// and underscore, not starting with a digit.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateEngineEnvPassthrough checks the --engine-env-passthrough names. Each
// must be an exact variable name (no '=', no wildcard), so an entry cannot
// smuggle a value in or widen into a prefix match.
func ValidateEngineEnvPassthrough(names []string) error {
	var bad []string
	for _, n := range names {
		if !envNameRE.MatchString(n) {
			bad = append(bad, fmt.Sprintf("%q", n))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("invalid environment variable name(s) %s: each must match %s",
			strings.Join(bad, ", "), envNameRE.String())
	}
	return nil
}

// buildEngineEnv returns the environment for an engine child process: the
// entries of parent whose name is on engineEnvAllowlist, starts with LC_, or
// is named exactly in passthrough, with explicit ("NAME=value") applied on
// top so an explicit variable replaces an inherited one. Each name appears
// once (an inherited duplicate keeps its first value, the one os.Getenv
// reports), and the result is sorted by name so it does not depend on the
// parent's order. Entries without a '=' or with an empty name are skipped.
//
// The result is never nil: exec.Cmd treats a nil Env as "inherit the agent's
// whole environment", the opposite of what an empty allowlist result means.
func buildEngineEnv(parent, passthrough []string, explicit ...string) []string {
	vals := make(map[string]string, len(engineEnvAllowlist)+len(passthrough)+len(explicit))
	for _, kv := range parent {
		name, val, ok := splitEnvEntry(kv)
		if !ok || !engineEnvInherits(name, passthrough) {
			continue
		}
		if _, seen := vals[name]; !seen {
			vals[name] = val
		}
	}
	for _, kv := range explicit {
		if name, val, ok := splitEnvEntry(kv); ok {
			vals[name] = val
		}
	}
	names := make([]string, 0, len(vals))
	for name := range vals {
		names = append(names, name)
	}
	slices.Sort(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+vals[name])
	}
	return env
}

func splitEnvEntry(kv string) (name, val string, ok bool) {
	name, val, ok = strings.Cut(kv, "=")
	return name, val, ok && name != ""
}

func engineEnvInherits(name string, passthrough []string) bool {
	return slices.Contains(engineEnvAllowlist, name) ||
		strings.HasPrefix(name, engineEnvAllowedPrefix) ||
		slices.Contains(passthrough, name)
}

// engineEnv is embedded in every executor that spawns an engine, so each
// spawn site builds its child's environment the same way from the same
// passthrough list. The zero value passes nothing extra through.
type engineEnv struct {
	envPassthrough []string
}

// SetEngineEnvPassthrough sets the extra variable names (exact names, see
// ValidateEngineEnvPassthrough) copied from the agent's environment into
// every engine this executor starts.
func (e *engineEnv) SetEngineEnvPassthrough(names []string) {
	e.envPassthrough = slices.Clone(names)
}

// childEnv returns the environment for an engine this executor starts: the
// agent's own environment filtered by buildEngineEnv, plus explicit.
func (e *engineEnv) childEnv(explicit ...string) []string {
	return buildEngineEnv(os.Environ(), e.envPassthrough, explicit...)
}
