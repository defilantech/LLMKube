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

const testMirror = "https://mirror.corp.example/artifactory/api/huggingfaceml/hf"

// servingEnv returns the serving container's env entries for name, in order.
func servingEnv(dep *appsv1.Deployment, name string) []corev1.EnvVar {
	var out []corev1.EnvVar
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// TestRuntimeHFMirrorEnv drives constructDeploymentForCache and asserts on the serving
// container's env: the mirror reaches a runtime that downloads the Model
// itself, and never carries a token to a mirror its Secret does not name.
func TestRuntimeHFMirrorEnv(t *testing.T) {
	repoModel := func(secret string) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "Qwen/Qwen3.8-27B-FP8"},
		}
		if secret != "" {
			m.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: secret}
		}
		return m
	}
	tokenRef := func(secret, key string) *corev1.SecretKeySelector {
		return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key}
	}
	isvc := func(runtime string, skip bool, mutate func(*inferencev1alpha1.InferenceServiceSpec)) *inferencev1alpha1.InferenceService {
		s := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: runtime, ModelRef: "qwen"},
		}
		if skip {
			s.Spec.SkipModelInit = ptr.To(true)
		}
		if mutate != nil {
			mutate(&s.Spec)
		}
		return s
	}
	sameSecretToken := corev1.EnvVar{Name: "HF_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "hf-mirror"}, Key: "HF_TOKEN", Optional: ptr.To(true),
	}}}

	cases := []struct {
		name         string
		isvc         *inferencev1alpha1.InferenceService
		model        *inferencev1alpha1.Model
		hfEndpoint   string
		wantEndpoint bool
		wantTokens   []corev1.EnvVar
	}{
		{
			name:         "vLLM fetching a repo id gets the mirror and the same Secret's token",
			isvc:         isvc(RuntimeVLLM, true, nil),
			model:        repoModel("hf-mirror"),
			hfEndpoint:   testMirror,
			wantEndpoint: true,
			wantTokens:   []corev1.EnvVar{sameSecretToken},
		},
		{
			name:         "SGLang fetching a repo id gets the mirror",
			isvc:         isvc(RuntimeSGLANG, true, nil),
			model:        repoModel("hf-mirror"),
			hfEndpoint:   testMirror,
			wantEndpoint: true,
			wantTokens:   []corev1.EnvVar{sameSecretToken},
		},
		{
			// TGI stages nothing by default, so it always fetches the repo.
			name:         "TGI without skipModelInit still fetches the repo and gets the mirror",
			isvc:         isvc("tgi", false, nil),
			model:        repoModel("hf-mirror"),
			hfEndpoint:   testMirror,
			wantEndpoint: true,
			wantTokens:   []corev1.EnvVar{sameSecretToken},
		},
		{
			name: "a runtime token in the same Secret is kept and not duplicated",
			isvc: isvc(RuntimeVLLM, true, func(s *inferencev1alpha1.InferenceServiceSpec) {
				s.VLLMConfig = &inferencev1alpha1.VLLMConfig{HFTokenSecretRef: tokenRef("hf-mirror", "token")}
			}),
			model:        repoModel("hf-mirror"),
			hfEndpoint:   testMirror,
			wantEndpoint: true,
			wantTokens:   []corev1.EnvVar{{Name: "HF_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: tokenRef("hf-mirror", "token")}}},
		},
		{
			// The security case: that token's Secret never named the mirror.
			name: "a runtime token in another Secret keeps the mirror off",
			isvc: isvc(RuntimeVLLM, true, func(s *inferencev1alpha1.InferenceServiceSpec) {
				s.VLLMConfig = &inferencev1alpha1.VLLMConfig{HFTokenSecretRef: tokenRef("hf-public", "token")}
			}),
			model:        repoModel("hf-mirror"),
			hfEndpoint:   testMirror,
			wantEndpoint: false,
			wantTokens:   []corev1.EnvVar{{Name: "HF_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: tokenRef("hf-public", "token")}}},
		},
		{
			name:       "a staged repo source goes through the operator's downloader, not the runtime",
			isvc:       isvc(RuntimeVLLM, false, nil),
			model:      repoModel("hf-mirror"),
			hfEndpoint: testMirror,
		},
		{
			// Before this change the runtime got no token here; it still gets none.
			name:  "no HF_ENDPOINT leaves the runtime env as it was",
			isvc:  isvc(RuntimeVLLM, true, nil),
			model: repoModel("hf-mirror"),
		},
		{
			name: "a file URL source is not a repo the runtime fetches",
			isvc: isvc(RuntimeVLLM, true, nil),
			model: &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:          testMirror + "/unsloth/Qwen3-GGUF/resolve/main/q4.gguf",
					SourceSecretRef: &corev1.LocalObjectReference{Name: "hf-mirror"},
				},
			},
			hfEndpoint: testMirror,
		},
		{
			name:       "llama.cpp does not download with huggingface_hub",
			isvc:       isvc("llamacpp", true, nil),
			model:      repoModel("hf-mirror"),
			hfEndpoint: testMirror,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
			dep := r.deploymentFor(tc.isvc, tc.model, resolveEffectiveModelCache(tc.isvc, tc.model), nil, 1, tc.hfEndpoint, "")

			endpoints := servingEnv(dep, "HF_ENDPOINT")
			if tc.wantEndpoint {
				if len(endpoints) != 1 || endpoints[0].Value != testMirror || endpoints[0].ValueFrom != nil {
					t.Fatalf("HF_ENDPOINT = %+v, want one literal %q", endpoints, testMirror)
				}
			} else if len(endpoints) != 0 {
				t.Fatalf("HF_ENDPOINT = %+v, want none", endpoints)
			}

			tokens := servingEnv(dep, "HF_TOKEN")
			if len(tokens) != len(tc.wantTokens) {
				t.Fatalf("HF_TOKEN entries = %+v, want %+v", tokens, tc.wantTokens)
			}
			for i := range tokens {
				got, want := tokens[i].ValueFrom.SecretKeyRef, tc.wantTokens[i].ValueFrom.SecretKeyRef
				if got.Name != want.Name || got.Key != want.Key || ptr.Deref(got.Optional, false) != ptr.Deref(want.Optional, false) {
					t.Errorf("HF_TOKEN[%d] = %s/%s optional=%v, want %s/%s optional=%v", i,
						got.Name, got.Key, ptr.Deref(got.Optional, false), want.Name, want.Key, ptr.Deref(want.Optional, false))
				}
			}
		})
	}
}

