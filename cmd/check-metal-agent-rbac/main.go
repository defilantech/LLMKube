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

// Command check-metal-agent-rbac keeps the Helm chart's metal-agent identity
// identical to deployment/macos/metal-agent-rbac.yaml (#2152), and the chart's
// agent write policy identical to deployment/macos/metal-agent-write-policy.yaml
// apart from the controller exclusion only the chart needs (#2081).
//
// The standalone manifest is what cmd/metal-agent/rbac_test.go pins to the
// agent's own API calls, so it is the source of truth for the rules. The chart
// now renders a second copy of that identity for people who install with Helm,
// and nothing else stops the two drifting: a rule added to the manifest alone
// leaves Helm installs under-privileged, and a rule added to the chart alone
// grants the agent more than the audited set.
//
// The comparison is exact equality, not a superset check. An extra chart rule
// is a privilege grant nobody audited against the agent's code, so it fails
// here the same way a missing one does.
//
// Usage:
//
//	go run ./cmd/check-metal-agent-rbac --root .
//
// It exits 1 and prints one FAIL line per problem.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// render is one way the chart's template can produce the identity: a scope, an
// optional pinned Secret list, and the namespaces to expect.
type render struct {
	name       string
	scope      string
	namespaces []string
	secretPins []string
}

// renders covers both scopes, with the Secret read unpinned and pinned. The
// namespaced renders use two namespaces so a template that ignores the loop
// fails instead of passing on a single-element list. serviceAccount.create
// false is covered by the chart's unit tests, not here: it changes which
// documents render, not the rules those documents carry.
var renders = []render{
	{name: "namespaced", scope: "namespaced", namespaces: []string{"ml", "team-b"}},
	{name: "cluster", scope: "cluster"},
	{
		name:       "namespaced-pinned",
		scope:      "namespaced",
		namespaces: []string{"ml"},
		secretPins: []string{"llmkube-metal-relay"},
	},
	// The cluster-wide read is the highest-stakes branch of the pin option.
	{name: "cluster-pinned", scope: "cluster", secretPins: []string{"llmkube-metal-relay"}},
}

func main() {
	root := flag.String("root", ".", "repo root")
	manifest := flag.String("manifest", "deployment/macos/metal-agent-rbac.yaml",
		"standalone RBAC manifest, relative to --root")
	writePolicyManifest := flag.String("write-policy", "deployment/macos/metal-agent-write-policy.yaml",
		"standalone agent write policy, relative to --root")
	labelPolicyManifest := flag.String("label-policy", "deployment/macos/metal-agent-label-policy.yaml",
		"standalone agent label policy, relative to --root")
	chart := flag.String("chart", "charts/llmkube", "chart directory, relative to --root")
	helm := flag.String("helm", "helm", "helm binary")
	flag.Parse()

	problems, err := run(*root, *manifest, *chart, *helm)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-metal-agent-rbac:", err)
		os.Exit(2)
	}
	policyProblems, err := runWritePolicy(*root, *writePolicyManifest, *chart, *helm, writePolicyTemplate)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-metal-agent-rbac:", err)
		os.Exit(2)
	}
	problems = append(problems, policyProblems...)
	labelProblems, err := runWritePolicy(*root, *labelPolicyManifest, *chart, *helm, labelPolicyTemplate)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-metal-agent-rbac:", err)
		os.Exit(2)
	}
	problems = append(problems, labelProblems...)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "FAIL: %s\n", p)
		}
		os.Exit(1)
	}
	fmt.Println("✅ metal-agent RBAC matches deployment/macos/metal-agent-rbac.yaml")
	fmt.Println("✅ metal-agent write policy matches deployment/macos/metal-agent-write-policy.yaml")
	fmt.Println("✅ metal-agent label policy matches deployment/macos/metal-agent-label-policy.yaml")
}

