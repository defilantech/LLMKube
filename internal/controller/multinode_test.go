package controller

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// multiNodeFixture is the two-Spark DeepSeek-V4-Flash-Vision pair: rank 0 on
// dgx3 uses the service claim, rank 1 on dgx1 brings its own claim because
// the cluster has no ReadWriteMany class.
func multiNodeFixture() (*inferencev1alpha1.InferenceService, *inferencev1alpha1.Model) {
	tp := int32(2)
	gid := int32(3)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "ring", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:    "vllm",
			ModelRef:   "dsv4",
			Resources:  &inferencev1alpha1.InferenceResourceRequirements{GPU: 1, Memory: "110Gi", CPU: "8"},
			ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "shared-claim"},
			VLLMConfig: &inferencev1alpha1.VLLMConfig{TensorParallelSize: &tp},
			Env:        []corev1.EnvVar{{Name: "USER_VAR", Value: "1"}},
			MultiNode: &inferencev1alpha1.MultiNodeSpec{
				RDMAResource: "rdma/rdma_shared_device_a",
				IBGIDIndex:   &gid,
				Members: []inferencev1alpha1.MultiNodeMember{
					{Node: "dgx3", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.10.4.1", SocketInterface: "enp1s0f0np0", IBHCA: "rocep1s0f0"}},
					{Node: "dgx1", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.10.4.2", SocketInterface: "enp1s0f1np1", IBHCA: "rocep1s0f1"},
						ModelCache: &inferencev1alpha1.MultiNodeMemberCache{ClaimName: "dgx1-claim"}},
				},
			},
		},
	}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "dsv4", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "s3://models/deepseek-ai/DeepSeek-V4-Flash-Vision-Exp",
			Format: "safetensors",
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "cuda",
				GPU:         &inferencev1alpha1.GPUSpec{Enabled: true, Count: 1, Vendor: "nvidia"},
			},
		},
		// A Ready Model carries the cache key the Model controller computed;
		// without it the storage builder uses no cache volume at all.
		Status: inferencev1alpha1.ModelStatus{Phase: PhaseReady, CacheKey: "deepseek-v4-flash-vision-exp"},
	}
	return isvc, model
}

func pvcClaim(p *corev1.Pod) string {
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

// buildMemberPods runs the builder on the fixture and returns head, worker
// and the group hash, failing the test on any error.
func buildMemberPods(t *testing.T) (*corev1.Pod, *corev1.Pod, string) {
	t.Helper()
	isvc, model := multiNodeFixture()
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	pods, hash, err := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	if len(pods) != 2 || hash == "" {
		t.Fatalf("got %d pods, hash %q", len(pods), hash)
	}
	return pods[0], pods[1], hash
}

func TestConstructMemberPodsPlacementAndLabels(t *testing.T) {
	head, worker, hash := buildMemberPods(t)
	if head.Name != "ring-mn-0" || worker.Name != "ring-mn-1" {
		t.Errorf("names = %s, %s", head.Name, worker.Name)
	}
	if head.Spec.NodeName != "dgx3" || worker.Spec.NodeName != "dgx1" {
		t.Errorf("nodeName = %s, %s", head.Spec.NodeName, worker.Spec.NodeName)
	}
	for _, p := range []*corev1.Pod{head, worker} {
		if !p.Spec.HostNetwork || !p.Spec.HostIPC || p.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
			t.Errorf("%s: hostNetwork=%v hostIPC=%v dns=%s", p.Name, p.Spec.HostNetwork, p.Spec.HostIPC, p.Spec.DNSPolicy)
		}
		if p.Labels[LabelMultiNodeGroup] != "ring" {
			t.Errorf("%s: missing group label", p.Name)
		}
		if p.Annotations[AnnotationMultiNodeGroupHash] != hash {
			t.Errorf("%s: hash annotation %q != %q", p.Name, p.Annotations[AnnotationMultiNodeGroupHash], hash)
		}
		if p.Namespace != "default" {
			t.Errorf("%s: namespace %q", p.Name, p.Namespace)
		}
	}
	if head.Labels["inference.llmkube.dev/service"] != "ring" || head.Labels["app"] != "ring" {
		t.Errorf("head must keep the Service labels: %v", head.Labels)
	}
	if _, ok := worker.Labels["inference.llmkube.dev/service"]; ok {
		t.Errorf("worker must not carry the service label: %v", worker.Labels)
	}
	if _, ok := worker.Labels["app"]; ok {
		t.Errorf("worker must not carry the app label: %v", worker.Labels)
	}
	if head.Labels[LabelMultiNodeRank] != "0" || worker.Labels[LabelMultiNodeRank] != "1" {
		t.Errorf("rank labels: %v / %v", head.Labels, worker.Labels)
	}
}

func TestConstructMemberPodsProbesArgsEnv(t *testing.T) {
	head, worker, _ := buildMemberPods(t)
	hc, wc := head.Spec.Containers[0], worker.Spec.Containers[0]
	if hc.ReadinessProbe == nil || hc.StartupProbe == nil {
		t.Errorf("head keeps its probes")
	}
	if wc.ReadinessProbe != nil || wc.StartupProbe == nil || wc.LivenessProbe == nil || len(wc.Ports) != 0 {
		t.Errorf("worker must keep no HTTP probes or ports, but carry startup+liveness: %+v", wc)
	}
	for name, probe := range map[string]*corev1.Probe{"startup": wc.StartupProbe, "liveness": wc.LivenessProbe} {
		if probe == nil || probe.Exec == nil || probe.HTTPGet != nil {
			t.Errorf("worker %s probe must dial the rendezvous via exec, got %+v", name, probe)
			continue
		}
		want := "exec 3<>/dev/tcp/10.10.4.1/29500"
		if len(probe.Exec.Command) != 3 || probe.Exec.Command[0] != "/bin/bash" || probe.Exec.Command[2] != want {
			t.Errorf("worker %s probe must dial %q, got %v", name, want, probe.Exec)
		}
	}
	if wc.StartupProbe != nil && (wc.StartupProbe.PeriodSeconds != 10 || wc.StartupProbe.FailureThreshold != 180) {
		t.Errorf("worker startup budget must match the head's HTTP startup probe: %+v", wc.StartupProbe)
	}
	if wc.LivenessProbe != nil && (wc.LivenessProbe.PeriodSeconds != 30 || wc.LivenessProbe.FailureThreshold != 10) {
		t.Errorf("worker liveness budget: %+v", wc.LivenessProbe)
	}
	if wc.ReadinessProbe != nil {
		t.Errorf("worker has nothing to serve, so it must have no readiness probe: %+v", wc.ReadinessProbe)
	}
	if !containsArg(hc.Args, "--node-rank", "0") || !containsArg(wc.Args, "--node-rank", "1") || !containsArg(wc.Args, "--master-addr", "10.10.4.1") {
		t.Errorf("rank args missing: head %v worker %v", hc.Args, wc.Args)
	}
	if !containsArg(hc.Args, "--tensor-parallel-size", "2") || !containsArg(wc.Args, "--tensor-parallel-size", "2") {
		t.Errorf("both ranks carry --tensor-parallel-size 2: head %v worker %v", hc.Args, wc.Args)
	}
	env := envMap(wc.Env)
	if env["VLLM_HOST_IP"] != "10.10.4.2" || env["NCCL_IB_HCA"] != "rocep1s0f1" || env["NCCL_IB_GID_INDEX"] != "3" || env["USER_VAR"] != "1" {
		t.Errorf("worker env = %v", env)
	}
}

