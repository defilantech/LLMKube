/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package audit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func TestWriteRecordCreatesDurableConfigMap(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	rec := Record{SchemaVersion: SchemaVersion, Task: TaskRef{Name: "coder-89", Namespace: "default"}, Verdict: "GO"}

	if err := WriteRecord(context.Background(), c, "default", rec, logr.Discard()); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}

	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: "default", Name: "foreman-audit-coder-89"}
	if err := c.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("audit ConfigMap not created: %v", err)
	}
	if len(cm.OwnerReferences) != 0 {
		t.Errorf("audit ConfigMap MUST NOT be owner-ref'd (must survive task GC), got %d refs", len(cm.OwnerReferences))
	}
	if cm.Labels[AuditLabel] != "true" || cm.Labels[AuditTaskLabel] != "coder-89" {
		t.Errorf("labels wrong: %v", cm.Labels)
	}
	var got Record
	if err := json.Unmarshal([]byte(cm.Data[auditDataKey]), &got); err != nil {
		t.Fatalf("decode audit.json: %v", err)
	}
	if got.Verdict != "GO" || got.Task.Name != "coder-89" {
		t.Errorf("round-trip wrong: %+v", got)
	}
}

func TestWriteRecordIdempotentUpsert(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	rec := Record{SchemaVersion: SchemaVersion, Task: TaskRef{Name: "t", Namespace: "default"}, Verdict: "GO"}
	ctx := context.Background()
	if err := WriteRecord(ctx, c, "default", rec, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	rec.Verdict = "NO-GO"
	if err := WriteRecord(ctx, c, "default", rec, logr.Discard()); err != nil {
		t.Fatalf("second write: %v", err)
	}
	var cm corev1.ConfigMap
	_ = c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "foreman-audit-t"}, &cm)
	var got Record
	_ = json.Unmarshal([]byte(cm.Data[auditDataKey]), &got)
	if got.Verdict != "NO-GO" {
		t.Errorf("upsert did not update, verdict=%q", got.Verdict)
	}
}

func TestRecordTerminalWritesOnceAndSetsAnnotation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = foremanv1alpha1.AddToScheme(scheme)

	task := coderTask() // from record_test.go (same package)
	agent := coderAgent()
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(task, agent).
		WithStatusSubresource(task).Build()
	ctx := context.Background()

	if err := RecordTerminal(ctx, c, task, "", logr.Discard()); err != nil {
		t.Fatalf("RecordTerminal: %v", err)
	}
	// audit CM exists in the task namespace (empty audit ns -> task ns)
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "foreman-audit-coder-89"}, &cm); err != nil {
		t.Fatalf("audit CM not written: %v", err)
	}
	// annotation guard set on the task
	var got foremanv1alpha1.AgenticTask
	_ = c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "coder-89"}, &got)
	if got.Annotations[AuditedAnnotation] != "true" {
		t.Errorf("audited annotation not set: %v", got.Annotations)
	}

	// second call is a no-op (already audited): delete the CM, call again,
	// confirm it is NOT recreated.
	_ = c.Delete(ctx, &cm)
	if err := RecordTerminal(ctx, c, &got, "", logr.Discard()); err != nil {
		t.Fatalf("second RecordTerminal: %v", err)
	}
	err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "foreman-audit-coder-89"}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("already-audited task must not re-write the record; got err=%v", err)
	}
}

func TestReapOldRecordsDeletesExpiredConfigMaps(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	ctx := context.Background()

	// Create an expired audit ConfigMap (TTL=1s, created 10s ago).
	expired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foreman-audit-expired-task",
			Namespace: "default",
			Labels: map[string]string{
				AuditLabel:     "true",
				AuditTaskLabel: "expired-task",
			},
			Annotations: map[string]string{
				TTLAnnotationKey: "1",
			},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Second)),
		},
		Data: map[string]string{auditDataKey: "{}"},
	}
	if err := c.Create(ctx, expired); err != nil {
		t.Fatalf("create expired CM: %v", err)
	}

	// Create a fresh audit ConfigMap (TTL=3600s, created just now).
	fresh := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foreman-audit-fresh-task",
			Namespace: "default",
			Labels: map[string]string{
				AuditLabel:     "true",
				AuditTaskLabel: "fresh-task",
			},
			Annotations: map[string]string{
				TTLAnnotationKey: "3600",
			},
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Data: map[string]string{auditDataKey: "{}"},
	}
	if err := c.Create(ctx, fresh); err != nil {
		t.Fatalf("create fresh CM: %v", err)
	}

	// Create an audit ConfigMap without TTL annotation (should be skipped).
	noTTL := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foreman-audit-nottl-task",
			Namespace: "default",
			Labels: map[string]string{
				AuditLabel:     "true",
				AuditTaskLabel: "nottl-task",
			},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Second)),
		},
		Data: map[string]string{auditDataKey: "{}"},
	}
	if err := c.Create(ctx, noTTL); err != nil {
		t.Fatalf("create no-TTL CM: %v", err)
	}

	deleted, err := ReapOldRecords(ctx, c, "default", logr.Discard())
	if err != nil {
		t.Fatalf("ReapOldRecords: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", deleted)
	}

	// Expired CM should be gone.
	expKey := types.NamespacedName{Namespace: "default", Name: "foreman-audit-expired-task"}
	if err := c.Get(ctx, expKey, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("expired CM should have been deleted, got err=%v", err)
	}
	// Fresh CM should still exist.
	freshKey := types.NamespacedName{Namespace: "default", Name: "foreman-audit-fresh-task"}
	if err := c.Get(ctx, freshKey, &corev1.ConfigMap{}); err != nil {
		t.Errorf("fresh CM should still exist: %v", err)
	}
	// No-TTL CM should still exist.
	noTTLKey := types.NamespacedName{Namespace: "default", Name: "foreman-audit-nottl-task"}
	if err := c.Get(ctx, noTTLKey, &corev1.ConfigMap{}); err != nil {
		t.Errorf("no-TTL CM should still exist: %v", err)
	}
}

func TestWriteRecordSetsTTLAnnotation(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	rec := Record{SchemaVersion: SchemaVersion, Task: TaskRef{Name: "coder-89", Namespace: "default"}, Verdict: "GO"}

	if err := WriteRecord(context.Background(), c, "default", rec, logr.Discard()); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}

	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: "default", Name: "foreman-audit-coder-89"}
	if err := c.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("audit ConfigMap not created: %v", err)
	}
	if cm.Annotations[TTLAnnotationKey] != "2592000" {
		t.Errorf("TTL annotation not set correctly, got %q", cm.Annotations[TTLAnnotationKey])
	}
}
