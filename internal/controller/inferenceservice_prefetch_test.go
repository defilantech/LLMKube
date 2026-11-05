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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// pendingPrefetchPod is the pod a prefetch Job's controller would create,
// scheduled but not started, with the Job's own container names, volumes and
// mounts so a builder change cannot leave the fixture describing another pod.
// NodeName is set here: a Pod's nodeName is immutable after create.
func pendingPrefetchPod(model *inferencev1alpha1.Model, tag string) *corev1.Pod {
	job := &batchv1.Job{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: prefetchJobName(model), Namespace: model.Namespace}, job)).To(Succeed())
	spec := job.Spec.Template.Spec
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prefetchJobName(model) + "-" + tag,
			Namespace: model.Namespace,
			Labels:    map[string]string{"batch.kubernetes.io/job-name": job.Name},
		},
		Spec: corev1.PodSpec{
			NodeName:       "worker-1",
			InitContainers: append([]corev1.Container(nil), spec.InitContainers...),
			Containers:     append([]corev1.Container(nil), spec.Containers...),
			Volumes:        append([]corev1.Volume(nil), spec.Volumes...),
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}},
		},
	}
}

// terminatedPrefetchInitStatuses reports the pod's first n init containers as
// exited 0, in spec order. The slice it returns is fresh: append to it rather
// than mutating it, or the appended status lands in a discarded copy.
func terminatedPrefetchInitStatuses(pod *corev1.Pod, n int) []corev1.ContainerStatus {
	out := make([]corev1.ContainerStatus, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, corev1.ContainerStatus{
			Name:  pod.Spec.InitContainers[i].Name,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		})
	}
	return out
}

// waitingPrefetchInitStatuses reports every init container waiting on reason.
func waitingPrefetchInitStatuses(pod *corev1.Pod, reason string) []corev1.ContainerStatus {
	out := make([]corev1.ContainerStatus, 0, len(pod.Spec.InitContainers))
	for _, c := range pod.Spec.InitContainers {
		out = append(out, corev1.ContainerStatus{
			Name:  c.Name,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		})
	}
	return out
}

// waitingPrefetchMainStatuses reports the pod's containers waiting on reason.
func waitingPrefetchMainStatuses(pod *corev1.Pod, reason string) []corev1.ContainerStatus {
	out := make([]corev1.ContainerStatus, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		out = append(out, corev1.ContainerStatus{
			Name:  c.Name,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		})
	}
	return out
}

// assertNotStalled checks that a pass left the Model downloading its entry and
// holds nothing but a blocked verdict off Progressing.
func assertNotStalled(model *inferencev1alpha1.Model, result reconcile.Result) {
	Expect(model.Status.Prefetch).NotTo(BeEmpty())
	Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
	Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("downloading into"))
	Expect(model.Status.Prefetch[0].Message).NotTo(ContainSubstring("has been Pending"))
	Expect(model.Status.Prefetch[0].Message).NotTo(ContainSubstring("PodInitializing"))
	progressing := meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing)
	Expect(progressing).NotTo(BeNil())
	Expect(progressing.Reason).NotTo(Equal(ReasonPrefetchStalled))
	Expect(result.RequeueAfter).To(Equal(prefetchPollInterval))
}

// prefetchPodStatus writes the pod's status and reads it back, so a spec fails
// on what the cluster stored rather than on what it meant to send. The caller
// fills pod.Status first; this reads the live object, applies that status and
// keeps the two apart so a lost condition or container status fails loudly.
func prefetchPodStatus(pod *corev1.Pod, phase corev1.PodPhase, initStatuses, mainStatuses []corev1.ContainerStatus) {
	ctx := context.Background()
	key := types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}
	Expect(k8sClient.Get(ctx, key, pod)).To(Succeed())
	pod.Status.Phase = phase
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
	pod.Status.InitContainerStatuses = initStatuses
	pod.Status.ContainerStatuses = mainStatuses
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

	Expect(k8sClient.Get(ctx, key, pod)).To(Succeed())
	Expect(pod.Status.Phase).To(Equal(phase))
	Expect(pod.Status.Conditions).To(HaveLen(1))
	Expect(pod.Status.Conditions[0].Type).To(Equal(corev1.PodScheduled))
	Expect(pod.Status.Conditions[0].Status).To(Equal(corev1.ConditionTrue))
	Expect(pod.Status.InitContainerStatuses).To(HaveLen(len(initStatuses)))
	Expect(pod.Status.ContainerStatuses).To(HaveLen(len(mainStatuses)))
	for i, want := range initStatuses {
		Expect(pod.Status.InitContainerStatuses[i].Name).To(Equal(want.Name))
	}
}

// createCachePVC makes a user-owned cache claim a prefetch can target, and
// removes it with the spec.
func createCachePVC(ctx context.Context, ns, name string) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, pvc))).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(ctx, pvc) })
}

// markJobComplete marks a Job Complete. k8s >=1.31 validates the Job status
// subresource: a finished Job needs startTime/completionTime, and
// Complete=True requires SuccessCriteriaMet=True first.
func markJobComplete(ctx context.Context, key types.NamespacedName) {
	job := &batchv1.Job{}
	Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	job.Status.Succeeded = 1
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}

