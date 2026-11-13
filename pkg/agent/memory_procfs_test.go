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
	"os"
	"path/filepath"
	"testing"
)

// An 8 GB host, trimmed.
const meminfo = `MemTotal:        8245328 kB
MemFree:         5120000 kB
MemAvailable:    7012340 kB
Buffers:           61234 kB
Mlocked:           16384 kB
HugePages_Total:       0
Hugepagesize:       2048 kB
`

func writeProcFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLinuxMemoryProvider(t *testing.T) {
	root := writeProcFixture(t, map[string]string{
		"meminfo":   meminfo,
		"42/status": "Name:\tllama-server\nVmRSS:\t  204800 kB\nThreads:\t4\n",
		"43/status": "Name:\tkthreadd\nThreads:\t1\n",
	})
	p := &LinuxMemoryProvider{ProcRoot: root, CgroupRoot: filepath.Join(root, "no-cgroup")}

	for name, tc := range map[string]struct {
		fn   func() (uint64, error)
		want uint64
	}{
		"total":     {p.TotalMemory, 8245328 * 1024},
		"available": {p.AvailableMemory, 7012340 * 1024},
		"wired":     {p.WiredMemory, 16384 * 1024},
		"rss":       {func() (uint64, error) { return p.ProcessRSS(42) }, 204800 * 1024},
	} {
		got, err := tc.fn()
		if err != nil || got != tc.want {
			t.Errorf("%s = %d, %v; want %d", name, got, err, tc.want)
		}
	}

	if _, err := p.ProcessRSS(43); err == nil {
		t.Error("rss of a process without VmRSS: want an error, not zero")
	}
	if _, err := p.ProcessRSS(99); err == nil {
		t.Error("rss of a missing process: want an error")
	}
}

func TestLinuxMemoryProviderMissingAvailable(t *testing.T) {
	root := writeProcFixture(t, map[string]string{"meminfo": "MemTotal: 1024 kB\n"})
	p := &LinuxMemoryProvider{ProcRoot: root, CgroupRoot: filepath.Join(root, "no-cgroup")}
	if got, err := p.AvailableMemory(); err == nil {
		t.Errorf("available = %d with no MemAvailable; want an error (zero reads as Critical)", got)
	}
}

func TestLinuxMemoryProviderCgroupLimit(t *testing.T) {
	const gib = 1 << 30
	root := writeProcFixture(t, map[string]string{
		"proc/meminfo":     "MemTotal: 67108864 kB\nMemAvailable: 60000000 kB\n",
		"proc/self/cgroup": "0::/system.slice/llmkube.service\n",
		// The slice caps at 8 GiB with 2 GiB used; the service has no limit.
		"cg/system.slice/memory.max":                     "8589934592\n",
		"cg/system.slice/memory.current":                 "3221225472\n",
		"cg/system.slice/memory.stat":                    "anon 1\ninactive_file 1073741824\n",
		"cg/system.slice/llmkube.service/memory.max":     "max\n",
		"cg/system.slice/llmkube.service/memory.current": "1048576\n",
	})
	p := &LinuxMemoryProvider{ProcRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cg")}
	if got, err := p.TotalMemory(); err != nil || got != 8*gib {
		t.Errorf("total = %d, %v; want the 8 GiB slice limit, not the host's 64 GiB", got, err)
	}
	if got, err := p.AvailableMemory(); err != nil || got != 6*gib {
		t.Errorf("available = %d, %v; want 8 GiB minus 3 GiB used plus 1 GiB inactive file", got, err)
	}

	// A limit above the host's memory does not raise what is reported.
	sliceMax := filepath.Join(root, "cg/system.slice/memory.max")
	if err := os.WriteFile(sliceMax, []byte("137438953472\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.TotalMemory(); got != 67108864*1024 {
		t.Errorf("total = %d with a 128 GiB limit; want MemTotal", got)
	}
}

func TestLinuxMemoryProviderLimitAndRoomAreSeparateMinimums(t *testing.T) {
	const gib = 1 << 30
	root := writeProcFixture(t, map[string]string{
		"proc/meminfo":     "MemTotal: 67108864 kB\nMemAvailable: 60000000 kB\n",
		"proc/self/cgroup": "0::/parent/agent\n",
		// The parent is the larger limit but has the least room left.
		"cg/parent/memory.max":           "17179869184\n",
		"cg/parent/memory.current":       "16106127360\n",
		"cg/parent/agent/memory.max":     "4294967296\n",
		"cg/parent/agent/memory.current": "536870912\n",
	})
	p := &LinuxMemoryProvider{ProcRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cg")}
	if got, err := p.TotalMemory(); err != nil || got != 4*gib {
		t.Errorf("total = %d, %v; want the agent cgroup's 4 GiB limit, the smallest memory.max", got, err)
	}
	if got, err := p.AvailableMemory(); err != nil || got != 1*gib {
		t.Errorf("available = %d, %v; want the parent's 1 GiB of room, the least room left", got, err)
	}

	host := p.HostWide()
	if got, _ := host.TotalMemory(); got != 67108864*1024 {
		t.Errorf("host-wide total = %d; want MemTotal, for a daemon outside the agent's cgroup", got)
	}
	if got, _ := host.AvailableMemory(); got != 60000000*1024 {
		t.Errorf("host-wide available = %d; want MemAvailable", got)
	}
}

func TestLinuxMemoryProviderUnreadableProc(t *testing.T) {
	p := &LinuxMemoryProvider{ProcRoot: filepath.Join(t.TempDir(), "missing"), CgroupRoot: t.TempDir()}
	if _, err := p.TotalMemory(); err == nil {
		t.Error("TotalMemory with no meminfo: want an error")
	}
	if _, err := p.AvailableMemory(); err == nil {
		t.Error("AvailableMemory with no meminfo: want an error, not zero (zero reads as Critical)")
	}
	if _, err := p.WiredMemory(); err == nil {
		t.Error("WiredMemory with no meminfo: want an error")
	}
}

func TestHostWideProviderKeepsOtherProviders(t *testing.T) {
	other := &DarwinMemoryProvider{}
	if got, distinct := hostWideProvider(other); got != MemoryProvider(other) || distinct {
		t.Errorf("hostWideProvider(%T) = %T, %v; want the provider unchanged, not distinct", other, got, distinct)
	}
	lp := &LinuxMemoryProvider{ProcRoot: "/p", CgroupRoot: "/c"}
	got, distinct := hostWideProvider(lp)
	hw, ok := got.(*LinuxMemoryProvider)
	if !ok || !distinct || !hw.IgnoreCgroup || hw.ProcRoot != "/p" || hw.CgroupRoot != "/c" || hw == lp {
		t.Errorf("hostWideProvider(linux) = %+v, %v; want a new, distinct host-wide provider on the same roots",
			hw, distinct)
	}
}
