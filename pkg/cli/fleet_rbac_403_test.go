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
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	federationv1alpha1 "github.com/defilantech/llmkube/api/federation/v1alpha1"
)

// TestFleetRegisterScopedTokenRBACEnforcement is a RUNTIME proof of the
// design's least-privilege acceptance criterion ("the scoped token provably
// cannot write another cluster's status"), complementing
// TestFleetRegisterCreatesFederatedClusterAndScopedRBAC's proof BY
// CONSTRUCTION (which asserts the ClusterRole is exactly the two
// resourceNames-scoped rules, using a fake client that never evaluates
// RBAC). This test authenticates as the actual minted ServiceAccount token
// against a real, RBAC-enforcing apiserver and asserts on the resulting
// Allow/Forbid decisions.
//
// It runs its own *envtest.Environment, entirely separate from
// internal/controller/suite_test.go, so that suite's admin-access assumption
// is never touched by anything here.
//
// Why a naive token client would wrongly succeed without care: envtest's
// apiserver already defaults --authorization-mode=RBAC (see
// sigs.k8s.io/controller-runtime/pkg/internal/testing/controlplane/
// apiserver.go, defaultArgs()); RBAC is not something this test has to turn
// on. The actual trap is that the *rest.Config envtest.Start() returns
// authenticates via a client certificate whose group is "system:masters",
// which bypasses RBAC entirely (kube-apiserver treats system:masters as
// superuser). So the load-bearing step here is scopedRestConfig: build a
// SEPARATE config carrying ONLY the minted ServiceAccount bearer token, with
// every client-cert field stripped so the request can't ride the admin
// identity in through mutual TLS.
func TestFleetRegisterScopedTokenRBACEnforcement(t *testing.T) {
	binDir := envtestBinaryDir(t)

	testScheme := fleetTestScheme(t)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: binDir,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest environment: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest environment: %v", err)
		}
	})

	adminClient, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("build admin client: %v", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build clientset: %v", err)
	}

	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultFleetNamespace}}
	if err := adminClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace %q: %v", defaultFleetNamespace, err)
	}

	// Register edge-a and edge-b through the REAL production path
	// (fleetRegister, unmodified), so the ClusterRole this test exercises is
	// the exact one fleet.go ships, not a hand-rolled lookalike.
	if _, err := fleetRegister(ctx, adminClient, kube, fleetRegisterInput{Name: "edge-a"}); err != nil {
		t.Fatalf("fleetRegister(edge-a): %v", err)
	}
	// edge-b only needs to exist as a FederatedCluster that edge-a's token
	// must NOT be able to touch; its own RBAC/token are irrelevant here.
	if _, err := fleetRegister(ctx, adminClient, kube, fleetRegisterInput{Name: "edge-b"}); err != nil {
		t.Fatalf("fleetRegister(edge-b): %v", err)
	}

	// Mint a fresh token for edge-a's ServiceAccount via the same production
	// seam fleetRegister just used (mintFleetToken, unstubbed here), rather
	// than parsing one out of the printed edge-config snippet.
	token, _, err := mintFleetToken(ctx, kube, defaultFleetNamespace, fleetServiceAccountPrefix+"edge-a")
	if err != nil {
		t.Fatalf("mint token for edge-a's ServiceAccount: %v", err)
	}

	scopedClient, err := client.New(scopedRestConfig(cfg, token), client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("build scoped client: %v", err)
	}

	// --- (1) OWN status patch: allowed. ---
	edgeA := &federationv1alpha1.FederatedCluster{}
	if err := scopedClient.Get(ctx, types.NamespacedName{Name: "edge-a"}, edgeA); err != nil {
		t.Fatalf("scoped client get edge-a (allowed by the `get` rule on federatedclusters): %v", err)
	}
	patchOwnStatus := client.MergeFrom(edgeA.DeepCopy())
	edgeA.Status.ObservedVersion = "rbac-403-test"
	if err := scopedClient.Status().Patch(ctx, edgeA, patchOwnStatus); err != nil {
		t.Errorf("scoped client patch OWN (edge-a) status: want success, got error: %v", err)
	}

	// --- (2) OTHER cluster's status patch: forbidden. ---
	edgeB := &federationv1alpha1.FederatedCluster{}
	if err := adminClient.Get(ctx, types.NamespacedName{Name: "edge-b"}, edgeB); err != nil {
		t.Fatalf("admin client get edge-b: %v", err)
	}
	patchOtherStatus := client.MergeFrom(edgeB.DeepCopy())
	edgeB.Status.ObservedVersion = "rbac-403-test"
	err = scopedClient.Status().Patch(ctx, edgeB, patchOtherStatus)
	assertForbidden(t, err, "scoped client patch OTHER (edge-b) status")

	// --- (3) list federatedclusters: forbidden (no list verb granted). ---
	list := &federationv1alpha1.FederatedClusterList{}
	err = scopedClient.List(ctx, list)
	assertForbidden(t, err, "scoped client list FederatedClusters")

	// --- (4) OWN spec patch: forbidden (only the status subresource is
	// get/update/patch-able; the base resource's only granted verb is get). ---
	edgeASpec := &federationv1alpha1.FederatedCluster{}
	if err := adminClient.Get(ctx, types.NamespacedName{Name: "edge-a"}, edgeASpec); err != nil {
		t.Fatalf("admin client get edge-a for spec patch: %v", err)
	}
	patchOwnSpec := client.MergeFrom(edgeASpec.DeepCopy())
	edgeASpec.Spec.DisplayName = "renamed-by-rbac-403-test"
	err = scopedClient.Patch(ctx, edgeASpec, patchOwnSpec)
	assertForbidden(t, err, "scoped client patch OWN (edge-a) spec")
}

// assertForbidden fails the test unless err is a genuine RBAC-authorizer
// 403, i.e. apierrors.IsForbidden(err). A nil error, or any other error
// shape (NotFound, connection failure, etc.), means the assertion didn't
// actually prove what it claims to prove.
func assertForbidden(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: want Forbidden, got success", what)
		return
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("%s: want apierrors.IsForbidden, got %v (%T)", what, err, err)
	}
}

// scopedRestConfig returns a copy of cfg (envtest's admin config)
// re-authenticated as ONLY the given bearer token. It strips every
// client-certificate and other identity field: if a client cert were left
// in place, the request would authenticate via mutual TLS as the cert's
// identity (system:masters, for envtest's admin config) rather than the
// token, silently defeating the entire point of this test by letting the
// "scoped" client ride in on the admin identity.
func scopedRestConfig(cfg *rest.Config, token string) *rest.Config {
	scoped := rest.CopyConfig(cfg)
	scoped.BearerToken = token
	scoped.BearerTokenFile = ""
	scoped.Username = ""
	scoped.Password = ""
	scoped.CertData = nil
	scoped.CertFile = ""
	scoped.KeyData = nil
	scoped.KeyFile = ""
	scoped.AuthProvider = nil
	scoped.ExecProvider = nil
	return scoped
}

// envtestBinaryDir locates the envtest KUBEBUILDER_ASSETS directory: the env
// var `make test` exports, or (for ad hoc/IDE runs, mirroring
// internal/controller/suite_test.go's getFirstFoundEnvTestBinaryDir) the
// first entry under ../../bin/k8s. Skips (rather than fails) when neither is
// present, so `go test ./pkg/cli/...` run without `make envtest` first
// doesn't hard-fail the whole package.
func envtestBinaryDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	matches, err := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*"))
	if err != nil || len(matches) == 0 {
		t.Skip("no envtest binaries found: run `make envtest` first (see internal/controller/suite_test.go)")
	}
	return matches[0]
}
