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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The parity guard for #2148: an InferenceService that leaves
// spec.imagePullPolicy unset must render the pod template this operator emitted
// before the field existed. Rollouts key on desiredTemplateHash, so a byte that
// moves here rolls every serving pod on an operator upgrade.
//
// This file deliberately names no field the change adds, so it compiles and runs
// on both upstream/main and the branch. The hash below was recorded by running
// this file in a detached worktree of upstream/main.

const (
	// imagePullPolicyTripwireDeploymentHash is desiredTemplateHash of
	// constructDeployment for imagePullPolicyTripwireISvc and
	// imagePullPolicyTripwireModel.
	imagePullPolicyTripwireDeploymentHash = "cb92c4fc1275579a" //nolint:gosec // a SHA-256 prefix of a pod template, not a credential
)

func imagePullPolicyTripwireModel() *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "pull-policy-model", Namespace: "pull-policy-ns"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/models/pull-policy.gguf",
			Format: "gguf",
		},
		Status: inferencev1alpha1.ModelStatus{CacheKey: "pull-policy-key", Path: "/models/pull-policy-key/pull-policy.gguf"},
	}
}

// The fixture mirrors the report: a generic runtime on a moving nightly tag,
// with stageModel so the pod also carries the operator's own init containers.
func imagePullPolicyTripwireISvc() *inferencev1alpha1.InferenceService {
	stageModel := true
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "pull-policy-isvc", Namespace: "pull-policy-ns"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:   "pull-policy-model",
			Runtime:    "generic",
			StageModel: &stageModel,
			Image:      "registry.example.com/firefly2442/amd-strix-halo-comfyui:nightly",
		},
	}
}

func imagePullPolicyTripwireReconciler() *InferenceServiceReconciler {
	return &InferenceServiceReconciler{
		ModelCachePath:     "/models",
		ModelCacheMode:     ModelCacheModeShared,
		InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		DefaultFSGroup:     102,
	}
}

func TestImagePullPolicyUnsetTemplateUnchanged(t *testing.T) {
	r := imagePullPolicyTripwireReconciler()
	dep := r.constructDeployment(imagePullPolicyTripwireISvc(), imagePullPolicyTripwireModel(), nil, 1, "", "")

	if got := desiredTemplateHash(dep.Spec.Template); got != imagePullPolicyTripwireDeploymentHash {
		t.Errorf("pod template drifted from base: desiredTemplateHash = %s, want %s\n"+
			"every serving pod would roll on upgrade; an unset spec.imagePullPolicy must render exactly what base rendered",
			got, imagePullPolicyTripwireDeploymentHash)
	}

	if got := dep.Spec.Template.Spec.Containers[0].ImagePullPolicy; got != "" {
		t.Errorf("runtime container imagePullPolicy = %q, want empty so Kubernetes applies its own default", got)
	}

	initContainers := dep.Spec.Template.Spec.InitContainers
	if len(initContainers) == 0 {
		t.Fatal("fixture rendered no init containers; the init-container assertion below would be vacuous")
	}
	for _, c := range initContainers {
		if c.ImagePullPolicy != "" {
			t.Errorf("init container %q imagePullPolicy = %q, want empty", c.Name, c.ImagePullPolicy)
		}
	}
}
