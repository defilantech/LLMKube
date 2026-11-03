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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// reconcileRouterService creates or updates the K8s Services in front of
// the router-proxy pods: the data-plane Service (ClusterIP by default;
// spec.endpoint.type can upgrade it to NodePort / LoadBalancer to expose the
// router beyond the cluster) and the internal admin Service.
//
// The data-plane Service is named by routerProxyServiceName, so a change of
// spec.proxy.serviceName (#2279) moves it. The previous Service this router
// controls is deleted only after the new one is in place: a refusal (the new
// name is held by an object this router does not control) or any other error
// returns first, so the router keeps serving on the old name.
func (r *ModelRouterReconciler) reconcileRouterService(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) error {
	stale, err := r.staleRouterServices(ctx, mr)
	if err != nil {
		return err
	}
	desired := newRouterService(mr)
	pinDeferred := deferHeldNodePort(desired, stale)
	if err := r.reconcileServiceObject(ctx, mr, desired); err != nil {
		return err
	}
	if err := r.reconcileServiceObject(ctx, mr, newRouterAdminService(mr)); err != nil {
		return err
	}
	for i := range stale {
		old := &stale[i]
		// The UID precondition makes the delete hit exactly the Service listed
		// as ours: if that one is gone and someone recreated the name since,
		// the apiserver answers Conflict and the newcomer is left alone.
		err := r.Delete(ctx, old, client.Preconditions{UID: &old.UID})
		if err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
			return fmt.Errorf("delete previous router Service %s: %w", old.Name, err)
		}
	}
	if pinDeferred {
		// The previous Service released the pinned node port; take it now.
		return r.reconcileServiceObject(ctx, mr, newRouterService(mr))
	}
	return nil
}

// staleRouterServices returns the data-plane Services this router controls
// under a name it no longer uses: the previous name after a change of
// spec.proxy.serviceName. It lists by the proxy's selector labels, which both
// router Services carry, then keeps only Services this router controls, so a
// Service someone else made with the same labels (an alias selecting the
// proxy pods, say) is never deleted. In steady state it returns nothing.
func (r *ModelRouterReconciler) staleRouterServices(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) ([]corev1.Service, error) {
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list, client.InNamespace(mr.Namespace),
		client.MatchingLabels(routerProxySelectorLabels(mr))); err != nil {
		return nil, fmt.Errorf("list router Services: %w", err)
	}
	current := routerProxyServiceName(mr)
	admin := routerProxyAdminResourceName(mr.Name)
	var stale []corev1.Service
	for i := range list.Items {
		svc := &list.Items[i]
		if svc.Name == current || svc.Name == admin || !metav1.IsControlledBy(svc, mr) {
			continue
		}
		stale = append(stale, *svc)
	}
	return stale, nil
}

// deferHeldNodePort clears desired's pinned node port when a Service about to
// be deleted still holds it, and reports whether it did. A node port belongs
// to one Service at a time, so a rename that keeps endpoint.nodePort pinned
// cannot create the new Service on that port while the old one exists: the
// new Service is created on an allocated port, and reconcileRouterService
// moves it to the pinned one once the old Service is gone.
func deferHeldNodePort(desired *corev1.Service, stale []corev1.Service) bool {
	pinned := desired.Spec.Ports[0].NodePort
	if pinned == 0 {
		return false
	}
	for i := range stale {
		for _, p := range stale[i].Spec.Ports {
			if p.NodePort == pinned {
				desired.Spec.Ports[0].NodePort = 0
				return true
			}
		}
	}
	return false
}

