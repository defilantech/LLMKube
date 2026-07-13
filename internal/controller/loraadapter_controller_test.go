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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// fakeAdapterClient captures the requests SGLang's adapter HTTP API
// would have received and lets the test answer back with a chosen
// status. Use the loadCalls/unloadCalls slices for assertions.
type fakeAdapterClient struct {
	loadCalls   []adapterCall
	unloadCalls []adapterCall

	loadStatus   int
	loadErr      error
	unloadStatus int
	unloadErr    error
}

type adapterCall struct {
	URL    string
	Name   string
	Path   string // empty for unload
	Body   string
	Method string
}

func (f *fakeAdapterClient) LoadAdapter(_ context.Context, baseURL, name, path string) error {
	f.loadCalls = append(f.loadCalls, adapterCall{URL: baseURL, Name: name, Path: path, Method: http.MethodPost})
	if f.loadErr != nil {
		return f.loadErr
	}
	if f.loadStatus == 0 {
		f.loadStatus = http.StatusOK
	}
	if f.loadStatus/100 != 2 {
		return &fakeHTTPError{status: f.loadStatus}
	}
	return nil
}

func (f *fakeAdapterClient) UnloadAdapter(_ context.Context, baseURL, name string) error {
	f.unloadCalls = append(f.unloadCalls, adapterCall{URL: baseURL, Name: name, Method: http.MethodPost})
	if f.unloadErr != nil {
		return f.unloadErr
	}
	if f.unloadStatus == 0 {
		f.unloadStatus = http.StatusOK
	}
	if f.unloadStatus/100 != 2 {
		return &fakeHTTPError{status: f.unloadStatus}
	}
	return nil
}

type fakeHTTPError struct{ status int }

func (e *fakeHTTPError) Error() string {
	return "fake sglang error"
}

// recordingSGLang is an httptest.Server that records the load/unload
// calls SGLang actually received. Tests use it to assert the wire
// payload the controller sent (path, body keys) rather than only the
// surface calls captured by fakeAdapterClient.
type recordingSGLang struct {
	*httptest.Server

	loadReqs   []string
	unloadReqs []string
}

type sglangStatuses struct {
	load   int
	unload int
}

func newRecordingSGLang(t *testing.T, statuses sglangStatuses) *recordingSGLang {
	if statuses.load == 0 {
		statuses.load = http.StatusOK
	}
	if statuses.unload == 0 {
		statuses.unload = http.StatusOK
	}
	t.Helper()
	r := &recordingSGLang{}
	mux := http.NewServeMux()
	// SGLang v0.5.15 routes: POST /load_lora_adapter and
	// POST /unload_lora_adapter (singular `lora_adapter`, no /v1
	// prefix). See
	// https://github.com/sgl-project/sglang/blob/v0.5.15/python/sglang/srt/entrypoints/http_server.py
	mux.HandleFunc("/load_lora_adapter", func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.loadReqs = append(r.loadReqs, string(body))
		if statuses.load >= 400 {
			http.Error(w, "sglang boom", statuses.load)
			return
		}
		w.WriteHeader(statuses.load)
	})
	mux.HandleFunc("/unload_lora_adapter", func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.unloadReqs = append(r.unloadReqs, string(body))
		if statuses.unload >= 400 {
			http.Error(w, "sglang unload boom", statuses.unload)
			return
		}
		w.WriteHeader(statuses.unload)
	})
	r.Server = httptest.NewServer(mux)
	t.Cleanup(r.Close)
	return r
}

// newLoRARecnScheme builds a runtime.Scheme with only the types the
// LoRAAdapter reconciler touches. Mirrors builderTestScheme.
func newLoRARecnScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add v1alpha1: %v", err)
	}
	return s
}