// TestRendezvousEndpoint: the worker probe dials whatever rendezvous the
// runtime's argv carries, and stays unset when the argv names none.
func TestRendezvousEndpoint(t *testing.T) {
	tests := []struct {
		name string
		args []string
		host string
		port string
		ok   bool
	}{
		{name: "vllm argv", args: []string{"vllm", "serve", "m", "--nnodes", "2", "--node-rank", "1", "--master-addr", "10.10.4.1", "--master-port", "29500"}, host: "10.10.4.1", port: "29500", ok: true},
		{name: "ipv6 host", args: []string{"--master-addr", "[fd00::1]", "--master-port", "29500"}, host: "[fd00::1]", port: "29500", ok: true},
		{name: "shell metachars are refused", args: []string{"--master-addr", "10.0.0.1;reboot", "--master-port", "29500"}, host: "10.0.0.1;reboot", port: "29500", ok: false},
		{name: "hostname is refused", args: []string{"--master-addr", "dgx3", "--master-port", "29500"}, host: "dgx3", port: "29500", ok: false},
		{name: "trailing flag with no value", args: []string{"--master-addr", "10.0.0.1", "--master-port"}, host: "10.0.0.1", port: "", ok: false},
		{name: "no rendezvous flags", args: []string{"vllm", "serve", "m"}, ok: false},
		{name: "empty argv", args: nil, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, ok := rendezvousEndpoint(tt.args)
			if host != tt.host || port != tt.port || ok != tt.ok {
				t.Errorf("rendezvousEndpoint(%v) = (%q, %q, %v), want (%q, %q, %v)", tt.args, host, port, ok, tt.host, tt.port, tt.ok)
			}
		})
	}
}

func TestConstructMemberPodsResourcesAndClaims(t *testing.T) {
	head, worker, _ := buildMemberPods(t)
	wc := worker.Spec.Containers[0]
	rdma := corev1.ResourceName("rdma/rdma_shared_device_a")
	req, lim := wc.Resources.Requests[rdma], wc.Resources.Limits[rdma]
	if req.Value() != 1 || lim.Value() != 1 {
		t.Errorf("rdma resource missing: %v", wc.Resources)
	}
	capOK := false
	if wc.SecurityContext != nil && wc.SecurityContext.Capabilities != nil {
		for _, c := range wc.SecurityContext.Capabilities.Add {
			if c == "IPC_LOCK" {
				capOK = true
			}
		}
	}
	if !capOK {
		t.Errorf("IPC_LOCK missing: %+v", wc.SecurityContext)
	}
	if pvcClaim(head) != "shared-claim" || pvcClaim(worker) != "dgx1-claim" {
		t.Errorf("claims: head %q worker %q", pvcClaim(head), pvcClaim(worker))
	}
}

// TestConstructMemberPodsAutoTP: with tensorParallelSize unset the builder pins
// TP = members x GPUs into every rank's argv, because BuildArgs' own
// auto-derive only knows one pod's GPU count.
func TestConstructMemberPodsAutoTP(t *testing.T) {
	isvc, model := multiNodeFixture()
	isvc.Spec.VLLMConfig = nil
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	pods, _, err := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	for _, p := range pods {
		if !containsArg(p.Spec.Containers[0].Args, "--tensor-parallel-size", "2") {
			t.Errorf("%s: want --tensor-parallel-size 2, got %v", p.Name, p.Spec.Containers[0].Args)
		}
	}
	if isvc.Spec.VLLMConfig != nil {
		t.Errorf("the caller's spec must not be mutated")
	}
}

func TestConstructMemberPodsHashStableAndSpecSensitive(t *testing.T) {
	isvc, model := multiNodeFixture()
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	_, h1, _ := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	_, h2, _ := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if h1 != h2 {
		t.Fatalf("hash not stable: %s vs %s", h1, h2)
	}
	isvc.Spec.MultiNode.Members[1].Fabric.IBHCA = "rocep1s0f0"
	_, h3, _ := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if h3 == h1 {
		t.Fatalf("hash must change when a member's fabric changes")
	}
}

