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

	corev1 "k8s.io/api/core/v1"
)

func TestImagePullPolicyReachesRuntimeContainer(t *testing.T) {
	for _, policy := range []corev1.PullPolicy{
		corev1.PullAlways,
		corev1.PullIfNotPresent,
		corev1.PullNever,
	} {
		t.Run(string(policy), func(t *testing.T) {
			isvc := imagePullPolicyTripwireISvc()
			isvc.Spec.ImagePullPolicy = policy

			dep := imagePullPolicyTripwireReconciler().
				constructDeployment(isvc, imagePullPolicyTripwireModel(), nil, 1, "", "")

			if got := dep.Spec.Template.Spec.Containers[0].ImagePullPolicy; got != policy {
				t.Errorf("runtime container imagePullPolicy = %q, want %q", got, policy)
			}
			if got := dep.Spec.Template.Spec.Containers[0].Image; got != isvc.Spec.Image {
				t.Errorf("runtime container image = %q, want %q", got, isvc.Spec.Image)
			}
		})
	}
}

// The init containers run operator-chosen images, so the field must not leak
// into them: a user asking for Always on their own image does not ask for it on
// the model downloader or the cache prep.
func TestImagePullPolicyInitContainersUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy corev1.PullPolicy
	}{
		{"unset", ""},
		{"set to Always", corev1.PullAlways},
		{"set to Never", corev1.PullNever},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isvc := imagePullPolicyTripwireISvc()
			isvc.Spec.ImagePullPolicy = tc.policy

			dep := imagePullPolicyTripwireReconciler().
				constructDeployment(isvc, imagePullPolicyTripwireModel(), nil, 1, "", "")

			initContainers := dep.Spec.Template.Spec.InitContainers
			if len(initContainers) == 0 {
				t.Fatal("fixture rendered no init containers; the assertion below would be vacuous")
			}
			for _, c := range initContainers {
				if c.ImagePullPolicy != "" {
					t.Errorf("init container %q imagePullPolicy = %q, want empty", c.Name, c.ImagePullPolicy)
				}
			}
		})
	}
}

// Setting the policy has to change the pod template, or the reconciler would
// treat it as a no-op and never roll the pods that need the new policy.
func TestImagePullPolicyChangesTemplateHash(t *testing.T) {
	r := imagePullPolicyTripwireReconciler()
	model := imagePullPolicyTripwireModel()

	unset := imagePullPolicyTripwireISvc()
	set := imagePullPolicyTripwireISvc()
	set.Spec.ImagePullPolicy = corev1.PullAlways

	unsetHash := desiredTemplateHash(r.constructDeployment(unset, model, nil, 1, "", "").Spec.Template)
	setHash := desiredTemplateHash(r.constructDeployment(set, model, nil, 1, "", "").Spec.Template)

	if unsetHash == setHash {
		t.Errorf("pod template hash did not change when imagePullPolicy was set (%s); the pods would never roll", unsetHash)
	}
}

// The field sits on the runtime container, so it applies even when the operator
// chose the image. It is not gated on a user-supplied spec.image.
func TestImagePullPolicyWithoutSpecImage(t *testing.T) {
	isvc := imagePullPolicyTripwireISvc()
	isvc.Spec.Runtime = "llamacpp"
	isvc.Spec.Image = ""
	isvc.Spec.ImagePullPolicy = corev1.PullAlways

	dep := imagePullPolicyTripwireReconciler().
		constructDeployment(isvc, imagePullPolicyTripwireModel(), nil, 1, "", "")

	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image == "" {
		t.Fatal("fixture rendered an empty runtime image; the backend default should have filled it")
	}
	if c.Image != resolveBackend(isvc).DefaultImage() {
		t.Errorf("runtime container image = %q, want the backend default %q", c.Image, resolveBackend(isvc).DefaultImage())
	}
	if c.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("runtime container imagePullPolicy = %q, want Always", c.ImagePullPolicy)
	}
}

// multiNode member pods are specialised from the Deployment template, so they
// inherit the policy and the group hash covers it.
func TestMultiNodeMemberPodsInheritImagePullPolicy(t *testing.T) {
	isvc, model := multiNodeFixture()
	isvc.Spec.ImagePullPolicy = corev1.PullAlways
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}

	pods, _, err := r.constructMemberPods(isvc, model, nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	if len(pods) == 0 {
		t.Fatal("constructMemberPods returned no pods")
	}
	for _, p := range pods {
		if got := p.Spec.Containers[0].ImagePullPolicy; got != corev1.PullAlways {
			t.Errorf("pod %q container imagePullPolicy = %q, want Always", p.Name, got)
		}
	}

	unsetIsvc, _ := multiNodeFixture()
	_, unsetHash, err := r.constructMemberPods(unsetIsvc, model, nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	_, setHash, err := r.constructMemberPods(isvc, model, nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	if unsetHash == setHash {
		t.Errorf("multiNode group hash did not change with imagePullPolicy (%s)", setHash)
	}
}