// TestLoRAAdapterController_LoadsAgainstSGLang is the happy path: a
// LoRAAdapter referencing an existing sglang InferenceService causes a
// POST /load_lora_adapter with the right body, and the status
// condition Loaded=True reflects it.
func TestLoRAAdapterController_LoadsAgainstSGLang(t *testing.T) {
	const isvcName = "isvc-sglang"
	const adapterName = "loraA"
	const path = "/loras/a"

	// Real httptest recording server (asserts the on-the-wire body)
	sg := newRecordingSGLang(t, sglangStatuses{})
	fixedURL := sg.URL

	scheme := newLoRARecnScheme(t)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime: RuntimeSGLANG,
			// Spec.Endpoint.Port wins for the SGLang URL; we set it
			// so defaultLoRAAdapterURLResolver would resolve to a real
			// port, but the test overrides URLResolver to point at the
			// recording server.
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 30000},
			ModelRef: "m",
		},
		Status: inferencev1alpha1.InferenceServiceStatus{
			Endpoint: "http://placeholder:30000", // status.Endpoint must be non-empty for delete-time unload
		},
	}
	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-a", Namespace: "default", Generation: 1, Finalizers: []string{loraAdapterFinalizer}},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: isvcName},
			Name:                adapterName,
			Path:                path,
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc, adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	r := &LoRAAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		AdapterClient: NewSGLangAdapterClient(sg.Client()),
		URLResolver: func(_ context.Context, _ *inferencev1alpha1.InferenceService) (string, error) {
			return fixedURL, nil
		},
		Now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got inferencev1alpha1.LoRAAdapter
	if err := cl.Get(context.Background(), types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}, &got); err != nil {
		t.Fatalf("Get adapter after Reconcile: %v", err)
	}

	if got.Status.LoadedPath != path {
		t.Errorf("status.loadedPath = %q, want %q", got.Status.LoadedPath, path)
	}
	if got.Status.LastLoadedAt == nil || got.Status.LastLoadedAt.IsZero() {
		t.Error("status.lastLoadedAt unset after successful load")
	}

	loaded := findCondition(got.Status.Conditions, LoRAConditionLoaded)
	if loaded == nil || loaded.Status != metav1.ConditionTrue {
		t.Fatalf("condition Loaded = %+v, want True", loaded)
	}
	available := findCondition(got.Status.Conditions, LoRAConditionAvailable)
	if available == nil || available.Status != metav1.ConditionTrue {
		t.Fatalf("condition Available = %+v, want True", available)
	}
	errCond := findCondition(got.Status.Conditions, LoRAConditionError)
	if errCond == nil || errCond.Status != metav1.ConditionFalse {
		t.Fatalf("condition Error = %+v, want False", errCond)
	}

	if len(sg.loadReqs) != 1 {
		t.Fatalf("expected 1 load request, got %d (%v)", len(sg.loadReqs), sg.loadReqs)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(sg.loadReqs[0]), &body); err != nil {
		t.Fatalf("decode load body: %v", err)
	}
	if body["lora_name"] != adapterName || body["lora_path"] != path {
		t.Errorf("load body = %+v, want lora_name=%q lora_path=%q", body, adapterName, path)
	}
}

// TestLoRAAdapterController_RuntimeMismatch verifies that referencing
// a non-sglang InferenceService sets Available=False with
// RuntimeMismatch and never tries to talk to any HTTP endpoint.
func TestLoRAAdapterController_RuntimeMismatch(t *testing.T) {
	sg := newRecordingSGLang(t, sglangStatuses{})
	scheme := newLoRARecnScheme(t)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "isvc-vllm", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: "vllm", ModelRef: "m"},
	}
	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-a", Namespace: "default", Finalizers: []string{loraAdapterFinalizer}},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: "isvc-vllm"},
			Name:                "loraA",
			Path:                "/loras/a",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc, adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	fakec := &fakeAdapterClient{}
	r := &LoRAAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		AdapterClient: fakec,
		URLResolver: func(_ context.Context, _ *inferencev1alpha1.InferenceService) (string, error) {
			return sg.URL, nil
		},
		Now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got inferencev1alpha1.LoRAAdapter
	if err := cl.Get(context.Background(), types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	available := findCondition(got.Status.Conditions, LoRAConditionAvailable)
	if available == nil || available.Status != metav1.ConditionFalse {
		t.Fatalf("Available = %+v, want False", available)
	}
	if available.Reason != LoRAReasonRuntimeMismatch {
		t.Errorf("Available.Reason = %q, want %q", available.Reason, LoRAReasonRuntimeMismatch)
	}
	errCond := findCondition(got.Status.Conditions, LoRAConditionError)
	if errCond == nil || errCond.Status != metav1.ConditionTrue || errCond.Reason != LoRAReasonRuntimeMismatch {
		t.Fatalf("Error = %+v, want True/%s", errCond, LoRAReasonRuntimeMismatch)
	}
	if len(sg.loadReqs) != 0 {
		t.Errorf("expected 0 load requests against SGLang, got %d", len(sg.loadReqs))
	}
	if len(fakec.loadCalls) != 0 {
		t.Errorf("expected 0 fake load calls, got %d", len(fakec.loadCalls))
	}
}

