/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func mpoolFor(resident, pending, def string, names ...string) *inferencev1alpha1.ModelPool {
	members := make([]inferencev1alpha1.ModelPoolMember, 0, len(names))
	for _, n := range names {
		members = append(members, inferencev1alpha1.ModelPoolMember{
			InferenceServiceRef: corev1.LocalObjectReference{Name: n},
		})
	}
	return &inferencev1alpha1.ModelPool{
		Spec:   inferencev1alpha1.ModelPoolSpec{Members: members, Default: def},
		Status: inferencev1alpha1.ModelPoolStatus{ResidentMember: resident, PendingMember: pending},
	}
}

func isvcReplicas(r int32) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{Replicas: ptr.To(r)}}
}

func membersMap(scaled map[string]int32) map[string]*inferencev1alpha1.InferenceService {
	m := make(map[string]*inferencev1alpha1.InferenceService, len(scaled))
	for name, r := range scaled {
		m[name] = isvcReplicas(r)
	}
	return m
}

func TestResolveSlotOwner(t *testing.T) {
	tests := []struct {
		name    string
		pool    *inferencev1alpha1.ModelPool
		members map[string]int32
		want    string
	}{
		{
			name:    "cold pool, nothing scaled, spec.default warms",
			pool:    mpoolFor("", "", "coder", "coder", "judge"),
			members: map[string]int32{"coder": 0, "judge": 0},
			want:    "coder",
		},
		{
			name:    "cold pool, nothing scaled, no default stays cold",
			pool:    mpoolFor("", "", "", "coder", "judge"),
			members: map[string]int32{"coder": 0, "judge": 0},
			want:    "",
		},
		{
			name:    "cold pool, scaled member matching default wins",
			pool:    mpoolFor("", "", "judge", "coder", "judge"),
			members: map[string]int32{"coder": 1, "judge": 1},
			want:    "judge",
		},
		{
			name:    "cold pool, scaled members, no default takes declaration order",
			pool:    mpoolFor("", "", "", "coder", "judge"),
			members: map[string]int32{"coder": 1, "judge": 1},
			want:    "coder",
		},
		{
			name:    "cold pool, pending swap continues first",
			pool:    mpoolFor("", "judge", "coder", "coder", "judge"),
			members: map[string]int32{"coder": 0, "judge": 0},
			want:    "judge",
		},
		{
			name:    "warm pool, fresh activation supersedes resident",
			pool:    mpoolFor("coder", "", "", "coder", "judge"),
			members: map[string]int32{"coder": 1, "judge": 1},
			want:    "judge",
		},
		{
			name:    "warm pool, pending among newly activated wins",
			pool:    mpoolFor("coder", "gemma", "", "coder", "judge", "gemma"),
			members: map[string]int32{"coder": 1, "judge": 1, "gemma": 1},
			want:    "gemma",
		},
		{
			name:    "warm pool, no fresh activation, pending swap continues",
			pool:    mpoolFor("coder", "judge", "", "coder", "judge"),
			members: map[string]int32{"coder": 1, "judge": 0},
			want:    "judge",
		},
		{
			name:    "warm pool, quiescent keeps resident",
			pool:    mpoolFor("coder", "", "", "coder", "judge"),
			members: map[string]int32{"coder": 1, "judge": 0},
			want:    "coder",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveSlotOwner(tc.pool, membersMap(tc.members))
			if got != tc.want {
				t.Errorf("resolveSlotOwner = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplicasOf(t *testing.T) {
	if got := replicasOf(&inferencev1alpha1.InferenceService{}); got != 1 {
		t.Errorf("replicasOf(nil replicas) = %d, want 1 (apiserver default)", got)
	}
	if got := replicasOf(isvcReplicas(0)); got != 0 {
		t.Errorf("replicasOf(0) = %d, want 0", got)
	}
	if got := replicasOf(isvcReplicas(3)); got != 3 {
		t.Errorf("replicasOf(3) = %d, want 3", got)
	}
}

func TestMemberReady(t *testing.T) {
	if memberReady(nil) {
		t.Error("memberReady(nil) = true, want false")
	}
	notReady := &inferencev1alpha1.InferenceService{}
	notReady.Status.Phase = "Pending"
	if memberReady(notReady) {
		t.Error("memberReady(Pending) = true, want false")
	}
	ready := &inferencev1alpha1.InferenceService{}
	ready.Status.Phase = PhaseReady
	if !memberReady(ready) {
		t.Error("memberReady(Ready) = false, want true")
	}
}

func TestMemberNamesDeclarationOrder(t *testing.T) {
	pool := mpoolFor("", "", "", "judge", "coder", "gemma")
	got := memberNames(pool)
	want := []string{"judge", "coder", "gemma"}
	if len(got) != len(want) {
		t.Fatalf("memberNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("memberNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
