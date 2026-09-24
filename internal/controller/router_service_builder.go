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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// reconcileRouterService creates or updates the K8s Services in front of
// the router-proxy pods: the data-plane Service (ClusterIP by default;
// spec.endpoint.type can upgrade it to NodePort / LoadBalancer to expose the
// router beyond the cluster) and the internal admin Service.
func (r *ModelRouterReconciler) reconcileRouterService(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) error {
	if err := r.reconcileServiceObject(ctx, mr, newRouterService(mr)); err != nil {
		return err
	}
	return r.reconcileServiceObject(ctx, mr, newRouterAdminService(mr))
}

// reconcileServiceObject creates the Service if absent and otherwise syncs the
// mutable fields. The selector is immutable in the K8s API; it is set once at
// creation and never mutated. Ports and service-type are the only mutable bits
// worth syncing.
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

	existing.Spec.Type = desired.Spec.Type
	existing.Spec.Ports = desired.Spec.Ports
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
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routerProxyResourceName(mr.Name),
			Namespace: mr.Namespace,
			Labels:    routerProxyLabels(mr),
		},
		Spec: corev1.ServiceSpec{
			Type:     serviceType,
			Selector: routerProxySelectorLabels(mr),
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       port,
					TargetPort: intstr.FromInt(int(port)),
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
// ModelRouter publishes on status.endpoint. Mirrors the shape used by
// InferenceService.
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
		routerProxyResourceName(mr.Name), mr.Namespace, port, path)
}

// routerProxyBudgetEndpoint is the admin URL the reconciler polls for budget
// utilization. It rides the proxy's metrics listener, published on the
// internal admin Service rather than the data-plane Service.
func routerProxyBudgetEndpoint(mr *inferencev1alpha1.ModelRouter) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/admin/budgets",
		routerProxyAdminResourceName(mr.Name), mr.Namespace, routerProxyMetricsPort)
}
