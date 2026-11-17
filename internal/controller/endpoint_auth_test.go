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
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func keyedISvc(runtime string, extraArgs ...string) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime: runtime, ModelRef: "m", ExtraArgs: extraArgs,
			Endpoint: &inferencev1alpha1.EndpointSpec{APIKeySecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key",
			}},
		},
	}
}

func renderContainer(t *testing.T, isvc *inferencev1alpha1.InferenceService) corev1.Container {
	t.Helper()
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "org/model"},
	}
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	return r.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec.Containers[0]
}

func envNamed(c corev1.Container, name string) []corev1.EnvVar {
	var out []corev1.EnvVar
	for _, e := range c.Env {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

func TestEndpointAPIKeyVLLMEnv(t *testing.T) {
	c := renderContainer(t, keyedISvc(RuntimeVLLM))
	got := envNamed(c, "VLLM_API_KEY")
	if len(got) != 1 || got[0].ValueFrom == nil || got[0].ValueFrom.SecretKeyRef == nil {
		t.Fatalf("VLLM_API_KEY = %+v, want one secretKeyRef entry", got)
	}
	ref := got[0].ValueFrom.SecretKeyRef
	if ref.Name != "upstream-key" || ref.Key != "key" {
		t.Errorf("secretKeyRef = %s/%s, want upstream-key/key", ref.Name, ref.Key)
	}
	// Review Focus 2: a missing Secret must stop the pod, not start it open.
	if ref.Optional != nil && *ref.Optional {
		t.Errorf("secretKeyRef is optional; a missing Secret would start vLLM without auth")
	}
	if slices.ContainsFunc(c.Args, func(a string) bool { return strings.Contains(a, "api-key") }) {
		t.Errorf("vLLM args carry an api-key flag %v; the key must travel only as env", c.Args)
	}
}

func TestEndpointAPIKeySGLangConfigArgs(t *testing.T) {
	c := renderContainer(t, keyedISvc(RuntimeSGLANG))
	if got := envNamed(c, "LLMKUBE_API_KEY"); len(got) != 1 || got[0].Value != "" ||
		got[0].ValueFrom == nil || got[0].ValueFrom.SecretKeyRef == nil || got[0].ValueFrom.SecretKeyRef.Name != "upstream-key" {
		t.Fatalf("LLMKUBE_API_KEY = %+v, want one secretKeyRef to upstream-key", got)
	}
	if slices.ContainsFunc(c.Args, func(a string) bool {
		return strings.Contains(a, "api-key") || strings.Contains(a, "$(LLMKUBE_API_KEY)")
	}) {
		t.Fatalf("SGLang args expose the API key through a flag: %v", c.Args)
	}
	i := slices.Index(c.Args, "--config")
	if i < 0 || i+1 >= len(c.Args) {
		t.Fatalf("args = %v, want --config and a file path", c.Args)
	}
}

// Review Focus 5: a user-supplied --api-key wins, in both flag forms.
func TestEndpointAPIKeyUserFlagWins(t *testing.T) {
	for _, rt := range []string{RuntimeVLLM, RuntimeSGLANG} {
		for _, extra := range [][]string{{"--api-key", "mine"}, {"--api-key=mine"}} {
			c := renderContainer(t, keyedISvc(rt, extra...))
			if len(envNamed(c, "VLLM_API_KEY"))+len(envNamed(c, "LLMKUBE_API_KEY")) != 0 {
				t.Errorf("%s %v: LLMKube injected key env alongside the user's flag", rt, extra)
			}
			n := 0
			for _, a := range c.Args {
				if a == "--api-key" || strings.HasPrefix(a, "--api-key=") {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%s %v: %d --api-key flags in %v, want only the user's", rt, extra, n, c.Args)
			}
			if slices.Contains(c.Command, sglangAPIKeyLauncher) {
				t.Errorf("%s %v: Secret config launcher overrides the user's API-key flag", rt, extra)
			}
		}
	}
}

// Review Focus 2 (regression): a user's YAML can set apiKeySecretRef.optional:
// true (SecretKeySelector has the field). apiKeyEnvVar must still force the
// rendered secretKeyRef to required, and must do so on a copy, not by
// mutating the InferenceService's own spec value.
func TestEndpointAPIKeyUserOptionalIsForcedRequired(t *testing.T) {
	for _, rt := range []string{RuntimeVLLM, RuntimeSGLANG} {
		isvc := keyedISvc(rt)
		isvc.Spec.Endpoint.APIKeySecretRef.Optional = ptr.To(true)

		c := renderContainer(t, isvc)

		envName := vllmAPIKeyEnv
		if rt == RuntimeSGLANG {
			envName = sglangAPIKeyEnv
		}
		got := envNamed(c, envName)
		if len(got) != 1 || got[0].ValueFrom == nil || got[0].ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s: %s = %+v, want one secretKeyRef entry", rt, envName, got)
		}
		if opt := got[0].ValueFrom.SecretKeyRef.Optional; opt != nil && *opt {
			t.Errorf("%s: rendered secretKeyRef.optional = true; a missing Secret would start the server without auth", rt)
		}

		if opt := isvc.Spec.Endpoint.APIKeySecretRef.Optional; opt == nil || !*opt {
			t.Errorf("%s: isvc.Spec.Endpoint.APIKeySecretRef.Optional was mutated to %v; apiKeyEnvVar must copy, not mutate, the spec", rt, opt)
		}
	}
}

func TestEndpointAPIKeyUnsetRendersNothing(t *testing.T) {
	for _, rt := range []string{RuntimeVLLM, RuntimeSGLANG} {
		isvc := keyedISvc(rt)
		isvc.Spec.Endpoint = nil
		c := renderContainer(t, isvc)
		if len(envNamed(c, "VLLM_API_KEY"))+len(envNamed(c, "LLMKUBE_API_KEY")) != 0 ||
			slices.Contains(c.Args, "--api-key") {
			t.Errorf("%s without apiKeySecretRef rendered key wiring: env=%v args=%v", rt, c.Env, c.Args)
		}
	}
}

// endpointAPIKeyRef answers "what should LLMKube wire"; this answers "will the
// server want a key", which callers that talk to the server need instead.
func TestInferenceServiceRequiresAPIKey(t *testing.T) {
	withCommandArgs := func(command []string) *inferencev1alpha1.InferenceService {
		isvc := keyedISvc(RuntimeSGLANG)
		isvc.Spec.Endpoint = nil
		isvc.Spec.Command = command
		isvc.Spec.Args = []string{"--api-key", "$(MY_KEY)"}
		return isvc
	}
	unkeyed := keyedISvc(RuntimeSGLANG)
	unkeyed.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Port: 30000}
	flagOnly := keyedISvc(RuntimeSGLANG, "--api-key", "$(MY_KEY)")
	flagOnly.Spec.Endpoint = nil
	withEnv := func(runtime string, env ...corev1.EnvVar) *inferencev1alpha1.InferenceService {
		isvc := keyedISvc(runtime)
		isvc.Spec.Endpoint = nil
		isvc.Spec.Env = env
		return isvc
	}
	for _, tc := range []struct {
		name string
		isvc *inferencev1alpha1.InferenceService
		want bool
	}{
		{"nil", nil, false},
		{"no key", unkeyed, false},
		{"apiKeySecretRef", keyedISvc(RuntimeSGLANG), true},
		{"apiKeySecretRef and the user flag", keyedISvc(RuntimeSGLANG, "--api-key=$(MY_KEY)"), true},
		{"user flag only", flagOnly, true},
		{"spec.args flag with spec.command", withCommandArgs([]string{"python3", "-m", "sglang.launch_server"}), true},
		{"spec.args flag without spec.command", withCommandArgs(nil), false},
		{"vLLM env var literal", withEnv(RuntimeVLLM, corev1.EnvVar{Name: vllmAPIKeyEnv, Value: "sk"}), true},
		{"vLLM env var from a Secret", withEnv(RuntimeVLLM, corev1.EnvVar{Name: vllmAPIKeyEnv,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "k"}, Key: "key",
			}}}), true},
		// Presence is the verdict: the vLLM docs do not pin empty-value behavior.
		{"vLLM env var present but empty", withEnv(RuntimeVLLM, corev1.EnvVar{Name: vllmAPIKeyEnv}), true},
		{"SGLang ignores the vLLM env var", withEnv(RuntimeSGLANG, corev1.EnvVar{Name: vllmAPIKeyEnv, Value: "sk"}), false},
		{"llama.cpp (empty runtime) ignores the vLLM env var", withEnv("", corev1.EnvVar{Name: vllmAPIKeyEnv, Value: "sk"}), false},
		{"vLLM with an unrelated env var", withEnv(RuntimeVLLM, corev1.EnvVar{Name: "FOO", Value: "bar"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := inferenceServiceRequiresAPIKey(tc.isvc); got != tc.want {
				t.Errorf("inferenceServiceRequiresAPIKey = %v, want %v", got, tc.want)
			}
		})
	}
}
