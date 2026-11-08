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

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// writePolicyName is the policy and binding name in
// deployment/macos/metal-agent-write-policy.yaml.
const writePolicyName = "llmkube-metal-agent-writes"

// labelPolicyName is the policy and binding name in
// deployment/macos/metal-agent-label-policy.yaml.
const labelPolicyName = "llmkube-metal-agent-labels"

// writePolicyEnv is a real API server with the raw write policy and the
// shared metal-agent Role applied, and clients acting as a per-Mac agent
// ServiceAccount (matched by the policy) and as an unrelated ServiceAccount
// bound to the same Role (not matched).
type writePolicyEnv struct {
	cfg       *rest.Config
	scheme    *runtime.Scheme
	admin     client.Client
	agent     client.Client // system:serviceaccount:macs:llmkube-metal-test-mac
	shared    client.Client // system:serviceaccount:macs:llmkube-metal-agent
	other     client.Client // system:serviceaccount:macs:other-sa
	namespace string
}

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	// pkg/agent/<file> -> repo root is two directories up.
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(thisFile))), rel)
}

// yamlDocs splits a manifest into its non-empty documents.
// decodeManifest strictly decodes every document of the repo file rel into
// the type its kind names.
func decodeManifest(t *testing.T, rel string, kinds map[string]func() client.Object) []client.Object {
	t.Helper()
	raw, err := os.ReadFile(repoFile(t, rel))
	if err != nil {
		t.Fatal(err)
	}
	return decodeObjects(t, rel, raw, kinds)
}

// decodeObjects strictly decodes every document of a YAML stream into the
// type its kind names, so a typo in a field fails here instead of being
// dropped. Documents without a kind (comments only) are skipped.
func decodeObjects(t *testing.T, source string, raw []byte, kinds map[string]func() client.Object) []client.Object {
	t.Helper()
	docs := strings.Split(string(raw), "\n---")
	out := make([]client.Object, 0, len(docs))
	for _, doc := range docs {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		if head.Kind == "" {
			continue
		}
		mk, ok := kinds[head.Kind]
		if !ok {
			t.Fatalf("%s: unexpected kind %q", source, head.Kind)
		}
		obj := mk()
		if err := yaml.UnmarshalStrict([]byte(doc), obj); err != nil {
			t.Fatalf("%s: decode %s: %v", source, head.Kind, err)
		}
		out = append(out, obj)
	}
	return out
}

// policyKinds are the documents of a write policy stream.
var policyKinds = map[string]func() client.Object{
	"ValidatingAdmissionPolicy": func() client.Object {
		return &admissionregistrationv1.ValidatingAdmissionPolicy{}
	},
	"ValidatingAdmissionPolicyBinding": func() client.Object {
		return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	},
}

