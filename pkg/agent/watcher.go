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
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

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

// InferenceServiceWatcher watches for InferenceService resources
type InferenceServiceWatcher struct {
	client    client.Client
	namespace string
}

// NewInferenceServiceWatcher creates a new watcher
func NewInferenceServiceWatcher(client client.Client, namespace string) *InferenceServiceWatcher {
	return &InferenceServiceWatcher{
		client:    client,
		namespace: namespace,
	}
}

// Watch starts watching for InferenceService changes
func (w *InferenceServiceWatcher) Watch(ctx context.Context, eventChan chan<- InferenceServiceEvent) error {
	// List existing InferenceServices on startup
	if err := w.listExisting(ctx, eventChan); err != nil {
		return fmt.Errorf("failed to list existing services: %w", err)
	}

	// Watch for changes
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Store last seen resource versions to detect changes
	seen := make(map[string]string)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.poll(ctx, eventChan, seen); err != nil {
				fmt.Printf("⚠️  Warning: polling error: %v\n", err)
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
		if !w.shouldWatch(&list.Items[i]) {
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
func (w *InferenceServiceWatcher) poll(ctx context.Context, eventChan chan<- InferenceServiceEvent, seen map[string]string) error {
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
		if !w.shouldWatch(&list.Items[i]) {
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

// shouldWatch determines if this InferenceService should be watched
func (w *InferenceServiceWatcher) shouldWatch(isvc *inferencev1alpha1.InferenceService) bool {
	// TODO: Check if service has Metal annotation or label
	// For now, watch all services (the executor will determine if Metal is appropriate)
	return true
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
