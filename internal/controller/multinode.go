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
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Multi-node serving groups (#1423). One InferenceService, N pods on N named
// nodes, one runtime process each, cooperating over the fabric. Rank 0 keeps
// the labels the Service and PodMonitor select on; the other ranks are
// headless workers that only the group reconciler looks at.
const (
	// LabelMultiNodeGroup marks every member pod with its InferenceService.
	LabelMultiNodeGroup = "inference.llmkube.dev/multinode-group"
	// LabelMultiNodeRank is the member's rank as a decimal string.
	LabelMultiNodeRank = "inference.llmkube.dev/multinode-rank"
	// AnnotationMultiNodeGroupHash is the hash of every member's desired pod
	// spec; a member carrying a different hash belongs to a stale group.
	AnnotationMultiNodeGroupHash = "inference.llmkube.dev/multinode-group-hash"
	// ConditionMultiNodeGroupReady is True when every member is Running and
	// rank 0 is Ready.
	ConditionMultiNodeGroupReady = "MultiNodeGroupReady"
	// ConditionMultiNodeFormed is True from the first reconcile on which every
	// member's runtime container was up at once. Its LastTransitionTime is the
	// formation timestamp groupNeedsRecreate judges restarts against (#1757).
	ConditionMultiNodeFormed = "MultiNodeFormed"
	// ConditionMultiNodeStaged is True from the first reconcile on which every
	// desired member exists and every member has finished staging. Its
	// LastTransitionTime is when staging ended for the group: the deadline the
	// unformed mask in memberRestartFatal is measured from.
	ConditionMultiNodeStaged = "MultiNodeStaged"

	// multiNodeStartupBudget is how long rank 0 would have to stay Running but
	// not Ready for that alone to recreate the group. It matches the vLLM
	// startup probe's budget (180 x 10s) so a slow but honest weight load is
	// not punished. The group deadlines read the window of the head's desired
	// startup probe instead, so spec.startupTimeout and probeOverrides.startup
	// are not cut short here; this value is the fallback when the desired head
	// carries no startup probe with a budget.
	multiNodeStartupBudget = 30 * time.Minute

	// multiNodeMaxInitRestarts bounds how many times a member's init containers
	// may restart while the group waits for staging. Formation masking must not
	// hide a permanently broken download: past this bound the group is
	// recreated with reason MemberInitLoop (#1757).
	multiNodeMaxInitRestarts = int32(3)

	// multiNodeRecreateRequeue is how soon the reconciler comes back after
	// deleting a group, to recreate it once the pods are gone.
	multiNodeRecreateRequeue = 5 * time.Second

	// multiNodeMaxRecreates bounds the consecutive failure-driven recreates of
	// a group that never serves in between (#1856). A recreate replaces every
	// pod, so the kubelet's own CrashLoopBackOff never engages and a
	// deterministic failure (a bad flag, no room for the KV cache) would
	// otherwise reload the whole checkpoint on every member forever. The
	// failure after the last allowed recreate marks the service Failed. It
	// matches multiNodeMaxInitRestarts: three retries absorb a node reboot or
	// a one-off OOM kill, and a fourth identical outcome is not transient.
	multiNodeMaxRecreates = int32(3)

	// multiNodeRecreateBackoffBase is the wait before the first recreate after
	// a member failure; each further consecutive failure doubles it, up to
	// multiNodeRecreateBackoffMax.
	multiNodeRecreateBackoffBase = 30 * time.Second
	multiNodeRecreateBackoffMax  = 5 * time.Minute

	// multiNodeRecreateResetAfter is how long the group must stay Ready before
	// its failure count is forgotten. A group that crashes on its first
	// request becomes Ready for a moment every cycle; resetting on the first
	// Ready would loop forever with the minimum backoff.
	multiNodeRecreateResetAfter = 10 * time.Minute

	// terminationDetailLimit bounds the excerpt of a termination message that
	// lastTerminationDetail keeps.
	terminationDetailLimit = 200
)

// Group recreate reasons that are member failures. Only these count toward
// multiNodeMaxRecreates: a spec change or a member being deleted is intent or
// disruption, not evidence that the group cannot start.
const (
	multiNodeReasonMemberRestarted = "MemberRestarted"
	multiNodeReasonMemberFailed    = "MemberFailed"
	multiNodeReasonMemberInitLoop  = "MemberInitLoop"
	multiNodeReasonHeadNotReady    = "HeadNotReady"
	// multiNodeReasonRecreateLimit is the group condition once the bound is
	// reached and the group is no longer recreated.
	multiNodeReasonRecreateLimit = "RecreateLimitReached"
	// multiNodeReasonBackOff is the group condition while a recreate waits.
	multiNodeReasonBackOff = "BackOff"
)

// memberPodName is <isvc>-mn-<rank>.
func memberPodName(isvc *inferencev1alpha1.InferenceService, rank int) string {
	return fmt.Sprintf("%s-mn-%d", isvc.Name, rank)
}

// validateMultiNode rejects what the CRD's CEL cannot express: duplicate
// nodes, a runtime with no MultiNodeArgsBuilder, and a rank 0 without an
// address (belt and braces; CEL checks that too).
func validateMultiNode(isvc *inferencev1alpha1.InferenceService) error {
	mn := isvc.Spec.MultiNode
	if mn == nil {
		return nil
	}
	if len(mn.Members) < 2 {
		return fmt.Errorf("multiNode needs at least two members, got %d", len(mn.Members))
	}
	seen := map[string]int{}
	for i, m := range mn.Members {
		if m.Node == "" {
			return fmt.Errorf("multiNode.members[%d].node is empty", i)
		}
		if j, dup := seen[m.Node]; dup {
			return fmt.Errorf("multiNode.members[%d] and [%d] both name node %q; one rank per node", j, i, m.Node)
		}
		seen[m.Node] = i
	}
	if f := mn.Members[0].Fabric; f == nil || f.Address == "" {
		return fmt.Errorf("multiNode.members[0].fabric.address is required: it is the rendezvous address every other rank dials")
	}
	if _, ok := resolveBackend(isvc).(MultiNodeArgsBuilder); !ok {
		return fmt.Errorf("runtime %q does not support multiNode; supported: vllm", runtimeNameLabel(isvc))
	}
	return validateMultiNodeMemory(mn.Memory)
}

// multiNodeMemoryPrepContainer is the init container that prepares a member
// node's memory (spec.multiNode.memory).
const multiNodeMemoryPrepContainer = "multinode-memory-prep"

// The sysctl names and values the memory prep accepts. The CRD carries the
// same two patterns; they are repeated here because the name becomes a
// path under /proc/sys in a privileged container, so an object that reached
// the controller without that admission check (a CRD from an older chart)
// must not be able to name anything outside /proc/sys/vm.
var (
	multiNodeSysctlKey   = regexp.MustCompile(`^vm[.][a-z0-9_]+$`)
	multiNodeSysctlValue = regexp.MustCompile(`^[0-9]+$`)
)

func validateMultiNodeMemory(mem *inferencev1alpha1.MultiNodeMemory) error {
	if mem == nil {
		return nil
	}
	for k, v := range mem.Sysctls {
		if !multiNodeSysctlKey.MatchString(k) {
			return fmt.Errorf("multiNode.memory.sysctls key %q is not a vm.<name> sysctl", k)
		}
		if !multiNodeSysctlValue.MatchString(string(v)) {
			return fmt.Errorf("multiNode.memory.sysctls[%q] value %q is not a non-negative integer", k, v)
		}
	}
	return nil
}

// multiNodeMemoryPrepScript writes each "vm.name=value" argument under
// /proc/sys, then drops the page cache when DROP_PAGE_CACHE is 1. The sysctls
// go first: raising vm.min_free_kbytes before the drop is what leaves the
// kernel room afterwards. Every write prints before and after, which is the
// audit trail (kubectl logs <member> -c multinode-memory-prep). set -e makes a
// refused write fail the init container rather than boot on an unprepared node.
const multiNodeMemoryPrepScript = `set -eu
for kv in "$@"; do
  k=${kv%%=*}
  v=${kv#*=}
  f=/proc/sys/$(echo "$k" | tr . /)
  before=$(cat "$f")
  echo "$v" > "$f"
  echo "sysctl $k: $before -> $(cat "$f")"
done
if [ "${DROP_PAGE_CACHE:-0}" = 1 ]; then
  before=$(awk '/^MemFree:/{print int($2/1024)}' /proc/meminfo)
  sync
  echo 3 > /proc/sys/vm/drop_caches
  after=$(awk '/^MemFree:/{print int($2/1024)}' /proc/meminfo)
  echo "pre-boot page cache drop: MemFree $before -> $after MiB"
fi
`

// multiNodeMemoryPrepInitContainer builds the init container for
// spec.multiNode.memory, or reports false when the block asks for nothing.
//
// It is privileged and root: /proc/sys is read-only in any other container,
// and no capability makes it writable. The vm.* sysctls and drop_caches are
// not namespaced, so the write lands on the node. It mounts nothing and has a
// read-only root filesystem; the script and its arguments are the whole input,
// and validateMultiNodeMemory bounds the arguments.
func multiNodeMemoryPrepInitContainer(mem *inferencev1alpha1.MultiNodeMemory, image string) (corev1.Container, bool) {
	if mem == nil || (!mem.DropPageCacheBeforeBoot && len(mem.Sysctls) == 0) {
		return corev1.Container{}, false
	}
	if image == "" {
		image = defaultPrefetchImage
	}
	// Sorted so the pod spec, and with it the group hash, does not depend on
	// map iteration order.
	keys := make([]string, 0, len(mem.Sysctls))
	for k := range mem.Sysctls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	command := []string{"sh", "-c", multiNodeMemoryPrepScript, multiNodeMemoryPrepContainer}
	for _, k := range keys {
		command = append(command, k+"="+string(mem.Sysctls[k]))
	}
	drop := "0"
	if mem.DropPageCacheBeforeBoot {
		drop = "1"
	}
	return corev1.Container{
		Name:    multiNodeMemoryPrepContainer,
		Image:   image,
		Command: command,
		Env:     []corev1.EnvVar{{Name: "DROP_PAGE_CACHE", Value: drop}},
		SecurityContext: &corev1.SecurityContext{
			Privileged:             boolPtr(true),
			RunAsUser:              int64Ptr(0),
			ReadOnlyRootFilesystem: boolPtr(true),
		},
	}, true
}

// multiNodeResourceClaimsConflict rejects spec.multiNode on a Model that
// allocates GPUs through DRA resourceClaims. Member pods pin spec.nodeName,
// which bypasses the scheduler, and DRA claims are allocated by the scheduler
// (KEP-4381: a custom scheduler must allocate and reserve before setting
// nodeName), so the members' claims would never be allocated and the pods
// would never start. CEL on the InferenceService cannot see the referenced
// Model, so this runs in the validating webhook and again at reconcile, the
// same split as validateRuntimePlacement. A nil Model (not created yet at
// admission) is undecidable and passes.
func multiNodeResourceClaimsConflict(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) error {
	if isvc.Spec.MultiNode == nil || model == nil {
		return nil
	}
	if len(modelResourceClaims(model)) == 0 {
		return nil
	}
	return fmt.Errorf("spec.multiNode pins member pods to nodes with spec.nodeName, which bypasses the scheduler; DRA resourceClaims are allocated by the scheduler, so the members' claims would never be allocated: remove spec.multiNode or the Model's hardware.gpu.resourceClaims")
}

// constructMemberPods derives one pod per member from the single-node pod
// template. Returns the pods in rank order and the group hash shared by all
// of them. The hash covers every member's spec so that a change to any rank
// recreates the whole group: ranks are not independently replaceable.
//
// The caller's InferenceService is not mutated: the resolved tensor and
// pipeline sizes are pinned on a copy so BuildArgs emits them for every rank.
func (r *InferenceServiceReconciler) constructMemberPods(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	cache effectiveModelCache,
	draftModel *inferencev1alpha1.Model,
	hfEndpoint string,
	draftHFEndpoint string,
) ([]*corev1.Pod, string, error) {
	if err := validateMultiNode(isvc); err != nil {
		return nil, "", err
	}
	if err := multiNodeResourceClaimsConflict(isvc, model); err != nil {
		return nil, "", err
	}
	mn := isvc.Spec.MultiNode
	mnb := resolveBackend(isvc).(MultiNodeArgsBuilder) // validateMultiNode proved this

	pinned := isvc.DeepCopy()
	members := int32(len(mn.Members)) //nolint:gosec // G115: bounded by the CRD's MaxItems=64
	tp, pp, _, _ := multiNodeParallelism(pinned.Spec.VLLMConfig, members, resolveGPUCount(pinned, model))
	if pinned.Spec.VLLMConfig == nil {
		pinned.Spec.VLLMConfig = &inferencev1alpha1.VLLMConfig{}
	}
	pinned.Spec.VLLMConfig.TensorParallelSize = &tp
	pinned.Spec.VLLMConfig.PipelineParallelSize = &pp

	tmpl := r.constructDeploymentForCache(pinned, model, cache, draftModel, 1, hfEndpoint, draftHFEndpoint).Spec.Template

	// The group hash must describe intent, not observation. The template
	// reads InferenceService status in places (the disruption-protection
	// annotation is present only while the service is not Ready), so hashing
	// the pods as rendered would change the hash the moment the group came
	// up and tear it down again. Render the hash source from a copy with a
	// blank status so only spec (service, model, image) feeds it.
	statusless := pinned.DeepCopy()
	statusless.Status = inferencev1alpha1.InferenceServiceStatus{}
	hashTmpl := r.constructDeploymentForCache(statusless, model, cache, draftModel, 1, hfEndpoint, draftHFEndpoint).Spec.Template

	pods := make([]*corev1.Pod, 0, len(mn.Members))
	h := sha256.New()
	for i, member := range mn.Members {
		rank := int32(i) //nolint:gosec // G115: bounded by the CRD's MaxItems=64
		pod := memberPodFromTemplate(tmpl, pinned, member, rank, mnb, r.InitContainerImage)
		hashPod := memberPodFromTemplate(hashTmpl, statusless, member, rank, mnb, r.InitContainerImage)
		h.Write([]byte(desiredTemplateHash(corev1.PodTemplateSpec{ObjectMeta: hashPod.ObjectMeta, Spec: hashPod.Spec})))
		pods = append(pods, pod)
	}
	hash := hex.EncodeToString(h.Sum(nil))[:16]
	for _, pod := range pods {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[AnnotationMultiNodeGroupHash] = hash
	}
	return pods, hash, nil
}

// memberPodFromTemplate specialises the shared template for one rank.
func memberPodFromTemplate(
	tmpl corev1.PodTemplateSpec,
	isvc *inferencev1alpha1.InferenceService,
	member inferencev1alpha1.MultiNodeMember,
	rank int32,
	mnb MultiNodeArgsBuilder,
	initImage string,
) *corev1.Pod {
	spec := *tmpl.Spec.DeepCopy()
	labels := copyMap(tmpl.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	annotations := copyMap(tmpl.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}

	labels[LabelMultiNodeGroup] = isvc.Name
	labels[LabelMultiNodeRank] = strconv.Itoa(int(rank))
	if rank > 0 {
		// Only rank 0 serves; the Service selector and the PodMonitor must
		// never see a worker.
		delete(labels, "app")
		delete(labels, "inference.llmkube.dev/service")
	}

	// The pin is the placement: nodeName bypasses the scheduler, so a
	// selector could only contradict it.
	spec.NodeName = member.Node
	spec.NodeSelector = nil
	spec.HostNetwork = true
	spec.HostIPC = true
	spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet

	c := &spec.Containers[0]
	c.Args = append(c.Args, mnb.BuildMultiNodeArgs(isvc, rank)...)
	// Fabric env first, user env after, so a user override still wins.
	c.Env = append(mnb.BuildMultiNodeEnv(isvc, rank), c.Env...)
	if rank > 0 {
		// A headless worker serves no HTTP, so the runtime's HTTP probes
		// would never pass. Its health signal is the rendezvous dial instead.
		c.StartupProbe, c.LivenessProbe, c.ReadinessProbe = nil, nil, nil
		c.Ports = nil
		if host, port, ok := rendezvousEndpoint(c.Args); ok {
			// Startup gets the same budget as the head's own startup probe
			// (180 x 10s unless spec.startupTimeout or probeOverrides.startup
			// raised it) to form the rendezvous: a worker that restarts while
			// the head is still loading recreates the group. After that,
			// 30s x 10 = five minutes of unreachable rendezvous restarts the
			// container, and MemberRestarted then recreates the group.
			c.StartupProbe = rendezvousProbe(host, port, 10, 180)
			// Adopt the head's window, not its failure count: the worker's
			// period can differ from the head's. Raise-only, like the group
			// budget, so a short probe cannot cut the worker's 30m rendezvous.
			if head := tmpl.Spec.Containers[0].StartupProbe; head != nil {
				if w := startupWindow(head); w > startupWindow(c.StartupProbe) {
					c.StartupProbe.FailureThreshold = startupFailureThreshold(w, c.StartupProbe)
				}
			}
			c.LivenessProbe = rendezvousProbe(host, port, 30, 10)
		}
	}

	if res := isvc.Spec.MultiNode.RDMAResource; res != "" {
		name := corev1.ResourceName(res)
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		c.Resources.Requests[name] = resource.MustParse("1")
		c.Resources.Limits[name] = resource.MustParse("1")
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		if c.SecurityContext.Capabilities == nil {
			c.SecurityContext.Capabilities = &corev1.Capabilities{}
		}
		c.SecurityContext.Capabilities.Add = append(c.SecurityContext.Capabilities.Add, "IPC_LOCK")
	}

	if member.ModelCache != nil && member.ModelCache.ClaimName != "" {
		for i := range spec.Volumes {
			if spec.Volumes[i].PersistentVolumeClaim != nil {
				spec.Volumes[i].PersistentVolumeClaim.ClaimName = member.ModelCache.ClaimName
			}
		}
	}

	// Last, so it runs after the model-downloader has filled the page cache
	// and immediately before the runtime starts.
	if prep, ok := multiNodeMemoryPrepInitContainer(isvc.Spec.MultiNode.Memory, initImage); ok {
		spec.InitContainers = append(spec.InitContainers, prep)
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        memberPodName(isvc, int(rank)),
			Namespace:   isvc.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: spec,
	}
}

// rendezvousEndpoint extracts the rendezvous the rank dials from the backend's
// argv. Members of a group are pinned to their own nodes on hostNetwork, so
// the endpoint is only meaningful for the workers, which dial rank 0. ok is
// false unless both values are safe to interpolate into a probe command.
func rendezvousEndpoint(args []string) (host, port string, ok bool) {
	for i, a := range args {
		if a != "--master-addr" && a != "--master-port" {
			continue
		}
		if i+1 >= len(args) {
			continue
		}
		if a == "--master-addr" {
			host = args[i+1]
		} else {
			port = args[i+1]
		}
	}
	return host, port, rendezvousEndpointSafe(host) && rendezvousEndpointSafe(port)
}

// rendezvousEndpointSafe accepts IP literals (v4 and bracketed v6) and digits:
// exactly what BuildMultiNodeArgs emits, and nothing a shell would read as
// syntax. Anything else renders the worker probeless, as before this probe.
func rendezvousEndpointSafe(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '[' {
		return s[len(s)-1] == ']' && !strings.ContainsAny(s[1:len(s)-1], "[].")
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' && r != ':' {
			return false
		}
	}
	return true
}

// rendezvousProbe builds the worker's health signal for a headless vLLM
// worker: a shell dial of the rank-0 rendezvous endpoint. The invariant is
// narrow: if the worker cannot open a TCP connection to the master address,
// it cannot serve in the group, so it must restart and re-rendezvous.
func rendezvousProbe(host, port string, period, failureThreshold int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/bash", "-c", "exec 3<>/dev/tcp/" + host + "/" + port},
			},
		},
		PeriodSeconds:    period,
		TimeoutSeconds:   5,
		FailureThreshold: failureThreshold,
	}
}