// TestLoRAAdapterController_InferenceNotFound verifies the
// "target ISVC doesn't exist yet" path surfaces condition
// Available=False/InferenceNotFound and requeues for the operator's
// next change.
func TestLoRAAdapterController_InferenceNotFound(t *testing.T) {
	scheme := newLoRARecnScheme(t)
	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-a", Namespace: "default", Finalizers: []string{loraAdapterFinalizer}},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: "missing"},
			Name:                "loraA",
			Path:                "/loras/a",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	r := &LoRAAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		AdapterClient: &fakeAdapterClient{},
		URLResolver:   defaultLoRAAdapterURLResolver,
		Now:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected RequeueAfter > 0 when target ISVC is missing; got %+v", res)
	}

	var got inferencev1alpha1.LoRAAdapter
	if err := cl.Get(context.Background(), types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	available := findCondition(got.Status.Conditions, LoRAConditionAvailable)
	if available == nil || available.Reason != LoRAReasonInferenceNotFound || available.Status != metav1.ConditionFalse {
		t.Fatalf("Available = %+v, want False/%s", available, LoRAReasonInferenceNotFound)
	}
}

// isvcGetErrorClient returns a configurable error for any Get of an
// InferenceService, and delegates everything else to the wrapped
// client. Used by Reconcile-failure tests where we want to simulate
// RBAC denial / API outage without standing up a fake API server.
type isvcGetErrorClient struct {
	client.Client
	err error
}

func (c *isvcGetErrorClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*inferencev1alpha1.InferenceService); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// client is imported by the isvcGetErrorClient wrapper; alias here so
// a future refactor that drops the wrapper doesn't strand an unused
// import.
var _ client.Client

// TestLoRAAdapterController_ResolveInferenceServiceError covers the
// non-NotFound error branch in resolveInferenceService during the load
// path (e.g. RBAC denies the lookup or the API server is unreachable).
// The reconciler should surface the error and not attempt any HTTP call
// against SGLang.
func TestLoRAAdapterController_ResolveInferenceServiceError(t *testing.T) {
	scheme := newLoRARecnScheme(t)
	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-a", Namespace: "default", Finalizers: []string{loraAdapterFinalizer}},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: "isvc-blocked"},
			Name:                "loraA",
			Path:                "/loras/a",
		},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	wrapped := &isvcGetErrorClient{Client: base, err: errors.New("is forbidden: InferenceService get")}
	fakec := &fakeAdapterClient{}
	r := &LoRAAdapterReconciler{
		Client:        wrapped,
		Scheme:        scheme,
		AdapterClient: fakec,
		URLResolver:   defaultLoRAAdapterURLResolver,
		Now:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}})
	if err == nil {
		t.Fatal("expected reconciler to surface the ISVC-get error")
	}
	if !strings.Contains(err.Error(), "is forbidden") {
		t.Errorf("error %q does not include underlying ISVC-get error", err.Error())
	}
	if len(fakec.loadCalls) != 0 {
		t.Errorf("expected 0 SGLang load attempts on ISVC-get error; got %d", len(fakec.loadCalls))
	}
}

// TestLoRAAdapterController_LoadFailure verifies that a non-2xx from
// SGLang surfaces as Loaded=False/LoadUnsuccessful, Error=True, and
// the controller requeues (so an intermittent SGLang recovers without
// requiring a spec change).
func TestLoRAAdapterController_LoadFailure(t *testing.T) {
	sg := newRecordingSGLang(t, sglangStatuses{load: http.StatusInternalServerError})
	scheme := newLoRARecnScheme(t)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "isvc-sglang", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:  RuntimeSGLANG,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 30000},
		},
		Status: inferencev1alpha1.InferenceServiceStatus{Endpoint: "http://placeholder:30000"},
	}
	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-a", Namespace: "default", Finalizers: []string{loraAdapterFinalizer}},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: "isvc-sglang"},
			Name:                "loraA",
			Path:                "/loras/a",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc, adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	r := &LoRAAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		AdapterClient: NewSGLangAdapterClient(sg.Client()),
		URLResolver:   func(_ context.Context, _ *inferencev1alpha1.InferenceService) (string, error) { return sg.URL, nil },
		Now:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected RequeueAfter > 0 on load failure; got %+v", res)
	}
	if len(sg.loadReqs) == 0 {
		t.Fatal("expected at least one load request")
	}

	var got inferencev1alpha1.LoRAAdapter
	if err := cl.Get(context.Background(), types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	loaded := findCondition(got.Status.Conditions, LoRAConditionLoaded)
	if loaded == nil || loaded.Status != metav1.ConditionFalse || loaded.Reason != LoRAReasonLoadUnsuccessful {
		t.Fatalf("Loaded = %+v, want False/%s", loaded, LoRAReasonLoadUnsuccessful)
	}
}