func newWritePolicyEnv(t *testing.T) *writePolicyEnv {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		matches, _ := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*"))
		if len(matches) == 0 {
			t.Skip("no envtest binaries: run `make setup-envtest` or set KUBEBUILDER_ASSETS")
		}
		assets = matches[0]
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{repoFile(t, "config/crd/bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	e := &writePolicyEnv{cfg: cfg, scheme: scheme, admin: admin, namespace: "macs"}
	ctx := context.Background()

	mustCreate := func(obj client.Object) {
		t.Helper()
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
		}
	}
	mustCreate(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: e.namespace}})

	// The shared identity exactly as deployment/macos/metal-agent-rbac.yaml
	// ships it: ServiceAccount, Role and RoleBinding llmkube-metal-agent.
	for _, obj := range decodeManifest(t, "deployment/macos/metal-agent-rbac.yaml", map[string]func() client.Object{
		"ServiceAccount": func() client.Object { return &corev1.ServiceAccount{} },
		"Role":           func() client.Object { return &rbacv1.Role{} },
		"RoleBinding":    func() client.Object { return &rbacv1.RoleBinding{} },
	}) {
		obj.SetNamespace(e.namespace)
		if rb, ok := obj.(*rbacv1.RoleBinding); ok {
			for i := range rb.Subjects {
				rb.Subjects[i].Namespace = e.namespace
			}
		}
		mustCreate(obj)
	}

	// A per-Mac identity bound to the shared Role, as `llmkube metal token
	// create` binds it, and an unrelated ServiceAccount with the same Role.
	for _, sa := range []string{"llmkube-metal-test-mac", "other-sa"} {
		mustCreate(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: sa}})
		mustCreate(&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: sa + "-shared"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "llmkube-metal-agent"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: e.namespace, Name: sa}},
		})
	}

	for _, manifest := range []string{
		"deployment/macos/metal-agent-write-policy.yaml",
		"deployment/macos/metal-agent-label-policy.yaml",
	} {
		for _, obj := range decodeManifest(t, manifest, policyKinds) {
			mustCreate(obj)
		}
	}

	e.agent = e.clientAs(t, "llmkube-metal-test-mac")
	e.shared = e.clientAs(t, "llmkube-metal-agent")
	e.other = e.clientAs(t, "other-sa")

	// The API server loads policies asynchronously: wait until a write the
	// policy denies is denied. Dry runs, so nothing changes while the policy
	// is not yet in force. Three probes, one per validation and one for the
	// label policy, and all must pass, so a test run with either validation
	// removed still gets past this wait and fails on the case that names it,
	// and a run whose label policy has not loaded does not report a forged
	// write as admitted.
	mustCreate(e.service("load-probe", nil))
	err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			create := e.service("load-probe-new", nil)
			if !policyDenied(e.agent.Create(ctx, create, client.DryRunAll)) {
				return false, nil
			}
			update := e.service("load-probe", nil)
			if err := e.agent.Get(ctx, client.ObjectKeyFromObject(update), update); err != nil {
				return false, nil
			}
			update.Labels = managedLabels("load-probe")
			if !policyDenied(e.agent.Update(ctx, update, client.DryRunAll)) {
				return false, nil
			}
			// The label policy is a second object and loads on its own
			// schedule: an unrelated identity writing a labeled object is
			// admitted until it is in force.
			labeled := e.service("load-probe-labeled", managedLabels("load-probe-labeled"))
			return deniedBy(e.other.Create(ctx, labeled, client.DryRunAll), labelPolicyName), nil
		})
	if err != nil {
		t.Fatalf("the write or label policy never denied its probe write: %v", err)
	}
	return e
}

// clientAs mints a token for ServiceAccount sa and returns a client that
// authenticates with it only.
func (e *writePolicyEnv) clientAs(t *testing.T, sa string) client.Client {
	t.Helper()
	seconds := int64(3600)
	tr := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}
	obj := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: sa}}
	if err := e.admin.SubResource("token").Create(context.Background(), obj, tr); err != nil {
		t.Fatalf("mint token for %s: %v", sa, err)
	}
	c := rest.AnonymousClientConfig(e.cfg)
	c.BearerToken = tr.Status.Token
	cl, err := client.New(c, client.Options{Scheme: e.scheme})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// policyDenied reports whether err is the raw policy's denial, not an RBAC
// refusal or any other error.
func policyDenied(err error) bool {
	return deniedBy(err, writePolicyName)
}

func deniedBy(err error, policy string) bool {
	return err != nil && apierrors.IsForbidden(err) &&
		strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+policy+"'")
}

func managedLabels(isvc string) map[string]string {
	return map[string]string{
		"app":                          isvc,
		managedByLabel:                 managedByValue,
		"llmkube.ai/inference-service": isvc,
	}
}

func (e *writePolicyEnv) service(name string, labels map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: name, Labels: labels},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{Name: "http", Port: 8080, TargetPort: intstr.FromInt(9000)}},
		},
	}
}

func (e *writePolicyEnv) slice(name string, labels map[string]string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: e.namespace, Name: name, Labels: labels},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.5"}}},
	}
}

