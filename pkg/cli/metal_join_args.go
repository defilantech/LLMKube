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

package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/defilantech/llmkube/pkg/metalnode"
)

// agentFlagKind is how the agent's flag package reads a flag's value.
type agentFlagKind int

const (
	agentFlagString agentFlagKind = iota
	agentFlagBool
	agentFlagInt
	agentFlagFloat
	agentFlagDuration
)

// metalAgentFlags are llmkube-metal-agent's flags (its -help). join checks an
// --agent-arg against them before it spends the single-use join string: an
// agent started with a flag it does not have, or a value it cannot parse,
// exits at once, and launchd restarts it every 30 seconds.
// TestMetalAgentFlagsMatchTheAgent keeps the table in step with cmd/metal-agent.
var metalAgentFlags = map[string]agentFlagKind{
	"allow-model-memory-budget-override": agentFlagBool,
	"allow-unlabeled-source-secrets":     agentFlagBool,
	"allow-unsafe-extra-args":            agentFlagBool,
	"allowed-download-hosts":             agentFlagString,
	"allowed-model-roots":                agentFlagString,
	"apple-power-enabled":                agentFlagBool,
	"apple-power-interval":               agentFlagDuration,
	"client-port":                        agentFlagInt,
	"engine-env-passthrough":             agentFlagString,
	"eviction-enabled":                   agentFlagBool,
	"hailo-library-url":                  agentFlagString,
	"hailo-manifests-dir":                agentFlagString,
	"health-failure-threshold":           agentFlagInt,
	"host-ip":                            agentFlagString,
	"inference-service-allowlist":        agentFlagString,
	"ingress-port":                       agentFlagInt,
	"kube-context":                       agentFlagString,
	"kubeconfig":                         agentFlagString, // controller-runtime's client/config registers it
	"legacy-direct-endpoints":            agentFlagBool,
	"llama-server":                       agentFlagString,
	"llama-server-port":                  agentFlagInt,
	"llama-server-startup-timeout":       agentFlagDuration,
	"log-level":                          agentFlagString,
	"max-download-size":                  agentFlagString,
	"max-watch-failures":                 agentFlagInt,
	"memory-check-mode":                  agentFlagString,
	"memory-fraction":                    agentFlagFloat,
	"memory-pressure-critical":           agentFlagFloat,
	"memory-pressure-warning":            agentFlagFloat,
	"memory-watchdog-interval":           agentFlagDuration,
	"metal-node":                         agentFlagString,
	"metal-node-lease":                   agentFlagString,
	"mlx-server-bin":                     agentFlagString,
	"mlx-server-port":                    agentFlagInt,
	"mlx-server-startup-timeout":         agentFlagDuration,
	"model-store":                        agentFlagString,
	"namespace":                          agentFlagString,
	"ollama-port":                        agentFlagInt,
	"omlx-bin":                           agentFlagString,
	"omlx-port":                          agentFlagInt,
	"omlx-startup-timeout":               agentFlagDuration,
	"port":                               agentFlagInt,
	"powermetrics-bin":                   agentFlagString,
	"renew-service-account-token":        agentFlagBool,
	"runtime":                            agentFlagString,
	"state-dir":                          agentFlagString,
	"tensorfold-bin":                     agentFlagString,
	"tensorfold-startup-timeout":         agentFlagDuration,
	"version":                            agentFlagBool,
	"vllm-swift-bin":                     agentFlagString,
	"vllm-swift-startup-timeout":         agentFlagDuration,
}