// markJobFailed marks a Job Failed. Failed=True requires FailureTarget=True
// and a startTime under the same validation.
func markJobFailed(ctx context.Context, key types.NamespacedName) {
	job := &batchv1.Job{}
	Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "PodFailurePolicy", Message: "test"},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "PodFailurePolicy", Message: "test"},
	}
	job.Status.Failed = 3
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}

// finishJobDeletion stands in for the garbage collector envtest lacks. Every
// prefetch stager is deleted in the foreground (deletePrefetchJob), so it
// keeps the foregroundDeletion finalizer until its pods are gone, and no pods
// run here. A Job that is not being deleted is left alone.
func finishJobDeletion(ctx context.Context, key types.NamespacedName) {
	job := &batchv1.Job{}
	if err := k8sClient.Get(ctx, key, job); err != nil || job.DeletionTimestamp == nil {
		return
	}
	job.Finalizers = nil
	Expect(client.IgnoreNotFound(k8sClient.Update(ctx, job))).To(Succeed())
}

// collectPrefetchJobs stands in for the garbage collector after a spec: the
// Models it created are deleted, but envtest never removes the prefetch Jobs
// they own, and a Job left running holds every later prefetch of the same
// cache directory and claim (#2250). It removes every prefetch Job and pod in
// ns, finalizers included.
func collectPrefetchJobs(ctx context.Context, ns string) {
	selector := client.MatchingLabels{"app.kubernetes.io/component": "model-prefetch"}
	jobs := &batchv1.JobList{}
	Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns), selector)).To(Succeed())
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if len(job.Finalizers) > 0 {
			job.Finalizers = nil
			Expect(client.IgnoreNotFound(k8sClient.Update(ctx, job))).To(Succeed())
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)))).To(Succeed())
	}
	pods := &corev1.PodList{}
	Expect(k8sClient.List(ctx, pods, client.InNamespace(ns), selector)).To(Succeed())
	for i := range pods.Items {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &pods.Items[i], client.GracePeriodSeconds(0)))).To(Succeed())
	}
}

