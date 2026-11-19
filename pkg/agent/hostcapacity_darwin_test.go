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
	"errors"
	"strings"
	"testing"
)

func TestHostCapacityFromSysctl(t *testing.T) {
	values := map[string]string{
		"hw.memsize":                "137438953472\n",
		"machdep.cpu.brand_string":  "Apple M5 Max\n",
		"unrelated.key.never.asked": "x",
	}
	sysctl := func(name string) (string, error) {
		v, ok := values[name]
		if !ok {
			return "", errors.New("unknown oid")
		}
		return v, nil
	}
	got := hostCapacityFromSysctl(sysctl)
	if got.MemoryBytes != 137438953472 || got.Chip != "Apple M5 Max" {
		t.Errorf("capacity = %+v, want 128 GiB and the trimmed brand string", got)
	}

	failing := func(string) (string, error) { return "", errors.New("sysctl missing") }
	if got := hostCapacityFromSysctl(failing); got != (HostCapacity{}) {
		t.Errorf("capacity with sysctl failing = %+v, want empty rather than a guess", got)
	}

	values["machdep.cpu.brand_string"] = "Apple \u202eM5\u200b Max\u0085"
	if got := hostCapacityFromSysctl(sysctl).Chip; got != "Apple M5 Max" {
		t.Errorf("chip = %q, want bidi, zero-width and C1 characters stripped (the controller rejects them)", got)
	}

	values["hw.memsize"] = "lots"
	values["machdep.cpu.brand_string"] = strings.Repeat("M", 300) + "\x01"
	got = hostCapacityFromSysctl(sysctl)
	if got.MemoryBytes != 0 {
		t.Errorf("unparseable memsize gave %d, want 0", got.MemoryBytes)
	}
	if len(got.Chip) > 128 || strings.ContainsRune(got.Chip, 0x01) {
		t.Errorf("chip %q: want at most 128 printable bytes, the bound the controller enforces", got.Chip)
	}
}
