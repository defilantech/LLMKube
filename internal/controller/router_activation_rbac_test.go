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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func activationRBACScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		inferencev1alpha1.AddToScheme, corev1.AddToScheme, rbacv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}
	return s
}

// TestReconcileRouterActivationRBACNoPools verifies the unpooled path is a
// no-op: a router with no ModelPool members needs no API access, so no
// ServiceAccount/Role/RoleBinding is provisioned.
func TestReconcileRouterActivationRBACNoPools(t *testing.T) {
	scheme := activationRBACScheme(t)
	mr := &inferencev1alpha1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns", UID: "uid-1"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr).Build()
	r := &ModelRouterReconciler{Client: c, Scheme: scheme}

	if err := r.reconcileRouterActivationRBAC(context.Background(), mr, &compiledConfig{}); err != nil {
		t.Fatalf("reconcile (no pools): %v", err)
	}
	name := routerProxyResourceName(mr.Name)
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: mr.Namespace}, &corev1.ServiceAccount{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("ServiceAccount get err = %v, want NotFound (no RBAC when unpooled)", err)
	}
}

// TestReconcileRouterActivationRBACProvisions verifies that a pooled router gets
// a ServiceAccount, a Role granting the least-privilege InferenceService verbs,
// and a RoleBinding tying them together, all owner-referenced to the ModelRouter
// and idempotent across reconciles.
func TestReconcileRouterActivationRBACProvisions(t *testing.T) {
	scheme := activationRBACScheme(t)
	mr := &inferencev1alpha1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns", UID: "uid-1"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr).Build()
	r := &ModelRouterReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()
	name := routerProxyResourceName(mr.Name)

	if err := r.reconcileRouterActivationRBAC(ctx, mr, &compiledConfig{HasPools: true}); err != nil {
		t.Fatalf("reconcile (pools): %v", err)
	}

	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: mr.Namespace}, sa); err != nil {
		t.Fatalf("ServiceAccount not created: %v", err)
	}
	if len(sa.OwnerReferences) == 0 || sa.OwnerReferences[0].Name != mr.Name {
		t.Errorf("ServiceAccount owner refs = %v, want owned by %s", sa.OwnerReferences, mr.Name)
	}

	role := &rbacv1.Role{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: mr.Namespace}, role); err != nil {
		t.Fatalf("Role not created: %v", err)
	}
	if !roleGrantsISVCWrite(role) {
		t.Errorf("Role rules = %+v, want get/list/watch/update/patch on inferenceservices", role.Rules)
	}

	binding := &rbacv1.RoleBinding{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: mr.Namespace}, binding); err != nil {
		t.Fatalf("RoleBinding not created: %v", err)
	}
	if binding.RoleRef.Name != name || len(binding.Subjects) != 1 || binding.Subjects[0].Name != name {
		t.Errorf("RoleBinding ref/subjects mismatch: ref=%s subjects=%v", binding.RoleRef.Name, binding.Subjects)
	}

	// Second reconcile must not error (updates the existing Role/RoleBinding).
	if err := r.reconcileRouterActivationRBAC(ctx, mr, &compiledConfig{HasPools: true}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	roles := &rbacv1.RoleList{}
	if err := c.List(ctx, roles); err != nil {
		t.Fatalf("list roles: %v", err)
	}
	if len(roles.Items) != 1 {
		t.Errorf("Role count after two reconciles = %d, want 1 (idempotent)", len(roles.Items))
	}
}

func roleGrantsISVCWrite(role *rbacv1.Role) bool {
	for _, rule := range role.Rules {
		hasISVC := false
		for _, res := range rule.Resources {
			if res == "inferenceservices" {
				hasISVC = true
			}
		}
		if !hasISVC {
			continue
		}
		verbs := map[string]bool{}
		for _, v := range rule.Verbs {
			verbs[v] = true
		}
		if verbs["get"] && verbs["list"] && verbs["watch"] && verbs["update"] && verbs["patch"] {
			return true
		}
	}
	return false
}

