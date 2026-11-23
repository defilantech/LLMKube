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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// agentVersionSkew compares a metal agent's version with the controller's
// against the supported skew window (#2408): the agent on the controller's
// minor release, and not newer than the controller. The upgrade order the
// runbook gives (the chart first, then the agents) keeps a fleet inside it:
// agents behind by patch releases while they catch up, never ahead.
//
// Versions parse as release versions with an optional "v" and any build text
// after the patch number ignored, so a main build (0.10.16-main.g912dce7) or
// a git-describe agent build (v0.10.17-3-gabc1234) counts as its release. The
// agent's string is reported by the agent, so messages print only the
// release numbers parsed from it, never the string itself.
func agentVersionSkew(controllerVersion, agentVersion string) (metav1.ConditionStatus, string, string) {
	ctrl, err := version.ParseGeneric(controllerVersion)
	if err != nil {
		return metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonVersionUnknown,
			"the controller version is not a release version (a development build?); set --controller-version"
	}
	if agentVersion == "" {
		return metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonVersionUnknown,
			"the agent has not reported a version"
	}
	agent, err := version.ParseGeneric(agentVersion)
	if err != nil {
		return metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonVersionUnknown,
			"the agent version is not a release version (a development build?)"
	}
	c, a := releaseOf(ctrl), releaseOf(agent)
	switch {
	case agent.Major() > ctrl.Major() || (agent.Major() == ctrl.Major() && agent.Minor() > ctrl.Minor()) ||
		(agent.Major() == ctrl.Major() && agent.Minor() == ctrl.Minor() && agent.Patch() > ctrl.Patch()):
		return metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonAgentNewerThanController, fmt.Sprintf(
			"agent %s is newer than the controller %s; upgrade the chart (the controller must be at least "+
				"as new as its agents)", a, c)
	case agent.Major() != ctrl.Major() || agent.Minor() != ctrl.Minor():
		return metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonAgentMinorBehind, fmt.Sprintf(
			"agent %s is from an older minor release than the controller %s; upgrade the agent to %d.%d.x",
			a, c, ctrl.Major(), ctrl.Minor())
	}
	return metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonVersionInSkewWindow, fmt.Sprintf(
		"agent %s is inside the supported skew window of the controller %s", a, c)
}

// releaseOf prints v's release numbers only, as major.minor.patch.
func releaseOf(v *version.Version) string {
	return fmt.Sprintf("%d.%d.%d", v.Major(), v.Minor(), v.Patch())
}
