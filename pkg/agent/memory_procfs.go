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

// No build tag: the fake-/proc tests run on every OS, the macOS runner included.

package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// LinuxMemoryProvider implements MemoryProvider from /proc, capped by the
// cgroup v2 memory limits on the path from the agent's own cgroup to the
// root. Engines the agent starts inherit that cgroup; a daemon it only talks
// to (Ollama) does not, and is measured with HostWide.
type LinuxMemoryProvider struct {
	// ProcRoot is the procfs mount; empty means /proc.
	ProcRoot string
	// CgroupRoot is the cgroup v2 mount; empty means /sys/fs/cgroup.
	CgroupRoot string
	// IgnoreCgroup reports host memory only.
	IgnoreCgroup bool

	dirsOnce sync.Once
	dirs     []string
}

// HostWide returns a provider for the same host that ignores the agent's
// cgroup limits, for engines that run outside the agent's cgroup.
func (p *LinuxMemoryProvider) HostWide() *LinuxMemoryProvider {
	return &LinuxMemoryProvider{ProcRoot: p.ProcRoot, CgroupRoot: p.CgroupRoot, IgnoreCgroup: true}
}

// hostWideProvider is p measured host-wide when p is a LinuxMemoryProvider,
// else p. distinct reports whether the result is a different view from p.
func hostWideProvider(p MemoryProvider) (hostWide MemoryProvider, distinct bool) {
	if lp, ok := p.(*LinuxMemoryProvider); ok {
		return lp.HostWide(), true
	}
	return p, false
}

// cgroupDirs is the agent's cgroup directory and its ancestors up to the
// mount, read once from /proc/self/cgroup.
func (p *LinuxMemoryProvider) cgroupDirs() []string {
	p.dirsOnce.Do(func() {
		if p.IgnoreCgroup {
			return
		}
		root := p.CgroupRoot
		if root == "" {
			root = "/sys/fs/cgroup"
		}
		root = filepath.Clean(root)
		rel := "/"
		if data, err := os.ReadFile(p.procPath("self", "cgroup")); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if path, found := strings.CutPrefix(line, "0::"); found {
					rel = path
					break
				}
			}
		}
		for dir := filepath.Join(root, rel); strings.HasPrefix(dir, root); dir = filepath.Dir(dir) {
			p.dirs = append(p.dirs, dir)
			if dir == root {
				break
			}
		}
	})
	return p.dirs
}

// cgroupV1Memory reports whether /proc/self/cgroup places the agent under a
// cgroup v1 memory controller, as on a hybrid v1/v2 host. The provider reads
// only cgroup v2 limits, so such a limit is not applied.
func (p *LinuxMemoryProvider) cgroupV1Memory() bool {
	if p.IgnoreCgroup {
		return false
	}
	data, err := os.ReadFile(p.procPath("self", "cgroup"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		id, rest, found := strings.Cut(line, ":")
		if !found || id == "0" {
			continue
		}
		controllers, _, _ := strings.Cut(rest, ":")
		for _, c := range strings.Split(controllers, ",") {
			if c == "memory" {
				return true
			}
		}
	}
	return false
}

// logCgroupV1Memory logs at Info when provider is a Linux provider whose
// memory controller is cgroup v1, so its figures are host-wide. NewMetalAgent
// calls it once.
func logCgroupV1Memory(provider MemoryProvider, logger *zap.SugaredLogger) {
	lp, ok := provider.(*LinuxMemoryProvider)
	if !ok || !lp.cgroupV1Memory() {
		return
	}
	logger.Infow("memory controller is on cgroup v1, whose limits the agent does not read; "+
		"memory figures are the host's, not the agent's cgroup limit",
		"cgroupFile", lp.procPath("self", "cgroup"))
}

// cgroupLimit is the smallest memory.max on the agent's cgroup path. ok is
// false when no limit is set or cgroup v2 is not mounted.
func (p *LinuxMemoryProvider) cgroupLimit() (limit uint64, ok bool) {
	for _, dir := range p.cgroupDirs() {
		if l, set := readCgroupMax(dir); set && (!ok || l < limit) {
			limit, ok = l, true
		}
	}
	return limit, ok
}

// cgroupRoom is the smallest room left under a memory.max on the agent's
// cgroup path: limit minus usage, inactive file cache counted as
// reclaimable. It is taken separately from cgroupLimit, since the cgroup
// with the least room need not be the one with the smallest limit.
func (p *LinuxMemoryProvider) cgroupRoom() (room uint64, ok bool) {
	for _, dir := range p.cgroupDirs() {
		if r, set := readCgroupRoom(dir); set && (!ok || r < room) {
			room, ok = r, true
		}
	}
	return room, ok
}

// readCgroupMax reads one cgroup's memory.max. set is false for "max" or a
// missing file.
func readCgroupMax(dir string) (limit uint64, set bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		return 0, false
	}
	limit, err = strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return limit, err == nil
}

