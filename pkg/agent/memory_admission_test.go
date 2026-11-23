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
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/safehttp"
)

func newAdmissionTestModel(source, statusSize string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.ModelSpec{
			Source: source,
		},
		Status: inferencev1alpha1.ModelStatus{
			Size: statusSize,
		},
	}
}

func newAdmissionTestISVC() *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-isvc",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "test-model",
		},
	}
}

// newAdmissionTestAgent builds a MetalAgent whose fake client already holds
// the given InferenceService (with the status subresource enabled so
// Status().Update works) and whose model store is an empty temp dir.
func newAdmissionTestAgent(t *testing.T, isvc *inferencev1alpha1.InferenceService, cfg MetalAgentConfig) *MetalAgent {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(newTestScheme())
	if isvc != nil {
		builder = builder.
			WithObjects(isvc).
			WithStatusSubresource(isvc)
	}
	cfg.K8sClient = builder.Build()
	if cfg.ModelStorePath == "" {
		cfg.ModelStorePath = t.TempDir()
	}
	return NewMetalAgent(cfg)
}

func TestRemoteModelSize_UsesContentLength(t *testing.T) {
	const wantSize = 14_000_000_000
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD request, got %s", r.Method)
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", wantSize))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	size, err := remoteModelSize(context.Background(), testProbeClient(), ts.URL+"/model.gguf")
	if err != nil {
		t.Fatalf("remoteModelSize returned error: %v", err)
	}
	if size != wantSize {
		t.Errorf("size = %d, want %d", size, wantSize)
	}
}

func TestRemoteModelSize_NonOKStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	if _, err := remoteModelSize(context.Background(), testProbeClient(), ts.URL+"/gated.gguf"); err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
}

func TestRemoteModelSize_MissingContentLength(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Chunked response: no Content-Length header.
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	if _, err := remoteModelSize(context.Background(), testProbeClient(), ts.URL+"/model.gguf"); err == nil {
		t.Fatal("expected error for missing Content-Length, got nil")
	}
}

// The crash scenario from 2026-06-09: model not on disk yet (fresh boot wiped
// /tmp) and Model status.size is the literal string "0". The estimate must
// fall back to a HEAD probe of the source instead of erroring out (which the
// old call site then treated as "proceed without check").
func TestEstimateModelMemory_RemoteHEADFallback(t *testing.T) {
	const wantSize = uint64(20_000_000_000)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", wantSize))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{AllowedDownloadHosts: []string{"127.0.0.1"}})
	model := newAdmissionTestModel(ts.URL+"/model.gguf", "0")

	estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "", 0)
	if err != nil {
		t.Fatalf("estimateModelMemory returned error: %v", err)
	}
	if estimate.WeightsBytes != wantSize {
		t.Errorf("WeightsBytes = %d, want %d", estimate.WeightsBytes, wantSize)
	}
}

// The HEAD size probe is a Model-source fetch like the download itself, so it
// dials through the same SSRF guard: a loopback source is never contacted
// unless --allowed-download-hosts names it, and the failure says so.
func TestEstimateModelMemory_RemoteHEADRefusesBlockedSource(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "20000000000")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel(ts.URL+"/model.gguf", "0")

	_, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "", 0)
	assertGuardRefusal(t, err, "127.0.0.1")
	if n := hits.Load(); n != 0 {
		t.Errorf("blocked source received %d HEAD requests, want 0", n)
	}
}

func TestEstimateModelMemory_StatusSizePreferredOverRemote(t *testing.T) {
	// Valid status size: must be used without any HTTP traffic (source is
	// unreachable on purpose).
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "20.0 GiB")

	estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "", 0)
	if err != nil {
		t.Fatalf("estimateModelMemory returned error: %v", err)
	}
	want := uint64(20 * 1024 * 1024 * 1024)
	if estimate.WeightsBytes != want {
		t.Errorf("WeightsBytes = %d, want %d", estimate.WeightsBytes, want)
	}
}

func TestEstimateModelMemory_AllSourcesExhausted(t *testing.T) {
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "0")

	_, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "", 0)
	if err == nil {
		t.Fatal("expected error when no size source is available, got nil")
	}
	if !strings.Contains(err.Error(), "cannot determine model size") {
		t.Errorf("error %q should mention being unable to determine model size", err)
	}
}

