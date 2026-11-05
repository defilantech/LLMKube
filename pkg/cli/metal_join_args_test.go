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
	"context"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	_ "sigs.k8s.io/controller-runtime/pkg/client/config" // registers --kubeconfig, as in the agent

	"github.com/defilantech/llmkube/pkg/metaljoin"
)

// plistAgentArgs reads the agent's arguments (after the binary) from the
// LaunchAgent join wrote.
func plistAgentArgs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the LaunchAgent: %v", err)
	}
	args, err := metalAgentPlistArgs(raw)
	if err != nil || len(args) == 0 {
		t.Fatalf("ProgramArguments of %s: %q, %v", path, args, err)
	}
	return args[1:]
}

// agentFlagValues returns every value args give flag name, in either form
// Go's flag package reads (--name=value, or --name value).
func agentFlagValues(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := strings.TrimLeft(args[i], "-")
		if a == args[i] {
			continue
		}
		if n, v, ok := strings.Cut(a, "="); ok {
			if n == name {
				out = append(out, v)
			}
			continue
		}
		if a == name && i+1 < len(args) {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}

// editJoinedPlist adds args to the end of ProgramArguments, the hand edit
// the macOS guide describes for --host-ip and the fleet allowlist.
func editJoinedPlist(t *testing.T, path string, args ...string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var add strings.Builder
	for _, a := range args {
		add.WriteString("        <string>" + a + "</string>\n")
	}
	edited := strings.Replace(string(raw), "    </array>\n", add.String()+"    </array>\n", 1)
	if edited == string(raw) {
		t.Fatalf("no ProgramArguments array to edit in %s", path)
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil { //nolint:gosec // G306: a plist
		t.Fatal(err)
	}
}

// TestMetalRejoinKeepsTheAgentArgumentsItDoesNotManage: a Mac that was off
// for longer than its token's lifetime needs a new join string, and the guide
// tells users to add --host-ip and the like to the LaunchAgent. A re-join
// keeps every argument join does not set itself, and says so.
func TestMetalRejoinKeepsTheAgentArgumentsItDoesNotManage(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	paths := mac.paths()
	joinString, _ := issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0"); err != nil {
		t.Fatalf("first join: %v\n%s", err, stderr)
	}
	editJoinedPlist(t, paths.Plist, "--host-ip", "192.168.1.50", "--memory-fraction=0.8", "--eviction-enabled")

	joinString, _ = issueJoinString(t, ns)
	_, stderr, err := runMetal(t, "join", joinString, "--wait", "0")
	if err != nil {
		t.Fatalf("re-join: %v\n%s", err, stderr)
	}
	args := plistAgentArgs(t, paths.Plist)
	for name, want := range map[string]string{"host-ip": "192.168.1.50", "memory-fraction": "0.8"} {
		if got := agentFlagValues(args, name); len(got) != 1 || got[0] != want {
			t.Errorf("--%s after re-join: %q, want [%s]; agent arguments %q", name, got, want, args)
		}
	}
	if !containsArg(args, "--eviction-enabled") {
		t.Errorf("--eviction-enabled dropped by the re-join: %q", args)
	}
	if got := agentFlagValues(args, "namespace"); len(got) != 1 || got[0] != ns {
		t.Errorf("--namespace after re-join: %q, want exactly join's own [%s]", got, ns)
	}
	for _, want := range []string{"--host-ip=192.168.1.50", "--memory-fraction=0.8", "--eviction-enabled"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("re-join does not say it kept %s: %q", want, stderr)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestMetalAgentFlagsMatchTheAgent: join refuses an --agent-arg the agent
// does not have and parses an old plist by each flag's kind, so its table
// must be the agent's flag set. It reads the flag registrations in
// cmd/metal-agent, plus --kubeconfig, which controller-runtime's config
// package registers for it; and the port defaults join's port check uses.
func TestMetalAgentFlagsMatchTheAgent(t *testing.T) {
	kinds := map[string]agentFlagKind{
		"String": agentFlagString, "StringVar": agentFlagString, "Bool": agentFlagBool, "BoolVar": agentFlagBool,
		"Int": agentFlagInt, "IntVar": agentFlagInt, "Float64": agentFlagFloat, "Float64Var": agentFlagFloat,
		"Duration": agentFlagDuration, "DurationVar": agentFlagDuration,
	}
	got := map[string]agentFlagKind{}
	defaults := map[string]int{}
	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "metal-agent", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("agent sources: %v, %v", files, err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			kind, known := kinds[sel.Sel.Name]
			if !ok || pkg.Name != "flag" || !known {
				return true
			}
			at := 0
			if strings.HasSuffix(sel.Sel.Name, "Var") {
				at = 1
			}
			lit, ok := call.Args[at].(*ast.BasicLit)
			if !ok {
				t.Fatalf("%s: a flag name that is not a literal", fset.Position(call.Pos()))
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			got[name] = kind
			if def, ok := call.Args[at+1].(*ast.BasicLit); ok && def.Kind == token.INT {
				defaults[name], _ = strconv.Atoi(def.Value)
			}
			return true
		})
	}
	if flag.Lookup("kubeconfig") == nil {
		t.Fatal("controller-runtime's config package no longer registers --kubeconfig")
	}
	got["kubeconfig"] = agentFlagString
	if !reflect.DeepEqual(got, metalAgentFlags) {
		for name, kind := range got {
			if want, ok := metalAgentFlags[name]; !ok || want != kind {
				t.Errorf("the agent has --%s (kind %d), join's table has %d (listed %t)", name, kind, want, ok)
			}
		}
		for name := range metalAgentFlags {
			if _, ok := got[name]; !ok {
				t.Errorf("join's table has --%s, which the agent does not", name)
			}
		}
	}
	for name := range metalJoinManagedFlags {
		if _, ok := metalAgentFlags[name]; !ok {
			t.Errorf("managed flag --%s is not an agent flag", name)
		}
	}
	ports, err := metalAgentPorts(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ports {
		if defaults[p.flag] != p.port {
			t.Errorf("join checks --%s %d by default, the agent's default is %d", p.flag, p.port, defaults[p.flag])
		}
	}
}

func TestParseAgentArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []agentArg
		err  string
	}{
		{"both value forms and a bool", []string{"--host-ip", "10.0.0.5", "-port=9091", "--eviction-enabled", "--runtime",
			"omlx"}, []agentArg{{"host-ip", "10.0.0.5", true}, {"port", "9091", true}, {"eviction-enabled", "", false},
			{"runtime", "omlx", true}}, ""},
		{"a known flag takes the next argument even with a dash", []string{"--log-level", "-x"},
			[]agentArg{{"log-level", "-x", true}}, ""},
		{"an unknown flag before a word takes it", []string{"--from-a-newer-agent", "v", "--other-new"},
			[]agentArg{{"from-a-newer-agent", "v", true}, {"other-new", "", false}}, ""},
		{"an empty value", []string{"--model-store="}, []agentArg{{"model-store", "", true}}, ""},
		{"a stray word", []string{"--eviction-enabled", "true"}, nil, `"true" is not a flag`},
		{"the end of flags", []string{"--", "--port", "1"}, nil, `"--" is not a flag`},
		{"a known flag at the end without its value", []string{"--port"}, nil, "--port has no value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAgentArgs(tc.args)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("parseAgentArgs(%q) = %v, %v; want error %q", tc.args, got, err, tc.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseAgentArgs(%q) = %v, %v; want %v", tc.args, got, err, tc.want)
			}
		})
	}
}

