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
	what: "systemd unit", noun: "host", commandLine: systemdUnitExecStart}

// newSystemdAgentPaths are the files join writes on a Linux host: the state
// directory under $XDG_DATA_HOME, the agent's --state-dir default there
// (datadir.Dir), and the unit under $XDG_CONFIG_HOME, where the
// user's systemd instance loads it from.
func newSystemdAgentPaths(host metalHost) metalAgentPaths {
	data := filepath.Join(host.home, ".local", "share")
	if filepath.IsAbs(host.xdgDataHome) {
		data = host.xdgDataHome
	}
	config := filepath.Join(host.home, ".config")
	if filepath.IsAbs(host.xdgConfigHome) {
		config = host.xdgConfigHome
	}
	dir := filepath.Join(data, "llmkube", "metal-agent")
	return metalAgentPaths{
		Dir:        dir,
		Kubeconfig: filepath.Join(dir, "kubeconfig"),
		Token:      filepath.Join(dir, "token"),
		Unit:       filepath.Join(config, "systemd", "user", systemdAgentUnit),
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
	paths := newSystemdAgentPaths(host)
	if err := checkSystemdAgentUnit(paths, opts.force); err != nil {
		return err
	}
	plan, err := planJoinAgentArgs(systemdUnitFile, paths.Unit, paths.UnitBackup,
		metalJoinSets(paths, p, llamaServer), given, log)
	if err != nil {
		return err
	}
	if err := requireLlamaServer(plan, missingLlama, log); err != nil {
		return err
	}
	ports, err := metalAgentPorts(plan.extras)
	if err != nil {
		return err
	}
	if err := checkSystemdUserManager(ctx, host); err != nil {
		return err
	}
	if err := checkMetalAgentPorts(ctx, host, ports, log); err != nil {
		return err
	}
	if err := metalJoinSetup(ctx, p, opts.tokenLifetime, paths, log); err != nil {
		return err
	}
	started, err := installSystemdAgent(ctx, host, paths, metalAgentArgs(agentBin, paths, p, llamaServer, plan), log)
	if err != nil {
		return err
	}
	if !host.lingering() {
		_, _ = fmt.Fprintf(log, "the agent runs while %s is logged in; to keep it running after logout and start "+
			"it at boot, an admin runs: sudo loginctl enable-linger %s\n", host.username, host.username)
	}
	return finishMetalJoin(ctx, host, p, paths.Kubeconfig, started, opts.wait, systemdAgentLogs, out, log)
}

// checkSystemdAgentUnit refuses, before join changes anything, to replace a
// unit join did not write, unless force.
func checkSystemdAgentUnit(paths metalAgentPaths, force bool) error {
	exists, joined, err := readJoinMarkedFile(paths.Unit, metalJoinUnitMarker)
	if err != nil || !exists || joined || force {
		return err
	}
	return fmt.Errorf("%s runs a Metal agent that llmkube metal join did not install, with this host's own "+
		"arguments; join would replace it. Pass --force to replace it (join keeps it as %s), or join another host",
		paths.Unit, paths.UnitBackup)
}

// checkSystemdUserManager refuses when this user's systemd instance does not
// answer: without it, systemctl --user enable fails after the string is used
// up. It runs only while the user has a session, or with lingering on.
func checkSystemdUserManager(ctx context.Context, host metalHost) error {
	if _, err := host.systemctl(ctx, "show-environment"); err != nil {
		return fmt.Errorf("this user's systemd instance does not answer (%w). The Metal agent runs as a systemd "+
			"user unit, so join would use up the join string and then fail to start it. Run join in a login "+
			"session of this user (over SSH, one that sets XDG_RUNTIME_DIR), or have an admin turn on lingering "+
			"(sudo loginctl enable-linger %s), and run join again; the join string has not been used",
			err, host.username)
	}
	return nil
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
// write is kept as paths.UnitBackup; an earlier join's is not. It returns
// when the old agent had stopped: the new agent's status is newer than that.
func installSystemdAgent(ctx context.Context, host metalHost, paths metalAgentPaths, args []string,
	log io.Writer) (time.Time, error) {
	if err := os.MkdirAll(filepath.Dir(paths.Unit), 0o755); err != nil {
		return time.Time{}, fmt.Errorf("create %s: %w", filepath.Dir(paths.Unit), err)
	}
	if old, err := os.ReadFile(paths.Unit); err == nil && !strings.Contains(string(old), metalJoinUnitMarker) {
		if err := os.MkdirAll(filepath.Dir(paths.UnitBackup), 0o700); err != nil {
			return time.Time{}, fmt.Errorf("create %s: %w", filepath.Dir(paths.UnitBackup), err)
		}
		if err := writeFileAtomic(paths.UnitBackup, old, 0o600); err != nil {
			return time.Time{}, fmt.Errorf("keep the previous unit as %s: %w", paths.UnitBackup, err)
		}
		_, _ = fmt.Fprintf(log, "kept the previous unit as %s\n", paths.UnitBackup)
	}
	if err := writeFileAtomic(paths.Unit, renderSystemdAgentUnit(args, host.xdgDataHome), 0o644); err != nil {
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
	// running is not an error.
	_, _ = host.systemctl(ctx, "stop", systemdAgentUnit)
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
func renderSystemdAgentUnit(args []string, xdgDataHome string) []byte {
	var b strings.Builder
	b.WriteString(metalJoinUnitMarker + `
[Unit]
Description=LLMKube Metal agent
Documentation=https://llmkube.io/docs/guides/linux-host-agent

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
	if filepath.IsAbs(xdgDataHome) {
		// The agent's default model store and state directory follow it, and
		// must be where join wrote the credentials.
		b.WriteString("Environment=" + systemdQuote("XDG_DATA_HOME="+xdgDataHome, false) + "\n")
	}
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
	var value string
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
			if key, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(key) == "ExecStart" {
				value = strings.TrimSpace(v)
			}
		}
	}
	if value == "" {
		return nil, errors.New("no ExecStart")
	}
	words, err := splitSystemdCommandLine(value)
	if err == nil && len(words) == 0 {
		err = errors.New("ExecStart is empty")
	}
	return words, err
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
// agent's kubeconfig and token.
func runSystemdLeave(ctx context.Context, host metalHost, out, errOut io.Writer) error {
	paths := newSystemdAgentPaths(host)
	node, namespace := metalLeaveTarget(paths.Kubeconfig)
	exists, joined, err := readJoinMarkedFile(paths.Unit, metalJoinUnitMarker)
	if err != nil {
		return err
	}
	files := []string{paths.Kubeconfig, paths.Token}
	switch {
	case joined:
		if _, err := host.systemctl(ctx, "show-environment"); err != nil {
			return fmt.Errorf("this user's systemd instance does not answer (%w), so leave cannot stop the agent "+
				"and removed nothing; run llmkube metal leave in a login session of this user", err)
		}
		if _, err := host.systemctl(ctx, "disable", "--now", systemdAgentUnit); err != nil {
			_, _ = fmt.Fprintf(errOut, "the agent was not running (%v)\n", err)
		}
		if err := stopSystemdAgentEngines(ctx, host, paths, out, errOut); err != nil {
			return err
		}
		files = append([]string{paths.Unit}, files...)
	case exists:
		_, _ = fmt.Fprintf(out, "left %s running: llmkube metal join did not install it\n", paths.Unit)
	}
	removed, err := removeMetalJoinFiles(files, out)
	if err != nil {
		return err
	}
	if joined {
		if _, err := host.systemctl(ctx, "daemon-reload"); err != nil {
			_, _ = fmt.Fprintf(errOut, "warning: %v; systemd forgets %s at its next reload\n", err, systemdAgentUnit)
		}
	}
	if !reportMetalLeave(out, host.noun(), removed, node, namespace) {
		return nil
	}
	if _, err := os.Stat(paths.UnitBackup); joined && err == nil {
		_, _ = fmt.Fprintf(out, "join had replaced a systemd unit; to start it again: cp %q %q && %s\n",
			paths.UnitBackup, paths.Unit, systemdStartHint)
	}
	return nil
}

// stopSystemdAgentEngines is stopMetalEngines for the unit: it refuses
// unless the agent has stopped, then runs its stop-engines.
func stopSystemdAgentEngines(ctx context.Context, host metalHost, paths metalAgentPaths, out, errOut io.Writer) error {
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
	raw, err := os.ReadFile(paths.Unit)
	if err != nil {
		return fmt.Errorf("read %s: %w", paths.Unit, err)
	}
	args, err := systemdUnitExecStart(raw)
	if err != nil {
		return fmt.Errorf("read the agent's command line from %s: %w", paths.Unit, err)
	}
	return runAgentStopEngines(ctx, host, paths, args, out, errOut)
}
