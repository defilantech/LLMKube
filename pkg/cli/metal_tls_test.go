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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type testCA struct {
	cert *x509.Certificate
	key  crypto.Signer
}

func newTestCA(t *testing.T, cn string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{cert: cert, key: key}
}

func (ca testCA) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// issue returns a serving certificate signed by ca for the given SANs.
func (ca testCA) issue(t *testing.T, dnsNames []string, ips []net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "kube-apiserver"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// tlsAPIServer serves cert and records whether any request reached it (the
// probe must only handshake, never send a request).
func tlsAPIServer(t *testing.T, cert tls.Certificate) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &requests
}

func writeKubeconfig(t *testing.T, cluster *clientcmdapi.Cluster, namespace string) {
	t.Helper()
	kc := clientcmdapi.NewConfig()
	kc.Clusters["c"] = cluster
	kc.AuthInfos["u"] = &clientcmdapi.AuthInfo{Token: "unused"}
	kc.Contexts["c"] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "u", Namespace: namespace}
	kc.CurrentContext = "c"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kc, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
}

func TestLoadMetalTarget(t *testing.T) {
	ca := newTestCA(t, "kc-ca")
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, ca.pem(), 0o600); err != nil {
		t.Fatal(err)
	}

	withCA := func(server string) *clientcmdapi.Cluster {
		return &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: ca.pem()}
	}

	t.Run("context namespace and server", func(t *testing.T) {
		writeKubeconfig(t, withCA("https://10.0.0.1:6443"), "team-a")
		got, err := loadMetalTarget("", "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Namespace != "team-a" || got.Server != "https://10.0.0.1:6443" || got.KubeServer != "https://10.0.0.1:6443" {
			t.Errorf("target = ns %q server %q kube %q", got.Namespace, got.Server, got.KubeServer)
		}
		if len(got.CAs) != 1 || !got.CAs[0].Equal(ca.cert) {
			t.Errorf("CAs = %v, want the kubeconfig CA", got.CAs)
		}
	})
	t.Run("no context namespace means default", func(t *testing.T) {
		writeKubeconfig(t, withCA("https://10.0.0.1:6443"), "")
		got, err := loadMetalTarget("", "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Namespace != "default" {
			t.Errorf("namespace = %q, want default", got.Namespace)
		}
	})
	t.Run("flags override", func(t *testing.T) {
		writeKubeconfig(t, withCA("https://10.0.0.1:6443"), "team-a")
		got, err := loadMetalTarget("team-b", "https://100.64.0.9:6443")
		if err != nil {
			t.Fatal(err)
		}
		if got.Namespace != "team-b" || got.Server != "https://100.64.0.9:6443" || got.KubeServer != "https://10.0.0.1:6443" {
			t.Errorf("target = ns %q server %q kube %q", got.Namespace, got.Server, got.KubeServer)
		}
	})
	t.Run("CA file", func(t *testing.T) {
		writeKubeconfig(t, &clientcmdapi.Cluster{Server: "https://10.0.0.1:6443", CertificateAuthority: caFile}, "")
		got, err := loadMetalTarget("", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.CAs) != 1 || !got.CAs[0].Equal(ca.cert) {
			t.Errorf("CAs = %v, want the CA file's", got.CAs)
		}
	})
	t.Run("kubeconfig tls-server-name", func(t *testing.T) {
		writeKubeconfig(t, &clientcmdapi.Cluster{
			Server: "https://10.0.0.1:6443", CertificateAuthorityData: ca.pem(), TLSServerName: "kubernetes",
		}, "")
		got, err := loadMetalTarget("", "")
		if err != nil {
			t.Fatal(err)
		}
		if got.TLSServerName != "kubernetes" {
			t.Errorf("TLSServerName = %q, want kubernetes", got.TLSServerName)
		}
	})
	for _, tc := range []struct {
		name    string
		cluster *clientcmdapi.Cluster
		server  string
		want    string
	}{
		{"insecure", &clientcmdapi.Cluster{Server: "https://10.0.0.1:6443", InsecureSkipTLSVerify: true}, "",
			"insecure-skip-tls-verify"},
		{"no CA", &clientcmdapi.Cluster{Server: "https://10.0.0.1:6443"}, "", "certificate authority"},
		{"CA data not a certificate", &clientcmdapi.Cluster{
			Server: "https://10.0.0.1:6443", CertificateAuthorityData: []byte("junk"),
		}, "", "certificate"},
		{"http kubeconfig server", withCA("http://10.0.0.1:8080"), "", "https"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeKubeconfig(t, tc.cluster, "")
			_, err := loadMetalTarget("", tc.server)
			if err == nil {
				t.Fatal("loadMetalTarget accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestChooseJoinTLS(t *testing.T) {
	ca := newTestCA(t, "cluster-ca")
	other := newTestCA(t, "other-ca")
	loopback := []net.IP{net.ParseIP("127.0.0.1")}

	covered := ca.issue(t, []string{"kubernetes", "kubernetes.default"}, loopback).Leaf
	kubernetesOnly := ca.issue(t, []string{"kubernetes"}, nil).Leaf
	neither := ca.issue(t, []string{"api.internal"}, []net.IP{net.ParseIP("10.0.0.1")}).Leaf
	own := []*x509.Certificate{ca.cert}

	for _, tc := range []struct {
		name     string
		leaf     *x509.Certificate
		cas      []*x509.Certificate
		host     string
		explicit string
		wantName string
		wantErr  []string
	}{
		{name: "host covered", leaf: covered, cas: own, host: "127.0.0.1"},
		{name: "host not covered, kubernetes is", leaf: kubernetesOnly, cas: own, host: "100.64.0.9",
			wantName: "kubernetes"},
		{name: "neither covered", leaf: neither, cas: own, host: "100.64.0.9",
			wantErr: []string{"100.64.0.9", "api.internal", "10.0.0.1", "--tls-server-name"}},
		{name: "explicit name covered", leaf: neither, cas: own, host: "100.64.0.9", explicit: "api.internal",
			wantName: "api.internal"},
		{name: "explicit name not covered", leaf: neither, cas: own, host: "100.64.0.9", explicit: "kubernetes",
			wantErr: []string{"kubernetes", "api.internal"}},
		{name: "explicit name kept when host is covered", leaf: covered, cas: own, host: "127.0.0.1",
			explicit: "kubernetes", wantName: "kubernetes"},
		{name: "bundle picks the CA that signs the leaf", leaf: covered,
			cas: []*x509.Certificate{other.cert, ca.cert}, host: "127.0.0.1"},
		{name: "no CA signs the leaf", leaf: covered, cas: []*x509.Certificate{other.cert}, host: "127.0.0.1",
			wantErr: []string{"does not verify"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotCA, gotName, err := chooseJoinTLS([]*x509.Certificate{tc.leaf}, tc.cas, tc.host, tc.explicit)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("chooseJoinTLS succeeded with name %q", gotName)
				}
				for _, w := range tc.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q does not mention %q", err, w)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("chooseJoinTLS: %v", err)
			}
			if gotName != tc.wantName {
				t.Errorf("tls server name = %q, want %q", gotName, tc.wantName)
			}
			if !gotCA.Equal(ca.cert) {
				t.Errorf("chose CA %q, want %q", gotCA.Subject.CommonName, ca.cert.Subject.CommonName)
			}
		})
	}
}

func TestResolveJoinTLSAgainstALiveServer(t *testing.T) {
	ca := newTestCA(t, "cluster-ca")

	t.Run("address not in the certificate gets tls-server-name kubernetes", func(t *testing.T) {
		srv, requests := tlsAPIServer(t, ca.issue(t, []string{"kubernetes"}, nil))
		target := &metalTarget{Server: srv.URL, KubeServer: srv.URL, CAs: []*x509.Certificate{ca.cert}}
		var log bytes.Buffer
		gotCA, name, err := resolveJoinTLS(context.Background(), target, "", false, &log)
		if err != nil {
			t.Fatal(err)
		}
		if name != "kubernetes" || !gotCA.Equal(ca.cert) {
			t.Errorf("got name %q CA %q", name, gotCA.Subject.CommonName)
		}
		if !strings.Contains(log.String(), "kubernetes") {
			t.Errorf("stderr does not explain the tls-server-name: %q", log.String())
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("the probe sent %d HTTP requests; it must only handshake", n)
		}
	})

	t.Run("join server unreachable falls back to the kubeconfig server's certificate", func(t *testing.T) {
		srv, _ := tlsAPIServer(t, ca.issue(t, []string{"kubernetes"}, []net.IP{net.ParseIP("127.0.0.1")}))
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		dead := "https://" + ln.Addr().String()
		_ = ln.Close()
		target := &metalTarget{Server: dead, KubeServer: srv.URL, CAs: []*x509.Certificate{ca.cert}}
		// Without --assume-same-server, nothing proves --server is this API
		// server, so the command refuses.
		if _, _, err := resolveJoinTLS(context.Background(), target, "", false, &bytes.Buffer{}); err == nil ||
			!strings.Contains(err.Error(), "--assume-same-server") {
			t.Fatalf("unreachable --server without --assume-same-server: err = %v, want a refusal naming the flag", err)
		}
		var log bytes.Buffer
		_, name, err := resolveJoinTLS(context.Background(), target, "", true, &log)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(log.String(), "not verified") {
			t.Errorf("stderr does not say --server was not verified: %q", log.String())
		}
		// The join server's host (127.0.0.1) is in the certificate.
		if name != "" {
			t.Errorf("tls server name = %q, want none", name)
		}
		if !strings.Contains(log.String(), srv.URL) {
			t.Errorf("stderr does not say which server's certificate was checked: %q", log.String())
		}
	})

	t.Run("unreachable with no fallback is an error", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		dead := "https://" + ln.Addr().String()
		_ = ln.Close()
		target := &metalTarget{Server: dead, KubeServer: dead, CAs: []*x509.Certificate{ca.cert}}
		if _, _, err := resolveJoinTLS(context.Background(), target, "", true, &bytes.Buffer{}); err == nil {
			t.Fatal("resolveJoinTLS succeeded against a closed port")
		}
	})
}