// readCgroupRoom reads one cgroup's memory.max, memory.current and
// memory.stat inactive_file. set is false when memory.max is "max" or
// missing.
func readCgroupRoom(dir string) (room uint64, set bool) {
	limit, set := readCgroupMax(dir)
	if !set {
		return 0, false
	}
	var used uint64
	if raw, err := os.ReadFile(filepath.Join(dir, "memory.current")); err == nil {
		used, _ = strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "memory.stat")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if v, found := strings.CutPrefix(line, "inactive_file "); found {
				if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil && n <= used {
					used -= n
				}
				break
			}
		}
	}
	if used >= limit {
		return 0, true
	}
	return limit - used, true
}

func (p *LinuxMemoryProvider) procPath(elem ...string) string {
	root := p.ProcRoot
	if root == "" {
		root = "/proc"
	}
	return filepath.Join(append([]string{root}, elem...)...)
}

func (p *LinuxMemoryProvider) meminfoField(key string) (uint64, error) {
	data, err := os.ReadFile(p.procPath("meminfo"))
	if err != nil {
		return 0, err
	}
	fields := parseProcKB(string(data))
	v, ok := fields[key]
	if !ok {
		return 0, fmt.Errorf("meminfo: no %s field", key)
	}
	return v, nil
}

// TotalMemory is MemTotal, or the cgroup limit when that is lower.
func (p *LinuxMemoryProvider) TotalMemory() (uint64, error) {
	total, err := p.meminfoField("MemTotal")
	if err != nil {
		return 0, err
	}
	if limit, ok := p.cgroupLimit(); ok && limit < total {
		return limit, nil
	}
	return total, nil
}

// AvailableMemory is MemAvailable, or the room under the cgroup limit when
// that is lower. A missing MemAvailable is an error, not zero: the watchdog
// reads zero as Critical pressure.
func (p *LinuxMemoryProvider) AvailableMemory() (uint64, error) {
	avail, err := p.meminfoField("MemAvailable")
	if err != nil {
		return 0, err
	}
	if room, ok := p.cgroupRoom(); ok && room < avail {
		return room, nil
	}
	return avail, nil
}

// WiredMemory is Mlocked, the closest Linux analogue of macOS wired pages.
func (p *LinuxMemoryProvider) WiredMemory() (uint64, error) {
	return p.meminfoField("Mlocked")
}

// ProcessRSS is VmRSS from /proc/<pid>/status.
func (p *LinuxMemoryProvider) ProcessRSS(pid int) (uint64, error) {
	data, err := os.ReadFile(p.procPath(strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, fmt.Errorf("rss for pid %d: %w", pid, err)
	}
	v, ok := parseProcKB(string(data))["VmRSS"]
	if !ok {
		return 0, fmt.Errorf("rss for pid %d: no VmRSS field", pid)
	}
	return v, nil
}

// parseProcKB parses "Key:   123 kB" lines (/proc/meminfo, /proc/<pid>/status)
// into bytes. Lines without a numeric kB value are skipped.
func parseProcKB(data string) map[string]uint64 {
	out := make(map[string]uint64)
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		out[strings.TrimSpace(key)] = n * 1024
	}
	return out
}