// run reads the manifest, renders the chart for every branch, and returns one
// human-readable problem per divergence.
func run(root, manifestPath, chartPath, helmBin string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, manifestPath))
	if err != nil {
		return nil, err
	}
	manifestRules, manifestSA, err := readManifest(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", manifestPath, err)
	}

	var problems []string
	for _, r := range renders {
		out, err := renderChart(helmBin, filepath.Join(root, chartPath), r)
		if err != nil {
			return nil, fmt.Errorf("render (%s): %w", r.name, err)
		}
		problems = append(problems, checkRender(r, out, manifestRules, manifestSA)...)
	}
	return problems, nil
}

// readManifest returns the rules of the Role in the standalone manifest and the
// ServiceAccount name, which is the name the chart must reproduce.
func readManifest(raw []byte) ([]rbacv1.PolicyRule, string, error) {
	var rules []rbacv1.PolicyRule
	var saName string
	var roleFound, saFound bool
	for _, doc := range splitDocs(string(raw)) {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			return nil, "", err
		}
		switch head.Kind {
		case "Role":
			var role rbacv1.Role
			if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
				return nil, "", err
			}
			rules, roleFound = role.Rules, true
		case "ServiceAccount":
			var sa struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			}
			if err := yaml.Unmarshal([]byte(doc), &sa); err != nil {
				return nil, "", err
			}
			saName, saFound = sa.Metadata.Name, true
		}
	}
	if !roleFound {
		return nil, "", fmt.Errorf("no Role document")
	}
	if !saFound {
		return nil, "", fmt.Errorf("no ServiceAccount document")
	}
	return rules, saName, nil
}

// renderChart renders the chart's metal-agent template for one branch and
// returns the documents it produced.
func renderChart(helmBin, chartPath string, r render) ([]map[string]any, error) {
	args := []string{
		"template", "rel", chartPath,
		"--namespace", "llmkube-system",
		"--show-only", "templates/metal-agent-rbac.yaml",
		"--set", "metalAgent.rbac.enabled=true",
		"--set", fmt.Sprintf("metalAgent.rbac.scope=%s", r.scope),
	}
	if len(r.namespaces) > 0 {
		args = append(args, "--set", "metalAgent.rbac.namespaces={"+strings.Join(r.namespaces, ",")+"}")
	}
	if len(r.secretPins) > 0 {
		args = append(args, "--set", "metalAgent.rbac.secretNames={"+strings.Join(r.secretPins, ",")+"}")
	}
	cmd := exec.Command(helmBin, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var docs []map[string]any
	for _, doc := range splitDocs(string(out)) {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			return nil, err
		}
		if len(m) > 0 {
			docs = append(docs, m)
		}
	}
	return docs, nil
}

// checkRender compares one rendered branch against the manifest.
func checkRender(r render, docs []map[string]any, manifestRules []rbacv1.PolicyRule, manifestSA string) []string {
	var problems []string

	wantNamespaces := r.namespaces
	if r.scope == "cluster" {
		wantNamespaces = []string{""}
	}

	// No credential may be rendered: the chart installs the identity and the
	// kubeconfig is minted by hand (helm uninstall deletes every manifest
	// object, and a rendered token Secret would be a long-lived credential the
	// release owns).
	for _, d := range docs {
		if kind, _ := d["kind"].(string); kind == "Secret" {
			problems = append(problems, fmt.Sprintf(
				"[%s] renders a Secret; the chart must not create the agent's credentials", r.name))
		}
	}

	expectKind := "Role"
	expectBinding := "RoleBinding"
	if r.scope == "cluster" {
		expectKind = "ClusterRole"
		expectBinding = "ClusterRoleBinding"
	}

	for _, ns := range wantNamespaces {
		// The ServiceAccount and binding live in a real namespace in both
		// scopes; only the Role/ClusterRole lookup differs ("" for cluster).
		saNS := ns
		if saNS == "" {
			saNS = "llmkube-system"
		}
		ruleDoc := findDoc(docs, expectKind, ns)
		if ruleDoc == nil {
			problems = append(problems, fmt.Sprintf(
				"[%s] no %s for namespace %q", r.name, expectKind, ns))
			continue
		}
		rendered, err := rulesOf(ruleDoc)
		if err != nil {
			problems = append(problems, fmt.Sprintf("[%s] %s in %q: %v", r.name, expectKind, ns, err))
			continue
		}
		problems = append(problems, compareRules(r.name, rendered, withPins(manifestRules, r.secretPins), r.secretPins)...)

		saDoc := findDoc(docs, "ServiceAccount", saNS)
		if saDoc == nil {
			problems = append(problems, fmt.Sprintf(
				"[%s] no ServiceAccount for namespace %q", r.name, ns))
		} else if name := metaName(saDoc); name != manifestSA {
			problems = append(problems, fmt.Sprintf(
				"[%s] ServiceAccount name %q != manifest %q", r.name, name, manifestSA))
		}

		binding := findDoc(docs, expectBinding, ns)
		if binding == nil {
			problems = append(problems, fmt.Sprintf(
				"[%s] no %s for namespace %q", r.name, expectBinding, ns))
			continue
		}
		problems = append(problems, checkBinding(r.name, expectBinding, expectKind, binding, manifestSA, saNS)...)
	}
	return problems
}

