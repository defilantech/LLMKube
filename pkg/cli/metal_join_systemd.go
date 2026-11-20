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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/datadir"
	"github.com/defilantech/llmkube/pkg/metaljoin"
)

const (
	// systemdAgentUnit is the agent's systemd user unit on a Linux host.
	systemdAgentUnit = "llmkube-metal-agent.service"
	// systemdAgentLogs is where the unit's output goes: the user journal.
	systemdAgentLogs = "journalctl --user -u " + systemdAgentUnit
	// metalJoinUnitMarker marks a unit join wrote, as metalJoinPlistMarker
	// marks a plist.
	metalJoinUnitMarker = "# Written by llmkube metal join; llmkube metal leave removes it."
	// systemdStartHint starts the unit by hand once what failed is fixed.
	systemdStartHint = "systemctl --user daemon-reload && systemctl --user enable --now " + systemdAgentUnit
)

var systemdUnitFile = agentServiceFile{marker: metalJoinUnitMarker, kind: "unit", argsKey: "ExecStart",
	what: "systemd unit", commandLine: systemdUnitExecStart}

// newSystemdAgentPaths are the files join writes on a Linux host: the state
// directory, the agent's --state-dir default for the data home join pins in
// the unit, and the unit in unitDir (systemdUnitDir).
func newSystemdAgentPaths(host metalHost, unitDir string) metalAgentPaths {
	dir := filepath.Join(datadir.DirFor(goosLinux, host.home, host.xdgDataHome), "metal-agent")
	return metalAgentPaths{
		Dir:        dir,
		Kubeconfig: filepath.Join(dir, "kubeconfig"),
		Token:      filepath.Join(dir, "token"),
		Unit:       filepath.Join(unitDir, systemdAgentUnit),
		UnitBackup: filepath.Join(dir, systemdAgentUnit+".bak"),
	}
}

// runSystemdJoin is runMetalJoin on a Linux host, from after the join string
// is decoded: the same checks, redemption and wait, with the agent installed
// as a systemd user unit.
func runSystemdJoin(ctx context.Context, host metalHost, p metaljoin.Payload, opts *metalJoinOptions,
	given []metalJoinExtra, out, log io.Writer) error {
	agentBin, err := findMetalAgentBinary(host, opts.agentBinary)
	if err != nil {
		return err
	}
	llamaServer, missingLlama := findLlamaServer(host, opts.llamaServer)
	if missingLlama != nil && opts.llamaServer != "" {
		return missingLlama
	}
	managerEnv, err := checkSystemdUserManager(ctx, host)
	if err != nil {
		return err
	}
	unitDir, err := systemdUnitDir(ctx, host, managerEnv)
	if err != nil {
		return err
	}
	paths := newSystemdAgentPaths(host, unitDir)
	unit, err := readSystemdAgentUnit(ctx, host, paths.Unit)
	if err != nil {
		return err
	}
	if err := checkSystemdAgentUnit(paths, unit, opts.force); err != nil {
		return err
	}
	plan, err := planJoinAgentArgs(systemdUnitFile, unit.agentServiceState, paths.Unit, paths.UnitBackup,
		host.noun(), metalJoinSets(paths, p, llamaServer), given, log)
	if err != nil {
		return err
	}
	if err := checkSystemdAgentRuntimes(plan); err != nil {
		return err
	}
	if err := requireLlamaServer(plan, missingLlama, log); err != nil {
		return err
	}
	ports, err := metalAgentPorts(plan.extras)
	if err != nil {
		return err
	}
	if err := checkMetalAgentPorts(ctx, host, ports, log); err != nil {
		return err
	}
	if err := metalJoinSetup(ctx, p, opts.tokenLifetime, paths, log); err != nil {
		return err
	}
	args := metalAgentArgs(agentBin, paths, p, llamaServer, plan)
	started, err := installSystemdAgent(ctx, host, paths, unit.agentServiceState, args, log)
	if err != nil {
		return err
	}
	if !host.lingering() {
		_, _ = fmt.Fprintf(log, "the agent runs only while this user is logged in; to keep it running after logout "+
			"and start it at boot, an admin runs: sudo loginctl enable-linger %s\n", host.lingerName())
	}
	return finishMetalJoin(ctx, host, p, paths.Kubeconfig, started, opts.wait, systemdAgentLogs, out, log)
}

