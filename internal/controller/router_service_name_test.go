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
	"context"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestRouterProxyServiceNameBuilders pins the scope of spec.proxy.serviceName
// on the pure builders: it names the data-plane Service and the URL in
// status.endpoint, and nothing else. The Service still selects the proxy pods
// by their generated labels, and the ConfigMap, Deployment, and admin Service
// render exactly as they do without the field, so setting it never rolls the
// proxy pods or moves the operator's budget poll.
func TestRouterProxyServiceNameBuilders(t *testing.T) {
	plain := canonicalModelRouter()
	named := canonicalModelRouter()
	named.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{ServiceName: "llmkube"}

	svc := newRouterService(named)
	if svc.Name != "llmkube" {
		t.Errorf("data-plane Service name = %q, want llmkube", svc.Name)
	}
	base := newRouterService(plain)
	svc.Name = base.Name
	if !reflect.DeepEqual(svc, base) {
		t.Errorf("data-plane Service differs beyond its name:\nnamed: %+v\nplain: %+v", svc, base)
	}
	if got := svc.Spec.Selector["app"]; got != "coding-router-router-proxy" {
		t.Errorf("Service selector app = %q, want the generated coding-router-router-proxy", got)
	}

	if got, want := routerProxyEndpoint(named),
		"http://llmkube.router-builder-test.svc.cluster.local:8080/v1/chat/completions"; got != want {
		t.Errorf("status.endpoint = %q, want %q", got, want)
	}
	named.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Port: 80, Path: "/v1"}
	if got, want := routerProxyEndpoint(named),
		"http://llmkube.router-builder-test.svc.cluster.local:80/v1"; got != want {
		t.Errorf("status.endpoint with port 80 = %q, want %q", got, want)
	}
	named.Spec.Endpoint = nil

	if got, want := routerProxyBudgetEndpoint(named), routerProxyBudgetEndpoint(plain); got != want {
		t.Errorf("budget admin URL = %q, want %q (the admin Service keeps its generated name)", got, want)
	}
	if got, want := newRouterAdminService(named), newRouterAdminService(plain); !reflect.DeepEqual(got, want) {
		t.Errorf("admin Service changed with serviceName:\ngot:  %+v\nwant: %+v", got, want)
	}
	compiled := &compiledConfig{Hash: "deadbeef", JSON: []byte(`{}`)}
	if got, want := newRouterConfigMap(named, compiled), newRouterConfigMap(plain, compiled); !reflect.DeepEqual(got, want) {
		t.Errorf("ConfigMap changed with serviceName:\ngot:  %+v\nwant: %+v", got, want)
	}
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	// A Proxy block with only serviceName must render the Deployment of a
	// router with no Proxy block at all: the field must not reach the pods.
	if got, want := r.newRouterDeployment(named, "h", false), r.newRouterDeployment(plain, "h", false); !reflect.DeepEqual(got, want) {
		t.Errorf("Deployment changed with serviceName; it would roll the proxy pods:\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestValidateModelRouterServiceNameAdminCollision pins the one name the CRD
// pattern cannot reject: the router's own admin Service. Both Services would
// be controlled by the router, so the ownership gate would pass and each
// reconcile would rewrite one Service as the other.
func TestValidateModelRouterServiceNameAdminCollision(t *testing.T) {
	mr := canonicalModelRouter()
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{ServiceName: routerProxyAdminResourceName(mr.Name)}
	var found bool
	for _, e := range validateModelRouter(mr) {
		if e.Field == "spec.proxy.serviceName" && strings.Contains(e.Message, "admin") {
			found = true
		}
	}
	if !found {
		t.Errorf("validateModelRouter(serviceName = %q) = %v, want a spec.proxy.serviceName error naming the admin Service",
			mr.Spec.Proxy.ServiceName, validateModelRouter(mr))
	}
	// The admission webhook runs the same check, so the apply is rejected
	// before the reconciler ever sees it.
	if _, err := (&ModelRouterValidator{}).ValidateCreate(context.Background(), mr); err == nil ||
		!strings.Contains(err.Error(), "spec.proxy.serviceName") {
		t.Errorf("webhook ValidateCreate err = %v, want a spec.proxy.serviceName rejection", err)
	}

	mr.Spec.Proxy.ServiceName = routerProxyResourceName(mr.Name)
	for _, e := range validateModelRouter(mr) {
		if e.Field == "spec.proxy.serviceName" {
			t.Errorf("serviceName equal to the generated name rejected: %s", e)
		}
	}
}

// TestModelRouterServiceNameSchema pins the CRD validation: a DNS-1035 label of
// at most 63 characters, because the name becomes a Service name and a DNS
// label in the client URL.
func TestModelRouterServiceNameSchema(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	for i, tc := range []struct {
		name  string
		valid bool
	}{
		{"llmkube", true},
		{"a", true},
		{"router-1", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
		{"Llmkube", false},
		{"1router", false},
		{"router-", false},
		{"my.router", false},
		{"my_router", false},
	} {
		mr := externalProxyRouter(ns, "schema-"+string(rune('a'+i)), inferencev1alpha1.ModelRouterDataPlaneProxy)
		mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{ServiceName: tc.name}
		err := c.Create(context.Background(), mr)
		switch {
		case tc.valid && err != nil:
			t.Errorf("serviceName %q rejected: %v", tc.name, err)
		case !tc.valid && err == nil:
			t.Errorf("serviceName %q accepted, want a schema rejection", tc.name)
		case !tc.valid && !apierrors.IsInvalid(err):
			t.Errorf("serviceName %q: err = %v, want Invalid", tc.name, err)
		}
	}
}

// serviceOpRecorder records Service creates and deletes in call order, so a
// test can prove the old Service outlives the creation of the new one.
type serviceOpRecorder struct {
	client.Client
	ops []string
}

func (c *serviceOpRecorder) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Service); ok {
		c.ops = append(c.ops, "create "+obj.GetName())
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *serviceOpRecorder) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*corev1.Service); ok {
		c.ops = append(c.ops, "delete "+obj.GetName())
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *serviceOpRecorder) index(op string) int {
	for i, o := range c.ops {
		if o == op {
			return i
		}
	}
	return -1
}

func reconcileRouterOnce(t *testing.T, r *ModelRouterReconciler, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	if err != nil {
		t.Fatalf("reconcile %s/%s: %v", ns, name, err)
	}
	return res
}

// setRouterServiceName sets (or, with "", clears) spec.proxy.serviceName the
// way a user would.
func setRouterServiceName(t *testing.T, c client.Client, ns, name, serviceName string) {
	t.Helper()
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if fresh.Spec.Proxy == nil {
		fresh.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{}
	}
	fresh.Spec.Proxy.ServiceName = serviceName
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("set serviceName %q: %v", serviceName, err)
	}
}

