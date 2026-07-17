package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// containsArg reports whether args contains the given flag. When value is
// non-empty, it also requires the immediately following entry to equal value
// (i.e. `--flag value` as separate slice elements, which is how BuildArgs
// emits everything).
func containsArg(args []string, flag, value string) bool {
	for i, a := range args {
		if a != flag {
			continue
		}
		if value == "" {
			return true
		}
		if i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// ptrString, ptrBool, ptrInt32 are local helpers so tests read naturally.
func ptrBool(b bool) *bool          { return &b }
func ptrFloat64(f float64) *float64 { return &f }
func ptrInt32(i int32) *int32       { return &i }
func ptrString(s string) *string    { return &s }

type FlagCheck struct {
	flag  string
	value string
}

func TestRuntimeNameLabel(t *testing.T) {
	cases := []struct {
		name     string
		runtime  string
		expected string
	}{
		{name: "empty runtime defaults to llamacpp", runtime: "", expected: "llamacpp"},
		{name: "vllm passes through", runtime: "vllm", expected: "vllm"},
		{name: "tgi passes through", runtime: "tgi", expected: "tgi"},
		{name: "personaplex passes through", runtime: "personaplex", expected: "personaplex"},
		{name: "generic passes through", runtime: "generic", expected: "generic"},
		{name: "llamacpp-router passes through", runtime: "llamacpp-router", expected: "llamacpp-router"},
		// Future runtimes (vllm-swift on metal, etc.) pass through
		// untouched: the label is the user-declared identifier, not a
		// validated enum, so new backends do not need to update this map.
		{name: "unknown runtime passes through verbatim", runtime: "future-thing", expected: "future-thing"},
		{name: "sglang passes through", runtime: "sglang", expected: "sglang"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: tc.runtime},
			}
			if got := runtimeNameLabel(isvc); got != tc.expected {
				t.Errorf("runtimeNameLabel(%q) = %q, want %q", tc.runtime, got, tc.expected)
			}
		})
	}

	t.Run("nil isvc returns llamacpp", func(t *testing.T) {
		if got := runtimeNameLabel(nil); got != "llamacpp" {
			t.Errorf("runtimeNameLabel(nil) = %q, want %q", got, "llamacpp")
		}
	})
}

func TestResolveGPUCount(t *testing.T) {
	cases := []struct {
		expected int32
		isvc     *inferencev1alpha1.InferenceService
		model    *inferencev1alpha1.Model
		name     string
	}{
		{
			expected: 1,
			isvc:     &inferencev1alpha1.InferenceService{},
			model: &inferencev1alpha1.Model{
				Spec: inferencev1alpha1.ModelSpec{
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count: 1,
						},
					},
				},
			},
			name: "model.Spec.Hardware.GPU.Count set resolve GPU count",
		},
		{
			expected: 1,
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			},
			model: &inferencev1alpha1.Model{},
			name:  "isvc.Spec.Resources.GPU set resolve GPU count",
		},
		{
			expected: 1,
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 2,
					},
				},
			},
			model: &inferencev1alpha1.Model{
				Spec: inferencev1alpha1.ModelSpec{
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count: 1,
						},
					},
				},
			},
			name: "model.Spec.Hardware.GPU.Count have precedence over isvc.Spec.Resources.GPU",
		},
		{
			expected: 0,
			isvc:     &inferencev1alpha1.InferenceService{},
			model:    &inferencev1alpha1.Model{},
			name:     "no GPU set resolve to 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actual := resolveGPUCount(tc.isvc, tc.model)
			if actual != tc.expected {
				t.Errorf("expected %d GPU count, got: %d", tc.expected, actual)
			}
		})
	}
}

func TestResolveBackend_SGLang(t *testing.T) {
	isvc := &inferencev1alpha1.InferenceService{
		Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "sglang"},
	}
	backend := resolveBackend(isvc)
	if _, ok := backend.(*SGLangBackend); !ok {
		t.Errorf("resolveBackend(sglang) = %T, want *SGLangBackend", backend)
	}
}