// checkSystemdAgentRuntimes refuses, before join spends the join string, a
// runtime or binary flag in plan that the agent refuses at startup off macOS
// (cmd/metal-agent checkHostRuntimes, from the same metaljoin table).
func checkSystemdAgentRuntimes(plan metalJoinArgPlan) error {
	rt := planRuntime(plan)
	var refused []string
	if inferencev1alpha1.IsMacOSOnlyRuntime(rt) {
		refused = append(refused, "runtime="+rt)
	}
	values := map[string]string{}
	for _, a := range plan.extras {
		values[a.name] = a.value
	}
	for _, b := range metaljoin.MacOSOnlyBinFlags {
		if values[b.Flag] != "" && b.Runtime != rt {
			refused = append(refused, b.Flag)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf("agent argument %s: only on macOS with Apple Silicon; on Linux the agent serves "+
		"runtime=llama-server (llama.cpp) and runtime=ollama; the join string has not been used",
		strings.Join(refused, ", "))
}

// systemdAgentUnitState is the agent's unit as join and leave found it: the
// unit file, and the drop-ins systemd applies over it.
type systemdAgentUnitState struct {
	agentServiceState
	// dropIns are the drop-in files' contents, in the order systemd applies
	// them.
	dropIns [][]byte
	// execStartDropIn is the first drop-in that sets ExecStart, or "".
	execStartDropIn string
}

// effective is the unit as systemd reads it: the unit file, then its
// drop-ins.
func (u systemdAgentUnitState) effective() []byte {
	raw := append([]byte(nil), u.raw...)
	for _, d := range u.dropIns {
		raw = append(append(raw, '\n'), d...)
	}
	return raw
}

// readSystemdAgentUnit reads the unit at path and the drop-ins systemd's
// user instance lists for it (DropInPaths), wherever they are.
func readSystemdAgentUnit(ctx context.Context, host metalHost, path string) (systemdAgentUnitState, error) {
	file, err := readAgentService(path, metalJoinUnitMarker)
	if err != nil {
		return systemdAgentUnitState{}, err
	}
	u := systemdAgentUnitState{agentServiceState: file}
	out, err := host.systemctl(ctx, "show", "--property=DropInPaths", "--value", systemdAgentUnit)
	if err != nil {
		return systemdAgentUnitState{}, fmt.Errorf("list the drop-ins of %s: %w", systemdAgentUnit, err)
	}
	for _, f := range splitSystemdPaths(out, ".conf") {
		raw, err := os.ReadFile(f)
		if err != nil {
			return systemdAgentUnitState{}, fmt.Errorf("read the drop-in %s: %w", f, err)
		}
		u.dropIns = append(u.dropIns, raw)
		if u.execStartDropIn == "" && len(systemdServiceValues(raw, "ExecStart")) > 0 {
			u.execStartDropIn = f
		}
	}
	return u, nil
}

// splitSystemdPaths splits a space-separated path list systemctl show
// prints. Each path ends in suffix, so a space inside one stays in it.
func splitSystemdPaths(s, suffix string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, suffix+" ")
	for i := range parts[:len(parts)-1] {
		parts[i] += suffix
	}
	return parts
}

// checkSystemdAgentUnit refuses, before join changes anything, to replace a
// unit join did not write, unless force, and a unit whose ExecStart a drop-in
// overrides, since systemd would not run the command line join writes.
func checkSystemdAgentUnit(paths metalAgentPaths, unit systemdAgentUnitState, force bool) error {
	if unit.execStartDropIn != "" {
		return fmt.Errorf("the drop-in %s sets ExecStart for %s, so systemd would run its command line instead "+
			"of the one join writes; remove its ExecStart lines (then systemctl --user daemon-reload) and run join "+
			"again; the join string has not been used", unit.execStartDropIn, systemdAgentUnit)
	}
	if !unit.exists || unit.joined || force {
		return nil
	}
	return fmt.Errorf("%s runs a Metal agent that llmkube metal join did not install, with this host's own "+
		"arguments; join would replace it. Pass --force to replace it (join keeps it as %s), or join another host",
		paths.Unit, paths.UnitBackup)
}

// checkSystemdUserManager refuses when this user's systemd instance does not
// answer: without it, systemctl --user enable fails after the string is used
// up. It runs only while the user has a session, or with lingering on. It
// returns the instance's environment.
func checkSystemdUserManager(ctx context.Context, host metalHost) (string, error) {
	env, err := host.systemctl(ctx, "show-environment")
	if err != nil {
		return "", fmt.Errorf("this user's systemd instance does not answer (%w). The Metal agent runs as a systemd "+
			"user unit, so join would use up the join string and then fail to start it. Run join in a login "+
			"session of this user (over SSH, one that sets XDG_RUNTIME_DIR), or have an admin turn on lingering "+
			"(sudo loginctl enable-linger %s), and run join again; the join string has not been used",
			err, host.lingerName())
	}
	return env, nil
}

// systemdUnitDir is the directory the user's systemd instance loads the
// agent's unit from: its own $XDG_CONFIG_HOME/systemd/user, else
// $HOME/.config/systemd/user, from managerEnv (show-environment). The
// shell's XDG_CONFIG_HOME does not count: the instance reads its own. The
// directory must be on the instance's UnitPath, so a unit join writes is one
// systemd loads.
func systemdUnitDir(ctx context.Context, host metalHost, managerEnv string) (string, error) {
	unitPath, err := host.systemctl(ctx, "show", "--property=UnitPath", "--value")
	if err != nil {
		return "", fmt.Errorf("read the unit search path of this user's systemd instance: %w; "+
			"the join string has not been used", err)
	}
	var candidates []string
	if v := systemdEnvValue(managerEnv, "XDG_CONFIG_HOME"); filepath.IsAbs(v) {
		candidates = append(candidates, filepath.Join(v, "systemd", "user"))
	}
	if v := systemdEnvValue(managerEnv, "HOME"); filepath.IsAbs(v) {
		candidates = append(candidates, filepath.Join(v, ".config", "systemd", "user"))
	}
	candidates = append(candidates, filepath.Join(host.home, ".config", "systemd", "user"))
	searched := " " + strings.TrimSpace(unitPath) + " "
	for _, c := range candidates {
		if strings.Contains(searched, " "+c+" ") {
			return c, nil
		}
	}
	return "", fmt.Errorf("this user's systemd instance loads units from %s, which includes none of %s; "+
		"join would write a unit systemd never loads. The join string has not been used",
		strings.TrimSpace(unitPath), strings.Join(candidates, ", "))
}

// systemdEnvValue is the value of key in a show-environment listing, or ""
// when the listing does not set it or quotes it ($'...', for a value with
// special characters).
func systemdEnvValue(env, key string) string {
	for _, line := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok && !strings.HasPrefix(v, "$'") {
			return v
		}
	}
	return ""
}