func getRouter(t *testing.T, c client.Client, ns, name string) *inferencev1alpha1.ModelRouter {
	t.Helper()
	mr := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, mr); err != nil {
		t.Fatalf("get modelrouter %s/%s: %v", ns, name, err)
	}
	return mr
}

// assertOwnedProxyService asserts the data-plane Service exists under name,
// is controlled by mr, and selects the proxy pods by their generated labels.
func assertOwnedProxyService(t *testing.T, c client.Client, mr *inferencev1alpha1.ModelRouter, name string) *corev1.Service {
	t.Helper()
	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: mr.Namespace}, svc); err != nil {
		t.Fatalf("data-plane Service %s/%s: %v", mr.Namespace, name, err)
	}
	if !metav1.IsControlledBy(svc, mr) {
		t.Errorf("Service %s owner refs = %v, want controlled by %s", name, svc.OwnerReferences, mr.Name)
	}
	if !reflect.DeepEqual(svc.Spec.Selector, routerProxySelectorLabels(mr)) {
		t.Errorf("Service %s selector = %v, want the proxy pods' %v", name, svc.Spec.Selector, routerProxySelectorLabels(mr))
	}
	return svc
}

func assertServiceGone(t *testing.T, c client.Client, ns, name, why string) {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, &corev1.Service{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Service %s/%s: err = %v, want NotFound (%s)", ns, name, err, why)
	}
}

func assertStatusEndpointHost(t *testing.T, c client.Client, ns, name, svcName string) {
	t.Helper()
	got := getRouter(t, c, ns, name).Status.Endpoint
	want := "http://" + svcName + "." + ns + ".svc.cluster.local:"
	if !strings.HasPrefix(got, want) {
		t.Errorf("status.endpoint = %q, want it on Service %s (%s...)", got, svcName, want)
	}
}

