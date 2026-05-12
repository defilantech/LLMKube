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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// reconcileRouterService creates or updates the K8s Service in front of
// the router-proxy pods. ClusterIP by default; spec.endpoint.type can
// upgrade to NodePort / LoadBalancer to expose the router beyond the
// cluster (most users won't, but enterprise admins occasionally do for
// per-router edge-of-cluster ingress).
func (r *ModelRouterReconciler) reconcileRouterService(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) error {
	desired := newRouterService(mr)
	if err := controllerutil.SetControllerReference(mr, desired, r.Scheme); err != nil {
		return fmt.Errorf("set owner ref on router Service: %w", err)
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	switch {
	case errors.IsNotFound(err):
		return r.Create(ctx, desired)
	case err != nil:
		return err
	}

	// The selector is immutable in the K8s API; we set it once at
	// creation and never mutate it. The port and service-type are the
	// only mutable bits worth syncing here.
	existing.Spec.Type = desired.Spec.Type
	existing.Spec.Ports = desired.Spec.Ports
	existing.Labels = desired.Labels
	if err := controllerutil.SetControllerReference(mr, existing, r.Scheme); err != nil {
		return fmt.Errorf("set owner ref on existing router Service: %w", err)
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