// systemdAgentMainPID is the pid of the running unit, or 0.
func systemdAgentMainPID(ctx context.Context, host metalHost) int {
	out, err := host.systemctl(ctx, "show", "--property=MainPID", "--value", systemdAgentUnit)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return pid
}

// installSystemdAgent writes the unit and (re)starts it. A unit join did not
// write, prev, is kept as paths.UnitBackup; an earlier join's is not. It
// returns when the old agent had stopped: the new agent's status is newer
// than that.
func installSystemdAgent(ctx context.Context, host metalHost, paths metalAgentPaths, prev agentServiceState,
	args []string, log io.Writer) (time.Time, error) {
	if err := os.MkdirAll(filepath.Dir(paths.Unit), 0o755); err != nil {
		return time.Time{}, fmt.Errorf("create %s: %w", filepath.Dir(paths.Unit), err)
	}
	if prev.exists && !prev.joined {
		if err := os.MkdirAll(filepath.Dir(paths.UnitBackup), 0o700); err != nil {
			return time.Time{}, fmt.Errorf("create %s: %w", filepath.Dir(paths.UnitBackup), err)
		}
		if err := writeFileAtomic(paths.UnitBackup, prev.raw, 0o600); err != nil {
			return time.Time{}, fmt.Errorf("keep the previous unit as %s: %w", paths.UnitBackup, err)
		}
		_, _ = fmt.Fprintf(log, "kept the previous unit as %s\n", paths.UnitBackup)
	}
	dataHome := datadir.XDGDataHome(host.home, host.xdgDataHome)
	if err := writeFileAtomic(paths.Unit, renderSystemdAgentUnit(args, dataHome), 0o644); err != nil {
		return time.Time{}, fmt.Errorf("write %s: %w", paths.Unit, err)
	}
	startErr := func(err error) error {
		return fmt.Errorf("start the agent: %w; the join string is used up but the agent's credentials are in "+
			"place, so once that is fixed start it with: %s", err, systemdStartHint)
	}
	if _, err := host.systemctl(ctx, "daemon-reload"); err != nil {
		return time.Time{}, startErr(err)
	}
	// stop returns once the previous agent has exited; a unit that is not
	// running is not an error. One still active would make enable --now a
	// no-op, leaving the old agent running with its old arguments.
	if _, err := host.systemctl(ctx, "stop", systemdAgentUnit); err != nil {
		return time.Time{}, startErr(err)
	}
	// Truncated to the Lease's renewTime precision, so a renewal in the
	// same microsecond still counts as the new agent's.
	started := time.Now().Truncate(time.Microsecond)
	if _, err := host.systemctl(ctx, "enable", "--now", systemdAgentUnit); err != nil {
		return time.Time{}, startErr(err)
	}
	_, _ = fmt.Fprintf(log, "started the %s systemd user unit (%s)\n", systemdAgentUnit, paths.Unit)
	return started, nil
}

