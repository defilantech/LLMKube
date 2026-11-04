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

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// foreignObject is an object with one of Mac "mac-a"'s derived names that
// is not mac-a's: unlabeled, or labeled for another Mac.
type foreignObject struct {
	kind string
	make func(ns string, labels map[string]string) client.Object
}

func foreignObjects() []foreignObject {
	meta := func(ns, name string, labels map[string]string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}
	}
	someoneElse := func(ns string) []rbacv1.Subject {
		return []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "someone-else", Namespace: ns}}
	}
	return []foreignObject{
		{"ServiceAccount", func(ns string, l map[string]string) client.Object {
			return &corev1.ServiceAccount{ObjectMeta: meta(ns, "llmkube-metal-mac-a", l)}
		}},
		{"Role", func(ns string, l map[string]string) client.Object {
			return &rbacv1.Role{ObjectMeta: meta(ns, "llmkube-metal-mac-a", l), Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"},
			}}}
		}},
		{"RoleBinding", func(ns string, l map[string]string) client.Object {
			return &rbacv1.RoleBinding{
				ObjectMeta: meta(ns, "llmkube-metal-mac-a", l),
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"},
				Subjects:   someoneElse(ns),
			}
		}},
		{"shared RoleBinding", func(ns string, l map[string]string) client.Object {
			return &rbacv1.RoleBinding{
				ObjectMeta: meta(ns, "llmkube-metal-mac-a-shared", l),
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"},
				Subjects:   someoneElse(ns),
			}
		}},
		{"Lease", func(ns string, l map[string]string) client.Object {
			holder := "someone-else"
			return &coordinationv1.Lease{
				ObjectMeta: meta(ns, "mac-a", l),
				Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
			}
		}},
		{"Secret", func(ns string, l map[string]string) client.Object {
			return &corev1.Secret{ObjectMeta: meta(ns, "llmkube-metal-mac-a-join", l), Data: map[string][]byte{"k": []byte("v")}}
		}},
		{"MetalNode", func(ns string, l map[string]string) client.Object {
			return &inferencev1alpha1.MetalNode{ObjectMeta: meta(ns, "mac-a", l), Spec: inferencev1alpha1.MetalNodeSpec{
				ServiceAccountName: "someone-else", LeaseName: "someone-else", HostIP: "10.0.0.9"}}
		}},
	}
}

var foreignLabelings = []struct {
	name   string
	labels map[string]string
}{
	{"unlabeled", nil},
	{"other-mac", map[string]string{inferencev1alpha1.LabelMetalNode: "mac-b"}},
}

// assertUntouched fails unless obj still exists exactly as created.
func assertUntouched(t *testing.T, c client.Client, created client.Object) {
	t.Helper()
	got, ok := created.DeepCopyObject().(client.Object)
	if !ok {
		t.Fatal("copy object")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(created), got); err != nil {
		t.Fatalf("foreign %T %s: %v", created, created.GetName(), err)
	}
	if got.GetUID() != created.GetUID() || got.GetResourceVersion() != created.GetResourceVersion() {
		t.Errorf("foreign %T %s was changed (uid %s->%s, rv %s->%s)", created, created.GetName(),
			created.GetUID(), got.GetUID(), created.GetResourceVersion(), got.GetResourceVersion())
	}
}

// TestMetalTokenCreateRefusesForeignObjectsBeforeWriting: any derived-name
// object that is not this Mac's makes token create refuse before its first
// write, so the cluster is left as it was.
func TestMetalTokenCreateRefusesForeignObjectsBeforeWriting(t *testing.T) {
	for _, fo := range foreignObjects() {
		for _, lab := range foreignLabelings {
			t.Run(fo.kind+"/"+lab.name, func(t *testing.T) {
				_, c, ns := metalEnvtest(t)
				ctx := context.Background()
				foreign := fo.make(ns, lab.labels)
				if err := c.Create(ctx, foreign); err != nil {
					t.Fatal(err)
				}
				_, _, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
				if err == nil || !strings.Contains(err.Error(), foreign.GetName()) {
					t.Fatalf("token create: err = %v, want a refusal naming %s", err, foreign.GetName())
				}
				assertUntouched(t, c, foreign)
				assertNothingWritten(t, c, ns, foreign)
			})
		}
	}
}

