package controller

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// envtest has no kubelet, so member pods stay Pending. These specs cover what
// the reconciler owns: creation, ownership, no Deployment, status shape, and
// the recreate paths. Readiness math is unit-tested in multinode_test.go.
var _ = Describe("multiNode group reconcile", func() {
	const (
		ns        = "default"
		modelName = "mn-model"
		isvcName  = "mn-ring"
	)
	var (
		ctx        context.Context
		reconciler *InferenceServiceReconciler
	)

	key := types.NamespacedName{Name: isvcName, Namespace: ns}
	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}
	memberPods := func() []corev1.Pod {
		var list corev1.PodList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{LabelMultiNodeGroup: isvcName})).To(Succeed())
		return list.Items
	}
	// finishTerminating plays the kubelet: envtest has none, so a pod the
	// reconciler deleted sits Terminating forever until force-removed.
	finishTerminating := func() {
		for _, p := range memberPods() {
			if p.DeletionTimestamp == nil {
				continue
			}
			pod := p
			_ = k8sClient.Delete(ctx, &pod, client.GracePeriodSeconds(0))
		}
	}
	memberPodNames := func() []string {
		pods := memberPods()
		names := make([]string, 0, len(pods))
		for _, p := range pods {
			names = append(names, p.Name)
		}
		return names
	}
	// stagedNoRuntime plays a member whose weights are on disk but whose
	// runtime has not started: every init container exited 0, the main
	// container is still waiting. The group is staged, not formed.
	stagedNoRuntime := func(name string) {
		var p corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &p)).To(Succeed())
		p.Status.Phase = corev1.PodRunning
		ics := make([]corev1.ContainerStatus, 0, len(p.Spec.InitContainers))
		for _, c := range p.Spec.InitContainers {
			ics = append(ics, corev1.ContainerStatus{
				Name: c.Name,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0, FinishedAt: metav1.Now(),
				}},
			})
		}
		p.Status.InitContainerStatuses = ics
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "vllm",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}
		Expect(k8sClient.Status().Update(ctx, &p)).To(Succeed())
	}

	// expireBackoff moves the pending recreate into the past so a spec does
	// not sleep through the backoff between failure-driven recreates (#1856).
	expireBackoff := func() {
		var got inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.MultiNode).NotTo(BeNil())
		rec := got.Status.MultiNode.Recreates
		Expect(rec).NotTo(BeNil())
		Expect(rec.NextRecreateTime).NotTo(BeNil())
		past := metav1.NewTime(time.Now().Add(-time.Second))
		rec.NextRecreateTime = &past
		Expect(k8sClient.Status().Update(ctx, &got)).To(Succeed())
	}

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			ModelCachePath:     "/models",
			ModelCacheMode:     ModelCacheModePerService,
		}
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: ns},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "s3://models/org/name", Format: "safetensors",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cuda", GPU: &inferencev1alpha1.GPUSpec{Enabled: true, Count: 1, Vendor: "nvidia"}},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.Phase = PhaseReady
		model.Status.CacheKey = "org-name"
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, model) })

		// The chart ships the llmkube-* PriorityClasses; envtest has none, and
		// unlike a Deployment a directly created pod is validated against it.
		pc := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "llmkube-normal"}, Value: 1000}
		if err := k8sClient.Create(ctx, pc); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		// Claims are user-owned: the service claim is checked by
		// ensureModelCachePVC, member claims by checkMemberClaims.
		for _, name := range []string{"mn-claim-0", "mn-claim-1"} {
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
				},
			}
			// envtest runs no pvc-protection controller, so a deleted claim
			// never finishes terminating: create once, keep for the suite.
			if err := k8sClient.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		}

		tp := int32(2)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: ns},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: "vllm", ModelRef: modelName,
				Resources:  &inferencev1alpha1.InferenceResourceRequirements{GPU: 1},
				ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "mn-claim-0"},
				VLLMConfig: &inferencev1alpha1.VLLMConfig{TensorParallelSize: &tp},
				MultiNode: &inferencev1alpha1.MultiNodeSpec{Members: []inferencev1alpha1.MultiNodeMember{
					{Node: "node-a", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.1"}},
					{Node: "node-b", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.2"}, ModelCache: &inferencev1alpha1.MultiNodeMemberCache{ClaimName: "mn-claim-1"}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, isvc)
			// envtest runs no garbage collector: remove the members and the
			// Service by hand, since both are owner-referenced. The
			// InferenceService is waited out first, so no reconcile can wake
			// and recreate the Service after its own delete.
			Eventually(func() bool {
				var got inferencev1alpha1.InferenceService
				return apierrors.IsNotFound(k8sClient.Get(ctx, key, &got))
			}).Should(BeTrue())
			for _, p := range memberPods() {
				pod := p
				_ = k8sClient.Delete(ctx, &pod, client.GracePeriodSeconds(0))
			}
			Eventually(memberPods).Should(BeEmpty())
			_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: ns}})
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: ns}, &corev1.Service{}))
			}).Should(BeTrue(), "an orphaned Service would hold the name for the next spec")
		})
	})

	It("creates one owned pod per member and no Deployment", func() {
		reconcileOnce()
		pods := memberPods()
		Expect(pods).To(HaveLen(2))

		var owner inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &owner)).To(Succeed())
		for _, p := range pods {
			pod := p
			Expect(metav1.IsControlledBy(&pod, &owner)).To(BeTrue(), "%s must be controlled by the ISVC", pod.Name)
			Expect(pod.Spec.HostNetwork).To(BeTrue())
		}
		Expect(memberPodNames()).To(ConsistOf("mn-ring-mn-0", "mn-ring-mn-1"))

		var dep appsv1.Deployment
		err := k8sClient.Get(ctx, key, &dep)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no Deployment for a multiNode service")

		Expect(owner.Status.MultiNode).NotTo(BeNil())
		Expect(owner.Status.MultiNode.Size).To(Equal(int32(2)))
		Expect(owner.Status.MultiNode.Members).To(HaveLen(2))
		Expect(owner.Status.MultiNode.Members[1].Node).To(Equal("node-b"))
		cond := meta.FindStatusCondition(owner.Status.Conditions, ConditionMultiNodeGroupReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(owner.Status.Phase).NotTo(Equal(PhaseReady))
	})

	// Each member is pinned to a node and requests its GPU; a member that
	// cannot mount the CA ConfigMap would hold that GPU in ContainerCreating.
	It("holds member creation while the CA ConfigMap is missing, then forms the group", func() {
		caName := "mn-corp-ca"
		reconciler.CACertConfigMap = caName

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(memberPods()).To(BeEmpty(), "no member may be created without the CA ConfigMap")

		var got inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal("Pending"))
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionCACertConfigMapAvailable)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(ContainSubstring(caName))

		cm := caConfigMap(caName, ns)
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cm) })

		reconcileOnce()
		pods := memberPods()
		Expect(pods).To(HaveLen(2))
		for i := range pods {
			Expect(podSpecMountsConfigMap(&pods[i].Spec, caName)).To(BeTrue(), "%s keeps the CA mount", pods[i].Name)
		}
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		cond = meta.FindStatusCondition(got.Status.Conditions, ConditionCACertConfigMapAvailable)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("recreates the whole group when a member fails", func() {
		reconcileOnce()
		Expect(memberPods()).To(HaveLen(2))

		var worker corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mn-ring-mn-1", Namespace: ns}, &worker)).To(Succeed())
		worker.Status.Phase = corev1.PodFailed
		Expect(k8sClient.Status().Update(ctx, &worker)).To(Succeed())

		reconcileOnce() // observes the failure, deletes the group
		finishTerminating()
		Eventually(memberPods).Should(BeEmpty())

		var mid inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &mid)).To(Succeed())
		cond := meta.FindStatusCondition(mid.Status.Conditions, ConditionMultiNodeGroupReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("MemberFailed"))

		reconcileOnce() // a failure-driven recreate waits out its backoff (#1856)
		Expect(memberPods()).To(BeEmpty())
		expireBackoff()
		reconcileOnce() // recreates
		Expect(memberPods()).To(HaveLen(2))
	})

	It("recreates the group when the spec changes", func() {
		reconcileOnce()
		before := memberPods()
		Expect(before).To(HaveLen(2))
		oldHash := before[0].Annotations[AnnotationMultiNodeGroupHash]
		Expect(oldHash).NotTo(BeEmpty())

		var isvc inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &isvc)).To(Succeed())
		isvc.Spec.MultiNode.Members[1].Fabric.SocketInterface = "enp9"
		Expect(k8sClient.Update(ctx, &isvc)).To(Succeed())

		reconcileOnce() // stale -> delete
		finishTerminating()
		Eventually(memberPods).Should(BeEmpty())
		reconcileOnce() // recreate
		after := memberPods()
		Expect(after).To(HaveLen(2))
		Expect(after[0].Annotations[AnnotationMultiNodeGroupHash]).NotTo(Equal(oldHash))
	})

	// #1857: the memory block reaches the member pods the API server stores,
	// and the CRD refuses a sysctl outside vm.* before the controller sees it.
	It("adds the memory prep to every member and recreates the group for it", func() {
		reconcileOnce()
		for _, p := range memberPods() {
			for _, c := range p.Spec.InitContainers {
				Expect(c.Name).NotTo(Equal(multiNodeMemoryPrepContainer), "%s: no memory block, no prep", p.Name)
			}
		}

		var isvc inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &isvc)).To(Succeed())
		isvc.Spec.MultiNode.Memory = &inferencev1alpha1.MultiNodeMemory{
			DropPageCacheBeforeBoot: true,
			Sysctls:                 map[string]inferencev1alpha1.MultiNodeSysctlValue{"vm.min_free_kbytes": "1048576"},
		}
		Expect(k8sClient.Update(ctx, &isvc)).To(Succeed())

		reconcileOnce() // stale -> delete
		finishTerminating()
		Eventually(memberPods).Should(BeEmpty())
		reconcileOnce() // recreate
		pods := memberPods()
		Expect(pods).To(HaveLen(2))
		for _, p := range pods {
			Expect(p.Spec.InitContainers).NotTo(BeEmpty())
			last := p.Spec.InitContainers[len(p.Spec.InitContainers)-1]
			Expect(last.Name).To(Equal(multiNodeMemoryPrepContainer), "%s: the prep runs last", p.Name)
			Expect(last.SecurityContext).NotTo(BeNil())
			Expect(last.SecurityContext.Privileged).To(HaveValue(BeTrue()))
			Expect(last.Command).To(ContainElement("vm.min_free_kbytes=1048576"))
			Expect(last.Env).To(ContainElement(corev1.EnvVar{Name: "DROP_PAGE_CACHE", Value: "1"}))
		}
	})

	It("refuses memory sysctls that are not vm.* integers at admission", func() {
		for _, tc := range []struct {
			bad  map[string]inferencev1alpha1.MultiNodeSysctlValue
			want string
		}{
			{map[string]inferencev1alpha1.MultiNodeSysctlValue{"kernel.panic": "1"}, "sysctls keys must be vm.<name>"},
			{map[string]inferencev1alpha1.MultiNodeSysctlValue{"vm.../kernel/panic": "1"}, "sysctls keys must be vm.<name>"},
			{map[string]inferencev1alpha1.MultiNodeSysctlValue{"vm.min_free_kbytes": "1; reboot"}, "should match '^[0-9]+$'"},
		} {
			var isvc inferencev1alpha1.InferenceService
			Expect(k8sClient.Get(ctx, key, &isvc)).To(Succeed())
			isvc.Spec.MultiNode.Memory = &inferencev1alpha1.MultiNodeMemory{Sysctls: tc.bad}
			err := k8sClient.Update(ctx, &isvc)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "sysctls %v must be rejected, got %v", tc.bad, err)
			Expect(err.Error()).To(ContainSubstring(tc.want))
		}
	})

	It("fails clearly when a member claim is missing", func() {
		var isvc inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &isvc)).To(Succeed())
		isvc.Spec.MultiNode.Members[1].ModelCache.ClaimName = "does-not-exist"
		Expect(k8sClient.Update(ctx, &isvc)).To(Succeed())

		reconcileOnce()
		Expect(memberPods()).To(BeEmpty())
		var got inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(PhaseFailed))
		degraded := meta.FindStatusCondition(got.Status.Conditions, ConditionDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Message).To(ContainSubstring(`members[1].modelCache.claimName "does-not-exist" not found`))
	})

	It("rejects a world size that does not fill the group", func() {
		var isvc inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &isvc)).To(Succeed())
		one := int32(1)
		isvc.Spec.VLLMConfig.TensorParallelSize = &one
		Expect(k8sClient.Update(ctx, &isvc)).To(Succeed())

		reconcileOnce()
		Expect(memberPods()).To(BeEmpty())
		var got inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(PhaseFailed))
		degraded := meta.FindStatusCondition(got.Status.Conditions, ConditionDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Message).To(ContainSubstring("multiNode group provides 2 GPUs"))
	})

	// #2061: member pods pin spec.nodeName, which bypasses the scheduler that
	// allocates DRA claims, so the combination can never run.
	It("parks a multiNode service whose Model uses DRA resourceClaims", func() {
		claimName := "mn-dra-claim"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "mn-dra-model", Namespace: ns},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "s3://models/org/dra", Format: "safetensors",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cuda", GPU: &inferencev1alpha1.GPUSpec{
					Enabled:        true,
					ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: &claimName}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.Phase = PhaseReady
		model.Status.CacheKey = "org-dra"
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, model) })

		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "mn-dra", Namespace: ns},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: "vllm", ModelRef: "mn-dra-model",
				Resources: &inferencev1alpha1.InferenceResourceRequirements{GPU: 1},
				MultiNode: &inferencev1alpha1.MultiNodeSpec{Members: []inferencev1alpha1.MultiNodeMember{
					{Node: "node-a", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.1"}},
					{Node: "node-b", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.2"}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, isvc) })

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "mn-dra", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		var list corev1.PodList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{LabelMultiNodeGroup: "mn-dra"})).To(Succeed())
		Expect(list.Items).To(BeEmpty(), "no member pod may be created for a multiNode DRA service")

		var got inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mn-dra", Namespace: ns}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(PhaseFailed))
		degraded := meta.FindStatusCondition(got.Status.Conditions, ConditionDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Message).To(ContainSubstring("resourceClaims"))
	})

	It("holds the group while a member stages, then polices post-formation restarts", func() {
		// #1757: one member's claim starts empty, so its init container is
		// still downloading while the other member's runtime waits for the
		// rendezvous, exits, and is restarted by the kubelet. Recreating the
		// group on that exit kills the downloader and loses its progress,
		// which never converges: 62 recreates in 20 minutes in the field.
		reconcileOnce()
		Expect(memberPods()).To(HaveLen(2))

		// mn-1 is staging: init container running, runtime never started.
		var staging corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mn-ring-mn-1", Namespace: ns}, &staging)).To(Succeed())
		staging.Status.Phase = corev1.PodPending
		staging.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name:  "model-downloader",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
		}}
		Expect(k8sClient.Status().Update(ctx, &staging)).To(Succeed())

		// mn-0 started, could not rendezvous, exited, was restarted.
		var waiting corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mn-ring-mn-0", Namespace: ns}, &waiting)).To(Succeed())
		waiting.Status.Phase = corev1.PodRunning
		waiting.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "vllm", RestartCount: 2,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Reason: "Error", FinishedAt: metav1.Now(),
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, &waiting)).To(Succeed())

		reconcileOnce()
		Expect(memberPods()).To(HaveLen(2), "formation noise must not tear the group down while a member is staging")
		var held inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &held)).To(Succeed())
		Expect(meta.FindStatusCondition(held.Status.Conditions, ConditionMultiNodeFormed)).To(BeNil(),
			"a group with a staging member is not formed")
		Expect(meta.FindStatusCondition(held.Status.Conditions, ConditionMultiNodeStaged)).To(BeNil(),
			"a staging member does not anchor the post-staging deadline")

		// Staging completes: the downloader's init container exits 0 and the
		// runtime starts. The group forms, stamping a fresh timestamp.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mn-ring-mn-1", Namespace: ns}, &staging)).To(Succeed())
		staging.Status.Phase = corev1.PodRunning
		staging.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name:  "model-downloader",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: metav1.Now()}},
		}}
		staging.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "vllm",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
		}}
		Expect(k8sClient.Status().Update(ctx, &staging)).To(Succeed())

		reconcileOnce()
		var formed inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &formed)).To(Succeed())
		formCond := meta.FindStatusCondition(formed.Status.Conditions, ConditionMultiNodeFormed)
		Expect(formCond).NotTo(BeNil())
		Expect(formCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(memberPods()).To(HaveLen(2), "the pre-formation exits finished at or before the formation timestamp")

		// A termination strictly after formation is a real crash and must
		// recreate, proving the masking is not permanent. The timestamp is
		// nudged into the future so the ordering against the stamp cannot race
		// the test clock.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mn-ring-mn-0", Namespace: ns}, &waiting)).To(Succeed())
		waiting.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "vllm", RestartCount: 3,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.NewTime(time.Now().Add(time.Second)),
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, &waiting)).To(Succeed())

		reconcileOnce() // observes the post-formation crash, deletes the group
		finishTerminating()
		Eventually(memberPods).Should(BeEmpty())
		var torn inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &torn)).To(Succeed())
		cond := meta.FindStatusCondition(torn.Status.Conditions, ConditionMultiNodeGroupReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("MemberRestarted"))
		Expect(meta.FindStatusCondition(torn.Status.Conditions, ConditionMultiNodeFormed)).To(BeNil(),
			"teardown clears the formation timestamp so the next group forms afresh")
		Expect(meta.FindStatusCondition(torn.Status.Conditions, ConditionMultiNodeStaged)).To(BeNil(),
			"teardown clears the staging deadline too")
	})

	It("recreates a group that never forms after staging ends", func() {
		// The formation mask must not hide a runtime crashloop forever. Once
		// every member has finished staging, the group has a bounded window to
		// form; past it a restart is fatal, not rendezvous noise.
		reconcileOnce()
		Expect(memberPods()).To(HaveLen(2))

		// Both members have finished staging but their runtimes have not
		// started, so the group is staged but not formed.
		stagedNoRuntime("mn-ring-mn-0")
		stagedNoRuntime("mn-ring-mn-1")

		reconcileOnce()
		var staged inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &staged)).To(Succeed())
		stagedCond := meta.FindStatusCondition(staged.Status.Conditions, ConditionMultiNodeStaged)
		Expect(stagedCond).NotTo(BeNil(), "every member finished staging")
		Expect(stagedCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(meta.FindStatusCondition(staged.Status.Conditions, ConditionMultiNodeFormed)).To(BeNil(),
			"a runtime that has not started does not form the group")

		// mn-0's runtime keeps dying while the group never forms.
		var crashing corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mn-ring-mn-0", Namespace: ns}, &crashing)).To(Succeed())
		crashing.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "vllm", RestartCount: 2,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.NewTime(time.Now().Add(-time.Minute)),
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, &crashing)).To(Succeed())

		// Inside the post-staging window the restart is still masked; the
		// deadline is aged past the budget so the bound is what is under test.
		reconcileOnce()
		Expect(memberPods()).To(HaveLen(2), "inside the post-staging window the restart is noise")

		var aging inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &aging)).To(Succeed())
		ageCond := meta.FindStatusCondition(aging.Status.Conditions, ConditionMultiNodeStaged)
		Expect(ageCond).NotTo(BeNil())
		ageCond.LastTransitionTime = metav1.NewTime(time.Now().Add(-multiNodeStartupBudget - time.Minute))
		Expect(k8sClient.Status().Update(ctx, &aging)).To(Succeed())

		reconcileOnce() // past the deadline the crashloop recreates the group
		finishTerminating()
		Eventually(memberPods).Should(BeEmpty())
		var torn inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &torn)).To(Succeed())
		cond := meta.FindStatusCondition(torn.Status.Conditions, ConditionMultiNodeGroupReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("MemberRestarted"))
		Expect(meta.FindStatusCondition(torn.Status.Conditions, ConditionMultiNodeStaged)).To(BeNil(),
			"teardown clears the staging deadline")
	})

	Context("bounded recreates (#1856)", func() {
		getISVC := func() inferencev1alpha1.InferenceService {
			var got inferencev1alpha1.InferenceService
			Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
			return got
		}
		recreates := func() *inferencev1alpha1.MultiNodeRecreateStatus {
			got := getISVC()
			if got.Status.MultiNode == nil {
				return nil
			}
			return got.Status.MultiNode.Recreates
		}
		groupCond := func() *metav1.Condition {
			got := getISVC()
			return meta.FindStatusCondition(got.Status.Conditions, ConditionMultiNodeGroupReady)
		}
		// failMember plays the kubelet reporting a member whose runtime
		// exited for good with the #1856 KV cache traceback.
		failMember := func(name string) {
			var p corev1.Pod
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &p)).To(Succeed())
			p.Status.Phase = corev1.PodFailed
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "vllm",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1, Reason: "Error", Message: vllmKVCacheTermination, FinishedAt: metav1.Now(),
				}},
			}}
			Expect(k8sClient.Status().Update(ctx, &p)).To(Succeed())
		}
		// markGroupReady plays the kubelet bringing every runtime up with
		// rank 0 passing its readiness probe.
		markGroupReady := func() {
			for _, name := range []string{"mn-ring-mn-0", "mn-ring-mn-1"} {
				var p corev1.Pod
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &p)).To(Succeed())
				p.Status.Phase = corev1.PodRunning
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "vllm", Ready: name == "mn-ring-mn-0",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
				}}
				if name == "mn-ring-mn-0" {
					p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				}
				Expect(k8sClient.Status().Update(ctx, &p)).To(Succeed())
			}
		}
		// failAndTearDown fails the worker and lets the reconciler count it
		// and delete the group.
		failAndTearDown := func() {
			failMember("mn-ring-mn-1")
			reconcileOnce()
			finishTerminating()
			Eventually(memberPods).Should(BeEmpty())
		}
		drain := func(rec *events.FakeRecorder) []string {
			var out []string
			for {
				select {
				case e := <-rec.Events:
					out = append(out, e)
				default:
					return out
				}
			}
		}
		countWith := func(evts []string, substr string) int {
			n := 0
			for _, e := range evts {
				if strings.Contains(e, substr) {
					n++
				}
			}
			return n
		}

		It("backs off between recreates and marks a deterministic failure Failed after the bound", func() {
			recorder := events.NewFakeRecorder(64)
			reconciler.Recorder = recorder

			reconcileOnce()
			Expect(memberPods()).To(HaveLen(2))
			for i := int32(1); i <= multiNodeMaxRecreates; i++ {
				failAndTearDown()
				Expect(getISVC().Status.Phase).NotTo(Equal(PhaseFailed), "failure %d is inside the bound", i)
				rec := recreates()
				Expect(rec).NotTo(BeNil())
				Expect(rec.Count).To(Equal(i), "every recreate is counted where the per-pod restart count cannot")
				Expect(rec.Message).To(ContainSubstring("mn-ring-mn-1"))
				Expect(rec.Message).To(ContainSubstring("ValueError: No available memory for the cache blocks."))

				// The group is not recreated before the backoff has passed.
				res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				Expect(memberPods()).To(BeEmpty(), "recreate %d must wait out its backoff", i)
				Expect(res.RequeueAfter).To(BeNumerically(">", 0))
				Expect(res.RequeueAfter).To(BeNumerically("<=", multiNodeRecreateBackoff(i)))
				Expect(groupCond().Reason).To(Equal(multiNodeReasonBackOff))

				expireBackoff()
				reconcileOnce()
				Expect(memberPods()).To(HaveLen(2), "the group is recreated once its backoff has passed")
			}

			// The failure after the last allowed recreate is terminal.
			failAndTearDown()
			for range 3 {
				reconcileOnce()
				Expect(memberPods()).To(BeEmpty(), "a group past the bound is not recreated")
			}
			failed := getISVC()
			Expect(failed.Status.Phase).To(Equal(PhaseFailed))
			Expect(failed.Status.MultiNode.Recreates.Count).To(Equal(multiNodeMaxRecreates + 1))
			degraded := meta.FindStatusCondition(failed.Status.Conditions, ConditionDegraded)
			Expect(degraded).NotTo(BeNil())
			Expect(degraded.Message).To(ContainSubstring("mn-ring-mn-1"))
			Expect(degraded.Message).To(ContainSubstring("ValueError: No available memory for the cache blocks."))
			Expect(groupCond().Reason).To(Equal(multiNodeReasonRecreateLimit))

			evts := drain(recorder)
			Expect(countWith(evts, "MultiNodeGroupRecreating")).To(Equal(int(multiNodeMaxRecreates)), "one event per recreate: %v", evts)
			Expect(countWith(evts, "MultiNodeGroupFailed")).To(Equal(1), "one event at the bound: %v", evts)

			// Suspend and resume is the retry: the count is forgotten.
			zero, one := int32(0), int32(1)
			failed.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, &failed)).To(Succeed())
			reconcileOnce()
			Expect(recreates()).To(BeNil(), "suspending forgets the failure count")
			resumed := getISVC()
			resumed.Spec.Replicas = &one
			Expect(k8sClient.Update(ctx, &resumed)).To(Succeed())
			reconcileOnce()
			Expect(memberPods()).To(HaveLen(2), "a resumed group is created at once")
			Expect(getISVC().Status.Phase).NotTo(Equal(PhaseFailed))
		})

		It("recreates a group held at the bound once its spec changes", func() {
			reconcileOnce()
			Expect(memberPods()).To(HaveLen(2))
			// A record left by an older spec: past the bound, but for a hash
			// the current spec no longer renders.
			got := getISVC()
			got.Status.MultiNode.Recreates = &inferencev1alpha1.MultiNodeRecreateStatus{
				Count: multiNodeMaxRecreates + 1, Reason: multiNodeReasonMemberFailed, GroupHash: "stale-hash",
			}
			Expect(k8sClient.Status().Update(ctx, &got)).To(Succeed())
			reconcileOnce()
			Expect(memberPods()).To(HaveLen(2), "a record for another spec does not hold the group")
			Expect(recreates()).To(BeNil())
			Expect(getISVC().Status.Phase).NotTo(Equal(PhaseFailed))
		})

		It("recovers from a transient failure and forgets it once the group has served", func() {
			reconcileOnce()
			Expect(memberPods()).To(HaveLen(2))
			failAndTearDown()
			expireBackoff()
			reconcileOnce()
			Expect(memberPods()).To(HaveLen(2))

			// The retry comes up: the group serves.
			markGroupReady()
			res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(groupCond().Status).To(Equal(metav1.ConditionTrue))
			Expect(getISVC().Status.Phase).NotTo(Equal(PhaseFailed))
			Expect(recreates()).NotTo(BeNil())
			Expect(recreates().Count).To(Equal(int32(1)), "a moment of Ready does not forget the failure")
			Expect(res.RequeueAfter).To(BeNumerically("~", multiNodeRecreateResetAfter, 5*time.Second),
				"a healthy group raises no events, so the reset needs its own requeue")

			// Ready for longer than the reset window: the failure is forgotten.
			ready := getISVC()
			cond := meta.FindStatusCondition(ready.Status.Conditions, ConditionMultiNodeGroupReady)
			Expect(cond).NotTo(BeNil())
			cond.LastTransitionTime = metav1.NewTime(time.Now().Add(-multiNodeRecreateResetAfter - time.Minute))
			Expect(k8sClient.Status().Update(ctx, &ready)).To(Succeed())
			reconcileOnce()
			Expect(recreates()).To(BeNil(), "a group that served for the reset window starts over")

			// The next failure starts a fresh count.
			failMember("mn-ring-mn-1")
			reconcileOnce()
			Expect(recreates()).NotTo(BeNil())
			Expect(recreates().Count).To(Equal(int32(1)))
		})
	})

	It("clears the staging deadline when the group is scaled to zero", func() {
		// The suspended path deletes the group too: a later recreate must not
		// inherit the old group's staging deadline.
		reconcileOnce()
		Expect(memberPods()).To(HaveLen(2))
		stagedNoRuntime("mn-ring-mn-0")
		stagedNoRuntime("mn-ring-mn-1")

		reconcileOnce()
		var before inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &before)).To(Succeed())
		Expect(meta.FindStatusCondition(before.Status.Conditions, ConditionMultiNodeStaged)).NotTo(BeNil())

		zero := int32(0)
		before.Spec.Replicas = &zero
		Expect(k8sClient.Update(ctx, &before)).To(Succeed())

		reconcileOnce()
		finishTerminating()
		Eventually(memberPods).Should(BeEmpty())
		var after inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &after)).To(Succeed())
		Expect(meta.FindStatusCondition(after.Status.Conditions, ConditionMultiNodeFormed)).To(BeNil())
		Expect(meta.FindStatusCondition(after.Status.Conditions, ConditionMultiNodeStaged)).To(BeNil(),
			"scale to zero clears the staging deadline")
	})
})