// TestMetalJoinAgentFlags: --host-ip, --allowlist and --agent-arg reach the
// agent's command line; on a re-join a flag given again replaces the kept
// value, an empty --allowlist drops it, and what was not given again stays.
func TestMetalJoinAgentFlags(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	paths := mac.paths()
	joinString, _ := issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0", "--host-ip", "192.168.1.50",
		"--allowlist", "phi-4-mini, qwen", "--agent-arg", "memory-fraction=0.7",
		"--agent-arg", "--eviction-enabled"); err != nil {
		t.Fatalf("join: %v\n%s", err, stderr)
	}
	args := plistAgentArgs(t, paths.Plist)
	for name, want := range map[string]string{"host-ip": "192.168.1.50",
		"inference-service-allowlist": "phi-4-mini,qwen", "memory-fraction": "0.7", "log-level": "info"} {
		if got := agentFlagValues(args, name); len(got) != 1 || got[0] != want {
			t.Errorf("--%s: %q, want [%s]; agent arguments %q", name, got, want, args)
		}
	}
	if !containsArg(args, "--eviction-enabled") {
		t.Errorf("--agent-arg eviction-enabled missing: %q", args)
	}

	joinString, _ = issueJoinString(t, ns)
	_, stderr, err := runMetal(t, "join", joinString, "--wait", "0", "--host-ip", "10.0.0.5", "--allowlist", "",
		"--agent-arg", "log-level=debug")
	if err != nil {
		t.Fatalf("re-join: %v\n%s", err, stderr)
	}
	args = plistAgentArgs(t, paths.Plist)
	for name, want := range map[string][]string{"host-ip": {"10.0.0.5"}, "inference-service-allowlist": nil,
		"memory-fraction": {"0.7"}, "log-level": {"debug"}} {
		if got := agentFlagValues(args, name); !reflect.DeepEqual(got, want) {
			t.Errorf("--%s after re-join: %q, want %q; agent arguments %q", name, got, want, args)
		}
	}
	for _, want := range []string{
		"keeping agent arguments from the previous join: --memory-fraction=0.7 --eviction-enabled",
		"--host-ip=10.0.0.5 replaces --host-ip=192.168.1.50",
		"dropping --inference-service-allowlist=phi-4-mini,qwen"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("re-join does not say %q: %q", want, stderr)
		}
	}
	if strings.Contains(stderr, "log-level=info") {
		t.Errorf("re-join reports join's own --log-level default as the user's: %q", stderr)
	}
}