func TestResolveBackend_LlamaCppRouter(t *testing.T) {
	isvc := &inferencev1alpha1.InferenceService{
		Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "llamacpp-router"},
	}
	backend := resolveBackend(isvc)
	if _, ok := backend.(*LlamaCppRouterBackend); !ok {
		t.Errorf("resolveBackend(llamacpp-router) = %T, want *LlamaCppRouterBackend", backend)
	}
}

func TestVLLMIdleProbe(t *testing.T) {
	backend := &VLLMBackend{}
	client := &http.Client{Timeout: 5 * time.Second}
	cases := []struct {
		name     string
		body     string
		status   int
		wantErr  bool
		wantIdle bool
	}{
		{"idle when sum is 0", "vllm:num_requests_running{m=\"a\"} 0\n", 200, false, true},
		{"busy when sum > 0", "vllm:num_requests_running 3\n", 200, false, false},
		{"busy when absent", "other_metric 5\n", 200, false, false},
		{"error on non-200", "", 500, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			probe := backend.IdleProbe(nil, client)
			idle, err := probe(context.Background(), server.URL)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
			if idle != tc.wantIdle {
				t.Errorf("idle = %v, want %v", idle, tc.wantIdle)
			}
		})
	}
}

func TestTGIIdleProbe(t *testing.T) {
	backend := &TGIBackend{}
	client := &http.Client{Timeout: 5 * time.Second}
	cases := []struct {
		name     string
		body     string
		status   int
		wantErr  bool
		wantIdle bool
	}{
		{"idle when value is 0", "tgi_batch_current_size 0\n", 200, false, true},
		{"busy when value > 0", "tgi_batch_current_size 3\n", 200, false, false},
		{"busy when absent", "other_metric 5\n", 200, false, false},
		{"error on non-200", "", 500, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			probe := backend.IdleProbe(nil, client)
			idle, err := probe(context.Background(), server.URL)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
			if idle != tc.wantIdle {
				t.Errorf("idle = %v, want %v", idle, tc.wantIdle)
			}
		})
	}
}

func TestSGLangIdleProbe(t *testing.T) {
	backend := &SGLangBackend{}
	client := &http.Client{Timeout: 5 * time.Second}
	cases := []struct {
		name     string
		body     string
		status   int
		wantErr  bool
		wantIdle bool
	}{
		{"idle when sum is 0", "sglang:num_running_reqs{model_name=\"llama\"} 0\n", 200, false, true},
		{"busy when sum > 0", "sglang:num_running_reqs{model_name=\"llama\"} 3\n", 200, false, false},
		{"busy when absent", "other_metric 5\n", 200, false, false},
		{"error on non-200", "", 500, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			probe := backend.IdleProbe(nil, client)
			idle, err := probe(context.Background(), server.URL)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
			if idle != tc.wantIdle {
				t.Errorf("idle = %v, want %v", idle, tc.wantIdle)
			}
		})
	}
}

func TestGenericIdleProbe(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	cases := []struct {
		name            string
		isvc            *inferencev1alpha1.InferenceService
		status          int
		wantErr         bool
		wantUnsupported bool
		wantIdle        bool
	}{
		{
			name: "idle on 200 with annotation",
			isvc: &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						inferencev1alpha1.AnnotationIdleEndpoint: "/health",
					},
				},
			},
			status: 200, wantErr: false, wantIdle: true,
		},
		{
			name: "busy on 404 with annotation",
			isvc: &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						inferencev1alpha1.AnnotationIdleEndpoint: "/health",
					},
				},
			},
			status: 404, wantErr: false, wantIdle: false,
		},
		{
			name:    "unsupported when annotation absent",
			isvc:    &inferencev1alpha1.InferenceService{},
			wantErr: true, wantUnsupported: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &GenericBackend{}
			var server *httptest.Server
			if tc.status != 0 {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
				}))
				defer server.Close()
			}

			probe := backend.IdleProbe(tc.isvc, client)
			var idle bool
			var err error
			if server != nil {
				idle, err = probe(context.Background(), server.URL)
			} else {
				idle, err = probe(context.Background(), "http://example.com")
			}

			if tc.wantUnsupported {
				if !errors.Is(err, errIdleUnsupported) {
					t.Errorf("expected errIdleUnsupported, got: %v", err)
				}
			} else if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
			if idle != tc.wantIdle {
				t.Errorf("idle = %v, want %v", idle, tc.wantIdle)
			}
		})
	}
}