// The user's spec.env still wins: the mirror entries are appended before it,
// and the last definition of a name is the one the container gets.
func TestRuntimeHFMirrorEnvUserEnvWins(t *testing.T) {
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          "Qwen/Qwen3.8-27B-FP8",
			SourceSecretRef: &corev1.LocalObjectReference{Name: "hf-mirror"},
		},
	}
	svc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime: RuntimeVLLM, ModelRef: "qwen", SkipModelInit: ptr.To(true),
			Env: []corev1.EnvVar{{Name: "HF_ENDPOINT", Value: "https://user.example"}},
		},
	}
	endpoints := servingEnv(r.deploymentFor(svc, model, resolveEffectiveModelCache(svc, model), nil, 1, testMirror, ""), "HF_ENDPOINT")
	if len(endpoints) != 2 || endpoints[len(endpoints)-1].Value != "https://user.example" {
		t.Fatalf("HF_ENDPOINT entries = %+v, want the operator's first and the user's last", endpoints)
	}
}

// The conflict must be visible: Reconcile emits a Warning naming both ways out,
// and the Deployment it creates carries no mirror.
var _ = Describe("runtime HF mirror reconcile", func() {
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
		modelName, isvcName, secretName = "mirror-model-"+suffix, "mirror-isvc-"+suffix, "hf-mirror-"+suffix
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
			StringData: map[string]string{"HF_ENDPOINT": testMirror + "\n", "HF_TOKEN": "t"},
		})).To(Succeed())
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
			Spec: inferencev1alpha1.ModelSpec{
				Source:          "Qwen/Qwen3.8-27B-FP8",
				SourceSecretRef: &corev1.LocalObjectReference{Name: secretName},
				Hardware:        &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
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

	reconcileService := func(vllm *inferencev1alpha1.VLLMConfig) *appsv1.Deployment {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: modelName, Runtime: RuntimeVLLM, Replicas: ptr.To(int32(1)),
				SkipModelInit: ptr.To(true), VLLMConfig: vllm,
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		r := &InferenceServiceReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder,
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: isvcName, Namespace: namespace}})
		Expect(err).NotTo(HaveOccurred())
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, dep)).To(Succeed())
		return dep
	}

	It("points the runtime at the trimmed mirror from the Model's Secret", func() {
		dep := reconcileService(nil)
		Expect(servingEnv(dep, "HF_ENDPOINT")).To(ConsistOf(corev1.EnvVar{Name: "HF_ENDPOINT", Value: testMirror}))
		Expect(drainEvents()).NotTo(ContainElement(ContainSubstring(ReasonHFMirrorNotApplied)))
	})

	It("warns and keeps the mirror off when the runtime's token is in another Secret", func() {
		dep := reconcileService(&inferencev1alpha1.VLLMConfig{HFTokenSecretRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "hf-public"}, Key: "token",
		}})
		Expect(servingEnv(dep, "HF_ENDPOINT")).To(BeEmpty())
		Expect(drainEvents()).To(ContainElement(SatisfyAll(
			ContainSubstring(ReasonHFMirrorNotApplied),
			ContainSubstring(secretName),
		)))
	})
})