// cacheClaimOf returns the PersistentVolumeClaim the pod's model-cache volume
// mounts, or "".
func cacheClaimOf(spec *corev1.PodSpec) string {
	for _, v := range spec.Volumes {
		if v.Name == "model-cache" && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

func initContainerNames(spec *corev1.PodSpec) []string {
	names := make([]string, 0, len(spec.InitContainers))
	for _, c := range spec.InitContainers {
		names = append(names, c.Name)
	}
	return names
}

var _ = Describe("InferenceService modelCache.prefetch (#2136)", func() {
	ctx := context.Background()
	const ns = "prefetch-isvc-test"

	modelReconciler := func() *ModelReconciler {
		return &ModelReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
			DefaultFSGroup:       102,
			ModelCacheSize:       "10Gi",
			ModelCacheAccessMode: "ReadWriteOnce",
			ModelCacheMode:       ModelCacheModeShared,
			ModelCachePath:       "/models",
		}
	}
	isvcReconciler := func() *InferenceServiceReconciler {
		return &InferenceServiceReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			Recorder:             events.NewFakeRecorder(50),
			ModelCachePath:       "/models",
			ModelCacheMode:       ModelCacheModeShared,
			ModelCacheSize:       "10Gi",
			ModelCacheAccessMode: "ReadWriteOnce",
			InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
			DefaultFSGroup:       102,
		}
	}

	// readyModel creates a Model and writes the Ready status the Model
	// controller would, so the service controller proceeds past its gate.
	readyModel := func(name, source string, mutate func(*inferencev1alpha1.Model)) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       inferencev1alpha1.ModelSpec{Source: source, Format: "gguf"},
		}
		if mutate != nil {
			mutate(m)
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, m) })
		key := types.NamespacedName{Name: name, Namespace: ns}
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		m.Status.Phase = PhaseReady
		if !isPVCSource(source) {
			m.Status.CacheKey = computeCacheKey(source)
		}
		setCondition(m, ConditionAvailable, metav1.ConditionTrue, "Test", "ready")
		Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		return m
	}

	newService := func(name, modelName string, cache *inferencev1alpha1.ModelCacheSpec) *inferencev1alpha1.InferenceService {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: modelName, ModelCache: cache},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, isvc)
			_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
			_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		})
		return isvc
	}

	reconcileService := func(r *InferenceServiceReconciler, name string) *inferencev1alpha1.InferenceService {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		got := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got)).To(Succeed())
		return got
	}

	// prefetchModel runs the Model controller's prefetch path on a fresh read.
	prefetchModel := func(r *ModelReconciler, name string) (*inferencev1alpha1.Model, bool, time.Duration) {
		m := &inferencev1alpha1.Model{}
		key := types.NamespacedName{Name: name, Namespace: ns}
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		handled, result, err := r.reconcilePrefetch(ctx, m)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		return m, handled, result.RequeueAfter
	}

	// prefetchUntilRunning drives the Model controller through a Job
	// replacement (delete, then create) until the claim's entry runs on a
	// new fingerprint.
	prefetchUntilRunning := func(r *ModelReconciler, name, claim, previousFingerprint string) {
		Eventually(func() bool {
			m, _, _ := prefetchModel(r, name)
			e := findPrefetchStatus(m, claim)
			if e != nil && e.JobName != "" {
				finishJobDeletion(ctx, types.NamespacedName{Name: e.JobName, Namespace: ns})
			}
			return e != nil && e.Phase == inferencev1alpha1.PrefetchPhaseRunning && e.Fingerprint != previousFingerprint
		}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())
	}

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, nsObj))).To(Succeed())
	})
	AfterEach(func() { collectPrefetchJobs(ctx, ns) })

	// The prefetch Job writes the cache as the operator's default fsGroup, and a
	// prefetched pod mounts the claim read-only, so the kubelet never applies the
	// service's own fsGroup to it. A service that sets its own podSecurityContext
	// must still be able to enter the cache root (#2450).
	It("runs a pod with its own podSecurityContext in the group the prefetch Job writes the cache as", func() {
		createCachePVC(ctx, ns, "own-uid-cache")
		model := readyModel("own-uid-model", "https://example.com/models/own.gguf", nil)
		isvc := newService("own-uid", model.Name, &inferencev1alpha1.ModelCacheSpec{ClaimName: "own-uid-cache", Prefetch: true})
		uid := int64(2000)
		isvc.Spec.PodSecurityContext = &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid}
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
		mr, ir := modelReconciler(), isvcReconciler()
		key := types.NamespacedName{Name: isvc.Name, Namespace: ns}

		prefetchModel(mr, model.Name)
		markJobComplete(ctx, types.NamespacedName{Name: prefetchTargetJobName(model, "own-uid-cache"), Namespace: ns})
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "own-uid-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
		reconcileService(ir, isvc.Name)

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(initContainerNames(&dep.Spec.Template.Spec)).To(Equal([]string{"model-verify"}))
		psc := dep.Spec.Template.Spec.SecurityContext
		Expect(psc).NotTo(BeNil())
		Expect(psc.SupplementalGroups).To(ConsistOf(int64(102)), "the group the prefetch Job writes the cache as")
		Expect(psc.FSGroup).To(HaveValue(Equal(uid)))
		Expect(psc.RunAsUser).To(HaveValue(Equal(uid)))

		stored := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(stored.Spec.PodSecurityContext.SupplementalGroups).To(BeEmpty(), "the service's spec is not rewritten")
	})

	It("holds the first rollout until the prefetch into its claim succeeds, then mounts the cache read-only with no downloader, and holds later rollouts the same way", func() {
		createCachePVC(ctx, ns, "svc-cache")
		model := readyModel("gated-model", "https://example.com/models/llama.gguf", func(m *inferencev1alpha1.Model) {
			m.Spec.PrefetchNodeSelector = map[string]string{"example.com/pool": "downloads"}
		})
		isvc := newService("gated", model.Name, &inferencev1alpha1.ModelCacheSpec{ClaimName: "svc-cache", Prefetch: true})
		mr, ir := modelReconciler(), isvcReconciler()
		depKey := types.NamespacedName{Name: isvc.Name, Namespace: ns}
		jobKey := types.NamespacedName{Name: prefetchTargetJobName(model, "svc-cache"), Namespace: ns}

		By("the Model controller starting a prefetch for the service's claim although the Model does not set spec.prefetch")
		model, handled, requeue := prefetchModel(mr, model.Name)
		Expect(handled).To(BeFalse(), "a service-requested prefetch does not hold the Model's Ready")
		Expect(requeue).To(Equal(prefetchPollInterval))
		Expect(model.Status.Phase).To(Equal(PhaseReady))
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
		Expect(cacheClaimOf(&job.Spec.Template.Spec)).To(Equal("svc-cache"))
		Expect(job.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("example.com/pool", "downloads"))
		entry := findPrefetchStatus(model, "svc-cache")
		Expect(entry).NotTo(BeNil())
		Expect(entry.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))

		By("the service creating no Deployment while it runs")
		got := reconcileService(ir, isvc.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, depKey, &appsv1.Deployment{}))).To(BeTrue())
		Expect(got.Status.Phase).To(Equal("Pending"))
		Expect(got.Status.SchedulingStatus).To(Equal(ReasonWaitingForPrefetch))
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonWaitingForPrefetch))
		Expect(cond.Message).To(ContainSubstring("svc-cache"))

		By("rolling out onto the prefetched cache once the Job succeeds")
		markJobComplete(ctx, jobKey)
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "svc-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
		got = reconcileService(ir, isvc.Name)
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		pod := &dep.Spec.Template.Spec
		Expect(initContainerNames(pod)).To(Equal([]string{"model-verify"}), "no cache-prep and no downloader on a prefetched cache")
		Expect(envValue(pod.InitContainers[0].Env, "MODEL_FILES")).To(Equal(canonicalModelBasename(model)))
		Expect(envValue(pod.InitContainers[0].Env, "CACHE_DIR")).To(Equal("/models/" + model.Status.CacheKey))
		Expect(cacheClaimOf(pod)).To(Equal("svc-cache"))
		for _, v := range pod.Volumes {
			if v.Name == "model-cache" {
				Expect(v.PersistentVolumeClaim.ReadOnly).To(BeTrue(), "the serving pod only consumes the cache")
			}
		}
		Expect(pod.Containers[0].VolumeMounts).To(ContainElement(corev1.VolumeMount{Name: "model-cache", MountPath: "/models", ReadOnly: true}))
		cond = meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(ReasonPrefetchComplete))
		Expect(got.Status.SchedulingStatus).To(BeEmpty(), "the hold's diagnosis is cleared when the gate opens")
		Expect(envValue(pod.Containers[0].Env, "HF_HUB_OFFLINE")).To(BeEmpty(), "prefetch stamps no Hub-offline env; that stays the user's spec.env or spec.hubOffline")
		Expect(findPrefetchStatus(model, "svc-cache").Files).To(Equal([]string{canonicalModelBasename(model)}), "the Job records what it wrote")
		hash1 := dep.Annotations[AnnotationDesiredTemplateHash]
		Expect(hash1).NotTo(BeEmpty())
		key1 := model.Status.CacheKey

		By("serving the name the Job wrote even after GGUF metadata would derive another")
		model.Status.GGUF = &inferencev1alpha1.GGUFMetadata{ModelName: "Llama Renamed By Metadata"}
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(canonicalModelBasename(model)).NotTo(Equal(findPrefetchStatus(model, "svc-cache").Files[0]), "the test needs the derived name to differ")
		reconcileService(ir, isvc.Name)
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		Expect(envValue(dep.Spec.Template.Spec.InitContainers[0].Env, "MODEL_FILES")).To(Equal(findPrefetchStatus(model, "svc-cache").Files[0]))
		Expect(dep.Annotations[AnnotationDesiredTemplateHash]).To(Equal(hash1), "no rollout for a name the Job never used")

		By("keeping the live Deployment on its template while a new prefetch for a changed source runs")
		fingerprint1 := findPrefetchStatus(model, "svc-cache").Fingerprint
		model.Spec.Source = "https://example.com/models/llama-v2.gguf"
		Expect(k8sClient.Update(ctx, model)).To(Succeed())
		prefetchUntilRunning(mr, model.Name, "svc-cache", fingerprint1)
		got = reconcileService(ir, isvc.Name)
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		Expect(dep.Annotations[AnnotationDesiredTemplateHash]).To(Equal(hash1), "the old template keeps serving")
		Expect(envValue(dep.Spec.Template.Spec.InitContainers[0].Env, "CACHE_DIR")).To(Equal("/models/" + key1))
		cond = meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonWaitingForPrefetch))

		By("rolling once the new prefetch completes")
		markJobComplete(ctx, jobKey)
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "svc-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
		Expect(model.Status.CacheKey).NotTo(Equal(key1))
		got = reconcileService(ir, isvc.Name)
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		Expect(dep.Annotations[AnnotationDesiredTemplateHash]).NotTo(Equal(hash1))
		Expect(envValue(dep.Spec.Template.Spec.InitContainers[0].Env, "CACHE_DIR")).To(Equal("/models/" + model.Status.CacheKey))
		Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched).Status).To(Equal(metav1.ConditionTrue))
		hash2 := dep.Annotations[AnnotationDesiredTemplateHash]

		By("leaving the workload as it is when a later prefetch fails")
		fingerprint2 := findPrefetchStatus(model, "svc-cache").Fingerprint
		model.Spec.Source = "https://example.com/models/llama-v3.gguf"
		Expect(k8sClient.Update(ctx, model)).To(Succeed())
		prefetchUntilRunning(mr, model.Name, "svc-cache", fingerprint2)
		markJobFailed(ctx, jobKey)
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "svc-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseFailed))
		got = reconcileService(ir, isvc.Name)
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		Expect(dep.Annotations[AnnotationDesiredTemplateHash]).To(Equal(hash2), "a failed prefetch never rolls pods onto an unfilled cache")
		cond = meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonPrefetchFailed))
		Expect(cond.Message).To(ContainSubstring("failed"))
	})

	// A cache clear or a recreated claim empties what the status still
	// records as filled. The pods say so; the service turns that into a
	// re-stage by deleting the Job, once per episode.
	It("deletes the prefetch Job once when its pods report the staged files missing", func() {
		createCachePVC(ctx, ns, "wiped-cache")
		model := readyModel("wiped-model", "https://example.com/models/llama.gguf", nil)
		isvc := newService("wiped", model.Name, &inferencev1alpha1.ModelCacheSpec{ClaimName: "wiped-cache", Prefetch: true})
		mr := modelReconciler()
		ir := isvcReconciler()
		recorder := events.NewFakeRecorder(50)
		ir.Recorder = recorder
		jobKey := types.NamespacedName{Name: prefetchTargetJobName(model, "wiped-cache"), Namespace: ns}

		prefetchModel(mr, model.Name)
		markJobComplete(ctx, jobKey)
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "wiped-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
		reconcileService(ir, isvc.Name)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, &appsv1.Deployment{})).To(Succeed())

		By("a serving pod failing model-verify on the emptied claim")
		// Status times have second resolution; the failure must be seen
		// after the staging completed.
		time.Sleep(1100 * time.Millisecond)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: isvc.Name + "-7f9c4", Namespace: ns,
				Labels: map[string]string{"app": isvc.Name, "inference.llmkube.dev/service": isvc.Name}},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "model-verify", Image: "busybox"}},
				Containers:     []corev1.Container{{Name: "x", Image: "busybox"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
		pod.Status.Phase = corev1.PodPending
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name: "model-verify",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1,
				Message:  "ERROR: PrefetchedFilesMissing - /models/abc/llama.gguf is not on the cache claim.",
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		got := reconcileService(ir, isvc.Name)
		transfer := meta.FindStatusCondition(got.Status.Conditions, ConditionModelTransferHealthy)
		Expect(transfer).NotTo(BeNil())
		Expect(transfer.Reason).To(Equal(ReasonPrefetchedFilesMissing))
		finishJobDeletion(ctx, jobKey)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))).To(BeTrue(),
			"the prefetch Job is deleted so the Model stages the claim again")
		Expect(drainEvents(recorder)).To(ContainElement(HavePrefix("Warning " + ReasonPrefetchRestaged)))

		By("the Model re-staging the claim with its Ready and the entry's Succeeded untouched")
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(model.Status.Phase).To(Equal(PhaseReady))
		entry := findPrefetchStatus(model, "wiped-cache")
		Expect(entry.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded), "consumers are not held for a re-stage")
		Expect(entry.Message).To(ContainSubstring("re-verifying"))
		Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed())

		By("leaving a re-stage in flight alone")
		reconcileService(ir, isvc.Name)
		Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed(), "the running re-stage is the remedy, not a stale Job")

		By("leaving the completed re-stage alone for the same failure episode")
		markJobComplete(ctx, jobKey)
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "wiped-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
		Expect(findPrefetchStatus(model, "wiped-cache").Message).To(BeEmpty())
		reconcileService(ir, isvc.Name)
		Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed(),
			"a failure observed before the re-stage completed does not trigger another")
	})

	// prefetch: true means "serve only from a prefetched cache". A request the
	// pods could not consume is held and said so, never rolled out with a
	// downloader on the serving node.
	It("holds a request the pods could not consume as PrefetchUnsupported", func() {
		createCachePVC(ctx, ns, "unsup-cache")
		model := readyModel("unsup-model", "https://example.com/models/llama.gguf", nil)
		isvc := newService("unsup", model.Name, &inferencev1alpha1.ModelCacheSpec{ClaimName: "unsup-cache", Prefetch: true})
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, isvc)).To(Succeed())
		isvc.Spec.Runtime = "tgi"
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

		got := reconcileService(isvcReconciler(), isvc.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue(),
			"no pods for a runtime that would download on the serving node")
		Expect(got.Status.Phase).To(Equal(PhaseFailed))
		Expect(got.Status.SchedulingStatus).To(Equal(ReasonPrefetchUnsupported))
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(ReasonPrefetchUnsupported))
		Expect(cond.Message).To(ContainSubstring("tgi"))
	})

	It("reports PrefetchFailed on a first rollout and creates nothing", func() {
		createCachePVC(ctx, ns, "fail-cache")
		model := readyModel("fail-first-model", "https://example.com/models/llama.gguf", nil)
		isvc := newService("fail-first", model.Name, &inferencev1alpha1.ModelCacheSpec{ClaimName: "fail-cache", Prefetch: true})
		mr, ir := modelReconciler(), isvcReconciler()
		jobKey := types.NamespacedName{Name: prefetchTargetJobName(model, "fail-cache"), Namespace: ns}

		prefetchModel(mr, model.Name)
		markJobFailed(ctx, jobKey)
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "fail-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseFailed))

		got := reconcileService(ir, isvc.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())
		Expect(got.Status.Phase).To(Equal(PhaseFailed))
		Expect(got.Status.SchedulingStatus).To(Equal(ReasonPrefetchFailed))
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(ReasonPrefetchFailed))
	})

	It("inherits the Model's claim on a service without its own, in memory only, and not the Model's prefetch request", func() {
		createCachePVC(ctx, ns, "model-cache")
		model := readyModel("inherit-model", "https://example.com/models/llama.gguf", func(m *inferencev1alpha1.Model) {
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "model-cache", Prefetch: true}
		})
		isvc := newService("inherit", model.Name, nil)

		got := reconcileService(isvcReconciler(), isvc.Name)
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, dep)).To(Succeed(),
			"the Model's modelCache.prefetch is spec.prefetch, which holds no service")
		Expect(cacheClaimOf(&dep.Spec.Template.Spec)).To(Equal("model-cache"), "the claim is declared once, on the Model")
		Expect(initContainerNames(&dep.Spec.Template.Spec)).To(ContainElement("model-downloader"), "the service did not ask for prefetch, so the pod downloads as before")
		Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)).To(BeNil())

		stored := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, stored)).To(Succeed())
		Expect(stored.Spec.ModelCache).To(BeNil(), "inheritance is never written back to the service's spec")
	})

	It("does not hold a service whose Model source is not prefetchable", func() {
		model := readyModel("pvc-model", "pvc://staged-models/llama.gguf", nil)
		isvc := newService("pvc-prefetch", model.Name, &inferencev1alpha1.ModelCacheSpec{Prefetch: true})

		got := reconcileService(isvcReconciler(), isvc.Name)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, &appsv1.Deployment{})).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonPrefetchNotApplicable))
	})

	It("holds a service as Pending, not Failed, while its Model has no cache key yet", func() {
		createCachePVC(ctx, ns, "nokey-cache")
		model := readyModel("nokey-model", "https://example.com/models/llama.gguf", nil)
		model.Status.CacheKey = ""
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		isvc := newService("nokey", model.Name, &inferencev1alpha1.ModelCacheSpec{ClaimName: "nokey-cache", Prefetch: true})

		got := reconcileService(isvcReconciler(), isvc.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())
		Expect(got.Status.Phase).To(Equal("Pending"), "the key arrives with the Model's next reconcile; nothing is terminal")
		Expect(got.Status.SchedulingStatus).To(Equal(ReasonWaitingForModel))
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonWaitingForModel))
	})

	It("rejects modelCache.prefetch with persistence Ephemeral at admission", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "ephemeral-prefetch", Namespace: ns},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:   "any",
				ModelCache: &inferencev1alpha1.ModelCacheSpec{Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral, Prefetch: true},
			},
		}
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("prefetch cannot be set when persistence is Ephemeral"))
	})
})

