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
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	federationv1alpha1 "github.com/defilantech/llmkube/api/federation/v1alpha1"
)

// TestFleetRegisterEdgeKubeconfigTLS runs the real `fleet register` command
// (kubeconfig loading included) against an envtest apiserver, once per shape
// of the caller's kubeconfig cluster entry, and asserts on how the printed
// edge kubeconfig lets the edge site verify the datacenter API server.
func TestFleetRegisterEdgeKubeconfigTLS(t *testing.T) {
	stubToken(t, "edge-token")
	cfg, adminClient := startFleetTLSEnv(t)

	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, cfg.CAData, 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	wantCAData := "certificate-authority-data: " + base64.StdEncoding.EncodeToString(cfg.CAData)
	const insecureLine = "insecure-skip-tls-verify: true"
	const warning = "WARNING: --insecure-skip-tls-verify"

	tests := []struct {
		name    string
		site    string
		cluster clientcmdapi.Cluster
		flag    bool
		// wantErr lists what the refusal must name; empty means register
		// succeeds.
		wantErr    []string
		wantOut    []string
		notOut     []string
		wantStderr []string
		notStderr  []string
	}{
		{
			name:      "certificate-authority file is read and embedded",
			site:      "edge-ca-file",
			cluster:   clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthority: caFile},
			wantOut:   []string{wantCAData},
			notOut:    []string{"insecure-skip-tls-verify"},
			notStderr: []string{warning},
		},
		{
			name:      "certificate-authority-data is passed through",
			site:      "edge-ca-data",
			cluster:   clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData},
			wantOut:   []string{wantCAData},
			notOut:    []string{"insecure-skip-tls-verify"},
			notStderr: []string{warning},
		},
		{
			name:    "no certificate authority is refused before anything is created",
			site:    "edge-no-ca",
			cluster: clientcmdapi.Cluster{Server: cfg.Host},
			wantErr: []string{"no certificate authority", cfg.Host, "--insecure-skip-tls-verify"},
		},
		{
			name:    "an insecure context is not carried over without the flag",
			site:    "edge-insecure-source",
			cluster: clientcmdapi.Cluster{Server: cfg.Host, InsecureSkipTLSVerify: true},
			wantErr: []string{"skips TLS verification", "--insecure-skip-tls-verify"},
		},
		{
			name:       "the flag on an insecure context writes an insecure kubeconfig and warns",
			site:       "edge-insecure-flag",
			cluster:    clientcmdapi.Cluster{Server: cfg.Host, InsecureSkipTLSVerify: true},
			flag:       true,
			wantOut:    []string{insecureLine},
			notOut:     []string{"TODO", "certificate-authority-data"},
			wantStderr: []string{warning, cfg.Host},
		},
		{
			// A cluster entry cannot carry both: client-go refuses a CA
			// together with insecure-skip-tls-verify.
			name:       "the flag wins over an available certificate authority and warns",
			site:       "edge-ca-and-flag",
			cluster:    clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData},
			flag:       true,
			wantOut:    []string{insecureLine},
			notOut:     []string{"certificate-authority-data"},
			wantStderr: []string{warning},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cluster := tc.cluster
			useFleetTestKubeconfig(t, cfg, &cluster)
			args := []string{"--name", tc.site}
			if tc.flag {
				args = append(args, "--insecure-skip-tls-verify")
			}

			stdout, stderr, err := runFleetRegisterCommand(t, args...)
			if len(tc.wantErr) > 0 {
				assertFleetRegisterRefused(t, adminClient, tc.site, err, stdout, tc.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("fleet register %v: %v", args, err)
			}
			assertOutput(t, "edge kubeconfig", stdout, tc.wantOut, tc.notOut)
			assertOutput(t, "stderr", stderr, tc.wantStderr, tc.notStderr)
			fc := &federationv1alpha1.FederatedCluster{}
			if err := adminClient.Get(context.Background(), types.NamespacedName{Name: tc.site}, fc); err != nil {
				t.Errorf("FederatedCluster %q: %v", tc.site, err)
			}
		})
	}
}

// startFleetTLSEnv starts an envtest apiserver with the CRDs installed and
// the fleet namespace created, and returns its admin config and client.
func startFleetTLSEnv(t *testing.T) (*rest.Config, client.Client) {
	t.Helper()
	binDir := envtestBinaryDir(t)

	// clientcmd's migration rule reads $HOME/.kube; keep it off the real one.
	t.Setenv("HOME", t.TempDir())

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: binDir,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest environment: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest environment: %v", err)
		}
	})
	if len(cfg.CAData) == 0 {
		t.Fatal("envtest config carries no CA data; the test cannot build a CA-backed kubeconfig")
	}

	adminClient, err := client.New(cfg, client.Options{Scheme: fleetTestScheme(t)})
	if err != nil {
		t.Fatalf("build admin client: %v", err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultFleetNamespace}}
	if err := adminClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace %q: %v", defaultFleetNamespace, err)
	}
	return cfg, adminClient
}