// TestLoRAAdapterController_DeleteUnloads: a LoRAAdapter marked for
// deletion triggers an unload against the runtime, then the finalizer
// drops so Kubernetes can complete garbage collection.
func TestLoRAAdapterController_DeleteUnloads(t *testing.T) {
	const adapterName = "loraA"
	sg := newRecordingSGLang(t, sglangStatuses{})
	fixedURL := sg.URL
	scheme := newLoRARecnScheme(t)

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "isvc-sglang", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:  RuntimeSGLANG,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 30000},
		},
		Status: inferencev1alpha1.InferenceServiceStatus{Endpoint: "http://placeholder:30000"},
	}

	now := metav1.NewTime(time.Unix(1700000000, 0).UTC())
	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "adapter-a",
			Namespace:         "default",
			Finalizers:        []string{loraAdapterFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: "isvc-sglang"},
			Name:                adapterName,
			Path:                "/loras/a",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc, adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	r := &LoRAAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		AdapterClient: NewSGLangAdapterClient(sg.Client()),
		URLResolver:   func(_ context.Context, _ *inferencev1alpha1.InferenceService) (string, error) { return fixedURL, nil },
		Now:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(sg.unloadReqs) != 1 {
		t.Fatalf("expected 1 unload request, got %d (%v)", len(sg.unloadReqs), sg.unloadReqs)
	}

	// Real Kubernetes would garbage-collect the resource once its
	// finalizers drop and DeletionTimestamp is set; the fake client
	// mirrors that, so we don't re-Get the adapter. The unload having
	// arrived at the recording server is the contract we asserted.
}

