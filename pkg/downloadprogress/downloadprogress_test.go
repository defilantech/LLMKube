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

package downloadprogress

import "testing"

// The lines here are the shapes download_with_progress prints. The controller's
// shell tests feed Parse the real script's output as well, so a change to the
// script that this package does not follow fails there.
func TestParse(t *testing.T) {
	tests := []struct {
		name string
		line string
		want Progress
		ok   bool
	}{
		{
			name: "single file with a known total",
			line: "Downloaded 1104 MiB of 2765 MiB (39%)",
			want: Progress{DownloadedMiB: 1104, TotalMiB: 2765, Percent: 39, HasTotal: true},
			ok:   true,
		},
		{
			name: "single file with no total",
			line: "Downloaded 2 MiB",
			want: Progress{DownloadedMiB: 2},
			ok:   true,
		},
		{
			name: "nothing written yet",
			line: "Downloaded 0 MiB of 2765 MiB (0%)",
			want: Progress{TotalMiB: 2765, HasTotal: true},
			ok:   true,
		},
		{
			name: "a file under a MiB still carries its percent",
			line: "Downloaded 0 MiB of 0 MiB (60%)",
			want: Progress{Percent: 60, HasTotal: true},
			ok:   true,
		},
		{
			name: "pool worker with a total and the files in flight",
			line: "Downloaded 2 MiB of 4 MiB of model-00001-of-00002.safetensors (50%) (concurrency: 2)",
			want: Progress{DownloadedMiB: 2, TotalMiB: 4, Percent: 50, HasTotal: true, File: "model-00001-of-00002.safetensors"},
			ok:   true,
		},
		{
			name: "pool worker without a count",
			line: "Downloaded 2 MiB of 4 MiB of m.bin (50%)",
			want: Progress{DownloadedMiB: 2, TotalMiB: 4, Percent: 50, HasTotal: true, File: "m.bin"},
			ok:   true,
		},
		{
			name: "pool worker with no total, as S3 reports",
			line: "Downloaded 7 MiB of a.bin (concurrency: 1)",
			want: Progress{DownloadedMiB: 7, File: "a.bin"},
			ok:   true,
		},
		{
			name: "file name with a parenthesis",
			line: "Downloaded 3 MiB of 9 MiB of dir/model (1).gguf (33%)",
			want: Progress{DownloadedMiB: 3, TotalMiB: 9, Percent: 33, HasTotal: true, File: "dir/model (1).gguf"},
			ok:   true,
		},
		{
			name: "trailing carriage return",
			line: "Downloaded 5 MiB of 10 MiB (50%)\r",
			want: Progress{DownloadedMiB: 5, TotalMiB: 10, Percent: 50, HasTotal: true},
			ok:   true,
		},
		{name: "start banner", line: "Downloading model..."},
		{name: "success line", line: "Model downloaded successfully"},
		{name: "cache hit", line: "Model already cached, skipping download"},
		{name: "pool wait", line: "Waiting to start c.bin: 1 MiB in flight, 1 MiB needed, 1 MiB allowed (concurrency: 1)"},
		{name: "error", line: "ERROR: failed to download a.bin"},
		{name: "prefix without a number", line: "Downloaded some MiB"},
		{name: "empty", line: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Parse(tt.line)
			if ok != tt.ok {
				t.Fatalf("Parse(%q) ok = %v, want %v", tt.line, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tt.line, got, tt.want)
			}
		})
	}
}

func TestLast(t *testing.T) {
	tests := []struct {
		name string
		logs string
		want Progress
		ok   bool
	}{
		{
			name: "latest report wins",
			logs: "Downloading model...\n" +
				"Downloaded 10 MiB of 100 MiB (10%)\n" +
				"Downloaded 20 MiB of 100 MiB (20%)\n",
			want: Progress{DownloadedMiB: 20, TotalMiB: 100, Percent: 20, HasTotal: true},
			ok:   true,
		},
		{
			name: "a later line that is not a report is skipped",
			logs: "Downloaded 20 MiB of 100 MiB (20%)\n" +
				"Waiting to start b.bin: 1 MiB in flight, 1 MiB needed, 1 MiB allowed (concurrency: 1)\n",
			want: Progress{DownloadedMiB: 20, TotalMiB: 100, Percent: 20, HasTotal: true},
			ok:   true,
		},
		{
			name: "no trailing newline",
			logs: "Downloaded 1 MiB of 2 MiB (50%)",
			want: Progress{DownloadedMiB: 1, TotalMiB: 2, Percent: 50, HasTotal: true},
			ok:   true,
		},
		{name: "no report yet", logs: "Downloading model...\n"},
		{name: "empty", logs: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Last(tt.logs)
			if ok != tt.ok || got != tt.want {
				t.Errorf("Last() = %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}