// reconcileServiceObject creates the Service if absent and otherwise syncs the
// mutable fields. The selector is immutable in the K8s API; it is set once at
// creation and never mutated. Ports, service-type and externalTrafficPolicy
// are the only mutable bits worth syncing.
func (r *ModelRouterReconciler) reconcileServiceObject(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	desired *corev1.Service,
) error {
	if err := setControllerReferenceUnblocked(mr, desired, r.Scheme); err != nil {
		return fmt.Errorf("set owner ref on router Service %s: %w", desired.Name, err)
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	switch {
	case errors.IsNotFound(err):
		return r.Create(ctx, desired)
	case err != nil:
		return err
	}
	if err := ensureRouterProxyOwned(mr, existing, "Service"); err != nil {
		return err
	}

	existing.Spec.Type = desired.Spec.Type
	// Carry the allocated node port over when the spec does not pin one, so an
	// update never sends nodePort 0 and lets the apiserver reassign it.
	existing.Spec.Ports = mergeServicePorts(existing.Spec.Ports, desired.Spec.Ports, desired.Spec.Type)
	// Empty is safe to send: the apiserver re-defaults it to Cluster on
	// NodePort/LoadBalancer, so removing the field reverts to the default.
	existing.Spec.ExternalTrafficPolicy = desired.Spec.ExternalTrafficPolicy
	existing.Labels = desired.Labels
	if err := setControllerReferenceUnblocked(mr, existing, r.Scheme); err != nil {
		return fmt.Errorf("set owner ref on existing router Service %s: %w", desired.Name, err)
	}
	return r.Update(ctx, existing)
}

// newRouterService is the in-memory blueprint of the Service. Pure
// function for testability.
func newRouterService(mr *inferencev1alpha1.ModelRouter) *corev1.Service {
	port := routerProxyPort
	serviceType := corev1.ServiceTypeClusterIP
	var nodePort int32
	var etp corev1.ServiceExternalTrafficPolicy
	if mr.Spec.Endpoint != nil {
		if mr.Spec.Endpoint.Port > 0 {
			port = mr.Spec.Endpoint.Port
		}
		switch mr.Spec.Endpoint.Type {
		case "NodePort":
			serviceType = corev1.ServiceTypeNodePort
		case "LoadBalancer":
			serviceType = corev1.ServiceTypeLoadBalancer
		}
		if serviceType == corev1.ServiceTypeNodePort && mr.Spec.Endpoint.NodePort != nil {
			nodePort = *mr.Spec.Endpoint.NodePort
		}
		if mr.Spec.Endpoint.ExternalTrafficPolicy != "" && serviceType != corev1.ServiceTypeClusterIP {
			etp = corev1.ServiceExternalTrafficPolicy(mr.Spec.Endpoint.ExternalTrafficPolicy)
		}
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routerProxyServiceName(mr),
			Namespace: mr.Namespace,
			Labels:    routerProxyLabels(mr),
		},
		Spec: corev1.ServiceSpec{
			Type:                  serviceType,
			Selector:              routerProxySelectorLabels(mr),
			ExternalTrafficPolicy: etp,
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Port:     port,
					NodePort: nodePort,
					// spec.endpoint.port is the Service port only: the proxy
					// always listens on routerProxyPort, so a Service on port 80
					// still reaches it without the pod binding below 1024 (#2278).
					TargetPort: intstr.FromInt(int(routerProxyPort)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// newRouterAdminService is the in-memory blueprint of the internal Service
// that carries the proxy's metrics/admin listener. It is always ClusterIP:
// the admin endpoint is read by the operator, never published off-cluster, so
// it does not ride the data-plane Service whose spec.endpoint.type the user
// controls.
func newRouterAdminService(mr *inferencev1alpha1.ModelRouter) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routerProxyAdminResourceName(mr.Name),
			Namespace: mr.Namespace,
			Labels:    routerProxyLabels(mr),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: routerProxySelectorLabels(mr),
			Ports: []corev1.ServicePort{
				{
					Name:       "admin",
					Port:       routerProxyMetricsPort,
					TargetPort: intstr.FromInt(int(routerProxyMetricsPort)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// routerProxyEndpoint constructs the in-cluster URL that the
// ModelRouter publishes on status.endpoint, on the data-plane Service's
// current name. Mirrors the shape used by InferenceService.
func routerProxyEndpoint(mr *inferencev1alpha1.ModelRouter) string {
	port := routerProxyPort
	path := "/v1/chat/completions"
	if mr.Spec.Endpoint != nil {
		if mr.Spec.Endpoint.Port > 0 {
			port = mr.Spec.Endpoint.Port
		}
		if mr.Spec.Endpoint.Path != "" {
			path = mr.Spec.Endpoint.Path
		}
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d%s",
		routerProxyServiceName(mr), mr.Namespace, port, path)
}

// routerProxyBudgetEndpoint is the admin URL the reconciler polls for budget
// utilization. It rides the proxy's metrics listener, published on the
// internal admin Service rather than the data-plane Service.
func routerProxyBudgetEndpoint(mr *inferencev1alpha1.ModelRouter) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/admin/budgets",
		routerProxyAdminResourceName(mr.Name), mr.Namespace, routerProxyMetricsPort)
}
