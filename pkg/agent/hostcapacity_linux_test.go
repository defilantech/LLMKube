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

// x86CPUInfo is the head of an x86 kernel's /proc/cpuinfo, which names the
// CPU on a "model name" line.
const x86CPUInfo = "processor\t: 0\nvendor_id\t: GenuineIntel\ncpu family\t: 6\nmodel\t\t: 158\n" +
	"model name\t: Intel(R) Core(TM) i7-8700 CPU @ 3.20GHz\nstepping\t: 10\n"

// arm64CPUInfo is an arm64 kernel's /proc/cpuinfo, which has no "model name"
// line for a 64-bit reader; the board's name follows the per-CPU blocks on a
// "Model" line.
const arm64CPUInfo = "processor\t: 0\nBogoMIPS\t: 108.00\nFeatures\t: fp asimd evtstrm aes pmull sha1 sha2 crc32\n" +
	"CPU implementer\t: 0x41\nCPU architecture: 8\nCPU variant\t: 0x4\nCPU part\t: 0xd0b\nCPU revision\t: 1\n\n" +
	"Revision\t: d04170\nSerial\t\t: 0000000000000000\nModel\t\t: Example Board Model B Rev 1.0\n"

func TestHostChipFromProc(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"device-tree model over cpuinfo": {
			files: map[string]string{
				"/proc/device-tree/model": "Example Board Model B Rev 1.0\x00",
				"/proc/cpuinfo":           x86CPUInfo,
			},
			want: "Example Board Model B Rev 1.0",
		},
		"x86 model name": {
			files: map[string]string{"/proc/cpuinfo": x86CPUInfo},
			want:  "Intel(R) Core(TM) i7-8700 CPU @ 3.20GHz",
		},
		"arm64 Model line": {
			files: map[string]string{"/proc/cpuinfo": arm64CPUInfo},
			want:  "Example Board Model B Rev 1.0",
		},
		"all-NUL device-tree model falls through to cpuinfo": {
			files: map[string]string{
				"/proc/device-tree/model": "\x00\x00\x00\x00",
				"/proc/cpuinfo":           arm64CPUInfo,
			},
			want: "Example Board Model B Rev 1.0",
		},
		"no /proc": {files: map[string]string{}, want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			read := func(p string) ([]byte, error) {
				if v, ok := tc.files[p]; ok {
					return []byte(v), nil
				}
				return nil, errors.New("no such file")
			}
			if got := hostChipFromProc(read); got != tc.want {
				t.Errorf("chip = %q, want %q", got, tc.want)
			}
		})
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