func TestValidateMultiNode(t *testing.T) {
	isvc, _ := multiNodeFixture()
	if err := validateMultiNode(isvc); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	dup := *isvc
	dup.Spec.MultiNode = &inferencev1alpha1.MultiNodeSpec{Members: []inferencev1alpha1.MultiNodeMember{
		{Node: "a", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.1"}}, {Node: "a"},
	}}
	if err := validateMultiNode(&dup); err == nil {
		t.Errorf("duplicate node must be rejected")
	}
	noAddr := *isvc
	noAddr.Spec.MultiNode = &inferencev1alpha1.MultiNodeSpec{Members: []inferencev1alpha1.MultiNodeMember{{Node: "a"}, {Node: "b"}}}
	if err := validateMultiNode(&noAddr); err == nil {
		t.Errorf("rank 0 without an address must be rejected")
	}
	rt := *isvc
	rt.Spec.Runtime = "llamacpp"
	if err := validateMultiNode(&rt); err == nil {
		t.Errorf("llamacpp is not supported by this slice and must be rejected")
	}
	var none inferencev1alpha1.InferenceService
	if err := validateMultiNode(&none); err != nil {
		t.Errorf("no multiNode block is valid: %v", err)
	}
}

func TestMultiNodeResourceClaimsConflict(t *testing.T) {
	claimName := "gpu-claim"
	claimsModel := func() *inferencev1alpha1.Model {
		_, m := multiNodeFixture()
		m.Spec.Hardware.GPU.ResourceClaims = []corev1.PodResourceClaim{
			{Name: "gpu", ResourceClaimName: &claimName},
		}
		return m
	}

	isvc, plainModel := multiNodeFixture()

	// multiNode on a claims Model cannot work: members pin spec.nodeName,
	// which bypasses the scheduler that would allocate the claims (#2061).
	err := multiNodeResourceClaimsConflict(isvc, claimsModel())
	if err == nil {
		t.Fatal("multiNode with resourceClaims must be rejected")
	}
	if !strings.Contains(err.Error(), "resourceClaims") || !strings.Contains(err.Error(), "bypasses the scheduler") {
		t.Fatalf("error must name the conflict, got %q", err.Error())
	}

	// The reconciler path rejects through constructMemberPods before any
	// member pod is rendered.
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	if _, _, err := r.constructMemberPods(isvc, claimsModel(), resolveEffectiveModelCache(isvc, claimsModel()), nil, "", ""); err == nil {
		t.Fatal("constructMemberPods must reject multiNode with resourceClaims")
	}

	// A claims-free Model, a nil Model, and no multiNode block all pass.
	if err := multiNodeResourceClaimsConflict(isvc, plainModel); err != nil {
		t.Errorf("multiNode on a device-plugin Model must pass: %v", err)
	}
	if err := multiNodeResourceClaimsConflict(isvc, nil); err != nil {
		t.Errorf("nil Model (not created yet at admission) must pass: %v", err)
	}
	noMN := *isvc
	noMN.Spec.MultiNode = nil
	if err := multiNodeResourceClaimsConflict(&noMN, claimsModel()); err != nil {
		t.Errorf("no multiNode block must pass: %v", err)
	}
}

func TestGroupReadinessAndRecreate(t *testing.T) {
	mk := func(name string, phase corev1.PodPhase, ready bool, restarts int32, hash string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{AnnotationMultiNodeGroupHash: hash}}}
		p.Status.Phase = phase
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: restarts}}
		if ready {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		return p
	}
	now := time.Now()
	obs := []memberObservation{
		{rank: 0, existing: mk("h", corev1.PodRunning, true, 0, "abc")},
		{rank: 1, existing: mk("w", corev1.PodRunning, false, 0, "abc")},
	}
	if ok, n := groupReadiness(obs); !ok || n != 2 {
		t.Errorf("head Ready + worker Running must be ready (got %v, %d)", ok, n)
	}
	if reason, _, _ := groupNeedsRecreate(obs, "abc", now, now, now); reason != "" {
		t.Errorf("healthy group must not recreate: %s", reason)
	}
	obs[1].existing = mk("w", corev1.PodRunning, false, 1, "abc")
	if reason, _, _ := groupNeedsRecreate(obs, "abc", now, now, now); reason != "MemberRestarted" {
		t.Errorf("restarted worker: got %q", reason)
	}
	obs[1].existing = mk("w", corev1.PodRunning, false, 0, "old")
	if reason, _, _ := groupNeedsRecreate(obs, "abc", now, now, now); reason != "SpecChanged" {
		t.Errorf("stale hash: got %q", reason)
	}
	obs[1].existing = mk("w", corev1.PodFailed, false, 0, "abc")
	if reason, _, _ := groupNeedsRecreate(obs, "abc", now, now, now); reason != "MemberFailed" {
		t.Errorf("failed worker: got %q", reason)
	}
	obs[1].existing = nil
	if reason, _, _ := groupNeedsRecreate(obs, "abc", now, now, now); reason != "" {
		t.Errorf("a missing member is created, not a recreate trigger: got %q", reason)
	}
	if ok, n := groupReadiness(obs); ok || n != 1 {
		t.Errorf("missing member: ready=%v running=%d", ok, n)
	}
	obs[1].existing = mk("w", corev1.PodRunning, false, 0, "abc")
	obs[0].existing = mk("h", corev1.PodRunning, false, 0, "abc")
	obs[0].existing.CreationTimestamp = metav1.NewTime(now.Add(-multiNodeStartupBudget - time.Minute))
	if reason, _, _ := groupNeedsRecreate(obs, "abc", now, now, now); reason != "HeadNotReady" {
		t.Errorf("head past budget: got %q", reason)
	}
	obs[0].existing = mk("h", corev1.PodRunning, false, 0, "abc")
	if reason, _, _ := groupNeedsRecreate(obs, "abc", now, now, now); reason != "" {
		t.Errorf("head with no start time must wait: got %q", reason)
	}
	obs[0].existing.CreationTimestamp = metav1.NewTime(now.Add(-time.Minute))
	if reason, _, _ := groupNeedsRecreate(obs, "abc", now, now, now); reason != "" {
		t.Errorf("head inside budget must wait: got %q", reason)
	}
	if ok, _ := groupReadiness(obs); ok {
		t.Errorf("head not Ready must not be ready")
	}
}

// TestGroupNeedsRecreateWorkerRestartStillTriggersRecreate pins the path the
// #1781 worker liveness probe feeds: once the probe kills the wedged
// headless container, the restart alone recreates the group. Rank must not
// gate this: a restarted worker is as fatal to the group as a restarted head
// (#1781 kept a dead rank-1 process alive because health was rank-0-only).
func TestGroupNeedsRecreateWorkerRestartStillTriggersRecreate(t *testing.T) {
	mk := func(name string, restarts int32) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{AnnotationMultiNodeGroupHash: "abc"}}}
		p.Status.Phase = corev1.PodRunning
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: restarts}}
		return p
	}
	tests := []struct {
		name   string
		worker *corev1.Pod
		want   string
	}{
		{name: "probe-killed worker restarts, group recreates", worker: mk("w", 1), want: "MemberRestarted"},
		{name: "healthy worker with no restart stays up", worker: mk("w", 0), want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := []memberObservation{
				{rank: 0, existing: mk("h", 0)},
				{rank: 1, existing: tt.worker},
			}
			reason, _, _ := groupNeedsRecreate(obs, "abc", time.Now(), time.Now(), time.Time{})
			if reason != tt.want {
				t.Errorf("worker restart: got reason %q, want %q", reason, tt.want)
			}
		})
	}
}

