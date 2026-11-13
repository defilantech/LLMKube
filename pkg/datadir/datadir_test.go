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

package datadir

import (
	"runtime"
	"testing"
)

func TestDirPerOS(t *testing.T) {
	for _, tc := range []struct{ goos, xdg, want string }{
		{"darwin", "/xdg", "/home/u/Library/Application Support/llmkube"},
		{"linux", "", "/home/u/.local/share/llmkube"},
		{"linux", "/xdg", "/xdg/llmkube"},
		{"linux", "relative", "/home/u/.local/share/llmkube"},
	} {
		if got := dir(tc.goos, "/home/u", tc.xdg); got != tc.want {
			t.Errorf("dir(%s, XDG_DATA_HOME=%q) = %q, want %q", tc.goos, tc.xdg, got, tc.want)
		}
	}
}

func TestDirReadsXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/xdg")
	want := "/xdg/llmkube"
	if runtime.GOOS == "darwin" {
		want = "/home/u/Library/Application Support/llmkube"
	}
	if got := Dir("/home/u"); got != want {
		t.Errorf("Dir with XDG_DATA_HOME=/xdg on %s = %q, want %q", runtime.GOOS, got, want)
	}
}
