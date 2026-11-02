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

package watchcanary

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/metrics"
)

// informerFreezer builds the manager's informers with a watch it can silence:
// once frozen, events still arrive from the API server but never reach the
// informer's store. The reflector keeps running and the cache keeps serving
// reads, frozen at what it last delivered. That is the single-dead-informer
// shape of kubernetes-sigs/controller-runtime#3428.
type informerFreezer struct {
	frozen atomic.Bool
}

func (f *informerFreezer) newInformer(lw toolscache.ListerWatcher, obj runtime.Object, resync time.Duration, indexers toolscache.Indexers) toolscache.SharedIndexInformer {
	inner := toolscache.ToListerWatcherWithContext(lw)
	return toolscache.NewSharedIndexInformer(&toolscache.ListWatch{
		ListWithContextFunc: inner.ListWithContext,
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			w, err := inner.WatchWithContext(ctx, opts)
			if err != nil {
				return nil, err
			}
			return newGatedWatch(w, &f.frozen), nil
		},
	}, obj, resync, indexers)
}

// gatedWatch forwards events from inner until frozen, then drops them.
type gatedWatch struct {
	inner    watch.Interface
	out      chan watch.Event
	stopOnce sync.Once
	done     chan struct{}
}

func newGatedWatch(inner watch.Interface, frozen *atomic.Bool) *gatedWatch {
	g := &gatedWatch{inner: inner, out: make(chan watch.Event), done: make(chan struct{})}
	go func() {
		defer close(g.out)
		for ev := range inner.ResultChan() {
			if frozen.Load() {
				continue
			}
			select {
			case g.out <- ev:
			case <-g.done:
				return
			}
		}
	}()
	return g
}

func (g *gatedWatch) Stop() {
	g.stopOnce.Do(func() {
		close(g.done)
		g.inner.Stop()
	})
}

func (g *gatedWatch) ResultChan() <-chan watch.Event { return g.out }

// TestGenerationLagSeenPastADeadInformer is the story's acceptance case: the
// controllers reconciled an InferenceService and a Model, then both kinds'
// informers stopped delivering, and a spec change landed on each. The
// manager's cache still shows the reconciled generation, so only a checker
// reading through the API server sees the lag. It must report each kind once
// the lag outlasts the threshold, not before, and clear once a reconcile
// stamps the new generation.
//
// Falsification: make NewGenerationLagChecker read through the manager's
// cached client instead of its API reader. The frozen cache then shows both
// objects reconciled and the gauge stays 0.
func TestGenerationLagSeenPastADeadInformer(t *testing.T) {
	mgr, freezer, direct := startManagerWithFreezableInformers(t)
	ctx := context.Background()

	const threshold = 20 * time.Minute
	checker, err := NewGenerationLagChecker(mgr, GenerationLagOptions{
		Interval:  time.Minute,
		Threshold: threshold,
		Log:       logr.Discard(),
	})
	if err != nil {
		t.Fatalf("new checker: %v", err)
	}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "dead-informer-isvc"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "dead-informer-model"},
	}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "dead-informer-model"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
	}
	for _, obj := range []client.Object{isvc, model} {
		if err := direct.Create(ctx, obj); err != nil {
			t.Fatalf("create %s: %v", obj.GetName(), err)
		}
	}

	// "Reconcile" both at generation 1 the way the controllers do, then wait
	// for the informers to deliver it.
	stampReconciled(t, direct, isvc, model)
	cached := mgr.GetClient()
	waitFor(t, "the cache to hold both objects reconciled at generation 1", func() bool {
		ci, cm := &inferencev1alpha1.InferenceService{}, &inferencev1alpha1.Model{}
		return cached.Get(ctx, client.ObjectKeyFromObject(isvc), ci) == nil &&
			cached.Get(ctx, client.ObjectKeyFromObject(model), cm) == nil &&
			MaxObservedGeneration(ci.Status.Conditions) == 1 && MaxObservedGeneration(cm.Status.Conditions) == 1
	})

	// Both informers die; a spec change lands on each.
	freezer.frozen.Store(true)
	isvc.Spec.Replicas = ptr.To(int32(2))
	model.Spec.Quantization = "Q8_0"
	for _, obj := range []client.Object{isvc, model} {
		if err := direct.Update(ctx, obj); err != nil {
			t.Fatalf("bump %s spec: %v", obj.GetName(), err)
		}
		if obj.GetGeneration() != 2 {
			t.Fatalf("fixture: expected %s at generation 2, got %d", obj.GetName(), obj.GetGeneration())
		}
	}
	// The fixture holds: the cache is frozen at generation 1.
	time.Sleep(500 * time.Millisecond)
	frozenISVC := &inferencev1alpha1.InferenceService{}
	if err := cached.Get(ctx, client.ObjectKeyFromObject(isvc), frozenISVC); err != nil {
		t.Fatalf("cached get: %v", err)
	}
	if frozenISVC.Generation != 1 {
		t.Fatalf("fixture: the frozen cache moved to generation %d", frozenISVC.Generation)
	}

	t0 := time.Unix(1750000000, 0)
	assertLag := func(at time.Duration, wantObjects int, wantOldest time.Duration) {
		t.Helper()
		checker.check(ctx, t0.Add(at))
		for _, kind := range []string{KindInferenceService, KindModel} {
			if got := testutil.ToFloat64(metrics.UnreconciledGenerationObjects.WithLabelValues(kind)); got != float64(wantObjects) {
				t.Fatalf("at +%s: %s objects = %v, want %d", at, kind, got, wantObjects)
			}
			if got := testutil.ToFloat64(metrics.UnreconciledGenerationOldestSeconds.WithLabelValues(kind)); got != wantOldest.Seconds() {
				t.Fatalf("at +%s: %s oldest = %vs, want %s", at, kind, got, wantOldest)
			}
		}
	}

	assertLag(0, 0, 0)
	assertLag(threshold-time.Minute, 0, threshold-time.Minute)
	assertLag(threshold+time.Minute, 1, threshold+time.Minute)

	// A reconcile catches both up (a restarted operator, in practice).
	stampReconciled(t, direct, isvc, model)
	assertLag(threshold+2*time.Minute, 0, 0)
}

