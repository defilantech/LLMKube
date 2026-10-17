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
	"strings"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// hpaNameTakenRequeueAfter is the bounded requeue for a name-taken refusal.
// The refusal depends on cluster state outside the service's spec and nothing
// watches the foreign HPA, so the requeue is the only way deleting or
// renaming it takes effect without an unrelated event. Same cadence as the
// router-proxy and AgentGateway planes' identical refusals
// (routerProxyNameTakenRequeueAfter, agentGatewayConflictRequeueAfter).
const hpaNameTakenRequeueAfter = 2 * time.Minute

const (
	// ConditionAutoscalingReady reports whether the reconciler manages the HPA
	// named after the service. Diagnostic only: it never feeds phase or the
	// Available condition. The reason enumeration lives in the autoscaling
	// guide's table (docs/site/guides/metrics-driven-autoscaling.md):
	// True/HPAManaged, False/HPANameTaken (a foreign same-name HPA holds the
	// name, reported by reconcileHPA or by the Deployment-name refusal when
	// it stops the sequence before reconcileHPA runs),
	// False/DeploymentNameTaken (the Deployment-name refusal removed or
	// cannot provision the HPA), False/HPAStateUnknown (the HPA could not be
	// read), and absent when there is no HPA to report on (autoscaling not
	// configured, the service suspended, or a Metal workload).
	ConditionAutoscalingReady = "AutoscalingReady"

	// ReasonHPAManaged is set when AutoscalingReady=True: the HPA was created
	// or updated by this InferenceService.
	ReasonHPAManaged = "HPAManaged"

	// ReasonHPANameTaken is set when AutoscalingReady=False because an HPA
	// this InferenceService did not create already holds the generated name,
	// so the reconciler refuses to overwrite or delete it.
	ReasonHPANameTaken = "HPANameTaken"

	// ReasonHPAStateUnknown is set when AutoscalingReady=False because the
	// HPA could not be read (a non-NotFound Get failure while refusing a
	// name-taken Deployment), so the previous state cannot be verified or
	// corrected on this pass. The bounded requeue retries the read.
	ReasonHPAStateUnknown = "HPAStateUnknown"
)

// hpaNameTakenError reports an HPA whose generated name is held by an
// existing object this InferenceService does not control. reconcileHPA turns
// it into the AutoscalingReady condition and a bounded requeue instead of a
// reconcile error.
type hpaNameTakenError struct {
	ns, name, owner string
}

func (e *hpaNameTakenError) Error() string {
	if e.owner != "" {
		return fmt.Sprintf("HorizontalPodAutoscaler %s/%s is already controlled by another InferenceService (uid %s); "+
			"refusing to overwrite or delete it; rename one of the services", e.ns, e.name, e.owner)
	}
	return fmt.Sprintf("HorizontalPodAutoscaler %s/%s already exists and is not controlled by this InferenceService; "+
		"refusing to overwrite or delete it (LLMKube did not create it); delete or rename the object, or the service",
		e.ns, e.name)
}

// ensureHPAOwned is the ownership gate for every HPA write and delete: write
// only what this InferenceService controls. An HPA with the generated name
// but no controller reference, a foreign reference, or another
// InferenceService's reference is refused, never adopted, never overwritten,
// and never deleted.
func ensureHPAOwned(isvc *inferencev1alpha1.InferenceService, existing *autoscalingv2.HorizontalPodAutoscaler) error {
	if metav1.IsControlledBy(existing, isvc) {
		return nil
	}
	taken := &hpaNameTakenError{ns: existing.GetNamespace(), name: existing.GetName()}
	if ref := metav1.GetControllerOf(existing); ref != nil && ref.Kind == "InferenceService" {
		// The owner ref's APIVersion is "<group>/<version>" (or bare version
		// for the core group); match the group so a same-Kind controller in
		// another API group is not misreported as another InferenceService.
		if strings.SplitN(ref.APIVersion, "/", 2)[0] == inferencev1alpha1.GroupVersion.Group {
			taken.owner = string(ref.UID)
		}
	}
	return taken
}

// setHPAManagedCondition reports the service's own HPA as in place; the main
// status write later in the reconcile persists it.
func setHPAManagedCondition(isvc *inferencev1alpha1.InferenceService) {
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionAutoscalingReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: isvc.Generation,
		Reason:             ReasonHPAManaged,
		Message:            fmt.Sprintf("HorizontalPodAutoscaler %s/%s is managed by this InferenceService", isvc.Namespace, isvc.Name),
	})
}

// setHPANameTakenCondition reports a foreign same-name HPA on the service
// status and returns whether the False condition is new, so the caller emits
// one Warning event per episode rather than one per requeue.
func setHPANameTakenCondition(isvc *inferencev1alpha1.InferenceService, err error) (transitioned bool) {
	transitioned = !meta.IsStatusConditionFalse(isvc.Status.Conditions, ConditionAutoscalingReady)
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionAutoscalingReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: isvc.Generation,
		Reason:             ReasonHPANameTaken,
		Message:            err.Error(),
	})
	return transitioned
}

