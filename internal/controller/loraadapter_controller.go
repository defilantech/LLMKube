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
	"fmt"
	"io"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Condition types and reasons used by LoRAAdapterReconciler. Surfaced
// through status.conditions so an operator can grep `kubectl get
// loraadapter -o yaml` to understand why an adapter is or is not loaded.
const (
	LoRAConditionAvailable = "Available"
	LoRAConditionLoaded    = "Loaded"
	LoRAConditionError     = "Error"

	LoRAReasonRuntimeMismatch    = "RuntimeMismatch"
	LoRAReasonInferenceNotFound  = "InferenceServiceNotFound"
	LoRAReasonInvalidPort        = "InvalidPort"
	LoRAReasonLoadUnsuccessful   = "LoadUnsuccessful"
	LoRAReasonUnloadUnsuccessful = "UnloadUnsuccessful"
	LoRAReasonReconcilerError    = "ReconcilerError"
	LoRAReasonReconcileSuccess   = "ReconcileSuccess"
	LoRAReasonFinalizerAdded     = "FinalizerAdded"
	LoRAReasonAlreadyLoaded      = "AlreadyLoaded"
)

// loadSkipWindow is the idempotency guard for shouldSkipLoad. If the
// last successful load happened within this window AND the LoadedPath
// matches spec.Path, the reconciler skips re-issuing the HTTP call —
// otherwise every controller-side patch (status update, finalizer
// touch, annotation change) would flap the served adapter set on
// SGLang. The window is intentionally short: long enough to swallow the
// quick reconciles a single user action produces, short enough that
// stale controller state recovers within a few minutes even if the
// guard ever mistakenly under-counts.
const loadSkipWindow = 2 * time.Minute

// loraAdapterFinalizer is removed once SGLang has acknowledged the
// unload; until then, the resource stays in the API to give the
// operator (and SGLang) a chance to settle.
const loraAdapterFinalizer = "loraadapter.inference.llmkube.dev/finalizer"

// SGLangAdapterClient is the surface LoRAAdapterReconciler uses to talk
// to SGLang's /load_lora_adapter and /unload_lora_adapter HTTP
// endpoints (no /v1 prefix, singular `lora_adapter`, as of SGLang
// v0.5.15 — see
// https://github.com/sgl-project/sglang/blob/v0.5.15/python/sglang/srt/entrypoints/http_server.py).
// The production wiring builds one with NewSGLangAdapterClient over an
// http.Client; tests inject a fake in package-internal _test.go files.
type SGLangAdapterClient interface {
	LoadAdapter(ctx context.Context, baseURL, name, path string) error
	UnloadAdapter(ctx context.Context, baseURL, name string) error
}

// sglangAdapterClient is the default HTTP-backed implementation. It is
// stateless and safe for concurrent use.
type sglangAdapterClient struct {
	httpClient *http.Client
}

// NewSGLangAdapterClient builds an SGLangAdapterClient suitable for
// production. Callers should provide a Client with a sensible Timeout
// (the reconciler does not set one).
func NewSGLangAdapterClient(c *http.Client) SGLangAdapterClient {
	return &sglangAdapterClient{httpClient: c}
}

func (c *sglangAdapterClient) LoadAdapter(ctx context.Context, baseURL, name, path string) error {
	return c.postJSON(ctx, baseURL+"/load_lora_adapter",
		map[string]string{"lora_name": name, "lora_path": path})
}

func (c *sglangAdapterClient) UnloadAdapter(ctx context.Context, baseURL, name string) error {
	return c.postJSON(ctx, baseURL+"/unload_lora_adapter",
		map[string]string{"lora_name": name})
}

func (c *sglangAdapterClient) postJSON(ctx context.Context, url string, body map[string]string) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		// Surface SGLang's body too — it's the only diagnostic the
		// operator gets without checking the SGLang pod directly.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("sglang %s: %s: %s", url, resp.Status, string(b))
	}
	return nil
}