// Fail closed: when the model size cannot be determined at all, admission
// must reject the service instead of starting an unsized llama-server.
func TestCheckMemoryAdmission_FailsClosedWhenSizeUnknown(t *testing.T) {
	isvc := newAdmissionTestISVC()
	agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 128 * 1024 * 1024 * 1024},
	})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "0")

	err := agent.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", "")
	if err == nil {
		t.Fatal("expected admission to fail closed, got nil error")
	}

	updated := &inferencev1alpha1.InferenceService{}
	if getErr := agent.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "test-isvc"}, updated); getErr != nil {
		t.Fatalf("failed to re-fetch InferenceService: %v", getErr)
	}
	if updated.Status.SchedulingStatus != "MemoryCheckFailed" {
		t.Errorf("SchedulingStatus = %q, want %q", updated.Status.SchedulingStatus, "MemoryCheckFailed")
	}
	if updated.Status.SchedulingMessage == "" {
		t.Error("SchedulingMessage should explain why admission failed")
	}
}

func TestCheckMemoryAdmission_WarnModeProceeds(t *testing.T) {
	isvc := newAdmissionTestISVC()
	agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
		MemoryProvider:  &mockMemoryProvider{totalBytes: 128 * 1024 * 1024 * 1024},
		MemoryCheckMode: MemoryCheckModeWarn,
	})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "0")

	if err := agent.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err != nil {
		t.Fatalf("warn mode should preserve the legacy proceed-without-check behavior, got: %v", err)
	}
}

// TestCheckMemoryAdmission_GQAShapeAdmits pins the end-to-end admission effect
// of the grouped-query-attention KV estimate (#2085): a Model whose GGUF status
// carries the attention shape fits a budget that the embedding-width estimate
// refuses, and loses that margin the moment the fields are absent from status.
func TestCheckMemoryAdmission_GQAShapeAdmits(t *testing.T) {
	// 15 GiB of weights: with GQA accounting the 131072-context KV is 16 GiB
	// (total 31.5 GiB, under the 36 GiB budget); the embedding-width KV is
	// 64 GiB (total 79.5 GiB, over it).
	isvc := newAdmissionTestISVC()
	agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 48 * 1024 * 1024 * 1024},
		MemoryFraction: 0.75,
	})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "15.0 GiB")
	model.Status.GGUF = &inferencev1alpha1.GGUFMetadata{
		LayerCount:    32,
		EmbeddingSize: 4096,
		HeadCount:     32,
		HeadCountKV:   8,
		KeyLength:     128,
		ValueLength:   128,
	}

	if err := agent.checkMemoryAdmission(context.Background(), isvc, model, 131072, "", ""); err != nil {
		t.Fatalf("a GQA-shaped model must be admitted at 131072 context, got: %v", err)
	}

	// The same weights and context without the attention shape in status:
	// the embedding-width estimate refuses.
	isvcRefused := newAdmissionTestISVC()
	agentRefused := newAdmissionTestAgent(t, isvcRefused, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 48 * 1024 * 1024 * 1024},
		MemoryFraction: 0.75,
	})
	modelRefused := newAdmissionTestModel("https://model-host.invalid/model.gguf", "15.0 GiB")
	modelRefused.Status.GGUF = &inferencev1alpha1.GGUFMetadata{
		LayerCount:    32,
		EmbeddingSize: 4096,
		HeadCount:     32,
	}

	err := agentRefused.checkMemoryAdmission(context.Background(), isvcRefused, modelRefused, 131072, "", "")
	if err == nil {
		t.Fatal("the same model without GQA metadata must still be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "insufficient memory") {
		t.Errorf("error %q should mention insufficient memory", err)
	}

	// The tightening direction: a Gemma-shaped model (16 heads x 256 dims
	// against a 3072 embedding) that the embedding-width estimate admits at
	// this budget is refused once the attention shape is known, because the
	// attention-shape charge is the larger, correct one.
	isvcGemma := newAdmissionTestISVC()
	agentGemma := newAdmissionTestAgent(t, isvcGemma, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 48 * 1024 * 1024 * 1024},
		MemoryFraction: 0.75,
	})
	// 8 GiB of weights at 65536 context: legacy KV is 21 GiB (total 29.5 GiB,
	// admitted); the attention-shape KV is 28 GiB (total 36.5 GiB, refused).
	gemmaLegacy := newAdmissionTestModel("https://model-host.invalid/model.gguf", "8.0 GiB")
	gemmaLegacy.Status.GGUF = &inferencev1alpha1.GGUFMetadata{
		LayerCount:    28,
		EmbeddingSize: 3072,
		HeadCount:     16,
	}
	if err := agentGemma.checkMemoryAdmission(context.Background(), isvcGemma, gemmaLegacy, 65536, "", ""); err != nil {
		t.Fatalf("a head-dim model without attention metadata must be admitted on the legacy estimate, got: %v", err)
	}

	isvcGemmaShaped := newAdmissionTestISVC()
	agentGemmaShaped := newAdmissionTestAgent(t, isvcGemmaShaped, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 48 * 1024 * 1024 * 1024},
		MemoryFraction: 0.75,
	})
	gemmaShaped := newAdmissionTestModel("https://model-host.invalid/model.gguf", "8.0 GiB")
	gemmaShaped.Status.GGUF = &inferencev1alpha1.GGUFMetadata{
		LayerCount:    28,
		EmbeddingSize: 3072,
		HeadCount:     16,
		HeadCountKV:   16,
		KeyLength:     256,
		ValueLength:   256,
	}
	err = agentGemmaShaped.checkMemoryAdmission(context.Background(), isvcGemmaShaped, gemmaShaped, 65536, "", "")
	if err == nil {
		t.Fatal("the same model with its attention shape must be refused at this budget, got nil error")
	}
	if !strings.Contains(err.Error(), "insufficient memory") {
		t.Errorf("error %q should mention insufficient memory", err)
	}
}

