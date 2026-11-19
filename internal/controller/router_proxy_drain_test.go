/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// proxyDrainTestRouter returns the canonical router with spec.proxy.drainTimeout
// set to d.
func proxyDrainTestRouter(d time.Duration) *inferencev1alpha1.ModelRouter {
	mr := canonicalModelRouter()
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{
		DrainTimeout: &metav1.Duration{Duration: d},
	}
	return mr
}

// shutdownTimeoutArg returns the value of --shutdown-timeout in a container
// arg list, and whether the flag is present at all.
func shutdownTimeoutArg(t *testing.T, args []string) (string, bool) {
	t.Helper()
	const name = "--shutdown-timeout"
	for i, a := range args {
		if a != name {
			continue
		}
		if i+1 >= len(args) {
			t.Fatalf("%s is the last arg; args = %v", name, args)
		}
		return args[i+1], true
	}
	return "", false
}

// graceSecondsString renders a grace period pointer for an assertion message.
func graceSecondsString(p *int64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatInt(*p, 10)
}

// The goldens are generated on the branch's base commit and committed
// unchanged: the unset-drain pod template must stay byte-identical, so an
// operator upgrade does not roll existing proxies. Regenerate with
// -update-routerproxy-goldens only when a change to that shape is intended,
// and move the generated files back from the base tree, not from here.
var updateRouterProxyGoldens = flag.Bool("update-routerproxy-goldens", false,
	"rewrite the router-proxy Deployment golden files")

// TestRouterProxyDeploymentGolden pins every byte of the rendered proxy
// Deployment for a router that sets no spec.proxy.drainTimeout, and for one
// that sets every other proxy option. Both goldens were generated on the base
// commit, so a drift here is a pod-template change that would roll an existing
// proxy on an operator upgrade.
func TestRouterProxyDeploymentGolden(t *testing.T) {
	withOptions := canonicalModelRouter()
	withOptions.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{
		Replicas:              ptr.To[int32](2),
		RevisionHistoryLimit:  ptr.To[int32](1),
		QuarantineDuration:    &metav1.Duration{Duration: 15 * time.Second},
		ResponseHeaderTimeout: &metav1.Duration{Duration: 2 * time.Minute},
		StreamIdleTimeout:     &metav1.Duration{Duration: 5 * time.Minute},
		NodeSelector:          map[string]string{"kubernetes.io/hostname": "edge-1"},
		PodLabels:             map[string]string{"example.com/team": "llm"},
		PodAnnotations:        map[string]string{"example.com/tier": "edge"},
	}

	for _, tc := range []struct {
		name     string
		mr       *inferencev1alpha1.ModelRouter
		hasPools bool
	}{
		{name: "routerproxy-deployment.golden", mr: canonicalModelRouter()},
		{name: "routerproxy-deployment-proxy-options.golden", mr: withOptions, hasPools: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
			dep := r.newRouterDeployment(tc.mr, &compiledConfig{Hash: "deadbeef", HasPools: tc.hasPools})
			// The namespace is per-test on the envtest helpers and fixed on this
			// fixture; render it as a constant so the golden pins the shape.
			dep.Namespace = "router-builder-test"
			got, err := yaml.Marshal(dep)
			if err != nil {
				t.Fatalf("marshal Deployment: %v", err)
			}

			path := filepath.Join("testdata", tc.name)
			if *updateRouterProxyGoldens {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write golden %s: %v", tc.name, err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v", tc.name, err)
			}
			if string(got) != string(want) {
				t.Errorf("%s drifted from the golden; an operator upgrade would roll existing proxies:\ngot:\n%s\nwanted:\n%s",
					tc.name, got, want)
			}
		})
	}
}

// TestRouterDeploymentBuilderDrainTimeout covers the
// spec.proxy.drainTimeout plumbing: the value reaches the proxy as
// --shutdown-timeout and the pod's grace period as drain plus the fixed
// margin, so the kubelet does not SIGKILL mid-drain.
func TestRouterDeploymentBuilderDrainTimeout(t *testing.T) {
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	spec := r.newRouterDeployment(proxyDrainTestRouter(10*time.Minute), &compiledConfig{Hash: "hash"}).Spec.Template.Spec

	got, ok := shutdownTimeoutArg(t, spec.Containers[0].Args)
	if !ok {
		t.Fatalf("--shutdown-timeout flag not rendered; args = %v", spec.Containers[0].Args)
	}
	if got != "10m0s" {
		t.Errorf("--shutdown-timeout = %q, want 10m0s", got)
	}
	if spec.TerminationGracePeriodSeconds == nil {
		t.Fatalf("terminationGracePeriodSeconds = nil, want 605")
	}
	if *spec.TerminationGracePeriodSeconds != 605 {
		t.Errorf("terminationGracePeriodSeconds = %d, want 605 (10m drain + 5s margin)",
			*spec.TerminationGracePeriodSeconds)
	}
}