var _ = Describe("buildPrefetchedStorageConfig", func() {
	multiFileModel := func(generation int64) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default", Generation: generation},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "hf://Qwen/Qwen2.5-0.5B-Instruct",
				Format: "safetensors",
				Files:  []string{"config.json", "*.safetensors"},
			},
			Status: inferencev1alpha1.ModelStatus{
				Phase:       PhaseReady,
				StagedFiles: []string{"config.json", "a.safetensors", "b.safetensors"},
				Conditions:  []metav1.Condition{{Type: ConditionAvailable, Status: metav1.ConditionTrue, ObservedGeneration: 2, Reason: "x"}},
			},
		}
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "qwen", ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "svc-cache", Prefetch: true}},
	}

	It("mounts the claim read-only and verifies the staged file set instead of downloading", func() {
		model := multiFileModel(2)
		cfg := buildPrefetchedStorageConfig(model, isvc, resolveEffectiveModelCache(isvc, model), ModelCacheModeShared, "curl:8.18.0", nil)
		key := effectiveModelCacheKey(model)
		Expect(cfg.modelPath).To(Equal("/models/" + key + "/config.json"))
		Expect(cfg.stagedDir).To(Equal("/models/" + key))
		Expect(cfg.volumes).To(HaveLen(1))
		Expect(cfg.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("svc-cache"))
		Expect(cfg.volumes[0].PersistentVolumeClaim.ReadOnly).To(BeTrue())
		Expect(cfg.volumeMounts).To(ConsistOf(corev1.VolumeMount{Name: "model-cache", MountPath: "/models", ReadOnly: true}))
		Expect(cfg.initContainers).To(HaveLen(1))
		verify := cfg.initContainers[0]
		Expect(verify.Name).To(Equal("model-verify"))
		Expect(verify.Image).To(Equal("curl:8.18.0"))
		Expect(envValue(verify.Env, "MODEL_FILES")).To(Equal("config.json\na.safetensors\nb.safetensors"))
		Expect(envValue(verify.Env, "CACHE_DIR")).To(Equal("/models/" + key))
		Expect(strings.Join(verify.Command, " ")).To(ContainSubstring("PrefetchedFilesMissing"))
		Expect(strings.Join(verify.Command, " ")).NotTo(ContainSubstring("curl"), "a verify step downloads nothing")
		Expect(verify.VolumeMounts).To(ConsistOf(corev1.VolumeMount{Name: "model-cache", MountPath: "/models", ReadOnly: true}))
		Expect(verify.TerminationMessagePolicy).To(Equal(corev1.TerminationMessageFallbackToLogsOnError))
	})

	It("fails closed when the glob expansion is not current for the Model's generation", func() {
		cfg := buildPrefetchedStorageConfig(multiFileModel(3), isvc, resolveEffectiveModelCache(isvc, multiFileModel(3)), ModelCacheModeShared, "curl:8.18.0", nil)
		Expect(cfg.initContainers).To(HaveLen(1))
		Expect(strings.Join(cfg.initContainers[0].Command, " ")).To(ContainSubstring("StagedFilesNotCurrent"))
		Expect(cfg.volumes[0].PersistentVolumeClaim.ReadOnly).To(BeTrue())
	})

	It("verifies the names the entry recorded over names derived now", func() {
		multi := multiFileModel(2)
		cfg := buildPrefetchedStorageConfig(multi, isvc, resolveEffectiveModelCache(isvc, multi), ModelCacheModeShared, "curl:8.18.0", []string{"config.json", "a.safetensors"})
		key := effectiveModelCacheKey(multi)
		Expect(envValue(cfg.initContainers[0].Env, "MODEL_FILES")).To(Equal("config.json\na.safetensors"))
		Expect(cfg.modelPath).To(Equal("/models/" + key + "/config.json"))
		Expect(cfg.stagedDir).To(Equal("/models/" + key))

		single := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "llama", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/models/llama.gguf"},
			Status:     inferencev1alpha1.ModelStatus{CacheKey: "abc123", GGUF: &inferencev1alpha1.GGUFMetadata{ModelName: "Parsed After The Job"}},
		}
		cfg = buildPrefetchedStorageConfig(single, nil, resolveEffectiveModelCache(nil, single), ModelCacheModeShared, "curl:8.18.0", []string{"llama.gguf"})
		Expect(canonicalModelBasename(single)).NotTo(Equal("llama.gguf"))
		Expect(envValue(cfg.initContainers[0].Env, "MODEL_FILES")).To(Equal("llama.gguf"))
		Expect(cfg.modelPath).To(Equal("/models/abc123/llama.gguf"))
		Expect(cfg.stagedDir).To(BeEmpty())
	})

	It("hashes the primary file against a pinned spec.sha256", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "pinned", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/models/llama.gguf", SHA256: strings.Repeat("AB", 32)},
			Status:     inferencev1alpha1.ModelStatus{CacheKey: "abc123"},
		}
		cfg := buildPrefetchedStorageConfig(model, nil, resolveEffectiveModelCache(nil, model), ModelCacheModeShared, "curl:8.18.0", []string{"pinned.gguf"})
		verify := cfg.initContainers[0]
		Expect(envValue(verify.Env, "MODEL_SHA256")).To(Equal(strings.Repeat("ab", 32)))
		Expect(envValue(verify.Env, "MODEL_PATH")).To(Equal("/models/abc123/pinned.gguf"))
		cmd := strings.Join(verify.Command, " ")
		Expect(cmd).To(ContainSubstring("llmkube_sha256_hash"))
		Expect(cmd).To(ContainSubstring("spec.sha256"))

		model.Spec.SHA256 = ""
		cfg = buildPrefetchedStorageConfig(model, nil, resolveEffectiveModelCache(nil, model), ModelCacheModeShared, "curl:8.18.0", []string{"pinned.gguf"})
		Expect(envValue(cfg.initContainers[0].Env, "MODEL_SHA256")).To(BeEmpty())
		Expect(strings.Join(cfg.initContainers[0].Command, " ")).NotTo(ContainSubstring("llmkube_sha256_hash"))
	})

	It("verifies the canonical basename of a single-file model", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "llama", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/models/llama.gguf"},
			Status:     inferencev1alpha1.ModelStatus{CacheKey: "abc123"},
		}
		cfg := buildPrefetchedStorageConfig(model, nil, resolveEffectiveModelCache(nil, model), ModelCacheModeShared, "curl:8.18.0", nil)
		Expect(cfg.modelPath).To(Equal("/models/abc123/" + canonicalModelBasename(model)))
		Expect(cfg.stagedDir).To(BeEmpty())
		Expect(cfg.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(ModelCachePVCName))
		Expect(envValue(cfg.initContainers[0].Env, "MODEL_FILES")).To(Equal(canonicalModelBasename(model)))
	})
})

