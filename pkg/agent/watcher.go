/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// ErrWatchStalled is returned from Watch when the polling loop has accumulated
// more consecutive list errors than the configured threshold. The agent treats
// this as a fatal condition and exits non-zero so the process supervisor
// (launchd / systemd) can recycle it with a fresh Kubernetes client. This
// recovers from situations like the underlying controller-runtime cache
// silently serving stale data after its watch connection drops.
var ErrWatchStalled = errors.New("watcher polling stalled: consecutive list failures exceeded threshold")

// DefaultMaxConsecutiveFailures is the threshold at which the watcher gives
// up on its current Kubernetes connection. Three failures over the 5-second
// poll interval is 15 seconds of consecutive trouble — long enough to rule
// out a single transient network blip and short enough that the agent does
// not silently sit on a dead watch for hours.
const DefaultMaxConsecutiveFailures = 3

// EventType represents the type of InferenceService event
type EventType string

const (
	EventTypeCreated EventType = "CREATED"
	EventTypeUpdated EventType = "UPDATED"
	EventTypeDeleted EventType = "DELETED"
)

// InferenceServiceEvent represents a change to an InferenceService
type InferenceServiceEvent struct {
	Type             EventType
	InferenceService *inferencev1alpha1.InferenceService
}

// defaultPollInterval is how often the watcher re-lists InferenceServices
// looking for changes. Five seconds is a balance between responsiveness for
// scaling events and load on the API server when many agents share a cluster.
const defaultPollInterval = 5 * time.Second

// InferenceServiceWatcher watches for InferenceService resources
type InferenceServiceWatcher struct {
	client                 client.Client
	namespace              string
	logger                 *zap.SugaredLogger
	maxConsecutiveFailures int
	pollInterval           time.Duration
}

// NewInferenceServiceWatcher creates a new watcher with the default failure
// threshold (DefaultMaxConsecutiveFailures). Use SetMaxConsecutiveFailures to
// override.
func NewInferenceServiceWatcher(
	k8sClient client.Client,
	namespace string,
	logger *zap.SugaredLogger,
) *InferenceServiceWatcher {
	return &InferenceServiceWatcher{
		client:                 k8sClient,
		namespace:              namespace,
		logger:                 logger,
		maxConsecutiveFailures: DefaultMaxConsecutiveFailures,
		pollInterval:           defaultPollInterval,
	}
}

// SetMaxConsecutiveFailures overrides the default failure threshold. Values
// less than 1 are coerced to DefaultMaxConsecutiveFailures.
func (w *InferenceServiceWatcher) SetMaxConsecutiveFailures(n int) {
	if n < 1 {
		n = DefaultMaxConsecutiveFailures
	}
	w.maxConsecutiveFailures = n
}

// SetPollInterval overrides the default 5s polling interval. Intended for
// tests that want to drive the loop quickly; production code should leave
// the default alone. Zero or negative values are ignored.
func (w *InferenceServiceWatcher) SetPollInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	w.pollInterval = d
}

// Watch starts watching for InferenceService changes. It returns nil on clean
// context cancellation, ErrWatchStalled when consecutive list failures exceed
// the configured threshold, or a wrapped error on initial-list failure.
func (w *InferenceServiceWatcher) Watch(ctx context.Context, eventChan chan<- InferenceServiceEvent) error {
	// List existing InferenceServices on startup
	if err := w.listExisting(ctx, eventChan); err != nil {
		return fmt.Errorf("failed to list existing services: %w", err)
	}

	// Watch for changes
	interval := w.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Store last seen resource versions to detect changes
	seen := make(map[string]string)

	threshold := w.maxConsecutiveFailures
	if threshold < 1 {
		threshold = DefaultMaxConsecutiveFailures
	}
	consecutiveFailures := 0
	watchConsecutiveFailures.Set(0)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.poll(ctx, eventChan, seen); err != nil {
				consecutiveFailures++
				watchConsecutiveFailures.Set(float64(consecutiveFailures))
				w.logger.Warnw("polling error",
					"error", err,
					"consecutiveFailures", consecutiveFailures,
					"threshold", threshold,
				)
				if consecutiveFailures >= threshold {
					w.logger.Errorw(
						"watcher stalled — exceeded consecutive failure threshold, "+
							"returning ErrWatchStalled so the agent can exit and "+
							"be restarted by its supervisor",
						"consecutiveFailures", consecutiveFailures,
						"threshold", threshold,
						"lastError", err,
					)
					return ErrWatchStalled
				}
				continue
			}
			if consecutiveFailures > 0 {
				w.logger.Infow("polling recovered after errors", "previousFailures", consecutiveFailures)
				consecutiveFailures = 0
				watchConsecutiveFailures.Set(0)
			}
		}
	}
}