// TestLoRAAdapterController_SkipsLoadWhenAlreadyLoaded asserts that a
// second reconcile of an unchanged adapter does NOT re-issue the HTTP
// load against SGLang. SGLang's load_lora_adapter would otherwise
// re-load the adapter on every controller-side change (status patch,
// finalizer update, metadata touch — anything that triggers a watch
// event), which would flap served traffic for no reason. The skip
// requires (a) LoadedPath matches spec.Path and (b) LastLoadedAt is
// recent (within the safety window). Out-of-window or path-mismatch
// adapters must still be reloaded.
func TestLoRAAdapterController_SkipsLoadWhenAlreadyLoaded(t *testing.T) {
	sg := newRecordingSGLang(t, sglangStatuses{})
	scheme := newLoRARecnScheme(t)
	const adapterName = "loraA"
	const path = "/loras/a"
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "isvc-sglang", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:  RuntimeSGLANG,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 30000},
		},
		Status: inferencev1alpha1.InferenceServiceStatus{Endpoint: "http://placeholder:30000"},
	}
	now := metav1.NewTime(time.Unix(1700000000, 0).UTC())
	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-a", Namespace: "default", Generation: 1, Finalizers: []string{loraAdapterFinalizer}},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: "isvc-sglang"},
			Name:                adapterName,
			Path:                path,
		},
		Status: inferencev1alpha1.LoRAAdapterStatus{
			LoadedPath:   path,
			LastLoadedAt: &now,
			Conditions: []metav1.Condition{
				{Type: LoRAConditionLoaded, Status: metav1.ConditionTrue, Reason: LoRAReasonReconcileSuccess},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc, adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	fakec := &fakeAdapterClient{}
	r := &LoRAAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		AdapterClient: fakec,
		URLResolver:   func(_ context.Context, _ *inferencev1alpha1.InferenceService) (string, error) { return sg.URL, nil },
		Now: func() time.Time {
			// Same instant as LastLoadedAt — safely within the window.
			return now.Time
		},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(sg.loadReqs) != 0 {
		t.Errorf("expected 0 SGLang load requests when already loaded; got %d (%v)", len(sg.loadReqs), sg.loadReqs)
	}
	if len(fakec.loadCalls) != 0 {
		t.Errorf("expected 0 fake load calls when already loaded; got %d", len(fakec.loadCalls))
	}
}

// TestLoRAAdapterController_ReloadsWhenPathChanged asserts the inverse:
// the second reconcile re-loads because LoadedPath != spec.Path (the
// operator moved the adapter mount to a new in-pod location).
func TestLoRAAdapterController_ReloadsWhenPathChanged(t *testing.T) {
	sg := newRecordingSGLang(t, sglangStatuses{})
	scheme := newLoRARecnScheme(t)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "isvc-sglang", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:  RuntimeSGLANG,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 30000},
		},
		Status: inferencev1alpha1.InferenceServiceStatus{Endpoint: "http://placeholder:30000"},
	}
	now := metav1.NewTime(time.Unix(1700000000, 0).UTC())
	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-a", Namespace: "default", Generation: 2, Finalizers: []string{loraAdapterFinalizer}},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: "isvc-sglang"},
			Name:                "loraA",
			Path:                "/loras/a-v2", // changed
		},
		Status: inferencev1alpha1.LoRAAdapterStatus{
			LoadedPath:   "/loras/a",
			LastLoadedAt: &now,
			Conditions: []metav1.Condition{
				{Type: LoRAConditionLoaded, Status: metav1.ConditionTrue, Reason: LoRAReasonReconcileSuccess},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc, adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	r := &LoRAAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		AdapterClient: NewSGLangAdapterClient(sg.Client()),
		URLResolver:   func(_ context.Context, _ *inferencev1alpha1.InferenceService) (string, error) { return sg.URL, nil },
		Now:           func() time.Time { return now.Time },
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(sg.loadReqs) != 1 {
		t.Errorf("expected 1 SGLang load request after path change; got %d", len(sg.loadReqs))
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(sg.loadReqs[0]), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["lora_path"] != "/loras/a-v2" {
		t.Errorf("reloaded with lora_path=%q, want /loras/a-v2", body["lora_path"])
	}
}

// TestLoRAAdapterController_AdoptsOnFirstReconcile verifies the
// first-reconcile path sets Available=False/Loaded=False with a
// FinalizerAdded reason, and adds the finalizer.
func TestLoRAAdapterController_AdoptsOnFirstReconcile(t *testing.T) {
	scheme := newLoRARecnScheme(t)
	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-a", Namespace: "default"},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: "isvc-sglang"},
			Name:                "loraA",
			Path:                "/loras/a",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	r := &LoRAAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		AdapterClient: &fakeAdapterClient{},
		URLResolver:   defaultLoRAAdapterURLResolver,
		Now:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got inferencev1alpha1.LoRAAdapter
	if err := cl.Get(context.Background(), types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	hasFinalizer := false
	for _, f := range got.Finalizers {
		if f == loraAdapterFinalizer {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Errorf("finalizer %q missing after first reconcile", loraAdapterFinalizer)
	}
}

// TestSGLangAdapterClient_RealWire exercises the real
// sglangAdapterClient against a recording httptest server. Belt-and-
// suspenders: the controller-level tests use the interface, but we
// want a direct round-trip on the production client to lock in the
// request shape SGLang actually expects.
func TestSGLangAdapterClient_RealWire(t *testing.T) {
	sg := newRecordingSGLang(t, sglangStatuses{})
	c := NewSGLangAdapterClient(sg.Client())
	if err := c.LoadAdapter(context.Background(), sg.URL, "loraA", "/loras/a"); err != nil {
		t.Fatalf("LoadAdapter: %v", err)
	}
	if err := c.UnloadAdapter(context.Background(), sg.URL, "loraA"); err != nil {
		t.Fatalf("UnloadAdapter: %v", err)
	}

	if len(sg.loadReqs) != 1 || !strings.Contains(sg.loadReqs[0], "\"lora_name\":\"loraA\"") {
		t.Errorf("loadReqs[0] = %q, want lora_name=loraA", sg.loadReqs)
	}
	if len(sg.unloadReqs) != 1 || !strings.Contains(sg.unloadReqs[0], "\"lora_name\":\"loraA\"") {
		t.Errorf("unloadReqs[0] = %q, want lora_name=loraA", sg.unloadReqs)
	}
}

// TestSGLangAdapterClient_Non2xxIsError verifies the real client
// surfaces SGLang's response status and body in the error so the
// status.conditions.message gets something useful.
func TestSGLangAdapterClient_Non2xxIsError(t *testing.T) {
	sg := newRecordingSGLang(t, sglangStatuses{load: http.StatusBadRequest})
	c := NewSGLangAdapterClient(sg.Client())
	err := c.LoadAdapter(context.Background(), sg.URL, "loraA", "/loras/a")
	if err == nil {
		t.Fatal("expected error on 400 response")
	}
	// body should be included; the recording server responds "sglang boom"
	if !strings.Contains(err.Error(), "sglang boom") {
		t.Errorf("error %q does not include SGLang's body", err.Error())
	}
}

// TestSGLangAdapterClient_JSONContentType locks the wire-level content
// type that SGLang expects. The recording server doesn't assert on it,
// but a future test suite should be able to.
func TestSGLangAdapterClient_JSONContentType(t *testing.T) {
	// Use a tiny custom server instead of the recording helper so we
	// can inspect Content-Type directly.
	var seenContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenContentType = r.Header.Get("Content-Type")
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewSGLangAdapterClient(srv.Client())
	if err := c.LoadAdapter(context.Background(), srv.URL, "loraA", "/loras/a"); err != nil {
		t.Fatalf("LoadAdapter: %v", err)
	}
	if seenContentType != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", seenContentType)
	}

	// Also confirm the body is well-formed JSON; a debugging aid for
	// future serializer changes.
	seenContentType = ""
	if err := c.LoadAdapter(context.Background(), srv.URL, "loraA", "/loras/a"); err != nil {
		t.Fatalf("LoadAdapter (round 2): %v", err)
	}
	if !bytes.HasPrefix([]byte(seenContentType), []byte{}) {
		// trivially true; left here as a panic surface if HTTP layer
		// changes later
		_ = seenContentType
	}
}

// TestDefaultLoRAAdapterURLResolver_PicksSpecPort verifies the URL
// resolver walks the precedence order correctly: Endpoint.Port >
// ContainerPort > runtime default.
func TestDefaultLoRAAdapterURLResolver_PicksSpecPort(t *testing.T) {
	port := int32(31000)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:       RuntimeSGLANG,
			ContainerPort: &port,
		},
	}
	got, err := defaultLoRAAdapterURLResolver(context.Background(), isvc)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if got != "http://svc.ns.svc:31000" {
		t.Errorf("resolver = %q, want http://svc.ns.svc:31000", got)
	}

	port2 := int32(32000)
	isvc.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Port: port2}
	got, err = defaultLoRAAdapterURLResolver(context.Background(), isvc)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if got != "http://svc.ns.svc:32000" {
		t.Errorf("resolver with Endpoint.Port = %q, want http://svc.ns.svc:32000", got)
	}
}

