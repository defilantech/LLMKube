/*
Copyright 2025.

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

package cli

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	federationv1alpha1 "github.com/defilantech/llmkube/api/federation/v1alpha1"
)

// fleetTestScheme builds a scheme with core, rbac, and federation types
// registered, matching what the real CLI client wires up in runFleetRegister.
func fleetTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := federationv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add federation scheme: %v", err)
	}
	return s
}

// stubToken swaps mintFleetToken for a fake that returns token without
// touching a real (or fake) TokenRequest subresource. client-go's fake
// clientset echoes the TokenRequest object back unchanged instead of
// synthesizing a token value, so it can't be used to assert on the minted
// token itself; mintFleetToken is a seam for exactly this reason.
func stubToken(t *testing.T, token string) {
	t.Helper()
	prev := mintFleetToken
	mintFleetToken = func(_ context.Context, _ kubernetes.Interface, _, _ string) (string, error) {
		return token, nil
	}
	t.Cleanup(func() { mintFleetToken = prev })
}

func TestFleetRegisterCreatesFederatedClusterAndScopedRBAC(t *testing.T) {
	stubToken(t, "test-token-123")

	c := fake.NewClientBuilder().WithScheme(fleetTestScheme(t)).Build()
	ctx := context.Background()

	edgeConfig, err := fleetRegister(ctx, c, nil, fleetRegisterInput{
		Name:                     "edge-a",
		ResidencyTier:            "floor-3",
		HeartbeatIntervalSeconds: 45,
		DatacenterEndpoint:       "https://dc.example.com:6443",
		Namespace:                "llmkube-system",
	})
	if err != nil {
		t.Fatalf("fleetRegister: %v", err)
	}

	// FederatedCluster: spec tier + interval.
	fc := &federationv1alpha1.FederatedCluster{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-a"}, fc); err != nil {
		t.Fatalf("get FederatedCluster: %v", err)
	}
	if fc.Spec.DataResidencyTier != "floor-3" {
		t.Errorf("DataResidencyTier = %q, want %q", fc.Spec.DataResidencyTier, "floor-3")
	}
	if fc.Spec.HeartbeatIntervalSeconds != 45 {
		t.Errorf("HeartbeatIntervalSeconds = %d, want 45", fc.Spec.HeartbeatIntervalSeconds)
	}

	// ServiceAccount fedcluster-edge-a in the target namespace.
	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, types.NamespacedName{Name: "fedcluster-edge-a", Namespace: "llmkube-system"}, sa); err != nil {
		t.Fatalf("get ServiceAccount: %v", err)
	}

	// ClusterRole: EXACTLY the least-privilege rules. No list/watch/delete,
	// no other resources, no cluster-wide (unscoped) access.
	cr := &rbacv1.ClusterRole{}
	if err := c.Get(ctx, types.NamespacedName{Name: "fedcluster-edge-a"}, cr); err != nil {
		t.Fatalf("get ClusterRole: %v", err)
	}
	if len(cr.Rules) != 2 {
		t.Fatalf("ClusterRole has %d rules, want exactly 2: %+v", len(cr.Rules), cr.Rules)
	}

	statusRule, listGetRule := findRules(t, cr.Rules)

	assertStringSlice(t, "status rule APIGroups", statusRule.APIGroups, []string{"federation.llmkube.dev"})
	assertStringSlice(t, "status rule Resources", statusRule.Resources, []string{"federatedclusters/status"})
	assertStringSlice(t, "status rule Verbs", statusRule.Verbs, []string{"get", "update", "patch"})
	assertStringSlice(t, "status rule ResourceNames", statusRule.ResourceNames, []string{"edge-a"})

	assertStringSlice(t, "get rule APIGroups", listGetRule.APIGroups, []string{"federation.llmkube.dev"})
	assertStringSlice(t, "get rule Resources", listGetRule.Resources, []string{"federatedclusters"})
	assertStringSlice(t, "get rule Verbs", listGetRule.Verbs, []string{"get"})
	assertStringSlice(t, "get rule ResourceNames", listGetRule.ResourceNames, []string{"edge-a"})

	// ClusterRoleBinding: binds the ClusterRole to the ServiceAccount.
	crb := &rbacv1.ClusterRoleBinding{}
	if err := c.Get(ctx, types.NamespacedName{Name: "fedcluster-edge-a"}, crb); err != nil {
		t.Fatalf("get ClusterRoleBinding: %v", err)
	}
	if crb.RoleRef.Kind != "ClusterRole" || crb.RoleRef.Name != "fedcluster-edge-a" {
		t.Errorf("RoleRef = %+v, want ClusterRole/fedcluster-edge-a", crb.RoleRef)
	}
	if len(crb.Subjects) != 1 {
		t.Fatalf("Subjects = %+v, want exactly 1", crb.Subjects)
	}
	subj := crb.Subjects[0]
	if subj.Kind != rbacv1.ServiceAccountKind || subj.Name != "fedcluster-edge-a" || subj.Namespace != "llmkube-system" {
		t.Errorf("Subject = %+v, want ServiceAccount fedcluster-edge-a/llmkube-system", subj)
	}

	// Returned edge-config snippet: cluster name, datacenter endpoint, and
	// the operator flags a site admin sets.
	for _, want := range []string{
		"edge-a",
		"https://dc.example.com:6443",
		"test-token-123",
		"--federation-role=edge",
		"--federation-cluster-name=edge-a",
		"--federation-datacenter-kubeconfig=",
	} {
		if !strings.Contains(edgeConfig, want) {
			t.Errorf("edge config snippet missing %q:\n%s", want, edgeConfig)
		}
	}
}

// findRules splits a 2-rule ClusterRole into the federatedclusters/status
// rule and the federatedclusters rule, regardless of order, and fails the
// test if the rules don't match that shape.
func findRules(t *testing.T, rules []rbacv1.PolicyRule) (statusRule, getRule rbacv1.PolicyRule) {
	t.Helper()
	var foundStatus, foundGet bool
	for _, r := range rules {
		switch {
		case len(r.Resources) == 1 && r.Resources[0] == "federatedclusters/status":
			statusRule, foundStatus = r, true
		case len(r.Resources) == 1 && r.Resources[0] == "federatedclusters":
			getRule, foundGet = r, true
		default:
			t.Fatalf("unexpected rule with broader/unknown resources: %+v", r)
		}
	}
	if !foundStatus || !foundGet {
		t.Fatalf("rules missing expected shape: %+v", rules)
	}
	return statusRule, getRule
}

func assertStringSlice(t *testing.T, label string, got, want []string) {
	t.Helper()
	gotSorted := append([]string(nil), got...)
	wantSorted := append([]string(nil), want...)
	sort.Strings(gotSorted)
	sort.Strings(wantSorted)
	if len(gotSorted) != len(wantSorted) {
		t.Errorf("%s = %v, want %v", label, got, want)
		return
	}
	for i := range gotSorted {
		if gotSorted[i] != wantSorted[i] {
			t.Errorf("%s = %v, want %v", label, got, want)
			return
		}
	}
}

func TestFleetRegisterDefaultsNamespaceAndInterval(t *testing.T) {
	stubToken(t, "tok")
	c := fake.NewClientBuilder().WithScheme(fleetTestScheme(t)).Build()
	ctx := context.Background()

	if _, err := fleetRegister(ctx, c, nil, fleetRegisterInput{Name: "edge-b"}); err != nil {
		t.Fatalf("fleetRegister: %v", err)
	}

	fc := &federationv1alpha1.FederatedCluster{}
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-b"}, fc); err != nil {
		t.Fatalf("get FederatedCluster: %v", err)
	}
	if fc.Spec.HeartbeatIntervalSeconds != defaultHeartbeatIntervalSeconds {
		t.Errorf("HeartbeatIntervalSeconds = %d, want default %d",
			fc.Spec.HeartbeatIntervalSeconds, defaultHeartbeatIntervalSeconds)
	}

	sa := &corev1.ServiceAccount{}
	saKey := types.NamespacedName{Name: "fedcluster-edge-b", Namespace: defaultFleetNamespace}
	if err := c.Get(ctx, saKey, sa); err != nil {
		t.Fatalf("get ServiceAccount in default namespace %q: %v", defaultFleetNamespace, err)
	}
}

func TestFleetRegisterRequiresName(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(fleetTestScheme(t)).Build()
	if _, err := fleetRegister(context.Background(), c, nil, fleetRegisterInput{}); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestNewFleetCommandWiring(t *testing.T) {
	cmd := NewFleetCommand()
	if cmd.Name() != "fleet" {
		t.Fatalf("Name() = %q, want fleet", cmd.Name())
	}

	var register *cobra.Command
	for _, sub := range cmd.Commands() {
		if sub.Name() == "register" {
			register = sub
			break
		}
	}
	if register == nil {
		t.Fatal("fleet command is missing the register subcommand")
	}

	flag := register.Flags().Lookup("heartbeat-interval")
	if flag == nil {
		t.Fatal("register command is missing --heartbeat-interval flag")
	}
	if flag.DefValue != "30" {
		t.Errorf("--heartbeat-interval default = %q, want 30", flag.DefValue)
	}

	for _, name := range []string{"name", "residency", "datacenter-endpoint"} {
		if register.Flags().Lookup(name) == nil {
			t.Errorf("register command is missing --%s flag", name)
		}
	}
}