//nolint:staticcheck // SA1019: the agent still reaps legacy core/v1 Endpoints.
func (e *writePolicyEnv) endpoints(name string, labels map[string]string) *corev1.Endpoints {
	return &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: name, Labels: labels}}
}

// isvcOwnerRef is the controller ownerReference the LLMKube controller puts
// on the Service it writes (setControllerReferenceUnblocked).
func isvcOwnerRef(name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: inferencev1alpha1.GroupVersion.String(), Kind: "InferenceService",
		Name: name, UID: uid, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(false),
	}
}

// seed creates obj outside the policies, standing up a fixture the case under
// test then acts on: as the admin when it carries no metal-agent label, and
// with the agent's username when it does, since llmkube-metal-agent-labels
// admits a labeled object only from a llmkube-metal-* ServiceAccount
// (GHSA-863w-vh42-mjhj).
func (e *writePolicyEnv) seed(t *testing.T, obj client.Object) {
	t.Helper()
	if obj.GetLabels()[managedByLabel] == managedByValue {
		e.seedAsAgent(t, obj)
		return
	}
	if err := e.admin.Create(context.Background(), obj); err != nil {
		t.Fatalf("seed %T %s: %v", obj, obj.GetName(), err)
	}
}

// labelControllerUser is the controller identity the standalone label
// manifest exempts: the kustomize install's controller ServiceAccount.
const labelControllerUser = "system:serviceaccount:llmkube-system:llmkube-controller-manager"

