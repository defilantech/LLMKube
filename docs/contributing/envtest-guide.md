# envtest Test-Craft Guide

This guide covers the recurring traps that make correct production logic fail
its test gate. Each section shows the wrong pattern and the blessed in-repo
pattern.

## 1. Status Is a Subresource

Populating `.status` inline in `k8sClient.Create()` is silently stripped by the
API server. Specs then assert against state that was never constructed and can
pass vacuously. This applies to Pods, InferenceServices, Workloads, and any
resource with a status subresource.

**Wrong:**

```go
model := &inferencev1alpha1.Model{
    ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
    Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/m.gguf"},
    Status:     inferencev1alpha1.ModelStatus{Phase: PhaseReady}, // silently dropped
}
Expect(k8sClient.Create(ctx, model)).To(Succeed())
// model.Status.Phase is now empty -- the API server stripped it
```

**Right:**

```go
model := &inferencev1alpha1.Model{
    ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
    Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/m.gguf"},
}
Expect(k8sClient.Create(ctx, model)).To(Succeed())
model.Status.Phase = PhaseReady
Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
// model.Status.Phase is now PhaseReady
```

This is the pattern used throughout `inferenceservice_storage_test.go` and
`drain_before_roll_test.go`.

## 2. Stub HTTP Probes

Controller paths that perform real HTTP calls (for example the rollout idle
probe) cannot resolve `*.svc.cluster.local` in envtest and fail closed. The
blessed pattern is `httptest.NewServer` plus the base-URL override.

**Wrong:**

```go
// The reconciler will try to reach http://my-isvc.default.svc.cluster.local/slots
// which envtest cannot resolve -- the probe silently fails closed
reconciler := &InferenceServiceReconciler{
    Client: k8sClient,
    Scheme: k8sClient.Scheme(),
}
```

**Right:**

```go
testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    if r.URL.Path == "/slots" {
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte(`[{"id":0,"is_processing":false}]`))
        return
    }
    w.WriteHeader(http.StatusNotFound)
}))
defer testServer.Close()

reconciler := &InferenceServiceReconciler{
    Client:             k8sClient,
    Scheme:             k8sClient.Scheme(),
    RolloutIdleBaseURL: testServer.URL, // override the real service URL
}
```

This is exactly how the busy-path specs in `drain_before_roll_test.go` are
written.

## 3. envtest Asset Hygiene

Three practical rules for running envtest locally:

- **Use an absolute `--bin-dir`** with `setup-envtest`. Relative paths break
  because the working directory can differ between invocations.
- **Killed test runs orphan `kube-apiserver` and `etcd` processes.** These
  leftover processes make later runs hang or fail. Before rerunning, execute:

  ```bash
  pkill -f 'bin/k8s'
  ```

- **`make test` wires `KUBEBUILDER_ASSETS` automatically**, so you do not need
  to set it manually when using the Makefile.

## 4. Mirror a Neighbor

Before writing a new spec, find the nearest existing spec exercising the same
controller path and mirror its fixture and reconcile-driving pattern. The two
files above (`drain_before_roll_test.go` and `inferenceservice_storage_test.go`)
are the usual starting points.

## Why This Exists

These traps cost real review cycles. The #1250 run (shipped in #1262) converged
on correct production logic but lost a green build to fixture craft: status
fields stripped by the API server and an unstubbed HTTP probe. The same traps
affect human first-time contributors. This guide captures the knowledge that
previously lived only in scattered existing tests and maintainers' heads.

See issue #1263 for the background.
