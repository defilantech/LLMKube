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
	"testing"
)

func TestHostChipFromProc(t *testing.T) {
	files := map[string]string{
		"/proc/device-tree/model": "Example Board Model B Rev 1.0\x00",
		"/proc/cpuinfo":           "processor\t: 0\nmodel name\t: Cortex-A76\n",
	}
	read := func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("no such file")
	}

	if got := hostChipFromProc(read); got != "Example Board Model B Rev 1.0" {
		t.Errorf("chip = %q, want the device-tree model without its NUL", got)
	}
	delete(files, "/proc/device-tree/model")
	if got := hostChipFromProc(read); got != "Cortex-A76" {
		t.Errorf("chip without device tree = %q, want the cpuinfo model name", got)
	}
	noProc := func(string) ([]byte, error) { return nil, errors.New("no proc") }
	if got := hostChipFromProc(noProc); got != "" {
		t.Errorf("chip without /proc = %q, want empty rather than a guess", got)
	}
}

func TestDetectHostCapacityOnLinux(t *testing.T) {
	c := DetectHostCapacity()
	if c.MemoryBytes <= 0 {
		t.Errorf("MemoryBytes = %d on a Linux host; want MemTotal or the cgroup limit", c.MemoryBytes)
	}
	if c.GPUCores != 0 || c.MetalVersion != "" {
		t.Errorf("capacity %+v reports Metal fields on Linux", c)
	}
}
