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

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"
)

// The agent write policy (#2081) ships twice as well: the chart template and
// deployment/macos/metal-agent-write-policy.yaml for installs without Helm.
// pkg/agent/write_policy_envtest_test.go proves the manifest's CEL against a
// real API server, so the manifest is the source of truth here too. The chart
// copy must carry the same match constraints and validations, and the same
// match conditions plus one more that excludes the controller's own
// ServiceAccount (a release named llmkube-metal-* would otherwise put the
// controller under the agent's rule).

const (
	writePolicyTemplate = "templates/metal-agent-write-policy.yaml"
	// labelPolicyTemplate is the second half of the same rule: any writer of a
	// labeled object must be a mac agent (GHSA-863w-vh42-mjhj). It has no
	// identity match condition, so the chart copy equals the manifest.
	labelPolicyTemplate = "templates/metal-agent-label-policy.yaml"
	// controllerExclusion is the name of the chart's extra match condition.
	controllerExclusion = "not-llmkube-controller"
	// writePolicyController is the controller username of the release
	// renderWritePolicy renders: release rel in namespace llmkube-system.
	writePolicyController = "system:serviceaccount:llmkube-system:rel-llmkube-controller-manager"
	// labelPolicyController is the controller username the standalone label
	// manifest names, whose install has no release. The chart's copy names
	// its own release's controller in the same condition.
	labelPolicyController = "system:serviceaccount:llmkube-system:llmkube-controller-manager"
)

// writePolicy is one copy of the policy: its ValidatingAdmissionPolicy and
// binding.
type writePolicy struct {
	policy  *admissionregistrationv1.ValidatingAdmissionPolicy
	binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
}

// readWritePolicy decodes a stream holding exactly one policy and one binding.
func readWritePolicy(raw []byte) (writePolicy, error) {
	var wp writePolicy
	for _, doc := range splitDocs(string(raw)) {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			return wp, err
		}
		switch head.Kind {
		case "":
			continue
		case "ValidatingAdmissionPolicy":
			if wp.policy != nil {
				return wp, fmt.Errorf("more than one ValidatingAdmissionPolicy")
			}
			wp.policy = &admissionregistrationv1.ValidatingAdmissionPolicy{}
			if err := yaml.UnmarshalStrict([]byte(doc), wp.policy); err != nil {
				return wp, err
			}
		case "ValidatingAdmissionPolicyBinding":
			if wp.binding != nil {
				return wp, fmt.Errorf("more than one ValidatingAdmissionPolicyBinding")
			}
			wp.binding = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			if err := yaml.UnmarshalStrict([]byte(doc), wp.binding); err != nil {
				return wp, err
			}
		default:
			return wp, fmt.Errorf("unexpected kind %q", head.Kind)
		}
	}
	if wp.policy == nil {
		return wp, fmt.Errorf("no ValidatingAdmissionPolicy document")
	}
	if wp.binding == nil {
		return wp, fmt.Errorf("no ValidatingAdmissionPolicyBinding document")
	}
	return wp, nil
}

// renderWritePolicy renders the chart's write policy as on a cluster that
// serves ValidatingAdmissionPolicy, with the default values.
func renderWritePolicy(helmBin, chartPath, template string) ([]byte, error) {
	cmd := exec.Command(helmBin, "template", "rel", chartPath,
		"--namespace", "llmkube-system",
		"--show-only", template,
		"--api-versions", "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy")
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// runWritePolicy reads the manifest, renders the chart, and returns one
// problem per divergence.
func runWritePolicy(root, manifestPath, chartPath, helmBin, template string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, manifestPath))
	if err != nil {
		return nil, err
	}
	manifest, err := readWritePolicy(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", manifestPath, err)
	}
	out, err := renderWritePolicy(helmBin, filepath.Join(root, chartPath), template)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", template, err)
	}
	chart, err := readWritePolicy(out)
	if err != nil {
		return nil, fmt.Errorf("rendered %s: %w", template, err)
	}
	chartUser := writePolicyController
	manifestUser := ""
	if template == labelPolicyTemplate {
		manifestUser = labelPolicyController
	}
	return compareWritePolicy(manifest, chart, chartUser, manifestUser), nil
}

// compareWritePolicy returns one problem per field where the chart's policy
// is not the manifest's plus the controller exclusion for controllerUser.
func compareWritePolicy(manifest, chart writePolicy, controllerUser, manifestController string) []string {
	var problems []string
	differ := func(field string, want, got any) {
		if !reflect.DeepEqual(want, got) {
			problems = append(problems, fmt.Sprintf(
				"[write-policy] chart %s differs from the manifest:\n  want:  %+v\n  chart: %+v", field, want, got))
		}
	}

	m, c := manifest.policy.Spec, chart.policy.Spec
	differ("spec.failurePolicy", m.FailurePolicy, c.FailurePolicy)
	differ("spec.paramKind", m.ParamKind, c.ParamKind)
	differ("spec.matchConstraints", m.MatchConstraints, c.MatchConstraints)
	differ("spec.variables", m.Variables, c.Variables)
	differ("spec.validations", m.Validations, c.Validations)
	differ("spec.auditAnnotations", m.AuditAnnotations, c.AuditAnnotations)

	var wantConditions []admissionregistrationv1.MatchCondition
	if len(m.MatchConditions) > 0 {
		wantConditions = append([]admissionregistrationv1.MatchCondition{}, m.MatchConditions...)
	}
	var chartConditions []admissionregistrationv1.MatchCondition
	if len(c.MatchConditions) > 0 {
		chartConditions = c.MatchConditions
	}
	switch {
	case manifestController == "":
		// The write manifest names no controller; the chart adds the
		// exclusion.
		wantConditions = append(wantConditions, admissionregistrationv1.MatchCondition{
			Name:       controllerExclusion,
			Expression: fmt.Sprintf("request.userInfo.username != '%s'", controllerUser),
		})
		differ("spec.matchConditions (the manifest's plus the controller exclusion)", wantConditions, chartConditions)
	default:
		// The label manifest names the kustomize controller in the same
		// condition; the chart names its own release's.
		if len(m.MatchConditions) > 0 {
			wantConditions[len(wantConditions)-1].Expression =
				fmt.Sprintf("request.userInfo.username != '%s'", controllerUser)
		}
		differ("spec.matchConditions (the manifest's, naming the chart's controller)", wantConditions, chartConditions)
	}

	mb, cb := manifest.binding.Spec, chart.binding.Spec
	differ("binding spec.validationActions", mb.ValidationActions, cb.ValidationActions)
	differ("binding spec.matchResources", mb.MatchResources, cb.MatchResources)
	differ("binding spec.paramRef", mb.ParamRef, cb.ParamRef)
	if mb.PolicyName != manifest.policy.Name {
		problems = append(problems, fmt.Sprintf(
			"[write-policy] manifest binding names policy %q, want %q", mb.PolicyName, manifest.policy.Name))
	}
	if cb.PolicyName != chart.policy.Name {
		problems = append(problems, fmt.Sprintf(
			"[write-policy] chart binding names policy %q, want %q", cb.PolicyName, chart.policy.Name))
	}
	return problems
}
