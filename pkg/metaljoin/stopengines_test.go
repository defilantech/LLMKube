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

package metaljoin

import (
	"reflect"
	"testing"
)

func TestStopEnginesArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"none set", []string{"--metal-node", "mac-a", "--renew-service-account-token"},
			[]string{StopEnginesCommand, "--kubeconfig", "/default"}},
		{"two-argument flags", []string{"--kubeconfig", "/kc", "--namespace", "team", "--state-dir", "/s"},
			[]string{StopEnginesCommand, "--kubeconfig", "/kc", "--namespace", "team", "--state-dir", "/s"}},
		{"--flag=value, as join writes --agent-arg", []string{"--kubeconfig", "/kc", "--state-dir=/s",
			"--ollama-port=11500"},
			[]string{StopEnginesCommand, "--kubeconfig", "/kc", "--state-dir", "/s", "--ollama-port", "11500"}},
		{"one dash", []string{"-namespace=team", "-state-dir", "/s"},
			[]string{StopEnginesCommand, "--kubeconfig", "/default", "--namespace", "team", "--state-dir", "/s"}},
		{"the last one wins, as in the agent", []string{"--state-dir", "/first", "--state-dir=/last"},
			[]string{StopEnginesCommand, "--kubeconfig", "/default", "--state-dir", "/last"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StopEnginesArgs(tc.args, "/default"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("StopEnginesArgs(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
