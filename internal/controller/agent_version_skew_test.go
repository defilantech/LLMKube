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
	"strings"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestAgentVersionSkew is the skew window #2408 documents: the agent on the
// controller's minor release and not newer than it.
func TestAgentVersionSkew(t *testing.T) {
	tests := []struct {
		name, controller, agent string
		want                    metav1.ConditionStatus
		reason                  string
		msgHas                  string
	}{
		{"same release", "0.10.17", "0.10.17", metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonVersionInSkewWindow, ""},
		{"agent one patch behind", "0.10.18", "0.10.17", metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonVersionInSkewWindow, ""},
		{"agent many patches behind", "0.10.18", "0.10.1", metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonVersionInSkewWindow, ""},
		{"v prefixes", "v0.10.17", "v0.10.17", metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonVersionInSkewWindow, ""},
		{"main build of the controller", "0.10.16-main.g912dce7", "0.10.16", metav1.ConditionTrue,
			inferencev1alpha1.MetalNodeReasonVersionInSkewWindow, ""},
		{"git describe agent", "0.10.18", "v0.10.17-3-gabc1234", metav1.ConditionTrue,
			inferencev1alpha1.MetalNodeReasonVersionInSkewWindow, ""},
		{"agent one patch ahead", "0.10.17", "0.10.18", metav1.ConditionFalse,
			inferencev1alpha1.MetalNodeReasonAgentNewerThanController, "agent 0.10.18 is newer than the controller 0.10.17"},
		{"agent a minor ahead", "0.10.17", "0.11.0", metav1.ConditionFalse,
			inferencev1alpha1.MetalNodeReasonAgentNewerThanController, "upgrade the chart"},
		{"agent a minor behind", "0.10.17", "0.9.30", metav1.ConditionFalse,
			inferencev1alpha1.MetalNodeReasonAgentMinorBehind, "agent 0.9.30 is from an older minor release than the controller 0.10.17"},
		{"development controller", "dev", "0.10.17", metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonVersionUnknown,
			"the controller version is not a release version"},
		{"no agent version", "0.10.17", "", metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonVersionUnknown,
			"the agent has not reported a version"},
		{"unparseable agent version", "0.10.17", "banana\nsplit", metav1.ConditionUnknown,
			inferencev1alpha1.MetalNodeReasonVersionUnknown, "the agent version is not a release version"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, reason, msg := agentVersionSkew(tc.controller, tc.agent)
			if status != tc.want || reason != tc.reason {
				t.Fatalf("agentVersionSkew(%q, %q) = %s/%s (%q), want %s/%s", tc.controller, tc.agent,
					status, reason, msg, tc.want, tc.reason)
			}
			if tc.msgHas != "" && !strings.Contains(msg, tc.msgHas) {
				t.Errorf("message %q, want it to contain %q", msg, tc.msgHas)
			}
			if strings.Contains(msg, "banana") {
				t.Errorf("message %q quotes the agent's raw value", msg)
			}
		})
	}
}

// TestMetalNodeReconcileSetsAgentVersionSupported: the MetalNode reports
// whether its agent is inside the skew window.
func TestMetalNodeReconcileSetsAgentVersionSupported(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.r.ControllerVersion = "0.10.17"
	for _, tc := range []struct {
		name, agent string
		want        metav1.ConditionStatus
		reason      string
	}{
		{"skew-ahead", "0.10.18", metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonAgentNewerThanController},
		{"skew-same", "0.10.17", metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonVersionInSkewWindow},
	} {
		f.createMetalNode(tc.name, nil)
		report := testReport()
		report.Version = tc.agent
		f.putLease(tc.name, f.now.Add(-time.Second), encodeReport(t, report))
		f.reconcile(tc.name)

		c := meta.FindStatusCondition(f.get(tc.name).Status.Conditions,
			inferencev1alpha1.MetalNodeConditionAgentVersionSupported)
		if c == nil || c.Status != tc.want || c.Reason != tc.reason {
			t.Errorf("%s: AgentVersionSupported = %+v, want %s/%s", tc.name, c, tc.want, tc.reason)
		}
	}
}

// TestMetalAgentVersionReadsOnlyAgentSlices: a mirrored or foreign slice is
// not the agent's word on its version.
func TestMetalAgentVersionReadsOnlyAgentSlices(t *testing.T) {
	slices := []discoveryv1.EndpointSlice{
		{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{inferencev1alpha1.AnnotationAgentVersion: "0.11.0"}}},
		{ObjectMeta: metav1.ObjectMeta{
			Labels:      map[string]string{inferencev1alpha1.LabelManagedBy: "metal-agent"},
			Annotations: map[string]string{inferencev1alpha1.AnnotationAgentVersion: "0.10.17"},
		}},
	}
	if got := metalAgentVersion(slices); got != "0.10.17" {
		t.Errorf("metalAgentVersion = %q, want 0.10.17 from the agent's own slice", got)
	}
}
