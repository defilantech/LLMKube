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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// AnnotationDesiredTemplateHash stores a SHA-256 hash of the desired pod template
// on the Deployment. The reconciler uses this to detect meaningful template changes
// without being fooled by API-server-applied defaults that differ between the
// in-memory object and the persisted object.
const AnnotationDesiredTemplateHash = "llmkube.ai/desired-template-hash"

// desiredTemplateHash computes a deterministic hash of the pod template for the
// purpose of detecting operator-driven changes. It serializes the normalized
// template to JSON and hashes it, so API-server defaulting differences don't
// produce false positives on subsequent reconciles.
func desiredTemplateHash(template corev1.PodTemplateSpec) string {
	normalized := computePodTemplateForComparison(template)
	data, _ := json.Marshal(normalized)
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:8])
}

// llamaCPUSlot represents a single slot from the llama.cpp /slots endpoint.
type llamaCPUSlot struct {
	ID int `json:"id"`
	// IsProcessing is true while the slot is handling a request (prompt
	// evaluation or generation). A slot is idle when this is false.
	IsProcessing bool `json:"is_processing"`
}

// checkServerIdle checks whether all slots on a llama.cpp server are idle.
// It queries the /slots endpoint and returns true only if every slot reports
// is_processing == false. A server error (unreachable, non-200, unparseable
// body) is surfaced as an error to the caller, which treats it as fail-closed
// (defer the rollout) — see reconcileRolloutPolicy.
// baseURL should include the port (e.g., "http://svc.ns.svc.cluster.local:8080").
func (r *InferenceServiceReconciler) checkServerIdle(ctx context.Context, baseURL string) (bool, error) {
	url := fmt.Sprintf("%s/slots", baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create request: %w", err)
	}

	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to query /slots: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("/slots returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("failed to read /slots response: %w", err)
	}

	var slots []llamaCPUSlot
	if err := json.Unmarshal(body, &slots); err != nil {
		return false, fmt.Errorf("failed to parse /slots response: %w", err)
	}

	for _, slot := range slots {
		if slot.IsProcessing {
			return false, nil
		}
	}

	return true, nil
}

// checkServiceIdle checks whether the InferenceService Service currently routes
// to an idle backend. This targets the single-replica local-model case from
// #856; multi-replica per-pod draining can be layered on later.
func (r *InferenceServiceReconciler) checkServiceIdle(ctx context.Context, isvc *inferencev1alpha1.InferenceService, svc *corev1.Service) (bool, error) {
	log := logf.FromContext(ctx)

	var svcURL string
	if r.RolloutIdleBaseURL != "" {
		svcURL = r.RolloutIdleBaseURL
	} else {
		port := int32(8080)
		if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
			port = isvc.Spec.Endpoint.Port
		} else if isvc.Spec.ContainerPort != nil {
			port = *isvc.Spec.ContainerPort
		}
		svcURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", svc.Name, svc.Namespace, port)
	}
	idle, err := r.checkServerIdle(ctx, svcURL)
	if err != nil {
		log.Info("Failed to check server idle status", "error", err)
		return false, err
	}

	if !idle {
		log.Info("Backend slots are busy, deferring rollout")
	} else {
		log.Info("All backend slots are idle, proceeding with rollout")
	}

	return idle, nil
}