// renderSystemdAgentUnit renders the agent's unit. Restart and RestartSec
// match the LaunchAgent's KeepAlive and ThrottleInterval. KillMode=process
// stops only the agent, as launchd does: engines outlive it (#2079), the
// agent adopts them when it starts again, and leave's stop-engines stops them.
// XDG_DATA_HOME is dataHome, where join wrote the credentials: the agent's
// default state directory and model store follow it, and the instance's own
// environment may set another one.
func renderSystemdAgentUnit(args []string, dataHome string) []byte {
	var b strings.Builder
	b.WriteString(metalJoinUnitMarker + `
[Unit]
Description=LLMKube Metal agent
Documentation=https://llmkube.com/docs/guides/linux-host-agent

[Service]
Type=simple
ExecStart=`)
	for i, a := range args {
		if i > 0 {
			b.WriteString(" \\\n    ")
		}
		b.WriteString(systemdQuote(a, true))
	}
	b.WriteString("\n")
	b.WriteString("Environment=" + systemdQuote("XDG_DATA_HOME="+dataHome, false) + "\n")
	b.WriteString(`WorkingDirectory=~
Restart=on-failure
RestartSec=30
KillMode=process
SyslogIdentifier=llmkube-metal-agent

[Install]
WantedBy=default.target
`)
	return []byte(b.String())
}

// systemdQuote quotes s as one word of a unit setting: systemd expands %
// specifiers in the raw line, undoes C escapes in quoted words, and in
// ExecStart (exec) expands $VARIABLE references.
func systemdQuote(s string, exec bool) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '%':
			b.WriteString("%%")
		case c == '$' && exec:
			b.WriteString("$$")
		case c == '\n':
			b.WriteString(`\n`)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// systemdUnitExecStart is the command line of a unit's ExecStart, read the
// way systemd reads it: continuation lines joined, the last ExecStart in
// [Service] winning, %% and $$ unescaped, quotes and C escapes undone. A
// specifier other than %%, or a $VARIABLE reference, is refused: its value
// depends on systemd.
func systemdUnitExecStart(raw []byte) ([]string, error) {
	values := systemdServiceValues(raw, "ExecStart")
	if len(values) == 0 || values[len(values)-1] == "" {
		return nil, errors.New("no ExecStart")
	}
	words, err := splitSystemdCommandLine(values[len(values)-1])
	if err == nil && len(words) == 0 {
		err = errors.New("ExecStart is empty")
	}
	return words, err
}

