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
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/defilantech/llmkube/pkg/metaljoin"
)

func TestValidateMacName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"mac-studio", true},
		{"m5", true},
		{strings.Repeat("a", 63), true},
		{"", false},
		{"Mac-Studio", false},
		{"mac_studio", false},
		{"-mac", false},
		{"mac.studio", false},
		{strings.Repeat("a", 64), false},
		// Collides with the shared Role, RoleBinding and ServiceAccount
		// llmkube-metal-agent from deployment/macos/metal-agent-rbac.yaml.
		{"agent", false},
		// Mac "x-shared"'s Role and RoleBinding would be named like Mac
		// "x"'s shared RoleBinding.
		{"x-shared", false},
	} {
		err := validateMacName(tc.name)
		if (err == nil) != tc.ok {
			t.Errorf("validateMacName(%q) = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestValidateMetalTTL(t *testing.T) {
	for _, tc := range []struct {
		ttl time.Duration
		ok  bool
	}{
		{30 * time.Minute, true},
		{10 * time.Minute, true},
		{24 * time.Hour, true},
		{10*time.Minute - time.Second, false},
		{24*time.Hour + time.Second, false},
		{0, false},
		{-time.Hour, false},
	} {
		err := validateMetalTTL(tc.ttl)
		if (err == nil) != tc.ok {
			t.Errorf("validateMetalTTL(%s) = %v, want ok=%v", tc.ttl, err, tc.ok)
		}
	}
}

func TestMetalNamesAreDerivedFromTheMac(t *testing.T) {
	got := newMetalNames("mac-a")
	want := metalNames{ //nolint:gosec // G101: object names, not credentials
		Mac:            "mac-a",
		ServiceAccount: "llmkube-metal-mac-a",
		Role:           "llmkube-metal-mac-a",
		RoleBinding:    "llmkube-metal-mac-a",
		SharedBinding:  "llmkube-metal-mac-a-shared",
		Lease:          "mac-a",
		MetalNode:      "mac-a",
		JoinSecret:     "llmkube-metal-mac-a-join",
	}
	if got != want {
		t.Errorf("newMetalNames = %+v, want %+v", got, want)
	}
}

// wantMacARules is the per-Mac Role for Mac "mac-a", written out by hand
// from the design: every rule by resourceNames, nothing else.
func wantMacARules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"},
			Verbs: []string{"create"}, ResourceNames: []string{"llmkube-metal-mac-a"},
		},
		{
			APIGroups: []string{"inference.llmkube.dev"}, Resources: []string{"metalnodes"},
			Verbs: []string{"get"}, ResourceNames: []string{"mac-a"},
		},
		{
			APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"},
			Verbs: []string{"get", "update"}, ResourceNames: []string{"mac-a"},
		},
		{
			APIGroups: []string{""}, Resources: []string{"secrets"},
			Verbs: []string{"delete"}, ResourceNames: []string{"llmkube-metal-mac-a-join"},
		},
	}
}

func TestMetalPerMacRules(t *testing.T) {
	got := metalPerMacRules(newMetalNames("mac-a"))
	want := wantMacARules()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("per-Mac rules:\n got %+v\nwant %+v", got, want)
	}
}

// countingAPIServer stands in for an API server and counts every request, so
// a test can prove a command failed before it talked to the cluster.
func countingAPIServer(t *testing.T) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	kc := clientcmdapi.NewConfig()
	kc.Clusters["c"] = &clientcmdapi.Cluster{Server: srv.URL}
	kc.AuthInfos["u"] = &clientcmdapi.AuthInfo{Token: "unused"}
	kc.Contexts["c"] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "u"}
	kc.CurrentContext = "c"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kc, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	return &hits
}

func runMetal(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := NewMetalCommand()
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err = cmd.ExecuteContext(ctx)
	return out.String(), errOut.String(), err
}

