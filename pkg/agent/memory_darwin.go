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
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// DarwinMemoryProvider implements MemoryProvider for macOS using sysctl and vm_stat.
type DarwinMemoryProvider struct{}

// TotalMemory returns the total physical memory via sysctl hw.memsize.
func (p *DarwinMemoryProvider) TotalMemory() (uint64, error) {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	total, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse hw.memsize %q: %w", strings.TrimSpace(string(out)), err)
	}
	return total, nil
}

// AvailableMemory returns an estimate of available memory by parsing vm_stat output.
// It sums free and inactive pages multiplied by the page size.
// Apple Silicon uses 16KB pages.
func (p *DarwinMemoryProvider) AvailableMemory() (uint64, error) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, fmt.Errorf("vm_stat: %w", err)
	}

	lines := strings.Split(string(out), "\n")
	if len(lines) == 0 {
		return 0, fmt.Errorf("vm_stat: empty output")
	}

	// First line contains page size: "Mach Virtual Memory Statistics: (page size of 16384 bytes)"
	var pageSize uint64
	firstLine := lines[0]
	if idx := strings.Index(firstLine, "page size of "); idx >= 0 {
		sizeStr := firstLine[idx+len("page size of "):]
		if endIdx := strings.Index(sizeStr, " "); endIdx >= 0 {
			sizeStr = sizeStr[:endIdx]
		}
		pageSize, err = strconv.ParseUint(sizeStr, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse page size %q: %w", sizeStr, err)
		}
	}
	if pageSize == 0 {
		pageSize = 16384 // default to 16KB for Apple Silicon
	}

	var freePages, inactivePages uint64
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Pages free:") {
			freePages = parseVMStatValue(line)
		} else if strings.HasPrefix(line, "Pages inactive:") {
			inactivePages = parseVMStatValue(line)
		}
	}

	return (freePages + inactivePages) * pageSize, nil
}

// parseVMStatValue extracts the numeric value from a vm_stat line like "Pages free:    123456."
func parseVMStatValue(line string) uint64 {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return 0
	}
	valStr := strings.TrimSpace(parts[1])
	valStr = strings.TrimSuffix(valStr, ".")
	val, _ := strconv.ParseUint(valStr, 10, 64)
	return val
}
