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

package agent

import (
	"math"
	"os"
	"strings"
)

// DetectHostCapacity reads the board or CPU name from /proc, and memory
// through the host MemoryProvider so a cgroup limit is reported the same way
// the memory budget sees it.
func DetectHostCapacity() HostCapacity {
	c := HostCapacity{Chip: hostChipFromProc(os.ReadFile)}
	if total, err := NewHostMemoryProvider().TotalMemory(); err == nil && total > 0 && total <= math.MaxInt64 {
		c.MemoryBytes = int64(total)
	}
	return c
}

// hostChipFromProc is the device-tree model when the board has one (ARM
// boards), else the first cpuinfo "model name" or "Model" line, else
// empty.
func hostChipFromProc(readFile func(path string) ([]byte, error)) string {
	if data, err := readFile("/proc/device-tree/model"); err == nil {
		if chip := printablePrefix(strings.TrimSpace(strings.TrimRight(string(data), "\x00")), maxChipBytes); chip != "" {
			return chip
		}
	}
	data, err := readFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if k := strings.TrimSpace(key); k == "model name" || k == "Model" {
			return printablePrefix(strings.TrimSpace(val), maxChipBytes)
		}
	}
	return ""
}