// listExisting lists all existing InferenceServices and sends create events
func (w *InferenceServiceWatcher) listExisting(ctx context.Context, eventChan chan<- InferenceServiceEvent) error {
	list := &inferencev1alpha1.InferenceServiceList{}
	opts := []client.ListOption{}
	if w.namespace != "" {
		opts = append(opts, client.InNamespace(w.namespace))
	}

	if err := w.client.List(ctx, list, opts...); err != nil {
		return err
	}

	for i := range list.Items {
		// Only watch services with Metal accelerator
		if !w.shouldWatch(ctx, &list.Items[i]) {
			continue
		}

		eventChan <- InferenceServiceEvent{
			Type:             EventTypeCreated,
			InferenceService: &list.Items[i],
		}
	}

	return nil
}

// poll checks for changes to InferenceServices
func (w *InferenceServiceWatcher) poll(
	ctx context.Context,
	eventChan chan<- InferenceServiceEvent,
	seen map[string]string,
) error {
	list := &inferencev1alpha1.InferenceServiceList{}
	opts := []client.ListOption{}
	if w.namespace != "" {
		opts = append(opts, client.InNamespace(w.namespace))
	}

	if err := w.client.List(ctx, list, opts...); err != nil {
		return err
	}

	// Track current services
	current := make(map[string]bool)

	for i := range list.Items {
		if !w.shouldWatch(ctx, &list.Items[i]) {
			continue
		}

		isvc := &list.Items[i]
		key := fmt.Sprintf("%s/%s", isvc.Namespace, isvc.Name)
		current[key] = true

		lastVersion, exists := seen[key]
		currentVersion := isvc.ResourceVersion

		if !exists {
			// New service
			eventChan <- InferenceServiceEvent{
				Type:             EventTypeCreated,
				InferenceService: isvc,
			}
			seen[key] = currentVersion
		} else if lastVersion != currentVersion {
			// Updated service
			eventChan <- InferenceServiceEvent{
				Type:             EventTypeUpdated,
				InferenceService: isvc,
			}
			seen[key] = currentVersion
		}
	}

	// Check for deleted services
	for key := range seen {
		if !current[key] {
			// Service was deleted (we don't have the object anymore)
			// Create a minimal object for the delete event
			namespace, name := parseKey(key)
			eventChan <- InferenceServiceEvent{
				Type: EventTypeDeleted,
				InferenceService: &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: namespace,
					},
				},
			}
			delete(seen, key)
		}
	}

	return nil
}

// shouldWatch determines if this InferenceService should be watched by the Metal Agent.
// It looks up the referenced Model and only returns true if the Model's accelerator is "metal".
func (w *InferenceServiceWatcher) shouldWatch(ctx context.Context, isvc *inferencev1alpha1.InferenceService) bool {
	if isvc.Spec.ModelRef == "" {
		return false
	}

	model := &inferencev1alpha1.Model{}
	if err := w.client.Get(ctx, types.NamespacedName{
		Namespace: isvc.Namespace,
		Name:      isvc.Spec.ModelRef,
	}, model); err != nil {
		w.logger.Debugw(
			"skipping inference service because referenced model cannot be fetched",
			"namespace", isvc.Namespace,
			"inferenceService", isvc.Name,
			"modelRef", isvc.Spec.ModelRef,
			"error", err,
		)
		return false
	}

	return model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator == "metal"
}

// parseKey splits "namespace/name" into components
func parseKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return "", key
}
