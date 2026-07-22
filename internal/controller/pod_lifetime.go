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

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	maxPodLifetimeSeconds int64 = 9223372036
	podLifetimeRetry            = 30 * time.Second
)

// earliestPositive merges controller timers without allowing a zero timer to
// create a polling loop.
func earliestPositive(values ...time.Duration) time.Duration {
	var earliest time.Duration
	for _, value := range values {
		if value > 0 && (earliest == 0 || value < earliest) {
			earliest = value
		}
	}
	return earliest
}

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
	owned, err := r.ownedActivePods(ctx, deployment)
	if err != nil {
		return 0, err
	}
	// stableDeployment already proved Status.Replicas is the desired count, so
	// it doubles as the expected number of active pods.
	if len(owned) != int(deployment.Status.Replicas) || !allPodsReady(owned) {
		return 0, nil
	}
	return r.recycleExpiredPod(ctx, owned, podLifetime(*isvc.Spec.MaxPodLifetimeSeconds), now)
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
func (r *InferenceServiceReconciler) recycleExpiredPod(ctx context.Context, owned []*corev1.Pod, lifetime time.Duration, now time.Time) (time.Duration, error) {
	var expired *corev1.Pod
	var expiredDeadline time.Time
	var earliest time.Duration
	for _, pod := range owned {
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
	return r.evictPod(ctx, expired)
}

func (r *InferenceServiceReconciler) evictPod(ctx context.Context, pod *corev1.Pod) (time.Duration, error) {
	eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}, DeleteOptions: &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}}}
	err := r.SubResource("eviction").Create(ctx, pod, eviction)
	switch {
	case apierrors.IsNotFound(err):
		return 0, nil
	case apierrors.IsTooManyRequests(err):
		return podLifetimeRetry, nil
	default:
		return 0, err
	}
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

func (r *InferenceServiceReconciler) ownedActivePods(ctx context.Context, deployment *appsv1.Deployment) ([]*corev1.Pod, error) {
	listOpts := []client.ListOption{client.InNamespace(deployment.Namespace)}
	if deployment.Spec.Selector != nil {
		listOpts = append(listOpts, client.MatchingLabels(deployment.Spec.Selector.MatchLabels))
	}
	replicaSets := &appsv1.ReplicaSetList{}
	if err := r.List(ctx, replicaSets, listOpts...); err != nil {
		return nil, err
	}
	ownedRS := make(map[types.UID]struct{})
	for i := range replicaSets.Items {
		if metav1.IsControlledBy(&replicaSets.Items[i], deployment) {
			ownedRS[replicaSets.Items[i].UID] = struct{}{}
		}
	}
	if len(ownedRS) == 0 {
		return nil, nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, listOpts...); err != nil {
		return nil, err
	}
	owned := make([]*corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		ref := metav1.GetControllerOfNoCopy(pod)
		if ref == nil {
			continue
		}
		if _, ok := ownedRS[ref.UID]; !ok {
			continue
		}
		owned = append(owned, pod)
	}
	return owned, nil
}

func allPodsReady(pods []*corev1.Pod) bool {
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil || pod.Status.StartTime == nil || !podReady(pod) {
			return false
		}
	}
	return true
}

func podLifetime(seconds int64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	// The CRD has no Maximum, so an unbounded value would overflow Duration
	// into the past and turn recycling into an eviction loop.
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