// assertFleetRegisterRefused checks that register failed naming every string
// in want, printed no edge kubeconfig, and created nothing on the datacenter,
// so the site can be registered again once the cause is fixed instead of
// failing on AlreadyExists.
func assertFleetRegisterRefused(t *testing.T, c client.Client, site string, err error, stdout string, want []string) {
	t.Helper()
	if err == nil {
		t.Fatalf("fleet register succeeded; want a refusal. Printed:\n%s", stdout)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not mention %q", err, w)
		}
	}
	if stdout != "" {
		t.Errorf("a refused register printed an edge kubeconfig:\n%s", stdout)
	}
	ctx := context.Background()
	err = c.Get(ctx, types.NamespacedName{Name: site}, &federationv1alpha1.FederatedCluster{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("FederatedCluster %q: want NotFound after a refused register, got %v", site, err)
	}
	saKey := types.NamespacedName{Name: fleetServiceAccountPrefix + site, Namespace: defaultFleetNamespace}
	err = c.Get(ctx, saKey, &corev1.ServiceAccount{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("ServiceAccount %s: want NotFound after a refused register, got %v", saKey, err)
	}
}

// assertOutput checks that got contains every string in want and none in
// notWant.
func assertOutput(t *testing.T, what, got string, want, notWant []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%s is missing %q:\n%s", what, w, got)
		}
	}
	for _, w := range notWant {
		if strings.Contains(got, w) {
			t.Errorf("%s contains %q:\n%s", what, w, got)
		}
	}
}

// TestEdgeClusterCAOnlyPlainHTTPNeedsNoCA pins the one case a context with
// no CA may register without the flag: a plain http server, where there is no
// certificate to verify. envtest serves only https, so this runs on the
// helper directly.
func TestEdgeClusterCAOnlyPlainHTTPNeedsNoCA(t *testing.T) {
	for _, tc := range []struct {
		server  string
		wantErr bool
	}{
		{"http://127.0.0.1:8001", false},
		{"https://dc.example.com:6443", true},
		// client-go dials a scheme-less host over TLS once a CA is set, so
		// it is not exempt.
		{"dc.example.com:6443", true},
	} {
		t.Run(tc.server, func(t *testing.T) {
			ca, err := edgeClusterCA(&rest.Config{Host: tc.server}, tc.server, false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("edgeClusterCA(%q) error = %v, want error %v", tc.server, err, tc.wantErr)
			}
			if ca != nil {
				t.Errorf("edgeClusterCA(%q) = %q, want no CA", tc.server, ca)
			}
		})
	}
}

// TestFleetRegisterSkipsVerificationOnlyWhenAsked pins the renderer: with no
// CA and no explicit InsecureSkipTLSVerify, the edge kubeconfig carries no
// TLS setting at all, never insecure-skip-tls-verify.
func TestFleetRegisterSkipsVerificationOnlyWhenAsked(t *testing.T) {
	stubToken(t, "tok")
	for _, tc := range []struct {
		name         string
		insecure     bool
		wantInsecure bool
	}{
		{"not asked", false, false},
		{"asked", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(fleetTestScheme(t)).Build()
			out, err := fleetRegister(context.Background(), c, nil, fleetRegisterInput{
				Name:                  "edge-http",
				DatacenterEndpoint:    "http://127.0.0.1:8001",
				InsecureSkipTLSVerify: tc.insecure,
			})
			if err != nil {
				t.Fatalf("fleetRegister: %v", err)
			}
			if got := strings.Contains(out, "insecure-skip-tls-verify"); got != tc.wantInsecure {
				t.Errorf("insecure-skip-tls-verify present = %v, want %v:\n%s", got, tc.wantInsecure, out)
			}
			if strings.Contains(out, "certificate-authority") {
				t.Errorf("edge kubeconfig carries a CA although none was given:\n%s", out)
			}
		})
	}
}

// useFleetTestKubeconfig writes a kubeconfig whose only context pairs cluster
// with envtest's admin client certificate, and points $KUBECONFIG at it for
// the rest of the test, so the command never reads ~/.kube.
func useFleetTestKubeconfig(t *testing.T, cfg *rest.Config, cluster *clientcmdapi.Cluster) {
	t.Helper()
	kc := clientcmdapi.NewConfig()
	kc.Clusters["datacenter"] = cluster
	kc.AuthInfos["admin"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
	}
	kc.Contexts["datacenter"] = &clientcmdapi.Context{Cluster: "datacenter", AuthInfo: "admin"}
	kc.CurrentContext = "datacenter"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kc, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	t.Setenv(clientcmd.RecommendedConfigPathEnvVar, path)
}

// runFleetRegisterCommand executes `fleet register` with args and returns
// what it wrote to stdout and stderr.
func runFleetRegisterCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newFleetRegisterCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}