// memberObservation is what the reconciler saw for one desired rank.
type memberObservation struct {
	rank     int
	desired  *corev1.Pod
	existing *corev1.Pod // nil when missing
}

// memberStaging reports whether the member has not started its runtime: an
// init container is still running (the model-downloader at work), still
// waiting to run, or exited non-zero. A member whose init containers all
// completed successfully is not staging, whether or not its main container
// has started yet.
func memberStaging(p *corev1.Pod) bool {
	for i := range p.Status.InitContainerStatuses {
		ics := p.Status.InitContainerStatuses[i]
		if ics.State.Running != nil {
			return true
		}
		if t := ics.State.Terminated; t == nil || t.ExitCode != 0 {
			return true
		}
	}
	return false
}

// initRestartCount sums the restart counts of the member's init containers, so
// a download that keeps dying is distinguishable from one that is merely slow.
func initRestartCount(p *corev1.Pod) int32 {
	var n int32
	for i := range p.Status.InitContainerStatuses {
		n += p.Status.InitContainerStatuses[i].RestartCount
	}
	return n
}

// memberStaged reports whether the member has genuinely finished staging: it
// is not staging and every init container in the pod spec has reported a
// status. A pod the kubelet has not started yet reports no statuses, so it is
// not staged: the post-staging deadline must not be anchored before the
// downloader runs.
func memberStaged(p *corev1.Pod) bool {
	if memberStaging(p) {
		return false
	}
	return len(p.Status.InitContainerStatuses) >= len(p.Spec.InitContainers)
}