var _ = Describe("prefetchGate classification", func() {
	newModel := func(source string) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default", Generation: 1},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
			Status:     inferencev1alpha1.ModelStatus{Phase: PhaseReady},
		}
		if !isPVCSource(source) {
			m.Status.CacheKey = computeCacheKey(source)
		}
		return m
	}
	newService := func(mutate func(*inferencev1alpha1.InferenceService)) *inferencev1alpha1.InferenceService {
		svc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "m", ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "c", Prefetch: true}},
		}
		if mutate != nil {
			mutate(svc)
		}
		return svc
	}
	reconciler := func(cachePath string) *InferenceServiceReconciler {
		return &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ModelCachePath: cachePath, ModelCacheMode: ModelCacheModeShared}
	}
	https := "https://example.com/models/llama.gguf"

	// Every case where the pods would still download at serve time is refused
	// and held, so prefetch: true never rolls out a downloader quietly.
	DescribeTable("refuses what prefetch cannot cover and passes through what never downloads",
		func(cachePath, source string, mutate func(*inferencev1alpha1.InferenceService), wantApplies, wantUnsupported bool, wantReason string) {
			svc := newService(mutate)
			mdl := newModel(source)
			g := reconciler(cachePath).prefetchGate(svc, mdl, resolveEffectiveModelCache(svc, mdl))
			Expect(g.requested).To(BeTrue())
			Expect(g.applies).To(Equal(wantApplies))
			Expect(g.unsupported).To(Equal(wantUnsupported))
			Expect(g.reason).To(Equal(wantReason))
		},
		Entry("a runtime the operator does not stage", "/models", https, func(s *inferencev1alpha1.InferenceService) { s.Spec.Runtime = "tgi" }, true, true, ReasonPrefetchUnsupported),
		Entry("skipModelInit", "/models", https, func(s *inferencev1alpha1.InferenceService) {
			s.Spec.Runtime = "vllm"
			s.Spec.SkipModelInit = ptr.To(true)
		}, true, true, ReasonPrefetchUnsupported),
		Entry("a draft model nobody prefetches", "/models", https, func(s *inferencev1alpha1.InferenceService) {
			s.Spec.SpeculativeDecoding = &inferencev1alpha1.SpeculativeDecodingSpec{Type: "draft-simple", DraftModelRef: "draft"}
		}, true, true, ReasonPrefetchUnsupported),
		Entry("caching disabled on the operator", "", https, nil, true, true, ReasonPrefetchUnsupported),
		Entry("a source the downloader does not stage ahead of time", "/models", "s3://bucket/llama.gguf", nil, true, true, ReasonPrefetchUnsupported),
		Entry("a pre-staged pvc:// source", "/models", "pvc://staged/llama.gguf", nil, false, false, ReasonPrefetchNotApplicable),
		Entry("a stageable source on a staging runtime", "/models", https, nil, true, false, ReasonWaitingForPrefetch),
	)

	It("inherits the Model's claim for a service without its own, not the Model's prefetch request", func() {
		model := newModel(https)
		model.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "model-cache", Prefetch: true}
		svc := newService(func(s *inferencev1alpha1.InferenceService) { s.Spec.ModelCache = nil })
		g := reconciler("/models").prefetchGate(svc, model, resolveEffectiveModelCache(svc, model))
		Expect(g.requested).To(BeFalse(), "modelCache.prefetch on the Model is spec.prefetch; a service asks with its own block")
		Expect(svc.Spec.ModelCache).To(BeNil(), "the gate reads an effective copy")
		Expect(resolveEffectiveModelCache(svc, model).claimName()).To(Equal("model-cache"))
	})

	It("refuses a multiNode member that mounts its own claim, which no prefetch stages", func() {
		svc := newService(func(s *inferencev1alpha1.InferenceService) {
			s.Spec.MultiNode = &inferencev1alpha1.MultiNodeSpec{Members: []inferencev1alpha1.MultiNodeMember{
				{Node: "node-a"},
				{Node: "node-b", ModelCache: &inferencev1alpha1.MultiNodeMemberCache{ClaimName: "node-b-cache"}},
			}}
		})
		mdl := newModel(https)
		g := reconciler("/models").prefetchGate(svc, mdl, resolveEffectiveModelCache(svc, mdl))
		Expect(g.unsupported).To(BeTrue(), "rank 1 would mount node-b-cache read-only and fail model-verify forever")
		Expect(g.message).To(ContainSubstring("node-b-cache"))
	})
})

