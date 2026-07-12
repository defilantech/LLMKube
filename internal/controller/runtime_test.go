package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
		{"idle when sum is 0", "sglang:num_requests_running{m=\"a\"} 0\n", 200, false, true},
		{"busy when sum > 0", "sglang:num_requests_running 3\n", 200, false, false},
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