// TestGroupNeedsRecreateFormation pins the #1757 formation rules: while a
// member is still staging its weights, the members that did start wait for the
// rendezvous and exit on their own, and kubelet restarts them; recreating the
// group on those exits kills the downloader and loses its progress, so they
// are formation noise. The noise ends at the group's formation timestamp, and
// a staging member whose init containers keep dying is bounded by
// MemberInitLoop so masking cannot hide a permanently broken download. When
// the group never forms, the noise ends at stagedAt + the group's startup
// budget instead, so a runtime crashloop after staging is recreated rather
// than masked.
func TestGroupNeedsRecreateFormation(t *testing.T) {
	now := time.Now()
	// mkWaiting is a member whose runtime has been up, exited and been
	// restarted by the kubelet.
	mkWaiting := func(name string, restarts int32, finishedAt time.Time) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{AnnotationMultiNodeGroupHash: "abc"}}}
		p.Status.Phase = corev1.PodRunning
		cs := corev1.ContainerStatus{RestartCount: restarts}
		if !finishedAt.IsZero() {
			cs.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Reason: "Error", FinishedAt: metav1.NewTime(finishedAt),
			}}
		}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
		return p
	}
	// mkStaging is a member still in its model-downloader init container.
	mkStaging := func(name string, initRestarts int32) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{AnnotationMultiNodeGroupHash: "abc"}}}
		p.Status.Phase = corev1.PodPending
		started := metav1.NewTime(now.Add(-10 * time.Minute))
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name:         "model-downloader",
			State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
			RestartCount: initRestarts,
		}}
		return p
	}

	tests := []struct {
		name     string
		obs      []memberObservation
		formedAt time.Time
		stagedAt time.Time
		want     string
	}{
		{
			name: "restarts while a member stages are formation noise",
			obs: []memberObservation{
				{rank: 0, existing: mkWaiting("h", 2, now.Add(-time.Minute))},
				{rank: 1, existing: mkStaging("w", 0)},
			},
			formedAt: time.Time{},
			want:     "",
		},
		{
			name: "a termination after formation is still fatal",
			obs: []memberObservation{
				{rank: 0, existing: mkWaiting("h", 1, now.Add(-time.Minute))},
				{rank: 1, existing: mkStaging("w", 0)},
			},
			formedAt: now.Add(-5 * time.Minute),
			want:     "MemberRestarted",
		},
		{
			name: "an init container that keeps dying is bounded by MemberInitLoop",
			obs: []memberObservation{
				{rank: 0, existing: mkWaiting("h", 0, now.Add(-time.Minute))},
				{rank: 1, existing: mkStaging("w", 4)},
			},
			formedAt: time.Time{},
			want:     "MemberInitLoop",
		},
		{
			name: "init restarts within the bound keep waiting for staging",
			obs: []memberObservation{
				{rank: 0, existing: mkWaiting("h", 0, now.Add(-time.Minute))},
				{rank: 1, existing: mkStaging("w", 3)},
			},
			formedAt: time.Time{},
			want:     "",
		},
		{
			name: "a staging member gates the head startup budget",
			obs: []memberObservation{
				{rank: 0, existing: mkWaiting("h", 0, time.Time{})},
				{rank: 1, existing: mkStaging("w", 0)},
			},
			formedAt: time.Time{},
			want:     "",
		},
		{
			name: "a runtime still rendezvousing inside the post-staging window is noise",
			obs: []memberObservation{
				{rank: 0, existing: mkWaiting("h", 2, now.Add(-time.Minute))},
				{rank: 1, existing: mkWaiting("w", 0, time.Time{})},
			},
			formedAt: time.Time{},
			stagedAt: now.Add(-2 * time.Minute),
			want:     "",
		},
		{
			name: "a runtime that keeps dying after staging is fatal",
			obs: []memberObservation{
				{rank: 0, existing: mkWaiting("h", 4, now.Add(-time.Minute))},
				{rank: 1, existing: mkWaiting("w", 0, time.Time{})},
			},
			formedAt: time.Time{},
			stagedAt: now.Add(-multiNodeStartupBudget - time.Minute),
			want:     "MemberRestarted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "a staging member gates the head startup budget" {
				// Age the head past the budget so the gating is what is under
				// test, not the budget itself.
				tt.obs[0].existing.CreationTimestamp = metav1.NewTime(now.Add(-multiNodeStartupBudget - time.Minute))
			}
			reason, _, _ := groupNeedsRecreate(tt.obs, "abc", now, tt.formedAt, tt.stagedAt)
			if reason != tt.want {
				t.Errorf("got reason %q, want %q", reason, tt.want)
			}
		})
	}
}

// TestConstructMemberPodsHashIgnoresStatus: the template reads the service's
// status phase (the disruption-protection annotation is only present while
// not Ready), which must not move the group hash: a group that became Ready
// would otherwise be torn down by its own success.
func TestConstructMemberPodsHashIgnoresStatus(t *testing.T) {
	isvc, model := multiNodeFixture()
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	isvc.Status.Phase = PhaseCreating
	podsCreating, h1, err := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	isvc.Status.Phase = PhaseReady
	podsReady, h2, err := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash moved with status phase: %s (Creating) vs %s (Ready)", h1, h2)
	}
	// The rendered pods still follow the real status (the annotation is a
	// startup protection), only the hash is status-blind.
	_, creatingHas := podsCreating[0].Annotations["karpenter.sh/do-not-disrupt"]
	_, readyHas := podsReady[0].Annotations["karpenter.sh/do-not-disrupt"]
	if !creatingHas || readyHas {
		t.Fatalf("expected the disruption annotation only while not Ready: creating=%v ready=%v", creatingHas, readyHas)
	}
}