// memberRestartFatal reports whether a member's restart counts as group-fatal.
//
// While the group is forming, a waiting runtime that cannot rendezvous yet
// exits on its own and kubelet restarts it, and that repeats until every
// member has staged and the runtimes come up. Recreating the group on those
// restarts would kill the member still staging and discard its progress, which
// never converges (#1757), so they are masked.
//
// The mask ends at stagedAt + budget, not at formation alone: a member's
// weights load can outlast its downloader by minutes while the runtimes still
// find each other, but past the budget an unformed group whose members have
// stopped staging is a crashloop and the group is recreated rather than masked
// forever. budget is the group's startup budget, derived by groupNeedsRecreate.
//
// After formation the old strictness holds: a termination that finished after
// formedAt is a real crash. A restart with no readable termination is also
// fatal, so an untraceable restart cannot hide behind the masking.
func memberRestartFatal(cs corev1.ContainerStatus, formedAt, stagedAt, now time.Time, budget time.Duration) bool {
	t := cs.LastTerminationState.Terminated
	if t == nil {
		t = cs.State.Terminated
	}
	if !formedAt.IsZero() {
		if t == nil {
			return true
		}
		return t.FinishedAt.Time.IsZero() || t.FinishedAt.Time.After(formedAt)
	}
	if stagedAt.IsZero() || now.Sub(stagedAt) <= budget {
		return false
	}
	return true
}

