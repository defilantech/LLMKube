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
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// writeKubeconfigWithContexts writes a kubeconfig whose clusters all point at
// cfg, with one context per entry of contexts, and makes it the one token
// create reads. current is the kubeconfig's current-context; empty writes
// none.
func writeKubeconfigWithContexts(t *testing.T, cfg *rest.Config, contexts []string, current string) {
	t.Helper()
	kc := clientcmdapi.NewConfig()
	kc.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
		InsecureSkipTLSVerify:    cfg.Insecure,
	}
	kc.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
	}
	for _, name := range contexts {
		kc.Contexts[name] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "envtest"}
	}
	kc.CurrentContext = current
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kc, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", path)
}

// TestMetalTokenCreateNamesItsTarget: token create says which cluster it is
// about to write to, on stderr, before it writes.
func TestMetalTokenCreateNamesItsTarget(t *testing.T) {
	cfg, _, ns := metalEnvtest(t)
	writeKubeconfigWithContexts(t, cfg, []string{"envtest"}, "envtest")

	_, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
	if err != nil {
		t.Fatalf("token create: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "context envtest") {
		t.Errorf("stderr does not name the kubeconfig context it uses: %q", stderr)
	}
	if !strings.Contains(stderr, cfg.Host) {
		t.Errorf("stderr does not name the API server %s: %q", cfg.Host, stderr)
	}

	// The line names the server the CLI talks to, not the one the Mac will:
	// a --server the Mac can reach leaves the target line on cfg.Host.
	u, perr := url.Parse(cfg.Host)
	if perr != nil {
		t.Fatal(perr)
	}
	_, stderr, err = runMetal(t, "token", "create", "mac-a2", "-n", ns,
		"--server", "https://localhost:"+u.Port())
	if err != nil {
		t.Fatalf("token create --server: %v\n%s", err, stderr)
	}
	targetLine, _, _ := strings.Cut(stderr, "\n")
	if !strings.Contains(targetLine, cfg.Host) {
		t.Errorf("target line does not name the CLI's server %s: %q", cfg.Host, targetLine)
	}
	if strings.Contains(targetLine, "localhost") {
		t.Errorf("target line names the join string's --server instead of the cluster token create writes to: %q",
			targetLine)
	}
}

// TestMetalTargetContextName lives in metal_token_advisory_test.go: it names
// a helper base does not have, so the base reproduction file stays compilable
// against base.

// TestMetalTokenCreateContextSelectsTheCluster: --context picks which
// kubeconfig context token create loads and names, so the cluster it writes to
// and the cluster it reports are the same one.
func TestMetalTokenCreateContextSelectsTheCluster(t *testing.T) {
	cfg, c, ns := metalEnvtest(t)
	ctx := context.Background()
	writeKubeconfigWithContexts(t, cfg, []string{"ctx-a", "ctx-b"}, "ctx-a")

	_, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns, "--context", "ctx-b")
	if err != nil {
		t.Fatalf("token create --context ctx-b: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "context ctx-b") {
		t.Errorf("stderr does not name the context --context selected: %q", stderr)
	}
	// The write landed in the cluster --context named: the Mac's objects exist.
	n := newMetalNames("mac-a")
	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: n.ServiceAccount}, sa); err != nil {
		t.Fatalf("token create --context ctx-b wrote no ServiceAccount: %v", err)
	}

	// An unknown --context fails before any write, naming the context.
	_, _, err = runMetal(t, "token", "create", "mac-b", "-n", ns, "--context", "ctx-missing")
	if err == nil || !strings.Contains(err.Error(), "ctx-missing") {
		t.Fatalf("token create --context ctx-missing: err = %v, want a refusal naming ctx-missing", err)
	}
	mb := newMetalNames("mac-b")
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: mb.ServiceAccount},
		&corev1.ServiceAccount{}); err == nil {
		t.Error("a refused --context still wrote mac-b's ServiceAccount")
	}
}

