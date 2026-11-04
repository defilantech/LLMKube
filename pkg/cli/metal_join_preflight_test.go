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

package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// requireJoinStringUnused fails if the join string was redeemed: join
// deletes its Secret only after minting the agent's token.
func requireJoinStringUnused(t *testing.T, c client.Client, ns string) {
	t.Helper()
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "llmkube-metal-mac-a-join"},
		&corev1.Secret{}); err != nil {
		t.Errorf("the join string was used up: get join Secret: %v", err)
	}
}

func requireNothingWritten(t *testing.T, mac *fakeMac) {
	t.Helper()
	if entries, _ := os.ReadDir(mac.home); len(entries) != 0 {
		t.Errorf("a refused join wrote %v", entries)
	}
	for _, c := range mac.calls {
		if !strings.HasPrefix(c, "print ") {
			t.Errorf("a refused join ran launchctl %s", c)
		}
	}
}

// TestMetalJoinRefusesRootBeforeDecodingTheString: sudo llmkube metal join
// would redeem the string and then fail at launchctl bootstrap gui/0. join
// refuses before it even reads the string.
func TestMetalJoinRefusesRootBeforeDecodingTheString(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	joinString, _ := issueJoinString(t, ns)
	for _, s := range []string{"not-a-join-string", joinString} {
		mac := newFakeMac(t)
		mac.uid = 0
		_, _, err := runMetal(t, "join", s, "--wait", "0")
		if err == nil || !strings.Contains(err.Error(), "not as root or with sudo") {
			t.Fatalf("join as root with %.20q: err = %v, want the root refusal", s, err)
		}
		requireNothingWritten(t, mac)
		if len(mac.calls) != 0 {
			t.Errorf("join as root ran launchctl %q", mac.calls)
		}
	}
	requireJoinStringUnused(t, c, ns)
}