// TestModelRouterProxyServiceNameCreate pins a router created with
// spec.proxy.serviceName: the data-plane Service exists only under that name,
// status.endpoint points at it, and every other proxy object keeps its
// generated name.
func TestModelRouterProxyServiceNameCreate(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)
	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}

	mr := externalProxyRouter(ns, "named-router", inferencev1alpha1.ModelRouterDataPlaneProxy)
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{ServiceName: "llmkube"}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	reconcileRouterOnce(t, r, ns, mr.Name)
	mr = getRouter(t, c, ns, mr.Name)

	assertOwnedProxyService(t, c, mr, "llmkube")
	base := routerProxyResourceName(mr.Name)
	assertServiceGone(t, c, ns, base, "the generated name is not created when serviceName is set")
	assertOwnedProxyService(t, c, mr, routerProxyAdminResourceName(mr.Name))
	for _, kind := range []string{"ConfigMap", "Deployment"} {
		if err := getProxyObject(t, c, ns, kind, base); err != nil {
			t.Errorf("router-proxy %s %s/%s: %v (only the Service is renamed)", kind, ns, base, err)
		}
	}
	if got, want := mr.Status.Endpoint, "http://llmkube."+ns+".svc.cluster.local:8080/v1/chat/completions"; got != want {
		t.Errorf("status.endpoint = %q, want %q", got, want)
	}
}

// TestModelRouterProxyServiceNameRename walks unset -> custom -> another
// custom -> unset. Each step creates the new Service before deleting the
// previous one this router controls, moves status.endpoint, and leaves the
// Deployment's spec alone (its generation does not move, so no pod rolls). A
// Service that carries the router's labels but is not controlled by it, such
// as a hand-made alias, survives every rename.
func TestModelRouterProxyServiceNameRename(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)
	rec := &serviceOpRecorder{Client: c}
	r := &ModelRouterReconciler{Client: rec, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}

	mr := externalProxyRouter(ns, "rename-router", inferencev1alpha1.ModelRouterDataPlaneProxy)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	reconcileRouterOnce(t, r, ns, mr.Name)
	mr = getRouter(t, c, ns, mr.Name)
	base := routerProxyResourceName(mr.Name)
	assertOwnedProxyService(t, c, mr, base)
	assertStatusEndpointHost(t, c, ns, mr.Name, base)

	alias := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "alias", Namespace: ns, Labels: routerProxyLabels(mr)},
		Spec: corev1.ServiceSpec{
			Selector: routerProxySelectorLabels(mr),
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}},
		},
	}
	if err := c.Create(context.Background(), alias); err != nil {
		t.Fatalf("create alias Service: %v", err)
	}

	deploy := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: base, Namespace: ns}, deploy); err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	generation := deploy.Generation

	for _, step := range []struct{ from, to, set string }{
		{from: base, to: "llmkube", set: "llmkube"},
		{from: "llmkube", to: "llmkube-v2", set: "llmkube-v2"},
		{from: "llmkube-v2", to: base, set: ""},
	} {
		setRouterServiceName(t, c, ns, mr.Name, step.set)
		rec.ops = nil
		reconcileRouterOnce(t, r, ns, mr.Name)

		assertOwnedProxyService(t, c, mr, step.to)
		assertServiceGone(t, c, ns, step.from, "the previous Service this router controls is deleted on rename")
		assertStatusEndpointHost(t, c, ns, mr.Name, step.to)
		created, deleted := rec.index("create "+step.to), rec.index("delete "+step.from)
		if created < 0 || deleted < 0 || created > deleted {
			t.Errorf("%s -> %s: Service ops = %v, want create %s before delete %s", step.from, step.to, rec.ops, step.to, step.from)
		}
		assertOwnedProxyService(t, c, mr, routerProxyAdminResourceName(mr.Name))

		got := &corev1.Service{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "alias", Namespace: ns}, got); err != nil {
			t.Errorf("hand-made alias Service with the router's labels was deleted on rename %s -> %s: %v", step.from, step.to, err)
		}
	}

	if err := c.Get(context.Background(), types.NamespacedName{Name: base, Namespace: ns}, deploy); err != nil {
		t.Fatalf("get Deployment after renames: %v", err)
	}
	if deploy.Generation != generation {
		t.Errorf("Deployment generation %d -> %d across Service renames; renaming the Service must not roll the proxy",
			generation, deploy.Generation)
	}

	// A steady-state reconcile deletes nothing.
	rec.ops = nil
	reconcileRouterOnce(t, r, ns, mr.Name)
	for _, op := range rec.ops {
		if strings.HasPrefix(op, "delete ") {
			t.Errorf("steady-state reconcile deleted a Service: %v", rec.ops)
		}
	}
}