// multiNodeFormedAt reads the formation timestamp off the InferenceService's
// MultiNodeFormed condition. Zero when the condition is absent or not True:
// the group has never been fully up.
func multiNodeFormedAt(isvc *inferencev1alpha1.InferenceService) time.Time {
	cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionMultiNodeFormed)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return time.Time{}
	}
	return cond.LastTransitionTime.Time
}

// multiNodeStagedAt reads staging-end off the InferenceService's
// MultiNodeStaged condition. Zero when the condition is absent or not True:
// some member is still staging, or not every member has been seen yet.
func multiNodeStagedAt(isvc *inferencev1alpha1.InferenceService) time.Time {
	cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionMultiNodeStaged)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return time.Time{}
	}
	return cond.LastTransitionTime.Time
}

// groupNeedsRecreate returns a non-empty reason when any existing member
// invalidates the group. A missing member is not a trigger: it is simply
// created.
//
// formedAt (the MultiNodeFormed timestamp, zero while forming) decides whether
// a member restart is fatal: see memberRestartFatal. While any member is
// staging, rank 0 cannot become Ready either (the ring is incomplete), so the
// startup budget is gated the same way; MemberInitLoop is the bound on a group
// whose staging never finishes.
//
// stagedAt (the MultiNodeStaged timestamp) is the unformed mask's deadline:
// past stagedAt + the group's startup budget a restart is finally fatal, so a
// group that never forms cannot be masked forever. A staging member that hangs
// without restarting (Running, RestartCount 0) is deliberately not bounded
// here: a 184 GiB download legitimately exceeds any fixed budget.
//
// The group's startup budget starts at multiNodeStartupBudget and is raised,
// never lowered, to the window of the desired rank-0 pod's startup probe, which
// is where spec.startupTimeout and probeOverrides.startup land. A spec probe
// shorter than the built-in budget does not shorten the group deadlines: those
// also have to cover a restart loop caused by something other than a slow
// load. The head's deadline runs from the first runtime container's start, so
// a long staging init does not consume it.
//
// trigger is the member the reason is about, so a failure can be attributed
// to one pod and counted once however many reconciles its teardown spans.
func groupNeedsRecreate(obs []memberObservation, hash string, now time.Time, formedAt, stagedAt time.Time) (reason, msg string, trigger *corev1.Pod) {
	budget := multiNodeStartupBudget
	for _, o := range obs {
		if o.rank != 0 || o.desired == nil || len(o.desired.Spec.Containers) == 0 {
			continue
		}
		if w := startupWindow(o.desired.Spec.Containers[0].StartupProbe); w > budget {
			budget = w
		}
		break
	}
	stagingAny := false
	for _, o := range obs {
		p := o.existing
		if p == nil {
			continue
		}
		if got := p.Annotations[AnnotationMultiNodeGroupHash]; got != hash {
			return "SpecChanged", fmt.Sprintf("%s carries group hash %q, want %q", p.Name, got, hash), p
		}
		if p.DeletionTimestamp != nil {
			return "MemberTerminating", p.Name + " is terminating", p
		}
		switch p.Status.Phase {
		case corev1.PodFailed, corev1.PodSucceeded, corev1.PodUnknown:
			detail := ""
			for _, cs := range p.Status.ContainerStatuses {
				detail += lastTerminationDetail(cs)
			}
			return multiNodeReasonMemberFailed, fmt.Sprintf("%s is %s%s", p.Name, p.Status.Phase, detail), p
		}
		if memberStaging(p) {
			stagingAny = true
			if n := initRestartCount(p); n > multiNodeMaxInitRestarts {
				return multiNodeReasonMemberInitLoop, fmt.Sprintf("%s init containers restarted %d time(s) while still staging", p.Name, n), p
			}
		}
	}
	for _, o := range obs {
		p := o.existing
		if p == nil {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount > 0 && memberRestartFatal(cs, formedAt, stagedAt, now, budget) {
				return multiNodeReasonMemberRestarted, fmt.Sprintf("%s restarted %d time(s)%s", p.Name, cs.RestartCount, lastTerminationDetail(cs)), p
			}
		}
	}
	if !stagingAny {
		for _, o := range obs {
			p := o.existing
			if p == nil {
				continue
			}
			if o.rank == 0 && p.Status.Phase == corev1.PodRunning && !podReady(p) && headNotReadyFor(p, now) > budget {
				return multiNodeReasonHeadNotReady, fmt.Sprintf("%s not Ready after %s", p.Name, budget), p
			}
		}
	}
	return "", "", nil
}

// headNotReadyFor is how long the head's runtime has had to answer. The
// startup probe's window starts when a runtime container starts, so a head
// still in its staging init reports zero (and is Pending, so the deadline is
// gated off anyway); a Running head whose container status is not yet readable
// falls back to the pod's creation time. An inference pod has exactly one
// runtime container.
func headNotReadyFor(p *corev1.Pod, now time.Time) time.Duration {
	if p.Status.Phase != corev1.PodRunning {
		return 0
	}
	started := time.Time{}
	for _, cs := range p.Status.ContainerStatuses {
		if r := cs.State.Running; r != nil && !r.StartedAt.IsZero() && (started.IsZero() || r.StartedAt.Time.Before(started)) {
			started = r.StartedAt.Time
		}
	}
	if started.IsZero() {
		if p.CreationTimestamp.IsZero() {
			return 0
		}
		started = p.CreationTimestamp.Time
	}
	return now.Sub(started)
}

