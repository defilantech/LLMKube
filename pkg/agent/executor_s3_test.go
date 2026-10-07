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

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// An s3:// source must be signed with AWS SigV4, never sent as a plain GET.
//
// Regression for #1449/#1667: before this fix the metal fetch path handed the
// raw s3:// source string to a plain net/http GET, which cannot sign the
// request, so a private MinIO returns 403. This test points AWS_ENDPOINT_URL at
// a real test server standing in for MinIO and asserts the request the executor
// makes carries a SigV4 Authorization header scoped to the s3 service. A plain
// GET (the buggy behavior) carries no Authorization header, so this fails
// without the fix.
func TestEnsureModel_S3UsesSigV4(t *testing.T) {
	tmpDir := t.TempDir()

	// Stand-in for the S3/MinIO endpoint. It rejects unsigned requests the way
	// a real bucket does, and answers signed ones with the object bytes.
	var gotAuth, gotDate, gotPayloadHash, gotURL string
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDate = r.Header.Get("x-amz-date")
		gotPayloadHash = r.Header.Get("x-amz-content-sha256")
		gotURL = r.URL.String()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			w.WriteHeader(http.StatusForbidden) // anonymous GET refused
			return
		}
		body := []byte("fake-gguf-bytes")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(capture)
	defer srv.Close()

	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}

	// The secret the executor must resolve. The endpoint points at the test
	// server, standing in for the MinIO endpoint.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-models", Namespace: "default"},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("AKIAEXAMPLE0000000"),
			"AWS_SECRET_ACCESS_KEY": []byte("secretaccesskeyvalue0000000000000"),
			"AWS_REGION":            []byte("us-east-1"),
			"AWS_ENDPOINT_URL":      []byte(srv.URL),
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger(),
		WithKubeClient("default", k8sClient, nil), allowTestServers())

	modelDir := filepath.Join(tmpDir, "s3-model")
	path, err := executor.ensureModel(
		t.Context(),
		"s3://models/org/repo/model-Q4_K_M.gguf",
		"s3-model",
		&corev1.LocalObjectReference{Name: "minio-models"},
		"",
	)
	if err != nil {
		t.Fatalf("ensureModel: %v", err)
	}

	// The downloaded file must land at the expected path.
	want := filepath.Join(modelDir, "model-Q4_K_M.gguf")
	if path != want {
		t.Errorf("ensureModel path = %q, want %q", path, want)
	}

	// The request must carry a SigV4 Authorization header scoped to s3.
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE0000000/") {
		t.Errorf("Authorization = %q, want an AWS4-HMAC-SHA256 header with the access key", gotAuth)
	}
	if !strings.Contains(gotAuth, "/us-east-1/s3/aws4_request") {
		t.Errorf("Authorization = %q, want a us-east-1/s3 scope", gotAuth)
	}
	if !strings.Contains(gotAuth, "Signature=") {
		t.Errorf("Authorization = %q, want a Signature", gotAuth)
	}
	if gotDate == "" {
		t.Error("x-amz-date header not set")
	}
	if gotPayloadHash == "" {
		t.Error("x-amz-content-sha256 header not set")
	}

	// The object URL must be the path-style endpoint <endpoint>/<bucket>/<key>,
	// matching the controller and init-container shape.
	if !strings.HasSuffix(gotURL, "/models/org/repo/model-Q4_K_M.gguf") {
		t.Errorf("request URL = %q, want a path-style <endpoint>/models/org/repo/... object URL", gotURL)
	}

	// The body must have been written to disk.
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read downloaded model: %v", err)
	}
	if string(got) != "fake-gguf-bytes" {
		t.Errorf("downloaded body = %q, want %q", string(got), "fake-gguf-bytes")
	}
}

