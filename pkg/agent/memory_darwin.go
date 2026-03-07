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

	pageSize, bodyLines := parseVMStatHeader(string(out))

	var freePages, inactivePages uint64
	for _, line := range bodyLines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Pages free:") {
			freePages = parseVMStatValue(line)
		} else if strings.HasPrefix(line, "Pages inactive:") {
			inactivePages = parseVMStatValue(line)
		}
	}

	return (freePages + inactivePages) * pageSize, nil
}

// WiredMemory returns the amount of wired (non-pageable) memory by parsing vm_stat.
func (p *DarwinMemoryProvider) WiredMemory() (uint64, error) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, fmt.Errorf("vm_stat: %w", err)
	}

	pageSize, lines := parseVMStatHeader(string(out))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Pages wired down:") {
			return parseVMStatValue(line) * pageSize, nil
		}
	}
	return 0, fmt.Errorf("vm_stat: 'Pages wired down' not found")
}

// ProcessRSS returns the resident set size of a process in bytes.
func (p *DarwinMemoryProvider) ProcessRSS(pid int) (uint64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p",
		strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, fmt.Errorf("ps rss for pid %d: %w", pid, err)
	}
	// ps reports RSS in kilobytes
	kb, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf(
			"parse rss for pid %d: %w", pid, err)
	}
	return kb * 1024, nil
}

// parseVMStatHeader extracts the page size and body lines from vm_stat output.
func parseVMStatHeader(output string) (uint64, []string) {
	lines := strings.Split(output, "\n")
	if len(lines) == 0 {
		return 16384, nil
	}

	var pageSize uint64
	firstLine := lines[0]
	if idx := strings.Index(firstLine, "page size of "); idx >= 0 {
		sizeStr := firstLine[idx+len("page size of "):]
		if endIdx := strings.Index(sizeStr, " "); endIdx >= 0 {
			sizeStr = sizeStr[:endIdx]
		}
		parsed, err := strconv.ParseUint(sizeStr, 10, 64)
		if err == nil {
			pageSize = parsed
		}
	}
	if pageSize == 0 {
		pageSize = 16384 // default to 16KB for Apple Silicon
	}
	return pageSize, lines[1:]
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