// lastTerminationDetail renders the exit code, reason and an excerpt of the
// message of a container's last termination, so the recreate condition keeps
// the evidence the pod deletion is about to erase. Empty when nothing
// terminated.
func lastTerminationDetail(cs corev1.ContainerStatus) string {
	t := cs.LastTerminationState.Terminated
	if t == nil {
		t = cs.State.Terminated
	}
	if t == nil {
		return ""
	}
	msg := terminationExcerpt(t.Message, terminationDetailLimit)
	if msg != "" {
		msg = ": " + msg
	}
	return fmt.Sprintf(" (container %s exit %d %s%s)", cs.Name, t.ExitCode, t.Reason, msg)
}

var (
	// exceptionLine matches a line that names a raised failure: a Python
	// exception ("ValueError: ...", "torch.OutOfMemoryError: ..."), possibly
	// behind a vLLM process prefix, a Go panic, a C++ terminate, or a fatal
	// Python error. Warnings ("UserWarning:") deliberately do not match.
	exceptionLine = regexp.MustCompile(`\b[A-Za-z_][\w.]*(?:Error|Exception)\b:|^panic:|terminate called after throwing|Fatal Python error`)
	// errorLine is the weaker fallback: any line that says error, fatal or
	// failed, which is how llama.cpp and most C++ runtimes report.
	errorLine = regexp.MustCompile(`(?i)\b(?:error|fatal|failed)\b`)
)

// terminationExcerpt keeps the informative part of a termination message
// within limit bytes (plus "..." markers where text was dropped).
//
// Member containers use FallbackToLogsOnError, so the message is the last
// 2 KiB of the log, cut at an arbitrary point. Neither end of that window is
// reliably the cause: a Python runtime ends with shutdown noise (leaked
// shared_memory warnings) and the window's start is mid-log. The cause is the
// first exception line: Python prints the root cause of a chained exception
// first ("ValueError: No available memory for the cache blocks." before the
// wrapping "RuntimeError: Engine core initialization failed."). So the
// excerpt is the first exception line, else the first error line, else the
// head and the tail of the message around an ellipsis.
func terminationExcerpt(msg string, limit int) string {
	msg = strings.TrimSpace(msg)
	if len(msg) <= limit {
		return msg
	}
	for _, re := range []*regexp.Regexp{exceptionLine, errorLine} {
		offset := 0
		for _, raw := range strings.SplitAfter(msg, "\n") {
			line := strings.TrimSpace(raw)
			start := offset + strings.Index(raw, line)
			offset += len(raw)
			loc := re.FindStringIndex(line)
			if line == "" || loc == nil {
				continue
			}
			if re == exceptionLine {
				// Start at the exception itself: a log prefix such as
				// "(EngineCore pid=175) ERROR [core.py:1385] " spends the
				// budget the exception's own message needs.
				line, start = line[loc[0]:], start+loc[0]
			}
			out := cutHead(line, limit)
			if start > 0 {
				out = "..." + out
			}
			if len(out) < len(line) || start+len(line) < len(msg) {
				out += "..."
			}
			return out
		}
	}
	half := limit / 2
	return strings.TrimSpace(cutHead(msg, half)) + " ... " + strings.TrimSpace(cutTail(msg, limit-half))
}

// cutHead returns at most n bytes from the start of s without splitting a
// UTF-8 sequence.
func cutHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutTail returns at most n bytes from the end of s without splitting a UTF-8
// sequence.
func cutTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// multiNodeFailureReason reports whether a recreate reason is a member failure
// that counts toward multiNodeMaxRecreates.
func multiNodeFailureReason(reason string) bool {
	switch reason {
	case multiNodeReasonMemberRestarted, multiNodeReasonMemberFailed, multiNodeReasonMemberInitLoop, multiNodeReasonHeadNotReady:
		return true
	}
	return false
}

// multiNodeRecreateBackoff is the wait before the recreate that follows the
// count-th consecutive failure: base, doubling, capped.
func multiNodeRecreateBackoff(count int32) time.Duration {
	d := multiNodeRecreateBackoffBase
	for i := int32(1); i < count; i++ {
		d *= 2
		if d >= multiNodeRecreateBackoffMax {
			return multiNodeRecreateBackoffMax
		}
	}
	return d
}

// countGroupFailure folds one observed member failure into the recreate
// record. counted is false when trigger's failure was already counted: a
// teardown spans several reconciles, and a lagging cache can show the failed
// member again before its deletion, which must not burn a second retry.
//
// The count carries over from prev unless the group had been Ready for
// multiNodeRecreateResetAfter when it failed (readySince is when the group
// last became Ready, zero if it is not Ready). Any member failure counts, not
// only a repeat of the same message: the excerpt varies run to run (pids,
// timestamps, interleaved multi-process output), and a group that fails in
// different ways without ever serving is not converging either.
func countGroupFailure(prev *inferencev1alpha1.MultiNodeRecreateStatus, hash string, trigger *corev1.Pod, reason, msg string, readySince, now time.Time) (*inferencev1alpha1.MultiNodeRecreateStatus, bool) {
	uid := ""
	if trigger != nil {
		uid = string(trigger.UID)
	}
	if prev != nil && uid != "" && prev.PodUID == uid {
		return prev, false
	}
	var count int32
	if prev != nil && (readySince.IsZero() || now.Sub(readySince) < multiNodeRecreateResetAfter) {
		count = prev.Count
	}
	count++
	failedAt := metav1.NewTime(now)
	rec := &inferencev1alpha1.MultiNodeRecreateStatus{
		Count: count, Reason: reason, Message: msg,
		LastFailureTime: &failedAt, GroupHash: hash, PodUID: uid,
	}
	if count <= multiNodeMaxRecreates {
		next := metav1.NewTime(now.Add(multiNodeRecreateBackoff(count)))
		rec.NextRecreateTime = &next
	}
	return rec, true
}

// multiNodeRecreates returns the recreate record for the current group, nil
// when there is none.
func multiNodeRecreates(isvc *inferencev1alpha1.InferenceService) *inferencev1alpha1.MultiNodeRecreateStatus {
	if isvc.Status.MultiNode == nil {
		return nil
	}
	return isvc.Status.MultiNode.Recreates
}

// setMultiNodeRecreates stores the recreate record, creating status.multiNode
// if needed.
func setMultiNodeRecreates(isvc *inferencev1alpha1.InferenceService, rec *inferencev1alpha1.MultiNodeRecreateStatus) {
	if isvc.Status.MultiNode == nil {
		if rec == nil {
			return
		}
		isvc.Status.MultiNode = &inferencev1alpha1.MultiNodeStatus{}
	}
	isvc.Status.MultiNode.Recreates = rec
}

