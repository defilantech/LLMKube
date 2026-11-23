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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// hub offline values. huggingface_hub reads HF_HUB_OFFLINE with
// TRANSFORMERS_OFFLINE as its fallback, and transformers 4.41 and older read
// only the latter, so the field sets both.
const (
	wantHubOffline      = "1"
	wantTransformersOff = "1"
)

// TestHubOfflineEnv drives constructDeploymentForCache and asserts on the serving
// container's env: spec.hubOffline adds both offline variables exactly once, a
// spec.env entry still lands last, the operator's downloader stays online, and
// the runtime mirror env is dropped because an offline runtime makes no request.
func TestHubOfflineEnv(t *testing.T) {
	repoModel := func(name, secret string) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "Qwen/Qwen3.8-27B-FP8"},
		}
		if secret != "" {
			m.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: secret}
		}
		return m
	}
	pvcModel := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "staged", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "pvc://models/Qwen3-8B-FP8", Format: "safetensors"},
	}
	isvc := func(runtime string, offline bool, mutate func(*inferencev1alpha1.InferenceServiceSpec)) *inferencev1alpha1.InferenceService {
		s := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: runtime, ModelRef: "qwen"},
		}
		if offline {
			s.Spec.HubOffline = ptr.To(true)
		}
		if mutate != nil {
			mutate(&s.Spec)
		}
		return s
	}
	tokenRef := func(secret, key string) *corev1.SecretKeySelector {
		return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key}
	}

	cases := []struct {
		name  string
		isvc  *inferencev1alpha1.InferenceService
		model *inferencev1alpha1.Model
		// wantOffline is the HF_HUB_OFFLINE entries the serving container gets.
		wantOffline []corev1.EnvVar
		// wantEndpoint says whether the mirror env is still added.
		wantEndpoint bool
	}{
		{
			name:  "set adds both variables once",
			isvc:  isvc(RuntimeVLLM, true, func(s *inferencev1alpha1.InferenceServiceSpec) { s.SkipModelInit = ptr.To(true) }),
			model: repoModel("qwen", "hf-mirror"),
			wantOffline: []corev1.EnvVar{
				{Name: "HF_HUB_OFFLINE", Value: wantHubOffline},
				{Name: "TRANSFORMERS_OFFLINE", Value: wantTransformersOff},
			},
		},
		{
			name:  "unset leaves the env alone",
			isvc:  isvc(RuntimeVLLM, false, func(s *inferencev1alpha1.InferenceServiceSpec) { s.SkipModelInit = ptr.To(true) }),
			model: repoModel("qwen", "hf-mirror"),
		},
		{
			// The reported combination: a runtime that must fetch the Model
			// itself gets the offline env and no way to reach a mirror.
			name:  "drops the mirror for a self-fetching runtime",
			isvc:  isvc("tgi", true, nil),
			model: repoModel("qwen", "hf-mirror"),
			wantOffline: []corev1.EnvVar{
				{Name: "HF_HUB_OFFLINE", Value: wantHubOffline},
				{Name: "TRANSFORMERS_OFFLINE", Value: wantTransformersOff},
			},
		},
		{
			name: "drops the mirror for a runtime token in the same Secret",
			isvc: isvc(RuntimeVLLM, true, func(s *inferencev1alpha1.InferenceServiceSpec) {
				s.SkipModelInit = ptr.To(true)
				s.VLLMConfig = &inferencev1alpha1.VLLMConfig{HFTokenSecretRef: tokenRef("hf-mirror", "token")}
			}),
			model: repoModel("qwen", "hf-mirror"),
			wantOffline: []corev1.EnvVar{
				{Name: "HF_HUB_OFFLINE", Value: wantHubOffline},
				{Name: "TRANSFORMERS_OFFLINE", Value: wantTransformersOff},
			},
		},
		{
			name:  "a staged service leaves the downloader online",
			isvc:  isvc(RuntimeVLLM, true, nil),
			model: repoModel("qwen", "hf-mirror"),
			wantOffline: []corev1.EnvVar{
				{Name: "HF_HUB_OFFLINE", Value: wantHubOffline},
				{Name: "TRANSFORMERS_OFFLINE", Value: wantTransformersOff},
			},
		},
		{
			name:  "a pre-staged pvc source still gets the field",
			isvc:  isvc(RuntimeVLLM, true, nil),
			model: pvcModel,
			wantOffline: []corev1.EnvVar{
				{Name: "HF_HUB_OFFLINE", Value: wantHubOffline},
				{Name: "TRANSFORMERS_OFFLINE", Value: wantTransformersOff},
			},
		},
		{
			name:  "llama.cpp still gets the field",
			isvc:  isvc("llamacpp", true, nil),
			model: repoModel("qwen", "hf-mirror"),
			wantOffline: []corev1.EnvVar{
				{Name: "HF_HUB_OFFLINE", Value: wantHubOffline},
				{Name: "TRANSFORMERS_OFFLINE", Value: wantTransformersOff},
			},
		},
		{
			name:  "an explicit false adds nothing",
			isvc:  isvc(RuntimeVLLM, false, func(s *inferencev1alpha1.InferenceServiceSpec) { s.HubOffline = ptr.To(false) }),
			model: repoModel("qwen", "hf-mirror"),
			// No offline env; the mirror still applies.
			wantEndpoint: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
			dep := r.constructDeploymentForCache(tc.isvc, tc.model, resolveEffectiveModelCache(tc.isvc, tc.model), nil, 1, testMirror, "")

			for _, want := range tc.wantOffline {
				got := servingEnv(dep, want.Name)
				if len(got) != 1 || got[0].Value != want.Value || got[0].ValueFrom != nil {
					t.Fatalf("%s = %+v, want one literal %q", want.Name, got, want.Value)
				}
			}
			if len(tc.wantOffline) == 0 {
				for _, name := range []string{"HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE"} {
					if got := servingEnv(dep, name); len(got) != 0 {
						t.Fatalf("%s = %+v, want none", name, got)
					}
				}
			}

			if ptr.Deref(tc.isvc.Spec.HubOffline, false) {
				// The mirror's own pair is what the field makes dead: the
				// optional HF_TOKEN that runtimeHFMirrorEnv adds from the
				// Model's Secret alongside HF_ENDPOINT. A backends' own
				// hfTokenSecretRef entry is the user's to keep.
				if got := servingEnv(dep, "HF_ENDPOINT"); len(got) != 0 {
					t.Fatalf("HF_ENDPOINT = %+v, want none under hubOffline", got)
				}
				for _, e := range servingEnv(dep, "HF_TOKEN") {
					ref := e.ValueFrom
					if ref == nil || ref.SecretKeyRef == nil {
						continue
					}
					if ptr.Deref(ref.SecretKeyRef.Optional, false) {
						t.Fatalf("HF_TOKEN = %+v, want the mirror's optional token gone under hubOffline", e)
					}
				}
			}

			// The operator stages the Model through its own init containers,
			// which must keep their Hub access under the field.
			for _, c := range dep.Spec.Template.Spec.InitContainers {
				if envValue(c.Env, "HF_HUB_OFFLINE") != "" || envValue(c.Env, "TRANSFORMERS_OFFLINE") != "" {
					t.Fatalf("init container %q got the offline env; the operator's staging path must stay online", c.Name)
				}
			}
		})
	}
}