// assertNothingWritten: no object of the Mac's exists besides the foreign
// one, and the shared Role was not created either.
func assertNothingWritten(t *testing.T, c client.Client, ns string, foreign client.Object) {
	t.Helper()
	ctx := context.Background()
	n := newMetalNames("mac-a")
	for _, o := range []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.ServiceAccount}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.Role}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.RoleBinding}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.SharedBinding}},
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.Lease}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.JoinSecret}},
		&inferencev1alpha1.MetalNode{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.MetalNode}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: metalSharedRoleName}},
	} {
		if sameKindAndName(o, foreign) {
			continue
		}
		err := c.Get(ctx, client.ObjectKeyFromObject(o), o)
		if !apierrors.IsNotFound(err) {
			t.Errorf("a refused token create left %T %s behind (get: %v)", o, o.GetName(), err)
		}
	}
}

func sameKindAndName(a, b client.Object) bool {
	return a.GetName() == b.GetName() && strings.EqualFold(typeName(a), typeName(b))
}

func typeName(o client.Object) string {
	switch o.(type) {
	case *corev1.ServiceAccount:
		return "ServiceAccount"
	case *corev1.Secret:
		return "Secret"
	case *rbacv1.Role:
		return "Role"
	case *rbacv1.RoleBinding:
		return "RoleBinding"
	case *coordinationv1.Lease:
		return "Lease"
	case *inferencev1alpha1.MetalNode:
		return "MetalNode"
	}
	return ""
}

// TestMetalStepGuardsRefuseForeignObjects drives each create step on its own,
// past the preflight, so the per-step guards (which close the window between
// the preflight and the write) are tested too.
func TestMetalStepGuardsRefuseForeignObjects(t *testing.T) {
	steps := map[string]func(context.Context, metalApply) error{
		"ServiceAccount":     ensureMetalServiceAccount,
		"Role":               ensureMetalRole,
		"RoleBinding":        ensureMetalRoleBindings,
		"shared RoleBinding": ensureMetalRoleBindings,
		"Lease":              ensureMetalLease,
		"MetalNode":          ensureMetalNode,
		"Secret": func(ctx context.Context, a metalApply) error {
			_, err := recreateJoinSecret(ctx, a)
			return err
		},
	}
	for _, fo := range foreignObjects() {
		for _, lab := range foreignLabelings {
			t.Run(fo.kind+"/"+lab.name, func(t *testing.T) {
				_, c, ns := metalEnvtest(t)
				ctx := context.Background()
				foreign := fo.make(ns, lab.labels)
				if err := c.Create(ctx, foreign); err != nil {
					t.Fatal(err)
				}
				a := metalApply{c: c, namespace: ns, names: newMetalNames("mac-a"), log: &bytes.Buffer{}}
				err := steps[fo.kind](ctx, a)
				if err == nil || !strings.Contains(err.Error(), inferencev1alpha1.LabelMetalNode) {
					t.Fatalf("%s step: err = %v, want an ownership refusal", fo.kind, err)
				}
				assertUntouched(t, c, foreign)
			})
		}
	}
}

// TestMetalTokenCreateRefusesAnInvalidJoinStringBeforeWriting: a value the
// TLS check accepts but the join string does not (certificate host names
// match without regard to case, a join string's tlsServerName must be a
// lower-case DNS name) is refused before the first write. Found only by
// Encode at the end, it used to leave the Mac's objects and a live join
// token behind, and kill an earlier unused string.
func TestMetalTokenCreateRefusesAnInvalidJoinStringBeforeWriting(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	_, _, err := runMetal(t, "token", "create", "mac-a", "-n", ns, "--tls-server-name", "LOCALHOST")
	if err == nil || !strings.Contains(err.Error(), "tlsServerName") {
		t.Fatalf("token create: err = %v, want a refusal naming tlsServerName", err)
	}
	assertNothingWritten(t, c, ns, &corev1.ConfigMap{})
}