// TestModelRouterProxyServiceNameRefusesForeignService pins the conflict rule:
// renaming onto a Service this router does not control is refused with
// RouterProxyNameTaken and a bounded requeue, the foreign Service is left
// untouched, and the router keeps serving through its previous Service with
// status.endpoint still pointing there. Once the foreign Service is gone the
// rename completes.
func TestModelRouterProxyServiceNameRefusesForeignService(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)
	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}

	mr := externalProxyRouter(ns, "conflict-router", inferencev1alpha1.ModelRouterDataPlaneProxy)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	reconcileRouterOnce(t, r, ns, mr.Name)
	mr = getRouter(t, c, ns, mr.Name)
	base := routerProxyResourceName(mr.Name)
	oldEndpoint := mr.Status.Endpoint

	foreign := foreignService("llmkube")
	foreign.SetNamespace(ns)
	if err := c.Create(context.Background(), foreign); err != nil {
		t.Fatalf("create foreign Service: %v", err)
	}

	setRouterServiceName(t, c, ns, mr.Name, "llmkube")
	res := reconcileRouterOnce(t, r, ns, mr.Name)
	if res.RequeueAfter != routerProxyNameTakenRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v (bounded requeue on the refusal)", res.RequeueAfter, routerProxyNameTakenRequeueAfter)
	}
	fresh := getRouter(t, c, ns, mr.Name)
	cond := findCondition(fresh.Status.Conditions, ConditionAvailable)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ModelRouterReasonRouterProxyNameTaken {
		t.Fatalf("Available condition = %+v, want False/%s", cond, ModelRouterReasonRouterProxyNameTaken)
	}
	if !strings.Contains(cond.Message, "Service "+ns+"/llmkube") {
		t.Errorf("refusal message %q does not name Service %s/llmkube", cond.Message, ns)
	}
	verifyForeignService(t, c, ns, "llmkube")
	assertOwnedProxyService(t, c, mr, base)
	if fresh.Status.Endpoint != oldEndpoint {
		t.Errorf("status.endpoint = %q after the refusal, want the still-serving %q", fresh.Status.Endpoint, oldEndpoint)
	}

	if err := c.Delete(context.Background(), foreign); err != nil {
		t.Fatalf("delete foreign Service: %v", err)
	}
	reconcileRouterOnce(t, r, ns, mr.Name)
	assertOwnedProxyService(t, c, mr, "llmkube")
	assertServiceGone(t, c, ns, base, "the rename completes once the name is free")
	assertStatusEndpointHost(t, c, ns, mr.Name, "llmkube")
	if cond := findCondition(getRouter(t, c, ns, mr.Name).Status.Conditions, ConditionAvailable); cond != nil &&
		cond.Reason == ModelRouterReasonRouterProxyNameTaken {
		t.Errorf("RouterProxyNameTaken survives recovery: %s", cond.Message)
	}
}

// TestModelRouterProxyServiceNameSharedByTwoRouters pins the two-router case:
// the second router to ask for a name the first already serves on is refused
// by the ownership rule, naming the other ModelRouter, and the first router's
// Service is not overwritten.
func TestModelRouterProxyServiceNameSharedByTwoRouters(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)
	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}

	first := externalProxyRouter(ns, "first-router", inferencev1alpha1.ModelRouterDataPlaneProxy)
	first.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{ServiceName: "shared"}
	if err := c.Create(context.Background(), first); err != nil {
		t.Fatalf("create first modelrouter: %v", err)
	}
	reconcileRouterOnce(t, r, ns, first.Name)
	first = getRouter(t, c, ns, first.Name)
	assertOwnedProxyService(t, c, first, "shared")

	second := externalProxyRouter(ns, "second-router", inferencev1alpha1.ModelRouterDataPlaneProxy)
	second.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{ServiceName: "shared"}
	second.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Port: 9100}
	if err := c.Create(context.Background(), second); err != nil {
		t.Fatalf("create second modelrouter: %v", err)
	}
	res := reconcileRouterOnce(t, r, ns, second.Name)
	if res.RequeueAfter != routerProxyNameTakenRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, routerProxyNameTakenRequeueAfter)
	}
	got := getRouter(t, c, ns, second.Name)
	cond := findCondition(got.Status.Conditions, ConditionAvailable)
	if cond == nil || cond.Reason != ModelRouterReasonRouterProxyNameTaken {
		t.Fatalf("second router Available condition = %+v, want %s", cond, ModelRouterReasonRouterProxyNameTaken)
	}
	if !strings.Contains(cond.Message, "another ModelRouter") || !strings.Contains(cond.Message, string(first.UID)) {
		t.Errorf("refusal message %q does not name the first router (uid %s)", cond.Message, first.UID)
	}
	if got.Status.Endpoint != "" {
		t.Errorf("refused router published status.endpoint %q, want none", got.Status.Endpoint)
	}

	svc := assertOwnedProxyService(t, c, first, "shared")
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != routerProxyPort {
		t.Errorf("first router's Service ports = %+v, want its own port %d (not overwritten)", svc.Spec.Ports, routerProxyPort)
	}
	assertServiceGone(t, c, ns, routerProxyResourceName(second.Name), "the refused router does not fall back to its generated name")

	// The first router keeps reconciling cleanly on its Service.
	reconcileRouterOnce(t, r, ns, first.Name)
	if cond := findCondition(getRouter(t, c, ns, first.Name).Status.Conditions, ConditionAvailable); cond != nil &&
		cond.Reason == ModelRouterReasonRouterProxyNameTaken {
		t.Errorf("first router refused on its own Service: %s", cond.Message)
	}
}