// LoRAAdapterURLResolver builds the per-LoRAAdapter SGLang base URL given
// the target InferenceService. The default wiring uses the cluster-local
// Service DNS that the InferenceService controller creates:
//
//	http://<isvc-name>.<isvc-namespace>.svc:<port>
//
// where <port> is Spec.Endpoint.Port, then Spec.ContainerPort, then the
// runtime's DefaultPort. Tests override this to point at httptest.URL.
type LoRAAdapterURLResolver func(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (string, error)

// defaultLoRAAdapterURLResolver is the production resolver. It mirrors
// how the InferenceService controller builds its cluster-local Service
// (so the LoRAAdapter controller and the SGLang pod agree on the
// endpoint). Service names are sanitized via the shared sanitizeDNSName
// helper — dots become dashes, otherwise an ISVC named "llama-3.1-8b"
// would resolve to a host that does not exist (#1060 review).
func defaultLoRAAdapterURLResolver(_ context.Context, isvc *inferencev1alpha1.InferenceService) (string, error) {
	if isvc == nil {
		return "", errors.New("nil InferenceService")
	}
	svcName := sanitizeDNSName(isvc.Name)
	if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		return fmt.Sprintf("http://%s.%s.svc:%d", svcName, isvc.Namespace, isvc.Spec.Endpoint.Port), nil
	}
	if isvc.Spec.ContainerPort != nil {
		return fmt.Sprintf("http://%s.%s.svc:%d", svcName, isvc.Namespace, *isvc.Spec.ContainerPort), nil
	}
	if isvc.Spec.Runtime == RuntimeSGLANG {
		return fmt.Sprintf("http://%s.%s.svc:%d", svcName, isvc.Namespace, 30000), nil
	}
	return "", fmt.Errorf("cannot resolve port for %s/%s runtime=%q", isvc.Namespace, isvc.Name, isvc.Spec.Runtime)
}

// LoRAAdapterReconciler reconciles LoRAAdapter resources by issuing
// /load_lora_adapter and /unload_lora_adapter against the target SGLang
// inference service. The reconciler is idempotent: a second reconcile
// of an unchanged adapter (same LoadedPath, recent LastLoadedAt) skips
// the HTTP call, so a status patch or finalizer touch does not flap
// SGLang's served adapter set.
//
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=loraadapters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=loraadapters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=loraadapters/finalizers,verbs=update
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices,verbs=get;list;watch
type LoRAAdapterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// AdapterClient is the HTTP client for SGLang. Defaults to a 30s
	// client if nil.
	AdapterClient SGLangAdapterClient
	// URLResolver builds the SGLang base URL per InferenceService.
	// Defaults to defaultLoRAAdapterURLResolver if nil.
	URLResolver LoRAAdapterURLResolver
	// Now is injectable for tests; production uses time.Now.
	Now func() time.Time
}

// SetupWithManager wires this reconciler into mgr. It watches LoRAAdapter
// primary resources; the controller-runtime-built index reconciliations
// every change to a LoRAAdapter without depending on a watcher on
// InferenceService (we Get the inference inside Reconcile).
func (r *LoRAAdapterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.AdapterClient == nil {
		r.AdapterClient = NewSGLangAdapterClient(&http.Client{Timeout: 30 * time.Second})
	}
	if r.URLResolver == nil {
		r.URLResolver = defaultLoRAAdapterURLResolver
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.LoRAAdapter{}).
		Named("loraadapter").
		Complete(r)
}

// Reconcile implements the SGLang-side load/unload lifecycle for a single
// LoRAAdapter. Idempotent on every entry: the controller treats the
// desired world as "SGLang's loaded set matches the LoRAAdapter set with
// matching (isvc, name)". Finalizer handling ensures SGLang unloads
// before Kubernetes garbage-collects the resource.
func (r *LoRAAdapterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logc := log.FromContext(ctx).WithValues("loraAdapter", req.NamespacedName)

	adapter := &inferencev1alpha1.LoRAAdapter{}
	if err := r.Get(ctx, req.NamespacedName, adapter); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Finalizer path: the resource is being deleted. Run the unload
	// against SGLang, then drop the finalizer so Kubernetes can finish
	// the deletion.
	if !adapter.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(adapter, loraAdapterFinalizer) {
			return ctrl.Result{}, nil
		}
		return r.reconcileDelete(ctx, logc, adapter)
	}

	// Adopt the resource on first sight so we keep SGLang state in sync
	// with the resource lifecycle.
	if !controllerutil.ContainsFinalizer(adapter, loraAdapterFinalizer) {
		if err := r.addFinalizer(ctx, adapter); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	return r.reconcileLoad(ctx, logc, adapter)
}

