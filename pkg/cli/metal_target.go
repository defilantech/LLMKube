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
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// metalProbeTimeout bounds the TLS handshake token create uses to read the
// API server's certificate.
const metalProbeTimeout = 10 * time.Second

// metalTarget is what token create needs from the admin's kubeconfig.
type metalTarget struct {
	// Config talks to the cluster as the admin.
	Config *rest.Config
	// Namespace is -n, else the context's namespace, else "default".
	Namespace string
	// Server goes into the join string: --server, else KubeServer.
	Server string
	// KubeServer is the kubeconfig cluster's server.
	KubeServer string
	// CAs are the certificates in the kubeconfig cluster's CA data or file.
	CAs []*x509.Certificate
	// TLSServerName is the kubeconfig cluster's tls-server-name, if any.
	TLSServerName string
}

// loadMetalTarget reads the current kubeconfig context the way kubectl does
// ($KUBECONFIG, else ~/.kube/config). It refuses a cluster entry the Mac
// could not trust on its own: one that skips TLS verification or has no CA.
func loadMetalTarget(namespaceFlag, serverFlag string) (*metalTarget, error) {
	cfg, ns, err := loadMetalAdminConfig(namespaceFlag)
	if err != nil {
		return nil, err
	}
	if err := checkHTTPSServer(cfg.Host, "the kubeconfig's server"); err != nil {
		return nil, err
	}
	if cfg.Insecure {
		return nil, errors.New("the kubeconfig's cluster entry sets insecure-skip-tls-verify, so there is no " +
			"certificate authority to give the Mac; use a kubeconfig with certificate-authority-data")
	}
	caPEM := cfg.CAData
	if len(caPEM) == 0 && cfg.CAFile != "" {
		if caPEM, err = os.ReadFile(cfg.CAFile); err != nil {
			return nil, fmt.Errorf("read the kubeconfig's certificate-authority file: %w", err)
		}
	}
	if len(caPEM) == 0 {
		return nil, errors.New("the kubeconfig's cluster entry has no certificate authority " +
			"(certificate-authority-data or certificate-authority); the Mac trusts only the CA in the join string, " +
			"so token create needs one")
	}
	cas, err := parseCertificates(caPEM)
	if err != nil {
		return nil, fmt.Errorf("the kubeconfig's certificate authority: %w", err)
	}
	server := cfg.Host
	if serverFlag != "" {
		server = serverFlag
	}
	return &metalTarget{
		Config:        cfg,
		Namespace:     ns,
		Server:        server,
		KubeServer:    cfg.Host,
		CAs:           cas,
		TLSServerName: cfg.ServerName,
	}, nil
}

// loadMetalAdminConfig returns the current context's client config and the
// namespace: namespaceFlag, else the context's, else "default".
func loadMetalAdminConfig(namespaceFlag string) (*rest.Config, string, error) {
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig: %w", err)
	}
	ns := namespaceFlag
	if ns == "" {
		if ns, _, err = cc.Namespace(); err != nil {
			return nil, "", fmt.Errorf("read the kubeconfig context's namespace: %w", err)
		}
	}
	return cfg, ns, nil
}

func checkHTTPSServer(server, what string) error {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return fmt.Errorf("%s %q must be an https:// URL with a host", what, server)
	}
	return nil
}

func parseCertificates(pemBytes []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	remaining := pemBytes
	for {
		var block *pem.Block
		block, remaining = pem.Decode(remaining)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("no PEM certificate found")
	}
	return certs, nil
}

// resolveJoinTLS reads the API server's certificate and returns the CA and
// the TLS server name the Mac must use. It checks Server, the address the
// Mac will dial. When that is not reachable from here (a --server only the
// Mac can reach), it refuses unless assumeSameServer, in which case it reads
// the certificate from the kubeconfig's server instead, unverified to be the
// same API server. explicit is --tls-server-name.
func resolveJoinTLS(ctx context.Context, t *metalTarget, explicit string, assumeSameServer bool,
	log io.Writer) (*x509.Certificate, string, error) {
	if explicit == "" {
		explicit = t.TLSServerName
	}
	u, err := url.Parse(t.Server)
	if err != nil {
		return nil, "", fmt.Errorf("parse server %q: %w", t.Server, err)
	}
	probed := t.Server
	chain, err := fetchServerChain(ctx, t.Server, explicit, t.CAs)
	if err != nil && t.KubeServer != "" && t.KubeServer != t.Server {
		var dialErr *net.OpError
		if !errors.As(err, &dialErr) {
			return nil, "", err
		}
		if !assumeSameServer {
			return nil, "", fmt.Errorf("%w; token create checks the certificate --server presents before it "+
				"creates anything. Run it where %s is reachable, or pass --assume-same-server to check the "+
				"certificate %s presents instead", err, t.Server, t.KubeServer)
		}
		_, _ = fmt.Fprintf(log, "cannot reach %s from here (%v); checking the certificate %s presents instead "+
			"(--assume-same-server: not verified that %s serves it)\n", t.Server, err, t.KubeServer, t.Server)
		probed = t.KubeServer
		chain, err = fetchServerChain(ctx, t.KubeServer, explicit, t.CAs)
	}
	if err != nil {
		return nil, "", err
	}
	ca, name, err := chooseJoinTLS(chain, t.CAs, u.Hostname(), explicit)
	if err != nil {
		return nil, "", err
	}
	if name != "" && explicit == "" {
		// The certificate came from a handshake that sent the server's host
		// (or no name at all); the Mac will send name. An API server with
		// --tls-sni-cert-key, or a proxy that routes by SNI, can present
		// another certificate for it, so check what the Mac will get.
		if err := verifyJoinTLS(ctx, probed, name, ca); err != nil {
			return nil, "", err
		}
		_, _ = fmt.Fprintf(log, "the API server's certificate does not cover %s; "+
			"the Mac will verify it as %q (tls-server-name)\n",
			u.Hostname(), name)
	}
	return ca, name, nil
}

