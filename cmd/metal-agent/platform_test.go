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

package main

import (
	"testing"

	"go.uber.org/zap"

	"github.com/defilantech/llmkube/internal/platform"
)

func TestCheckHostPlatform(t *testing.T) {
	origCaps, origGOOS, origGlob := detectCapabilities, hostGOOS, globDevices
	t.Cleanup(func() { detectCapabilities, hostGOOS, globDevices = origCaps, origGOOS, origGlob })

	mac := platform.Capabilities{Metal: true, GPUCores: 40, OS: "darwin", Arch: "arm64"}
	linuxHost := platform.Capabilities{OS: "linux", Arch: "arm64"}
	hailoDevice := func(string) ([]string, error) { return []string{"/dev/hailo0"}, nil }
	noDevice := func(string) ([]string, error) { return nil, nil }

	for _, tc := range []struct {
		name, goos string
		caps       platform.Capabilities
		glob       func(string) ([]string, error)
		wantErr    bool
	}{
		{name: "mac with Metal", goos: "darwin", caps: mac, glob: noDevice},
		{name: "linux with a Hailo device", goos: "linux", caps: linuxHost, glob: hailoDevice},
		{name: "linux without a device still starts", goos: "linux", caps: linuxHost, glob: noDevice},
		{name: "mac without Metal", goos: "darwin", caps: platform.Capabilities{OS: "darwin"}, glob: noDevice, wantErr: true},
		{name: "other OS", goos: "freebsd", caps: platform.Capabilities{OS: "freebsd"}, glob: hailoDevice, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostGOOS = tc.goos
			detectCapabilities = func() platform.Capabilities { return tc.caps }
			globDevices = tc.glob
			caps, err := checkHostPlatform(zap.NewNop().Sugar())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err == nil && caps != tc.caps {
				t.Errorf("caps = %+v, want the detected %+v passed through", caps, tc.caps)
			}
		})
	}
}
