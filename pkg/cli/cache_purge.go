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

package cli

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
)

const (
	cachePurgePodPrefix     = "llmkube-cache-purge-"
	cacheInspectorPodPrefix = "llmkube-cache-inspector-"
	// cachePurgeComponent is the purge pod's component label.
	cachePurgeComponent = "cache-purge"
	managedByLabel      = "app.kubernetes.io/managed-by"
	managedByValue      = "llmkube-cli"
	cachePurgeRunLabel  = "llmkube.dev/cache-purge-run"

	// cacheKeyDirPattern matches the directory names the operator writes under
	// the cache mount (/models/<key>, key from cachekey.Compute).
	cacheKeyDirPattern = `^[0-9a-f]{16}$`
	// cacheKeySegmentPattern bounds a key to one path segment that cannot be
	// read as a flag.
	cacheKeySegmentPattern = `^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
)

// Both cache-access paths, the purge and the inspector, create their pod,
// wait for it and exec in it through these seams, so a unit test drives
// either without a cluster.
//
// Test seams: the unit tests have no cluster to exec in or wait on.
var (
	purgeExec  = execInPod
	purgeWait  = waitForPodRunning
	purgeSleep = time.Sleep
	// purgeRunID stamps every pod a single command creates, so a stale pod is
	// replaced while a pod another llmkube invocation is using is left alone.
	purgeRunID = func() string {
		return cachekey.Compute(fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid()))
	}
)

var (
	cacheKeyDirRe     = regexp.MustCompile(cacheKeyDirPattern)
	cacheKeySegmentRe = regexp.MustCompile(cacheKeySegmentPattern)
)

// purgeOutcome is what one PVC's purge did.
type purgeOutcome struct {
	PVC     string
	Removed []string
	Absent  []string
	// Skipped holds directory names the purge left alone: entries that are not
	// cache keys on a user-owned claim, and names that fail validation.
	Skipped []string
}

// isEmpty reports whether the purge had no effect on this PVC.
func (o purgeOutcome) isEmpty() bool {
	return len(o.Removed) == 0 && len(o.Absent) == 0 && len(o.Skipped) == 0
}

// validateCacheKey rejects any key that is not a single path segment. The key
// is interpolated into a shell command run as root inside a pod, so a key that
// could contain a separator or start with a dash is the one thing standing
// between "delete this entry" and "delete the volume".
func validateCacheKey(key string) error {
	if !cacheKeySegmentRe.MatchString(key) {
		return fmt.Errorf("invalid cache key %q: expected a single path segment", key)
	}
	return nil
}

// purgeTarget is a cache claim a purge may act on. UserOwned marks a claim a
// Model's own InferenceServices bring with spec.modelCache.claimName; the
// operator creates and garbage-collects those without the cache label, so they
// are reachable only through the service spec. InferenceService names the
// service whose owner reference makes the operator garbage-collect the claim
// (empty when no such owner). Order is the shared claim first, then the rest
// by name.
type purgeTarget struct {
	Name             string
	UserOwned        bool
	InferenceService string
	// SinglePod is claimSinglePod of the claim as resolution read it, the one
	// read that happens before a caller's own delete can collect the claim.
	SinglePod bool
}

// targetedClaims narrows the claims in a namespace to the ones that can hold
// modelName's files, resolving each referencing service's claim as the
// controller does: a service with a modelCache block mounts the claim it
// names, or the default when it names none (its "<isvc>-model-cache" only in
// perService mode, else the shared cache); a service with no block mounts the
// Model's spec.modelCache.claimName when the Model names one. The Model's own
// claim counts with or without a service, and the shared cache counts when
// there is neither a service nor a Model claim to consult, so a model that
// demonstrably lives on per-service or override claims never costs a pod on
// the shared claim, which a shared-mode service may hold under RWO. The
// claims of services that do not reference the model stay untouched, so
// clearing one model does not create a read-write pod on every running
// service's volume or fail on their attach timeouts. stagedClaims are the
// claims the Model's status.prefetch records: the controller's sticky target
// keeps a claim the services, or an explicit claimName, have since moved away
// from, and the files are there too. all is every cache claim in the
// namespace, in the order cachePVCandidates returns.
func targetedClaims(
	all []purgeTarget, svcs []inferencev1alpha1.InferenceService, modelName, modelClaim string, stagedClaims []string,
) []purgeTarget {
	exists := make(map[string]bool, len(all))
	for _, target := range all {
		exists[target.Name] = true
	}

	own := map[string]bool{}
	consulted := false
	if modelClaim != "" {
		own[modelClaim] = true
		consulted = true
	}
	for _, claim := range stagedClaims {
		if claim != "" {
			own[claim] = true
			consulted = true
		}
	}
	for i := range svcs {
		svc := &svcs[i]
		if svc.Name != modelName && svc.Spec.ModelRef != modelName {
			continue
		}
		consulted = true
		claim := userModelCacheClaimName(svc)
		if svc.Spec.ModelCache == nil && svc.Spec.ModelRef == modelName {
			claim = modelClaim
		}
		if claim != "" {
			own[claim] = true
			continue
		}
		// No claim of its own and none inherited: the operator gives it a
		// per-service claim only in perService mode, which is the mode whose
		// claim exists here; otherwise its pods mount the shared cache.
		if perService := perServiceCacheClaimName(svc.Name); exists[perService] {
			own[perService] = true
			continue
		}
		own[modelCachePVCName] = true
	}

	if !consulted {
		own[modelCachePVCName] = true
	}

	var selected []purgeTarget
	for _, target := range all {
		if own[target.Name] {
			selected = append(selected, target)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].Name == modelCachePVCName {
			return true
		}
		if selected[j].Name == modelCachePVCName {
			return false
		}
		return selected[i].Name < selected[j].Name
	})
	return selected
}

// discoveredClaims keeps the operator-managed claims, the ones `cache list`
// discovers by label or by name, and drops the user-owned claimName targets.
func discoveredClaims(all []purgeTarget) []purgeTarget {
	var discovered []purgeTarget
	for _, target := range all {
		if target.UserOwned {
			continue
		}
		discovered = append(discovered, target)
	}
	return discovered
}

// perServiceCacheClaimName mirrors the controller's naming for a per-service
// cache claim (internal/controller modelCachePVCName).
func perServiceCacheClaimName(isvcName string) string {
	return isvcName + "-model-cache"
}

// purgeKey returns the key `cache clear --model` removes for a model, or an
// error when the model has none.
func purgeKey(model *inferencev1alpha1.Model, modelName string) (string, error) {
	key := cachekey.EffectiveKey(model)
	if key == "" {
		return "", fmt.Errorf("model '%s' has no cache key (not cached, or a metal/single-file source)", modelName)
	}
	return key, nil
}

// cacheAccessPodKind names one of the two short-lived pods the CLI creates to
// reach a model cache claim: the purge pod, which writes, and the inspector
// pod, which only reads.
type cacheAccessPodKind struct {
	// noun names the pod in the messages a refusal produces.
	noun      string
	component string
	container string
	readOnly  bool
}

// purgePodName returns the deterministic purge pod name for a claim. The hash
// keeps it unique per claim and DNS-1123 safe (cachekey.Compute is a 16-char
// hex digest).
func purgePodName(pvcName string) string {
	return cachePurgePodPrefix + cachekey.Compute(pvcName)
}

// inspectorPodName returns the deterministic inspector pod name for a claim.
func inspectorPodName(pvcName string) string {
	return cacheInspectorPodPrefix + cachekey.Compute(pvcName)
}

var (
	purgePodKind = cacheAccessPodKind{
		noun:      "purge",
		component: cachePurgeComponent,
		container: "purge",
		readOnly:  false,
	}
	inspectorPodKind = cacheAccessPodKind{
		noun:      "inspector",
		component: "cache-inspector",
		container: "inspector",
		readOnly:  true,
	}
)

// createCacheAccessPod creates the pod that mounts a cache claim. It mounts
// the claim at the path the inspector reads it from and the purge writes to.
func createCacheAccessPod(
	ctx context.Context, clientset kubernetes.Interface, namespace, pvcName, podName string,
	kind cacheAccessPodKind, runID, node string, tolerations []corev1.Toleration,
) (string, error) {
	pod := newCacheAccessPodOnNode(podName, pvcName, kind.component, kind.container, kind.readOnly,
		runID, node, tolerations)
	pod.Namespace = namespace
	if _, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	return podName, nil
}

// createPurgePodForPVC creates the pod that removes entries from a cache
// claim, mounting it read-write.
func createPurgePodForPVC(
	ctx context.Context, clientset kubernetes.Interface, namespace, pvcName, runID string,
) (string, error) {
	return createCacheAccessPod(ctx, clientset, namespace, pvcName, purgePodName(pvcName),
		purgePodKind, runID, "", nil)
}

// ensurePurgePodReleasing returns a running purge pod, creating it when absent,
// with the caller's rule for a holder that is already going away:
// `delete --purge-cache` passes one so the pod its own InferenceService delete
// is tearing down does not refuse the purge, and waits for the drain instead.
// A purge pod is never pinned, so the holder check is the only guard before
// creating one.
func ensurePurgePodReleasing(
	ctx context.Context, k8sClient client.Client, clientset kubernetes.Interface,
	namespace string, target purgeTarget, releasing func(*corev1.Pod) bool,
) (string, error) {
	runID := purgeRunID()
	pods := &corev1.PodList{}
	if err := k8sClient.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		// No pod list means no evidence of a holder, and base did not look.
		pods = &corev1.PodList{}
	}
	if err := secondMountBlocker(claimHolder(pods.Items, target.Name), target.Name,
		purgePodName(target.Name), purgePodKind, runID, target.SinglePod, releasing); err != nil {
		return "", err
	}
	return ensureCacheAccessPod(ctx, clientset, namespace, target.Name, purgePodName(target.Name),
		purgePodKind, runID,
		func(runID string) (string, error) {
			return createPurgePodForPVC(ctx, clientset, namespace, target.Name, runID)
		})
}

// replaceStalePurgePod removes a purge pod an interrupted run left behind and
// waits for the object to be gone.
func replaceStalePurgePod(
	ctx context.Context, clientset kubernetes.Interface, namespace, pvcName, podName, runID string,
) error {
	return replaceStaleCacheAccessPod(ctx, clientset, namespace, pvcName, podName, purgePodKind, runID)
}

// ensureCacheAccessPod returns a running pod of kind kind that mounts pvcName,
// named podName and created by create, creating it when absent.
//
// The pod is ours to replace only when it carries both the managed-by and
// component labels; an unlabelled or foreign pod at that name is an error
// rather than something to delete.
func ensureCacheAccessPod(
	ctx context.Context, clientset kubernetes.Interface, namespace, pvcName, podName string,
	kind cacheAccessPodKind, runID string, create func(runID string) (string, error),
) (string, error) {
	// The two branches reach the same takeover; the purge one keeps
	// replaceStalePurgePod as the path's entry point rather than a
	// test-only symbol.
	replace := func() error {
		return replaceStaleCacheAccessPod(ctx, clientset, namespace, pvcName, podName, kind, runID)
	}
	if kind.component == purgePodKind.component {
		replace = func() error {
			return replaceStalePurgePod(ctx, clientset, namespace, pvcName, podName, runID)
		}
	}
	_, err := create(runID)
	if apierrors.IsAlreadyExists(err) {
		if err := replace(); err != nil {
			return "", err
		}
		_, err = create(runID)
	}
	if err != nil {
		return "", fmt.Errorf("failed to create %s pod for PVC %s: %w", kind.noun, pvcName, err)
	}

	if err := purgeWait(ctx, clientset, namespace, podName, 120*time.Second); err != nil {
		deleteCacheAccessPod(context.Background(), clientset, namespace, podName)
		return "", fmt.Errorf("%s pod for PVC %s failed to start: %w", kind.noun, pvcName, err)
	}
	return podName, nil
}

// replaceStaleCacheAccessPod removes a pod of kind kind that an interrupted run
// left behind and waits for the object to be gone.
//
// The delete is asynchronous: recreating the pod while the API server still
// holds the old one fails with AlreadyExists and the name stays occupied until
// the kubelet confirms termination, so a unit-test-speed retry is not enough.
// Only a stale llmkube pod of this kind is removed: an unlabelled pod at that
// name, or one another live invocation stamped, is an error.
func replaceStaleCacheAccessPod(
	ctx context.Context, clientset kubernetes.Interface, namespace, pvcName, podName string,
	kind cacheAccessPodKind, runID string,
) error {
	existing, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read existing %s pod %s: %w", kind.noun, podName, err)
	}
	if !cacheAccessPodOurs(existing, kind) {
		return fmt.Errorf("refusing to delete pod %s: it is not an llmkube cache %s pod", podName, kind.noun)
	}
	// A pod stamped by a live run is in use: it waits at that claim and is
	// deleted when the run ends. Only a pod from a run that never cleaned up
	// (no stamp at all) or this run's own leftover is replaced.
	if cacheAccessPodInUse(existing, runID) {
		return fmt.Errorf("refusing to delete pod %s: another llmkube cache %s run is using it", podName, kind.noun)
	}
	deleteCacheAccessPod(ctx, clientset, namespace, podName)

	deadline := time.Now().Add(30 * time.Second)
	for {
		pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to read terminating %s pod %s: %w", kind.noun, podName, err)
		}
		// A terminating pod keeps its name until the kubelet acks the delete.
		if pod.DeletionTimestamp != nil && time.Now().After(deadline) {
			return fmt.Errorf("%s pod %s for PVC %s is still terminating after 30s", kind.noun, podName, pvcName)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s pod %s for PVC %s was not deleted within 30s", kind.noun, podName, pvcName)
		}
		purgeSleep(time.Second)
	}
}

// purgePodAbandoned reports whether a stamped pod has outlived the process
// that created it. It cannot be told apart from a live run by the object
// alone, so age is the only signal unless the pod has finished: its
// `sleep 300` has exited, so nothing is using it.
func purgePodAbandoned(pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return true
	}
	return time.Since(pod.CreationTimestamp.Time) > 10*time.Minute
}

// withPurgePod runs fn inside a pod that mounts pvcName read-write, then
// deletes the pod.
//
// It always uses its own pod rather than a serving pod: the serving container
// mounts the claim read-only and may have no shell at all, and a strict RWO
// claim cannot be double-attached while that pod runs.
func withPurgePod(
	ctx context.Context, k8sClient client.Client, clientset kubernetes.Interface,
	namespace string, target purgeTarget, fn func(ctx context.Context, podName string) error,
) error {
	return withPurgePodReleasing(ctx, k8sClient, clientset, namespace, target, nil, fn)
}

// withPurgePodReleasing is withPurgePod with the caller's rule for a holder
// that is already going away.
func withPurgePodReleasing(
	ctx context.Context, k8sClient client.Client, clientset kubernetes.Interface,
	namespace string, target purgeTarget, releasing func(*corev1.Pod) bool,
	fn func(ctx context.Context, podName string) error,
) error {
	podName, err := ensurePurgePodReleasing(ctx, k8sClient, clientset, namespace, target, releasing)
	if err != nil {
		return err
	}
	defer deleteCacheAccessPod(context.Background(), clientset, namespace, podName)
	return fn(ctx, podName)
}

// listCacheDirs returns the entry names directly under the cache mount.
func listCacheDirs(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, podName string,
) ([]string, error) {
	output, err := purgeExec(ctx, cfg, clientset, namespace, podName, "purge",
		[]string{"sh", "-c", "ls -1 " + defaultModelMountPath})
	if err != nil {
		return nil, fmt.Errorf("failed to list the cache: %w", err)
	}
	var entries []string
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			entries = append(entries, line)
		}
	}
	return entries, nil
}

// deleteCachePaths removes the given absolute paths and verifies each is gone.
// It returns the paths that were removed and the ones that were already absent;
// an exec failure or a path that survives the rm is an error.
func deleteCachePaths(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, podName string, paths []string,
) (removed, absent []string, err error) {
	if len(paths) == 0 {
		return nil, nil, nil
	}

	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = "'" + p + "'"
	}
	script := `for p in ` + strings.Join(quoted, " ") + `; do ` +
		`if [ -e "$p" ]; then rm -rf "$p" && echo "removed $p" || exit 1; ` +
		`else echo "absent $p"; fi; done`

	output, err := purgeExec(ctx, cfg, clientset, namespace, podName, "purge",
		[]string{"sh", "-c", script})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to remove cache entries: %w", err)
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "removed":
			removed = append(removed, fields[1])
		case "absent":
			absent = append(absent, fields[1])
		}
	}

	if len(removed) == 0 {
		return removed, absent, nil
	}
	return removed, absent, verifyCachePathsGone(ctx, cfg, clientset, namespace, podName, removed)
}

// verifyCachePathsGone fails when any of paths still exists after the removal.
func verifyCachePathsGone(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, podName string, paths []string,
) error {
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = "'" + p + "'"
	}
	script := `for p in ` + strings.Join(quoted, " ") + `; do [ -e "$p" ] && echo "still $p"; done; exit 0`

	output, err := purgeExec(ctx, cfg, clientset, namespace, podName, "purge",
		[]string{"sh", "-c", script})
	if err != nil {
		return fmt.Errorf("failed to verify the cache entries were removed: %w", err)
	}
	var still []string
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "still" {
			still = append(still, fields[1])
		}
	}
	if len(still) > 0 {
		return fmt.Errorf("cache entries still present after removal: %s", strings.Join(still, ", "))
	}
	return nil
}

// purgeListed removes what choose selects from the claim's cache directories.
func purgeListed(
	ctx context.Context, cfg *rest.Config, k8sClient client.Client, clientset kubernetes.Interface,
	namespace string, target purgeTarget, choose func(entries []string) (del, skip []string),
) (purgeOutcome, error) {
	outcome := purgeOutcome{PVC: target.Name}
	err := withPurgePod(ctx, k8sClient, clientset, namespace, target,
		func(ctx context.Context, podName string) error {
			entries, err := listCacheDirs(ctx, cfg, clientset, namespace, podName)
			if err != nil {
				return err
			}
			del, skip := choose(entries)
			outcome.Skipped = append(outcome.Skipped, skip...)

			paths := make([]string, len(del))
			for i, name := range del {
				paths[i] = defaultModelMountPath + "/" + name
			}
			removed, absent, err := deleteCachePaths(ctx, cfg, clientset, namespace, podName, paths)
			outcome.Removed = removed
			outcome.Absent = absent
			return err
		})
	return outcome, err
}

// purgeCacheKeys removes the named entries from one claim. A key that is not
// there is reported, not failed: the entry is already gone. Every key is
// validated before a pod is created.
func purgeCacheKeys(
	ctx context.Context, cfg *rest.Config, k8sClient client.Client, clientset kubernetes.Interface,
	namespace string, target purgeTarget, keys []string,
) (purgeOutcome, error) {
	return purgeCacheKeysReleasing(ctx, cfg, k8sClient, clientset, namespace, target, nil, keys)
}

// purgeCacheKeysReleasing is purgeCacheKeys with the caller's rule for a holder
// that is already going away.
func purgeCacheKeysReleasing(
	ctx context.Context, cfg *rest.Config, k8sClient client.Client, clientset kubernetes.Interface,
	namespace string, target purgeTarget, releasing func(*corev1.Pod) bool, keys []string,
) (purgeOutcome, error) {
	for _, key := range keys {
		if err := validateCacheKey(key); err != nil {
			return purgeOutcome{PVC: target.Name}, err
		}
	}

	paths := make([]string, len(keys))
	for i, key := range keys {
		paths[i] = defaultModelMountPath + "/" + key
	}

	outcome := purgeOutcome{PVC: target.Name}
	err := withPurgePodReleasing(ctx, k8sClient, clientset, namespace, target, releasing,
		func(ctx context.Context, podName string) error {
			removed, absent, err := deleteCachePaths(ctx, cfg, clientset, namespace, podName, paths)
			outcome.Removed = removed
			outcome.Absent = absent
			return err
		})
	return outcome, err
}

// purgeAllEntries removes every cache entry from one claim.
//
// Only cache-key-shaped directories are eligible, on an operator claim and on
// a user-owned one alike: the operator writes nothing else, and a directory
// name with a quote or a space cannot be passed to the pod's shell safely.
func purgeAllEntries(
	ctx context.Context, cfg *rest.Config, k8sClient client.Client, clientset kubernetes.Interface,
	namespace string, target purgeTarget,
) (purgeOutcome, error) {
	return purgeListed(ctx, cfg, k8sClient, clientset, namespace, target,
		func(entries []string) (del, skip []string) {
			for _, name := range entries {
				if !cacheKeyDirRe.MatchString(name) {
					skip = append(skip, name)
					continue
				}
				del = append(del, name)
			}
			return del, skip
		})
}

// purgeOrphanedEntries removes the cache-key directories on a claim that no
// Model references.
func purgeOrphanedEntries(
	ctx context.Context, cfg *rest.Config, k8sClient client.Client, clientset kubernetes.Interface,
	namespace string, target purgeTarget, active map[string]bool,
) (purgeOutcome, error) {
	return purgeListed(ctx, cfg, k8sClient, clientset, namespace, target,
		func(entries []string) (del, skip []string) {
			for _, name := range entries {
				if !cacheKeyDirRe.MatchString(name) {
					skip = append(skip, name)
					continue
				}
				if active[name] {
					continue
				}
				del = append(del, name)
			}
			return del, skip
		})
}

// cachePVCandidates returns every cache claim in namespace that a purge may
// target, sorted and deduplicated: the label- and name-discovered operator
// caches, plus the spec.modelCache.claimName of an InferenceService named
// modelName or serving it. User-owned claims are outside the discovery label
// contract, so they only appear through the service spec.
func cachePVCandidates(
	ctx context.Context, k8sClient client.Client, namespace string,
) ([]string, []inferencev1alpha1.InferenceService, error) {
	infos, err := discoverCachePVCs(ctx, k8sClient, namespace)
	if err != nil {
		return nil, nil, err
	}
	svcs, models, err := modelServices(ctx, k8sClient, namespace)
	if err != nil {
		return nil, nil, err
	}

	seen := map[string]bool{}
	var candidates []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		candidates = append(candidates, name)
	}
	for _, info := range infos {
		add(info.Name)
	}
	// User-owned claims are outside the discovery label contract, so they only
	// appear through the service spec, or the Model's.
	for i := range svcs {
		add(userModelCacheClaimName(&svcs[i]))
	}
	for i := range models {
		add(modelCacheClaimName(&models[i]))
	}

	sort.Strings(candidates)
	return candidates, svcs, nil
}

// modelServices returns every InferenceService in namespace, which carries the
// claims a purge may target beyond the label-discovered ones, and every Model,
// both as stored. A Model's modelCache is inherited in one place,
// targetedClaims, by the services that reference the model being cleared, so
// a service that merely shares a model's name never resolves to another
// Model's claim.
func modelServices(
	ctx context.Context, k8sClient client.Client, namespace string,
) ([]inferencev1alpha1.InferenceService, []inferencev1alpha1.Model, error) {
	list := &inferencev1alpha1.InferenceServiceList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("failed to list InferenceServices: %w", err)
	}
	models := &inferencev1alpha1.ModelList{}
	if err := k8sClient.List(ctx, models, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("failed to list Models: %w", err)
	}
	return list.Items, models.Items, nil
}

// modelCacheClaimName mirrors the controller's helper: the claim a Model names
// with spec.modelCache.claimName, or "".
func modelCacheClaimName(model *inferencev1alpha1.Model) string {
	if model == nil || model.Spec.ModelCache == nil {
		return ""
	}
	return model.Spec.ModelCache.ClaimName
}

// modelCacheClaims returns the claim the named Model declares with
// spec.modelCache.claimName and the claims its status.prefetch records; both
// empty when the Model does not exist.
func modelCacheClaims(ctx context.Context, k8sClient client.Client, namespace, name string) (string, []string) {
	model := &inferencev1alpha1.Model{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, model); err != nil {
		return "", nil
	}
	return modelCacheClaimName(model), prefetchedClaims(model)
}

// prefetchedClaims lists the claims Model.status.prefetch records a staging
// into, in status order.
func prefetchedClaims(model *inferencev1alpha1.Model) []string {
	claims := make([]string, 0, len(model.Status.Prefetch))
	for _, e := range model.Status.Prefetch {
		claims = append(claims, e.ClaimName)
	}
	return claims
}

// userModelCacheClaimName mirrors the controller's helper: the claim a service
// brings with spec.modelCache.claimName, or "".
func userModelCacheClaimName(isvc *inferencev1alpha1.InferenceService) string {
	if isvc == nil || isvc.Spec.ModelCache == nil {
		return ""
	}
	return isvc.Spec.ModelCache.ClaimName
}

// resolvePurgeTargets keeps the candidates that exist and records who owns
// each.
func resolvePurgeTargets(
	ctx context.Context, k8sClient client.Client, namespace string,
) ([]purgeTarget, []inferencev1alpha1.InferenceService, error) {
	candidates, svcs, err := cachePVCandidates(ctx, k8sClient, namespace)
	if err != nil {
		return nil, nil, err
	}
	userOwned := map[string]bool{}
	for i := range svcs {
		if claim := userModelCacheClaimName(&svcs[i]); claim != "" {
			userOwned[claim] = true
		}
	}
	// A Model's own claim is user-owned too, with or without a service.
	if _, models, err := modelServices(ctx, k8sClient, namespace); err == nil {
		for i := range models {
			if claim := modelCacheClaimName(&models[i]); claim != "" {
				userOwned[claim] = true
			}
		}
	}
	infos, err := discoverCachePVCs(ctx, k8sClient, namespace)
	if err != nil {
		return nil, nil, err
	}
	ownedBy := map[string]string{}
	for _, info := range infos {
		ownedBy[info.Name] = info.InferenceService
	}

	var targets []purgeTarget
	for _, name := range candidates {
		pvc := &corev1.PersistentVolumeClaim{}
		err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, pvc)
		switch {
		case err == nil:
			targets = append(targets, purgeTarget{
				Name:             name,
				UserOwned:        userOwned[name],
				InferenceService: ownedBy[name],
				SinglePod:        claimSinglePod(pvc),
			})
			continue
		case apierrors.IsNotFound(err):
			// A user's claimName override can point at a claim that is being
			// collected with its InferenceService; the others are discovered
			// and therefore exist.
			continue
		}
		return nil, nil, fmt.Errorf("failed to get cache PVC %s: %w", name, err)
	}
	return targets, svcs, nil
}
