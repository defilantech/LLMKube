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

package metaljoin

import "strings"

// StopEnginesCommand is the metal-agent subcommand that stops every engine the
// agent started. llmkube metal leave runs it, and tells an agent binary that
// has it from one that does not by finding this word in the binary: an agent
// without it would parse the word as a stray argument and start serving.
const StopEnginesCommand = "stop-engines"

// StopEnginesArgs is the command line, after the binary, that stops the
// engines of an agent started with agentArgs (its LaunchAgent arguments after
// the binary): the agent's kubeconfig, or defaultKubeconfig when it names
// none, and its namespace, state directory and Ollama port when it sets them.
func StopEnginesArgs(agentArgs []string, defaultKubeconfig string) []string {
	args := []string{StopEnginesCommand, "--kubeconfig", flagValue(agentArgs, "kubeconfig", defaultKubeconfig)}
	for _, name := range []string{"namespace", "state-dir", "ollama-port"} {
		if v := flagValue(agentArgs, name, ""); v != "" {
			args = append(args, "--"+name, v)
		}
	}
	return args
}

// flagValue returns the value args give flag name, read the way the agent's
// flag package reads it: -name or --name, then =value or the next argument,
// the last one winning; def when none does.
func flagValue(args []string, name, def string) string {
	v := def
	for i := 0; i < len(args); i++ {
		a, ok := strings.CutPrefix(args[i], "-")
		if !ok {
			continue
		}
		a = strings.TrimPrefix(a, "-")
		if n, val, ok := strings.Cut(a, "="); ok {
			if n == name {
				v = val
			}
			continue
		}
		if a == name && i+1 < len(args) {
			v = args[i+1]
			i++
		}
	}
	return v
}
