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
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

// realWritePolicy reads the committed manifest, the source of truth the
// comparison cases mutate a chart copy of.
func realWritePolicy(t *testing.T) writePolicy {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "deployment/macos/metal-agent-write-policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wp, err := readWritePolicy(raw)
	if err != nil {
		t.Fatalf("readWritePolicy: %v", err)
	}
	return wp
}

// chartCopy is what the chart should render for manifest: the same policy
// with the controller exclusion appended, under a release-prefixed name.
func chartCopy(manifest writePolicy, controllerUser string) writePolicy {
	p := manifest.policy.DeepCopy()
	b := manifest.binding.DeepCopy()
	p.Name, b.Name, b.Spec.PolicyName = "rel-x", "rel-x", "rel-x"
	p.Spec.MatchConditions = append(p.Spec.MatchConditions, admissionregistrationv1.MatchCondition{
		Name:       controllerExclusion,
		Expression: fmt.Sprintf("request.userInfo.username != '%s'", controllerUser),
	})
	return writePolicy{policy: p, binding: b}
}

// TestReadWritePolicy pins the committed manifest's shape: one policy, one
// binding, both named llmkube-metal-agent-writes, and no controller exclusion
// (the kustomize controller's ServiceAccount does not match the prefix).
func TestReadWritePolicy(t *testing.T) {
	wp := realWritePolicy(t)
	if wp.policy.Name != "llmkube-metal-agent-writes" || wp.binding.Spec.PolicyName != wp.policy.Name {
		t.Fatalf("policy %q bound by %q, want llmkube-metal-agent-writes", wp.policy.Name, wp.binding.Spec.PolicyName)
	}
	for _, mc := range wp.policy.Spec.MatchConditions {
		if mc.Name == controllerExclusion {
			t.Fatalf("the manifest carries the chart-only %s condition", controllerExclusion)
		}
	}
	if len(wp.policy.Spec.Validations) != 2 {
		t.Fatalf("manifest has %d validations, want 2 (new object, old object)", len(wp.policy.Spec.Validations))
	}

	if _, err := readWritePolicy([]byte("kind: ValidatingAdmissionPolicy\nmetadata: {name: a}\n")); err == nil {
		t.Fatal("a stream without a binding was accepted")
	}
	if _, err := readWritePolicy([]byte("kind: Role\n")); err == nil {
		t.Fatal("an unexpected kind was accepted")
	}
}

// TestCompareWritePolicy pins each field the comparison must catch.
func TestCompareWritePolicy(t *testing.T) {
	const user = "system:serviceaccount:llmkube-system:rel-llmkube-controller-manager"
	manifest := realWritePolicy(t)

	if problems := compareWritePolicy(manifest, chartCopy(manifest, user), user, ""); len(problems) != 0 {
		t.Fatalf("a faithful chart copy reported problems: %v", problems)
	}

	tests := []struct {
		name    string
		mutate  func(c *writePolicy)
		wantSub string
	}{
		{
			name: "a validation expression changed",
			mutate: func(c *writePolicy) {
				c.policy.Spec.Validations[1].Expression = "true"
			},
			wantSub: "spec.validations",
		},
		{
			name: "DELETE dropped from the match constraints",
			mutate: func(c *writePolicy) {
				c.policy.Spec.MatchConstraints.ResourceRules[0].Operations =
					[]admissionregistrationv1.OperationType{"CREATE", "UPDATE"}
			},
			wantSub: "spec.matchConstraints",
		},
		{
			name: "controller exclusion missing",
			mutate: func(c *writePolicy) {
				c.policy.Spec.MatchConditions = c.policy.Spec.MatchConditions[:1]
			},
			wantSub: "spec.matchConditions",
		},
		{
			name: "controller exclusion names another user",
			mutate: func(c *writePolicy) {
				c.policy.Spec.MatchConditions[1].Expression = "request.userInfo.username != 'someone'"
			},
			wantSub: "spec.matchConditions",
		},
		{
			name: "identity condition changed",
			mutate: func(c *writePolicy) {
				c.policy.Spec.MatchConditions[0].Expression = "true"
			},
			wantSub: "spec.matchConditions",
		},
		{
			name: "failurePolicy Ignore",
			mutate: func(c *writePolicy) {
				ignore := admissionregistrationv1.Ignore
				c.policy.Spec.FailurePolicy = &ignore
			},
			wantSub: "spec.failurePolicy",
		},
		{
			name: "binding only warns",
			mutate: func(c *writePolicy) {
				c.binding.Spec.ValidationActions = []admissionregistrationv1.ValidationAction{"Warn"}
			},
			wantSub: "binding spec.validationActions",
		},
		{
			name: "binding scoped to a namespace selector",
			mutate: func(c *writePolicy) {
				c.binding.Spec.MatchResources = &admissionregistrationv1.MatchResources{}
			},
			wantSub: "binding spec.matchResources",
		},
		{
			name: "binding names another policy",
			mutate: func(c *writePolicy) {
				c.binding.Spec.PolicyName = "elsewhere"
			},
			wantSub: `chart binding names policy "elsewhere"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := chartCopy(manifest, user)
			tt.mutate(&c)
			problems := compareWritePolicy(manifest, c, user, "")
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantSub) {
				t.Fatalf("problems %v do not contain %q", problems, tt.wantSub)
			}
		})
	}
}

// TestRealRepoWritePolicy is the acceptance test: the committed chart renders
// the committed manifest's policy plus the controller exclusion. It needs
// helm and skips without it, like TestRealRepo.
func TestRealRepoWritePolicy(t *testing.T) {
	helmBin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not on PATH; run make check-metal-agent-rbac in CI")
	}
	problems, err := runWritePolicy(repoRoot(t),
		"deployment/macos/metal-agent-write-policy.yaml", "charts/llmkube", helmBin, writePolicyTemplate)
	if err != nil {
		t.Fatalf("runWritePolicy: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("chart and manifest write policies diverge:\n%s", strings.Join(problems, "\n"))
	}
}

// TestRealRepoLabelPolicy is the same acceptance test for the label policy:
// the chart's copy names its release's controller where the manifest names
// the kustomize one.
func TestRealRepoLabelPolicy(t *testing.T) {
	helmBin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not on PATH; run make check-metal-agent-rbac in CI")
	}
	problems, err := runWritePolicy(repoRoot(t),
		"deployment/macos/metal-agent-label-policy.yaml", "charts/llmkube", helmBin, labelPolicyTemplate)
	if err != nil {
		t.Fatalf("runWritePolicy: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("chart and manifest label policies diverge:\n%s", strings.Join(problems, "\n"))
	}
}