func TestLastTerminationDetail(t *testing.T) {
	cs := corev1.ContainerStatus{Name: "vllm"}
	if got := lastTerminationDetail(cs); got != "" {
		t.Fatalf("no termination must render empty, got %q", got)
	}
	cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: "value_error: no config.json"}
	got := lastTerminationDetail(cs)
	for _, want := range []string{"vllm", "exit 1", "Error", "no config.json"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail %q lacks %q", got, want)
		}
	}
	// #1856: the retained excerpt is the exception, not the teardown tail.
	cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: vllmKVCacheTermination}
	got = lastTerminationDetail(cs)
	if !strings.Contains(got, "ValueError: No available memory for the cache blocks.") || strings.Contains(got, "resource_tracker") {
		t.Errorf("detail kept the wrong part of the message: %q", got)
	}
	// The formation-aware recreate rule (#1757) judges a termination against
	// the group's formation timestamp, so the fixture terminates after the
	// group formed: formedAt an hour ago, FinishedAt now.
	obs := []memberObservation{{rank: 0, existing: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "h", Annotations: map[string]string{AnnotationMultiNodeGroupHash: "abc"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "vllm", RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.NewTime(time.Now())}}}}}}}}
	reason, msg, _ := groupNeedsRecreate(obs, "abc", time.Now(), time.Now().Add(-time.Hour), time.Time{})
	if reason != "MemberRestarted" || !strings.Contains(msg, "OOMKilled") || !strings.Contains(msg, "137") {
		t.Fatalf("restart detail missing: %s / %s", reason, msg)
	}
}

// makeDesired builds the desired-pod stubs setMultiNodeStatus matches against.
func makeDesired(n int) []*corev1.Pod {
	desired := make([]*corev1.Pod, n)
	for i := range desired {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ring-mn-" + strconv.Itoa(i)}}
		p.Spec.NodeName = "node"
		desired[i] = p
	}
	return desired
}

// TestSetMultiNodeStatusReadyMembers is the #1781 miscount: the aggregate
// counts Ready pods, matching the per-member m.Ready field it renders
// alongside, so status can no longer claim 2/2 while a member is 0/1.
func TestSetMultiNodeStatusReadyMembers(t *testing.T) {
	isvc, _ := multiNodeFixture()
	mk := func(name string, phase corev1.PodPhase, ready bool) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
		p.Status.Phase = phase
		if ready {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		return p
	}
	tests := []struct {
		name  string
		pods  []corev1.Pod
		wantR int32
	}{
		{name: "all members Ready", pods: []corev1.Pod{mk("ring-mn-0", corev1.PodRunning, true), mk("ring-mn-1", corev1.PodRunning, true)}, wantR: 2},
		{name: "running worker without Ready does not count", pods: []corev1.Pod{mk("ring-mn-0", corev1.PodRunning, true), mk("ring-mn-1", corev1.PodRunning, false)}, wantR: 1},
		{name: "pending member does not count", pods: []corev1.Pod{mk("ring-mn-0", corev1.PodPending, false), mk("ring-mn-1", corev1.PodPending, false)}, wantR: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isvc := isvc.DeepCopy()
			setMultiNodeStatus(isvc, makeDesired(len(tt.pods)), tt.pods, metav1.ConditionFalse, "Starting", "test")
			if isvc.Status.MultiNode == nil {
				t.Fatal("status.multiNode must be set")
			}
			if got := isvc.Status.MultiNode.ReadyMembers; got != tt.wantR {
				t.Errorf("readyMembers = %d, want %d", got, tt.wantR)
			}
			for _, m := range isvc.Status.MultiNode.Members {
				p := tt.pods[m.Rank]
				if m.Ready != (p.Status.Phase == corev1.PodRunning && len(p.Status.Conditions) > 0) {
					t.Errorf("member %d ready flag %v disagrees with its pod", m.Rank, m.Ready)
				}
			}
		})
	}
}

// TestSetMultiNodeStatusStagedClearedOnRestage pins the stale-stamp bug in the
// post-staging deadline. MultiNodeStaged was only ever written True, and only
// the teardown paths removed it, but a member replaced in place (node reboot,
// eviction, a deleted pod) is recreated without a teardown. Its replacement
// re-downloads while the condition still carries the PREVIOUS staging
// episode's timestamp, so once that stamp ages past the startup budget the
// next waiting-runtime restart is judged fatal and the group is recreated
// out from under the download: the #1757 loop, restored.
func TestSetMultiNodeStatusStagedClearedOnRestage(t *testing.T) {
	isvc, _ := multiNodeFixture()
	mk := func(name string, staging bool) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
		p.Spec.InitContainers = []corev1.Container{{Name: "model-downloader"}}
		if staging {
			p.Status.Phase = corev1.PodPending
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
				Name:  "model-downloader",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}
			return p
		}
		p.Status.Phase = corev1.PodRunning
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name:  "model-downloader",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "vllm",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
		return p
	}
	desired := makeDesired(2)

	// Every member has staged: the deadline is anchored.
	setMultiNodeStatus(isvc, desired,
		[]corev1.Pod{mk("ring-mn-0", false), mk("ring-mn-1", false)},
		metav1.ConditionFalse, "Starting", "test")
	if multiNodeStagedAt(isvc).IsZero() {
		t.Fatal("MultiNodeStaged must be stamped once every member has staged")
	}

	// Age the stamp past the budget, the way a long-lived group would.
	for i := range isvc.Status.Conditions {
		if isvc.Status.Conditions[i].Type == ConditionMultiNodeStaged {
			isvc.Status.Conditions[i].LastTransitionTime =
				metav1.NewTime(time.Now().Add(-2 * multiNodeStartupBudget))
		}
	}

	// Member 1 was replaced in place and is staging again.
	setMultiNodeStatus(isvc, desired,
		[]corev1.Pod{mk("ring-mn-0", false), mk("ring-mn-1", true)},
		metav1.ConditionFalse, "Starting", "test")

	if got := multiNodeStagedAt(isvc); !got.IsZero() {
		t.Errorf("a member is staging again, so the post-staging deadline must not be anchored; "+
			"multiNodeStagedAt = %s (%s old)", got, time.Since(got).Round(time.Minute))
	}

	// The mask must hold while that member stages, rather than judging the
	// peer's waiting-runtime restart against the stale stamp.
	waiting := mk("ring-mn-0", false)
	waiting.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "vllm", RestartCount: 2,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Reason: "Error", FinishedAt: metav1.Now(),
		}},
	}}
	restaging := mk("ring-mn-1", true)
	obs := []memberObservation{
		{rank: 0, existing: &waiting},
		{rank: 1, existing: &restaging},
	}
	for i := range obs {
		obs[i].existing.Annotations = map[string]string{AnnotationMultiNodeGroupHash: "abc"}
	}
	if reason, msg, _ := groupNeedsRecreate(obs, "abc", time.Now(), time.Time{}, multiNodeStagedAt(isvc)); reason != "" {
		t.Errorf("the group must be held while a member re-stages; got %s: %s", reason, msg)
	}
}

