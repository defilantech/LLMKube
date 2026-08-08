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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	kmcMember = "coder"
	kmcNS     = "lab"
)

func kubeMCScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return s
}

func memberISVC(replicas *int32, phase string) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: kmcMember, Namespace: kmcNS},
		Spec:       inferencev1alpha1.InferenceServiceSpec{Replicas: replicas},
		Status:     inferencev1alpha1.InferenceServiceStatus{Phase: phase},
	}
}

func liveReplicas(t *testing.T, c client.Client) int32 {
	t.Helper()
	got := &inferencev1alpha1.InferenceService{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: kmcNS, Name: kmcMember}, got); err != nil {
		t.Fatalf("get %s/%s: %v", kmcNS, kmcMember, err)
	}
	if got.Spec.Replicas == nil {
		return -1
	}
	return *got.Spec.Replicas
}

func TestKubeMemberControllerActivate(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kubeMCScheme(t)).
		WithObjects(memberISVC(ptr.To(int32(0)), "Stopped")).Build()
	k := NewKubeMemberController(c, 10*time.Millisecond)

	if err := k.Activate(context.Background(), kmcNS, kmcMember); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got := liveReplicas(t, c); got != 1 {
		t.Errorf("replicas after Activate = %d, want 1", got)
	}

	// Already scaled up: idempotent no-op, no error.
	if err := k.Activate(context.Background(), kmcNS, kmcMember); err != nil {
		t.Fatalf("Activate (already up): %v", err)
	}
	if got := liveReplicas(t, c); got != 1 {
		t.Errorf("replicas after second Activate = %d, want 1", got)
	}

	// Missing member surfaces an error.
	if err := k.Activate(context.Background(), kmcNS, "ghost"); err == nil {
		t.Error("Activate of missing member = nil error, want error")
	}
}

func TestKubeMemberControllerDeactivate(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kubeMCScheme(t)).
		WithObjects(memberISVC(ptr.To(int32(1)), "Ready")).Build()
	k := NewKubeMemberController(c, 10*time.Millisecond)

	if err := k.Deactivate(context.Background(), kmcNS, kmcMember); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if got := liveReplicas(t, c); got != 0 {
		t.Errorf("replicas after Deactivate = %d, want 0", got)
	}

	// Already zero: idempotent no-op.
	if err := k.Deactivate(context.Background(), kmcNS, kmcMember); err != nil {
		t.Fatalf("Deactivate (already zero): %v", err)
	}

	if err := k.Deactivate(context.Background(), kmcNS, "ghost"); err == nil {
		t.Error("Deactivate of missing member = nil error, want error")
	}
}

func TestKubeMemberControllerPhase(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kubeMCScheme(t)).
		WithObjects(memberISVC(ptr.To(int32(1)), "Ready")).Build()
	k := NewKubeMemberController(c, 0) // exercise the default pollInterval branch

	phase, err := k.Phase(context.Background(), kmcNS, kmcMember)
	if err != nil {
		t.Fatalf("Phase: %v", err)
	}
	if phase != "Ready" {
		t.Errorf("Phase = %q, want Ready", phase)
	}

	if _, err := k.Phase(context.Background(), kmcNS, "ghost"); err == nil {
		t.Error("Phase of missing member = nil error, want error")
	}
}

func TestKubeMemberControllerWaitReadySucceeds(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kubeMCScheme(t)).
		WithObjects(memberISVC(ptr.To(int32(1)), "Ready")).Build()
	k := NewKubeMemberController(c, 10*time.Millisecond)

	if err := k.WaitReady(context.Background(), kmcNS, kmcMember); err != nil {
		t.Fatalf("WaitReady on a Ready member: %v", err)
	}
}

func TestKubeMemberControllerWaitReadyTimesOut(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kubeMCScheme(t)).
		WithObjects(memberISVC(ptr.To(int32(1)), "Pending")).Build()
	k := NewKubeMemberController(c, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := k.WaitReady(ctx, kmcNS, kmcMember); err == nil {
		t.Fatal("WaitReady on a never-Ready member = nil, want context deadline error")
	}
}

func TestKubeMemberControllerWaitReadyPhaseError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kubeMCScheme(t)).Build() // no member present
	k := NewKubeMemberController(c, 10*time.Millisecond)

	if err := k.WaitReady(context.Background(), kmcNS, "ghost"); err == nil {
		t.Error("WaitReady when the member does not exist = nil, want error")
	}
}