// An s3:// source with Model.spec.sha256 set is verified and stamped exactly
// like the http(s) and hf:// download paths: s3 uses the non-resuming
// copyToFileNoResume, which shares copyToFileResume's verify-then-publish
// step, so this is the s3-specific slice of that same coverage.
func TestEnsureModel_S3SHA256MatchWritesStamp(t *testing.T) {
	tmpDir := t.TempDir()

	body := []byte("fake-gguf-bytes-for-sha256")
	digest := sha256Hex(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-models", Namespace: "default"},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("AKIAEXAMPLE0000000"),
			"AWS_SECRET_ACCESS_KEY": []byte("secretaccesskeyvalue0000000000000"),
			"AWS_REGION":            []byte("us-east-1"),
			"AWS_ENDPOINT_URL":      []byte(srv.URL),
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger(),
		WithKubeClient("default", k8sClient, nil), allowTestServers())

	path, err := executor.ensureModel(t.Context(), "s3://models/org/repo/model.gguf", "s3-sha-model",
		&corev1.LocalObjectReference{Name: "minio-models"}, digest)
	if err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	if !stampHasDigest(t, path+".sha256", digest) {
		t.Errorf("stamp does not name the downloaded digest %q", digest)
	}
}

// An s3:// source whose object bytes do not match Model.spec.sha256 must be
// refused, with the object deleted rather than published.
func TestEnsureModel_S3SHA256MismatchDeletesFile(t *testing.T) {
	tmpDir := t.TempDir()

	body := []byte("fake-gguf-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-models", Namespace: "default"},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("AKIAEXAMPLE0000000"),
			"AWS_SECRET_ACCESS_KEY": []byte("secretaccesskeyvalue0000000000000"),
			"AWS_REGION":            []byte("us-east-1"),
			"AWS_ENDPOINT_URL":      []byte(srv.URL),
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger(),
		WithKubeClient("default", k8sClient, nil), allowTestServers())

	_, err := executor.ensureModel(t.Context(), "s3://models/org/repo/model.gguf", "s3-sha-mismatch-model",
		&corev1.LocalObjectReference{Name: "minio-models"}, sha256Hex([]byte("not-the-right-bytes")))
	if err == nil {
		t.Fatal("ensureModel should fail on SHA256 mismatch for an s3 source")
	}
	if _, statErr := os.Stat(filepath.Join(tmpDir, "s3-sha-mismatch-model", "model.gguf")); !os.IsNotExist(statErr) {
		t.Errorf("mismatched s3 download left the final file")
	}
}

// A missing sourceSecretRef must fail clearly rather than fall through to an
// anonymous GET that 403s confusingly.
func TestEnsureModel_S3MissingSecretRefFails(t *testing.T) {
	tmpDir := t.TempDir()

	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger(),
		WithKubeClient("default", k8sClient, nil), allowTestServers())

	_, err := executor.ensureModel(
		t.Context(),
		"s3://models/org/repo/model.gguf",
		"s3-model",
		nil, // no sourceSecretRef
		"",
	)
	if err == nil {
		t.Fatal("ensureModel with an s3:// source and no sourceSecretRef should fail, not fall through to an anonymous GET")
	}
	if !strings.Contains(err.Error(), "sourceSecretRef") {
		t.Errorf("error = %q, want it to mention sourceSecretRef", err.Error())
	}
}

// An s3:// source whose secret is missing the access keys must fail clearly.
func TestEnsureModel_S3IncompleteSecretFails(t *testing.T) {
	tmpDir := t.TempDir()

	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "partial-creds", Namespace: "default"},
		Data: map[string][]byte{
			// Access key present, secret key missing.
			"AWS_ACCESS_KEY_ID": []byte("AKIAEXAMPLE0000000"),
			"AWS_REGION":        []byte("us-east-1"),
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger(),
		WithKubeClient("default", k8sClient, nil), allowTestServers())

	_, err := executor.ensureModel(
		t.Context(),
		"s3://models/org/repo/model.gguf",
		"s3-model",
		&corev1.LocalObjectReference{Name: "partial-creds"},
		"",
	)
	if err == nil {
		t.Fatal("ensureModel with an s3:// source and an incomplete secret should fail")
	}
	if !strings.Contains(err.Error(), "AWS_SECRET_ACCESS_KEY") {
		t.Errorf("error = %q, want it to name the missing AWS_SECRET_ACCESS_KEY", err.Error())
	}
}

