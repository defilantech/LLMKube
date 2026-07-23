package controller

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	maxPodLifetimeSeconds int64 = 9223372036
	podLifetimeRetry            = 30 * time.Second
)

// reconcilePodLifetime recycles at most one healthy, expired pod. All
// workload checks are deliberately conservative: a watch will cause another
// attempt after the Deployment has settled or a replacement pod is ready.
// now is injected so tests can drive the deadline arithmetic.
func (r *InferenceServiceReconciler) reconcilePodLifetime(ctx context.Context, isvc *inferencev1alpha1.InferenceService, isMetal bool, now time.Time) (time.Duration, error) {
	if isMetal || isvc.Spec.MaxPodLifetimeSeconds == nil {
		return 0, nil
	}

	deployment, err := r.getStableDeployment(ctx, isvc)
	if err != nil || deployment == nil {
		return 0, err
	}
	active, err := r.activePods(ctx, deployment)
	if err != nil {
		return 0, err
	}
	// stableDeployment already proved Status.Replicas is the desired count, and
	// it only counts pods the Deployment owns. A foreign pod sharing the
	// selector therefore fails this check and holds recycling rather than
	// risking an eviction the operator does not own.
	if len(active) != int(deployment.Status.Replicas) || !allPodsReady(active) {
		return 0, nil
	}
	return r.recycleExpiredPod(ctx, isvc, active, boundedSeconds(*isvc.Spec.MaxPodLifetimeSeconds), now)
}

func (r *InferenceServiceReconciler) getStableDeployment(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace}, deployment)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !stableDeployment(deployment, isvc) {
		return nil, nil
	}
	return deployment, nil
}

// recycleExpiredPod evicts the pod that expired first, or reports how long
// until the next one does. Ties break on name so repeated reconciles of the
// same state pick the same pod.
func (r *InferenceServiceReconciler) recycleExpiredPod(ctx context.Context, isvc *inferencev1alpha1.InferenceService, active []*corev1.Pod, lifetime time.Duration, now time.Time) (time.Duration, error) {
	var expired *corev1.Pod
	var expiredDeadline time.Time
	var earliest time.Duration
	for _, pod := range active {
		deadline := pod.Status.StartTime.Time.Add(lifetime)
		if deadline.After(now) {
			earliest = earliestPositive(earliest, deadline.Sub(now))
			continue
		}
		if expired == nil || deadline.Before(expiredDeadline) ||
			(deadline.Equal(expiredDeadline) && pod.Name < expired.Name) {
			expired, expiredDeadline = pod, deadline
		}
	}
	if expired == nil {
		return earliest, nil
	}
	// waitForIdle promises never to drop in-flight generations, so recycling
	// waits for an idle window rather than killing a busy pod on a timer.
	if isvc.RolloutPolicyEnabled() && !idleWaitExhausted(isvc, expiredDeadline, now) {
		idle, err := r.checkServiceIdle(ctx, isvc, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sanitizeDNSName(isvc.Name), Namespace: isvc.Namespace}})
		if err != nil || !idle {
			// Fail closed, matching reconcileRolloutPolicy: an idle probe that
			// cannot answer must not authorise killing a possibly-busy pod.
			logf.FromContext(ctx).Info("Backend not idle, holding pod recycle", "pod", expired.Name, "error", err)
			return inferencev1alpha1.DefaultIdleCheckInterval, nil
		}
	}
	return r.evictPod(ctx, isvc, expired)
}

// idleWaitExhausted reports whether maxPodLifetimeIdleTimeoutSeconds says to
// stop waiting for idle and recycle a still-busy pod. The budget runs from the
// pod's own expiry, so the deadline is derivable from the pod and nothing has
// to be persisted across reconciles. Omitted means wait indefinitely; 0 means
// never wait, since the caller only reaches here once deadline has passed.
func idleWaitExhausted(isvc *inferencev1alpha1.InferenceService, deadline time.Time, now time.Time) bool {
	timeout := isvc.Spec.MaxPodLifetimeIdleTimeoutSeconds
	if timeout == nil {
		return false
	}
	return !now.Before(deadline.Add(boundedSeconds(*timeout)))
}