// vllmKVCacheTermination is the shape of the #1856 termination message: the
// last chunk of a vLLM group member's log, as FallbackToLogsOnError hands it
// over. The cause is the ValueError in the middle; the tail is shutdown noise.
const vllmKVCacheTermination = `(Worker_TP0 pid=288) INFO [model_runner.py:408] Model loading took 83.88 GiB memory and 377.98 seconds
(Worker_TP0 pid=288) INFO [gpu_worker.py:632] Available KV cache memory: -1.13 GiB
(EngineCore pid=175) ERROR [core.py:1385] EngineCore failed to start.
(EngineCore pid=175) ERROR [core.py:1385] Traceback (most recent call last):
(EngineCore pid=175) ERROR [core.py:1385]   File "/usr/local/lib/python3.12/dist-packages/vllm/v1/engine/core.py", line 1376, in run_engine_core
(EngineCore pid=175) ERROR [core.py:1385]     engine_core = EngineCoreProc(*args, **kwargs)
(EngineCore pid=175) ERROR [core.py:1385]   File "/usr/local/lib/python3.12/dist-packages/vllm/v1/core/kv_cache_utils.py", line 701, in check_enough_kv_cache_memory
(EngineCore pid=175) ERROR [core.py:1385]     raise ValueError("No available memory for the cache blocks.")
(EngineCore pid=175) ERROR [core.py:1385] ValueError: No available memory for the cache blocks. Try increasing gpu_memory_utilization when initializing the engine.
(APIServer pid=1) Traceback (most recent call last):
(APIServer pid=1)   File "/usr/local/bin/vllm", line 10, in <module>
(APIServer pid=1) RuntimeError: Engine core initialization failed. See root cause above.
/usr/lib/python3.12/multiprocessing/resource_tracker.py:254: UserWarning: resource_tracker: There appear to be 1 leaked shared_memory objects to clean up at shutdown
  warnings.warn('resource_tracker: There appear to be %d '`

// TestTerminationExcerpt pins what lastTerminationDetail keeps of a long
// termination message (#1856): the line that names the failure, not the
// shutdown noise at the end of the log window.
func TestTerminationExcerpt(t *testing.T) {
	long := strings.Repeat("x", 300)
	tests := []struct {
		name     string
		msg      string
		want     []string
		wantNone []string
		exact    string
	}{
		{
			name:  "short message is kept whole, trimmed",
			msg:   "  value_error: no config.json  ",
			exact: "value_error: no config.json",
		},
		{
			name:     "vLLM traceback keeps the root-cause exception, not the teardown tail",
			msg:      vllmKVCacheTermination,
			want:     []string{"ValueError: No available memory for the cache blocks."},
			wantNone: []string{"resource_tracker", "RuntimeError", "(EngineCore pid=175)"},
		},
		{
			name: "a runtime without exceptions keeps its first error line",
			msg: strings.Repeat("llama_model_loader: - kv  12: general.name str = model\n", 6) +
				"llama_model_load: error loading model: tensor 'blk.0.attn_q.weight' has wrong shape\n" +
				"main: exiting due to model loading error\n" +
				strings.Repeat("ggml_backend_cuda_free: freed buffer\n", 4),
			want:     []string{"llama_model_load: error loading model: tensor 'blk.0.attn_q.weight' has wrong shape"},
			wantNone: []string{"ggml_backend_cuda_free"},
		},
		{
			name:     "with no recognisable failure line both ends survive",
			msg:      "HEAD-" + long + "-TAIL",
			want:     []string{"HEAD-", "-TAIL", " ... "},
			wantNone: []string{long},
		},
		{
			name: "an over-long exception line keeps its head",
			msg:  "noise\ntorch.OutOfMemoryError: CUDA out of memory. " + long + "\nmore noise",
			want: []string{"...torch.OutOfMemoryError: CUDA out of memory."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := terminationExcerpt(tt.msg, terminationDetailLimit)
			if tt.exact != "" && got != tt.exact {
				t.Fatalf("got %q, want %q", got, tt.exact)
			}
			// The budget is the existing 200 bytes plus the ellipsis markers.
			if len(got) > terminationDetailLimit+len("......") {
				t.Errorf("excerpt is %d bytes, over the bound: %q", len(got), got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("excerpt %q lacks %q", got, w)
				}
			}
			for _, w := range tt.wantNone {
				if strings.Contains(got, w) {
					t.Errorf("excerpt %q should not carry %q", got, w)
				}
			}
		})
	}
}

// TestTerminationExcerptUTF8 keeps cuts on rune boundaries, so the excerpt a
// condition message carries is valid UTF-8.
func TestTerminationExcerptUTF8(t *testing.T) {
	got := terminationExcerpt(strings.Repeat("é", 300), terminationDetailLimit)
	if !utf8.ValidString(got) {
		t.Fatalf("excerpt is not valid UTF-8: %q", got)
	}
}

func TestMultiNodeRecreateBackoff(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := multiNodeRecreateBackoff(int32(i + 1)); got != w {
			t.Errorf("backoff after failure %d: got %s, want %s", i+1, got, w)
		}
	}
}

