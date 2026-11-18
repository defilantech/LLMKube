/*
Copyright 2026.

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
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

// AnnotationDesiredTemplateHash stores a SHA-256 hash of the desired pod template
// on the Deployment. The reconciler stamps the hash of the freshly built desired
// template and compares it against the stored value on each reconcile to detect
// operator-driven changes. Both sides are derived from the desired template (not
// the persisted live object), so API-server defaulting on the live object cannot
// produce a false positive. A Deployment missing the annotation predates it and
// is treated as changed (drain before roll).
const AnnotationDesiredTemplateHash = "llmkube.ai/desired-template-hash"

var errIdleUnsupported = errors.New("runtime does not implement idle detection")

// errNoEndpoints is returned by checkServiceIdle when the Service has
// EndpointSlices but none of them carries an address: the service is scaled to
// zero, or every pod is gone. Whether that means "nothing to drain" or "pods
// exist but are not addressable yet" depends on the pod count, which the
// caller has, so the caller decides.
var errNoEndpoints = errors.New("service has no endpoints to probe")

// desiredTemplateHash computes a deterministic hash of the pod template for the
// purpose of detecting operator-driven changes. It serializes the template to
// JSON and hashes it. The reconciler stamps this hash on the Deployment and
// compares it against a freshly computed hash to detect real changes; a missing
// stored hash means the pre-annotation legacy path and is treated as changed.
func desiredTemplateHash(template corev1.PodTemplateSpec) string {
	data, _ := json.Marshal(template)
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:8])
}

// parsePrometheusGaugeSum scans Prometheus exposition text for lines matching
// metricName (with optional labels in curly braces or bare gauge), sums the
// numeric values, and returns (sum, found). Lines starting with '#' are skipped.
func parsePrometheusGaugeSum(body, metricName string) (float64, bool) {
	var sum float64
	found := false
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' {
			continue
		}
		if !strings.HasPrefix(line, metricName) {
			continue
		}
		rest := line[len(metricName):]
		if len(rest) == 0 || (rest[0] != '{' && rest[0] != ' ') {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		sum += val
		found = true
	}
	return sum, found
}

// resolveIdlePort returns the port to use for idle-check HTTP probes. It
// follows the same priority as the existing checkServiceIdle: Endpoint.Port >
// ContainerPort > backend.DefaultPort().
func resolveIdlePort(isvc *inferencev1alpha1.InferenceService, backend RuntimeBackend) int32 {
	if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		return isvc.Spec.Endpoint.Port
	}
	if isvc.Spec.ContainerPort != nil {
		return *isvc.Spec.ContainerPort
	}
	return backend.DefaultPort()
}

// collectReadyReplicaURLs builds a list of "http://<addr>:<port>" URLs from
// the ready endpoints in the given EndpointSliceList. Per Kubernetes convention,
// an endpoint with Conditions.Ready == nil is treated as ready. It delegates to
// router.ReadyEndpointURLs so the proxy applies the same rule.
func collectReadyReplicaURLs(slices *discoveryv1.EndpointSliceList, port int32) []string {
	ptrs := make([]*discoveryv1.EndpointSlice, 0, len(slices.Items))
	for i := range slices.Items {
		ptrs = append(ptrs, &slices.Items[i])
	}
	return router.ReadyEndpointURLs(ptrs, port)
}

// collectNotReadyReplicaURLs builds a list of "http://<addr>:<port>" URLs from
// the not-ready endpoints in the given EndpointSliceList. Per Kubernetes
// convention, an endpoint with Conditions.Ready == nil is treated as ready, so
// only endpoints explicitly marked not-ready are collected here. A not-ready
// pod has been removed from Service endpoints (so new requests stop arriving)
// but generations already accepted keep running, so it must still be probed.
func collectNotReadyReplicaURLs(slices *discoveryv1.EndpointSliceList, port int32) []string {
	var urls []string
	for i := range slices.Items {
		for j := range slices.Items[i].Endpoints {
			ep := &slices.Items[i].Endpoints[j]
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				continue
			}
			for _, addr := range ep.Addresses {
				urls = append(urls, fmt.Sprintf("http://%s:%d", addr, port))
			}
		}
	}
	return urls
}

// checkServiceIdle checks whether the InferenceService Service currently routes
// to idle backends. It resolves the backend for the given InferenceService,
// type-asserts to IdleDetector, and probes each Ready replica via EndpointSlices
// (or falls back to a single Service URL when no EndpointSlices exist).
func (r *InferenceServiceReconciler) checkServiceIdle(ctx context.Context, isvc *inferencev1alpha1.InferenceService, svc *corev1.Service) (bool, error) {
	log := logf.FromContext(ctx)

	backend := resolveBackend(isvc)
	detector, ok := backend.(IdleDetector)
	if !ok {
		return false, errIdleUnsupported
	}

	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	probe := detector.IdleProbe(isvc, httpClient)

	port := resolveIdlePort(isvc, backend)

	var svcURL string
	if r.RolloutIdleBaseURL != "" {
		svcURL = r.RolloutIdleBaseURL
	} else {
		svcURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", svc.Name, svc.Namespace, port)
	}

	slices := &discoveryv1.EndpointSliceList{}
	svcName := sanitizeDNSName(svc.Name)
	if err := r.List(ctx, slices, client.InNamespace(isvc.Namespace), client.MatchingLabels{"kubernetes.io/service-name": svcName}); err != nil {
		log.Info("Failed to list EndpointSlices, falling back to Service URL", "error", err)
	}

	if len(slices.Items) == 0 {
		idle, err := probe(ctx, svcURL)
		if err != nil {
			if errors.Is(err, errIdleUnsupported) {
				// The runtime cannot be idle-checked at all (e.g. a generic
				// runtime with no idle annotation). Preserve that signal so the
				// caller defers with IdleCheckUnsupported rather than treating it
				// as an empty endpoint list and rolling over live work.
				return false, err
			}
			// No EndpointSlice exists and the Service URL probe could not reach a
			// backend. Upstream's endpointslice reconciler deletes a slice once
			// its last endpoint is removed, so a scaled-to-zero Service has none
			// and its URL has nothing behind it. Returning the raw error defers
			// with IdleCheckFailed until idleTimeoutSeconds — the #1793 deadlock,
			// reached through the fallback rather than the empty-slice path. Hand
			// the decision to the caller's pod-count resolution, symmetric with
			// the empty-endpoints path below: it proceeds only when no pods
			// exist either, and stays deferred (fail-closed) when one does.
			log.Info("No EndpointSlices and the Service URL probe could not reach a backend; deferring to pod-count resolution", "error", err)
			return false, errNoEndpoints
		}
		if !idle {
			log.Info("Backend is busy (Service URL fallback), deferring rollout")
		} else {
			log.Info("Backend is idle (Service URL fallback), proceeding with rollout")
		}
		return idle, nil
	}

	// Probe both ready and not-ready endpoints. A not-ready pod has been
	// removed from Service endpoints so new requests stop arriving, but
	// generations already accepted keep running, so it must still be probed
	// rather than inferred idle from its readiness.
	replicaURLs := collectReadyReplicaURLs(slices, port)
	replicaURLs = append(replicaURLs, collectNotReadyReplicaURLs(slices, port)...)
	if len(replicaURLs) == 0 {
		// Nothing reachable: no ready and no not-ready addresses at all.
		// This is the crashloop / scale-to-zero case (#1250). We cannot tell
		// "scaled to zero, nothing to protect" from "unready but still
		// working" from the endpoints alone, so hand the decision back to the
		// caller, which knows the pod count.
		return false, errNoEndpoints
	}

	for _, url := range replicaURLs {
		idle, err := probe(ctx, url)
		if err != nil {
			log.Info("Failed to check replica idle status", "url", url, "error", err)
			return false, err
		}
		if !idle {
			log.Info("Replica is busy, deferring rollout", "url", url)
			return false, nil
		}
	}

	log.Info("All replicas are idle, proceeding with rollout")
	return true, nil
}

// countOldPods lists the pods owned by the InferenceService and returns the
// total count, how many are Ready and how many are running a serving container.
// A pod is Ready when its PodReady condition is True. It is running when the
// runtime's container reports State.Running, terminating pods included: a
// container that is not running holds no in-flight request, whatever its
// readiness. It differs from classifyMemberPods in modelpool_idle.go, which
// skips terminating pods and accepts any running container; this side has to
// name the serving container because the drain decision is about it alone.
// reconcileRolloutPolicy uses the counts to decide whether there is in-flight
// work to protect before deferring a rollout.
//
// A List error is returned rather than swallowed as a zero count. The caller
// promotes a zero count to a rollout gate (proceed when nothing is left to
// drain), so a transient List failure reported as zero would roll a pod that
// may still hold work. The caller must fail closed on the error.
func (r *InferenceServiceReconciler) countOldPods(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (total, ready, running int32, err error) {
	podList := &corev1.PodList{}
	labels := client.MatchingLabels{
		"app":                           isvc.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}
	if listErr := r.List(ctx, podList, client.InNamespace(isvc.Namespace), labels); listErr != nil {
		return 0, 0, 0, listErr
	}
	containerName := resolveBackend(isvc).ContainerName()
	for i := range podList.Items {
		pod := &podList.Items[i]
		total++
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready++
				break
			}
		}
		if podServingContainerRunning(pod, containerName) {
			running++
		}
	}
	return total, ready, running, nil
}

// podServingContainerRunning reports whether the pod is currently running its
// serving container. A terminating pod still counts: its container keeps the
// generations it already accepted until the process exits, so the rollout must
// wait for it exactly as it waits for any other running container.
//
// The deployment builder gives the pod exactly one container
// (deployment_builder.go), so a pod with a single reported container status
// is that container even when its name differs from the runtime's.
func podServingContainerRunning(pod *corev1.Pod, containerName string) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == containerName || len(pod.Spec.Containers) == 1 {
			return cs.State.Running != nil
		}
	}
	return false
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

	// Count old-generation pods for the mixed-state reason below. Readiness is
	// NOT used as a proxy for idleness: a pod with PodReady=False has been
	// removed from Service endpoints so new requests stop arriving, but
	// generations already accepted keep running. checkServiceIdle probes both
	// ready and not-ready endpoints, so we always consult it rather than
	// proceeding on the basis of readiness alone. A pod whose serving
	// container is not running is the one case that can be settled without a
	// probe: it holds no request.
	totalPods, _, runningPods, countErr := r.countOldPods(ctx, isvc)

	idle, checkErr := r.checkServiceIdle(ctx, isvc, svc)
	if errors.Is(checkErr, errNoEndpoints) {
		// A service scaled to zero (a stopped ModelPool member being started,
		// or a template change that landed while replicas was 0) has no
		// in-flight work to protect. Treating the empty endpoint list as
		// "busy" deadlocked the scale-up until idleTimeoutSeconds. When pods
		// do exist but none is addressable yet, keep the fail-closed path.
		switch {
		case countErr != nil:
			// The pod list failed, so we cannot prove nothing is left to
			// drain. This gate promotes a zero count to "proceed", so a
			// transient List error must defer rather than roll a pod that may
			// still hold work.
			idle, checkErr = false, nil
			log.Info("No endpoints and the pod list failed; deferring rollout (fail-closed)", "error", countErr)
		case totalPods == 0:
			idle, checkErr = true, nil
			log.Info("No pods and no endpoints, nothing to drain; proceeding with rollout")
		default:
			idle, checkErr = false, nil
		}
	}

	if countErr == nil && totalPods > 0 && runningPods == 0 && !idle {
		// No old pod is running its serving container, so none can hold a
		// request (#2553), whether the probe could not answer or was never
		// reached (no endpoint address). A running pod's unanswered probe
		// still defers, which is what #1543 kept.
		idle, checkErr = true, nil
		log.Info("No serving container is running; nothing to protect, proceeding with rollout", "totalPods", totalPods)
	}

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
	// spent. A failing idle probe (server unreachable, non-200, unparseable
	// metrics, or unsupported runtime) must not silently roll and drop
	// in-flight generations — that is exactly what waitForIdle is meant to prevent.
	reason := ReasonPodsBusy
	message := "Backend slots are busy, waiting for idle before rollout"
	if checkErr != nil {
		log.Info("Idle check failed; deferring rollout (fail-closed)", "error", checkErr)
		if errors.Is(checkErr, errIdleUnsupported) {
			reason = ReasonIdleCheckUnsupported
			message = fmt.Sprintf("Runtime does not support idle detection (%v); deferring rollout until idle or timeout", checkErr)
		} else {
			reason = ReasonIdleCheckFailed
			message = fmt.Sprintf("Idle check failed (%v); deferring rollout until idle or timeout", checkErr)
		}
	} else if totalPods > runningPods {
		// Mixed state: some old pods are running a serving container while
		// others are not. The rollout is deferred to protect in-flight work
		// on the running pods, but the reason must distinguish this from the
		// pure busy case so an operator can see why the rollout is waiting.
		// The not-running pods hold nothing, so only the running count says
		// how many are being protected (#2553).
		reason = ReasonPodsCrashLooping
		message = fmt.Sprintf("%d of %d old-generation pods are not running their serving container; deferring rollout to protect in-flight work on running pods", totalPods-runningPods, totalPods)
		log.Info("Mixed pod state, deferring rollout to protect in-flight work", "totalPods", totalPods, "runningPods", runningPods)
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
