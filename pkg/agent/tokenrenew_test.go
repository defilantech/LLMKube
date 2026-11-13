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
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func jwtWithClaims(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(claims)) + ".c2ln"
}

func TestParseServiceAccountToken(t *testing.T) {
	tok, err := parseServiceAccountToken(jwtWithClaims(
		`{"iat":1000,"exp":3000,"kubernetes.io":{"namespace":"macs","serviceaccount":{"name":"llmkube-metal-mac-a"}}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tok.namespace != "macs" || tok.name != "llmkube-metal-mac-a" || tok.lifetime != 2000*time.Second ||
		!tok.renewAt.Equal(time.Unix(2000, 0)) || !tok.expires.Equal(time.Unix(3000, 0)) {
		t.Errorf("parsed %+v, want macs/llmkube-metal-mac-a, lifetime 2000s, renewal at half of it", tok)
	}

	tok, err = parseServiceAccountToken(jwtWithClaims(`{"iat":1000,"exp":3000,"sub":"system:serviceaccount:macs:sa-b"}`))
	if err != nil || tok.namespace != "macs" || tok.name != "sa-b" {
		t.Errorf("sub fallback = %+v, %v; want macs/sa-b", tok, err)
	}

	for name, raw := range map[string]string{
		"not a JWT":         "abc",
		"not base64":        "a.%%%.c",
		"a user token":      jwtWithClaims(`{"iat":1000,"exp":3000,"sub":"alice"}`),
		"no expiry":         jwtWithClaims(`{"iat":1000,"sub":"system:serviceaccount:macs:sa"}`),
		"expiry before iat": jwtWithClaims(`{"iat":3000,"exp":1000,"sub":"system:serviceaccount:macs:sa"}`),
	} {
		if _, err := parseServiceAccountToken(raw); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
}

// renewalEnv is a real API server with a per-Mac ServiceAccount allowed only
// to mint its own token, as llmkube metal token create sets it up, and a
// token file holding that ServiceAccount's first token.
type renewalEnv struct {
	admin     client.Client
	agent     client.Client
	cfg       *rest.Config
	namespace string
	sa        string
	tokenFile string
	first     serviceAccountToken
}

func newRenewalEnv(t *testing.T) *renewalEnv {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		matches, _ := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*"))
		if len(matches) == 0 {
			t.Skip("no envtest binaries: run `make setup-envtest` or set KUBEBUILDER_ASSETS")
		}
		assets = matches[0]
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	admin, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	e := &renewalEnv{admin: admin, cfg: cfg, namespace: "macs", sa: "llmkube-metal-mac-a",
		tokenFile: filepath.Join(t.TempDir(), "token")}
	ctx := context.Background()
	for _, obj := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: e.namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: e.sa}},
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: e.sa},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"},
				Verbs: []string{"create"}, ResourceNames: []string{e.sa}}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: e.sa},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: e.sa},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: e.namespace, Name: e.sa}},
		},
	} {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	seconds := int64(3600)
	tr := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: e.sa}}
	if err := admin.SubResource("token").Create(ctx, sa, tr); err != nil {
		t.Fatalf("mint the first token: %v", err)
	}
	if err := os.WriteFile(e.tokenFile, []byte(tr.Status.Token), 0o600); err != nil {
		t.Fatal(err)
	}
	if e.first, err = parseServiceAccountToken(tr.Status.Token); err != nil {
		t.Fatalf("parse the API server's token: %v", err)
	}
	// The agent's client, as its kubeconfig builds it: the token comes from
	// the file.
	agentCfg := rest.AnonymousClientConfig(cfg)
	agentCfg.BearerTokenFile = e.tokenFile
	if e.agent, err = client.New(agentCfg, client.Options{Scheme: clientgoscheme.Scheme}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *renewalEnv) renewer(now time.Time) *tokenRenewer {
	return &tokenRenewer{client: e.agent, path: e.tokenFile, logger: newNopLogger(), now: func() time.Time { return now }}
}

// whoAmI authenticates token and returns the user the API server sees.
func (e *renewalEnv) whoAmI(t *testing.T, token string) (string, error) {
	t.Helper()
	cfg := rest.AnonymousClientConfig(e.cfg)
	cfg.BearerToken = token
	c, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	review := &authenticationv1.SelfSubjectReview{}
	if err := c.Create(context.Background(), review); err != nil {
		return "", err
	}
	return review.Status.UserInfo.Username, nil
}

// TestTokenRenewerRenewsThroughItsOwnServiceAccount runs renewal against a
// real API server with only the per-Mac rule: before half the lifetime the
// file is left alone; after it, the agent's own token buys a new one, which
// replaces the file owner-only and authenticates as the same ServiceAccount.
func TestTokenRenewerRenewsThroughItsOwnServiceAccount(t *testing.T) {
	e := newRenewalEnv(t)
	ctx := context.Background()
	first, _ := os.ReadFile(e.tokenFile)

	wait := e.renewer(e.first.issued.Add(time.Minute)).renewIfDue(ctx)
	if now, _ := os.ReadFile(e.tokenFile); string(now) != string(first) {
		t.Fatal("the token was replaced before half its lifetime")
	}
	if wait != tokenRenewMaxWait {
		t.Errorf("wait before renewal is due = %s, want the %s cap", wait, tokenRenewMaxWait)
	}

	wait = e.renewer(e.first.renewAt.Add(time.Second)).renewIfDue(ctx)
	renewed, err := os.ReadFile(e.tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(renewed) == string(first) {
		t.Fatal("the token was not replaced after half its lifetime")
	}
	if info, _ := os.Stat(e.tokenFile); info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %o, want 600", info.Mode().Perm())
	}
	next, err := parseServiceAccountToken(string(renewed))
	if err != nil {
		t.Fatalf("the renewed file does not hold a ServiceAccount token: %v", err)
	}
	if next.lifetime != e.first.lifetime {
		t.Errorf("renewed lifetime %s, want the first token's %s", next.lifetime, e.first.lifetime)
	}
	// The clock here sits at the first token's half-life, which is also about
	// the new token's: the wait must still not be zero, or renewal would spin.
	if wait < tokenRenewRetry || wait > tokenRenewMaxWait {
		t.Errorf("wait after a renewal = %s, want between %s and %s", wait, tokenRenewRetry, tokenRenewMaxWait)
	}
	user, err := e.whoAmI(t, string(renewed))
	if want := "system:serviceaccount:" + e.namespace + ":" + e.sa; err != nil || user != want {
		t.Errorf("the renewed token authenticates as %q (%v), want %q", user, err, want)
	}
}

// TestTokenRenewerStopsWhenTheServiceAccountIsDeleted: llmkube metal remove
// deletes the ServiceAccount, which revokes its tokens; renewal then fails
// and leaves the file as it was instead of writing anything else.
func TestTokenRenewerStopsWhenTheServiceAccountIsDeleted(t *testing.T) {
	e := newRenewalEnv(t)
	ctx := context.Background()
	first, _ := os.ReadFile(e.tokenFile)
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: e.sa}}
	if err := e.admin.Delete(ctx, sa); err != nil {
		t.Fatal(err)
	}

	wait := e.renewer(e.first.renewAt.Add(time.Second)).renewIfDue(ctx)
	if now, _ := os.ReadFile(e.tokenFile); string(now) != string(first) {
		t.Error("the token file changed although the ServiceAccount is gone")
	}
	if wait != tokenRenewRetry {
		t.Errorf("wait after a failed renewal = %s, want %s", wait, tokenRenewRetry)
	}
}

func TestTokenExpiredHint(t *testing.T) {
	cur := serviceAccountToken{namespace: "edge", name: "llmkube-metal-edge-1"}
	for _, tc := range []struct {
		goos          string
		want, notWant []string
	}{
		{goos: "darwin", want: []string{"join the host again", "llmkube metal join"}},
		{
			goos:    "linux",
			want:    []string{"kubectl -n edge create token llmkube-metal-edge-1", "write it over /srv/agent/token"},
			notWant: []string{"metal join"},
		},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			got := tokenExpiredHint(tc.goos, cur, "/srv/agent/token")
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("tokenExpiredHint(%q) = %q, want it to contain %q", tc.goos, got, w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("tokenExpiredHint(%q) = %q, want no %q", tc.goos, got, w)
				}
			}
		})
	}
}

// An expired token whose renewal fails logs this OS's hint at Error.
func TestTokenRenewerLogsExpiredHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	claims := `{"iat":1000,"exp":3000,` +
		`"kubernetes.io":{"namespace":"edge","serviceaccount":{"name":"llmkube-metal-edge-1"}}}`
	if err := os.WriteFile(path, []byte(jwtWithClaims(claims)), 0o600); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		SubResourceCreate: func(context.Context, client.Client, string, client.Object, client.Object,
			...client.SubResourceCreateOption) error {
			return errors.New("Unauthorized")
		},
	}).Build()
	core, logs := observer.New(zapcore.InfoLevel)
	r := &tokenRenewer{client: c, path: path, logger: zap.New(core).Sugar(),
		now: func() time.Time { return time.Unix(4000, 0) }}

	if wait := r.renewIfDue(context.Background()); wait != tokenRenewRetry {
		t.Errorf("wait = %s, want %s", wait, tokenRenewRetry)
	}
	entries := logs.FilterMessageSnippet("has expired and cannot be renewed").All()
	if len(entries) != 1 || entries[0].Level != zapcore.ErrorLevel {
		t.Fatalf("expired-token entries = %v, want one at Error", entries)
	}
	want := tokenExpiredHint(runtime.GOOS, serviceAccountToken{namespace: "edge", name: "llmkube-metal-edge-1"}, path)
	if !strings.Contains(entries[0].Message, want) {
		t.Errorf("message = %q, want it to end with %q", entries[0].Message, want)
	}
}
