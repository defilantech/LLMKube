/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
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

	if err := r.reconcileRouterActivationRBAC(context.Background(), mr, false); err != nil {
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

	if err := r.reconcileRouterActivationRBAC(ctx, mr, true); err != nil {
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
	if err := r.reconcileRouterActivationRBAC(ctx, mr, true); err != nil {
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