// reconcileLoad drives the load side: resolve the InferenceService,
// validate it is an SGLang runtime, then POST to SGLang. Status
// conditions reflect each step's outcome.
func (r *LoRAAdapterReconciler) reconcileLoad(ctx context.Context, logc ctrlLog, adapter *inferencev1alpha1.LoRAAdapter) (ctrl.Result, error) {
	isvc, err := r.resolveInferenceService(ctx, adapter)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.setCondition(adapter, LoRAConditionAvailable, metav1.ConditionFalse,
				LoRAReasonInferenceNotFound, err.Error())
			r.setCondition(adapter, LoRAConditionLoaded, metav1.ConditionFalse,
				LoRAReasonInferenceNotFound, "waiting for the referenced InferenceService to exist")
			r.setCondition(adapter, LoRAConditionError, metav1.ConditionTrue,
				LoRAReasonInferenceNotFound, err.Error())
			return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, adapter)
		}
		return ctrl.Result{}, err
	}

	if isvc.Spec.Runtime != RuntimeSGLANG {
		r.setCondition(adapter, LoRAConditionAvailable, metav1.ConditionFalse,
			LoRAReasonRuntimeMismatch,
			fmt.Sprintf("referenced InferenceService/%s/%s runtime=%q, expected sglang",
				isvc.Namespace, isvc.Name, isvc.Spec.Runtime))
		r.setCondition(adapter, LoRAConditionLoaded, metav1.ConditionFalse,
			LoRAReasonRuntimeMismatch, "skipped: target runtime is not sglang")
		r.setCondition(adapter, LoRAConditionError, metav1.ConditionTrue,
			LoRAReasonRuntimeMismatch, "load cannot proceed against a non-sglang runtime")
		// Backoff the same way the NotFound branch does: the spec is
		// not actionable until the operator changes something.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, adapter)
	}

	baseURL, err := r.URLResolver(ctx, isvc)
	if err != nil {
		r.setCondition(adapter, LoRAConditionAvailable, metav1.ConditionTrue,
			LoRAReasonReconcilerError, "spec is well-formed but port resolution failed")
		r.setCondition(adapter, LoRAConditionLoaded, metav1.ConditionFalse,
			LoRAReasonInvalidPort, err.Error())
		r.setCondition(adapter, LoRAConditionError, metav1.ConditionTrue,
			LoRAReasonInvalidPort, err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, adapter)
	}

	// Idempotency guard: if the adapter is already loaded at the same
	// path within the safety window, skip the HTTP call. Without this
	// every spec patch (status update, finalizer touch, metadata) would
	// re-load and flap SGLang's served adapter set. Out-of-window or
	// path-mismatch always re-loads.
	if r.shouldSkipLoad(adapter) {
		r.setCondition(adapter, LoRAConditionAvailable, metav1.ConditionTrue,
			LoRAReasonReconcileSuccess, "InferenceService exists and is sglang")
		r.setCondition(adapter, LoRAConditionLoaded, metav1.ConditionTrue,
			LoRAReasonAlreadyLoaded, "skip: already loaded within safety window")
		r.setCondition(adapter, LoRAConditionError, metav1.ConditionFalse,
			LoRAReasonReconcileSuccess, "no error")
		return ctrl.Result{}, r.Status().Update(ctx, adapter)
	}

	if err := r.AdapterClient.LoadAdapter(ctx, baseURL, adapter.Spec.Name, adapter.Spec.Path); err != nil {
		logc.Error(err, "sglang /load_lora_adapter failed")
		r.setCondition(adapter, LoRAConditionAvailable, metav1.ConditionTrue,
			LoRAReasonReconcileSuccess, "InferenceService exists and is sglang")
		r.setCondition(adapter, LoRAConditionLoaded, metav1.ConditionFalse,
			LoRAReasonLoadUnsuccessful, err.Error())
		r.setCondition(adapter, LoRAConditionError, metav1.ConditionTrue,
			LoRAReasonLoadUnsuccessful, err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, adapter)
	}

	now := metav1.NewTime(r.Now())
	adapter.Status.LoadedPath = adapter.Spec.Path
	adapter.Status.LastLoadedAt = &now
	r.setCondition(adapter, LoRAConditionAvailable, metav1.ConditionTrue,
		LoRAReasonReconcileSuccess, "InferenceService exists and is sglang")
	r.setCondition(adapter, LoRAConditionLoaded, metav1.ConditionTrue,
		LoRAReasonReconcileSuccess,
		fmt.Sprintf("loaded as %q at %s", adapter.Spec.Name, adapter.Spec.Path))
	r.setCondition(adapter, LoRAConditionError, metav1.ConditionFalse,
		LoRAReasonReconcileSuccess, "no error")
	return ctrl.Result{}, r.Status().Update(ctx, adapter)
}

// reconcileDelete is the finalizer-only path. It issues the unload
// against SGLang and unconditionally drops the finalizer so Kubernetes
// can complete the deletion. Requeueing on unload failure is racy — by
// the time we requeued, the resource would be GC'd. The compromise:
// best-effort unload, surface via the Error condition on the still-
// extant resource, and drop the finalizer regardless. An operator who
// wants strict unload ordering can re-create the LoRAAdapter.
func (r *LoRAAdapterReconciler) reconcileDelete(ctx context.Context, logc ctrlLog, adapter *inferencev1alpha1.LoRAAdapter) (ctrl.Result, error) {
	isvc := &inferencev1alpha1.InferenceService{}
	ref := adapter.Spec.InferenceServiceRef
	ns := adapter.Namespace
	if ref.Namespace != nil {
		ns = *ref.Namespace
	}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, isvc); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get InferenceService: %w", err)
	}

	if isvc.Spec.Runtime == RuntimeSGLANG && isvc.Status.Endpoint != "" {
		baseURL, err := r.URLResolver(ctx, isvc)
		if err == nil {
			if unloadErr := r.AdapterClient.UnloadAdapter(ctx, baseURL, adapter.Spec.Name); unloadErr != nil {
				// best-effort: log and drop the finalizer anyway.
				logc.Error(unloadErr, "sglang /unload_lora_adapter failed; dropping finalizer",
					"loraAdapter", adapter.Name)
				r.setCondition(adapter, LoRAConditionLoaded, metav1.ConditionFalse,
					LoRAReasonUnloadUnsuccessful, unloadErr.Error())
				r.setCondition(adapter, LoRAConditionError, metav1.ConditionTrue,
					LoRAReasonUnloadUnsuccessful, unloadErr.Error())
				_ = r.Status().Update(ctx, adapter)
			}
		}
	}

	controllerutil.RemoveFinalizer(adapter, loraAdapterFinalizer)
	if err := r.Update(ctx, adapter); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// resolveInferenceService fetches the InferenceService named by the