// removeAutoscalingReadyCondition drops the condition when there is no HPA
// to report on, the same removal semantics the CA-cert condition (#1995) uses
// when the state it reports does not apply.
func removeAutoscalingReadyCondition(isvc *inferencev1alpha1.InferenceService) {
	meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionAutoscalingReady)
}

// setHPARemovedForNameTaken records on AutoscalingReady that the service's
// HPA is not in place because a foreign Deployment holds the generated name:
// the refusal removed the operator-owned HPA so it cannot scale that object,
// or there was none left to remove. A destructive action must leave a status
// record, not only a transient event; reconcileHPA recreates the HPA and the
// condition returns to True when the name is free again. The removal lands
// before the refusal's status write, so a write conflict can leave the HPA
// deleted while the persisted condition still reads the previous state until
// the bounded requeue's next pass.
func setHPARemovedForNameTaken(isvc *inferencev1alpha1.InferenceService) {
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionAutoscalingReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: isvc.Generation,
		Reason:             inferencev1alpha1.ReasonDeploymentNameTaken,
		Message:            "the service's HPA is not in place while a foreign Deployment holds the generated name; it returns when the name is free",
	})
}

// reportHPANameTaken records a refusal on the condition and, on the
// transition into it, as a Warning event.
func (r *InferenceServiceReconciler) reportHPANameTaken(isvc *inferencev1alpha1.InferenceService, err error) {
	if setHPANameTakenCondition(isvc, err) && r.Recorder != nil {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, ReasonHPANameTaken, "Reconcile", "%s", err.Error())
	}
}

// HorizontalPodAutoscaler lifecycle. Runs as part of the main reconcile when
// spec.autoscaling is set; removes the HPA when it is cleared. Metal
// accelerator workloads skip HPA entirely since they run outside a Deployment.
// Metric selection falls through to the configured runtime's DefaultHPAMetric
// when the CRD does not supply an explicit metrics list.

// reconcileHPA manages the HPA named after the service. A foreign same-name
// HPA is reported through the AutoscalingReady condition instead of an
// error: the HPA is optional, so a refusal must not abort the rest of the
// reconcile. The returned duration is the bounded requeue for a refusal, 0
// otherwise.
func (r *InferenceServiceReconciler) reconcileHPA(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	deploymentName string,
	isMetal bool,
) (time.Duration, error) {
	logger := logf.FromContext(ctx)
	hpaName := types.NamespacedName{
		Name:      isvc.Name,
		Namespace: isvc.Namespace,
	}

	// If autoscaling is not configured, or the service is suspended, clean
	// up any existing HPA. Suspension must win over autoscaling: otherwise
	// the HPA's minReplicas (defaults to 1) would scale the Deployment back
	// above zero on its own reconcile loop.
	if isvc.Spec.Autoscaling == nil || isvc.Spec.Suspend {
		existingHPA := &autoscalingv2.HorizontalPodAutoscaler{}
		err := r.Get(ctx, hpaName, existingHPA)
		if err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
		if err == nil {
			if err := ensureHPAOwned(isvc, existingHPA); err != nil {
				r.reportHPANameTaken(isvc, err)
				logger.Info("Refusing to delete an HPA this InferenceService does not control",
					"error", err)
				return hpaNameTakenRequeueAfter, nil
			}
			reason := "Autoscaling removed"
			if isvc.Spec.Suspend {
				reason = "InferenceService suspended"
			}
			logger.Info(reason+", deleting HPA",
				"name", isvc.Name)
			if err := r.Delete(ctx, existingHPA); err != nil {
				return 0, fmt.Errorf("failed to delete HPA: %w", err)
			}
		}
		removeAutoscalingReadyCondition(isvc)
		return 0, nil
	}

	// Skip HPA for Metal accelerator (no Deployment to scale)
	if isMetal {
		logger.Info("Skipping HPA for Metal accelerator workload")
		removeAutoscalingReadyCondition(isvc)
		return 0, nil
	}

	hpa := r.constructHPA(isvc, deploymentName)

	if err := setControllerReferenceUnblocked(
		isvc, hpa, r.Scheme,
	); err != nil {
		return 0, fmt.Errorf(
			"failed to set controller reference on HPA: %w", err,
		)
	}

	existingHPA := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := r.Get(ctx, hpaName, existingHPA); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Creating HPA",
				"name", isvc.Name,
				"maxReplicas", isvc.Spec.Autoscaling.MaxReplicas)
			if err := r.Create(ctx, hpa); err != nil {
				return 0, err
			}
			setHPAManagedCondition(isvc)
			return 0, nil
		}
		return 0, err
	}

	if err := ensureHPAOwned(isvc, existingHPA); err != nil {
		r.reportHPANameTaken(isvc, err)
		logger.Info("Refusing to update an HPA this InferenceService does not control",
			"error", err)
		return hpaNameTakenRequeueAfter, nil
	}

	// Update existing HPA
	existingHPA.Spec = hpa.Spec
	if err := r.Update(ctx, existingHPA); err != nil {
		return 0, err
	}
	setHPAManagedCondition(isvc)
	return 0, nil
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
