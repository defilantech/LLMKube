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
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The decorator wiring tests pin where the deadline is applied (#2069): a
// reconciler's ReconcileDecorator has to be applied inside its own
// SetupWithManager, at the builder's Complete site, because that argument is
// what the controller stores as its Reconciler. If the application is
// dropped, the controller calls the bare reconciler and the decorator is
// never invoked; every unit test of the decorator itself stays green, so
// these tests are the only guard on the wiring. Both wrapped controllers are
// pinned because the InferenceService controller is the one #2069 actually
// froze.

// startWiringManager starts a manager on the shared envtest for the decorator
// wiring tests. The returned stop function cancels the manager and waits for
// it to exit; the control plane itself outlives the test.
func startWiringManager(t *testing.T) (ctrl.Manager, func()) {
	t.Helper()
	_, cfg, _ := sharedTestEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(inferencev1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	select {
	case <-mgr.Elected():
	case err := <-done:
		t.Fatalf("manager exited during startup: %v", err)
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("manager did not start")
	}
	return mgr, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("manager exited with an error: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Errorf("manager did not stop")
		}
	}
}

// recordingDecorator returns a ReconcileDecorator that reports which request
// it was invoked for and short-circuits the inner reconciler: the inner's
// behavior is not the wiring test's subject, and short-circuiting keeps the
// test hermetic. It reports every namespace: a drop would fail the very
// assertion the hits channel feeds, so the buffered channel is sized far
// above the objects a run leaves on the shared server.
func recordingDecorator(hits chan reconcile.Request) func(reconcile.Reconciler) reconcile.Reconciler {
	return func(_ reconcile.Reconciler) reconcile.Reconciler {
		return reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			select {
			case hits <- req:
			default:
			}
			return reconcile.Result{}, nil
		})
	}
}

// expectDecoratorHit waits for the decorated controller to reconcile want. The
// manager watches its kind cluster-wide, so on a shared API server the startup
// LIST also enqueues earlier tests' objects; a bare hit would let one of those
// satisfy the assertion and stop it pinning that the object this test created
// reached the decorator at all.
func expectDecoratorHit(t *testing.T, hits chan reconcile.Request, want types.NamespacedName) {
	t.Helper()
	if !drainUntilWanted(hits, want, 10*time.Second) {
		t.Fatalf("the decorator was never invoked for %s: SetupWithManager did not apply it at the Complete site", want)
	}
}

// drainUntilWanted discards reconciles of other objects until one for want
// arrives, and reports whether it did before the timeout. It returns a bool
// rather than failing so a test can drive it from a goroutine: t.Fatalf must
// run on the test's own goroutine, and a failure raised after the test returns
// is silently dropped.
func drainUntilWanted(hits chan reconcile.Request, want types.NamespacedName, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case req := <-hits:
			if req.NamespacedName == want {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func TestReconcileDecoratorReachesTheController(t *testing.T) {
	mgr, stop := startWiringManager(t)
	defer stop()
	ns := testNS
	hits := make(chan reconcile.Request, 1024)
	rec := &ModelReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
		Recorder:  mgr.GetEventRecorder("decorator-wiring-test"),
	}
	rec.ReconcileDecorator = recordingDecorator(hits)
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup model controller: %v", err)
	}

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "decorator-wiring", Namespace: ns},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/model.gguf", Format: "gguf", Quantization: "Q4_K_M",
		},
	}
	if err := mgr.GetClient().Create(context.Background(), model); err != nil {
		t.Fatalf("create model: %v", err)
	}

	expectDecoratorHit(t, hits, types.NamespacedName{Namespace: ns, Name: model.Name})
}

func TestInferenceServiceDecoratorReachesTheController(t *testing.T) {
	mgr, stop := startWiringManager(t)
	defer stop()
	ns := testNS
	hits := make(chan reconcile.Request, 1024)
	rec := &InferenceServiceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("decorator-wiring-test"),
	}
	rec.ReconcileDecorator = recordingDecorator(hits)
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup inferenceservice controller: %v", err)
	}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "decorator-wiring", Namespace: ns},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "decorator-wiring-model"},
	}
	if err := mgr.GetClient().Create(context.Background(), isvc); err != nil {
		t.Fatalf("create inferenceservice: %v", err)
	}

	expectDecoratorHit(t, hits, types.NamespacedName{Namespace: ns, Name: isvc.Name})
}

// TestExpectDecoratorHitIgnoresAnotherObjectsReconcile pins the filter
// expectDecoratorHit exists for: on a shared API server the manager's startup
// LIST enqueues every object of that kind, so a bare hit would let an earlier
// test's object satisfy the assertion for this one. A foreign hit alone must
// leave the assertion waiting rather than return on it, which is what happens
// once the wanted request arrives.
func TestExpectDecoratorHitIgnoresAnotherObjectsReconcile(t *testing.T) {
	want := types.NamespacedName{Namespace: "llmkube-test-foreign", Name: "someone-elses-isvc"}
	hits := make(chan reconcile.Request, 2)
	hits <- reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "llmkube-test-earlier", Name: "absent-isvc"}}

	done := make(chan bool, 1)
	go func() {
		done <- drainUntilWanted(hits, want, 10*time.Second)
	}()

	select {
	case got := <-done:
		if got {
			t.Fatal("a foreign object's reconcile satisfied the assertion: the filter is gone")
		}
		t.Fatal("the drain gave up instead of waiting for the wanted request")
	case <-time.After(200 * time.Millisecond):
	}

	hits <- reconcile.Request{NamespacedName: want}
	select {
	case got := <-done:
		if !got {
			t.Fatal("the wanted request never satisfied the assertion")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wanted request never satisfied the assertion")
	}
}
