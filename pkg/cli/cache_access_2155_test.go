package cli

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A leftover cache pod of this run's kind at a name this run does not use is
// still one llmkube replaces: the claim is free the moment its own run ends.
// Only a pod another live run stamps is a reason to wait.
func TestSecondMountBlockerReplacesItsOwnKindAtAnotherName(t *testing.T) {
	// Created now: a pod's own clock starts at zero, which reads as abandoned.
	created := metav1.NewTime(time.Now())
	pod := func(labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "llmkube-cache-purge-deadbeef", Labels: labels, CreationTimestamp: created,
		}}
	}
	for _, c := range []struct {
		name    string
		labels  map[string]string
		wantErr string
	}{
		{
			name: "abandoned pod of this kind",
			labels: map[string]string{
				managedByLabel: managedByValue, modelCacheLabel: purgePodKind.component,
			},
		},
		{
			name: "pod of the other kind",
			labels: map[string]string{
				managedByLabel: managedByValue, modelCacheLabel: inspectorPodKind.component,
			},
			wantErr: "nothing will remove it",
		},
		{
			name: "pod a live run stamps",
			labels: map[string]string{
				managedByLabel: managedByValue, modelCacheLabel: purgePodKind.component,
				cachePurgeRunLabel: "another-run",
			},
			wantErr: "Wait for that pod to stop",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := secondMountBlocker(pod(c.labels), modelCachePVCName, "llmkube-cache-purge-0123",
				purgePodKind, "this-run", true, nil)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("expected the claim to be taken over, got %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("error = %v, want %q", err, c.wantErr)
			}
		})
	}
}

// A pod this run stamped at its own name is replaced too: it is this run's
// leftover, not a concurrent run's.
func TestSecondMountBlockerReplacesThisRunsOwnStamp(t *testing.T) {
	holder := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:              "llmkube-cache-purge-0123",
		CreationTimestamp: metav1.NewTime(time.Now()),
		Labels: map[string]string{
			managedByLabel: managedByValue, modelCacheLabel: purgePodKind.component,
			cachePurgeRunLabel: "this-run",
		},
	}}
	if err := secondMountBlocker(holder, modelCachePVCName, "llmkube-cache-purge-0123",
		purgePodKind, "this-run", true, nil); err != nil {
		t.Fatalf("this run's own leftover must be replaced: %v", err)
	}
}