// TestReconcileRouterActivationRBACEndpointWatch: a router with an endpoint
// backend and no pools gets the ServiceAccount and a Role that can read
// EndpointSlices and nothing else; a pooled router with an endpoint backend
// gets both sets of rules in its one Role.
func TestReconcileRouterActivationRBACEndpointWatch(t *testing.T) {
	scheme := activationRBACScheme(t)
	ctx := context.Background()
	endpointRouter := func(name string) *inferencev1alpha1.ModelRouter {
		return &inferencev1alpha1.ModelRouter{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID("uid-" + name)},
			Spec: inferencev1alpha1.ModelRouterSpec{Backends: []inferencev1alpha1.RouterBackend{{
				Name: "q", InferenceServiceRef: &corev1.LocalObjectReference{Name: "q"},
				Resolution: inferencev1alpha1.RouterBackendResolutionEndpoint,
			}}},
		}
	}
	readsSlices := func(role *rbacv1.Role) bool {
		for _, rule := range role.Rules {
			if len(rule.APIGroups) == 1 && rule.APIGroups[0] == "discovery.k8s.io" &&
				len(rule.Resources) == 1 && rule.Resources[0] == "endpointslices" &&
				reflect.DeepEqual(rule.Verbs, []string{"get", "list", "watch"}) {
				return true
			}
		}
		return false
	}

	for _, tc := range []struct {
		name      string
		pools     bool
		wantWrite bool
	}{{"watch-only", false, false}, {"watch-and-pools", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			mr := endpointRouter(tc.name)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr).Build()
			r := &ModelRouterReconciler{Client: c, Scheme: scheme}
			if err := r.reconcileRouterActivationRBAC(ctx, mr, &compiledConfig{HasPools: tc.pools, NeedsEndpointWatch: routerNeedsEndpointWatch(mr.Spec.Backends)}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			name := routerProxyResourceName(mr.Name)
			if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "ns"}, &corev1.ServiceAccount{}); err != nil {
				t.Fatalf("ServiceAccount: %v", err)
			}
			if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "ns"}, &rbacv1.RoleBinding{}); err != nil {
				t.Fatalf("RoleBinding: %v", err)
			}
			role := &rbacv1.Role{}
			if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "ns"}, role); err != nil {
				t.Fatalf("Role: %v", err)
			}
			if !readsSlices(role) {
				t.Errorf("Role rules = %+v, want get/list/watch on endpointslices", role.Rules)
			}
			wantRules := 1
			if tc.wantWrite {
				wantRules = 3
			}
			if len(role.Rules) != wantRules {
				t.Errorf("Role has %d rules, want exactly %d: %+v", len(role.Rules), wantRules, role.Rules)
			}
			if got := roleGrantsISVCWrite(role); got != tc.wantWrite {
				t.Errorf("InferenceService write = %v, want %v", got, tc.wantWrite)
			}
		})
	}
}

// TestReconcileRouterActivationRBACAddsEndpointRuleOnTransition: the same
// ModelRouter object gains an endpoint-resolution backend between reconciles;
// the one Role must grow from the two InferenceService rules to those plus the
// EndpointSlices rule.
func TestReconcileRouterActivationRBACAddsEndpointRuleOnTransition(t *testing.T) {
	scheme := activationRBACScheme(t)
	ctx := context.Background()
	mr := &inferencev1alpha1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns", UID: "uid-1"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr).Build()
	r := &ModelRouterReconciler{Client: c, Scheme: scheme}
	key := types.NamespacedName{Name: routerProxyResourceName(mr.Name), Namespace: "ns"}

	hasSlices := func(role *rbacv1.Role) bool {
		for _, rule := range role.Rules {
			for _, res := range rule.Resources {
				if res == "endpointslices" {
					return true
				}
			}
		}
		return false
	}

	if err := r.reconcileRouterActivationRBAC(ctx, mr, &compiledConfig{
		HasPools:           true,
		NeedsEndpointWatch: routerNeedsEndpointWatch(mr.Spec.Backends),
	}); err != nil {
		t.Fatalf("reconcile (pools only): %v", err)
	}
	role := &rbacv1.Role{}
	if err := c.Get(ctx, key, role); err != nil {
		t.Fatalf("Role: %v", err)
	}
	if len(role.Rules) != 2 || hasSlices(role) || !roleGrantsISVCWrite(role) {
		t.Fatalf("pools-only Role rules = %+v, want exactly the two InferenceService rules", role.Rules)
	}

	mr.Spec.Backends = []inferencev1alpha1.RouterBackend{{
		Name: "q", InferenceServiceRef: &corev1.LocalObjectReference{Name: "q"},
		Resolution: inferencev1alpha1.RouterBackendResolutionEndpoint,
	}}
	if err := r.reconcileRouterActivationRBAC(ctx, mr, &compiledConfig{
		HasPools:           true,
		NeedsEndpointWatch: routerNeedsEndpointWatch(mr.Spec.Backends),
	}); err != nil {
		t.Fatalf("reconcile (after adding endpoint backend): %v", err)
	}
	role = &rbacv1.Role{}
	if err := c.Get(ctx, key, role); err != nil {
		t.Fatalf("Role: %v", err)
	}
	if len(role.Rules) != 3 || !hasSlices(role) || !roleGrantsISVCWrite(role) {
		t.Errorf("transitioned Role rules = %+v, want the two InferenceService rules plus endpointslices", role.Rules)
	}
	roles := &rbacv1.RoleList{}
	if err := c.List(ctx, roles); err != nil || len(roles.Items) != 1 {
		t.Errorf("Role count = %d (err %v), want 1", len(roles.Items), err)
	}
}