// metalJoinManagedFlags are the agent flags join sets itself, or that would
// undo what it sets up, and why. --agent-arg refuses them, and a re-join does
// not carry them over from the previous LaunchAgent.
var metalJoinManagedFlags = map[string]string{
	"kubeconfig":                  "join writes the agent's kubeconfig",
	"kube-context":                "join's kubeconfig has one context, the Mac's own",
	"namespace":                   "the join string sets it",
	"metal-node":                  "the join string sets it",
	"metal-node-lease":            "the Mac's Lease is the one token create made",
	"renew-service-account-token": "join turns it on so the agent keeps its token current",
	"llama-server":                "use join's --llama-server",
	"host-ip":                     "use join's --host-ip",
	"inference-service-allowlist": "use join's --allowlist",
	"legacy-direct-endpoints":     "join sets the agent up behind its authenticated ingress",
	"version":                     "the agent would print its version and exit",
}

// metalJoinCarriedOwnFlags are managed flags a re-join still carries over:
// join has a flag of its own for each, which replaces the carried value.
var metalJoinCarriedOwnFlags = map[string]bool{"host-ip": true, "inference-service-allowlist": true}

// metalAgentLogLevelDefault is the --log-level join writes unless one is
// given or carried over.
const metalAgentLogLevelDefault = "info"

// agentArg is one flag on the agent's command line.
type agentArg struct {
	name     string
	value    string
	hasValue bool
}

// String is the argument as join writes it: one token, so reading it back
// never depends on whether the flag takes a value.
func (a agentArg) String() string {
	if !a.hasValue {
		return "--" + a.name
	}
	return "--" + a.name + "=" + a.value
}

// cutFlagName strips the one or two dashes Go's flag package accepts, and
// reports whether arg is a flag at all ("-", "--" and plain words are not).
func cutFlagName(arg string) (string, bool) {
	name, ok := strings.CutPrefix(arg, "-")
	if !ok {
		return "", false
	}
	name = strings.TrimPrefix(name, "-")
	if name == "" || name[0] == '-' || name[0] == '=' {
		return "", false
	}
	return name, true
}

// parseAgentArgs reads the agent's arguments after the binary the way its flag
// package does. A flag without "=" takes the next argument as its value unless
// it is a bool; for a flag this llmkube does not know, the next argument is
// its value when it does not start with "-". Anything else is refused: the
// agent stops reading flags at the first word that is not one.
func parseAgentArgs(args []string) ([]agentArg, error) {
	var out []agentArg
	for i := 0; i < len(args); i++ {
		name, ok := cutFlagName(args[i])
		if !ok {
			return nil, fmt.Errorf("%q is not a flag", args[i])
		}
		if n, v, ok := strings.Cut(name, "="); ok {
			out = append(out, agentArg{name: n, value: v, hasValue: true})
			continue
		}
		kind, known := metalAgentFlags[name]
		next := i+1 < len(args)
		takesValue := known && kind != agentFlagBool || !known && next && !strings.HasPrefix(args[i+1], "-")
		if !takesValue {
			out = append(out, agentArg{name: name})
			continue
		}
		if !next {
			return nil, fmt.Errorf("--%s has no value", name)
		}
		out = append(out, agentArg{name: name, value: args[i+1], hasValue: true})
		i++
	}
	return out, nil
}

// parseAgentArgFlag reads one --agent-arg: <flag>=<value>, or <flag> alone for
// a bool, with or without leading dashes. The value must parse the way the
// agent parses it, so a typo fails here and not in a launchd restart loop.
func parseAgentArgFlag(s string) (agentArg, error) {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(s, "-"), "-")
	name, value, hasValue := strings.Cut(trimmed, "=")
	if name == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, " \t") {
		return agentArg{}, fmt.Errorf("--agent-arg %q: want <flag>=<value>, such as port=9091", s)
	}
	if why, ok := metalJoinManagedFlags[name]; ok {
		return agentArg{}, fmt.Errorf("--agent-arg %s: %s", name, why)
	}
	kind, ok := metalAgentFlags[name]
	if !ok {
		return agentArg{}, fmt.Errorf("--agent-arg %s: llmkube-metal-agent has no --%s flag "+
			"(if your agent is newer than this llmkube, upgrade llmkube)", name, name)
	}
	if !hasValue {
		if kind != agentFlagBool {
			return agentArg{}, fmt.Errorf("--agent-arg %s needs a value: %s=<value>", name, name)
		}
		return agentArg{name: name}, nil
	}
	var err error
	switch kind {
	case agentFlagBool:
		_, err = strconv.ParseBool(value)
	case agentFlagInt:
		_, err = strconv.ParseInt(value, 0, strconv.IntSize)
	case agentFlagFloat:
		_, err = strconv.ParseFloat(value, 64)
	case agentFlagDuration:
		_, err = time.ParseDuration(value)
	}
	if err != nil {
		return agentArg{}, fmt.Errorf("--agent-arg %s: the agent cannot read %q: %w", name, value, err)
	}
	return agentArg{name: name, value: value, hasValue: true}, nil
}

