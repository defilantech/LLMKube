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
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// readinessListenerArgs returns the values of every
// --readiness-required-listener in a container arg list, in order.
func readinessListenerArgs(t *testing.T, args []string) []string {
	t.Helper()
	const name = "--readiness-required-listener"
	var out []string
	for i, a := range args {
		if a != name {
			continue
		}
		if i+1 >= len(args) {
			t.Fatalf("%s is the last arg; args = %v", name, args)
		}
		out = append(out, args[i+1])
	}
	return out
}

// TestRouterDeploymentBuilderReadinessRequiredListeners pins what the field
// changes on the rendered Deployment: the readiness probe moves to /readyz on
// the same port and timings, liveness stays on /health, and each port reaches
// the proxy as its own flag. The unset shape is pinned byte for byte by the
// routerproxy-*.golden files.
func TestRouterDeploymentBuilderReadinessRequiredListeners(t *testing.T) {
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	base := r.newRouterDeployment(canonicalModelRouter(), "hash", false).Spec.Template.Spec.Containers[0]

	mr := canonicalModelRouter()
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{ReadinessRequiredListeners: []int32{15008, 15006}}
	got := r.newRouterDeployment(mr, "hash", false).Spec.Template.Spec.Containers[0]

	if got.ReadinessProbe == nil || got.ReadinessProbe.HTTPGet == nil {
		t.Fatalf("readiness probe = %+v, want an HTTP GET", got.ReadinessProbe)
	}
	if p := got.ReadinessProbe.HTTPGet.Path; p != "/readyz" {
		t.Errorf("readiness probe path = %q, want /readyz", p)
	}
	wantReady := base.ReadinessProbe.DeepCopy()
	wantReady.HTTPGet.Path = "/readyz"
	if !reflect.DeepEqual(got.ReadinessProbe, wantReady) {
		t.Errorf("readiness probe = %+v, want the default probe with only the path changed: %+v",
			got.ReadinessProbe, wantReady)
	}
	if p := got.LivenessProbe.HTTPGet.Path; p != "/health" {
		t.Errorf("liveness probe path = %q, want /health", p)
	}
	if !reflect.DeepEqual(got.LivenessProbe, base.LivenessProbe) {
		t.Errorf("liveness probe = %+v, want it unchanged: %+v", got.LivenessProbe, base.LivenessProbe)
	}
	if ports := readinessListenerArgs(t, got.Args); !slices.Equal(ports, []string{"15008", "15006"}) {
		t.Errorf("--readiness-required-listener values = %v, want [15008 15006]; args = %v", ports, got.Args)
	}
}

// TestRouterDeploymentBuilderReadinessRequiredListenersEmpty confirms that an
// empty list renders like an unset one, so neither rolls the proxy.
func TestRouterDeploymentBuilderReadinessRequiredListenersEmpty(t *testing.T) {
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	base := r.newRouterDeployment(canonicalModelRouter(), "hash", false)

	mr := canonicalModelRouter()
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{ReadinessRequiredListeners: []int32{}}
	got := r.newRouterDeployment(mr, "hash", false)

	if !reflect.DeepEqual(base.Spec.Template, got.Spec.Template) {
		t.Errorf("pod template differs between nil proxy and an empty readinessRequiredListeners")
	}
}

// TestModelRouterProxyReconcileReadinessRequiredListeners drives set and clear
// through the reconciler on a real API server, so the probe and flags reach
// the live Deployment and clearing the field puts readiness back on /health.
func TestModelRouterProxyReconcileReadinessRequiredListeners(t *testing.T) {
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
		ports     []int32
		wantPath  string
		wantPorts []string
	}{
		{name: "unset", wantPath: "/health"},
		{name: "set", ports: []int32{15008}, wantPath: "/readyz", wantPorts: []string{"15008"}},
		{name: "cleared", wantPath: "/health"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh := &inferencev1alpha1.ModelRouter{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
				t.Fatalf("get modelrouter: %v", err)
			}
			fresh.Spec.Proxy = nil
			if tc.ports != nil {
				fresh.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{ReadinessRequiredListeners: tc.ports}
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
			ctr := dep.Spec.Template.Spec.Containers[0]
			if p := probePath(ctr.ReadinessProbe); p != tc.wantPath {
				t.Errorf("live readiness probe path = %q, want %q", p, tc.wantPath)
			}
			if p := probePath(ctr.LivenessProbe); p != "/health" {
				t.Errorf("live liveness probe path = %q, want /health", p)
			}
			if ports := readinessListenerArgs(t, ctr.Args); !slices.Equal(ports, tc.wantPorts) {
				t.Errorf("live --readiness-required-listener values = %v, want %v", ports, tc.wantPorts)
			}
		})
	}
}

func probePath(p *corev1.Probe) string {
	if p == nil || p.HTTPGet == nil {
		return ""
	}
	return p.HTTPGet.Path
}

// TestModelRouterReadinessRequiredListenersBounds pins the CRD's own guard on
// the field, which holds with admission disabled: a port outside 1-65535 would
// otherwise reach the proxy flag and crash-loop the pod.
func TestModelRouterReadinessRequiredListenersBounds(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()

	for _, tc := range []struct {
		name    string
		ports   []int32
		wantErr string
	}{
		{name: "0", ports: []int32{0}, wantErr: "should be greater than or equal to 1"},
		{name: "65536", ports: []int32{65536}, wantErr: "should be less than or equal to 65535"},
		{name: "duplicate", ports: []int32{15008, 15008}, wantErr: "Duplicate value"},
		{name: "1", ports: []int32{1}},
		{name: "65535", ports: []int32{65535}},
		{name: "15008", ports: []int32{15008}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := canonicalModelRouter()
			mr.Namespace = newTestNamespace(t, c)
			mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{ReadinessRequiredListeners: tc.ports}
			err := c.Create(context.Background(), mr)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("create with readinessRequiredListeners %v: %v", tc.ports, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("create with readinessRequiredListeners %v succeeded, want a validation error", tc.ports)
			}
			if !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "readinessRequiredListeners") {
				t.Errorf("error = %v, want a readinessRequiredListeners error containing %q", err, tc.wantErr)
			}
		})
	}
}