// TestRouterDeploymentBuilderDrainTimeoutShort pins that a drain shorter than
// the cluster's 30s default produces a proportionally shorter grace period
// rather than a floor at the default: the drain is the real bound.
func TestRouterDeploymentBuilderDrainTimeoutShort(t *testing.T) {
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	spec := r.newRouterDeployment(proxyDrainTestRouter(5*time.Second), &compiledConfig{Hash: "hash"}).Spec.Template.Spec

	got, ok := shutdownTimeoutArg(t, spec.Containers[0].Args)
	if !ok || got != "5s" {
		t.Fatalf("--shutdown-timeout = %q (present %v), want 5s", got, ok)
	}
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != 10 {
		t.Errorf("terminationGracePeriodSeconds = %s, want 10 (5s drain + 5s margin)",
			graceSecondsString(spec.TerminationGracePeriodSeconds))
	}
}

// TestRouterDeploymentBuilderDrainTimeoutFractional pins the round-up: a
// fractional drain must never truncate the grace period below the drain.
func TestRouterDeploymentBuilderDrainTimeoutFractional(t *testing.T) {
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	spec := r.newRouterDeployment(proxyDrainTestRouter(1500*time.Millisecond), &compiledConfig{Hash: "hash"}).Spec.Template.Spec

	got, ok := shutdownTimeoutArg(t, spec.Containers[0].Args)
	if !ok || got != "1.5s" {
		t.Fatalf("--shutdown-timeout = %q (present %v), want 1.5s", got, ok)
	}
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != 7 {
		t.Errorf("terminationGracePeriodSeconds = %s, want 7 (1.5s drain + 5s margin, rounded up)",
			graceSecondsString(spec.TerminationGracePeriodSeconds))
	}
}

// TestRouterDeploymentBuilderDrainTimeoutUnset confirms the flag is omitted
// and the grace period left to the API server's 30s default when the user did
// NOT set spec.proxy.drainTimeout, so existing proxies keep their pod template.
func TestRouterDeploymentBuilderDrainTimeoutUnset(t *testing.T) {
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	base := r.newRouterDeployment(canonicalModelRouter(), &compiledConfig{Hash: "hash"})

	withProxy := canonicalModelRouter()
	withProxy.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{Replicas: ptr.To[int32](1)}
	got := r.newRouterDeployment(withProxy, &compiledConfig{Hash: "hash"})

	for _, dep := range []*appsv1.Deployment{base, got} {
		spec := dep.Spec.Template.Spec
		if _, ok := shutdownTimeoutArg(t, spec.Containers[0].Args); ok {
			t.Errorf("--shutdown-timeout must be omitted when unset; args = %v", spec.Containers[0].Args)
		}
		if spec.TerminationGracePeriodSeconds != nil {
			t.Errorf("terminationGracePeriodSeconds = %d, want nil when unset",
				*spec.TerminationGracePeriodSeconds)
		}
	}
	if !reflect.DeepEqual(base.Spec.Template, got.Spec.Template) {
		t.Errorf("pod template differs between nil proxy and proxy without drainTimeout")
	}
}

// TestModelRouterProxyUpgradeKeepsUnsetDrain is the upgrade path through a
// real API server and the reconciler entry point: a proxy Deployment written
// by the base commit (the golden, unchanged except for its namespace and the
// owner reference the operator sets) must survive a reconcile of a router that
// sets no drainTimeout without the write that rolls the pods. It asserts the
// drain observables rather than the whole template or metadata.generation:
// the config-hash annotation is hash-derived and rewritten on every reconcile,
// so a generation check would test the harness. Byte identity for a fixed hash
// is TestRouterProxyDeploymentGolden's job.
func TestModelRouterProxyUpgradeKeepsUnsetDrain(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	raw, err := os.ReadFile(filepath.Join("testdata", "routerproxy-deployment.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	base := &appsv1.Deployment{}
	if err := yaml.Unmarshal(raw, base); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}

	mr := externalProxyRouter(ns, "coding-router", inferencev1alpha1.ModelRouterDataPlaneProxy)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	// A Deployment written by the previous release carries the owner reference
	// the operator set, so build it the way the base reconcile would have.
	stored := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: base.Name, Namespace: ns, Labels: base.Labels},
		Spec:       base.Spec,
	}
	if err := setControllerReferenceUnblocked(mr, stored, scheme.Scheme); err != nil {
		t.Fatalf("set owner reference on base Deployment: %v", err)
	}
	if err := c.Create(context.Background(), stored); err != nil {
		t.Fatalf("create base proxy Deployment: %v", err)
	}
	if got := stored.Spec.Template.Spec.TerminationGracePeriodSeconds; got == nil || *got != 30 {
		t.Fatalf("stored terminationGracePeriodSeconds = %v, want the API server's 30", got)
	}

	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	key := types.NamespacedName{Name: routerProxyResourceName(mr.Name), Namespace: ns}
	after := &appsv1.Deployment{}
	if err := c.Get(context.Background(), key, after); err != nil {
		t.Fatalf("get proxy Deployment after reconcile: %v", err)
	}
	// The hash annotation is written only by reconcileRouterDeployment, so a
	// reconcile that skipped the Deployment leaves the golden's value and the
	// drain assertions below would test nothing.
	if got := after.Spec.Template.Annotations[routerProxyConfigHashAnnotation]; got == "" || got == "deadbeef" {
		t.Fatalf("config-hash annotation = %q, want the reconcile's own hash; the Deployment was not re-applied", got)
	}
	if got := after.Spec.Template.Spec.TerminationGracePeriodSeconds; got == nil || *got != 30 {
		t.Errorf("grace after an upgrade with no drainTimeout = %s, want 30",
			graceSecondsString(got))
	}
	if _, ok := shutdownTimeoutArg(t, after.Spec.Template.Spec.Containers[0].Args); ok {
		t.Errorf("reconcile rendered --shutdown-timeout for a router that sets no drainTimeout")
	}
}

