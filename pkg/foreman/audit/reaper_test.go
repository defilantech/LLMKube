/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package audit

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// makeAuditCM builds a synthetic audit ConfigMap with the labels the
// real writer stamps onto it, and a caller-controlled CreationTimestamp.
func makeAuditCM(namespace, name string, age time.Duration) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			Labels: map[string]string{
				AuditLabel:     "true",
				AuditTaskLabel: name,
			},
		},
		Data: map[string]string{auditDataKey: "{}"},
	}
}

// TestSweepPrunesOldAuditConfigMaps pins the core reaper behavior: audit
// ConfigMaps older than `retention` are deleted, recent ones are kept,
// and ConfigMaps that do NOT carry the audit label are left untouched
// regardless of age. Without this reaper, audit CMs accumulate forever
// in the task namespace because the writer is deliberately owner-ref
// free (see writer.go) — they must outlive the AgenticTask for audit
// purposes. See defilantech/LLMKube#990.
func TestSweepPrunesOldAuditConfigMaps(t *testing.T) {
	oldAuditNS := makeAuditCM("ns-old", "old", 10*24*time.Hour)               // 10d old
	oldAuditDefault := makeAuditCM("default", "default-old", 30*24*time.Hour) // 30d old
	recentAudit := makeAuditCM("default", "recent", 1*time.Hour)              // 1h old
	oldNonAudit := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "important-config",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-30 * 24 * time.Hour)),
		},
		Data: map[string]string{"k": "v"},
	}

	c := fake.NewClientBuilder().WithObjects(oldAuditNS, oldAuditDefault, recentAudit, oldNonAudit).Build()

	deleted, err := Sweep(context.Background(), c, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 2 {
		t.Errorf("expected 2 deletions (old audit CMs), got %d", deleted)
	}

	// Old audit CMs must be gone.
	for _, gone := range []types.NamespacedName{
		{Namespace: "ns-old", Name: "old"},
		{Namespace: "default", Name: "default-old"},
	} {
		var cm corev1.ConfigMap
		err := c.Get(context.Background(), gone, &cm)
		if !apierrors.IsNotFound(err) {
			t.Errorf("expected %v to be deleted, got err=%v", gone, err)
		}
	}
	// Recent audit CM must survive (within window).
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "recent"}, &cm); err != nil {
		t.Errorf("recent audit CM should be preserved: %v", err)
	}
	// Non-audit CM must NOT be touched (label filter is the contract).
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "important-config"}, &cm); err != nil {
		t.Errorf("non-audit CM was incorrectly modified/deleted: %v", err)
	}
}

// TestSweepRetentionZeroIsNoop pins the disable switch: a retention of 0
// (or negative) means "feature off" and must be a no-op that returns 0
// deletions with no error. Operators can flip --audit-retention=0 to
// disable the reaper without removing the binary.
func TestSweepRetentionZeroIsNoop(t *testing.T) {
	oldAudit := makeAuditCM("default", "old", 365*24*time.Hour) // 1 year old
	c := fake.NewClientBuilder().WithObjects(oldAudit).Build()

	deleted, err := Sweep(context.Background(), c, 0)
	if err != nil {
		t.Fatalf("Sweep with retention=0: %v", err)
	}
	if deleted != 0 {
		t.Errorf("retention=0 must delete nothing, got %d", deleted)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old"}, &cm); err != nil {
		t.Errorf("retention=0 must not delete; got %v", err)
	}

	// Negative retention is also disabled (defensive).
	deleted, err = Sweep(context.Background(), c, -time.Hour)
	if err != nil {
		t.Fatalf("Sweep with negative retention: %v", err)
	}
	if deleted != 0 {
		t.Errorf("negative retention must delete nothing, got %d", deleted)
	}
}

// TestSweepHandlesEmptyCluster pins the no-input case: no audit CMs is
// a clean pass with 0 deletions and no error (guards against division-
// by-zero / nil-list edge cases).
func TestSweepHandlesEmptyCluster(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	deleted, err := Sweep(context.Background(), c, 24*time.Hour)
	if err != nil {
		t.Fatalf("Sweep on empty cluster: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deletions on empty cluster, got %d", deleted)
	}
}

// TestReaperRunTicksAndStops pins the periodic-runner behavior: Start
// runs Sweep on the configured interval, returns when ctx is cancelled,
// and never errors on a clean shutdown. We use a short interval and
// inject a fresh-old audit CM to assert at least one tick observed and
// deleted it.
func TestReaperRunTicksAndStops(t *testing.T) {
	old := makeAuditCM("default", "stale", 2*time.Hour)
	c := fake.NewClientBuilder().WithObjects(old).Build()

	r := &Reaper{
		Client:    c,
		Retention: time.Hour,
		Interval:  10 * time.Millisecond, // tight loop for the test
		Log:       logr.Discard(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Reaper.Start: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Reaper did not stop within 2s after ctx cancel")
	}

	// The stale audit CM should be gone after at least one tick.
	var cm corev1.ConfigMap
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "stale"}, &cm)
	if !apierrors.IsNotFound(err) {
		t.Errorf("stale audit CM should have been reaped; got err=%v", err)
	}
}

// TestReaperRunDisabledRetentionNeverSweeps pins that with Retention=0
// the periodic runner is a no-op: no deletions happen on its tick, and
// cancelling the context still returns cleanly. This is the contract
// for --audit-retention=0 disabling the feature.
func TestReaperRunDisabledRetentionNeverSweeps(t *testing.T) {
	old := makeAuditCM("default", "stale", 365*24*time.Hour)
	c := fake.NewClientBuilder().WithObjects(old).Build()

	r := &Reaper{
		Client:    c,
		Retention: 0,
		Interval:  10 * time.Millisecond,
		Log:       logr.Discard(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Reaper.Start with Retention=0: %v", err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "stale"}, &cm); err != nil {
		t.Errorf("disabled reaper must not delete; got %v", err)
	}
}

// silence unused-import linter when the test list above shrinks.
var _ = client.IgnoreNotFound