// The user's spec.env still wins: the operator's entries are appended before
// it, and the last definition of a name is the one the container gets.
func TestHubOfflineEnvUserEnvWins(t *testing.T) {
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "Qwen/Qwen3.8-27B-FP8"},
	}
	svc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime: RuntimeVLLM, ModelRef: "qwen", HubOffline: ptr.To(true),
			Env: []corev1.EnvVar{
				{Name: "HF_HUB_OFFLINE", Value: "0"},
				{Name: "TRANSFORMERS_OFFLINE", Value: "0"},
			},
		},
	}
	dep := r.constructDeploymentForCache(svc, model, resolveEffectiveModelCache(svc, model), nil, 1, testMirror, "")
	for _, name := range []string{"HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE"} {
		entries := servingEnv(dep, name)
		if len(entries) != 2 || entries[0].Value != "1" || entries[len(entries)-1].Value != "0" {
			t.Fatalf("%s entries = %+v, want the operator's first and the user's last", name, entries)
		}
	}
}

// The warning must be visible only where a Hub lookup actually exists: the
// operator stages nothing and the source is a Hub repo the runtime fetches.
var _ = Describe("runtime hub offline reconcile", func() {
	const namespace = "default"
	ctx := context.Background()

	var recorder *events.FakeRecorder
	var modelName, isvcName, secretName string

	drainEvents := func() []string {
		var out []string
		for {
			select {
			case e := <-recorder.Events:
				out = append(out, e)
			default:
				return out
			}
		}
	}

	BeforeEach(func() {
		recorder = events.NewFakeRecorder(20)
		suffix := rand.String(5)
		modelName, isvcName, secretName = "offline-model-"+suffix, "offline-isvc-"+suffix, "hf-mirror-"+suffix
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
			StringData: map[string]string{"HF_ENDPOINT": testMirror, "HF_TOKEN": "t"},
		})).To(Succeed())
	})

	AfterEach(func() {
		for _, obj := range []client.Object{
			&inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace}},
			&inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace}},
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace}},
		} {
			if err := k8sClient.Delete(ctx, obj); err != nil {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		}
	})

	createModel := func(source string) {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
			Spec: inferencev1alpha1.ModelSpec{
				Source:          source,
				SourceSecretRef: &corev1.LocalObjectReference{Name: secretName},
				Hardware:        &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
	}

	reconcileService := func(spec inferencev1alpha1.InferenceServiceSpec) []string {
		spec.ModelRef = modelName
		spec.Replicas = ptr.To(int32(1))
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		r := &InferenceServiceReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder,
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: isvcName, Namespace: namespace}})
		Expect(err).NotTo(HaveOccurred())
		return drainEvents()
	}

	It("warns when TGI would fetch a repo id under the field", func() {
		createModel("Qwen/Qwen3.8-27B-FP8")
		got := reconcileService(inferencev1alpha1.InferenceServiceSpec{Runtime: "tgi", HubOffline: ptr.To(true)})
		Expect(got).To(ContainElement(SatisfyAll(
			ContainSubstring(ReasonHubOfflineRuntimeDownload),
			ContainSubstring("tgi"),
			ContainSubstring(modelName),
		)))
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, dep)).To(Succeed())
		Expect(servingEnv(dep, "HF_HUB_OFFLINE")).To(ConsistOf(corev1.EnvVar{Name: "HF_HUB_OFFLINE", Value: "1"}))
	})

	It("does not warn for a staged vLLM service", func() {
		createModel("Qwen/Qwen3.8-27B-FP8")
		got := reconcileService(inferencev1alpha1.InferenceServiceSpec{Runtime: RuntimeVLLM, HubOffline: ptr.To(true)})
		Expect(got).NotTo(ContainElement(ContainSubstring(ReasonHubOfflineRuntimeDownload)))
	})

	It("does not warn for a pre-staged pvc source", func() {
		createModel("pvc://models/Qwen3-8B-FP8")
		got := reconcileService(inferencev1alpha1.InferenceServiceSpec{Runtime: "tgi", HubOffline: ptr.To(true)})
		Expect(got).NotTo(ContainElement(ContainSubstring(ReasonHubOfflineRuntimeDownload)))
	})

	It("does not warn when the field is unset", func() {
		createModel("Qwen/Qwen3.8-27B-FP8")
		got := reconcileService(inferencev1alpha1.InferenceServiceSpec{Runtime: "tgi", SkipModelInit: ptr.To(true)})
		Expect(got).NotTo(ContainElement(ContainSubstring(ReasonHubOfflineRuntimeDownload)))
	})

	It("does not emit the mirror conflict under the field", func() {
		createModel("Qwen/Qwen3.8-27B-FP8")
		got := reconcileService(inferencev1alpha1.InferenceServiceSpec{
			Runtime: RuntimeVLLM, HubOffline: ptr.To(true), SkipModelInit: ptr.To(true),
			VLLMConfig: &inferencev1alpha1.VLLMConfig{HFTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "hf-public"}, Key: "token",
			}},
		})
		Expect(got).NotTo(ContainElement(ContainSubstring(ReasonHFMirrorNotApplied)))
	})
})

// Every member of a multiNode group gets the offline env: memberPodFromTemplate
// specialises each rank's pod from the service's template, and must keep it.
func TestHubOfflineEnvReachesMultiNodeMembers(t *testing.T) {
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime: RuntimeVLLM, ModelRef: "qwen", HubOffline: ptr.To(true),
			MultiNode: &inferencev1alpha1.MultiNodeSpec{Members: []inferencev1alpha1.MultiNodeMember{{Node: "n0"}, {Node: "n1"}}},
		},
	}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "Qwen/Qwen3.8-27B-FP8"},
	}
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	tmpl := r.constructDeploymentForCache(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template
	mnb := resolveBackend(isvc).(MultiNodeArgsBuilder)
	for rank, member := range isvc.Spec.MultiNode.Members {
		pod := memberPodFromTemplate(tmpl, isvc, member, int32(rank), mnb, "") //nolint:gosec // G115: two members
		env := pod.Spec.Containers[0].Env
		for _, name := range []string{"HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE"} {
			if got := envValue(env, name); got != "1" {
				t.Errorf("rank %d: %s = %q, want \"1\"", rank, name, got)
			}
		}
	}
}