// isS3Source / parseS3Source must classify and split correctly, matching the
// controller helpers they mirror.
func TestS3SourceClassification(t *testing.T) {
	cases := []struct {
		source       string
		wantIsS3     bool
		wantBucket   string
		wantKey      string
		wantParseErr bool
	}{
		{"s3://bucket/key/model.gguf", true, "bucket", "key/model.gguf", false},
		{"s3://models/org/repo/model-Q4_K_M.gguf", true, "models", "org/repo/model-Q4_K_M.gguf", false},
		{"S3://bucket/key", true, "bucket", "key", false}, // case-folded
		{"https://bucket/key", false, "", "", true},
		{"s3://bucket", true, "", "", true}, // missing key
		{"s3://", true, "", "", true},       // empty
	}
	for _, tc := range cases {
		if got := isS3Source(tc.source); got != tc.wantIsS3 {
			t.Errorf("isS3Source(%q) = %v, want %v", tc.source, got, tc.wantIsS3)
		}
		bucket, key, err := parseS3Source(tc.source)
		if tc.wantParseErr {
			if err == nil {
				t.Errorf("parseS3Source(%q) = nil error, want error", tc.source)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseS3Source(%q) error: %v", tc.source, err)
			continue
		}
		if bucket != tc.wantBucket || key != tc.wantKey {
			t.Errorf("parseS3Source(%q) = (%q,%q), want (%q,%q)", tc.source, bucket, key, tc.wantBucket, tc.wantKey)
		}
	}
}

// The sigv4 round-tripper must attach a valid AWS SigV4 Authorization header to
// every request (the in-process equivalent of the init container's signed
// curl). This exercises sigv4RoundTripper.RoundTrip directly.
func TestSigV4RoundTripperSignsRequest(t *testing.T) {
	var gotAuth, gotDate, gotPayloadHash string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		gotDate = req.Header.Get("x-amz-date")
		gotPayloadHash = req.Header.Get("x-amz-content-sha256")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})

	signer := &sigv4RoundTripper{
		base:      base,
		endpoint:  &url.URL{Scheme: "http", Host: "minio.local"},
		accessKey: "AKIAEXAMPLE",
		secretKey: "secret",
		region:    "us-east-1",
	}

	req, err := http.NewRequest(http.MethodGet, "http://minio.local/models/org/repo/model.gguf", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := signer.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/") {
		t.Errorf("Authorization = %q, want AWS4-HMAC-SHA256 with access key", gotAuth)
	}
	if !strings.Contains(gotAuth, "/us-east-1/s3/aws4_request") {
		t.Errorf("Authorization = %q, want region/service scope us-east-1/s3", gotAuth)
	}
	if !strings.Contains(gotAuth, "Signature=") {
		t.Errorf("Authorization = %q, want a Signature", gotAuth)
	}
	if gotDate == "" {
		t.Error("x-amz-date header not set")
	}
	if gotPayloadHash == "" {
		t.Error("x-amz-content-sha256 header not set")
	}
}

// roundTripFunc adapts a func to http.RoundTripper for tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// s3RedirectHop records the requests one server in a redirect test sees.
type s3RedirectHop struct {
	mu     sync.Mutex
	hits   int
	header http.Header
}

func (h *s3RedirectHop) record(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hits++
	h.header = r.Header.Clone()
}

func (h *s3RedirectHop) snapshot() (int, http.Header) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits, h.header
}

// hasSigV4 reports whether hdr carries any of the headers the signer adds.
func hasSigV4(hdr http.Header) []string {
	var found []string
	for k := range hdr {
		if strings.EqualFold(k, "Authorization") || strings.HasPrefix(strings.ToLower(k), "x-amz-") {
			found = append(found, k)
		}
	}
	return found
}

// ensureS3ModelVia runs ensureModel for an s3:// source whose secret points
// AWS_ENDPOINT_URL at endpoint, and returns the downloaded bytes.
func ensureS3ModelVia(t *testing.T, endpoint string) (string, error) {
	t.Helper()
	tmpDir := t.TempDir()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-models", Namespace: "default"},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("AKIAEXAMPLE0000000"),
			"AWS_SECRET_ACCESS_KEY": []byte("secretaccesskeyvalue0000000000000"),
			"AWS_REGION":            []byte("us-east-1"),
			"AWS_ENDPOINT_URL":      []byte(endpoint),
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger(),
		WithKubeClient("default", k8sClient, nil), allowTestServers())
	path, err := executor.ensureModel(t.Context(), "s3://models/org/repo/model-Q4_K_M.gguf", "s3-model",
		&corev1.LocalObjectReference{Name: "minio-models"}, "")
	if err != nil {
		return "", err
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read downloaded model: %v", err)
	}
	return string(got), nil
}