// TestCountGroupFailure pins the #1856 retry policy: consecutive member
// failures accumulate with a doubling backoff, one failure is counted once
// however many reconciles observe it, a group that served for
// multiNodeRecreateResetAfter starts over, and past the bound no recreate is
// scheduled.
func TestCountGroupFailure(t *testing.T) {
	now := time.Now()
	pod := func(uid string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ring-mn-1", UID: types.UID(uid)}}
	}
	prev := func(count int32, uid string) *inferencev1alpha1.MultiNodeRecreateStatus {
		return &inferencev1alpha1.MultiNodeRecreateStatus{Count: count, GroupHash: "abc", PodUID: uid}
	}
	tests := []struct {
		name        string
		prev        *inferencev1alpha1.MultiNodeRecreateStatus
		uid         string
		readySince  time.Time
		wantCount   int32
		wantCounted bool
		wantNext    time.Duration // 0: no recreate scheduled
	}{
		{name: "first failure", prev: nil, uid: "u1", wantCount: 1, wantCounted: true, wantNext: 30 * time.Second},
		{name: "consecutive failure doubles the wait", prev: prev(1, "u1"), uid: "u2", wantCount: 2, wantCounted: true, wantNext: time.Minute},
		{name: "the same pod observed again is not counted twice", prev: prev(2, "u2"), uid: "u2", wantCount: 2, wantCounted: false},
		{name: "a brief Ready does not reset the count", prev: prev(2, "u2"), uid: "u3", readySince: now.Add(-time.Minute), wantCount: 3, wantCounted: true, wantNext: 2 * time.Minute},
		{name: "a sustained Ready resets the count", prev: prev(3, "u3"), uid: "u4", readySince: now.Add(-multiNodeRecreateResetAfter - time.Second), wantCount: 1, wantCounted: true, wantNext: 30 * time.Second},
		{name: "the failure past the bound schedules no recreate", prev: prev(multiNodeMaxRecreates, "u3"), uid: "u4", wantCount: multiNodeMaxRecreates + 1, wantCounted: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, counted := countGroupFailure(tt.prev, "abc", pod(tt.uid), multiNodeReasonMemberRestarted, "ring-mn-1 restarted 1 time(s)", tt.readySince, now)
			if counted != tt.wantCounted || rec.Count != tt.wantCount {
				t.Fatalf("got count %d counted %v, want %d %v", rec.Count, counted, tt.wantCount, tt.wantCounted)
			}
			if !counted {
				return
			}
			if rec.PodUID != tt.uid || rec.GroupHash != "abc" || rec.Reason != multiNodeReasonMemberRestarted || rec.LastFailureTime == nil {
				t.Errorf("record does not carry the failure: %+v", rec)
			}
			switch {
			case tt.wantNext == 0 && rec.NextRecreateTime != nil:
				t.Errorf("no recreate expected past the bound, got %s", rec.NextRecreateTime)
			case tt.wantNext != 0 && (rec.NextRecreateTime == nil || !rec.NextRecreateTime.Time.Equal(now.Add(tt.wantNext))):
				t.Errorf("next recreate: got %v, want now+%s", rec.NextRecreateTime, tt.wantNext)
			}
		})
	}
}

// TestMultiNodeRecreateResetRequeue: a Ready group with a failure count asks
// to be reconciled when the count is due to be forgotten.
func TestMultiNodeRecreateResetRequeue(t *testing.T) {
	now := time.Now()
	isvc, _ := multiNodeFixture()
	if got := multiNodeRecreateResetRequeue(isvc, now); got != 0 {
		t.Fatalf("no record, no requeue: got %s", got)
	}
	isvc.Status.MultiNode = &inferencev1alpha1.MultiNodeStatus{Recreates: &inferencev1alpha1.MultiNodeRecreateStatus{Count: 1}}
	if got := multiNodeRecreateResetRequeue(isvc, now); got != 0 {
		t.Fatalf("a group that is not Ready has nothing to wait for: got %s", got)
	}
	isvc.Status.Conditions = []metav1.Condition{{
		Type: ConditionMultiNodeGroupReady, Status: metav1.ConditionTrue, Reason: "AllMembersRunning",
		LastTransitionTime: metav1.NewTime(now.Add(-time.Minute)),
	}}
	if got := multiNodeRecreateResetRequeue(isvc, now); got != multiNodeRecreateResetAfter-time.Minute {
		t.Fatalf("got %s, want %s", got, multiNodeRecreateResetAfter-time.Minute)
	}
}

// memoryPrep returns the pod's multinode-memory-prep init container and its
// index, or -1.
func memoryPrep(p *corev1.Pod) (corev1.Container, int) {
	for i, c := range p.Spec.InitContainers {
		if c.Name == multiNodeMemoryPrepContainer {
			return c, i
		}
	}
	return corev1.Container{}, -1
}

// #1857: spec.multiNode.memory puts one privileged init container on every
// member, after the downloader, that writes the sysctls and drops the cache.
func TestConstructMemberPodsMemoryPrep(t *testing.T) {
	isvc, model := multiNodeFixture()
	isvc.Spec.MultiNode.Memory = &inferencev1alpha1.MultiNodeMemory{
		DropPageCacheBeforeBoot: true,
		Sysctls: map[string]inferencev1alpha1.MultiNodeSysctlValue{
			"vm.watermark_scale_factor": "200",
			"vm.min_free_kbytes":        "1048576",
		},
	}
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService, InitContainerImage: "mirror.example/curl:1"}
	pods, _, err := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	for _, p := range pods {
		c, i := memoryPrep(p)
		if i < 0 {
			t.Fatalf("%s: no %s init container: %v", p.Name, multiNodeMemoryPrepContainer, p.Spec.InitContainers)
		}
		if i != len(p.Spec.InitContainers)-1 {
			t.Errorf("%s: memory prep is init container %d of %d; it must run last, after the weights are staged", p.Name, i, len(p.Spec.InitContainers))
		}
		if c.Image != "mirror.example/curl:1" {
			t.Errorf("%s: image %q, want the operator's init container image", p.Name, c.Image)
		}
		sc := c.SecurityContext
		if sc == nil || sc.Privileged == nil || !*sc.Privileged || sc.RunAsUser == nil || *sc.RunAsUser != 0 {
			t.Errorf("%s: memory prep must be privileged and root to write /proc/sys: %+v", p.Name, sc)
		}
		if len(c.VolumeMounts) != 0 {
			t.Errorf("%s: memory prep must mount nothing: %v", p.Name, c.VolumeMounts)
		}
		// sh -c <script> <$0> <sysctls sorted by name>
		wantArgs := []string{"vm.min_free_kbytes=1048576", "vm.watermark_scale_factor=200"}
		if len(c.Command) != 4+len(wantArgs) || c.Command[0] != "sh" || c.Command[1] != "-c" || c.Command[2] != multiNodeMemoryPrepScript {
			t.Fatalf("%s: command = %q", p.Name, c.Command)
		}
		if got := strings.Join(c.Command[4:], " "); got != strings.Join(wantArgs, " ") {
			t.Errorf("%s: sysctl args = %q, want %q", p.Name, got, strings.Join(wantArgs, " "))
		}
		if len(c.Env) != 1 || c.Env[0].Name != "DROP_PAGE_CACHE" || c.Env[0].Value != "1" {
			t.Errorf("%s: env = %v, want DROP_PAGE_CACHE=1", p.Name, c.Env)
		}
	}

	// Sysctls alone must not drop the cache.
	isvc.Spec.MultiNode.Memory.DropPageCacheBeforeBoot = false
	pods, _, err = r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	if c, i := memoryPrep(pods[0]); i < 0 || c.Env[0].Value != "0" {
		t.Errorf("sysctls without the drop: container index %d, env %v; want DROP_PAGE_CACHE=0", i, c.Env)
	}
}

