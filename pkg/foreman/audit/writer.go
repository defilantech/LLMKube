/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

const (
	// AuditLabel marks an audit-record ConfigMap; AuditTaskLabel carries the
	// task name for filtered discovery (`-l foreman.llmkube.dev/audit=true`).
	AuditLabel     = "foreman.llmkube.dev/audit"
	AuditTaskLabel = "foreman.llmkube.dev/audit-task"

	// auditDataKey is the ConfigMap data key holding the JSON Record.
	auditDataKey = "audit.json"

	auditNamePrefix = "foreman-audit-"

	// AuditedAnnotation guards against re-writing a record on every reconcile
	// of an already-terminal task (which would emit duplicate audit lines).
	AuditedAnnotation = "foreman.llmkube.dev/audited"

	// TTLAnnotationKey is the annotation key that stores the TTL in seconds
	// after which the audit ConfigMap should be reaped.
	TTLAnnotationKey = "foreman.llmkube.dev/audit-ttl-seconds"

	// DefaultTTLSeconds is the default retention period for audit ConfigMaps
	// (30 days).
	DefaultTTLSeconds = 30 * 24 * 60 * 60
)

// AuditConfigMapName returns the ConfigMap name for a task's audit record.
func AuditConfigMapName(taskName string) string { return auditNamePrefix + taskName }

// WriteRecord upserts a durable, NON-owner-ref'd ConfigMap holding rec, plus
// emits the record as a single structured log line. The absence of an owner
// reference is deliberate: the record must outlive the AgenticTask so it
// remains a compliance trail after task garbage-collection. namespace is the
// audit namespace (caller passes the task namespace when unset upstream).
// The ConfigMap is annotated with a TTL (DefaultTTLSeconds) so that
// ReapOldRecords can clean up stale records.
func WriteRecord(ctx context.Context, c client.Client, namespace string, rec Record, log logr.Logger) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("audit: marshal record: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AuditConfigMapName(rec.Task.Name),
			Namespace: namespace,
			Labels: map[string]string{
				AuditLabel:                  "true",
				AuditTaskLabel:              rec.Task.Name,
				"app.kubernetes.io/part-of": "foreman",
			},
			Annotations: map[string]string{
				TTLAnnotationKey: fmt.Sprintf("%d", DefaultTTLSeconds),
			},
			// No OwnerReferences: survives task GC by design.
		},
		Data: map[string]string{auditDataKey: string(data)},
	}

	existing := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: namespace, Name: cm.Name}
	switch getErr := c.Get(ctx, key, existing); {
	case apierrors.IsNotFound(getErr):
		if err := c.Create(ctx, cm); err != nil {
			return fmt.Errorf("audit: create configmap: %w", err)
		}
	case getErr == nil:
		existing.Data = cm.Data
		existing.Labels = cm.Labels
		if err := c.Update(ctx, existing); err != nil {
			return fmt.Errorf("audit: update configmap: %w", err)
		}
	default:
		return fmt.Errorf("audit: get configmap: %w", getErr)
	}

	// Structured audit stream line for SIEM/Loki ingestion. The durable
	// ConfigMap above is the primary record; this is the streaming copy.
	log.Info("foreman.audit",
		"task", rec.Task.Name, "namespace", rec.Task.Namespace,
		"verdict", rec.Verdict, "node", rec.AssignedNode,
		"repo", rec.Repo, "issue", rec.Issue, "schemaVersion", rec.SchemaVersion)
	return nil
}

// RecordTerminal writes the audit record for a terminal task exactly once.
// It is a no-op if the task already carries AuditedAnnotation. auditNamespace
// is the namespace for the record; empty means the task's own namespace. The
// referenced Agent is fetched best-effort (a missing Agent yields a record
// with no agent block, not an error). On success it stamps AuditedAnnotation
// so subsequent reconciles skip the write.
func RecordTerminal(
	ctx context.Context,
	c client.Client,
	task *foremanv1alpha1.AgenticTask,
	auditNamespace string,
	log logr.Logger,
) error {
	if task.Annotations[AuditedAnnotation] == "true" {
		return nil
	}
	ns := auditNamespace
	if ns == "" {
		ns = task.Namespace
	}

	var agent *foremanv1alpha1.Agent
	if task.Spec.AgentRef != nil && task.Spec.AgentRef.Name != "" {
		var a foremanv1alpha1.Agent
		key := client.ObjectKey{Namespace: task.Namespace, Name: task.Spec.AgentRef.Name}
		if err := c.Get(ctx, key, &a); err == nil {
			agent = &a
		} else {
			log.Info("audit: agent not resolvable; recording without agent block",
				"agent", task.Spec.AgentRef.Name, "err", err.Error())
		}
	}

	rec := BuildRecord(task, agent)
	if err := WriteRecord(ctx, c, ns, rec, log); err != nil {
		return err
	}

	patch := client.MergeFrom(task.DeepCopy())
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[AuditedAnnotation] = "true"
	if err := c.Patch(ctx, task, patch); err != nil {
		return fmt.Errorf("audit: set audited annotation: %w", err)
	}
	return nil
}

// ReapOldRecords deletes audit ConfigMaps in namespace that are older than
// their TTL annotation (TTLAnnotationKey). ConfigMaps without the annotation
// are skipped (they are assumed to be managed by a different process).
// Returns the number of ConfigMaps deleted.
func ReapOldRecords(ctx context.Context, c client.Client, namespace string, log logr.Logger) (int, error) {
	cms := &corev1.ConfigMapList{}
	if err := c.List(ctx, cms, client.InNamespace(namespace), client.MatchingLabels{AuditLabel: "true"}); err != nil {
		return 0, fmt.Errorf("audit: list configmaps: %w", err)
	}
	var deleted int
	for i := range cms.Items {
		cm := &cms.Items[i]
		ttlStr, ok := cm.Annotations[TTLAnnotationKey]
		if !ok {
			continue
		}
		var ttlSec int64
		if _, err := fmt.Sscanf(ttlStr, "%d", &ttlSec); err != nil {
			log.Info("audit: invalid TTL annotation; skipping", "configmap", cm.Name, "annotation", ttlStr)
			continue
		}
		if cm.CreationTimestamp.Add(time.Duration(ttlSec) * time.Second).After(time.Now()) {
			continue
		}
		if err := c.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return deleted, fmt.Errorf("audit: delete configmap %s: %w", cm.Name, err)
		}
		deleted++
		log.Info("audit: reaped old ConfigMap", "configmap", cm.Name, "ttlSeconds", ttlSec)
	}
	return deleted, nil
}
