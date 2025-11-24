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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// ServiceRegistry manages Kubernetes Service and Endpoint resources
// to expose native Metal processes to the cluster
type ServiceRegistry struct {
	client client.Client
}

// NewServiceRegistry creates a new service registry
func NewServiceRegistry(client client.Client) *ServiceRegistry {
	return &ServiceRegistry{
		client: client,
	}
}

// RegisterEndpoint creates/updates a Kubernetes Service and Endpoints
// to expose the native process to the cluster
func (r *ServiceRegistry) RegisterEndpoint(ctx context.Context, isvc *inferencev1alpha1.InferenceService, port int) error {
	// Create or update Service
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      isvc.Name,
			Namespace: isvc.Namespace,
			Labels: map[string]string{
				"app":                          isvc.Name,
				"llmkube.ai/managed-by":        "metal-agent",
				"llmkube.ai/inference-service": isvc.Name,
			},
			Annotations: map[string]string{
				"llmkube.ai/metal-accelerated": "true",
				"llmkube.ai/native-process":    "true",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       8080,
					TargetPort: intstr.FromInt(port),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			// Note: No selector - we'll manually manage Endpoints
		},
	}

	if err := r.client.Create(ctx, service); err != nil {
		// Try update if already exists
		if err := r.client.Update(ctx, service); err != nil {
			return fmt.Errorf("failed to create/update service: %w", err)
		}
	}

	// Create or update Endpoints to point to localhost
	// Since the Metal agent runs on the same machine as minikube,
	// we can use the host's IP address
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      isvc.Name,
			Namespace: isvc.Namespace,
			Labels: map[string]string{
				"app":                          isvc.Name,
				"llmkube.ai/managed-by":        "metal-agent",
				"llmkube.ai/inference-service": isvc.Name,
			},
		},
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{
						// Use host.docker.internal for Docker Desktop
						// This allows pods to access host services
						IP: "host.docker.internal",
						TargetRef: &corev1.ObjectReference{
							Kind: "Pod",
							Name: fmt.Sprintf("%s-metal", isvc.Name),
						},
					},
				},
				Ports: []corev1.EndpointPort{
					{
						Name:     "http",
						Port:     int32(port),
						Protocol: corev1.ProtocolTCP,
					},
				},
			},
		},
	}

	if err := r.client.Create(ctx, endpoints); err != nil {
		// Try update if already exists
		if err := r.client.Update(ctx, endpoints); err != nil {
			return fmt.Errorf("failed to create/update endpoints: %w", err)
		}
	}

	fmt.Printf("📝 Registered endpoint for %s/%s -> localhost:%d\n",
		isvc.Namespace, isvc.Name, port)

	return nil
}

// UnregisterEndpoint removes the Service and Endpoints for a process
func (r *ServiceRegistry) UnregisterEndpoint(ctx context.Context, namespace, name string) error {
	// Delete Service
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	if err := r.client.Delete(ctx, service); err != nil {
		return fmt.Errorf("failed to delete service: %w", err)
	}

	// Delete Endpoints
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	if err := r.client.Delete(ctx, endpoints); err != nil {
		return fmt.Errorf("failed to delete endpoints: %w", err)
	}

	return nil
}