// validateMetalJoinHostIP checks --host-ip. The agent registers it in an IPv4
// EndpointSlice and offers it first to the MetalNode dial-back, so it must be
// an IPv4 address the cluster can route to.
func validateMetalJoinHostIP(s string) error {
	ip := net.ParseIP(s)
	switch {
	case ip == nil || ip.To4() == nil || strings.Contains(s, ":"):
		return fmt.Errorf("--host-ip %q is not an IPv4 address: the agent registers it in an IPv4 EndpointSlice", s)
	case ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast():
		return fmt.Errorf("--host-ip %s: the cluster cannot reach the Mac at a loopback, link-local, multicast "+
			"or unspecified address", s)
	}
	return nil
}

// normalizeMetalJoinAllowlist checks --allowlist, InferenceService names
// separated by commas, and returns it as the agent's
// --inference-service-allowlist.
func normalizeMetalJoinAllowlist(s string) (string, error) {
	var names []string
	for _, n := range strings.Split(s, ",") {
		n = strings.TrimSpace(n)
		if errs := validation.IsDNS1123Subdomain(n); len(errs) > 0 {
			return "", fmt.Errorf("--allowlist %q: %q is not an InferenceService name: %s", s, n, strings.Join(errs, "; "))
		}
		names = append(names, n)
	}
	return strings.Join(names, ","), nil
}

// metalJoinExtra is an agent argument given on this join: a value, or with
// drop, an empty --host-ip or --allowlist that removes a carried one.
type metalJoinExtra struct {
	arg  agentArg
	drop bool
}

// previousJoinAgentArgs reads the agent's arguments after the binary from a
// LaunchAgent an earlier join wrote. It returns nil for no plist, and for one
// join did not write: --force replaces that one without taking its arguments.
func previousJoinAgentArgs(path string) ([]agentArg, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if !isJoinPlist(raw) {
		return nil, nil
	}
	args, err := metalAgentPlistArgs(raw)
	if err == nil && len(args) == 0 {
		err = errors.New("ProgramArguments is empty")
	}
	if err != nil {
		return nil, fmt.Errorf("read the agent arguments the previous join wrote in %s, which join keeps: %w", path, err)
	}
	parsed, err := parseAgentArgs(args[1:])
	if err != nil {
		return nil, fmt.Errorf("read the agent arguments in %s, which join keeps: %w; "+
			"fix the plist and run join again", path, err)
	}
	return parsed, nil
}

// metalJoinArgPlan is the agent's command line beyond what join computes
// from the join string, and what join tells the user about it.
type metalJoinArgPlan struct {
	logLevel string
	extras   []agentArg
	notes    []string
}