// TestMetalTokenCreateWarnsOnALoopbackServer: a server only a Mac on this
// machine could reach is named as such, and --server is the fix.
func TestMetalTokenCreateWarnsOnALoopbackServer(t *testing.T) {
	cfg, _, ns := metalEnvtest(t)
	u, err := url.Parse(cfg.Host)
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		t.Skipf("envtest listens on %s, which is not a loopback address", u.Hostname())
	}
	writeKubeconfigWithContexts(t, cfg, []string{"envtest"}, "envtest")

	_, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
	if err != nil {
		t.Fatalf("token create: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "only a Mac on this machine") {
		t.Errorf("stderr does not warn that the join string works only here: %q", stderr)
	}
	if !strings.Contains(stderr, "--server") {
		t.Errorf("the warning does not name --server as the fix: %q", stderr)
	}

	// A loopback --server is still loopback: it warns, and the warning
	// cannot ask for the flag that was just given.
	_, stderr, err = runMetal(t, "token", "create", "mac-a", "-n", ns, "--server", cfg.Host, "--assume-same-server")
	if err != nil {
		t.Fatalf("token create --server: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "only a Mac on this machine") {
		t.Errorf("--server was itself loopback, but nothing warned: %q", stderr)
	}
	if !strings.Contains(stderr, "--server "+cfg.Host+" is a loopback address") {
		t.Errorf("the --server warning does not name the address given: %q", stderr)
	}
}

// TestMetalTokenCreateWarnsAboutAnotherMacInTheNamespace: a second Mac in a
// namespace that already has one shares every Metal service, which nothing
// else in the flow says.
func TestMetalTokenCreateWarnsAboutAnotherMacInTheNamespace(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	ctx := context.Background()
	other := &inferencev1alpha1.MetalNode{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "mac-a",
			Labels: map[string]string{inferencev1alpha1.LabelMetalNode: "mac-a"},
		},
		Spec: inferencev1alpha1.MetalNodeSpec{ServiceAccountName: "llmkube-metal-mac-a", LeaseName: "mac-a"},
	}
	if err := c.Create(ctx, other); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runMetal(t, "token", "create", "mac-b", "-n", ns)
	if err != nil {
		t.Fatalf("token create: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "mac-a") {
		t.Errorf("stderr does not name the Mac already in the namespace: %q", stderr)
	}
	if !strings.Contains(stderr, "namespace per Mac") || !strings.Contains(stderr, "allowlist") {
		t.Errorf("the warning does not name the two fixes: %q", stderr)
	}

	// A MetalNode in another namespace is not in this Mac's namespace, so it
	// is not what the second Mac would share a service with.
	otherNS := ns[:40] + "-other"
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: otherNS}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &inferencev1alpha1.MetalNode{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: otherNS, Name: "mac-elsewhere",
			Labels: map[string]string{inferencev1alpha1.LabelMetalNode: "mac-elsewhere"},
		},
		Spec: inferencev1alpha1.MetalNodeSpec{ServiceAccountName: "sa", LeaseName: "l"},
	}); err != nil {
		t.Fatal(err)
	}
	// The MetalNode in otherNS is not in the namespace mac-d joins, so a
	// fresh namespace warns about nothing.
	isoNS := ns[:40] + "-iso"
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: isoNS}}); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runMetal(t, "token", "create", "mac-d", "-n", isoNS)
	if err != nil {
		t.Fatalf("token create mac-d: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "namespace per Mac") {
		t.Errorf("token create in a namespace with no other Mac warned anyway: %q", stderr)
	}

	// A reissue for the Mac's own MetalNode must not warn: the Mac's own
	// MetalNode is not another Mac. mac-c is fresh, so the only MetalNode in
	// its namespace will be its own.
	ownNS := ns[:40] + "-own"
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ownNS}}); err != nil {
		t.Fatal(err)
	}
	for _, run := range [][]string{
		{"token", "create", "mac-c", "-n", ownNS},
		{"token", "create", "mac-c", "-n", ownNS},
	} {
		_, stderr, err := runMetal(t, run...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", run, err, stderr)
		}
		if strings.Contains(stderr, "namespace per Mac") {
			t.Errorf("%v warned about another Mac with only mac-c's own MetalNode present: %q", run, stderr)
		}
	}
}

// TestMetalTokenCreateOtherMacWarningIsAdvisory lives in
// metal_token_advisory_test.go: it names the helper directly, so the base
// reproduction file stays compilable against base.