// TestCheckMetalOwnership pins the one ownership rule the preflight and every
// write step share: an object read from the API server must carry
// inference.llmkube.dev/metal-node=<mac>; one not created yet is the Mac's.
func TestCheckMetalOwnership(t *testing.T) {
	const mac = "mac-a"
	for _, tc := range []struct {
		name   string
		rv     string
		labels map[string]string
		want   string // "" accepts; otherwise a substring of the refusal
	}{
		{name: "not created yet", labels: nil},
		{name: "not created yet, labels set for the write", labels: metalLabels(mac)},
		{name: "exists, labeled for the Mac", rv: "7", labels: map[string]string{inferencev1alpha1.LabelMetalNode: mac}},
		{name: "exists, labeled for the Mac among others", rv: "7", labels: metalLabels(mac)},
		{name: "exists, unlabeled", rv: "7", want: "is not labeled " + inferencev1alpha1.LabelMetalNode + "=mac-a"},
		{name: "exists, only managed-by", rv: "7", labels: map[string]string{labelManagedBy: metalManagedBy},
			want: "is not labeled"},
		{name: "exists, empty label value", rv: "7", labels: map[string]string{inferencev1alpha1.LabelMetalNode: ""},
			want: "is not labeled"},
		{name: "exists, labeled for another Mac", rv: "7",
			labels: map[string]string{inferencev1alpha1.LabelMetalNode: "mac-b"}, want: `belongs to Mac "mac-b"`},
		{name: "exists, label differs in case", rv: "7",
			labels: map[string]string{inferencev1alpha1.LabelMetalNode: "MAC-A"}, want: `belongs to Mac "MAC-A"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns", Name: "llmkube-metal-mac-a", ResourceVersion: tc.rv, Labels: tc.labels,
			}}
			err := checkMetalOwnership("ServiceAccount", obj, mac)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "ns/llmkube-metal-mac-a") {
				t.Fatalf("err = %v, want a refusal naming ns/llmkube-metal-mac-a and %q", err, tc.want)
			}
		})
	}
}

// TestGetMetalOwned: the read the preflight, the RoleBinding step and the
// join Secret step share reports whether the object exists and applies
// checkMetalOwnership to what it read.
func TestGetMetalOwned(t *testing.T) {
	s, err := metalScheme()
	if err != nil {
		t.Fatal(err)
	}
	secret := func(name string, labels map[string]string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, Labels: labels}}
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		secret("owned", metalLabels("mac-a")),
		secret("unlabeled", nil),
		secret("other-mac", map[string]string{inferencev1alpha1.LabelMetalNode: "mac-b"}),
	).Build()
	broken := interceptor.NewClient(fake.NewClientBuilder().WithScheme(s).Build(), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("connection refused")
		},
	})
	for _, tc := range []struct {
		name      string
		c         client.Client
		obj       string
		wantFound bool
		wantErr   string
	}{
		{name: "missing", c: c, obj: "missing"},
		{name: "owned", c: c, obj: "owned", wantFound: true},
		{name: "unlabeled", c: c, obj: "unlabeled", wantFound: true, wantErr: "is not labeled"},
		{name: "another Mac's", c: c, obj: "other-mac", wantFound: true, wantErr: `belongs to Mac "mac-b"`},
		{name: "read fails", c: broken, obj: "owned", wantErr: "get Secret ns/owned: connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := secret(tc.obj, nil)
			found, err := getMetalOwned(context.Background(), tc.c, "Secret", obj, "mac-a")
			if found != tc.wantFound {
				t.Errorf("found = %v, want %v", found, tc.wantFound)
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("err = %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if tc.wantFound && obj.GetUID() == "" && obj.GetResourceVersion() == "" {
				t.Error("getMetalOwned did not read the object into obj")
			}
		})
	}
}

// relabelMacALease sets the labels of Mac mac-a's Lease, as the Mac itself
// can (its Role may update the Lease), and returns the Lease as updated.
func relabelMacALease(t *testing.T, c client.Client, ns string, labels map[string]string) *coordinationv1.Lease {
	t.Helper()
	lease := &coordinationv1.Lease{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "mac-a"}, lease); err != nil {
		t.Fatal(err)
	}
	lease.Labels = labels
	if err := c.Update(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	return lease
}

// TestMetalLeaseRelabeledByTheMac: the Mac may update its own Lease, labels
// included. A Lease whose label no longer says mac-a is still mac-a's when
// mac-a's MetalNode, which the Mac cannot write, names it: token create puts
// the label back instead of refusing, and remove deletes the Lease instead of
// skipping it.
func TestMetalLeaseRelabeledByTheMac(t *testing.T) {
	for _, lab := range foreignLabelings {
		t.Run(lab.name, func(t *testing.T) {
			_, c, ns := metalEnvtest(t)
			ctx := context.Background()
			if _, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns); err != nil {
				t.Fatalf("token create: %v\n%s", err, stderr)
			}
			relabeled := relabelMacALease(t, c, ns, lab.labels)

			_, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
			if err != nil {
				t.Fatalf("token create after the Mac relabeled its Lease: %v\n%s", err, stderr)
			}
			if !strings.Contains(stderr, "restoring the label") {
				t.Errorf("token create does not say it restored the Lease's label: %q", stderr)
			}
			lease := &coordinationv1.Lease{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(relabeled), lease); err != nil {
				t.Fatal(err)
			}
			wantMacALabels(t, "Lease", lease)
			if lease.UID != relabeled.UID {
				t.Errorf("token create replaced the Lease (uid %s -> %s); it must keep the agent's", relabeled.UID, lease.UID)
			}

			relabelMacALease(t, c, ns, lab.labels)
			stdout, stderr, err := runMetal(t, "remove", "mac-a", "-n", ns)
			if err != nil {
				t.Fatalf("remove: %v\n%s", err, stderr)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(relabeled), &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
				t.Errorf("remove left the relabeled Lease (get: %v); stdout %q", err, stdout)
			}
			if !strings.Contains(stderr, "names it as its Lease") {
				t.Errorf("remove does not say why it deleted a Lease not labeled for the Mac: %q", stderr)
			}
		})
	}
}

// TestMetalLeaseNotClaimedByTheMetalNode: the MetalNode vouches for the Lease
// only when it is labeled for the Mac and its spec.leaseName is the Mac's
// Lease. Otherwise a Lease not labeled for the Mac is refused and left, as
// before.
func TestMetalLeaseNotClaimedByTheMetalNode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		leaseName string
		mnLabels  map[string]string
	}{
		{"MetalNode names another Lease", "someone-else", metalLabels("mac-a")},
		{"MetalNode not labeled for the Mac", "mac-a", nil},
		{"MetalNode labeled for another Mac", "mac-a", map[string]string{inferencev1alpha1.LabelMetalNode: "mac-b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, c, ns := metalEnvtest(t)
			ctx := context.Background()
			if _, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns); err != nil {
				t.Fatalf("token create: %v\n%s", err, stderr)
			}
			mn := &inferencev1alpha1.MetalNode{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "mac-a"}, mn); err != nil {
				t.Fatal(err)
			}
			mn.Spec.LeaseName = tc.leaseName
			mn.Labels = tc.mnLabels
			if err := c.Update(ctx, mn); err != nil {
				t.Fatal(err)
			}
			lease := relabelMacALease(t, c, ns, map[string]string{inferencev1alpha1.LabelMetalNode: "mac-b"})

			_, _, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
			if err == nil || !strings.Contains(err.Error(), "Lease "+ns+"/mac-a") {
				t.Fatalf("token create: err = %v, want a refusal naming the Lease", err)
			}
			assertUntouched(t, c, lease)

			stdout, _, err := runMetal(t, "remove", "mac-a", "-n", ns)
			if err != nil {
				t.Fatalf("remove: %v", err)
			}
			assertUntouched(t, c, lease)
			if !strings.Contains(stdout, "skipped Lease "+ns+"/mac-a") {
				t.Errorf("remove does not report the skipped Lease: %q", stdout)
			}
		})
	}
}