var _ = Describe("warnPrefetchTargetMismatch", func() {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "prefetched", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/models/llama.gguf", Prefetch: true},
		Status: inferencev1alpha1.ModelStatus{
			Phase:    PhaseReady,
			CacheKey: "abc123",
			Prefetch: []inferencev1alpha1.PrefetchStatus{{ClaimName: ModelCachePVCName, Phase: inferencev1alpha1.PrefetchPhaseSucceeded}},
		},
	}
	service := func(cache *inferencev1alpha1.ModelCacheSpec) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, ModelCache: cache},
		}
	}
	newReconciler := func() (*InferenceServiceReconciler, *events.FakeRecorder) {
		recorder := events.NewFakeRecorder(10)
		return &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder,
			ModelCachePath: "/models", ModelCacheMode: ModelCacheModeShared}, recorder
	}

	It("warns a service that mounts a claim its prefetched Model was not staged into", func() {
		r, recorder := newReconciler()
		elsewhere := service(&inferencev1alpha1.ModelCacheSpec{ClaimName: "elsewhere"})
		r.warnPrefetchTargetMismatch(elsewhere, model, resolveEffectiveModelCache(elsewhere, model))
		Expect(drainEvents(recorder)).To(ContainElement(HavePrefix("Warning " + ReasonPrefetchTargetMismatch)))
	})

	It("stays quiet for a service on the prefetched claim, or one that asks for its own prefetch", func() {
		r, recorder := newReconciler()
		own := service(nil)
		r.warnPrefetchTargetMismatch(own, model, resolveEffectiveModelCache(own, model))
		asksOwn := service(&inferencev1alpha1.ModelCacheSpec{ClaimName: "elsewhere", Prefetch: true})
		r.warnPrefetchTargetMismatch(asksOwn, model, resolveEffectiveModelCache(asksOwn, model))
		Expect(drainEvents(recorder)).To(BeEmpty())
	})
})