func TestMetalCommandsValidateBeforeAnyAPICall(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"token create bad mac", []string{"token", "create", "Bad_Mac"}, "Bad_Mac"},
		{"token create reserved mac", []string{"token", "create", "agent"}, "agent"},
		{"token create short ttl", []string{"token", "create", "mac-a", "--ttl", "5m"}, "10m"},
		{"token create long ttl", []string{"token", "create", "mac-a", "--ttl", "25h"}, "24h"},
		{"token create bad output", []string{"token", "create", "mac-a", "-o", "yaml"}, "yaml"},
		{"token create http server", []string{"token", "create", "mac-a", "--server", "http://10.0.0.1:6443"}, "https"},
		{"server with credentials", []string{"token", "create", "mac-a", "--server", "https://u:p@10.0.0.1:6443"},
			"credentials"},
		{"server with query", []string{"token", "create", "mac-a", "--server", "https://10.0.0.1:6443/?x=1"}, "query"},
		{"server with fragment", []string{"token", "create", "mac-a", "--server", "https://10.0.0.1:6443#f"}, "fragment"},
		{"remove bad mac", []string{"remove", "-mac"}, "-mac"},
		{"token create no mac", []string{"token", "create"}, "arg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := countingAPIServer(t)
			_, _, err := runMetal(t, tc.args...)
			if err == nil {
				t.Fatal("command succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("the command made %d API requests before failing validation", n)
			}
		})
	}
}

func TestWriteMetalTokenText(t *testing.T) {
	loc := time.FixedZone("PDT", -7*3600)
	res := metalTokenOutput{
		JoinCommand: "llmkube metal join llmkube://abc",
		ExpiresAt:   time.Date(2026, 10, 24, 22, 30, 0, 0, time.UTC),
		CAPin:       "sha256:00ff",
		Namespace:   "default",
		Mac:         "mac-a",
		lifetime:    30 * time.Minute,
	}
	var buf bytes.Buffer
	if err := writeMetalTokenText(&buf, res, loc); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	want := []string{
		"llmkube metal join llmkube://abc",
		"valid for 30 minutes (until 2026-10-24 15:30 PDT), single use, Mac mac-a in namespace default",
		"CA pin: sha256:00ff",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("text output:\n got %q\nwant %q", lines, want)
	}
}