// TestMetalRejoinDoesNotKeepWhatJoinManages: a hand-added flag that would
// undo join's setup is not carried over, and join says so.
func TestMetalRejoinDoesNotKeepWhatJoinManages(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	paths := mac.paths()
	joinString, _ := issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0"); err != nil {
		t.Fatalf("join: %v\n%s", err, stderr)
	}
	editJoinedPlist(t, paths.Plist, "--kube-context", "admin", "--namespace", "other", "--port", "9191")
	mac.listened = nil
	joinString, _ = issueJoinString(t, ns)
	_, stderr, err := runMetal(t, "join", joinString, "--wait", "0")
	if err != nil {
		t.Fatalf("re-join: %v\n%s", err, stderr)
	}
	args := plistAgentArgs(t, paths.Plist)
	if got := agentFlagValues(args, "kube-context"); len(got) != 0 {
		t.Errorf("--kube-context carried over: %q", args)
	}
	if got := agentFlagValues(args, "namespace"); len(got) != 1 || got[0] != ns {
		t.Errorf("--namespace after re-join: %q, want only the join string's %s", got, ns)
	}
	for _, want := range []string{"not keeping --kube-context=admin", "not keeping --namespace=other",
		"keeping agent arguments from the previous join: --port=9191"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("re-join does not say %q: %q", want, stderr)
		}
	}
	if !containsArg(mac.listened, "127.0.0.1:9191") || containsArg(mac.listened, "127.0.0.1:9090") {
		t.Errorf("join checked %q, want the carried --port 9191 instead of 9090", mac.listened)
	}
}

// TestMetalJoinRefusesBadAgentFlags: an agent flag join cannot pass on is
// refused before join changes anything or uses the string; otherwise the
// agent would exit at startup and launchd would restart it every 30 seconds.
func TestMetalJoinRefusesBadAgentFlags(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	joinString, _ := issueJoinString(t, ns)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"kubeconfig", []string{"--agent-arg", "kubeconfig=/tmp/admin"}, "join writes the agent's kubeconfig"},
		{"namespace", []string{"--agent-arg", "--namespace=other"}, "the join string sets it"},
		{"metal node", []string{"--agent-arg", "metal-node=mac-b"}, "the join string sets it"},
		{"token renewal", []string{"--agent-arg", "renew-service-account-token=false"}, "keeps its token current"},
		{"host-ip through agent-arg", []string{"--agent-arg", "host-ip=10.0.0.5"}, "use join's --host-ip"},
		{"allowlist through agent-arg", []string{"--agent-arg", "inference-service-allowlist=a"}, "--allowlist"},
		{"version", []string{"--agent-arg", "version"}, "print its version and exit"},
		{"unknown flag", []string{"--agent-arg", "no-such-flag=1"}, "has no --no-such-flag flag"},
		{"bad number", []string{"--agent-arg", "port=nine"}, `cannot read "nine"`},
		{"bad duration", []string{"--agent-arg", "memory-watchdog-interval=10"}, "cannot read"},
		{"missing value", []string{"--agent-arg", "port"}, "needs a value: port=<value>"},
		{"no name", []string{"--agent-arg", "=9091"}, "want <flag>=<value>"},
		{"given twice", []string{"--agent-arg", "port=9091", "--agent-arg", "port=9092"}, "more than once"},
		{"host-ip with a port", []string{"--host-ip", "10.0.0.5:9443"}, "not an IPv4 address"},
		{"host-ip IPv6", []string{"--host-ip", "fd00::5"}, "not an IPv4 address"},
		{"host-ip name", []string{"--host-ip", "mac.local"}, "not an IPv4 address"},
		{"host-ip loopback", []string{"--host-ip", "127.0.0.1"}, "cannot reach"},
		{"allowlist bad name", []string{"--allowlist", "Phi_4"}, "not an InferenceService name"},
		{"allowlist empty entry", []string{"--allowlist", "a,,b"}, `"" is not an InferenceService name`},
		{"ingress port too low", []string{"--agent-arg", "ingress-port=443"}, "1024-65535"},
		{"port out of range", []string{"--agent-arg", "client-port=70000"}, "is not a port"},
		{"two listeners on one port", []string{"--agent-arg", "port=9443"}, "are both 9443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mac := newFakeMac(t)
			_, _, err := runMetal(t, append([]string{"join", joinString, "--wait", "0"}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("join %q: err = %v, want a refusal mentioning %q", tc.args, err, tc.want)
			}
			if entries, _ := os.ReadDir(mac.home); len(entries) != 0 || len(mac.calls) != 0 {
				t.Errorf("a refused join wrote %v and ran launchctl %q", entries, mac.calls)
			}
		})
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "llmkube-metal-mac-a-join"},
		&corev1.Secret{}); err != nil {
		t.Errorf("a refused join used up the join string: get join Secret: %v", err)
	}
}

