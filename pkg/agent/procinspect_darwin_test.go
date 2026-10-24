//go:build darwin

package agent

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// procargs builds a kern.procargs2 buffer: argc, then rest verbatim (the exec
// path, its padding, argv and envp, each written out with its NULs).
func procargs(argc uint32, rest string) []byte {
	buf := binary.LittleEndian.AppendUint32(nil, argc)
	return append(buf, rest...)
}

func TestParseProcargs2(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
		want []string
	}{
		{
			// "/a/b\0" is 5 bytes, padded to 8; argv[0] is empty.
			name: "empty argv[0] is kept",
			buf:  procargs(2, "/a/b\x00\x00\x00\x00"+"\x00/opt/x\x00"+"A=b\x00"),
			want: []string{"/a/b", "", "/opt/x"},
		},
		{
			// With argv[0] taken for padding, the first envp string would be
			// read as an argument.
			name: "empty argv[0] does not pull in envp",
			buf:  procargs(1, "/a/b\x00\x00\x00\x00"+"\x00"+"/looks/like/a/path\x00"),
			want: []string{"/a/b", ""},
		},
		{
			name: "exec path filling the alignment needs no padding",
			buf:  procargs(2, "/abcdef\x00"+"x\x00/y\x00"+"A=b\x00"),
			want: []string{"/abcdef", "x", "/y"},
		},
		{
			name: "exec path one past the alignment pads a whole word",
			buf:  procargs(2, "/abcdefg\x00"+strings.Repeat("\x00", 7)+"/x\x00/y\x00"),
			want: []string{"/abcdefg", "/x", "/y"},
		},
		{
			name: "only the first n arguments",
			buf:  procargs(3, "/a/b\x00\x00\x00\x00"+"/x\x00/y\x00/z\x00"),
			want: []string{"/a/b", "/x", "/y"},
		},
		{
			name: "argc bounds the arguments",
			buf:  procargs(1, "/a/b\x00\x00\x00\x00"+"/x\x00/env\x00"),
			want: []string{"/a/b", "/x"},
		},
		{
			// Padding shorter than the alignment: a layout the parser does not
			// know, so it falls back to skipping NULs.
			name: "unknown padding falls back to skipping NULs",
			buf:  procargs(2, "/a/b\x00\x00/x\x00/y\x00"),
			want: []string{"/a/b", "/x", "/y"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProcargs2(tt.buf, 2)
			if err != nil || !slices.Equal(got, tt.want) {
				t.Fatalf("parseProcargs2 = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestParseProcargs2_Malformed(t *testing.T) {
	for name, buf := range map[string][]byte{
		"short":                  {1, 0, 0, 0},
		"unterminated exec path": procargs(1, "/a/b"),
	} {
		if got, err := parseProcargs2(buf, 2); err == nil {
			t.Errorf("%s: parseProcargs2 = %q, want an error", name, got)
		}
	}
}

// A process started through a symlink reports the link as its exec path;
// Executables offers it as executed and resolved, so a record of either form
// matches after the link is repointed.
func TestProcessInspector_ExecutablesKeepTheLinkAndItsTarget(t *testing.T) {
	link := filepath.Join(t.TempDir(), "sleep-link")
	if err := os.Symlink("/bin/sleep", link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	got, err := newProcessInspector().Executables(cmd.Process.Pid)
	if err != nil || !slices.Contains(got, link) || !slices.Contains(got, "/bin/sleep") {
		t.Fatalf("Executables = %q, %v; want both %q and /bin/sleep", got, err, link)
	}
}