// reconcileRolloutPolicy checks whether the rollout should be deferred based on
// the RolloutPolicy configuration. Returns a reconciliation result if the rollout
// is deferred, or ctrl.Result{} if the rollout can proceed.
func (r *InferenceServiceReconciler) reconcileRolloutPolicy(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	svc *corev1.Service,
) (ctrl.Result, error) {
	if !isvc.RolloutPolicyEnabled() {
		// Policy disabled (nil or waitForIdle=false). Clear any stale condition.
		if meta.FindStatusCondition(isvc.Status.Conditions, ConditionRolloutDeferred) != nil {
			meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionRolloutDeferred)
			if updateErr := r.Status().Update(ctx, isvc); updateErr != nil {
				return ctrl.Result{}, fmt.Errorf("failed to clear stale RolloutDeferred condition: %w", updateErr)
			}
		}
		return ctrl.Result{}, nil
	}

	if !isvc.ShouldDeferRollout() {
		// force=true: clear any stale condition.
		if meta.FindStatusCondition(isvc.Status.Conditions, ConditionRolloutDeferred) != nil {
			meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionRolloutDeferred)
			if updateErr := r.Status().Update(ctx, isvc); updateErr != nil {
				return ctrl.Result{}, fmt.Errorf("failed to clear RolloutDeferred on force: %w", updateErr)
			}
		}
		return ctrl.Result{}, nil
	}

	log := logf.FromContext(ctx)
	existingCond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionRolloutDeferred)
	now := metav1.Now()

	idle, checkErr := r.checkServiceIdle(ctx, isvc, svc)

	if checkErr == nil && idle {
		if existingCond != nil {
			meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionRolloutDeferred)
			if updateErr := r.Status().Update(ctx, isvc); updateErr != nil {
				return ctrl.Result{}, fmt.Errorf("failed to clear RolloutDeferred condition: %w", updateErr)
			}
		}
		log.Info("All slots idle, proceeding with rollout")
		return ctrl.Result{}, nil
	}

	// Not idle, or the idle check itself failed. Fail closed: defer the
	// rollout until the backend is idle or the idleTimeoutSeconds budget is
	// spent. A failing /slots probe (server unreachable, non-200, --slots
	// disabled so it 404s) must not silently roll and drop in-flight
	// generations — that is exactly what waitForIdle is meant to prevent.
	reason := ReasonPodsBusy
	message := "Backend slots are busy, waiting for idle before rollout"
	if checkErr != nil {
		log.Info("Idle check failed; deferring rollout (fail-closed)", "error", checkErr)
		reason = ReasonIdleCheckFailed
		message = fmt.Sprintf("Idle check failed (%v); deferring rollout until idle or timeout", checkErr)
	} else {
		log.Info("Backend slots are busy, deferring rollout")
	}

	timeout := time.Duration(isvc.Spec.RolloutPolicy.IdleTimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	var deferSince time.Time
	if existingCond != nil && !existingCond.LastTransitionTime.Time.IsZero() {
		deferSince = existingCond.LastTransitionTime.Time
	} else {
		deferSince = now.Time
	}

	if time.Since(deferSince) > timeout {
		log.Info("Idle timeout exceeded, proceeding with rollout despite busy slots")
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type:               ConditionRolloutDeferred,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             ReasonIdleTimeoutExceeded,
			Message:            fmt.Sprintf("Idle timeout of %v exceeded, proceeding with rollout", timeout),
		})
		if updateErr := r.Status().Update(ctx, isvc); updateErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update RolloutDeferred condition on timeout: %w", updateErr)
		}
		return ctrl.Result{}, nil
	}

	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionRolloutDeferred,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: isvc.Generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
	if updateErr := r.Status().Update(ctx, isvc); updateErr != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set RolloutDeferred condition: %w", updateErr)
	}

	requeueAfter := inferencev1alpha1.DefaultIdleCheckInterval
	if timeout-requeueAfter < requeueAfter {
		requeueAfter = timeout - time.Since(deferSince)
	}
	if requeueAfter < 0 {
		requeueAfter = 0
	}

	log.Info("Deferring rollout, backend slots are busy", "requeueAfter", requeueAfter)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// computePodTemplateForComparison returns the PodTemplateSpec for comparison
// purposes. Exposed as a function so the comparison logic in the reconciler
// remains readable and can be extended if needed (e.g., stripping transient
// fields injected by the API server).
func computePodTemplateForComparison(t corev1.PodTemplateSpec) corev1.PodTemplateSpec {
	return t
}