func TestFormatMetalLifetime(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Minute:                "30 minutes",
		10 * time.Minute:                "10 minutes",
		time.Hour:                       "1 hour",
		90 * time.Minute:                "90 minutes",
		24 * time.Hour:                  "24 hours",
		30*time.Minute - 2*time.Second:  "30 minutes",
		time.Minute:                     "1 minute",
		29*time.Minute + 31*time.Second: "30 minutes",
	} {
		if got := formatMetalLifetime(d); got != want {
			t.Errorf("formatMetalLifetime(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestMetalTokenJSONOutput(t *testing.T) {
	res := metalTokenOutput{
		JoinCommand: "llmkube metal join llmkube://abc",
		// client-go hands back the expiry in the local zone; scripts get UTC.
		ExpiresAt: time.Date(2026, 10, 24, 15, 30, 0, 0, time.FixedZone("PDT", -7*3600)),
		CAPin:     "sha256:00ff",
		Namespace: "default",
		Mac:       "mac-a",
		lifetime:  30 * time.Minute,
	}
	var buf bytes.Buffer
	if err := writeMetalTokenJSON(&buf, res); err != nil {
		t.Fatal(err)
	}
	want := `{"joinCommand":"llmkube metal join llmkube://abc","expiresAt":"2026-10-24T22:30:00Z",` +
		`"caPin":"sha256:00ff","namespace":"default","mac":"mac-a"}`
	if strings.TrimSpace(buf.String()) != want {
		t.Errorf("JSON output:\n got %s\nwant %s", buf.String(), want)
	}
}

// TestJoinCommandPrefix keeps the printed command and the join string format
// in step: D2's `llmkube metal join` takes exactly this argument.
func TestJoinCommandPrefix(t *testing.T) {
	if got := metalJoinCommand(metaljoin.Prefix + "x"); got != "llmkube metal join llmkube://x" {
		t.Errorf("metalJoinCommand = %q", got)
	}
}

func TestJWTLifetime(t *testing.T) {
	// header.payload.signature with iat/exp 1800 s apart; the signature is
	// never checked.
	tok := "eyJhbGciOiJSUzI1NiJ9." +
		"eyJpYXQiOjE3OTk5OTk5OTksImV4cCI6MTgwMDAwMTc5OX0." +
		"c2ln"
	d, ok := jwtLifetime(tok)
	if !ok || d != 30*time.Minute {
		t.Errorf("jwtLifetime = %s, %v; want 30m, true", d, ok)
	}
	if _, ok := jwtLifetime("not-a-jwt"); ok {
		t.Error("jwtLifetime accepted a non-JWT")
	}
}

// TestMintJoinToken: the join token is minted through the controller-runtime
// client's serviceaccounts/token subresource (no second clientset), for the
// Mac's ServiceAccount, with the asked lifetime and bound to the join Secret
// by name and UID. envtest checks the minted token itself.
func TestMintJoinToken(t *testing.T) {
	s, err := metalScheme()
	if err != nil {
		t.Fatal(err)
	}
	n := newMetalNames("mac-a")
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: n.ServiceAccount}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: n.JoinSecret, UID: types.UID("u-1")}}

	var calls []string
	var got *authenticationv1.TokenRequest
	c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(s).WithObjects(sa).Build(), interceptor.Funcs{
		SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj, subObj client.Object,
			opts ...client.SubResourceCreateOption) error {
			calls = append(calls, fmt.Sprintf("%s %T %s/%s", sub, obj, obj.GetNamespace(), obj.GetName()))
			if tr, ok := subObj.(*authenticationv1.TokenRequest); ok {
				got = tr.DeepCopy()
			}
			return c.SubResource(sub).Create(ctx, obj, subObj, opts...)
		},
	})
	issued, err := mintJoinToken(context.Background(), c, "ns", n, secret, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"token *v1.ServiceAccount ns/" + n.ServiceAccount}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("subresource creates = %q, want %q", calls, want)
	}
	if got.Spec.ExpirationSeconds == nil || *got.Spec.ExpirationSeconds != 1800 {
		t.Errorf("expirationSeconds = %v, want 1800", got.Spec.ExpirationSeconds)
	}
	wantRef := authenticationv1.BoundObjectReference{Kind: "Secret", APIVersion: "v1", Name: n.JoinSecret, UID: "u-1"}
	if got.Spec.BoundObjectRef == nil || *got.Spec.BoundObjectRef != wantRef {
		t.Errorf("boundObjectRef = %+v, want %+v", got.Spec.BoundObjectRef, wantRef)
	}
	if issued.token != "fake-token" {
		t.Errorf("token = %q, want the one the API returned", issued.token)
	}

	// An API server that answers without a token is an error, not an empty
	// join string.
	empty := interceptor.NewClient(fake.NewClientBuilder().WithScheme(s).WithObjects(sa).Build(), interceptor.Funcs{
		SubResourceCreate: func(context.Context, client.Client, string, client.Object, client.Object,
			...client.SubResourceCreateOption) error {
			return nil
		},
	})
	if _, err := mintJoinToken(context.Background(), empty, "ns", n, secret, 30*time.Minute); err == nil ||
		!strings.Contains(err.Error(), "returned no token") {
		t.Errorf("empty TokenRequest status: err = %v, want a no-token error", err)
	}
}

// TestMetalHelpNamesOnlyExistingCommands: help text must not send users to
// an `llmkube metal` subcommand this build does not have.
func TestMetalHelpNamesOnlyExistingCommands(t *testing.T) {
	root := NewMetalCommand()
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, text := range []string{cmd.Short, cmd.Long, cmd.Example} {
			for _, m := range metalCommandRef.FindAllStringSubmatch(text, -1) {
				if found, _, err := root.Find(strings.Fields(m[1])); err != nil || found == root {
					t.Errorf("help of %q names %q, which is not a command", cmd.CommandPath(), "llmkube metal "+m[1])
				}
			}
		}
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

var metalCommandRef = regexp.MustCompile(`llmkube metal ([a-z]+(?: [a-z]+)?)`)