func TestCheckMemoryAdmission_RejectsOverBudget(t *testing.T) {
	isvc := newAdmissionTestISVC()
	agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 8 * 1024 * 1024 * 1024},
		MemoryFraction: 0.75,
	})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "100.0 GiB")

	err := agent.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", "")
	if err == nil {
		t.Fatal("expected insufficient-memory rejection, got nil error")
	}
	if !strings.Contains(err.Error(), "insufficient memory") {
		t.Errorf("error %q should mention insufficient memory", err)
	}

	updated := &inferencev1alpha1.InferenceService{}
	if getErr := agent.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "test-isvc"}, updated); getErr != nil {
		t.Fatalf("failed to re-fetch InferenceService: %v", getErr)
	}
	if updated.Status.SchedulingStatus != "InsufficientMemory" {
		t.Errorf("SchedulingStatus = %q, want %q", updated.Status.SchedulingStatus, "InsufficientMemory")
	}
}

func TestCheckMemoryAdmission_PassesWithinBudget(t *testing.T) {
	isvc := newAdmissionTestISVC()
	agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 128 * 1024 * 1024 * 1024},
		MemoryFraction: 0.75,
	})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "20.0 GiB")

	if err := agent.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err != nil {
		t.Fatalf("model within budget should pass admission, got: %v", err)
	}
}

// Regression test for #777: when a previous memory check failed and set
// SchedulingStatus/Message, a subsequent passing check must clear those
// fields so the status reflects the current healthy state.
func TestCheckMemoryAdmission_ClearsStaleSchedulingStatus(t *testing.T) {
	isvc := newAdmissionTestISVC()
	isvc.Status.SchedulingStatus = "InsufficientMemory"
	isvc.Status.SchedulingMessage = "estimated 100 GiB required, budget 6 GiB"

	agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 128 * 1024 * 1024 * 1024},
		MemoryFraction: 0.75,
	})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "20.0 GiB")

	if err := agent.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err != nil {
		t.Fatalf("model within budget should pass admission, got: %v", err)
	}

	updated := &inferencev1alpha1.InferenceService{}
	if getErr := agent.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "test-isvc"}, updated); getErr != nil {
		t.Fatalf("failed to re-fetch InferenceService: %v", getErr)
	}
	if updated.Status.SchedulingStatus != "" {
		t.Errorf("SchedulingStatus = %q, want empty (stale status should be cleared)", updated.Status.SchedulingStatus)
	}
	if updated.Status.SchedulingMessage != "" {
		t.Errorf("SchedulingMessage = %q, want empty (stale message should be cleared)", updated.Status.SchedulingMessage)
	}
}