func TestIdleDetectorConformance(t *testing.T) {
	backends := []struct {
		name    string
		backend RuntimeBackend
	}{
		{"llamacpp", &LlamaCppBackend{}},
		{"llamacpp-router", &LlamaCppRouterBackend{}},
		{"vllm", &VLLMBackend{}},
		{"tgi", &TGIBackend{}},
		{"sglang", &SGLangBackend{}},
		{"generic", &GenericBackend{}},
	}
	for _, tc := range backends {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.backend.(IdleDetector); !ok {
				t.Errorf("%T does not implement IdleDetector", tc.backend)
			}
		})
	}

	t.Run("personaplex must NOT implement IdleDetector", func(t *testing.T) {
		if reflect.TypeOf(&PersonaPlexBackend{}).Implements(reflect.TypeOf((*IdleDetector)(nil)).Elem()) {
			t.Error("PersonaPlexBackend must NOT implement IdleDetector")
		}
	})
}

// errorRoundTripper always returns a transport error.
type errorRoundTripper struct{}

func (e *errorRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("simulated transport failure")
}

// errorReadCloser implements io.ReadCloser that always fails on Read.
type errorReadCloser struct{}

func (e *errorReadCloser) Read(_ []byte) (int, error) {
	return 0, fmt.Errorf("simulated read failure")
}

func (e *errorReadCloser) Close() error { return nil }

// errorBodyRoundTripper returns a 200 response whose Body fails on Read.
type errorBodyRoundTripper struct{}

func (e *errorBodyRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(&errorReadCloser{}),
	}, nil
}

// malformedJSONRoundTripper returns a 200 response with non-JSON body.
type malformedJSONRoundTripper struct{}

func (m *malformedJSONRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("not json")),
	}, nil
}

func TestVLLMIdleProbeTransportError(t *testing.T) {
	backend := &VLLMBackend{}
	client := &http.Client{
		Transport: &errorRoundTripper{},
		Timeout:   5 * time.Second,
	}
	probe := backend.IdleProbe(nil, client)
	idle, err := probe(context.Background(), "http://10.0.0.1:8000")
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if idle {
		t.Errorf("expected idle=false on error")
	}
}

func TestVLLMIdleProbeBodyReadError(t *testing.T) {
	backend := &VLLMBackend{}
	client := &http.Client{
		Transport: &errorBodyRoundTripper{},
		Timeout:   5 * time.Second,
	}
	probe := backend.IdleProbe(nil, client)
	idle, err := probe(context.Background(), "http://10.0.0.1:8000")
	if err == nil {
		t.Fatal("expected body read error, got nil")
	}
	if idle {
		t.Errorf("expected idle=false on error")
	}
}

func TestTGIIdleProbeTransportError(t *testing.T) {
	backend := &TGIBackend{}
	client := &http.Client{
		Transport: &errorRoundTripper{},
		Timeout:   5 * time.Second,
	}
	probe := backend.IdleProbe(nil, client)
	idle, err := probe(context.Background(), "http://10.0.0.1:8000")
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if idle {
		t.Errorf("expected idle=false on error")
	}
}

func TestTGIIdleProbeBodyReadError(t *testing.T) {
	backend := &TGIBackend{}
	client := &http.Client{
		Transport: &errorBodyRoundTripper{},
		Timeout:   5 * time.Second,
	}
	probe := backend.IdleProbe(nil, client)
	idle, err := probe(context.Background(), "http://10.0.0.1:8000")
	if err == nil {
		t.Fatal("expected body read error, got nil")
	}
	if idle {
		t.Errorf("expected idle=false on error")
	}
}

func TestSGLangIdleProbeTransportError(t *testing.T) {
	backend := &SGLangBackend{}
	client := &http.Client{
		Transport: &errorRoundTripper{},
		Timeout:   5 * time.Second,
	}
	probe := backend.IdleProbe(nil, client)
	idle, err := probe(context.Background(), "http://10.0.0.1:8000")
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if idle {
		t.Errorf("expected idle=false on error")
	}
}

