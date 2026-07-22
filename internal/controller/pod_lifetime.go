package controller

import (
	"context"
	"sort"
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
func (r *InferenceServiceReconciler) reconcilePodLifetime(ctx context.Context, isvc *inferencev1alpha1.InferenceService, isMetal bool) (time.Duration, error) {
	return r.reconcilePodLifetimeAt(ctx, isvc, isMetal, time.Now())
}

func (r *InferenceServiceReconciler) reconcilePodLifetimeAt(ctx context.Context, isvc *inferencev1alpha1.InferenceService, isMetal bool, now time.Time) (time.Duration, error) {
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
	if !activePodCountMatches(deployment, owned) || !allPodsReady(owned) {
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
	_, stable := stableDeployment(deployment, isvc.UID)
	if !stable {
		return nil, nil
	}
	return deployment, nil
}

func activePodCountMatches(deployment *appsv1.Deployment, pods []*corev1.Pod) bool {
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}
	return int64(len(pods)) == int64(replicas)
}

func (r *InferenceServiceReconciler) recycleExpiredPod(ctx context.Context, owned []*corev1.Pod, lifetime time.Duration, now time.Time) (time.Duration, error) {
	type candidate struct {
		pod      *corev1.Pod
		deadline time.Time
	}
	candidates := make([]candidate, 0, len(owned))
	var earliest time.Duration
	for _, pod := range owned {
		deadline := pod.Status.StartTime.Time.Add(lifetime)
		if deadline.After(now) {
			earliest = earliestPositive(earliest, deadline.Sub(now))
		} else {
			candidates = append(candidates, candidate{pod: pod, deadline: deadline})
		}
	}
	if len(candidates) == 0 {
		return earliest, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].deadline.Equal(candidates[j].deadline) {
			return candidates[i].pod.Name < candidates[j].pod.Name
		}
		return candidates[i].deadline.Before(candidates[j].deadline)
	})
	return evictPod(ctx, r.Client, candidates[0].pod)
}

func evictPod(ctx context.Context, r client.Client, pod *corev1.Pod) (time.Duration, error) {
	uid := pod.UID
	eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}, DeleteOptions: &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}}
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

func stableDeployment(deployment *appsv1.Deployment, isvcUID types.UID) (int32, bool) {
	if !controlledBy(deployment, isvcUID) || deployment.Generation == 0 || deployment.Status.ObservedGeneration != deployment.Generation {
		return 0, false
	}
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}
	return replicas, deployment.Status.Replicas == replicas && deployment.Status.UpdatedReplicas == replicas && deployment.Status.ReadyReplicas == replicas && deployment.Status.AvailableReplicas == replicas
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
		if controlledBy(&replicaSets.Items[i], deployment.UID) {
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
		if _, ok := ownedRS[controllerUID(pod)]; !ok || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
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
	if seconds > maxPodLifetimeSeconds {
		seconds = maxPodLifetimeSeconds
	}
	return time.Duration(seconds) * time.Second
}

func controlledBy(obj metav1.Object, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.UID == uid {
			return true
		}
	}
	return false
}

func controllerUID(obj metav1.Object) types.UID {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return ref.UID
		}
	}
	return ""
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