// The drop must stay in the script and come after the sysctl loop: the
// rendered-spec assertions above pass with an empty script.
func TestMultiNodeMemoryPrepScriptOrder(t *testing.T) {
	sysctl := strings.Index(multiNodeMemoryPrepScript, `echo "$v" > "$f"`)
	drop := strings.Index(multiNodeMemoryPrepScript, "echo 3 > /proc/sys/vm/drop_caches")
	sync := strings.Index(multiNodeMemoryPrepScript, "\n  sync\n")
	if sysctl < 0 || drop < 0 || sync < 0 {
		t.Fatalf("script lost a step (sysctl write %d, sync %d, drop %d):\n%s", sysctl, sync, drop, multiNodeMemoryPrepScript)
	}
	if sysctl >= sync || sync >= drop {
		t.Errorf("order must be sysctls, sync, drop; got offsets %d, %d, %d", sysctl, sync, drop)
	}
	if !strings.HasPrefix(multiNodeMemoryPrepScript, "set -eu\n") {
		t.Errorf("script must fail the init container on a refused write (set -eu)")
	}
}

// An unset or empty memory block must leave the member pods, and so the group
// hash, exactly as they were: an operator upgrade must not recreate a serving
// group that never asked for the prep.
func TestConstructMemberPodsMemoryUnsetIsInert(t *testing.T) {
	isvc, model := multiNodeFixture()
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	basePods, baseHash, err := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	if _, i := memoryPrep(basePods[0]); i >= 0 {
		t.Fatalf("no memory block must add no init container")
	}

	isvc.Spec.MultiNode.Memory = &inferencev1alpha1.MultiNodeMemory{}
	emptyPods, emptyHash, err := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	if _, i := memoryPrep(emptyPods[0]); i >= 0 || emptyHash != baseHash {
		t.Errorf("an empty memory block must change nothing: prep index %d, hash %s vs %s", i, emptyHash, baseHash)
	}

	isvc.Spec.MultiNode.Memory.DropPageCacheBeforeBoot = true
	_, dropHash, err := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	if dropHash == baseHash {
		t.Errorf("turning the drop on must change the group hash so the group is recreated with it")
	}
}

func TestValidateMultiNodeMemory(t *testing.T) {
	cases := []struct {
		name    string
		sysctls map[string]inferencev1alpha1.MultiNodeSysctlValue
		wantErr string
	}{
		{"vm sysctls", map[string]inferencev1alpha1.MultiNodeSysctlValue{"vm.min_free_kbytes": "1048576", "vm.watermark_scale_factor": "200"}, ""},
		{"none", nil, ""},
		{"other subsystem", map[string]inferencev1alpha1.MultiNodeSysctlValue{"kernel.panic": "1"}, `key "kernel.panic"`},
		{"path traversal", map[string]inferencev1alpha1.MultiNodeSysctlValue{"vm.../kernel/panic": "1"}, "is not a vm.<name> sysctl"},
		{"nested name", map[string]inferencev1alpha1.MultiNodeSysctlValue{"vm.a.b": "1"}, "is not a vm.<name> sysctl"},
		{"shell in the name", map[string]inferencev1alpha1.MultiNodeSysctlValue{"vm.x$(reboot)": "1"}, "is not a vm.<name> sysctl"},
		{"shell in the value", map[string]inferencev1alpha1.MultiNodeSysctlValue{"vm.min_free_kbytes": "1; reboot"}, "is not a non-negative integer"},
		{"negative value", map[string]inferencev1alpha1.MultiNodeSysctlValue{"vm.min_free_kbytes": "-1"}, "is not a non-negative integer"},
		{"empty value", map[string]inferencev1alpha1.MultiNodeSysctlValue{"vm.min_free_kbytes": ""}, "is not a non-negative integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc, _ := multiNodeFixture()
			isvc.Spec.MultiNode.Memory = &inferencev1alpha1.MultiNodeMemory{Sysctls: tc.sysctls}
			err := validateMultiNode(isvc)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// The prep exists to clear what staging left in the page cache, so it must
// come after the model-downloader on a member that has one (#2527 review).
func TestConstructMemberPodsMemoryPrepRunsAfterDownloader(t *testing.T) {
	isvc, model := multiNodeFixture()
	isvc.Spec.MultiNode.Memory = &inferencev1alpha1.MultiNodeMemory{DropPageCacheBeforeBoot: true}
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	pods, _, err := r.constructMemberPods(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	for _, p := range pods {
		downloader := -1
		for i, c := range p.Spec.InitContainers {
			if c.Name == "model-downloader" {
				downloader = i
			}
		}
		if downloader < 0 {
			t.Fatalf("%s: the fixture no longer renders a model-downloader, so this test proves nothing: %v", p.Name, p.Spec.InitContainers)
		}
		if _, prep := memoryPrep(p); prep <= downloader {
			t.Errorf("%s: memory prep is init container %d, model-downloader is %d; the prep must run after the weights are staged", p.Name, prep, downloader)
		}
	}
}

// The CRD caps sysctls at 16 entries; the controller repeats the cap for an
// object stored under a CRD that did not have it (#2527 review).
func TestValidateMultiNodeMemorySysctlLimit(t *testing.T) {
	build := func(n int) *inferencev1alpha1.InferenceService {
		isvc, _ := multiNodeFixture()
		s := map[string]inferencev1alpha1.MultiNodeSysctlValue{}
		for i := range n {
			s["vm.knob_"+strconv.Itoa(i)] = "1"
		}
		isvc.Spec.MultiNode.Memory = &inferencev1alpha1.MultiNodeMemory{Sysctls: s}
		return isvc
	}
	if err := validateMultiNode(build(multiNodeMaxSysctls)); err != nil {
		t.Errorf("%d sysctls must be accepted: %v", multiNodeMaxSysctls, err)
	}
	err := validateMultiNode(build(multiNodeMaxSysctls + 1))
	if err == nil || !strings.Contains(err.Error(), "has 17 entries, at most 16 are allowed") {
		t.Errorf("17 sysctls: err = %v, want the entry limit", err)
	}
}
