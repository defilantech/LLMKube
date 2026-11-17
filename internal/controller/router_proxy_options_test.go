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
	"net"
	"reflect"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// allocatedNodePort stands in for the port the apiserver allocated.
const allocatedNodePort int32 = 31555

// routerWithEndpoint returns the canonical router with spec.endpoint set.
func routerWithEndpoint(ep *inferencev1alpha1.EndpointSpec) *inferencev1alpha1.ModelRouter {
	mr := canonicalModelRouter()
	mr.Spec.Endpoint = ep
	return mr
}

// TestRouterServiceNodePort pins #2175: spec.endpoint.nodePort reaches the
// data-plane Service port on a NodePort router and is 0 (let Kubernetes
// allocate) when unset.
func TestRouterServiceNodePort(t *testing.T) {
	svc := newRouterService(routerWithEndpoint(&inferencev1alpha1.EndpointSpec{
		Type: "NodePort", NodePort: ptr.To[int32](30801),
	}))
	if got := svc.Spec.Ports[0].NodePort; got != 30801 {
		t.Errorf("NodePort = %d, want 30801", got)
	}

	svc = newRouterService(routerWithEndpoint(&inferencev1alpha1.EndpointSpec{Type: "NodePort"}))
	if got := svc.Spec.Ports[0].NodePort; got != 0 {
		t.Errorf("NodePort = %d, want 0 when spec.endpoint.nodePort is unset", got)
	}
}

// TestRouterServiceExternalTrafficPolicy pins #2176 on the blueprint.
func TestRouterServiceExternalTrafficPolicy(t *testing.T) {
	for _, typ := range []string{"NodePort", "LoadBalancer"} {
		svc := newRouterService(routerWithEndpoint(&inferencev1alpha1.EndpointSpec{
			Type: typ, ExternalTrafficPolicy: "Local",
		}))
		if got := svc.Spec.ExternalTrafficPolicy; got != corev1.ServiceExternalTrafficPolicyLocal {
			t.Errorf("type %s: ExternalTrafficPolicy = %q, want Local", typ, got)
		}
	}
	svc := newRouterService(routerWithEndpoint(&inferencev1alpha1.EndpointSpec{Type: "NodePort"}))
	if got := svc.Spec.ExternalTrafficPolicy; got != "" {
		t.Errorf("unset: ExternalTrafficPolicy = %q, want empty", got)
	}
	// A ClusterIP Service must never carry the field (the API rejects it).
	svc = newRouterService(canonicalModelRouter())
	if got := svc.Spec.ExternalTrafficPolicy; got != "" {
		t.Errorf("ClusterIP: ExternalTrafficPolicy = %q, want empty", got)
	}
}

// seededRouterService creates the data-plane Service through the reconciler
// the way a first reconcile would, then simulates the apiserver allocating a
// node port, and returns the reconciler and the live Service key.
func seededRouterService(t *testing.T, mr *inferencev1alpha1.ModelRouter) (*ModelRouterReconciler, client.Client, types.NamespacedName) {
	t.Helper()
	scheme := builderTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr).Build()
	rec := &ModelRouterReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()
	if err := rec.reconcileServiceObject(ctx, mr, newRouterService(mr)); err != nil {
		t.Fatalf("create Service: %v", err)
	}
	key := types.NamespacedName{Name: routerProxyResourceName(mr.Name), Namespace: mr.Namespace}
	live := &corev1.Service{}
	if err := c.Get(ctx, key, live); err != nil {
		t.Fatal(err)
	}
	live.Spec.Ports[0].NodePort = allocatedNodePort
	if err := c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	return rec, c, key
}