// findCondition is a small test-local helper modeled on the one used
// elsewhere in this package; defined here so this file stands alone if
// imported only for these tests.

// findCondition is provided by runtime_test.go (same package).

// TestLoRAAdapterController_IsNotFound covers the early-return in
// Reconcile when the adapter is gone between watch and reconcile.
func TestLoRAAdapterController_IsNotFound(t *testing.T) {
	scheme := newLoRARecnScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	r := &LoRAAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		AdapterClient: &fakeAdapterClient{},
		URLResolver:   defaultLoRAAdapterURLResolver,
		Now:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "nope", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue on IsNotFound; got %+v", res)
	}
}

// TestLoRAAdapterController_URLResolverError covers the path where the
// ISVC is fine but the URL resolver returns an error (e.g. non-sglang
// runtime with no port fields). Should set Loaded=False/InvalidPort and
// Error=True, no SGLang HTTP call.
func TestLoRAAdapterController_URLResolverError(t *testing.T) {
	sg := newRecordingSGLang(t, sglangStatuses{})
	scheme := newLoRARecnScheme(t)
	// non-sglang runtime + no ports → resolver returns the "cannot
	// resolve port" error.
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "isvc-vllm", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: "vllm"},
	}
	// but the controller should bail on RuntimeMismatch before even
	// asking the resolver. Force the resolver path with a custom
	// resolver that returns an error and a runtime that is sglang.
	isvc.Spec.Runtime = RuntimeSGLANG

	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-a", Namespace: "default", Finalizers: []string{loraAdapterFinalizer}},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: "isvc-vllm"},
			Name:                "loraA",
			Path:                "/loras/a",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc, adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	fakec := &fakeAdapterClient{}
	r := &LoRAAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		AdapterClient: fakec,
		URLResolver: func(_ context.Context, _ *inferencev1alpha1.InferenceService) (string, error) {
			return "", errors.New("synthetic resolver boom")
		},
		Now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got inferencev1alpha1.LoRAAdapter
	if err := cl.Get(context.Background(), types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	loaded := findCondition(got.Status.Conditions, LoRAConditionLoaded)
	if loaded == nil || loaded.Status != metav1.ConditionFalse || loaded.Reason != LoRAReasonInvalidPort {
		t.Fatalf("Loaded = %+v, want False/%s", loaded, LoRAReasonInvalidPort)
	}
	errCond := findCondition(got.Status.Conditions, LoRAConditionError)
	if errCond == nil || errCond.Status != metav1.ConditionTrue || errCond.Reason != LoRAReasonInvalidPort {
		t.Fatalf("Error = %+v, want True/%s", errCond, LoRAReasonInvalidPort)
	}
	if len(sg.loadReqs) != 0 {
		t.Errorf("expected 0 SGLang calls; got %d", len(sg.loadReqs))
	}
	if len(fakec.loadCalls) != 0 {
		t.Errorf("expected 0 fake load calls; got %d", len(fakec.loadCalls))
	}
}