// fetchServerChain completes a TLS handshake with server, trusting only the
// kubeconfig CAs, and returns the certificates it presented. A handshake
// that fails on the name alone still returns the chain once the chain
// itself verifies against those CAs (chooseJoinTLS then picks a name the
// certificate does cover); any other failure is an error. No request is sent,
// so no credential crosses this connection.
func fetchServerChain(ctx context.Context, server, sni string, cas []*x509.Certificate) ([]*x509.Certificate, error) {
	roots := x509.NewCertPool()
	for _, c := range cas {
		roots.AddCert(c)
	}
	conn, err := dialServerTLS(ctx, server, &tls.Config{RootCAs: roots, ServerName: sni, MinVersion: tls.VersionTLS12})
	if err != nil {
		var verr *tls.CertificateVerificationError
		var herr x509.HostnameError
		if errors.As(err, &verr) && errors.As(verr.Err, &herr) && len(verr.UnverifiedCertificates) > 0 {
			// x509 checks the name before it builds the chain, so a name
			// mismatch says nothing about the chain: verify it here.
			chain := verr.UnverifiedCertificates
			intermediates := x509.NewCertPool()
			for _, c := range chain[1:] {
				intermediates.AddCert(c)
			}
			if _, cerr := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates}); cerr != nil {
				return nil, fmt.Errorf("TLS handshake with %s against the kubeconfig's certificate authority: %w",
					server, cerr)
			}
			return chain, nil
		}
		return nil, fmt.Errorf("TLS handshake with %s against the kubeconfig's certificate authority: %w", server, err)
	}
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates, nil
}

// verifyJoinTLS completes a TLS handshake with server the way the Mac will:
// sending sni as the server name and trusting only ca, the one CA the join
// string carries. Like fetchServerChain, it sends no request.
func verifyJoinTLS(ctx context.Context, server, sni string, ca *x509.Certificate) error {
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	conn, err := dialServerTLS(ctx, server, &tls.Config{RootCAs: roots, ServerName: sni, MinVersion: tls.VersionTLS12})
	if err == nil {
		_ = conn.Close()
		return nil
	}
	var verr *tls.CertificateVerificationError
	if errors.As(err, &verr) {
		return fmt.Errorf("%s presents a different certificate when asked for TLS server name %q, as the Mac "+
			"will ask, and it does not verify for that name against the certificate authority the join string "+
			"would carry (%w), so the Mac could not join. Pass --tls-server-name with a name the API server "+
			"presents a matching certificate for, or --server with an address its certificate covers",
			server, sni, err)
	}
	return fmt.Errorf("TLS handshake with %s sending server name %q, as the Mac will: %w", server, sni, err)
}

// dialServerTLS completes a TLS handshake with server's host and port (443
// when the URL has none) within metalProbeTimeout.
func dialServerTLS(ctx context.Context, server string, cfg *tls.Config) (*tls.Conn, error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("parse server %q: %w", server, err)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: metalProbeTimeout}, Config: cfg}
	ctx, cancel := context.WithTimeout(ctx, metalProbeTimeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, err
	}
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("connect to %s: not a TLS connection", server)
	}
	return tlsConn, nil
}

// chooseJoinTLS picks the kubeconfig CA certificate that verifies the
// server's leaf on its own (the Mac will trust exactly that one) and the TLS
// server name: explicit if given (it must be in the leaf), none if the
// leaf covers host, else "kubernetes" if the leaf covers that (every
// kubeadm-style API server certificate does), else an error listing what
// the leaf covers.
func chooseJoinTLS(chain, cas []*x509.Certificate, host, explicit string) (*x509.Certificate, string, error) {
	leaf := chain[0]
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	var ca *x509.Certificate
	for _, c := range cas {
		roots := x509.NewCertPool()
		roots.AddCert(c)
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates}); err == nil {
			ca = c
			break
		}
	}
	if ca == nil {
		return nil, "", fmt.Errorf("the API server's certificate (subject %q, issuer %q) does not verify against "+
			"the kubeconfig's certificate authority", leaf.Subject.CommonName, leaf.Issuer.CommonName)
	}
	sans := strings.Join(leafSANs(leaf), ", ")
	switch {
	case explicit != "":
		if err := leaf.VerifyHostname(explicit); err != nil {
			return nil, "", fmt.Errorf("the API server's certificate does not cover the TLS server name %q; it covers: %s",
				explicit, sans)
		}
		return ca, explicit, nil
	case leaf.VerifyHostname(host) == nil:
		return ca, "", nil
	case leaf.VerifyHostname("kubernetes") == nil:
		return ca, "kubernetes", nil
	}
	return nil, "", fmt.Errorf("the API server's certificate does not cover %s, and not \"kubernetes\" either; "+
		"it covers: %s. "+
		"Pass --server with one of those, or --tls-server-name", host, sans)
}

func leafSANs(leaf *x509.Certificate) []string {
	sans := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		sans = append(sans, ip.String())
	}
	if len(sans) == 0 {
		return []string{"(no subject alternative names)"}
	}
	return sans
}