func TestSGLangIdleProbeBodyReadError(t *testing.T) {
	backend := &SGLangBackend{}
	client := &http.Client{
		Transport: &errorBodyRoundTripper{},
		Timeout:   5 * time.Second,
	}
	probe := backend.IdleProbe(nil, client)
	idle, err := probe(context.Background(), "http://10.0.0.1:8000")
	if err == nil {
		t.Fatal("expected body read error, got nil")
	}
	if idle {
		t.Errorf("expected idle=false on error")
	}
}

func TestLlamaCppIdleProbeTransportError(t *testing.T) {
	backend := &LlamaCppBackend{}
	client := &http.Client{
		Transport: &errorRoundTripper{},
		Timeout:   5 * time.Second,
	}
	probe := backend.IdleProbe(nil, client)
	idle, err := probe(context.Background(), "http://10.0.0.1:8000")
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if idle {
		t.Errorf("expected idle=false on error")
	}
}

func TestLlamaCppIdleProbeBodyReadError(t *testing.T) {
	backend := &LlamaCppBackend{}
	client := &http.Client{
		Transport: &errorBodyRoundTripper{},
		Timeout:   5 * time.Second,
	}
	probe := backend.IdleProbe(nil, client)
	idle, err := probe(context.Background(), "http://10.0.0.1:8000")
	if err == nil {
		t.Fatal("expected body read error, got nil")
	}
	if idle {
		t.Errorf("expected idle=false on error")
	}
}

func TestLlamaCppIdleProbeJSONUnmarshalError(t *testing.T) {
	backend := &LlamaCppBackend{}
	client := &http.Client{
		Transport: &malformedJSONRoundTripper{},
		Timeout:   5 * time.Second,
	}
	probe := backend.IdleProbe(nil, client)
	idle, err := probe(context.Background(), "http://10.0.0.1:8000")
	if err == nil {
		t.Fatal("expected JSON unmarshal error, got nil")
	}
	if idle {
		t.Errorf("expected idle=false on error")
	}
}

func TestGenericIdleProbeTransportError(t *testing.T) {
	backend := &GenericBackend{}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				inferencev1alpha1.AnnotationIdleEndpoint: "/health",
			},
		},
	}
	client := &http.Client{
		Transport: &errorRoundTripper{},
		Timeout:   5 * time.Second,
	}
	probe := backend.IdleProbe(isvc, client)
	idle, err := probe(context.Background(), "http://10.0.0.1:8080")
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if idle {
		t.Errorf("expected idle=false on error")
	}
}

// TestIdleProbeRequestCreationError exercises the defensive error-handling
// branch after http.NewRequestWithContext in each backend's IdleProbe. A URL
// with a null byte causes request construction to fail.
func TestIdleProbeRequestCreationError(t *testing.T) {
	cases := []struct {
		name    string
		backend IdleDetector
		isvc    *inferencev1alpha1.InferenceService
	}{
		{"llamacpp", &LlamaCppBackend{}, nil},
		{"llamacpp-router", &LlamaCppRouterBackend{}, nil},
		{"vllm", &VLLMBackend{}, nil},
		{"tgi", &TGIBackend{}, nil},
		{"sglang", &SGLangBackend{}, nil},
		{"generic", &GenericBackend{}, &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					inferencev1alpha1.AnnotationIdleEndpoint: "/health",
				},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := tc.backend.IdleProbe(tc.isvc, &http.Client{Timeout: 5 * time.Second})
			idle, err := probe(context.Background(), "http://\x00bad")
			if err == nil {
				t.Fatal("expected request creation error, got nil")
			}
			if idle {
				t.Errorf("expected idle=false on error")
			}
		})
	}
}