// startManagerWithFreezableInformers starts envtest with the LLMKube CRDs and
// a manager whose informers the returned freezer can silence, warms the
// InferenceService and Model informers the controllers would hold, and
// returns a direct client for writes. Everything stops at test cleanup.
func startManagerWithFreezableInformers(t *testing.T) (ctrl.Manager, *informerFreezer, client.Client) {
	t.Helper()
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	testEnv := &envtest.Environment{
		BinaryAssetsDirectory: firstEnvtestBinaryDir(),
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("stop envtest: %v", err)
		}
	})

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	freezer := &informerFreezer{}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{NewInformer: freezer.newInformer},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	// Registered after testEnv.Stop, so it runs first: the manager stops
	// before the API server it talks to.
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("manager exited with an error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	})
	select {
	case <-mgr.Elected():
	case err := <-done:
		t.Fatalf("manager exited during startup: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("manager did not start")
	}

	for _, obj := range []client.Object{&inferencev1alpha1.InferenceService{}, &inferencev1alpha1.Model{}} {
		if _, err := mgr.GetCache().GetInformer(ctx, obj); err != nil {
			t.Fatalf("get informer for %T: %v", obj, err)
		}
	}
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("new direct client: %v", err)
	}
	return mgr, freezer, direct
}

// stampReconciled re-reads each object and writes an Available condition at
// its current generation, the way a controller's status write does.
func stampReconciled(t *testing.T, c client.Client, objs ...client.Object) {
	t.Helper()
	ctx := context.Background()
	for _, obj := range objs {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("get %s: %v", obj.GetName(), err)
		}
		conds := []metav1.Condition{reconciledAt(obj.GetGeneration())}
		switch o := obj.(type) {
		case *inferencev1alpha1.InferenceService:
			o.Status.Conditions = conds
		case *inferencev1alpha1.Model:
			o.Status.Conditions = conds
		default:
			t.Fatalf("stampReconciled: unsupported %T", obj)
		}
		if err := c.Status().Update(ctx, obj); err != nil {
			t.Fatalf("stamp %s at generation %d: %v", obj.GetName(), obj.GetGeneration(), err)
		}
	}
}

func reconciledAt(gen int64) metav1.Condition {
	return metav1.Condition{
		Type:               "Available",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		ObservedGeneration: gen,
		LastTransitionTime: metav1.Now(),
	}
}