// systemdServiceValues is every value of key in raw's [Service] sections, in
// order, with continuation lines joined and comments skipped.
func systemdServiceValues(raw []byte, key string) []string {
	var values []string
	section := ""
	lines := strings.Split(string(raw), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		for strings.HasSuffix(line, `\`) && i+1 < len(lines) {
			i++
			next := strings.TrimSpace(lines[i])
			if strings.HasPrefix(next, "#") || strings.HasPrefix(next, ";") {
				continue
			}
			line = strings.TrimSuffix(line, `\`) + " " + next
		}
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line[0] == '[':
			section = line
		case section == "[Service]":
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
				values = append(values, strings.TrimSpace(v))
			}
		}
	}
	return values
}

// splitSystemdCommandLine splits an ExecStart value into its words.
func splitSystemdCommandLine(s string) ([]string, error) {
	s = strings.TrimLeft(s, "-:+!|")
	if strings.HasPrefix(s, "@") {
		return nil, errors.New("ExecStart with @ names argv[0] apart from the binary, which join does not read")
	}
	var expanded strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			expanded.WriteByte(s[i])
			continue
		}
		if i+1 >= len(s) || s[i+1] != '%' {
			return nil, fmt.Errorf("ExecStart uses a %% specifier (%.2q), which join does not expand", s[i:])
		}
		expanded.WriteByte('%')
		i++
	}
	t := expanded.String()
	var (
		words  []string
		word   strings.Builder
		inWord bool
		quote  byte
	)
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\''):
			quote, inWord = c, true
		case quote == 0 && (c == ' ' || c == '\t'):
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		case c == '\\':
			r, n, err := systemdUnescape(t[i+1:])
			if err != nil {
				return nil, err
			}
			word.WriteString(r)
			i += n
			inWord = true
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("ExecStart has an unterminated quote")
	}
	if inWord {
		words = append(words, word.String())
	}
	for i, w := range words {
		v, err := systemdUnescapeDollars(w)
		if err != nil {
			return nil, err
		}
		words[i] = v
	}
	return words, nil
}

// systemdUnescape undoes the C escape at the start of rest, which follows a
// backslash, and returns its value and how many bytes of rest it took.
func systemdUnescape(rest string) (string, int, error) {
	if rest == "" {
		return "", 0, errors.New("ExecStart ends in a backslash")
	}
	simple := map[byte]string{'a': "\a", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t", 'v': "\v",
		'\\': `\`, '"': `"`, '\'': "'", 's': " "}
	if v, ok := simple[rest[0]]; ok {
		return v, 1, nil
	}
	// parse reads n digits in base (8 or 16) from digits.
	parse := func(digits string, base rune, n int) (rune, error) {
		if len(digits) < n {
			return 0, fmt.Errorf("ExecStart has a short escape \\%.9s", rest)
		}
		var v rune
		for _, d := range digits[:n] {
			var x rune
			switch {
			case d >= '0' && d <= '9':
				x = d - '0'
			case d >= 'a' && d <= 'f':
				x = d - 'a' + 10
			case d >= 'A' && d <= 'F':
				x = d - 'A' + 10
			default:
				x = base
			}
			if x >= base {
				return 0, fmt.Errorf("ExecStart has an invalid escape \\%.9s", rest)
			}
			v = v*base + x
		}
		return v, nil
	}
	n, base := 0, rune(16)
	switch rest[0] {
	case 'x':
		n = 2
	case 'u':
		n = 4
	case 'U':
		n = 8
	case '0', '1', '2', '3', '4', '5', '6', '7':
		n, base = 3, 8
	default:
		return "", 0, fmt.Errorf("ExecStart has an unknown escape \\%c", rest[0])
	}
	digits, took := rest[1:], n+1
	if base == 8 {
		digits, took = rest, n
	}
	v, err := parse(digits, base, n)
	switch {
	case err != nil:
		return "", 0, err
	case n <= 3 && v > 0xff:
		return "", 0, fmt.Errorf("ExecStart has an out of range escape \\%.3s", rest)
	case n <= 3:
		return string([]byte{byte(v)}), took, nil //nolint:gosec // G115: v is at most 0xff
	case !utf8.ValidRune(v):
		return "", 0, fmt.Errorf("ExecStart has an invalid escape \\%.9s", rest)
	}
	return string(v), took, nil
}

// systemdUnescapeDollars undoes $$ in an ExecStart word and refuses any
// other $.
func systemdUnescapeDollars(w string) (string, error) {
	if !strings.Contains(w, "$") {
		return w, nil
	}
	var b strings.Builder
	for i := 0; i < len(w); i++ {
		if w[i] != '$' {
			b.WriteByte(w[i])
			continue
		}
		if i+1 >= len(w) || w[i+1] != '$' {
			return "", fmt.Errorf("ExecStart word %q refers to an environment variable, which join does not expand", w)
		}
		b.WriteByte('$')
		i++
	}
	return b.String(), nil
}

// runSystemdLeave is runMetalLeave on a Linux host: it stops, disables and
// removes the unit join installed, stops the agent's engines, and removes the
// agent's kubeconfig and token. It finds the unit as join does, and the
// credentials where the unit's --kubeconfig names them.
func runSystemdLeave(ctx context.Context, host metalHost, out, errOut io.Writer) error {
	managerEnv, managerErr := host.systemctl(ctx, "show-environment")
	unitDir := filepath.Join(host.home, ".config", "systemd", "user")
	if managerErr == nil {
		if dir, err := systemdUnitDir(ctx, host, managerEnv); err == nil {
			unitDir = dir
		}
	}
	paths := newSystemdAgentPaths(host, unitDir)
	file, err := readAgentService(paths.Unit, metalJoinUnitMarker)
	if err != nil {
		return err
	}
	var agentArgs []string
	if file.joined {
		if managerErr != nil {
			return fmt.Errorf("this user's systemd instance does not answer (%w), so leave cannot stop the agent "+
				"and removed nothing; run llmkube metal leave in a login session of this user", managerErr)
		}
		unit, err := readSystemdAgentUnit(ctx, host, paths.Unit)
		if err != nil {
			return err
		}
		agentArgs, err = systemdUnitExecStart(unit.effective())
		if err != nil {
			return fmt.Errorf("read the agent's command line from %s and its drop-ins: %w; nothing was stopped "+
				"or removed", paths.Unit, err)
		}
		paths = joinedSystemdAgentPaths(paths, agentArgs)
	}
	// Read which host this was before the kubeconfig goes, for the hint.
	node, namespace := metalLeaveTarget(paths.Kubeconfig)
	files := []string{paths.Kubeconfig, paths.Token}
	switch {
	case file.joined:
		if _, err := host.systemctl(ctx, "disable", "--now", systemdAgentUnit); err != nil {
			_, _ = fmt.Fprintf(errOut, "the agent was not running (%v)\n", err)
		}
		if err := stopSystemdAgentEngines(ctx, host, paths, agentArgs, out, errOut); err != nil {
			return err
		}
		files = append([]string{paths.Unit}, files...)
	case file.exists:
		_, _ = fmt.Fprintf(out, "left %s running: llmkube metal join did not install it\n", paths.Unit)
	}
	removed, removeErr := removeMetalJoinFiles(files, out)
	if _, err := os.Stat(paths.Unit); file.joined && errors.Is(err, os.ErrNotExist) {
		if _, err := host.systemctl(ctx, "daemon-reload"); err != nil {
			_, _ = fmt.Fprintf(errOut, "warning: %v; systemd forgets %s at its next reload\n", err, systemdAgentUnit)
		}
	}
	if removeErr != nil {
		return removeErr
	}
	if !reportMetalLeave(out, host.noun(), removed, node, namespace) {
		return nil
	}
	if _, err := os.Stat(paths.UnitBackup); file.joined && err == nil {
		_, _ = fmt.Fprintf(out, "join had replaced a systemd unit; to start it again: cp %q %q && %s\n",
			paths.UnitBackup, paths.Unit, systemdStartHint)
	}
	return nil
}

// joinedSystemdAgentPaths is paths with the state directory, kubeconfig,
// token and backup where the joined unit's command line args puts them: join
// writes the kubeconfig into the state directory and names it with
// --kubeconfig, whatever the data home of the shell leave runs in.
func joinedSystemdAgentPaths(paths metalAgentPaths, args []string) metalAgentPaths {
	kubeconfig := metaljoin.FlagValue(args[1:], "kubeconfig", "")
	if !filepath.IsAbs(kubeconfig) {
		return paths
	}
	dir := filepath.Dir(kubeconfig)
	paths.Dir, paths.Kubeconfig = dir, kubeconfig
	paths.Token = filepath.Join(dir, "token")
	paths.UnitBackup = filepath.Join(dir, systemdAgentUnit+".bak")
	return paths
}

// stopSystemdAgentEngines is stopMetalEngines for the unit: it refuses
// unless the agent has stopped, then runs the stop-engines of the agent
// args names, with the state directory join pinned unless args set one.
func stopSystemdAgentEngines(ctx context.Context, host metalHost, paths metalAgentPaths, args []string,
	out, errOut io.Writer) error {
	state, err := host.systemctl(ctx, "show", "--property=ActiveState", "--value", systemdAgentUnit)
	state = strings.TrimSpace(state)
	if err != nil {
		state = err.Error()
	}
	if state != "inactive" && state != "failed" {
		return fmt.Errorf("the agent is not stopped after systemctl --user disable --now (%s), so its engines were "+
			"not stopped and nothing was removed; run llmkube metal leave again once systemctl --user is-active %s "+
			"says inactive", state, systemdAgentUnit)
	}
	withStateDir := append([]string{args[0], "--state-dir", paths.Dir}, args[1:]...)
	return runAgentStopEngines(ctx, host, paths, withStateDir, out, errOut)
}