func reconcileAndGetService(t *testing.T, rec *ModelRouterReconciler, c client.Client, mr *inferencev1alpha1.ModelRouter, key types.NamespacedName) *corev1.Service {
	t.Helper()
	ctx := context.Background()
	if err := rec.reconcileServiceObject(ctx, mr, newRouterService(mr)); err != nil {
		t.Fatalf("update Service: %v", err)
	}
	got := &corev1.Service{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestReconcileRouterServicePreservesAllocatedNodePort pins #2175 (b): with
// spec.endpoint.nodePort unset, an update must not send nodePort 0 and let
// the apiserver reassign the port it already allocated.
func TestReconcileRouterServicePreservesAllocatedNodePort(t *testing.T) {
	mr := routerWithEndpoint(&inferencev1alpha1.EndpointSpec{Type: "NodePort"})
	rec, c, key := seededRouterService(t, mr)
	got := reconcileAndGetService(t, rec, c, mr, key)
	if np := got.Spec.Ports[0].NodePort; np != allocatedNodePort {
		t.Errorf("NodePort after update = %d, want the allocated %d preserved", np, allocatedNodePort)
	}
}

// TestReconcileRouterServiceAppliesChangedNodePort pins #2175 (a).
func TestReconcileRouterServiceAppliesChangedNodePort(t *testing.T) {
	mr := routerWithEndpoint(&inferencev1alpha1.EndpointSpec{Type: "NodePort"})
	rec, c, key := seededRouterService(t, mr)
	mr.Spec.Endpoint.NodePort = ptr.To[int32](30801)
	got := reconcileAndGetService(t, rec, c, mr, key)
	if np := got.Spec.Ports[0].NodePort; np != 30801 {
		t.Errorf("NodePort after update = %d, want 30801", np)
	}
}

// TestReconcileRouterServiceDropsNodePortOnClusterIP: switching the router
// back to ClusterIP must not carry a stale node port (the API rejects it).
func TestReconcileRouterServiceDropsNodePortOnClusterIP(t *testing.T) {
	mr := routerWithEndpoint(&inferencev1alpha1.EndpointSpec{Type: "NodePort"})
	rec, c, key := seededRouterService(t, mr)
	mr.Spec.Endpoint.Type = "ClusterIP"
	got := reconcileAndGetService(t, rec, c, mr, key)
	if np := got.Spec.Ports[0].NodePort; np != 0 {
		t.Errorf("NodePort after switch to ClusterIP = %d, want 0", np)
	}
}

// TestReconcileRouterServiceAppliesExternalTrafficPolicy pins #2176 on the
// update path: set, and removed again.
func TestReconcileRouterServiceAppliesExternalTrafficPolicy(t *testing.T) {
	mr := routerWithEndpoint(&inferencev1alpha1.EndpointSpec{Type: "NodePort"})
	rec, c, key := seededRouterService(t, mr)

	mr.Spec.Endpoint.ExternalTrafficPolicy = "Local"
	got := reconcileAndGetService(t, rec, c, mr, key)
	if p := got.Spec.ExternalTrafficPolicy; p != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("ExternalTrafficPolicy after update = %q, want Local", p)
	}

	mr.Spec.Endpoint.ExternalTrafficPolicy = ""
	got = reconcileAndGetService(t, rec, c, mr, key)
	if p := got.Spec.ExternalTrafficPolicy; p != "" {
		t.Errorf("ExternalTrafficPolicy after removal = %q, want empty (apiserver re-defaults to Cluster)", p)
	}
}

// TestRouterDeploymentBuilderRuntimeClassName pins #2156: the field reaches
// the pod spec when set.
func TestRouterDeploymentBuilderRuntimeClassName(t *testing.T) {
	mr := canonicalModelRouter()
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{RuntimeClassName: ptr.To("gvisor")}
	r := &ModelRouterReconciler{RouterProxyImage: "img"}
	got := r.newRouterDeployment(mr, &compiledConfig{Hash: "hash"}).Spec.Template.Spec.RuntimeClassName
	if got == nil || *got != "gvisor" {
		t.Errorf("pod RuntimeClassName = %v, want gvisor", got)
	}
}

// TestRouterDeploymentBuilderRuntimeClassNameUnset checks only that the field
// is nil when unset, and that a proxy block without the field produces the same
// pod template as no proxy block at all.
func TestRouterDeploymentBuilderRuntimeClassNameUnset(t *testing.T) {
	r := &ModelRouterReconciler{RouterProxyImage: "img"}
	base := r.newRouterDeployment(canonicalModelRouter(), &compiledConfig{Hash: "hash"})

	withProxy := canonicalModelRouter()
	withProxy.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{Replicas: ptr.To[int32](1)}
	got := r.newRouterDeployment(withProxy, &compiledConfig{Hash: "hash"})

	if base.Spec.Template.Spec.RuntimeClassName != nil || got.Spec.Template.Spec.RuntimeClassName != nil {
		t.Fatalf("RuntimeClassName = %v / %v, want nil when unset",
			base.Spec.Template.Spec.RuntimeClassName, got.Spec.Template.Spec.RuntimeClassName)
	}
	if !reflect.DeepEqual(base.Spec.Template, got.Spec.Template) {
		t.Errorf("pod template differs between nil proxy and proxy without runtimeClassName")
	}
}

// TestReconcileRouterDeploymentAppliesRuntimeClassName: a set and a changed
// value reach the live Deployment through the update path.
func TestReconcileRouterDeploymentAppliesRuntimeClassName(t *testing.T) {
	mr := canonicalModelRouter()
	scheme := builderTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr).Build()
	rec := &ModelRouterReconciler{Client: c, Scheme: scheme, RouterProxyImage: "img"}
	ctx := context.Background()
	key := types.NamespacedName{Name: routerProxyResourceName(mr.Name), Namespace: mr.Namespace}

	if err := rec.reconcileRouterDeployment(ctx, mr, &compiledConfig{Hash: "hash"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gvisor", "kata"} {
		mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{RuntimeClassName: ptr.To(want)}
		if err := rec.reconcileRouterDeployment(ctx, mr, &compiledConfig{Hash: "hash"}); err != nil {
			t.Fatal(err)
		}
		dep := &appsv1.Deployment{}
		if err := c.Get(ctx, key, dep); err != nil {
			t.Fatal(err)
		}
		got := dep.Spec.Template.Spec.RuntimeClassName
		if got == nil || *got != want {
			t.Errorf("live RuntimeClassName = %v, want %s", got, want)
		}
	}

	mr.Spec.Proxy.RuntimeClassName = nil
	if err := rec.reconcileRouterDeployment(ctx, mr, &compiledConfig{Hash: "hash"}); err != nil {
		t.Fatal(err)
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, key, dep); err != nil {
		t.Fatal(err)
	}
	if got := dep.Spec.Template.Spec.RuntimeClassName; got != nil {
		t.Errorf("live RuntimeClassName after removal = %q, want nil", *got)
	}
}

// proxyListenPort is the port the router-proxy container is told to listen on:
// the value of its --listen argument in the Deployment the controller builds.
func proxyListenPort(t *testing.T, mr *inferencev1alpha1.ModelRouter) int {
	t.Helper()
	dep := (&ModelRouterReconciler{RouterProxyImage: "img"}).newRouterDeployment(mr, &compiledConfig{Hash: "hash"})
	args := dep.Spec.Template.Spec.Containers[0].Args
	for i, a := range args {
		if a == "--listen" && i+1 < len(args) {
			_, port, err := net.SplitHostPort(args[i+1])
			if err != nil {
				t.Fatalf("--listen %q: %v", args[i+1], err)
			}
			n, err := strconv.Atoi(port)
			if err != nil {
				t.Fatalf("--listen port %q: %v", port, err)
			}
			return n
		}
	}
	t.Fatalf("router-proxy container has no --listen argument: %q", args)
	return 0
}

// TestRouterServiceTargetsTheProxyListenPort pins #2278: spec.endpoint.port
// sets the Service port, and the Service always forwards to the port the
// proxy listens on. Before, targetPort followed spec.endpoint.port, so any
// value but 8080 pointed the Service at a port nothing listened on.
func TestRouterServiceTargetsTheProxyListenPort(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint *inferencev1alpha1.EndpointSpec
		wantPort int32
	}{
		{"no endpoint", nil, routerProxyPort},
		{"port 80", &inferencev1alpha1.EndpointSpec{Port: 80}, 80},
		{"port 8080", &inferencev1alpha1.EndpointSpec{Port: 8080}, 8080},
		{"NodePort on 9000", &inferencev1alpha1.EndpointSpec{Port: 9000, Type: "NodePort"}, 9000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := routerWithEndpoint(tc.endpoint)
			p := newRouterService(mr).Spec.Ports[0]
			if p.Port != tc.wantPort {
				t.Errorf("Service port = %d, want %d", p.Port, tc.wantPort)
			}
			if listen := proxyListenPort(t, mr); p.TargetPort.IntValue() != listen {
				t.Errorf("Service targetPort = %s, but the proxy listens on %d", p.TargetPort.String(), listen)
			}
		})
	}
}

// TestReconcileRouterServiceRepointsTargetPort: a Service an older operator
// built with targetPort equal to spec.endpoint.port is corrected on the next
// reconcile, so upgrading fixes a router that was unreachable.
func TestReconcileRouterServiceRepointsTargetPort(t *testing.T) {
	mr := routerWithEndpoint(&inferencev1alpha1.EndpointSpec{Port: 80})
	rec, c, key := seededRouterService(t, mr)
	live := &corev1.Service{}
	if err := c.Get(context.Background(), key, live); err != nil {
		t.Fatal(err)
	}
	live.Spec.Ports[0].TargetPort = intstr.FromInt(80)
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	got := reconcileAndGetService(t, rec, c, mr, key)
	if p := got.Spec.Ports[0]; p.Port != 80 || p.TargetPort.IntValue() != proxyListenPort(t, mr) {
		t.Errorf("after reconcile: port %d -> targetPort %s, want 80 -> %d", p.Port, p.TargetPort.String(), proxyListenPort(t, mr))
	}
}