// clientAsUsername returns a client impersonating username with admin
// rights, for an identity that has no token in this environment (the
// controller's own ServiceAccount).
func (e *writePolicyEnv) clientAsUsername(t *testing.T, username string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(e.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: username, Groups: []string{"system:masters"}}
	c, err := client.New(cfg, client.Options{Scheme: e.scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// seedAsAgent creates obj carrying the agent's username, for a labeled object
// the agent's Role cannot create itself: it holds only get and delete on
// legacy core/v1 Endpoints. The write is authorized as admin but names the
// agent, which is what llmkube-metal-agent-labels reads, so the seed stands
// up the fixture without touching the Role under test.
func (e *writePolicyEnv) seedAsAgent(t *testing.T, obj client.Object) {
	t.Helper()
	cfg := rest.CopyConfig(e.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + e.namespace + ":llmkube-metal-test-mac",
		Groups:   []string{"system:masters"},
	}
	c, err := client.New(cfg, client.Options{Scheme: e.scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatalf("seed %T %s as the agent: %v", obj, obj.GetName(), err)
	}
}

// refetch reads obj back through c so the next write carries the current
// resourceVersion.
func refetch(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatalf("get %T %s: %v", obj, obj.GetName(), err)
	}
}

func wantAllowed(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: want allowed, got %v", what, err)
	}
}

func wantDenied(t *testing.T, what string, err error) {
	t.Helper()
	if !policyDenied(err) {
		t.Fatalf("%s: want denied by %s, got %v", what, writePolicyName, err)
	}
}

func (e *writePolicyEnv) exists(t *testing.T, obj client.Object) bool {
	t.Helper()
	err := e.admin.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// TestWritePolicy_Envtest applies deployment/macos/metal-agent-write-policy.yaml
// and the Role from metal-agent-rbac.yaml to a real API server and checks
// what an agent ServiceAccount may write (#2081), first with direct API
// calls, then through the agent's own registry code.
func TestWritePolicy_Envtest(t *testing.T) {
	e := newWritePolicyEnv(t)
	ctx := context.Background()

	t.Run("labeled Service create, update and delete are allowed", func(t *testing.T) {
		svc := e.service("labeled-svc", managedLabels("labeled-svc"))
		wantAllowed(t, "create", e.agent.Create(ctx, svc))
		svc.Annotations = map[string]string{"llmkube.ai/native-process": "true"}
		wantAllowed(t, "update", e.agent.Update(ctx, svc))
		wantAllowed(t, "delete", e.agent.Delete(ctx, svc))
	})

	t.Run("labeled EndpointSlice create, update and delete are allowed", func(t *testing.T) {
		sl := e.slice("labeled-slice", managedLabels("labeled-slice"))
		wantAllowed(t, "create", e.agent.Create(ctx, sl))
		sl.Endpoints[0].Addresses = []string{"10.0.0.6"}
		wantAllowed(t, "update", e.agent.Update(ctx, sl))
		wantAllowed(t, "delete", e.agent.Delete(ctx, sl))
	})

	t.Run("unlabeled create is denied", func(t *testing.T) {
		wantDenied(t, "Service create", e.agent.Create(ctx, e.service("unlabeled-svc", map[string]string{"app": "x"})))
		wantDenied(t, "EndpointSlice create", e.agent.Create(ctx, e.slice("unlabeled-slice", nil)))
		wrong := managedLabels("wrong-value")
		wrong[managedByLabel] = "someone-else"
		wantDenied(t, "Service create with another managed-by value", e.agent.Create(ctx, e.service("wrong-value", wrong)))
		if e.exists(t, e.service("unlabeled-svc", nil)) {
			t.Fatal("the denied Service was created")
		}
	})

	t.Run("the shared llmkube-metal-agent ServiceAccount is held to the policy too", func(t *testing.T) {
		wantDenied(t, "Service create", e.shared.Create(ctx, e.service("shared-unlabeled", nil)))
		wantAllowed(t, "labeled Service create", e.shared.Create(ctx, e.service("shared-labeled", managedLabels("s"))))
	})

	t.Run("update of an unlabeled Service is denied, even when it adds the label", func(t *testing.T) {
		svc := e.service("foreign-svc", map[string]string{"app": "foreign"})
		e.seed(t, svc)
		refetch(t, e.agent, svc)
		svc.Labels = managedLabels("foreign-svc")
		wantDenied(t, "update", e.agent.Update(ctx, svc))

		sl := e.slice("foreign-slice", nil)
		e.seed(t, sl)
		refetch(t, e.agent, sl)
		sl.Labels = managedLabels("foreign-slice")
		wantDenied(t, "EndpointSlice update", e.agent.Update(ctx, sl))
	})

	t.Run("update of an unlabeled Service an InferenceService controls is allowed with the label", func(t *testing.T) {
		svc := e.service("adopt-svc", map[string]string{"app": "adopt-svc"})
		svc.OwnerReferences = []metav1.OwnerReference{isvcOwnerRef("adopt-svc", "uid-adopt")}
		e.seed(t, svc)

		refetch(t, e.agent, svc)
		svc.Annotations = map[string]string{"touched": "true"}
		wantDenied(t, "update that leaves the label off", e.agent.Update(ctx, svc))

		refetch(t, e.agent, svc)
		svc.Labels = managedLabels("adopt-svc")
		wantAllowed(t, "update that adds the label", e.agent.Update(ctx, svc))
	})

	t.Run("the adoption exception needs a controller ownerReference to an InferenceService", func(t *testing.T) {
		notController := isvcOwnerRef("nc", "uid-nc")
		notController.Controller = ptr.To(false)
		otherKind := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d",
			UID: "uid-d", Controller: ptr.To(true)}
		otherGroup := isvcOwnerRef("og", "uid-og")
		otherGroup.APIVersion = "example.com/v1"
		for name, ref := range map[string]metav1.OwnerReference{
			"not-controller": notController, "other-kind": otherKind, "other-group": otherGroup,
		} {
			svc := e.service("ref-"+name, map[string]string{"app": name})
			svc.OwnerReferences = []metav1.OwnerReference{ref}
			e.seed(t, svc)
			refetch(t, e.agent, svc)
			svc.Labels = managedLabels(name)
			wantDenied(t, name, e.agent.Update(ctx, svc))
		}
	})

	t.Run("the adoption exception covers Services only", func(t *testing.T) {
		sl := e.slice("owned-slice", nil)
		sl.OwnerReferences = []metav1.OwnerReference{isvcOwnerRef("owned-slice", "uid-os")}
		e.seed(t, sl)
		refetch(t, e.agent, sl)
		sl.Labels = managedLabels("owned-slice")
		wantDenied(t, "EndpointSlice update", e.agent.Update(ctx, sl))
	})

	t.Run("delete of an unlabeled Service is denied, even when an InferenceService controls it", func(t *testing.T) {
		plain := e.service("del-plain", map[string]string{"app": "x"})
		e.seed(t, plain)
		wantDenied(t, "delete", e.agent.Delete(ctx, plain))

		owned := e.service("del-owned", map[string]string{"app": "x"})
		owned.OwnerReferences = []metav1.OwnerReference{isvcOwnerRef("del-owned", "uid-del")}
		e.seed(t, owned)
		wantDenied(t, "delete of a controller-owned Service", e.agent.Delete(ctx, owned))

		sl := e.slice("del-slice", nil)
		e.seed(t, sl)
		wantDenied(t, "EndpointSlice delete", e.agent.Delete(ctx, sl))
		for _, obj := range []client.Object{plain, owned, sl} {
			if !e.exists(t, obj) {
				t.Fatalf("%s was deleted", obj.GetName())
			}
		}
	})

	t.Run("stripping the label on update is denied", func(t *testing.T) {
		svc := e.service("strip-svc", managedLabels("strip-svc"))
		wantAllowed(t, "create", e.agent.Create(ctx, svc))
		svc.Labels = map[string]string{"app": "strip-svc"}
		wantDenied(t, "Service update", e.agent.Update(ctx, svc))

		owned := e.service("strip-owned", managedLabels("strip-owned"))
		owned.OwnerReferences = []metav1.OwnerReference{isvcOwnerRef("strip-owned", "uid-strip")}
		e.seed(t, owned)
		refetch(t, e.agent, owned)
		delete(owned.Labels, managedByLabel)
		wantDenied(t, "update of a controller-owned Service", e.agent.Update(ctx, owned))

		sl := e.slice("strip-slice", managedLabels("strip-slice"))
		wantAllowed(t, "create", e.agent.Create(ctx, sl))
		sl.Labels = nil
		wantDenied(t, "EndpointSlice update", e.agent.Update(ctx, sl))
	})

	t.Run("core Endpoints delete: unlabeled denied, labeled allowed", func(t *testing.T) {
		unlabeled := e.endpoints("ep-plain", nil)
		e.seed(t, unlabeled)
		wantDenied(t, "unlabeled delete", e.agent.Delete(ctx, unlabeled))
		labeled := e.endpoints("ep-agent", managedLabels("ep-agent"))
		e.seed(t, labeled)
		wantAllowed(t, "labeled delete", e.agent.Delete(ctx, labeled))
	})

	t.Run("a ServiceAccount outside the llmkube-metal- prefix is not affected", func(t *testing.T) {
		svc := e.service("other-svc", map[string]string{"app": "other"})
		wantAllowed(t, "unlabeled create", e.other.Create(ctx, svc))
		svc.Annotations = map[string]string{"touched": "true"}
		wantAllowed(t, "unlabeled update", e.other.Update(ctx, svc))
		wantAllowed(t, "unlabeled delete", e.other.Delete(ctx, svc))
		sl := e.slice("other-slice", nil)
		wantAllowed(t, "unlabeled EndpointSlice create", e.other.Create(ctx, sl))
		wantAllowed(t, "unlabeled EndpointSlice delete", e.other.Delete(ctx, sl))
	})

	t.Run("the agent's registry", func(t *testing.T) {
		testRegistryUnderWritePolicy(t, e)
	})

	// Last: it replaces the raw policy's binding with the chart's.
	t.Run("the chart's copy exempts the controller of a llmkube-metal- release", func(t *testing.T) {
		testChartWritePolicy(t, e)
	})
}

// testChartWritePolicy renders the chart's policy for a release named
// llmkube-metal-prod, whose controller ServiceAccount
// llmkube-metal-prod-controller-manager matches the agent prefix, and checks
// that the chart's exclusion lets that controller write unlabeled Services
// while the agent stays held to the rule. make check-metal-agent-rbac keeps
// the rest of the chart's policy identical to the raw manifest's.
func testChartWritePolicy(t *testing.T, e *writePolicyEnv) {
	helmBin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not on PATH")
	}
	const release, chartPolicy = "llmkube-metal-prod", "llmkube-metal-prod-metal-agent-writes"
	ctx := context.Background()
	cmd := exec.Command(helmBin, "template", release, repoFile(t, "charts/llmkube"),
		"--namespace", e.namespace,
		"--show-only", "templates/metal-agent-write-policy.yaml",
		"--api-versions", "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy")
	cmd.Stderr = os.Stderr
	rendered, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}

	// The raw policy has no exclusion, so while it is bound it denies this
	// release's controller too: a Helm install uses the chart's copy only.
	rawBinding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: writePolicyName}}
	if err := e.admin.Delete(ctx, rawBinding); err != nil {
		t.Fatalf("unbind the raw policy: %v", err)
	}
	for _, obj := range decodeObjects(t, "rendered chart policy", rendered, policyKinds) {
		e.seed(t, obj)
	}

	controllerSA := release + "-controller-manager"
	e.seed(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: controllerSA}})
	e.seed(t, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: controllerSA},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "llmkube-metal-agent"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: e.namespace, Name: controllerSA}},
	})
	controller := e.clientAs(t, controllerSA)

	// Both changes load asynchronously: wait for the chart's policy to deny
	// the agent and for the raw one to stop denying the controller.
	err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true,
		func(ctx context.Context) (bool, error) {
			agentErr := e.agent.Create(ctx, e.service("chart-probe", nil), client.DryRunAll)
			ctrlErr := controller.Create(ctx, e.service("chart-probe", nil), client.DryRunAll)
			return deniedBy(agentErr, chartPolicy) && ctrlErr == nil, nil
		})
	if err != nil {
		agentErr := e.agent.Create(ctx, e.service("chart-probe", nil), client.DryRunAll)
		ctrlErr := controller.Create(ctx, e.service("chart-probe", nil), client.DryRunAll)
		t.Fatalf("want the agent denied by %s and the controller allowed; agent: %v; controller: %v",
			chartPolicy, agentErr, ctrlErr)
	}

	svc := e.service("controller-svc", map[string]string{"app": "pod-backed"})
	wantAllowed(t, "controller unlabeled create", controller.Create(ctx, svc))
	delete(svc.Labels, "app")
	wantAllowed(t, "controller unlabeled update", controller.Update(ctx, svc))
	wantAllowed(t, "controller unlabeled delete", controller.Delete(ctx, svc))
	if err := e.agent.Create(ctx, e.service("agent-under-chart", nil)); !deniedBy(err, chartPolicy) {
		t.Fatalf("agent unlabeled create under the chart's policy: want denied by %s, got %v", chartPolicy, err)
	}
}

