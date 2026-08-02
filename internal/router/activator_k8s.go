/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// kubeMemberController is the production MemberController: it drives ModelPool
// swaps through the Kubernetes API. It scales a member InferenceService to one
// replica (the activation commit the ModelPoolReconciler acts on) and polls the
// member status until it reports Ready.
//
// This is the one place the router-proxy talks to the API server. It is only
// constructed when activation is enabled (--enable-activation), so a router
// with no pooled backends keeps the proxy free of API access.
type kubeMemberController struct {
	client       client.Client
	pollInterval time.Duration
}

// NewKubeMemberController builds a MemberController backed by cl. pollInterval
// controls how often WaitReady re-reads member status; zero uses a sane default.
func NewKubeMemberController(cl client.Client, pollInterval time.Duration) MemberController {
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	return &kubeMemberController{client: cl, pollInterval: pollInterval}
}

func (k *kubeMemberController) Activate(ctx context.Context, namespace, isvc string) error {
	cur := &inferencev1alpha1.InferenceService{}
	if err := k.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: isvc}, cur); err != nil {
		return fmt.Errorf("get member %s/%s: %w", namespace, isvc, err)
	}
	if cur.Spec.Replicas != nil && *cur.Spec.Replicas >= 1 {
		return nil
	}
	patched := cur.DeepCopy()
	patched.Spec.Replicas = ptr.To(int32(1))
	if err := k.client.Patch(ctx, patched, client.MergeFrom(cur)); err != nil {
		return fmt.Errorf("scale up member %s/%s: %w", namespace, isvc, err)
	}
	return nil
}

func (k *kubeMemberController) Deactivate(ctx context.Context, namespace, isvc string) error {
	cur := &inferencev1alpha1.InferenceService{}
	if err := k.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: isvc}, cur); err != nil {
		return fmt.Errorf("get member %s/%s: %w", namespace, isvc, err)
	}
	if cur.Spec.Replicas != nil && *cur.Spec.Replicas == 0 {
		return nil
	}
	patched := cur.DeepCopy()
	patched.Spec.Replicas = ptr.To(int32(0))
	if err := k.client.Patch(ctx, patched, client.MergeFrom(cur)); err != nil {
		return fmt.Errorf("scale down member %s/%s: %w", namespace, isvc, err)
	}
	return nil
}

func (k *kubeMemberController) WaitReady(ctx context.Context, namespace, isvc string) error {
	ticker := time.NewTicker(k.pollInterval)
	defer ticker.Stop()
	for {
		phase, err := k.Phase(ctx, namespace, isvc)
		if err != nil {
			return err
		}
		if phase == modelReadyPhase {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (k *kubeMemberController) Phase(ctx context.Context, namespace, isvc string) (string, error) {
	cur := &inferencev1alpha1.InferenceService{}
	if err := k.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: isvc}, cur); err != nil {
		return "", fmt.Errorf("get member %s/%s: %w", namespace, isvc, err)
	}
	return cur.Status.Phase, nil
}
