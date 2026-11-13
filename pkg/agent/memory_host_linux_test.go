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
	"testing"
)

// TestHostMemoryProviderReadsThisHost reads the real /proc and cgroup v2
// mount. A cgroup limit may or may not be set; the checks hold either way.
func TestHostMemoryProviderReadsThisHost(t *testing.T) {
	p := NewHostMemoryProvider()
	host, distinct := hostWideProvider(p)
	if !distinct {
		t.Fatalf("hostWideProvider(%T) is not a distinct host-wide view", p)
	}

	total, err := p.TotalMemory()
	if err != nil || total == 0 {
		t.Fatalf("TotalMemory = %d, %v; want > 0", total, err)
	}
	avail, err := p.AvailableMemory()
	if err != nil || avail == 0 || avail > total {
		t.Fatalf("AvailableMemory = %d, %v; want > 0 and <= total %d", avail, err, total)
	}
	hostTotal, err := host.TotalMemory()
	if err != nil || hostTotal < total {
		t.Errorf("host-wide TotalMemory = %d, %v; want >= the capped %d", hostTotal, err, total)
	}
	hostAvail, err := host.AvailableMemory()
	// MemAvailable moves between the capped and host-wide reads.
	slack := hostTotal / 32
	if err != nil || hostAvail+slack < avail {
		t.Errorf("host-wide AvailableMemory = %d, %v; want >= the capped %d (less %d slack)",
			hostAvail, err, avail, slack)
	}
	t.Logf("capped total=%s available=%s; host-wide total=%s available=%s",
		formatMemory(total), formatMemory(avail), formatMemory(hostTotal), formatMemory(hostAvail))

	if _, err := p.WiredMemory(); err != nil {
		t.Errorf("WiredMemory: %v", err)
	}
	if rss, err := p.ProcessRSS(os.Getpid()); err != nil || rss == 0 {
		t.Errorf("ProcessRSS(self) = %d, %v; want > 0", rss, err)
	}
}