// checkBinding pins the binding to the same Role/ClusterRole, a ServiceAccount
// subject in the same namespace, and the manifest's ServiceAccount name, so a
// rename on either side is caught here instead of at kubectl apply time.
func checkBinding(renderName, bindingKind, roleKind string, binding map[string]any, manifestSA, ns string) []string {
	var b rbacv1.RoleBinding
	if err := remarshal(binding, &b); err != nil {
		return []string{fmt.Sprintf("[%s] %s in %q: %v", renderName, bindingKind, ns, err)}
	}
	var problems []string
	if b.RoleRef.Kind != roleKind {
		problems = append(problems, fmt.Sprintf(
			"[%s] %s in %q references kind %q, want %q", renderName, bindingKind, ns, b.RoleRef.Kind, roleKind))
	}
	if b.RoleRef.Name != manifestSA {
		problems = append(problems, fmt.Sprintf(
			"[%s] %s in %q references %q, want %q", renderName, bindingKind, ns, b.RoleRef.Name, manifestSA))
	}
	if len(b.Subjects) != 1 {
		problems = append(problems, fmt.Sprintf(
			"[%s] %s in %q has %d subjects, want 1", renderName, bindingKind, ns, len(b.Subjects)))
		return problems
	}
	subj := b.Subjects[0]
	if subj.Kind != "ServiceAccount" || subj.Name != manifestSA || subj.Namespace != ns {
		problems = append(problems, fmt.Sprintf(
			"[%s] %s in %q subject %s %s/%s, want ServiceAccount %s/%s",
			renderName, bindingKind, ns, subj.Kind, subj.Namespace, subj.Name, ns, manifestSA))
	}
	return problems
}

// withPins returns the manifest rules with the Secret read narrowed to pins, so
// a pinned render compares exactly instead of reading as an extra rule. Empty
// pins return the manifest unchanged, which is the parity case.
func withPins(rules []rbacv1.PolicyRule, pins []string) []rbacv1.PolicyRule {
	if len(pins) == 0 {
		return rules
	}
	out := make([]rbacv1.PolicyRule, len(rules))
	copy(out, rules)
	for i := range out {
		// A rule the manifest already narrowed is left alone: only the
		// unpinned grant is what secretNames is narrowing.
		if has(out[i].APIGroups, "") && has(out[i].Resources, "secrets") &&
			has(out[i].Verbs, "get") && len(out[i].ResourceNames) == 0 {
			out[i].ResourceNames = append([]string(nil), pins...)
		}
	}
	return out
}

