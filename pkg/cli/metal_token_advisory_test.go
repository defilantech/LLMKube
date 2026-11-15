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
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestMetalTokenCreateOtherMacWarningIsAdvisory: the other-Mac warning reads
// MetalNodes, and a cluster without the CRD, or an admin who cannot list them,
// must still get a join string. The list is advisory, so its failure warns
// about nothing and does not fail the create.
func TestMetalTokenCreateOtherMacWarningIsAdvisory(t *testing.T) {
	var log bytes.Buffer
	warnAboutOtherMacs(context.Background(),
		failingListReader{err: errors.New("metalnodes is forbidden")}, "ns", "mac-b", &log)
	if log.Len() != 0 {
		t.Errorf("a failed MetalNode list warned anyway: %q", log.String())
	}
}

// TestMetalTargetContextName: client-go refuses a kubeconfig with no named
// current context before the command runs, so the placeholder is a fallback
// that only a future client-go change reaches. It still names a context
// rather than printing nothing.
func TestMetalTargetContextName(t *testing.T) {
	if got := metalTargetContextName("envtest"); got != "envtest" {
		t.Errorf("metalTargetContextName(envtest) = %q", got)
	}
	if got := metalTargetContextName(""); got != "(current context)" {
		t.Errorf("metalTargetContextName(\"\") = %q, want the placeholder", got)
	}
}

// TestIsLoopbackHost pins what counts as loopback: IP literals in 127.0.0.0/8
// or ::1, and the name localhost. A name that merely resolves to loopback is
// not detected, which is the documented limit of the check.
func TestIsLoopbackHost(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.53", true},
		{"127.255.255.254", true},
		{"::1", true},
		{"0:0:0:0:0:0:0:1", true},
		{"localhost", true},
		{"LOCALHOST", true},
		{"10.0.0.1", false},
		{"100.64.0.1", false},
		{"192.168.1.10", false},
		{"api.internal", false},
		{"my-mac.local", false},
		{"fe80::1", false},
	} {
		if got := isLoopbackHost(tc.host); got != tc.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// TestWarnLoopbackJoinServer pins the warning text and that a non-loopback
// server says nothing.
func TestWarnLoopbackJoinServer(t *testing.T) {
	var log bytes.Buffer
	warnLoopbackJoinServer(&log, "https://127.0.0.1:6443", false)
	for _, want := range []string{"loopback", "only a Mac on this machine", "--server"} {
		if !bytes.Contains(log.Bytes(), []byte(want)) {
			t.Errorf("warning %q does not mention %q", log.String(), want)
		}
	}
	log.Reset()
	warnLoopbackJoinServer(&log, "https://10.0.0.1:6443", false)
	if log.Len() != 0 {
		t.Errorf("a routable server warned: %q", log.String())
	}
	// A loopback --server warns too, and names the address given.
	log.Reset()
	warnLoopbackJoinServer(&log, "https://127.0.0.1:6443", true)
	for _, want := range []string{"--server https://127.0.0.1:6443 is a loopback address", "only a Mac on this machine"} {
		if !bytes.Contains(log.Bytes(), []byte(want)) {
			t.Errorf("--server warning %q does not mention %q", log.String(), want)
		}
	}
}

// failingListReader is a client.Reader whose List always fails.
type failingListReader struct{ err error }

func (f failingListReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return f.err
}

func (f failingListReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return f.err
}

var _ io.Writer = (*bytes.Buffer)(nil)