// TestModelRouterProxyServiceNamePinnedNodePortRename pins the one rename a
// plain create-before-delete cannot do: a NodePort Service whose
// endpoint.nodePort is pinned. The port is held by the old Service, so the new
// one is created on an allocated port, the old one is deleted, and the new one
// then takes the pinned port in the same reconcile.
func TestModelRouterProxyServiceNamePinnedNodePortRename(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)
	rec := &serviceOpRecorder{Client: c}
	r := &ModelRouterReconciler{Client: rec, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}
	const pinned int32 = 32279

	mr := externalProxyRouter(ns, "nodeport-router", inferencev1alpha1.ModelRouterDataPlaneProxy)
	mr.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Type: "NodePort", NodePort: ptr.To(pinned)}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	// The shared control plane outlives this test; free the pinned port.
	t.Cleanup(func() {
		for _, name := range []string{routerProxyResourceName(mr.Name), "edge"} {
			_ = c.Delete(context.Background(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		}
	})
	reconcileRouterOnce(t, r, ns, mr.Name)
	mr = getRouter(t, c, ns, mr.Name)
	base := routerProxyResourceName(mr.Name)
	if svc := assertOwnedProxyService(t, c, mr, base); svc.Spec.Ports[0].NodePort != pinned {
		t.Fatalf("initial nodePort = %d, want %d", svc.Spec.Ports[0].NodePort, pinned)
	}

	setRouterServiceName(t, c, ns, mr.Name, "edge")
	rec.ops = nil
	reconcileRouterOnce(t, r, ns, mr.Name)

	svc := assertOwnedProxyService(t, c, mr, "edge")
	if svc.Spec.Ports[0].NodePort != pinned {
		t.Errorf("renamed Service nodePort = %d, want the pinned %d", svc.Spec.Ports[0].NodePort, pinned)
	}
	assertServiceGone(t, c, ns, base, "the previous Service is deleted on rename")
	created, deleted := rec.index("create edge"), rec.index("delete "+base)
	if created < 0 || deleted < 0 || created > deleted {
		t.Errorf("Service ops = %v, want create edge before delete %s", rec.ops, base)
	}
	if cond := findCondition(getRouter(t, c, ns, mr.Name).Status.Conditions, ConditionAvailable); cond != nil &&
		cond.Reason == ModelRouterReasonReconcileFailed {
		t.Errorf("rename with a pinned nodePort failed: %s", cond.Message)
	}
}

// TestModelRouterProxyServiceNameSweptOnSwitch pins the non-Proxy sweep for a
// renamed Service: leaving Proxy mode removes the Service under its custom
// name too, still by name and only when this router controls it.
func TestModelRouterProxyServiceNameSweptOnSwitch(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)
	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}

	mr := externalProxyRouter(ns, "sweep-router", inferencev1alpha1.ModelRouterDataPlaneProxy)
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{ServiceName: "llmkube"}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	reconcileRouterOnce(t, r, ns, mr.Name)
	assertOwnedProxyService(t, c, getRouter(t, c, ns, mr.Name), "llmkube")

	switchDataPlane(t, c, ns, mr.Name, inferencev1alpha1.ModelRouterDataPlaneGateway)
	reconcileRouterOnce(t, r, ns, mr.Name)
	assertServiceGone(t, c, ns, "llmkube", "leaving Proxy mode removes the renamed Service")
	assertProxyObjectsGone(t, c, ns, mr.Name)
}