// compareRules returns one problem per rule the chart is missing, one per rule
// it grants that the manifest does not, and one when the pinned render does not
// actually pin the Secret read.
func compareRules(renderName string, got, want []rbacv1.PolicyRule, pins []string) []string {
	gotSet := ruleSet(got)
	wantSet := ruleSet(want)

	var problems []string
	for _, k := range sortedKeys(wantSet) {
		if !gotSet[k] {
			problems = append(problems, fmt.Sprintf(
				"[%s] chart rules are missing: %s", renderName, k))
		}
	}
	for _, k := range sortedKeys(gotSet) {
		if !wantSet[k] {
			problems = append(problems, fmt.Sprintf(
				"[%s] chart grants rules the manifest does not: %s", renderName, k))
		}
	}

	// The Secret rule must carry the pin when one is configured, and none
	// otherwise: a silently unpinned render is the cluster-wide read the
	// secretNames option exists to prevent.
	secretRule := findSecretRule(got)
	if len(pins) > 0 {
		if secretRule == nil {
			problems = append(problems, fmt.Sprintf("[%s] no secrets rule to pin", renderName))
		} else if strings.Join(secretRule.ResourceNames, ",") != strings.Join(pins, ",") {
			problems = append(problems, fmt.Sprintf(
				"[%s] secrets rule resourceNames %v, want %v", renderName, secretRule.ResourceNames, pins))
		}
	} else if secretRule != nil && len(secretRule.ResourceNames) > 0 {
		problems = append(problems, fmt.Sprintf(
			"[%s] secrets rule is pinned to %v without metalAgent.rbac.secretNames",
			renderName, secretRule.ResourceNames))
	}
	return problems
}

// findSecretRule returns the rule granting get on core Secrets, which is the
// one secretNames narrows.
func findSecretRule(rules []rbacv1.PolicyRule) *rbacv1.PolicyRule {
	for i := range rules {
		r := &rules[i]
		if has(r.APIGroups, "") && has(r.Resources, "secrets") && has(r.Verbs, "get") {
			return r
		}
	}
	return nil
}

// ruleSet keys each rule by its grants, sorted, so the comparison ignores rule
// order and grouping but not a single verb, resource or resourceName.
func ruleSet(rules []rbacv1.PolicyRule) map[string]bool {
	out := map[string]bool{}
	for _, r := range rules {
		groups := sortedCopy(r.APIGroups)
		resources := sortedCopy(r.Resources)
		verbs := sortedCopy(r.Verbs)
		names := sortedCopy(r.ResourceNames)
		out[fmt.Sprintf("apiGroups=%q resources=%q verbs=%q resourceNames=%q nonResourceURLs=%q",
			groups, resources, verbs, names, sortedCopy(r.NonResourceURLs))] = true
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// findDoc returns the document of the given kind. A namespaced kind must be in
// ns; a cluster-scoped kind must carry no namespace at all.
func findDoc(docs []map[string]any, kind, ns string) map[string]any {
	for _, d := range docs {
		if k, _ := d["kind"].(string); k != kind {
			continue
		}
		meta, _ := d["metadata"].(map[string]any)
		gotNS, _ := meta["namespace"].(string)
		if gotNS == ns {
			return d
		}
	}
	return nil
}

func metaName(doc map[string]any) string {
	meta, _ := doc["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return name
}

// rulesOf extracts the rules from a rendered document tree. The tree comes from
// Helm's parse of a template whose rule comments sit between list items, so it
// is re-parsed into a typed Role/ClusterRole rather than trusted as a list.
func rulesOf(doc map[string]any) ([]rbacv1.PolicyRule, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var role struct {
		Rules []rbacv1.PolicyRule `json:"rules"`
	}
	if err := json.Unmarshal(b, &role); err != nil {
		return nil, err
	}
	return role.Rules, nil
}

// remarshal round-trips a generic document through JSON into a typed object.
func remarshal(from any, to any) error {
	b, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, to)
}

func has(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// splitDocs splits a multi-document YAML stream the way the chart's own tests
// and deployment/macos/metal-agent-rbac.yaml are laid out.
func splitDocs(s string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "---" {
			if strings.TrimSpace(cur.String()) != "" {
				out = append(out, cur.String())
			}
			cur.Reset()
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}