// adapter's InferenceServiceRef, defaulting the namespace to the
// adapter's own. Returns the typed IsNotFound from k8s so callers can
// distinguish "create-loop because target gone" from a real fetch
// failure.
func (r *LoRAAdapterReconciler) resolveInferenceService(ctx context.Context, adapter *inferencev1alpha1.LoRAAdapter) (*inferencev1alpha1.InferenceService, error) {
	ns := adapter.Namespace
	if ref := adapter.Spec.InferenceServiceRef.Namespace; ref != nil {
		ns = *ref
	}
	isvc := &inferencev1alpha1.InferenceService{}
	if err := r.Get(ctx, types.NamespacedName{Name: adapter.Spec.InferenceServiceRef.Name, Namespace: ns}, isvc); err != nil {
		return nil, err
	}
	return isvc, nil
}

// shouldSkipLoad reports whether the reconciler can skip the HTTP
// load against SGLang. Returns true when the spec's Path matches the
// already-recorded LoadedPath AND the LastLoadedAt is within the
// safety window (loadSkipWindow). Path mismatch or stale load means
// we need to reload — adapter moved, or the previous load is too old
// to trust.
func (r *LoRAAdapterReconciler) shouldSkipLoad(adapter *inferencev1alpha1.LoRAAdapter) bool {
	if adapter.Status.LoadedPath == "" || adapter.Status.LastLoadedAt == nil {
		return false
	}
	if adapter.Status.LoadedPath != adapter.Spec.Path {
		return false
	}
	now := r.Now()
	if now.IsZero() {
		now = time.Now()
	}
	return now.Sub(adapter.Status.LastLoadedAt.Time) < loadSkipWindow
}

func (r *LoRAAdapterReconciler) addFinalizer(ctx context.Context, adapter *inferencev1alpha1.LoRAAdapter) error {
	controllerutil.AddFinalizer(adapter, loraAdapterFinalizer)
	// Persist the finalizer via spec/Update — status subresource
	// semantics mean r.Update leaves .status alone.
	if err := r.Update(ctx, adapter); err != nil {
		return fmt.Errorf("update spec with finalizer: %w", err)
	}
	r.setCondition(adapter, LoRAConditionAvailable, metav1.ConditionFalse,
		LoRAReasonFinalizerAdded, "first reconcile: adopted; waiting for load")
	r.setCondition(adapter, LoRAConditionLoaded, metav1.ConditionFalse,
		LoRAReasonFinalizerAdded, "first reconcile: not yet loaded")
	r.setCondition(adapter, LoRAConditionError, metav1.ConditionFalse,
		LoRAReasonFinalizerAdded, "no error")
	// Persist the seeded conditions via the status subresource.
	if err := r.Status().Update(ctx, adapter); err != nil {
		return fmt.Errorf("update status with seeded conditions: %w", err)
	}
	return nil
}

// setCondition is the single shape-mutator used by the reconciler. It
// is meta.SetStatusCondition wrapped so the rest of the package can
// stay decoupled from the metav1 helpers.
func (r *LoRAAdapterReconciler) setCondition(adapter *inferencev1alpha1.LoRAAdapter, t string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&adapter.Status.Conditions, metav1.Condition{
		Type:               t,
		Status:             status,
		ObservedGeneration: adapter.Generation,
		LastTransitionTime: metav1.NewTime(r.Now()),
		Reason:             reason,
		Message:            message,
	})
}

// ctrlLog aliases ctrl.Logger so the test file can stub it without
// importing controller-runtime internals.
type ctrlLog = ctrlLogger

// ctrlLogger is the subset of ctrl.Logger the reconciler actually uses.
// Defining an alias here keeps the package self-contained; the
// controller-runtime type satisfies it without an import.
type ctrlLogger interface {
	Error(err error, msg string, keysAndValues ...any)
	Info(msg string, keysAndValues ...any)
}
