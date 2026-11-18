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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// routerNeedsEndpointWatch reports whether the proxy tracks pods itself: any
// resolved local backend with resolution: endpoint.
func routerNeedsEndpointWatch(backends []inferencev1alpha1.RouterBackend) bool {
	for _, b := range backends {
		if b.InferenceServiceRef != nil && b.Resolution == inferencev1alpha1.RouterBackendResolutionEndpoint {
			return true
		}
	}
	return false
}

// reconcileRouterActivationRBAC provisions the least-privilege RBAC the
// router-proxy needs: a ServiceAccount, a namespaced Role, and a RoleBinding.
// The Role carries what the proxy uses, ModelPool activation (get/list/watch/
// update/patch on InferenceServices, get on their status) and endpoint
// tracking (get/list/watch on EndpointSlices). All three objects are
// owner-referenced to the ModelRouter so they are garbage-collected with it.
//
// When the router has neither pools nor endpoint backends it needs no API
// access; this is a no-op (the proxy runs under the namespace default SA). The
// objects are only created, never deleted on that path, so a router that
// briefly drops its last pool keeps the (unused) SA rather than churning RBAC
// on every toggle. The RBAC is harmless without --enable-activation, which the
// deployment omits when unpooled.
func (r *ModelRouterReconciler) reconcileRouterActivationRBAC(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	compiled *compiledConfig,
) error {
	watch := compiled.NeedsEndpointWatch
	if !compiled.HasPools && !watch {
		return nil
	}
	name := routerProxyResourceName(mr.Name)

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: mr.Namespace,
			Labels:    routerProxyLabels(mr),
		},
	}
	if err := r.upsertActivationObject(ctx, mr, sa, &corev1.ServiceAccount{}); err != nil {
		return fmt.Errorf("reconcile router-proxy ServiceAccount: %w", err)
	}

	var rules []rbacv1.PolicyRule
	if compiled.HasPools {
		rules = append(rules,
			rbacv1.PolicyRule{
				APIGroups: []string{"inference.llmkube.dev"},
				Resources: []string{"inferenceservices"},
				Verbs:     []string{"get", "list", "watch", "update", "patch"},
			},
			rbacv1.PolicyRule{
				APIGroups: []string{"inference.llmkube.dev"},
				Resources: []string{"inferenceservices/status"},
				Verbs:     []string{"get"},
			})
	}
	if watch {
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{"discovery.k8s.io"},
			Resources: []string{"endpointslices"},
			Verbs:     []string{"get", "list", "watch"},
		})
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: mr.Namespace,
			Labels:    routerProxyLabels(mr),
		},
		Rules: rules,
	}
	if err := r.upsertActivationRole(ctx, mr, role); err != nil {
		return fmt.Errorf("reconcile router-proxy Role: %w", err)
	}

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: mr.Namespace,
			Labels:    routerProxyLabels(mr),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      name,
				Namespace: mr.Namespace,
			},
		},
	}
	if err := r.upsertActivationBinding(ctx, mr, binding); err != nil {
		return fmt.Errorf("reconcile router-proxy RoleBinding: %w", err)
	}
	return nil
}

func (r *ModelRouterReconciler) upsertActivationObject(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	desired client.Object,
	into client.Object,
) error {
	if err := setControllerReferenceUnblocked(mr, desired, r.Scheme); err != nil {
		return err
	}
	err := r.Get(ctx, types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()}, into)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// The proxy runs as this ServiceAccount, so a same-name SA the router did
	// not create is a privilege boundary, not a convenience: refuse rather
	// than run the proxy under it and bind it through the RoleBinding below.
	if err := ensureRouterProxyOwned(mr, into, "ServiceAccount"); err != nil {
		return err
	}
	return nil
}

func (r *ModelRouterReconciler) upsertActivationRole(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	desired *rbacv1.Role,
) error {
	if err := setControllerReferenceUnblocked(mr, desired, r.Scheme); err != nil {
		return err
	}
	existing := &rbacv1.Role{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if err := ensureRouterProxyOwned(mr, existing, "Role"); err != nil {
		return err
	}
	existing.Rules = desired.Rules
	if err := setControllerReferenceUnblocked(mr, existing, r.Scheme); err != nil {
		return err
	}
	return r.Update(ctx, existing)
}

func (r *ModelRouterReconciler) upsertActivationBinding(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	desired *rbacv1.RoleBinding,
) error {
	if err := setControllerReferenceUnblocked(mr, desired, r.Scheme); err != nil {
		return err
	}
	existing := &rbacv1.RoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Overwriting the subjects of a binding the router did not create would
	// hand this router's ServiceAccount to whatever role the existing binding
	// references (roleRef is immutable), so the ownership gate comes first.
	if err := ensureRouterProxyOwned(mr, existing, "RoleBinding"); err != nil {
		return err
	}
	// RoleRef is immutable; only Subjects can change.
	existing.Subjects = desired.Subjects
	if err := setControllerReferenceUnblocked(mr, existing, r.Scheme); err != nil {
		return err
	}
	return r.Update(ctx, existing)
}
