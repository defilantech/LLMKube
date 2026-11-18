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
	"os/exec"
	"strconv"
	"strings"
)

// DetectHostCapacity reads the Mac's memory and chip name through sysctl.
// GPU cores and the Metal version come from platform.DetectCapabilities,
// which the agent already runs at startup.
func DetectHostCapacity() HostCapacity {
	return hostCapacityFromSysctl(func(name string) (string, error) {
		out, err := exec.Command("sysctl", "-n", name).Output()
		return string(out), err
	})
}

// hostCapacityFromSysctl reads unified memory (hw.memsize) and the chip name
// (machdep.cpu.brand_string). A value that cannot be read or parsed is left
// empty rather than guessed.
func hostCapacityFromSysctl(sysctl func(name string) (string, error)) HostCapacity {
	var c HostCapacity
	if out, err := sysctl("hw.memsize"); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil && n > 0 {
			c.MemoryBytes = n
		}
	}
	if out, err := sysctl("machdep.cpu.brand_string"); err == nil {
		c.Chip = printablePrefix(strings.TrimSpace(out), maxChipBytes)
	}
	return c
}