// planMetalAgentArgs merges the arguments of the previous join's LaunchAgent
// with the ones given now, which win. joinSets are the flags join writes
// itself: a previous value that differs from those is not kept, and said so.
func planMetalAgentArgs(previous []agentArg, given []metalJoinExtra, joinSets map[string]agentArg) metalJoinArgPlan {
	var plan metalJoinArgPlan
	var args []agentArg
	fromPrevious := map[string]bool{}
	index := func(name string) int {
		for i, a := range args {
			if a.name == name {
				return i
			}
		}
		return -1
	}
	for _, a := range previous {
		if why, ok := metalJoinManagedFlags[a.name]; ok && !metalJoinCarriedOwnFlags[a.name] {
			if own, ok := joinSets[a.name]; !ok || own != a {
				plan.notes = append(plan.notes, fmt.Sprintf("not keeping %s from the previous join: %s", a, why))
			}
			continue
		}
		// The agent reads the last of a repeated flag.
		if i := index(a.name); i >= 0 {
			args = append(args[:i], args[i+1:]...)
		}
		if a.name == "log-level" && a.value == metalAgentLogLevelDefault {
			// join's own default, which it writes anyway: not the user's.
			continue
		}
		args = append(args, a)
		fromPrevious[a.name] = true
	}
	for _, g := range given {
		i := index(g.arg.name)
		switch {
		case i >= 0 && g.drop:
			plan.notes = append(plan.notes, fmt.Sprintf("dropping %s from the previous join", args[i]))
			args = append(args[:i], args[i+1:]...)
		case i >= 0:
			if args[i] != g.arg {
				plan.notes = append(plan.notes, fmt.Sprintf("%s replaces %s from the previous join", g.arg, args[i]))
			}
			args[i] = g.arg
		case !g.drop:
			args = append(args, g.arg)
		}
		delete(fromPrevious, g.arg.name)
	}
	plan.logLevel = metalAgentLogLevelDefault
	var kept []string
	for _, a := range args {
		if a.name == "log-level" {
			plan.logLevel = a.value
		} else {
			plan.extras = append(plan.extras, a)
		}
		if fromPrevious[a.name] {
			kept = append(kept, a.String())
		}
	}
	if len(kept) > 0 {
		plan.notes = append([]string{"keeping agent arguments from the previous join: " + strings.Join(kept, " ")},
			plan.notes...)
	}
	return plan
}

// metalAgentPort is a port the agent binds at startup.
type metalAgentPort struct {
	flag string
	what string
	// fatal: the agent exits when it cannot bind this port.
	fatal bool
	addr  string
	port  int
}

// metalAgentPorts are the ports an agent started with extras binds, with the
// agent's defaults and its listen addresses (pkg/agent health.go,
// ingress_wiring.go, clientproxy.go). A health or client port of 0 or less
// turns that listener off, as in the agent.
func metalAgentPorts(extras []agentArg) ([]metalAgentPort, error) {
	ports := []metalAgentPort{
		{flag: "port", what: "health and metrics server", fatal: true, port: 9090},
		{flag: "ingress-port", what: "ingress, which the cluster dials", fatal: true, port: 9443},
		{flag: "client-port", what: "client proxy for tools on this Mac", port: 9999},
	}
	var out []metalAgentPort
	for _, p := range ports {
		for _, a := range extras {
			if a.name != p.flag {
				continue
			}
			n, err := strconv.ParseInt(a.value, 0, strconv.IntSize)
			if err != nil {
				return nil, fmt.Errorf("%s: not a port number", a)
			}
			p.port = int(n)
		}
		switch {
		case p.flag == "ingress-port" && (p.port < metalnode.MinIngressPort || p.port > 65535):
			return nil, fmt.Errorf("--ingress-port %d: the agent needs %d-65535 to report it on its MetalNode",
				p.port, metalnode.MinIngressPort)
		case p.port > 65535:
			return nil, fmt.Errorf("--%s %d is not a port", p.flag, p.port)
		case p.port <= 0:
			continue
		}
		for _, o := range out {
			if o.port == p.port {
				return nil, fmt.Errorf("--%s and --%s are both %d: the agent binds each one", o.flag, p.flag, p.port)
			}
		}
		p.addr = "127.0.0.1:" + strconv.Itoa(p.port)
		if p.flag == "ingress-port" {
			p.addr = ":" + strconv.Itoa(p.port)
		}
		out = append(out, p)
	}
	return out, nil
}
