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

package controller

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// newModelReconcilerWithStatus builds a reconciler whose fake client supports
// the status subresource, which reconcileBySourceType needs because every
// terminal path there writes status via r.Status().Update.
func newModelReconcilerWithStatus(t *testing.T, objects ...client.Object) *ModelReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&inferencev1alpha1.Model{}).
		Build()
	return &ModelReconciler{Client: c, Scheme: scheme}
}

// An s3:// Model must be routed to the runtime-resolved path, exactly like a
// remote http(s) source: the InferenceService Pod's init container does the
// fetch with a sigv4-signed curl, using credentials from sourceSecretRef that
// the controller never resolves.
//
// Before this was wired, s3:// matched no case in reconcileBySourceType and
// fell through to the controller-side eager fetch, where net/http rejected the
// scheme outright:
//
//	failed to download: Get "s3://bucket/key": unsupported protocol scheme "s3"
//
// The model never left Downloading, so no Pod was ever created and the
// perfectly good init-container S3 support in model_storage.go never ran.
func TestReconcileBySourceType_S3IsRuntimeResolved(t *testing.T) {
	const source = "s3://models/org/repo/model-Q4_K_M.gguf"

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-model", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          source,
			SourceSecretRef: &corev1.LocalObjectReference{Name: "minio-models"},
		},
	}
	r := newModelReconcilerWithStatus(t, model)

	handled, _, err := r.reconcileBySourceType(context.Background(), model)
	if err != nil {
		t.Fatalf("reconcileBySourceType: %v", err)
	}
	if !handled {
		t.Fatal("s3:// source fell through to the controller-side download path; " +
			"net/http cannot fetch an s3:// URL, so the Model can only fail there")
	}
	if model.Status.Phase != PhaseReady {
		t.Errorf("phase = %q, want %q", model.Status.Phase, PhaseReady)
	}
	// A cache key is what points the storage builder at the shared cache PVC
	// rather than an emptyDir; without it every Pod re-downloads the weights,
	// which is the whole thing an object store exists to avoid.
	if want := computeCacheKey(source); model.Status.CacheKey != want {
		t.Errorf("cacheKey = %q, want %q", model.Status.CacheKey, want)
	}
}

// The controller must not try to fetch an s3:// source itself. This pins the
// classification the dispatch depends on, independent of the switch above.
func TestS3SourceIsNotClassifiedAsFetchableByController(t *testing.T) {
	const source = "s3://models/org/repo/model.gguf"

	if !isS3Source(source) {
		t.Fatalf("isS3Source(%q) = false", source)
	}
	if isLocalSource(source) {
		t.Errorf("isLocalSource(%q) = true; fetchModel would try a filesystem copy", source)
	}
	if isRemoteHTTPSource(source) {
		t.Errorf("isRemoteHTTPSource(%q) = true; the controller would attempt an HTTP GET", source)
	}
}

// The sigv4 signer must attach a valid AWS Signature Version 4 Authorization
// header to every request it forwards, so an s3:// object store accepts the
// controller's metadata read. This exercises sigv4RoundTripper.RoundTrip
// directly (the in-process equivalent of the init container's signed curl) and
// fails if the signer stops producing a signed request.
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