// An s3 endpoint that 307-redirects the object GET to another host (here a
// second server on another port, as a presigned-URL redirect would) must not
// hand that host a SigV4 signature: the redirected request carries neither
// Authorization nor any x-amz-* header, and the download still completes
// from the second server (#1955).
func TestEnsureModel_S3CrossHostRedirectIsNotSigned(t *testing.T) {
	var first, second s3RedirectHop
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second.record(r)
		_, _ = w.Write([]byte("presigned-bytes"))
	}))
	defer other.Close()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first.record(r)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		http.Redirect(w, r, other.URL+"/presigned/model.gguf?X-Amz-Signature=abc", http.StatusTemporaryRedirect)
	}))
	defer endpoint.Close()

	body, err := ensureS3ModelVia(t, endpoint.URL)
	if err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	if body != "presigned-bytes" {
		t.Errorf("downloaded body = %q, want the second server's bytes", body)
	}
	if hits, _ := first.snapshot(); hits == 0 {
		t.Fatal("the endpoint was never asked")
	}
	hits, hdr := second.snapshot()
	if hits == 0 {
		t.Fatal("the redirect target was never asked")
	}
	if found := hasSigV4(hdr); len(found) > 0 {
		t.Errorf("cross-host redirect hop carried signing headers %v: %v", found, hdr)
	}
}

// A redirect to another path on the configured endpoint is still signed.
func TestEnsureModel_S3SameHostRedirectIsSigned(t *testing.T) {
	var redirected s3RedirectHop
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE0000000/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path != "/moved/model.gguf" {
			http.Redirect(w, r, "/moved/model.gguf", http.StatusTemporaryRedirect)
			return
		}
		redirected.record(r)
		_, _ = w.Write([]byte("moved-bytes"))
	}))
	defer srv.Close()

	body, err := ensureS3ModelVia(t, srv.URL)
	if err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	if body != "moved-bytes" {
		t.Errorf("downloaded body = %q, want %q", body, "moved-bytes")
	}
	if hits, hdr := redirected.snapshot(); hits == 0 || hdr.Get("x-amz-date") == "" {
		t.Errorf("same-host redirect hop was not signed: hits=%d header=%v", hits, hdr)
	}
}

// The signer passes a request to any other origin through untouched, and does
// not write its headers into the caller's request (the client builds each
// redirect hop from those headers).
func TestSigV4RoundTripperSignsOnlyItsEndpoint(t *testing.T) {
	var got http.Header
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	signer := &sigv4RoundTripper{
		base:      base,
		endpoint:  &url.URL{Scheme: "https", Host: "MinIO.lan"},
		accessKey: "AKIAEXAMPLE",
		secretKey: "secret",
		region:    "us-east-1",
	}
	for _, tc := range []struct {
		url  string
		sign bool
	}{
		{"https://minio.lan:443/models/m.gguf", true},
		{"https://minio.lan:9000/models/m.gguf", false},
		{"http://minio.lan/models/m.gguf", false},
		{"https://elsewhere.example/models/m.gguf?X-Amz-Signature=x", false},
	} {
		req, err := http.NewRequest(http.MethodGet, tc.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := signer.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip(%s): %v", tc.url, err)
		}
		_ = resp.Body.Close()
		if signed := got.Get("Authorization") != ""; signed != tc.sign {
			t.Errorf("%s: signed = %v, want %v", tc.url, signed, tc.sign)
		}
		if !tc.sign && len(hasSigV4(got)) > 0 {
			t.Errorf("%s: unsigned request carried %v", tc.url, hasSigV4(got))
		}
		if len(hasSigV4(req.Header)) > 0 {
			t.Errorf("%s: signer wrote %v into the caller's request", tc.url, hasSigV4(req.Header))
		}
	}
}