// TestMetalLeaveStopsTheEnginesWhereAgentArgPutThem: join writes
// --agent-arg flags as --flag=value, and leave must still hand stop-engines
// the agent's state directory, where the engine records are, and its
// Ollama port.
func TestMetalLeaveStopsTheEnginesWhereAgentArgPutThem(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	script := []byte("#!/bin/sh\n# usage: llmkube-metal-agent " + metaljoin.StopEnginesCommand + " [flags]\n")
	if err := os.WriteFile(mac.agentBin, script, 0o755); err != nil { //nolint:gosec // G306: an executable
		t.Fatal(err)
	}
	stateDir := filepath.Join(mac.home, "agent-state")
	joinString, _ := issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0", "--agent-arg", "state-dir="+stateDir,
		"--agent-arg", "ollama-port=11500"); err != nil {
		t.Fatalf("join: %v\n%s", err, stderr)
	}
	mac.calls = nil
	if _, stderr, err := runMetal(t, "leave"); err != nil {
		t.Fatalf("leave: %v\n%s", err, stderr)
	}
	want := []string{"agent " + mac.agentBin + " stop-engines --kubeconfig " + mac.paths().Kubeconfig +
		" --namespace " + ns + " --state-dir " + stateDir + " --ollama-port 11500"}
	if got := agentRuns(mac.calls); !reflect.DeepEqual(got, want) {
		t.Errorf("leave ran %q, want %q", got, want)
	}
}

func TestPlanMetalAgentArgs(t *testing.T) {
	joinSets := map[string]agentArg{"namespace": {"namespace", "team", true}}
	for _, tc := range []struct {
		name     string
		previous []agentArg
		given    []metalJoinExtra
		logLevel string
		extras   []agentArg
	}{
		{"join's own --log-level is not the user's", []agentArg{{"log-level", "info", true}, {"port", "9091", true}},
			nil, "info", []agentArg{{"port", "9091", true}}},
		{"the last of a repeated flag wins, as in the agent",
			[]agentArg{{"port", "9091", true}, {"log-level", "debug", true}, {"port", "9092", true},
				{"log-level", "info", true}},
			nil, "info", []agentArg{{"port", "9092", true}}},
		{"a carried --log-level", []agentArg{{"log-level", "info", true}, {"log-level", "debug", true}},
			nil, "debug", nil},
		{"a given flag over a carried one, an empty --host-ip dropping it",
			[]agentArg{{"host-ip", "10.0.0.5", true}, {"port", "9091", true}, {"namespace", "team", true}},
			[]metalJoinExtra{{arg: agentArg{"port", "9191", true}}, {arg: agentArg{name: "host-ip"}, drop: true},
				{arg: agentArg{"log-level", "warn", true}}},
			"warn", []agentArg{{"port", "9191", true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := planMetalAgentArgs(tc.previous, tc.given, joinSets)
			if plan.logLevel != tc.logLevel || !reflect.DeepEqual(plan.extras, tc.extras) {
				t.Errorf("plan = --log-level %s %v, want --log-level %s %v (notes %q)", plan.logLevel, plan.extras,
					tc.logLevel, tc.extras, plan.notes)
			}
		})
	}
}