// sniAPIServer presents byName[name] to a client that sends a server name it
// has, else def, like an API server with --tls-sni-cert-key or a proxy that
// routes by SNI. seen returns the server names clients sent.
func sniAPIServer(t *testing.T, def tls.Certificate, byName map[string]tls.Certificate) (
	srv *httptest.Server, seen func() []string) {
	t.Helper()
	var mu sync.Mutex
	var names []string
	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	// GetCertificate runs only for a client that sends a name; returning
	// nil falls back to Certificates.
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{def},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			mu.Lock()
			names = append(names, hello.ServerName)
			mu.Unlock()
			if cert, ok := byName[hello.ServerName]; ok {
				return &cert, nil
			}
			return nil, nil
		}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(names)
	}
}

// TestResolveJoinTLSProbesTheChosenServerName: when token create picks a
// tls-server-name because the address is not in the certificate, it checks
// the certificate the server presents for that name, against the one CA the
// join string carries, before anything is encoded. The first handshake sends
// no name (the server is an IP address), so without the second one a server
// that picks its certificate by SNI would hand the Mac a certificate the join
// string cannot verify.
func TestResolveJoinTLSProbesTheChosenServerName(t *testing.T) {
	ca := newTestCA(t, "cluster-ca")
	other := newTestCA(t, "other-ca")
	def := ca.issue(t, []string{"kubernetes"}, nil)

	for _, tc := range []struct {
		name       string
		kubernetes tls.Certificate
		cas        []*x509.Certificate
		wantErr    bool
	}{
		{name: "same CA, covers kubernetes", kubernetes: ca.issue(t, []string{"kubernetes"}, nil),
			cas: []*x509.Certificate{ca.cert}},
		{name: "another CA", kubernetes: other.issue(t, []string{"kubernetes"}, nil),
			cas: []*x509.Certificate{ca.cert}, wantErr: true},
		{name: "same CA, does not cover kubernetes", kubernetes: ca.issue(t, []string{"api.internal"}, nil),
			cas: []*x509.Certificate{ca.cert}, wantErr: true},
		// The kubeconfig trusts both CAs, but the join string carries only the
		// one that verified the first certificate, and so does the Mac.
		{name: "another CA of the kubeconfig's bundle", kubernetes: other.issue(t, []string{"kubernetes"}, nil),
			cas: []*x509.Certificate{ca.cert, other.cert}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, seen := sniAPIServer(t, def, map[string]tls.Certificate{"kubernetes": tc.kubernetes})
			target := &metalTarget{Server: srv.URL, KubeServer: srv.URL, CAs: tc.cas}
			gotCA, name, err := resolveJoinTLS(context.Background(), target, "", false, &bytes.Buffer{})
			if !slices.Contains(seen(), "kubernetes") {
				t.Errorf("no handshake sent the server name kubernetes (sent %q)", seen())
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveJoinTLS accepted tls-server-name %q, which the server presents an "+
						"unverifiable certificate for", name)
				}
				for _, w := range []string{`"kubernetes"`, "--tls-server-name"} {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q does not mention %s", err, w)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if name != "kubernetes" || !gotCA.Equal(ca.cert) {
				t.Errorf("got name %q CA %q", name, gotCA.Subject.CommonName)
			}
		})
	}
}

// TestFetchServerChainRejectsAForeignCA: a certificate from another CA is an
// error whether or not it also misses the host name. (x509 checks the name
// before the chain, so a name mismatch alone says nothing about the chain.)
func TestFetchServerChainRejectsAForeignCA(t *testing.T) {
	good := newTestCA(t, "cluster-ca")
	evil := newTestCA(t, "evil-ca")
	for name, sans := range map[string][]net.IP{"name mismatch": nil, "name matches": {net.ParseIP("127.0.0.1")}} {
		t.Run(name, func(t *testing.T) {
			srv, _ := tlsAPIServer(t, evil.issue(t, []string{"kubernetes"}, sans))
			chain, err := fetchServerChain(context.Background(), srv.URL, "", []*x509.Certificate{good.cert})
			if err == nil {
				t.Fatalf("fetchServerChain accepted a certificate from a foreign CA (chain of %d)", len(chain))
			}
		})
	}
}