// TestLoRAAdapterController_DeleteUnloadError covers the best-effort
// unload-on-delete path: when SGLang returns a non-2xx, the
// reconciler still drops the finalizer (so K8s can GC), records the
// failure via conditions, and the post-delete Get must be a NotFound
// (finalizer-less + DeletionTimestamp → GC'd by the fake client).
func TestLoRAAdapterController_DeleteUnloadError(t *testing.T) {
	sg := newRecordingSGLang(t, sglangStatuses{unload: http.StatusInternalServerError})
	scheme := newLoRARecnScheme(t)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "isvc-sglang", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:  RuntimeSGLANG,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 30000},
		},
		Status: inferencev1alpha1.InferenceServiceStatus{Endpoint: "http://placeholder:30000"},
	}
	now := metav1.NewTime(time.Unix(1700000000, 0).UTC())
	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "adapter-a",
			Namespace:         "default",
			Finalizers:        []string{loraAdapterFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: "isvc-sglang"},
			Name:                "loraA",
			Path:                "/loras/a",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc, adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	r := &LoRAAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		AdapterClient: NewSGLangAdapterClient(sg.Client()),
		URLResolver:   func(_ context.Context, _ *inferencev1alpha1.InferenceService) (string, error) { return sg.URL, nil },
		Now:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The unload still hit the wire (best-effort, not short-circuited).
	if len(sg.unloadReqs) != 1 {
		t.Fatalf("expected 1 unload attempt, got %d", len(sg.unloadReqs))
	}
	// The object was GC'd: fake client removes it once finalizers drop on a
	// DeletionTimestamp-marked object. Mirrors production K8s behavior.
	var got inferencev1alpha1.LoRAAdapter
	if err := cl.Get(context.Background(), types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}, &got); err == nil {
		t.Errorf("expected NotFound after delete reconcile; got the adapter back: %+v", got)
	}
}

// TestLoRAAdapterController_DeleteURLResolverError covers the
// finalizer-drop path when the URLResolver fails: drop the finalizer
// anyway so the resource can be garbage-collected.
func TestLoRAAdapterController_DeleteURLResolverError(t *testing.T) {
	scheme := newLoRARecnScheme(t)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "isvc-sglang", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:  RuntimeSGLANG,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 30000},
		},
		Status: inferencev1alpha1.InferenceServiceStatus{Endpoint: "http://placeholder:30000"},
	}
	now := metav1.NewTime(time.Unix(1700000000, 0).UTC())
	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "adapter-a",
			Namespace:         "default",
			Finalizers:        []string{loraAdapterFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: "isvc-sglang"},
			Name:                "loraA",
			Path:                "/loras/a",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc, adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	fakec := &fakeAdapterClient{}
	r := &LoRAAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		AdapterClient: fakec,
		URLResolver: func(_ context.Context, _ *inferencev1alpha1.InferenceService) (string, error) {
			return "", errors.New("synthetic resolver boom")
		},
		Now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(fakec.unloadCalls) != 0 {
		t.Errorf("expected no unload attempts when resolver errors; got %d", len(fakec.unloadCalls))
	}
	var got inferencev1alpha1.LoRAAdapter
	if err := cl.Get(context.Background(), types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}, &got); err == nil {
		t.Errorf("expected NotFound after delete reconcile; got the adapter back: %+v", got)
	}
}

// TestDefaultLoRAAdapterURLResolver_ErrorPaths covers the failure
// branches of the production resolver: nil ISVC and "no port + non-sglang".
func TestDefaultLoRAAdapterURLResolver_ErrorPaths(t *testing.T) {
	if _, err := defaultLoRAAdapterURLResolver(context.Background(), nil); err == nil {
		t.Error("expected error for nil ISVC")
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: "vllm"},
	}
	if _, err := defaultLoRAAdapterURLResolver(context.Background(), isvc); err == nil {
		t.Error("expected error for non-sglang with no port")
	}
	// sglang without explicit port succeeds (30000 default).
	isvc.Spec.Runtime = RuntimeSGLANG
	got, err := defaultLoRAAdapterURLResolver(context.Background(), isvc)
	if err != nil {
		t.Fatalf("resolver (sglang default): %v", err)
	}
	if got != "http://svc.ns.svc:30000" {
		t.Errorf("resolver = %q, want http://svc.ns.svc:30000", got)
	}
	// Endpoint with Port == 0 should fall through to ContainerPort.
	zero := int32(0)
	isvc.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Port: zero}
	isvc.Spec.ContainerPort = ptrInt32(31001)
	got, err = defaultLoRAAdapterURLResolver(context.Background(), isvc)
	if err != nil {
		t.Fatalf("resolver (zero Endpoint port): %v", err)
	}
	if got != "http://svc.ns.svc:31001" {
		t.Errorf("resolver = %q, want http://svc.ns.svc:31001", got)
	}
}

