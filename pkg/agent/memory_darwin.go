//go:build darwin

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
// Apple Silicon uses 16KB pages.
func (p *DarwinMemoryProvider) AvailableMemory() (uint64, error) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, fmt.Errorf("vm_stat: %w", err)
	}
	return availableBytesFromVMStat(string(out))
}

// availableBytesFromVMStat computes available memory from raw vm_stat
// output. Pure (no exec) so it can be unit-tested with a fixture. Output
// with no page size in its header, or with none of the four page counts
// below, is an error rather than zero bytes available: the watchdog would
// read zero as Critical memory pressure.
func availableBytesFromVMStat(output string) (uint64, error) {
	pageSize, pageSizeParsed, bodyLines := parseVMStatHeader(output)
	if !pageSizeParsed {
		return 0, fmt.Errorf("vm_stat: no page size in the header line")
	}

	// macOS treats free, inactive, speculative, and purgeable pages as
	// reclaimable/available (matching Activity Monitor's "memory
	// available" heuristic). Counting only free+inactive severely
	// undercounts on a host with a Metal-wired model loaded, where a
	// large fraction of RAM is wired and the rest of the available
	// budget lives in speculative + purgeable. See defilantech/LLMKube#578.
	var freePages, inactivePages, speculativePages, purgeablePages uint64
	found := 0
	for _, line := range bodyLines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Pages free:"):
			freePages = parseVMStatValue(line)
		case strings.HasPrefix(line, "Pages inactive:"):
			inactivePages = parseVMStatValue(line)
		case strings.HasPrefix(line, "Pages speculative:"):
			speculativePages = parseVMStatValue(line)
		case strings.HasPrefix(line, "Pages purgeable:"):
			purgeablePages = parseVMStatValue(line)
		default:
			continue
		}
		found++
	}
	if found == 0 {
		return 0, fmt.Errorf("vm_stat: no free, inactive, speculative or purgeable page counts")
	}

	return (freePages + inactivePages + speculativePages + purgeablePages) * pageSize, nil
}

// WiredMemory returns the amount of wired (non-pageable) memory by parsing vm_stat.
func (p *DarwinMemoryProvider) WiredMemory() (uint64, error) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, fmt.Errorf("vm_stat: %w", err)
	}

	pageSize, _, lines := parseVMStatHeader(string(out))
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
// The bool reports whether the header carried a page size; when it did not,
// the returned size is the Apple Silicon default of 16KB.
func parseVMStatHeader(output string) (uint64, bool, []string) {
	lines := strings.Split(output, "\n")
	if len(lines) == 0 {
		return 16384, false, nil
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
		return 16384, false, lines[1:] // default to 16KB for Apple Silicon
	}
	return pageSize, true, lines[1:]
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

// NewHostMemoryProvider returns the MemoryProvider for this OS.
func NewHostMemoryProvider() MemoryProvider { return &DarwinMemoryProvider{} }