// newISVC creates an InferenceService as the admin so the registry has a real
// UID to match ownerReferences against and its InferenceService lookups find
// it.
func (e *writePolicyEnv) newISVC(t *testing.T, name string) *inferencev1alpha1.InferenceService {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: name},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: name + "-model", Replicas: ptr.To(int32(1))},
	}
	e.seed(t, isvc)
	return isvc
}

// testRegistryUnderWritePolicy drives the agent's real ServiceRegistry through
// a client authenticated as the per-Mac agent ServiceAccount: everything it
// legitimately writes must get through the policy.
func testRegistryUnderWritePolicy(t *testing.T, e *writePolicyEnv) {
	ctx := context.Background()
	newRegistry := func(relay bool) *ServiceRegistry {
		r := NewServiceRegistry(e.agent, "10.0.0.5", newNopLogger(), "v-test")
		if relay {
			pin := sha256.Sum256([]byte("ingress key"))
			if err := r.EnableIngress(9443, base64.StdEncoding.EncodeToString(pin[:])); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	svcKey := func(name string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: name}}
	}
	sliceKey := func(name string) *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: name}}
	}
	wantPresent := func(t *testing.T, objs ...client.Object) {
		t.Helper()
		for _, obj := range objs {
			if !e.exists(t, obj) {
				t.Fatalf("%T %s missing", obj, obj.GetName())
			}
		}
	}
	wantGone := func(t *testing.T, objs ...client.Object) {
		t.Helper()
		for _, obj := range objs {
			if e.exists(t, obj) {
				t.Fatalf("%T %s still exists", obj, obj.GetName())
			}
		}
	}

	t.Run("legacy register, withdraw, re-register and unregister", func(t *testing.T) {
		isvc := e.newISVC(t, "legacy.model")
		// A legacy core/v1 Endpoints left by an older agent is reaped on
		// registration; that delete goes through the policy too.
		legacyEP := e.endpoints("legacy-model", managedLabels(isvc.Name))
		e.seed(t, legacyEP)

		r := newRegistry(false)
		wantAllowed(t, "RegisterEndpoint", r.RegisterEndpoint(ctx, isvc, 9000))
		wantPresent(t, svcKey("legacy-model"), sliceKey("legacy-model"))
		wantGone(t, legacyEP)
		wantAllowed(t, "WithdrawEndpoint", r.WithdrawEndpoint(ctx, isvc, 9000))
		wantAllowed(t, "RegisterEndpoint again", r.RegisterEndpoint(ctx, isvc, 9001))
		wantAllowed(t, "UnregisterEndpoint", r.UnregisterEndpoint(ctx, isvc.Namespace, isvc.Name))
		wantGone(t, svcKey("legacy-model"), sliceKey("legacy-model"))
	})

	t.Run("relay register removes the legacy slice, then unregister", func(t *testing.T) {
		isvc := e.newISVC(t, "relayed")
		wantAllowed(t, "legacy RegisterEndpoint", newRegistry(false).RegisterEndpoint(ctx, isvc, 9000))

		r := newRegistry(true)
		wantAllowed(t, "relay RegisterEndpoint", r.RegisterEndpoint(ctx, isvc, 9000))
		wantPresent(t, svcKey("relayed-agent"), sliceKey("relayed-agent"))
		// removeLegacySlice deleted the agent's own "<isvc>" slice.
		wantGone(t, sliceKey("relayed"))
		ok, err := r.WithdrawEndpointIfPresent(ctx, isvc)
		wantAllowed(t, "WithdrawEndpointIfPresent", err)
		if !ok {
			t.Fatal("WithdrawEndpointIfPresent wrote nothing")
		}
		wantAllowed(t, "UnregisterEndpoint", r.UnregisterEndpoint(ctx, isvc.Namespace, isvc.Name))
		wantGone(t, svcKey("relayed-agent"), sliceKey("relayed-agent"))
	})

	t.Run("orphan sweep and relay-registration cleanup delete through the policy", func(t *testing.T) {
		gone := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Namespace: e.namespace, Name: "deleted-isvc", UID: "uid-gone"}}
		wantAllowed(t, "register for a missing InferenceService",
			newRegistry(false).RegisterEndpoint(ctx, gone, 9000))
		n, err := newRegistry(false).ReconcileOrphanEndpoints(ctx, e.namespace)
		wantAllowed(t, "ReconcileOrphanEndpoints", err)
		if n < 1 {
			t.Fatalf("ReconcileOrphanEndpoints deleted %d Services, want the orphan", n)
		}
		wantGone(t, svcKey("deleted-isvc"), sliceKey("deleted-isvc"))

		live := e.newISVC(t, "switched")
		wantAllowed(t, "relay register", newRegistry(true).RegisterEndpoint(ctx, live, 9000))
		removed, err := newRegistry(false).RemoveRelayRegistrations(ctx, e.namespace,
			func(context.Context, *inferencev1alpha1.InferenceService) bool { return true })
		wantAllowed(t, "RemoveRelayRegistrations", err)
		if removed != 1 {
			t.Fatalf("RemoveRelayRegistrations removed %d, want 1", removed)
		}
		wantGone(t, svcKey("switched-agent"), sliceKey("switched-agent"))
	})

	t.Run("legacy mode takes over the controller's Service after a CUDA to Metal switch", func(t *testing.T) {
		isvc := e.newISVC(t, "migrated")
		// The Service constructService wrote while the Model was CUDA: no
		// managed-by label, a selector, and the controller ownerReference.
		cuda := e.service("migrated", map[string]string{"app": "migrated", "inference.llmkube.dev/service": "migrated"})
		cuda.Spec.Selector = map[string]string{"app": "migrated", "inference.llmkube.dev/service": "migrated"}
		cuda.OwnerReferences = []metav1.OwnerReference{isvcOwnerRef(isvc.Name, isvc.UID)}
		e.seed(t, cuda)

		wantAllowed(t, "RegisterEndpoint", newRegistry(false).RegisterEndpoint(ctx, isvc, 9000))
		got := svcKey("migrated")
		refetch(t, e.admin, got)
		if !ownedByAgent(got.Labels) || got.Spec.Selector != nil {
			t.Fatalf("Service not taken over: labels %v, selector %v", got.Labels, got.Spec.Selector)
		}
		if ref := metav1.GetControllerOf(got); ref == nil || ref.UID != isvc.UID {
			t.Fatalf("controller ownerReference = %+v, want the InferenceService's kept", ref)
		}
	})

	t.Run("legacy mode takes the Service back after the relay hand-back", func(t *testing.T) {
		isvc := e.newISVC(t, "handback")
		wantAllowed(t, "legacy RegisterEndpoint", newRegistry(false).RegisterEndpoint(ctx, isvc, 9000))

		// Reproduce what the controller leaves on "<isvc>". Relay adoption
		// (reconcileRelayService, internal/controller/metal_relay.go) sets
		// itself as controller owner with BlockOwnerDeletion false, deletes
		// the managed-by label, merges constructService's labels and points
		// the selector and target port at the relay pods. teardownRelay only
		// deletes the relay Deployment; the controller test "rollback hands
		// the Service back" asserts the Service's resourceVersion is unchanged
		// by it. So the agent next sees exactly this object.
		svc := svcKey("handback")
		refetch(t, e.admin, svc)
		scheme := e.admin.Scheme()
		if err := controllerutil.SetControllerReference(isvc, svc, scheme); err != nil {
			t.Fatal(err)
		}
		for i := range svc.OwnerReferences {
			svc.OwnerReferences[i].BlockOwnerDeletion = ptr.To(false)
		}
		delete(svc.Labels, managedByLabel)
		svc.Labels["inference.llmkube.dev/service"] = isvc.Name
		svc.Spec.Selector = map[string]string{"app": "handback-relay", "inference.llmkube.dev/metal-relay": isvc.Name}
		for i := range svc.Spec.Ports {
			svc.Spec.Ports[i].TargetPort = intstr.FromInt(8080)
		}
		if err := e.admin.Update(ctx, svc); err != nil {
			t.Fatalf("reproduce the controller's adoption: %v", err)
		}
		if ownedByAgent(svc.Labels) {
			t.Fatal("setup: the handed-back Service still carries the managed-by label")
		}

		wantAllowed(t, "RegisterEndpoint after hand-back", newRegistry(false).RegisterEndpoint(ctx, isvc, 9000))
		refetch(t, e.admin, svc)
		if !ownedByAgent(svc.Labels) || svc.Spec.Selector != nil {
			t.Fatalf("Service not taken back: labels %v, selector %v", svc.Labels, svc.Spec.Selector)
		}
	})
}