// testProbeClient is a guarded client that allowlists httptest's loopback
// address, for remoteModelSize tests about response handling.
func testProbeClient() *http.Client {
	return safehttp.NewClient(safehttp.ParseAllowlist([]string{"127.0.0.1"}), 0, downloadHostsFlag)
}

// newS3AdmissionAgent builds a MetalAgent whose fake client holds the s3
// credential Secret (and isvc, with the status subresource, when non-nil).
// allowHosts is the download allowlist; nil refuses the loopback endpoint.
func newS3AdmissionAgent(
	t *testing.T, isvc *inferencev1alpha1.InferenceService, endpoint string, allowHosts []string,
) *MetalAgent {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-models", Namespace: "default", Labels: modelSourceCredentialsLabels()},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("AKIAEXAMPLE0000000"),
			"AWS_SECRET_ACCESS_KEY": []byte("secretaccesskeyvalue0000000000000"),
			"AWS_REGION":            []byte("us-east-1"),
			"AWS_ENDPOINT_URL":      []byte(endpoint),
		},
	}
	builder := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(secret)
	if isvc != nil {
		builder = builder.WithObjects(isvc).WithStatusSubresource(isvc)
	}
	return NewMetalAgent(MetalAgentConfig{
		K8sClient:            builder.Build(),
		Namespace:            "default",
		ModelStorePath:       t.TempDir(),
		AllowedDownloadHosts: allowHosts,
		MemoryProvider:       &mockMemoryProvider{totalBytes: 128 * 1024 * 1024 * 1024},
		MemoryFraction:       0.75,
	})
}

// newS3AdmissionModel is an unsized s3:// Model whose sourceSecretRef names
// the credential Secret newS3AdmissionAgent seeds.
func newS3AdmissionModel(key string) *inferencev1alpha1.Model {
	m := newAdmissionTestModel("s3://models/"+key, "0")
	m.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: "minio-models"}
	return m
}

// s3HeadServer stands in for MinIO: it rejects unsigned requests the way a
// private bucket does and answers a signed HEAD with size in Content-Length.
// It records the method and Authorization header it saw.
func s3HeadServer(t *testing.T, size uint64, status int) (*httptest.Server, *string, *string) {
	t.Helper()
	var method, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		auth = r.Header.Get("Authorization")
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE0000000/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &method, &auth
}

// A Metal Model whose source is s3:// (with a sourceSecretRef) must be sized
// from a SigV4-signed HEAD of the object, so memory admission can decide
// before the download instead of failing with "cannot determine model size".
func TestEstimateModelMemory_S3SizesFromSignedHead(t *testing.T) {
	const wantSize = uint64(12_000_000_000)
	srv, method, auth := s3HeadServer(t, wantSize, http.StatusOK)

	agent := newS3AdmissionAgent(t, nil, srv.URL, []string{"127.0.0.1"})
	model := newS3AdmissionModel("org/repo/model.gguf")

	estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "", 0)
	if err != nil {
		t.Fatalf("estimateModelMemory for an s3:// source returned error: %v", err)
	}
	if estimate.WeightsBytes != wantSize {
		t.Errorf("WeightsBytes = %d, want %d", estimate.WeightsBytes, wantSize)
	}
	if *method != http.MethodHead {
		t.Errorf("probe method = %q, want HEAD", *method)
	}
	if !strings.Contains(*auth, "/us-east-1/s3/aws4_request") {
		t.Errorf("Authorization = %q, want a us-east-1/s3 SigV4 scope", *auth)
	}
}