func TestLlamaCppRouterBackend_BuildArgs(t *testing.T) {
	backend := &LlamaCppRouterBackend{}
	isvc := &inferencev1alpha1.InferenceService{
		Spec: inferencev1alpha1.InferenceServiceSpec{},
	}
	model := &inferencev1alpha1.Model{}
	args := backend.BuildArgs(isvc, model, "", 8080)

	// Router mode should use --models-dir instead of --model
	if !containsArg(args, "--models-dir", "/models") {
		t.Error("BuildArgs should include --models-dir /models")
	}

	// Should include standard host and port flags
	if !containsArg(args, "--host", "::") {
		t.Error("BuildArgs should include --host ::")
	}
	if !containsArg(args, "--port", "8080") {
		t.Error("BuildArgs should include --port 8080")
	}

	// Should include metrics flag
	if !containsArg(args, "--metrics", "") {
		t.Error("BuildArgs should include --metrics")
	}

	// Should NOT include --model flag (router mode uses --models-dir)
	if containsArg(args, "--model", "") {
		t.Error("BuildArgs should NOT include --model in router mode")
	}
}

func TestLlamaCppRouterBackend_NeedsModelInit(t *testing.T) {
	backend := &LlamaCppRouterBackend{}
	if backend.NeedsModelInit() {
		t.Error("LlamaCppRouterBackend should not need model init container")
	}
}

func TestLlamaCppRouterBackend_Defaults(t *testing.T) {
	backend := &LlamaCppRouterBackend{}

	if backend.ContainerName() != "llama-server" {
		t.Errorf("ContainerName = %q, want \"llama-server\"", backend.ContainerName())
	}

	if backend.DefaultImage() != "ghcr.io/ggml-org/llama.cpp:server" {
		t.Errorf("DefaultImage = %q, want \"ghcr.io/ggml-org/llama.cpp:server\"", backend.DefaultImage())
	}

	if backend.DefaultPort() != 8080 {
		t.Errorf("DefaultPort = %d, want 8080", backend.DefaultPort())
	}

	if backend.DefaultHPAMetric() != "llamacpp:requests_processing" {
		t.Errorf("DefaultHPAMetric = %q, want \"llamacpp:requests_processing\"", backend.DefaultHPAMetric())
	}
}

func TestLlamaCppRouterBackend_BuildProbes(t *testing.T) {
	backend := &LlamaCppRouterBackend{}
	startup, liveness, readiness := backend.BuildProbes(8080)

	// Verify startup probe
	if startup == nil {
		t.Fatal("startup probe should not be nil")
	}
	if startup.ProbeHandler.HTTPGet == nil {
		t.Fatal("startup probe HTTPGet should not be nil")
	}
	if startup.ProbeHandler.HTTPGet.Path != "/health" {
		t.Errorf("startup probe path = %q, want \"/health\"", startup.ProbeHandler.HTTPGet.Path)
	}
	if startup.PeriodSeconds != 10 {
		t.Errorf("startup probe period = %d, want 10", startup.PeriodSeconds)
	}
	if startup.FailureThreshold != 180 {
		t.Errorf("startup probe failure threshold = %d, want 180", startup.FailureThreshold)
	}

	// Verify liveness probe
	if liveness == nil {
		t.Fatal("liveness probe should not be nil")
	}
	if liveness.ProbeHandler.HTTPGet == nil {
		t.Fatal("liveness probe HTTPGet should not be nil")
	}
	if liveness.ProbeHandler.HTTPGet.Path != "/health" {
		t.Errorf("liveness probe path = %q, want \"/health\"", liveness.ProbeHandler.HTTPGet.Path)
	}
	if liveness.PeriodSeconds != 15 {
		t.Errorf("liveness probe period = %d, want 15", liveness.PeriodSeconds)
	}
	if liveness.FailureThreshold != 3 {
		t.Errorf("liveness probe failure threshold = %d, want 3", liveness.FailureThreshold)
	}

	// Verify readiness probe
	if readiness == nil {
		t.Fatal("readiness probe should not be nil")
	}
	if readiness.ProbeHandler.HTTPGet == nil {
		t.Fatal("readiness probe HTTPGet should not be nil")
	}
	if readiness.ProbeHandler.HTTPGet.Path != "/health" {
		t.Errorf("readiness probe path = %q, want \"/health\"", readiness.ProbeHandler.HTTPGet.Path)
	}
	if readiness.PeriodSeconds != 10 {
		t.Errorf("readiness probe period = %d, want 10", readiness.PeriodSeconds)
	}
	if readiness.FailureThreshold != 3 {
		t.Errorf("readiness probe failure threshold = %d, want 3", readiness.FailureThreshold)
	}
}
