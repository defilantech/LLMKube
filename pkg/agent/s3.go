/*
Copyright 2025.

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

// S3 support for the metal fetch path. The controller learned about s3:// in
// #1098 / #1450 (internal/controller/source.go, model_controller.go), but the
// metal-agent fetches on-node instead of deferring to the init container, so it
// needs its own in-process sigv4 downloader. This mirrors that controller code
// rather than inventing a second signing implementation: the same four secret
// keys, the same path-style endpoint shape, the same SigV4 round-tripper.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// s3Credentials holds the resolved AWS credentials/endpoint for an s3:// source.
// Mirrors the controller's s3Creds (internal/controller/model_controller.go).
type s3Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	Region          string
	Endpoint        string
}

// s3SecretRef is the Model's spec.sourceSecretRef, used to resolve credentials
// for s3:// fetches on the metal path. It is a plain alias so the executor
// signatures stay within the line limit.
type s3SecretRef = *corev1.LocalObjectReference

// isS3Source reports whether source is an s3:// URL. Case-folded to agree with
// the controller's isS3Source (internal/controller/source.go): url.Parse
// lowercases schemes, so a case-variant scheme ("S3://...") must not dodge this
// classifier and fall through to the plain-GET path.
func isS3Source(source string) bool {
	return len(source) >= len("s3://") && strings.EqualFold(source[:len("s3://")], "s3://")
}

// parseS3Source splits s3://bucket/key into bucket and key. Endpoint, region,
// and credentials are NOT in the URL; they come from the sourceSecretRef env
// (AWS_ENDPOINT_URL, AWS_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY).
// Mirrors the controller's parseS3Source (internal/controller/source.go).
func parseS3Source(source string) (bucket, key string, err error) {
	if !isS3Source(source) {
		return "", "", fmt.Errorf("not an S3 source: %s", source)
	}

	// Slice off the scheme by length rather than TrimPrefix: isS3Source already
	// guarantees the prefix is present and case-folded, and slicing (unlike a
	// case-sensitive TrimPrefix) leaves a case-variant scheme ("S3://...") with
	// its bucket/key intact. S3 bucket and key are case-sensitive, so the rest
	// of the source must not be lower-cased.
	rest := source[len("s3://"):]
	if rest == "" {
		return "", "", fmt.Errorf("empty S3 source: %s", source)
	}

	slashIdx := strings.Index(rest, "/")
	if slashIdx < 0 {
		return "", "", fmt.Errorf("S3 source must include a key: %s (expected s3://bucket/key)", source)
	}

	bucket = rest[:slashIdx]
	key = rest[slashIdx+1:]

	if bucket == "" {
		return "", "", fmt.Errorf("S3 source has empty bucket: %s", source)
	}
	if key == "" {
		return "", "", fmt.Errorf("S3 source has empty key: %s", source)
	}

	return bucket, key, nil
}

// isHFAuthHost reports whether a bearer token should be attached to this
// source on the metal path.
//
// Deliberately narrower than the controller's isHFAuthSource
// (internal/controller/source.go), which also accepts hf://. The metal
// downloader hands the source straight to http.NewRequestWithContext, and
// nothing here rewrites hf:// the way the init container's
// normalize_hf_source does in the shell, so an hf:// Model on a Metal agent
// fails with `unsupported protocol scheme "hf"` before any credential is
// consulted. Claiming to authenticate a scheme this path cannot fetch would
// describe a step that does not exist. That gap predates this change and is
// tracked as #1759; TestDownloadFile_HFSchemeUnsupported pins the current
// behaviour so a future fix has to update this comment too.
//
// The two predicates are therefore intentionally different rather than a
// copy that might drift: this one answers "can the metal downloader reach
// this host", the controller's answers "will the init container".
func isHFAuthHost(source string) bool {
	rest := source
	for _, scheme := range []string{"https://", "http://"} {
		if len(rest) >= len(scheme) && strings.EqualFold(rest[:len(scheme)], scheme) {
			rest = rest[len(scheme):]
			// Host is case-insensitive per RFC 3986, so fold BEFORE trimming
			// the www. label: trimming first leaves "WWW.huggingface.co"
			// intact and the match fails, downloading unauthenticated.
			rest = strings.ToLower(rest)
			rest = strings.TrimPrefix(rest, "www.")
			return strings.HasPrefix(rest, "huggingface.co/")
		}
	}
	return false
}

// resolveHFToken reads HF_TOKEN out of the Model's sourceSecretRef, the same
// secret the AWS_* keys come from. Unlike the S3 credentials this is NOT a hard
// error when absent: ungated repositories are the common case and must keep
// working with no secret at all, so a missing secret or a missing key simply
// yields an empty token and the request goes out unauthenticated.
func (e *MetalExecutor) resolveHFToken(ctx context.Context, secretName string) string {
	if e.k8sClient == nil || secretName == "" {
		return ""
	}
	secret := &corev1.Secret{}
	if err := e.k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: e.namespace}, secret); err != nil {
		e.logger.Debugw("sourceSecretRef unreadable; continuing unauthenticated",
			"secret", secretName, "namespace", e.namespace, "err", err)
		return ""
	}
	return string(secret.Data["HF_TOKEN"])
}

// resolveS3Credentials reads the AWS_* keys out of the Model's sourceSecretRef.
// The secret lives in the Model's namespace (the namespace the executor was
// constructed with). This mirrors the controller's s3Credentials
// (internal/controller/model_controller.go), but reads through the agent's
// controller-runtime client instead of a rest.Config. A missing secret or a
// secret without both access keys is a hard error: it is better to fail here
// with a clear message than to hand the raw source to an anonymous GET that
// 403s confusingly (the bug this file exists to fix).
func (e *MetalExecutor) resolveS3Credentials(ctx context.Context, source, secretName string) (s3Credentials, error) {
	if e.k8sClient == nil {
		return s3Credentials{}, fmt.Errorf("s3 source %q requires a Kubernetes client to resolve sourceSecretRef; "+
			"the metal-agent could not resolve credentials", source)
	}
	if secretName == "" {
		return s3Credentials{}, fmt.Errorf("s3 source %q requires spec.sourceSecretRef; "+
			"no secret was provided to the agent", source)
	}

	secret := &corev1.Secret{}
	if err := e.k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: e.namespace}, secret); err != nil {
		return s3Credentials{}, fmt.Errorf("read sourceSecretRef %q in namespace %q: %w", secretName, e.namespace, err)
	}

	creds := s3Credentials{
		AccessKeyID:     string(secret.Data["AWS_ACCESS_KEY_ID"]),
		SecretAccessKey: string(secret.Data["AWS_SECRET_ACCESS_KEY"]),
		Region:          string(secret.Data["AWS_REGION"]),
		Endpoint:        string(secret.Data["AWS_ENDPOINT_URL"]),
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return s3Credentials{}, fmt.Errorf("s3 source %q: secret %q is missing AWS_ACCESS_KEY_ID and/or "+
			"AWS_SECRET_ACCESS_KEY; a signed request cannot be produced, so refusing to fall back to an "+
			"anonymous GET", source, secretName)
	}
	return creds, nil
}

// s3DownloadClient builds an *http.Client that signs every request with AWS
// SigV4 for the s3 service, routing through a transport that trusts the
// configured custom CA (when any). objectURL is the path-style endpoint the
// caller should GET: <endpoint>/<bucket>/<key>, matching the controller's
// parseS3GGUFMetadata shape (internal/controller/model_controller.go).
func (e *MetalExecutor) s3DownloadClient(source string, creds s3Credentials) (*http.Client, string, error) {
	if creds.Endpoint == "" {
		return nil, "", fmt.Errorf("s3 source %q: AWS_ENDPOINT_URL not set in secret; "+
			"the agent cannot reach the object store without an endpoint", source)
	}

	// Path-style endpoint: <endpoint>/<bucket>/<key>. The init container's
	// signed curl uses the same shape (${AWS_ENDPOINT_URL}/${S3_BUCKET}/${S3_KEY}).
	bucket, key, perr := parseS3Source(source)
	if perr != nil {
		return nil, "", perr
	}
	objectURL := strings.TrimRight(creds.Endpoint, "/") + "/" + bucket + "/" + key

	var transport http.RoundTripper = http.DefaultTransport.(*http.Transport).Clone()
	if len(e.caCerts) > 0 {
		transport = withCACerts(transport, e.caCerts)
	}

	signer := &sigv4RoundTripper{
		base:      transport,
		accessKey: creds.AccessKeyID,
		secretKey: creds.SecretAccessKey,
		region:    creds.Region,
	}
	return &http.Client{Transport: signer}, objectURL, nil
}

// withCACerts returns a RoundTripper whose TLS config trusts the supplied PEM CA
// bundles, in addition to the system roots. When the s3 endpoint is a private
// MinIO behind a self-signed CA, this is what lets the agent trust it — the
// in-process equivalent of the init container's CURL_CA_BUNDLE (addCACertVolume,
// internal/controller/model_storage.go).
func withCACerts(base http.RoundTripper, caCerts [][]byte) http.RoundTripper {
	t, ok := base.(*http.Transport)
	if !ok {
		return base
	}
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{} //nolint:gosec // MinVersion inherited from Clone()
	}
	t.TLSClientConfig.RootCAs = withCACertPool(t.TLSClientConfig.RootCAs, caCerts)
	return t
}

// withCACertPool returns a cert pool seeded with the system roots plus any
// supplied PEM CA bundles. When the s3 endpoint is a private MinIO behind a
// self-signed CA, this is what lets the agent trust it — the in-process
// equivalent of the init container's CURL_CA_BUNDLE (addCACertVolume,
// internal/controller/model_storage.go).
func withCACertPool(base *x509.CertPool, caCerts [][]byte) *x509.CertPool {
	pool := base
	if pool == nil {
		pool, _ = x509.SystemCertPool()
	}
	if pool == nil {
		pool = x509.NewCertPool()
	}
	for _, ca := range caCerts {
		pool.AppendCertsFromPEM(ca)
	}
	return pool
}

// sigv4RoundTripper signs every request with AWS Signature Version 4 for the s3
// service, then delegates to the wrapped transport. This is the in-process
// equivalent of the init container's
//
//	curl --aws-sigv4 "aws:amz:${AWS_REGION}:s3" \
//	  -u "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}"
//
// and is a faithful copy of the controller's sigv4RoundTripper
// (internal/controller/model_controller.go).
type sigv4RoundTripper struct {
	base      http.RoundTripper
	accessKey string
	secretKey string
	region    string
}

func (t *sigv4RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	// S3 GET/HEAD carry no body; the payload hash is the SHA256 of the empty
	// string. Model downloads are body-less GETs.
	payloadHash := sha256Hex(nil)

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	canonicalHeaders, signedHeaders := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + t.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := hmacSHA256([]byte("AWS4"+t.secretKey), dateStamp)
	signingKey = hmacSHA256(signingKey, t.region)
	signingKey = hmacSHA256(signingKey, "s3")
	signingKey = hmacSHA256(signingKey, "aws4_request")

	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	auth := "AWS4-HMAC-SHA256 Credential=" + t.accessKey + "/" + scope +
		", SignedHeaders=" + signedHeaders +
		", Signature=" + signature
	req.Header.Set("Authorization", auth)

	return t.base.RoundTrip(req)
}

// canonicalURI percent-encodes each path segment, matching AWS SigV4's
// requirement that the path be URI-encoded (S3 path-style bucket included).
func canonicalURI(u *url.URL) string {
	segments := strings.Split(u.EscapedPath(), "/")
	for i, s := range segments {
		if s == "" {
			continue
		}
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

// canonicalQuery sorts and URI-encodes the query parameters per SigV4.
func canonicalQuery(u *url.URL) string {
	vals := u.Query()
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(k))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(vals[k][0]))
	}
	return b.String()
}

// canonicalHeaders returns the SigV4 canonical headers block and the
// semicolon-joined signed-header list. Only the host and x-amz-* headers are
// signed, which is sufficient for S3 and keeps the signature stable.
func canonicalHeaders(req *http.Request) (string, string) {
	headers := map[string]string{
		"host":                 req.URL.Host,
		"x-amz-date":           req.Header.Get("x-amz-date"),
		"x-amz-content-sha256": req.Header.Get("x-amz-content-sha256"),
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(strings.TrimSpace(headers[k]))
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(keys, ";")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(data))
	return h.Sum(nil)
}