// The size probe must feed memory admission: an s3:// Model within budget
// passes, using the object's HEAD size.
func TestCheckMemoryAdmission_S3PassesWithHeadSize(t *testing.T) {
	const wantSize = uint64(12_000_000_000)
	srv, _, _ := s3HeadServer(t, wantSize, http.StatusOK)

	isvc := newAdmissionTestISVC()
	agent := newS3AdmissionAgent(t, isvc, srv.URL, []string{"127.0.0.1"})
	model := newS3AdmissionModel("org/repo/model.gguf")

	if err := agent.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err != nil {
		t.Fatalf("s3 model within budget should pass admission, got: %v", err)
	}

	updated := &inferencev1alpha1.InferenceService{}
	if getErr := agent.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "test-isvc"}, updated); getErr != nil {
		t.Fatalf("failed to re-fetch InferenceService: %v", getErr)
	}
	if updated.Status.SchedulingStatus != "" {
		t.Errorf("SchedulingStatus = %q, want empty after a passing s3 admission", updated.Status.SchedulingStatus)
	}
}

// A non-2xx HEAD (for example a missing object) must fail closed.
func TestEstimateModelMemory_S3Non2xxFailsClosed(t *testing.T) {
	srv, _, _ := s3HeadServer(t, 0, http.StatusNotFound)

	agent := newS3AdmissionAgent(t, nil, srv.URL, []string{"127.0.0.1"})
	model := newS3AdmissionModel("org/repo/missing.gguf")

	_, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "", 0)
	if err == nil || !strings.Contains(err.Error(), "cannot determine model size") ||
		!strings.Contains(err.Error(), "remote size probe failed") ||
		!strings.Contains(err.Error(), "404") {
		t.Fatalf("estimateModelMemory for a 404 s3 HEAD = %v, want a fail-closed error naming the probe and 404", err)
	}
}

// A 200 HEAD without a usable Content-Length must fail closed.
func TestEstimateModelMemory_S3MissingContentLengthFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	agent := newS3AdmissionAgent(t, nil, srv.URL, []string{"127.0.0.1"})
	model := newS3AdmissionModel("org/repo/model.gguf")

	_, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "", 0)
	if err == nil || !strings.Contains(err.Error(), "cannot determine model size") ||
		!strings.Contains(err.Error(), "remote size probe failed") ||
		!strings.Contains(err.Error(), "Content-Length") {
		t.Fatalf("estimateModelMemory for an s3 HEAD without Content-Length = %v, "+
			"want a fail-closed error naming the probe", err)
	}
}

// A HEAD the SSRF guard refuses must fail closed and must not reach the
// endpoint: the probe reuses the download path's guarded client.
func TestEstimateModelMemory_S3GuardRefusalFailsClosed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "12000000000")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	agent := newS3AdmissionAgent(t, nil, srv.URL, nil)
	model := newS3AdmissionModel("org/repo/model.gguf")

	_, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "", 0)
	if err == nil || !strings.Contains(err.Error(), "cannot determine model size") ||
		!strings.Contains(err.Error(), "remote size probe failed") ||
		!strings.Contains(err.Error(), downloadHostsFlag) {
		t.Fatalf("estimateModelMemory for a guard-refused s3 endpoint = %v, "+
			"want a fail-closed error naming the SSRF guard", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("guard-refused s3 endpoint received %d HEAD requests, want 0", n)
	}
}

func TestCheckMemoryAdmission_OllamaUsesHostWideMemory(t *testing.T) {
	root := writeProcFixture(t, map[string]string{
		"proc/meminfo":            "MemTotal: 67108864 kB\nMemAvailable: 60000000 kB\n",
		"proc/self/cgroup":        "0::/agent\n",
		"cg/agent/memory.max":     "4294967296\n",
		"cg/agent/memory.current": "0\n",
	})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "20.0 GiB")

	for runtime, wantAdmit := range map[string]bool{runtimeLlamaServer: false, runtimeOllama: true} {
		isvc := newAdmissionTestISVC()
		isvc.Spec.Runtime = runtime
		agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
			MemoryProvider: &LinuxMemoryProvider{
				ProcRoot:   filepath.Join(root, "proc"),
				CgroupRoot: filepath.Join(root, "cg"),
			},
			MemoryFraction: 0.75,
		})
		err := agent.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", "")
		if wantAdmit && err != nil {
			t.Errorf("%s: 20 GiB model on a 64 GiB host = %v; want admitted against host-wide memory", runtime, err)
		}
		if !wantAdmit && err == nil {
			t.Errorf("%s: 20 GiB model under a 4 GiB cgroup limit admitted; want rejected", runtime)
		}
	}
}
