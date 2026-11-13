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

import "os/exec"

// DetectHostCapacity reads the Mac's memory and chip name through sysctl.
// GPU cores and the Metal version come from platform.DetectMetal,
// which the agent already runs at startup.
func DetectHostCapacity() HostCapacity {
	return hostCapacityFromSysctl(func(name string) (string, error) {
		out, err := exec.Command("sysctl", "-n", name).Output()
		return string(out), err
	})
}
