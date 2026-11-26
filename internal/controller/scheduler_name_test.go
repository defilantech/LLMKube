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
	"encoding/json"
	"strings"
	"testing"
)

// With spec.schedulerName unset the serving pod template must be exactly what
// base emitted: no schedulerName key at all, so no cache-backed Pod rolls on
// upgrade and desiredTemplateHash stays where it was.
func TestSchedulerNameUnsetTemplateUnchanged(t *testing.T) {
	tmpl := cacheBackedServingTemplate(cachePrepTripwireISvcReconciler(), cachePrepTripwireIsvc(), cachePrepTripwireModel())
	if tmpl.Spec.SchedulerName != "" {
		t.Fatalf("unset schedulerName rendered %q on the pod template", tmpl.Spec.SchedulerName)
	}
	raw, err := json.Marshal(tmpl)
	if err != nil {
		t.Fatalf("marshal pod template: %v", err)
	}
	if strings.Contains(string(raw), `"schedulerName"`) {
		t.Fatalf("the unset field still serialises a schedulerName key into the pod template: %s", raw)
	}
}

// The field lands on the serving Pod template, which is what the scheduler
// reads.
func TestSchedulerNameReachesPodTemplate(t *testing.T) {
	isvc := cachePrepTripwireIsvc()
	isvc.Spec.SchedulerName = "kai-scheduler"
	tmpl := cacheBackedServingTemplate(cachePrepTripwireISvcReconciler(), isvc, cachePrepTripwireModel())
	if got := tmpl.Spec.SchedulerName; got != "kai-scheduler" {
		t.Fatalf("pod template schedulerName = %q, want %q", got, "kai-scheduler")
	}
}

// desiredTemplateHash is what tells the rollout path a template changed, so
// setting or clearing the field must move it and roll the Pods.
func TestSchedulerNameChangesTemplateHash(t *testing.T) {
	r := cachePrepTripwireISvcReconciler()
	isvc := cachePrepTripwireIsvc()
	model := cachePrepTripwireModel()
	cache := resolveEffectiveModelCache(isvc, model)
	unset := desiredTemplateHash(r.deploymentFor(isvc, model, cache, nil, 1, "", "").Spec.Template)

	isvc.Spec.SchedulerName = "kai-scheduler"
	set := desiredTemplateHash(r.deploymentFor(isvc, model, cache, nil, 1, "", "").Spec.Template)

	if unset == set {
		t.Fatalf("the template hash did not change when schedulerName was set (%s); the RolloutPolicy path would not roll the Pods", set)
	}
}

// multiNode member Pods are specialised from the serving template: every one
// must carry the field, and a change must recreate the whole group.
func TestMultiNodeMemberPodsCarrySchedulerName(t *testing.T) {
	isvc, model := multiNodeFixture()
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	cache := resolveEffectiveModelCache(isvc, model)

	pods, unsetHash, err := r.memberPodsFor(isvc, model, cache, nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	for _, pod := range pods {
		if pod.Spec.SchedulerName != "" {
			t.Fatalf("unset schedulerName rendered %q on member pod %s", pod.Spec.SchedulerName, pod.Name)
		}
	}

	isvc.Spec.SchedulerName = "kai-scheduler"
	pods, setHash, err := r.memberPodsFor(isvc, model, cache, nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	for _, pod := range pods {
		if got := pod.Spec.SchedulerName; got != "kai-scheduler" {
			t.Fatalf("member pod %s schedulerName = %q, want %q", pod.Name, got, "kai-scheduler")
		}
	}
	if unsetHash == setHash {
		t.Fatalf("the multiNode group hash did not change when schedulerName was set (%s); the group would not be recreated", setHash)
	}
}

// A Metal service's relay runs CPU work for the agent, so it stays on the
// default scheduler whatever the InferenceService asks for.
func TestSchedulerNameNotOnRelayDeployment(t *testing.T) {
	isvc := cachePrepTripwireIsvc()
	isvc.Namespace = "team-a"
	isvc.Spec.SchedulerName = "kai-scheduler"
	dep := (&InferenceServiceReconciler{}).newRelayDeployment(isvc, "c2l4dGVlbi1ieXRlcy0xNg==")
	if dep.Spec.Template.Spec.SchedulerName != "" {
		t.Fatalf("relay pod template schedulerName = %q, want empty", dep.Spec.Template.Spec.SchedulerName)
	}
	raw, err := json.Marshal(dep.Spec.Template)
	if err != nil {
		t.Fatalf("marshal relay pod template: %v", err)
	}
	if strings.Contains(string(raw), `"schedulerName"`) {
		t.Fatalf("the relay pod template carries a schedulerName key: %s", raw)
	}
}