// TestModelRouterProxyReconcileDrainTimeout drives set, change and clear
// through the reconciler entry point on a real API server, so the value
// reaches the live Deployment the way an operator's apply does and a cleared
// field returns to the API server's 30s default.
func TestModelRouterProxyReconcileDrainTimeout(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	mr := externalProxyRouter(ns, "coding-router", inferencev1alpha1.ModelRouterDataPlaneProxy)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	key := types.NamespacedName{Name: routerProxyResourceName(mr.Name), Namespace: ns}

	for _, tc := range []struct {
		name      string
		drain     *time.Duration
		wantArg   string
		wantGrace int64
	}{
		{name: "unset", wantGrace: 30},
		{name: "10m", drain: ptr.To(10 * time.Minute), wantArg: "10m0s", wantGrace: 605},
		{name: "2m", drain: ptr.To(2 * time.Minute), wantArg: "2m0s", wantGrace: 125},
		{name: "cleared", wantGrace: 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh := &inferencev1alpha1.ModelRouter{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
				t.Fatalf("get modelrouter: %v", err)
			}
			if tc.drain == nil {
				fresh.Spec.Proxy = nil
			} else {
				fresh.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{
					DrainTimeout: &metav1.Duration{Duration: *tc.drain},
				}
			}
			if err := c.Update(context.Background(), fresh); err != nil {
				t.Fatalf("update modelrouter: %v", err)
			}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
			}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			dep := &appsv1.Deployment{}
			if err := c.Get(context.Background(), key, dep); err != nil {
				t.Fatalf("get proxy Deployment: %v", err)
			}
			spec := dep.Spec.Template.Spec
			got, ok := shutdownTimeoutArg(t, spec.Containers[0].Args)
			switch {
			case tc.wantArg == "" && ok:
				t.Errorf("--shutdown-timeout = %q, want the flag omitted", got)
			case tc.wantArg != "" && (!ok || got != tc.wantArg):
				t.Errorf("--shutdown-timeout = %q (present %v), want %s", got, ok, tc.wantArg)
			}
			if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != tc.wantGrace {
				t.Errorf("grace = %s, want %d", graceSecondsString(spec.TerminationGracePeriodSeconds), tc.wantGrace)
			}
		})
	}
}

// TestModelRouterDrainTimeoutBounds pins the CRD's own guard on the field. The
// bound is CEL, not the webhook, so it holds even with admission disabled: a
// negative value would make the pod's grace period invalid and fail every
// reconcile.
func TestModelRouterDrainTimeoutBounds(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()

	for _, tc := range []struct {
		name    string
		d       time.Duration
		wantErr bool
	}{
		{name: "below 1s", d: 500 * time.Millisecond, wantErr: true},
		{name: "above 1h", d: 61 * time.Minute, wantErr: true},
		{name: "negative", d: -time.Minute, wantErr: true},
		{name: "1s", d: time.Second},
		{name: "1h", d: time.Hour},
		{name: "10m", d: 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := canonicalModelRouter()
			mr.Namespace = newTestNamespace(t, c)
			mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{
				DrainTimeout: &metav1.Duration{Duration: tc.d},
			}
			err := c.Create(context.Background(), mr)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("create with drainTimeout %s: %v", tc.d, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("create with drainTimeout %s succeeded, want a validation error", tc.d)
			}
			if !strings.Contains(err.Error(), "drainTimeout must be between 1s and 1h") {
				t.Errorf("error = %v, want the drainTimeout bounds message", err)
			}
		})
	}
}

// TestRouterProxyDrainMarginDocumented pins the margin's one source of truth:
// routerProxyDrainGraceMargin. The plan says the margin is fixed, and the
// number appears in prose a user reads, so editing the constant must fail here
// until the CRD description, the concepts doc and the sample move with it.
func TestRouterProxyDrainMarginDocumented(t *testing.T) {
	phrase := fmt.Sprintf("%ds margin", int(routerProxyDrainGraceMargin.Seconds()))
	for _, path := range []string{
		filepath.Join("../../config/crd/bases/inference.llmkube.dev_modelrouters.yaml"),
		filepath.Join("../../docs/site/concepts/model-router.md"),
		filepath.Join("../../config/samples/inference_v1alpha1_modelrouter.yaml"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(data), phrase) {
			t.Errorf("%s does not mention %q; update the prose with routerProxyDrainGraceMargin", path, phrase)
		}
	}
}
