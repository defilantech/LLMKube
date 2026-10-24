//go:build darwin

package agent

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

type darwinProcessInspector struct{}

func newProcessInspector() processInspector { return darwinProcessInspector{} }

// StartToken is the process's kernel start time (kern.proc.pid.<pid>,
// p_starttime) in seconds and microseconds.
func (darwinProcessInspector) StartToken(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("read kern.proc.pid.%d: %w", pid, err)
	}
	if kp.Proc.P_pid != int32(pid) { //nolint:gosec // G115: PIDs fit in int32
		return "", fmt.Errorf("no process with PID %d", pid)
	}
	st := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", st.Sec, st.Usec), nil
}

// Executables returns the running program's candidate paths from
// kern.procargs2: the exec path, argv[0] and argv[1], each as the kernel
// reports it and resolved through symlinks, so a Homebrew bin/ link, its
// current Cellar target and the link an engine was started through all count.
// argv[1] covers a #! engine (a pip entry point), where the kernel runs the
// interpreter.
func (darwinProcessInspector) Executables(pid int) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("read kern.procargs2.%d: %w", pid, err)
	}
	raw, err := parseProcargs2(buf, 2)
	if err != nil {
		return nil, fmt.Errorf("kern.procargs2 for PID %d: %w", pid, err)
	}
	out := executableCandidates(raw)
	if len(out) == 0 {
		return nil, fmt.Errorf("no executable path for PID %d", pid)
	}
	return out, nil
}

// procargsAlign is the alignment of argv after the exec path in
// kern.procargs2: the kernel pads the exec path to the pointer size of the
// process, 8 bytes on every macOS the agent runs on.
const procargsAlign = 8

// parseProcargs2 returns the exec path followed by up to n argv strings from
// a kern.procargs2 buffer: argc as a little-endian int32, the NUL-terminated
// exec path, NUL padding to procargsAlign counted from the start of the exec
// path, then argc NUL-terminated argv strings. Skipping to the aligned offset
// rather than past every NUL keeps an empty argv[0], which would otherwise be
// taken for padding and shift every later argument. If the padding is not all
// NUL (a layout this code does not know), it falls back to skipping NULs.
func parseProcargs2(buf []byte, n int) ([]string, error) {
	if len(buf) < 5 {
		return nil, fmt.Errorf("short buffer (%d bytes)", len(buf))
	}
	argc := int(binary.LittleEndian.Uint32(buf[:4]))
	area := buf[4:]
	end := bytes.IndexByte(area, 0)
	if end < 0 {
		return nil, fmt.Errorf("unterminated exec path")
	}
	out := []string{string(area[:end])}
	next := (end + procargsAlign) &^ (procargsAlign - 1)
	if next > len(area) || bytes.ContainsFunc(area[end:next], func(r rune) bool { return r != 0 }) {
		for next = end; next < len(area) && area[next] == 0; next++ {
		}
	}
	for i := 0; i < argc && i < n && next < len(area); i++ {
		arg, _, _ := bytes.Cut(area[next:], []byte{0})
		out = append(out, string(arg))
		next += len(arg) + 1
	}
	return out, nil
}