func (r *InferenceServiceReconciler) evictPod(ctx context.Context, isvc *inferencev1alpha1.InferenceService, pod *corev1.Pod) (time.Duration, error) {
	eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}, DeleteOptions: &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}}}
	err := r.SubResource("eviction").Create(ctx, pod, eviction)
	switch {
	case apierrors.IsNotFound(err):
		return 0, nil
	case apierrors.IsTooManyRequests(err):
		// A PodDisruptionBudget rejected the eviction; nothing watches that, so
		// the retry has to be a timer.
		logf.FromContext(ctx).Info("Eviction blocked by a PodDisruptionBudget, retrying later", "pod", pod.Name, "retryAfter", podLifetimeRetry)
		return podLifetimeRetry, nil
	case apierrors.IsForbidden(err):
		// Missing pods/eviction RBAC — seen for real when the operator image is
		// upgraded without the chart that owns its ClusterRole. Returning an
		// error here would fail the whole InferenceService reconcile and retry
		// hot forever, since no amount of retrying grants a permission. Surface
		// it where an operator will actually look, and back off.
		logf.FromContext(ctx).Error(err, "Not permitted to evict pods; recycling is disabled until the operator is granted create on pods/eviction", "pod", pod.Name)
		if r.Recorder != nil {
			r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, "PodRecycleForbidden", "Reconcile",
				"Cannot recycle pod %s: %v; the operator needs create on pods/eviction", pod.Name, err)
		}
		return podLifetimeRetry, nil
	case err != nil:
		return 0, err
	}
	// A pod disappearing on a timer is invisible in `kubectl describe isvc`
	// without this.
	if r.Recorder != nil {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeNormal, "PodRecycled", "Reconcile",
			"Evicted pod %s after exceeding maxPodLifetimeSeconds; the Deployment will create a replacement", pod.Name)
	}
	return 0, nil
}

func stableDeployment(deployment *appsv1.Deployment, isvc *inferencev1alpha1.InferenceService) bool {
	if !metav1.IsControlledBy(deployment, isvc) || deployment.Generation == 0 || deployment.Status.ObservedGeneration != deployment.Generation {
		return false
	}
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}
	return deployment.Status.Replicas == replicas && deployment.Status.UpdatedReplicas == replicas && deployment.Status.ReadyReplicas == replicas && deployment.Status.AvailableReplicas == replicas
}

// activePods lists the non-terminal pods matching the Deployment's selector.
// The selector labels are unique per InferenceService, and the caller
// cross-checks the count against Status.Replicas, so this deliberately does not
// walk ReplicaSets to prove ownership pod by pod.
func (r *InferenceServiceReconciler) activePods(ctx context.Context, deployment *appsv1.Deployment) ([]*corev1.Pod, error) {
	listOpts := []client.ListOption{client.InNamespace(deployment.Namespace)}
	if deployment.Spec.Selector != nil {
		listOpts = append(listOpts, client.MatchingLabels(deployment.Spec.Selector.MatchLabels))
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, listOpts...); err != nil {
		return nil, err
	}
	active := make([]*corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		active = append(active, pod)
	}
	return active, nil
}

func allPodsReady(pods []*corev1.Pod) bool {
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil || pod.Status.StartTime == nil || !podReady(pod) {
			return false
		}
	}
	return true
}

// boundedSeconds converts a spec field to a Duration, clamping it so an
// unbounded value cannot overflow into the past.
func boundedSeconds(seconds int64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	if seconds > maxPodLifetimeSeconds {
		seconds = maxPodLifetimeSeconds
	}
	return time.Duration(seconds) * time.Second
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
