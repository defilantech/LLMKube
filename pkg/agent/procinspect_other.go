//go:build !darwin

package agent

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// errProcessInspectUnsupported reports a platform with no start-token source.
var errProcessInspectUnsupported = errors.New("process start time is not available on this platform")

type procfsProcessInspector struct{}

func newProcessInspector() processInspector { return procfsProcessInspector{} }

// StartToken is field 22 of /proc/<pid>/stat (start time in clock ticks since
// boot). The agent only runs on macOS; this exists so the package's tests run
// on Linux CI. Without /proc it returns errProcessInspectUnsupported.
func (procfsProcessInspector) StartToken(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if os.IsNotExist(err) {
		if _, statErr := os.Stat("/proc/self/stat"); statErr != nil {
			return "", errProcessInspectUnsupported
		}
		return "", fmt.Errorf("no process with PID %d", pid)
	}
	if err != nil {
		return "", err
	}
	// The command name (field 2) is parenthesized and may contain spaces;
	// fields after the last ")" are space-separated, starting at field 3.
	s := string(data)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	const startTimeField = 22 - 3
	if len(fields) <= startTimeField {
		return "", fmt.Errorf("short /proc/%d/stat", pid)
	}
	return fields[startTimeField], nil
}

// Executables is /proc/<pid>/exe plus the first two absolute fields of
// /proc/<pid>/cmdline (argv[0], and argv[1] for a #! script), each as read and
// symlink-resolved.
func (procfsProcessInspector) Executables(pid int) ([]string, error) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		return nil, errProcessInspectUnsupported
	}
	var raw []string
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		raw = append(raw, exe)
	}
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		args := strings.Split(string(data), "\x00")
		raw = append(raw, args[:min(len(args), 2)]...)
	}
	out := executableCandidates(raw)
	if len(out) == 0 {
		return nil, fmt.Errorf("no executable path for PID %d", pid)
	}
	return out, nil
}
