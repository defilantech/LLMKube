//go:build darwin

/*
Copyright 2025.

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

package agent

import (
	"os/exec"
	"strconv"
	"strings"
)

// detectPerfCoreCount returns the number of performance ("P") cores on Apple
// Silicon, or 0 when the count cannot be determined. The caller is expected to
// fall back to a sensible default (typically: omit the --threads flag and let
// llama-server choose).
//
// On Apple Silicon the kernel exposes per-tier core counts via sysctl:
//
//	hw.perflevel0.physicalcpu  performance cores (P-cores)
//	hw.perflevel1.physicalcpu  efficiency cores (E-cores)
//
// Pinning llama-server to the P-cores avoids E-core scheduling penalties on
// the inference hot path. On Intel Macs there is no perflevel split and the
// sysctl returns an error; we return 0 so the caller can fall back.
func detectPerfCoreCount() int {
	out, err := exec.Command("sysctl", "-n", "hw.perflevel0.physicalcpu").Output()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