// podTemplatesDiffer reports whether two PodTemplateSpecs differ in fields
// that would trigger a Deployment rollout. It compares operator-controlled
// fields (containers, init containers, volumes, labels, annotations, and
// scheduling fields) while ignoring API-server-applied defaults like
// TerminationGracePeriodSeconds that cause false positives on full DeepEqual.
func podTemplatesDiffer(existing, desired corev1.PodTemplateSpec) bool {
	// Deep-copy both templates up front. The normalization below mutates
	// container SecurityContexts and other fields in place, and a shallow
	// struct copy still shares the underlying slices and pointers with the
	// caller's templates. Without this, normalizing `desired` for comparison
	// nils out the real desired template's SecurityContext (e.g. the
	// model-cache-prep init container's RunAsUser/Capabilities), which then
	// gets applied to the Deployment. See #922.
	existing = *existing.DeepCopy()
	desired = *desired.DeepCopy()
	if !apiequality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(existing.Annotations, desired.Annotations) {
		return true
	}
	a, b := existing.Spec, desired.Spec
	// Compare fields the operator controls and that trigger rollouts.
	// Skip server-side defaulted fields that cause false positives:
	//   TerminationGracePeriodSeconds (default 30s), DNSPolicy (default ClusterFirst),
	//   RestartPolicy (default Always), ServiceAccountName (default "default"),
	//   AutomountServiceAccountToken, SchedulerName (default "default-scheduler").
	a.TerminationGracePeriodSeconds = nil
	b.TerminationGracePeriodSeconds = nil
	a.DNSPolicy = ""
	b.DNSPolicy = ""
	a.RestartPolicy = ""
	b.RestartPolicy = ""
	a.ServiceAccountName = ""
	b.ServiceAccountName = ""
	a.AutomountServiceAccountToken = nil
	b.AutomountServiceAccountToken = nil
	a.SchedulerName = ""
	b.SchedulerName = ""
	// Additional pod-level fields defaulted by the API server.
	a.HostNetwork = false
	b.HostNetwork = false
	a.HostPID = false
	b.HostPID = false
	a.HostIPC = false
	b.HostIPC = false
	a.ShareProcessNamespace = nil
	b.ShareProcessNamespace = nil
	a.Hostname = ""
	b.Hostname = ""
	a.Subdomain = ""
	b.Subdomain = ""
	a.SetHostnameAsFQDN = nil
	b.SetHostnameAsFQDN = nil
	a.DNSConfig = nil
	b.DNSConfig = nil
	a.ImagePullSecrets = nil
	b.ImagePullSecrets = nil
	a.HostAliases = nil
	b.HostAliases = nil
	// Normalize pod-level SecurityContext (API server defaults seccompProfile).
	if a.SecurityContext != nil {
		a.SecurityContext.SeccompProfile = nil
	}
	if b.SecurityContext != nil {
		b.SecurityContext.SeccompProfile = nil
	}
	// Normalize containers to strip API-server-applied defaults.
	normalizeContainers(a.Containers)
	normalizeContainers(b.Containers)
	normalizeContainers(a.InitContainers)
	normalizeContainers(b.InitContainers)
	if !apiequality.Semantic.DeepEqual(a.Containers, b.Containers) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(a.InitContainers, b.InitContainers) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(a.EphemeralContainers, b.EphemeralContainers) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(a.Volumes, b.Volumes) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(a.SecurityContext, b.SecurityContext) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(a.EnableServiceLinks, b.EnableServiceLinks) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(a.NodeSelector, b.NodeSelector) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(a.Tolerations, b.Tolerations) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(a.Affinity, b.Affinity) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(a.TopologySpreadConstraints, b.TopologySpreadConstraints) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(a.PriorityClassName, b.PriorityClassName) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(a.RuntimeClassName, b.RuntimeClassName) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(a.ResourceClaims, b.ResourceClaims) {
		return true
	}
	return false
}

// normalizeContainers strips API-server-applied defaults from container fields
// so that comparison doesn't produce false positives.
func normalizeContainers(containers []corev1.Container) {
	for i := range containers {
		c := &containers[i]
		c.TerminationMessagePath = ""
		c.TerminationMessagePolicy = ""
		c.ImagePullPolicy = ""
		c.WorkingDir = ""
		if c.SecurityContext != nil {
			c.SecurityContext.Privileged = nil
			c.SecurityContext.RunAsUser = nil
			c.SecurityContext.RunAsGroup = nil
			c.SecurityContext.RunAsNonRoot = nil
			c.SecurityContext.ReadOnlyRootFilesystem = nil
			c.SecurityContext.AllowPrivilegeEscalation = nil
			c.SecurityContext.SeccompProfile = nil
			c.SecurityContext.Capabilities = nil
			c.SecurityContext.ProcMount = nil
		}
		for j := range c.Ports {
			if c.Ports[j].Protocol == "" {
				c.Ports[j].Protocol = corev1.ProtocolTCP
			}
		}
		// Normalize resource requirements: empty and nil are semantically identical.
		if len(c.Resources.Limits) == 0 {
			c.Resources.Limits = nil
		}
		if len(c.Resources.Requests) == 0 {
			c.Resources.Requests = nil
		}
		// Normalize env var field/ref defaults (FieldPath API version).
		for j := range c.Env {
			if c.Env[j].ValueFrom != nil && c.Env[j].ValueFrom.FieldRef != nil {
				c.Env[j].ValueFrom.FieldRef.APIVersion = ""
			}
		}
	}
}
