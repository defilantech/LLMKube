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

// Package downloadprogress reads the progress reports the model-downloader init
// container prints, so a client that does not run the download can show it.
//
// The producer is download_with_progress, the shell helper the controller
// builds in internal/controller/model_storage.go. Its text is part of every
// staging pod template, so it changes rarely; the controller's shell tests run
// it and parse the output with this package to keep the two in step.
package downloadprogress

import (
	"regexp"
	"strconv"
	"strings"
)

// Progress is one report from the downloader.
type Progress struct {
	// DownloadedMiB is how much of the file is on disk, in whole MiB.
	DownloadedMiB int64
	// TotalMiB is the file's size in whole MiB. It is meaningful only when
	// HasTotal is set: a file under a MiB reports a total of 0.
	TotalMiB int64
	// Percent is the share of the file on disk, when HasTotal is set.
	Percent int
	// HasTotal reports whether the downloader knew the file's size. An S3
	// transfer, or a server that sends no Content-Length, reports bytes only.
	HasTotal bool
	// File names the file a concurrent multi-file worker is staging. Single-file
	// and serial downloads leave it empty.
	File string
}

// reportLine matches the four shapes the helper prints: with or without a
// total, and with or without the file name and pool count that concurrent
// staging adds ("Downloaded 2 MiB of 4 MiB of a.bin (50%) (concurrency: 2)").
var reportLine = regexp.MustCompile(
	`^Downloaded ([0-9]+) MiB(?: of ([0-9]+) MiB)?(?: of (.+?))?(?: \(([0-9]+)%\))?(?: \(concurrency: [0-9]+\))?$`)

// Parse reads one log line. It reports false for any line that is not a
// progress report, such as the start banner, a pool's wait notice or an error.
func Parse(line string) (Progress, bool) {
	m := reportLine.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return Progress{}, false
	}
	var p Progress
	var err error
	if p.DownloadedMiB, err = strconv.ParseInt(m[1], 10, 64); err != nil {
		return Progress{}, false
	}
	if m[2] != "" {
		p.HasTotal = true
		if p.TotalMiB, err = strconv.ParseInt(m[2], 10, 64); err != nil {
			return Progress{}, false
		}
	}
	p.File = m[3]
	if m[4] != "" {
		if p.Percent, err = strconv.Atoi(m[4]); err != nil {
			return Progress{}, false
		}
	}
	return p, true
}

// Last returns the latest report in a stretch of log, skipping any lines after
// it that are not reports.
func Last(logs string) (Progress, bool) {
	lines := strings.Split(logs, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if p, ok := Parse(lines[i]); ok {
			return p, true
		}
	}
	return Progress{}, false
}