// multiNodeRecreateWait is how long the pending recreate still has to wait
// out its backoff. Zero when no recreate is pending or the wait is over.
func multiNodeRecreateWait(isvc *inferencev1alpha1.InferenceService, now time.Time) time.Duration {
	rec := multiNodeRecreates(isvc)
	if rec == nil || rec.NextRecreateTime == nil {
		return 0
	}
	if wait := rec.NextRecreateTime.Sub(now); wait > 0 {
		return wait
	}
	return 0
}

// multiNodeReadySince is when the group last became Ready, from the
// MultiNodeGroupReady condition. Zero when the group is not Ready.
func multiNodeReadySince(isvc *inferencev1alpha1.InferenceService) time.Time {
	cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionMultiNodeGroupReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return time.Time{}
	}
	return cond.LastTransitionTime.Time
}

// multiNodeRecreateResetRequeue is how long until a Ready group with a
// failure count has stayed Ready long enough to forget it. A healthy group
// generates no watch events, so Reconcile requeues for this instead of
// leaving a stale count to be picked up by the next failure. Zero when there
// is nothing to reset.
func multiNodeRecreateResetRequeue(isvc *inferencev1alpha1.InferenceService, now time.Time) time.Duration {
	if multiNodeRecreates(isvc) == nil {
		return 0
	}
	since := multiNodeReadySince(isvc)
	if since.IsZero() {
		return 0
	}
	if wait := since.Add(multiNodeRecreateResetAfter).Sub(now); wait > 0 {
		return wait
	}
	// Already due: the Ready path of reconcileMultiNodeGroup clears it.
	return 0
}

// multiNodeGroupFailedMessage is the Degraded message of a group that reached
// the recreate bound: what failed, where, and how to retry.
func multiNodeGroupFailedMessage(rec *inferencev1alpha1.MultiNodeRecreateStatus) string {
	return fmt.Sprintf("multiNode group failed %d times in a row without serving and is no longer recreated; last failure (%s): %s. "+
		"Fix the cause and change the spec, or set spec.suspend to true (or replicas to 0) and back, to retry",
		rec.Count, rec.Reason, rec.Message)
}

// groupReadiness: the group serves when every member is Running and rank 0
// is Ready. The count is the number of Running members, for status.
func groupReadiness(obs []memberObservation) (bool, int32) {
	var running int32
	ready := true
	for _, o := range obs {
		p := o.existing
		if p == nil || p.Status.Phase != corev1.PodRunning {
			ready = false
			continue
		}
		running++
		if o.rank == 0 && !podReady(p) {
			ready = false
		}
	}
	return ready, running
}

// reconcileMultiNodeGroup owns the member pods of a multiNode service. It
// replaces reconcileDeployment for such services. Any member that is missing
// is created; any member that is stale (different group hash), failed,
// restarted, terminating, or a rank 0 that has not become Ready inside the
// startup budget, deletes the WHOLE group, which the next reconcile
// recreates. Returns readyReplicas (1 when the group serves, else 0).
func (r *InferenceServiceReconciler) reconcileMultiNodeGroup(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	cache effectiveModelCache,
	model *inferencev1alpha1.Model,
	draftModel *inferencev1alpha1.Model,
	desiredReplicas int32,
	modelReady bool,
) (int32, *ctrl.Result, error) {
	if err := r.deleteStaleSingleNodeDeployment(ctx, isvc); err != nil {
		return 0, nil, err
	}

	if err := parallelismExceedsGPUCount(isvc, model); err != nil {
		result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, isvc.Status.Replicas, desiredReplicas, "",
			fmt.Sprintf("Invalid parallelism: %v", err), nil)
		return 0, &result, updateErr
	}
	if err := r.checkMemberClaims(ctx, isvc); err != nil {
		result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, isvc.Status.Replicas, desiredReplicas, "",
			fmt.Sprintf("Invalid multiNode: %v", err), nil)
		return 0, &result, updateErr
	}
	desired, hash, err := r.constructMemberPods(isvc, model, cache, draftModel,
		hfEndpointFromSecret(ctx, r.Client, model), hfEndpointFromSecret(ctx, r.Client, draftModel))
	if err != nil {
		result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, isvc.Status.Replicas, desiredReplicas, "",
			fmt.Sprintf("Invalid multiNode: %v", err), nil)
		return 0, &result, updateErr
	}

	var existing corev1.PodList
	if err := r.List(ctx, &existing, client.InNamespace(isvc.Namespace), client.MatchingLabels{LabelMultiNodeGroup: isvc.Name}); err != nil {
		return 0, nil, err
	}
	byName := map[string]*corev1.Pod{}
	for i := range existing.Items {
		byName[existing.Items[i].Name] = &existing.Items[i]
	}

	// A failure count belongs to one group spec: once the hash moves, the
	// user changed something and the new group starts with a clean record.
	if rec := multiNodeRecreates(isvc); rec != nil && rec.GroupHash != hash {
		setMultiNodeRecreates(isvc, nil)
	}

	// Suspended (desiredReplicas 0): the group must not exist. Suspending is
	// also the operator's retry: the failure count is forgotten.
	if desiredReplicas == 0 {
		setMultiNodeRecreates(isvc, nil)
		if len(existing.Items) > 0 {
			setMultiNodeStatus(isvc, desired, existing.Items, metav1.ConditionFalse, "Suspended", "spec.suspend or replicas 0")
			meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeFormed)
			meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeStaged)
			r.deleteMemberPods(ctx, existing.Items)
			return r.persistGroupTeardown(ctx, isvc, PhaseSuspended, modelReady, desiredReplicas)
		}
		setMultiNodeStatus(isvc, desired, nil, metav1.ConditionFalse, "Suspended", "group is scaled to zero")
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeFormed)
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeStaged)
		return 0, nil, nil
	}

	// Past the bound the group stays down: no recreate until the spec
	// changes or the service is suspended and resumed.
	if rec := multiNodeRecreates(isvc); rec != nil && rec.Count > multiNodeMaxRecreates {
		return r.holdFailedGroup(ctx, isvc, desired, existing.Items, rec, modelReady, desiredReplicas)
	}

	now := time.Now()
	obs := make([]memberObservation, len(desired))
	for i, d := range desired {
		obs[i] = memberObservation{rank: i, desired: d, existing: byName[d.Name]}
	}
	if reason, msg, trigger := groupNeedsRecreate(obs, hash, now, multiNodeFormedAt(isvc), multiNodeStagedAt(isvc)); reason != "" {
		return r.tearDownGroup(ctx, isvc, desired, existing.Items, groupTeardown{hash: hash, reason: reason, msg: msg, trigger: trigger}, now, modelReady, desiredReplicas)
	}

	// Back off before recreating a group that just failed, so a load that
	// takes minutes is not repeated back to back.
	if len(existing.Items) < len(desired) {
		if wait := multiNodeRecreateWait(isvc, now); wait > 0 {
			return r.waitRecreateBackoff(ctx, isvc, desired, existing.Items, multiNodeRecreates(isvc), wait, modelReady, desiredReplicas)
		}
	}

	if held, result, err := r.holdMembersForCACertConfigMap(ctx, isvc, desired, obs, modelReady, desiredReplicas); held || err != nil {
		return 0, result, err
	}

	created := 0
	for _, o := range obs {
		if o.existing != nil {
			continue
		}
		if err := setControllerReferenceUnblocked(isvc, o.desired, r.Scheme); err != nil {
			return 0, nil, err
		}
		if err := r.Create(ctx, o.desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return 0, nil, fmt.Errorf("multiNode: create %s: %w", o.desired.Name, err)
		}
		created++
	}
	if created > 0 {
		setMultiNodeStatus(isvc, desired, existing.Items, metav1.ConditionFalse, "Creating", fmt.Sprintf("%d member(s) created", created))
		return 0, nil, nil
	}

	ready, running := groupReadiness(obs)
	if ready {
		setMultiNodeStatus(isvc, desired, existing.Items, metav1.ConditionTrue, "AllMembersRunning", "rank 0 is Ready and every member is Running")
		// A group that has served for a while has recovered: whatever failed
		// before was transient, so the next failure starts a fresh count.
		if since := multiNodeReadySince(isvc); !since.IsZero() && now.Sub(since) >= multiNodeRecreateResetAfter {
			setMultiNodeRecreates(isvc, nil)
		}
		return 1, nil, nil
	}
	setMultiNodeStatus(isvc, desired, existing.Items, metav1.ConditionFalse, "Starting", fmt.Sprintf("%d of %d members running", running, len(desired)))
	return 0, nil, nil
}

