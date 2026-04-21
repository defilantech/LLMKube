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

package controller

import (
	"context"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// HorizontalPodAutoscaler lifecycle. Runs as part of the main reconcile when
// spec.autoscaling is set; removes the HPA when it is cleared. Metal
// accelerator workloads skip HPA entirely since they run outside a Deployment.
// Metric selection falls through to the configured runtime's DefaultHPAMetric
// when the CRD does not supply an explicit metrics list.

func (r *InferenceServiceReconciler) reconcileHPA(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	deploymentName string,
	isMetal bool,
) error {
	logger := logf.FromContext(ctx)
	hpaName := types.NamespacedName{
		Name:      isvc.Name,
		Namespace: isvc.Namespace,
	}

	// If autoscaling is not configured, clean up any existing HPA
	if isvc.Spec.Autoscaling == nil {
		existingHPA := &autoscalingv2.HorizontalPodAutoscaler{}
		if err := r.Get(ctx, hpaName, existingHPA); err == nil {
			logger.Info("Autoscaling removed, deleting HPA",
				"name", isvc.Name)
			if err := r.Delete(ctx, existingHPA); err != nil {
				return fmt.Errorf("failed to delete HPA: %w", err)
			}
		}
		return nil
	}

	// Skip HPA for Metal accelerator (no Deployment to scale)
	if isMetal {
		logger.Info("Skipping HPA for Metal accelerator workload")
		return nil
	}

	hpa := r.constructHPA(isvc, deploymentName)

	if err := controllerutil.SetControllerReference(
		isvc, hpa, r.Scheme,
	); err != nil {
		return fmt.Errorf(
			"failed to set controller reference on HPA: %w", err,
		)
	}

	existingHPA := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := r.Get(ctx, hpaName, existingHPA); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Creating HPA",
				"name", isvc.Name,
				"maxReplicas", isvc.Spec.Autoscaling.MaxReplicas)
			return r.Create(ctx, hpa)
		}
		return err
	}

	// Update existing HPA
	existingHPA.Spec = hpa.Spec
	return r.Update(ctx, existingHPA)
}

func (r *InferenceServiceReconciler) constructHPA(
	isvc *inferencev1alpha1.InferenceService,
	deploymentName string,
) *autoscalingv2.HorizontalPodAutoscaler {
	autoscaling := isvc.Spec.Autoscaling

	minReplicas := int32(1)
	if autoscaling.MinReplicas != nil {
		minReplicas = *autoscaling.MinReplicas
	}

	// Build metrics list
	var metrics []autoscalingv2.MetricSpec

	if len(autoscaling.Metrics) == 0 {
		// Use the runtime's default metric if available
		backend := resolveBackend(isvc)
		metricName := "llamacpp:requests_processing"
		if hp, ok := backend.(HPAMetricProvider); ok && hp.DefaultHPAMetric() != "" {
			metricName = hp.DefaultHPAMetric()
		}
		targetValue := resource.MustParse("2")
		metrics = []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.PodsMetricSourceType,
				Pods: &autoscalingv2.PodsMetricSource{
					Metric: autoscalingv2.MetricIdentifier{
						Name: metricName,
					},
					Target: autoscalingv2.MetricTarget{
						Type:         autoscalingv2.AverageValueMetricType,
						AverageValue: &targetValue,
					},
				},
			},
		}
	} else {
		for _, m := range autoscaling.Metrics {
			switch m.Type {
			case "Pods":
				var target autoscalingv2.MetricTarget
				if m.TargetAverageValue != nil {
					val := resource.MustParse(*m.TargetAverageValue)
					target = autoscalingv2.MetricTarget{
						Type:         autoscalingv2.AverageValueMetricType,
						AverageValue: &val,
					}
				}
				metrics = append(metrics, autoscalingv2.MetricSpec{
					Type: autoscalingv2.PodsMetricSourceType,
					Pods: &autoscalingv2.PodsMetricSource{
						Metric: autoscalingv2.MetricIdentifier{
							Name: m.Name,
						},
						Target: target,
					},
				})
			case "Resource":
				if m.TargetAverageUtilization != nil {
					metrics = append(
						metrics,
						autoscalingv2.MetricSpec{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								Name: corev1.ResourceName(m.Name),
								Target: autoscalingv2.MetricTarget{
									Type:               autoscalingv2.UtilizationMetricType,
									AverageUtilization: m.TargetAverageUtilization,
								},
							},
						},
					)
				}
			}
		}
	}

	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      isvc.Name,
			Namespace: isvc.Namespace,
			Labels: map[string]string{
				"app":                           isvc.Name,
				"inference.llmkube.dev/service": isvc.Name,
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
			MinReplicas: &minReplicas,
			MaxReplicas: autoscaling.MaxReplicas,
			Metrics:     metrics,
		},
	}
}