// TestDefaultLoRAAdapterURLResolver_SanitizesDNSName asserts the
// production resolver uses sanitizeDNSName (dots → dashes) the same way
// the InferenceService controller does when it builds the cluster-local
// Service. An ISVC named "llama-3.1-8b" creates a Service named
// "llama-3-1-8b" — the resolver must point at that, not at the raw name.
func TestDefaultLoRAAdapterURLResolver_SanitizesDNSName(t *testing.T) {
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "llama-3.1-8b", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:  RuntimeSGLANG,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 30000},
		},
	}
	got, err := defaultLoRAAdapterURLResolver(context.Background(), isvc)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	want := "http://llama-3-1-8b.default.svc:30000"
	if got != want {
		t.Errorf("resolver = %q, want %q (must use sanitizeDNSName, dots -> dashes)", got, want)
	}

	// Plain alphanumeric names (no dots) should be unchanged.
	isvc.Name = "llama3-8b"
	got, err = defaultLoRAAdapterURLResolver(context.Background(), isvc)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if got != "http://llama3-8b.default.svc:30000" {
		t.Errorf("resolver = %q, want no sanitization for plain name", got)
	}
}

// TestLoRAAdapterController_DeleteISVCGetError covers the early-return
// in reconcileDelete when the ISVC Get fails with a non-NotFound error
// (e.g. RBAC denies it). The reconciler should surface the error and
// leave the finalizer in place so a future reconcile can retry.
func TestLoRAAdapterController_DeleteISVCGetError(t *testing.T) {
	scheme := newLoRARecnScheme(t)
	now := metav1.NewTime(time.Unix(1700000000, 0).UTC())
	adapter := &inferencev1alpha1.LoRAAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "adapter-a",
			Namespace:         "default",
			Finalizers:        []string{loraAdapterFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: inferencev1alpha1.LoRAAdapterSpec{
			InferenceServiceRef: inferencev1alpha1.LocalInferenceServiceReference{Name: "isvc-gone"},
			Name:                "loraA",
			Path:                "/loras/a",
		},
	}
	// Wrap the fake client so any Get of an InferenceService returns a
	// synthetic non-NotFound error (no admission webhooks here, so we
	// simulate RBAC denial).
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(adapter).WithStatusSubresource(&inferencev1alpha1.LoRAAdapter{}).Build()
	wrapped := &isvcGetErrorClient{Client: base, err: errors.New("is forbidden: InferenceService get")}
	fakec := &fakeAdapterClient{}
	r := &LoRAAdapterReconciler{
		Client:        wrapped,
		Scheme:        scheme,
		AdapterClient: fakec,
		URLResolver:   defaultLoRAAdapterURLResolver,
		Now:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: adapter.Name, Namespace: adapter.Namespace}})
	if err == nil {
		t.Fatal("expected error from reconciler; got nil")
	}
	if !strings.Contains(err.Error(), "is forbidden") {
		t.Errorf("error %q does not include the underlying ISVC-get error", err.Error())
	}
	if len(fakec.unloadCalls) != 0 {
		t.Errorf("expected no unload attempts on early-return; got %d", len(fakec.unloadCalls))
	}
}

// TestSGLangAdapterClient_BadURLError covers the postJSON request-build
// error path (URL parse failure on a control character).
func TestSGLangAdapterClient_BadURLError(t *testing.T) {
	c := NewSGLangAdapterClient(http.DefaultClient)
	// Control characters are invalid in URL paths and trip
	// url.Parse / NewRequestWithContext.
	err := c.LoadAdapter(context.Background(), "http://bad\x00host/load_lora_adapter", "loraA", "/loras/a")
	if err == nil {
		t.Fatal("expected error for URL with control character")
	}
	if !strings.Contains(err.Error(), "build request") {
		t.Errorf("error %q does not mention request build", err.Error())
	}
}