// deleteStaleSingleNodeDeployment removes a Deployment this service owned
// under a previous single-node spec of the same name: it would fight the
// group for the GPU and the Service.
func (r *InferenceServiceReconciler) deleteStaleSingleNodeDeployment(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	var stale appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace}, &stale)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(&stale, isvc) {
		return nil
	}
	logf.FromContext(ctx).Info("multiNode: deleting the single-node Deployment this service used to own", "name", stale.Name)
	if err := r.Delete(ctx, &stale); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// holdMembersForCACertConfigMap holds member creation while the operator's
// custom CA ConfigMap is missing from the service namespace. Every rank renders
// from one template, so one rank's volumes answer for the group. A member that
// cannot mount the CA would hold its pinned node's GPU in ContainerCreating, so
// nothing is created until the ConfigMap exists. held is true when the caller
// must return result and err without creating members.
func (r *InferenceServiceReconciler) holdMembersForCACertConfigMap(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	desired []*corev1.Pod,
	obs []memberObservation,
	modelReady bool,
	desiredReplicas int32,
) (held bool, result *ctrl.Result, err error) {
	caMissing, err := r.reconcileCACertConfigMap(ctx, isvc, &desired[0].Spec)
	if err != nil || !caMissing {
		return false, nil, err
	}
	for _, o := range obs {
		if o.existing == nil {
			logf.FromContext(ctx).Info("multiNode: CA ConfigMap missing in the service namespace; not creating members",
				"configMap", r.CACertConfigMap, "namespace", isvc.Namespace)
			result, updateErr := r.waitForCACertConfigMap(ctx, isvc, modelReady, desiredReplicas)
			return true, result, updateErr
		}
	}
	return false, nil, nil
}

// checkMemberClaims mirrors ensureModelCachePVC for per-member claims: they
// are user-owned and must exist before use, and a pod pinned to a node with
// a missing claim would sit Pending forever with no diagnosis.
func (r *InferenceServiceReconciler) checkMemberClaims(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	for i, m := range isvc.Spec.MultiNode.Members {
		if m.ModelCache == nil || m.ModelCache.ClaimName == "" {
			continue
		}
		var pvc corev1.PersistentVolumeClaim
		err := r.Get(ctx, types.NamespacedName{Name: m.ModelCache.ClaimName, Namespace: isvc.Namespace}, &pvc)
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("members[%d].modelCache.claimName %q not found in namespace %q: the claim is user-owned and must be created before use",
				i, m.ModelCache.ClaimName, isvc.Namespace)
		}
		if err != nil {
			return fmt.Errorf("members[%d].modelCache.claimName %q: %w", i, m.ModelCache.ClaimName, err)
		}
	}
	return nil
}

// deleteMemberPods removes every member, best effort. Members keep their
// normal grace period; the next reconcile sees them Terminating and waits.
func (r *InferenceServiceReconciler) deleteMemberPods(ctx context.Context, pods []corev1.Pod) {
	log := logf.FromContext(ctx)
	for i := range pods {
		if err := r.Delete(ctx, &pods[i]); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "multiNode: failed to delete member", "pod", pods[i].Name)
		}
	}
}

// persistGroupTeardown writes the status (including the group condition the
// caller just set) and asks for an early requeue so the group is recreated as
// soon as its pods are gone. The caller returns this result to Reconcile,
// which returns it as-is, so the status must be written here: Reconcile's own
// status write only runs on the fall-through path.
func (r *InferenceServiceReconciler) persistGroupTeardown(ctx context.Context, isvc *inferencev1alpha1.InferenceService, phase string, modelReady bool, desiredReplicas int32) (int32, *ctrl.Result, error) {
	// observed is 0 here deliberately: teardown is removing the member pods,
	// so there is no count left to carry (unlike the failure paths above).
	result, err := r.updateStatusWithSchedulingInfo(ctx, isvc, phase, modelReady, 0, 0, desiredReplicas, "", "", nil)
	result.RequeueAfter = earliestPositive(result.RequeueAfter, multiNodeRecreateRequeue)
	return 0, &result, err
}

// groupTeardown is one recreate decision from groupNeedsRecreate.
type groupTeardown struct {
	hash, reason, msg string
	trigger           *corev1.Pod
}

// tearDownGroup deletes the whole group for td. A member failure is counted
// against multiNodeMaxRecreates first: inside the bound the group is torn
// down for a recreate after its backoff, past it the group is held down and
// the service fails.
func (r *InferenceServiceReconciler) tearDownGroup(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	desired []*corev1.Pod,
	existing []corev1.Pod,
	td groupTeardown,
	now time.Time,
	modelReady bool,
	desiredReplicas int32,
) (int32, *ctrl.Result, error) {
	log := logf.FromContext(ctx)
	condMsg := td.msg
	if multiNodeFailureReason(td.reason) {
		rec, counted := countGroupFailure(multiNodeRecreates(isvc), td.hash, td.trigger, td.reason, td.msg, multiNodeReadySince(isvc), now)
		setMultiNodeRecreates(isvc, rec)
		if rec.Count > multiNodeMaxRecreates {
			if counted {
				log.Info("multiNode: recreate limit reached, not recreating the group", "reason", td.reason, "failures", rec.Count, "detail", td.msg)
				r.multiNodeEvent(isvc, "MultiNodeGroupFailed", multiNodeGroupFailedMessage(rec))
			}
			return r.holdFailedGroup(ctx, isvc, desired, existing, rec, modelReady, desiredReplicas)
		}
		wait := multiNodeRecreateBackoff(rec.Count)
		condMsg = fmt.Sprintf("%s; recreate %d of %d in %s", td.msg, rec.Count, multiNodeMaxRecreates, wait)
		if counted {
			r.multiNodeEvent(isvc, "MultiNodeGroupRecreating",
				fmt.Sprintf("member failure (%s), recreate %d of %d in %s: %s", td.reason, rec.Count, multiNodeMaxRecreates, wait, td.msg))
		}
	}
	// A teardown takes several reconciles (every member event requeues);
	// log the decision once per reason, not once per requeue.
	if cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionMultiNodeGroupReady); cond == nil || cond.Reason != td.reason {
		log.Info("multiNode: recreating group", "reason", td.reason, "detail", td.msg)
	}
	setMultiNodeStatus(isvc, desired, existing, metav1.ConditionFalse, td.reason, condMsg)
	// The group is going away: the next formation must stamp a fresh
	// timestamp, or the new pods' kubelet restarts would be judged against
	// the old group's formation (#1757).
	meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeFormed)
	meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeStaged)
	r.deleteMemberPods(ctx, existing)
	return r.persistGroupTeardown(ctx, isvc, PhaseCreating, modelReady, desiredReplicas)
}

// waitRecreateBackoff reports a group that is waiting out its recreate
// backoff and comes back when the wait is over.
func (r *InferenceServiceReconciler) waitRecreateBackoff(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	desired []*corev1.Pod,
	existing []corev1.Pod,
	rec *inferencev1alpha1.MultiNodeRecreateStatus,
	wait time.Duration,
	modelReady bool,
	desiredReplicas int32,
) (int32, *ctrl.Result, error) {
	setMultiNodeStatus(isvc, desired, existing, metav1.ConditionFalse, multiNodeReasonBackOff,
		fmt.Sprintf("recreate %d of %d in %s after %s: %s",
			rec.Count, multiNodeMaxRecreates, wait.Round(time.Second), rec.Reason, rec.Message))
	result, err := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseCreating, modelReady, 0, 0, desiredReplicas, "", "", nil)
	result.RequeueAfter = earliestPositive(result.RequeueAfter, wait)
	return 0, &result, err
}

// holdFailedGroup keeps a group that reached the recreate bound down: any
// member still around is deleted, nothing is created, and the service reports
// phase Failed with the last failure's evidence. The pods are not left
// crashlooping: each kubelet restart would reload the whole checkpoint while
// holding every member's GPUs, and the evidence is already in status.
func (r *InferenceServiceReconciler) holdFailedGroup(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	desired []*corev1.Pod,
	existing []corev1.Pod,
	rec *inferencev1alpha1.MultiNodeRecreateStatus,
	modelReady bool,
	desiredReplicas int32,
) (int32, *ctrl.Result, error) {
	msg := multiNodeGroupFailedMessage(rec)
	setMultiNodeStatus(isvc, desired, existing, metav1.ConditionFalse, multiNodeReasonRecreateLimit, msg)
	meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeFormed)
	meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeStaged)
	r.deleteMemberPods(ctx, existing)
	result, err := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, 0, desiredReplicas, "", msg, nil)
	return 0, &result, err
}

// multiNodeEvent records a Warning event on the service. The member pods
// that would carry the kubelet's own events are deleted by every recreate,
// so the service is the only object that keeps the history.
func (r *InferenceServiceReconciler) multiNodeEvent(isvc *inferencev1alpha1.InferenceService, reason, note string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, reason, "Reconcile", "%s", note)
}

// setMultiNodeStatus fills status.multiNode from the desired ranks and the
// observed pods, and sets the group condition. The caller's status write
// persists it.
//
// It also maintains the MultiNodeFormed condition: True from the first
// reconcile on which every desired member exists and none is staging. Its
// LastTransitionTime is the formation timestamp (#1757); SetStatusCondition
// preserves it across later True writes, so the timestamp is the group's
// first formation, not the most recent. Teardown paths clear the condition so
// a recreated group forms afresh.
//
// MultiNodeStaged mirrors it one step earlier: True from the first reconcile
// on which every desired member exists and every member has finished staging.
// Its LastTransitionTime is the deadline memberRestartFatal measures the
// unformed mask from, so a group that never forms is not masked forever.
func setMultiNodeStatus(isvc *inferencev1alpha1.InferenceService, desired []*corev1.Pod, existing []corev1.Pod, status metav1.ConditionStatus, reason, msg string) {
	byName := map[string]*corev1.Pod{}
	for i := range existing {
		byName[existing[i].Name] = &existing[i]
	}
	st := &inferencev1alpha1.MultiNodeStatus{Size: int32(len(desired))} //nolint:gosec // G115: bounded by the CRD's MaxItems=64
	// The recreate record outlives the pods it counts; the caller owns it.
	if isvc.Status.MultiNode != nil {
		st.Recreates = isvc.Status.MultiNode.Recreates
	}
	formed := len(desired) > 0
	staged := len(desired) > 0
	for rank, d := range desired {
		m := inferencev1alpha1.MultiNodeMemberStatus{Rank: int32(rank), Node: d.Spec.NodeName, Pod: d.Name} //nolint:gosec // G115: bounded by the CRD's MaxItems=64
		if p := byName[d.Name]; p != nil {
			m.Phase = string(p.Status.Phase)
			m.Ready = podReady(p)
			for _, cs := range p.Status.ContainerStatuses {
				m.Restarts += cs.RestartCount
			}
			if m.Ready {
				st.ReadyMembers++
			}
			if memberStaging(p) {
				formed = false
			}
			if !memberStaged(p) {
				staged = false
			}
			started := false
			for _, cs := range p.Status.ContainerStatuses {
				if cs.State.Running != nil {
					started = true
					break
				}
			}
			// A pod the kubelet has not started yet (no container statuses at
			// all) is neither staging nor formed: the formation timestamp must
			// mean every runtime container was genuinely up.
			if !started {
				formed = false
			}
		} else {
			formed = false
			staged = false
		}
		st.Members = append(st.Members, m)
	}
	isvc.Status.MultiNode = st
	if formed {
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type: ConditionMultiNodeFormed, Status: metav1.ConditionTrue,
			Reason: "AllMembersStarted", Message: "every member's runtime container has started",
		})
	}
	if staged {
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type: ConditionMultiNodeStaged, Status: metav1.ConditionTrue,
			Reason: "AllMembersStaged", Message: "every member has finished staging",
		})
	} else {
		// Staging is under way again: a member was replaced in place (node
		// reboot, eviction, a deleted pod) and its replacement re-downloads.
		// That path creates the missing member without a teardown, so nothing
		// else clears this condition, and leaving it True would measure the
		// post-staging deadline from a staging episode that already ended.
		// A stale stamp older than the group's startup budget makes the next
		// waiting-runtime restart fatal and recreates the group out from under
		// the download, which is the #1757 loop this file exists to prevent.
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeStaged)
	}
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type: ConditionMultiNodeGroupReady, Status: status, Reason: reason, Message: msg,
	})
}
