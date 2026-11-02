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
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metaljoin"
)

const (
	metalTokenTTLDefault = 30 * time.Minute
	// metalTokenTTLMin is the shortest token the API server issues.
	metalTokenTTLMin = 10 * time.Minute
	metalTokenTTLMax = 24 * time.Hour
	// metalDeleteWait bounds the wait for a deleted object to be gone.
	metalDeleteWait = time.Minute
	// metalDeletePoll is how often that wait checks.
	metalDeletePoll = 250 * time.Millisecond
)

type metalTokenOptions struct {
	namespace     string
	ttl           time.Duration
	server        string
	tlsServerName string
	output        string
	// assumeSameServer lets an unreachable --server be checked against the
	// certificate the kubeconfig's server presents.
	assumeSameServer bool
}

// metalTokenOutput is what token create prints; the JSON keys are for
// scripts.
type metalTokenOutput struct {
	JoinCommand string    `json:"joinCommand"`
	ExpiresAt   time.Time `json:"expiresAt"`
	CAPin       string    `json:"caPin"`
	Namespace   string    `json:"namespace"`
	Mac         string    `json:"mac"`
	// lifetime is the token's lifetime as the API server issued it.
	lifetime time.Duration
}

func newMetalTokenCreateCommand() *cobra.Command {
	opts := &metalTokenOptions{}
	cmd := &cobra.Command{
		Use:   "create <mac-name>",
		Short: "Create a Mac's identity and print a single-use join string",
		Long: `Create everything a Mac needs to join the cluster and print the join command
to run on that Mac.

In the namespace it creates (or updates) the Mac's ServiceAccount, a per-Mac
Role scoped to the Mac's own objects, RoleBindings to that Role and to the
shared llmkube-metal-agent Role (created from deployment/macos/metal-agent-rbac.yaml
if missing), the Mac's Lease and MetalNode, and a join Secret. The join token
is bound to that Secret, which the Mac deletes when it joins, so the string
works once, and only until it expires (--ttl, 30 minutes by default). It
refuses, before writing anything, when an object with one of the Mac's names
exists and is not labeled for this Mac.

Until it is used or expires, the string is a credential for the Mac's
identity: whoever redeems it can mint the Mac's own tokens. Running this
again for the same Mac kills an unused previous string (within ~10 seconds
if it was just used), but not a string that was already redeemed. If a
string may have leaked, run 'llmkube metal remove <mac>' and then create a
new one. The Mac never sees your credentials.`,
		Example: `  llmkube metal token create mac-studio
  llmkube metal token create mac-studio -n inference --server https://100.64.0.1:6443`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMetalTokenCreate(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "",
		"Namespace for the Mac's objects (default: the kubeconfig context's namespace, else default)")
	cmd.Flags().DurationVar(&opts.ttl, "ttl", metalTokenTTLDefault, "How long the join string works (10m to 24h)")
	cmd.Flags().StringVar(&opts.server, "server", "",
		"API server URL the Mac connects to (default: the current context's server)")
	cmd.Flags().StringVar(&opts.tlsServerName, "tls-server-name", "",
		"Name the Mac verifies the API server's certificate against (default: set automatically when the "+
			"server's address is not in the certificate)")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "text", "Output format: text or json")
	cmd.Flags().BoolVar(&opts.assumeSameServer, "assume-same-server", false,
		"When --server cannot be reached from here, check the certificate the kubeconfig's server presents "+
			"instead, assuming --server is the same API server (the Mac still verifies it when it joins)")
	return cmd
}

func validateMetalTTL(ttl time.Duration) error {
	if ttl < metalTokenTTLMin || ttl > metalTokenTTLMax {
		return fmt.Errorf("--ttl %s is out of range: it must be between %s and %s (the API server issues no "+
			"token shorter than 10m)", ttl, "10m", "24h")
	}
	return nil
}

func validateMetalTokenOptions(mac string, opts *metalTokenOptions) error {
	if err := validateMacName(mac); err != nil {
		return err
	}
	if err := validateMetalTTL(opts.ttl); err != nil {
		return err
	}
	if opts.output != "text" && opts.output != "json" {
		return fmt.Errorf("--output %q: must be text or json", opts.output)
	}
	if opts.server != "" {
		if err := metaljoin.ValidateServer(opts.server); err != nil {
			return fmt.Errorf("--server: %w", err)
		}
	}
	return nil
}

func runMetalTokenCreate(cmd *cobra.Command, mac string, opts *metalTokenOptions) error {
	if err := validateMetalTokenOptions(mac, opts); err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	log := cmd.ErrOrStderr()

	target, err := loadMetalTarget(opts.namespace, opts.server)
	if err != nil {
		return err
	}
	// Settle what the Mac will trust before creating anything, so a join
	// string that could not work leaves nothing behind.
	ca, tlsServerName, err := resolveJoinTLS(ctx, target, opts.tlsServerName, opts.assumeSameServer, log)
	if err != nil {
		return err
	}
	pin, err := metaljoin.CAPin(ca.Raw)
	if err != nil {
		return err
	}
	payload := metaljoin.Payload{
		V:             metaljoin.Version,
		Server:        target.Server,
		CA:            ca.Raw,
		Pin:           pin,
		TLSServerName: tlsServerName,
		Namespace:     target.Namespace,
		Mac:           mac,
		// Stands in for the token until it is minted, so every other field
		// is checked as Encode will check it, before anything is written.
		Token: "pending",
	}
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("the join string would be invalid: %w", err)
	}

	scheme, err := metalScheme()
	if err != nil {
		return err
	}
	c, err := client.New(target.Config, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	kube, err := kubernetes.NewForConfig(target.Config)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}

	issued, err := metalTokenCreate(ctx, c, kube, target.Namespace, mac, opts.ttl, log)
	if err != nil {
		return err
	}
	payload.Token = issued.token
	joinString, err := metaljoin.Encode(payload)
	if err != nil {
		return fmt.Errorf("build the join string: %w", err)
	}
	if issued.lifetime < opts.ttl-time.Minute {
		_, _ = fmt.Fprintf(log, "warning: the API server shortened the join token to %s (asked for %s)\n",
			issued.lifetime, opts.ttl)
	}
	out := metalTokenOutput{
		JoinCommand: metalJoinCommand(joinString),
		ExpiresAt:   issued.expiresAt,
		CAPin:       pin,
		Namespace:   target.Namespace,
		Mac:         mac,
		lifetime:    issued.lifetime,
	}
	if opts.output == "json" {
		return writeMetalTokenJSON(cmd.OutOrStdout(), out)
	}
	return writeMetalTokenText(cmd.OutOrStdout(), out, time.Local)
}

func metalJoinCommand(joinString string) string {
	return "llmkube metal join " + joinString
}

// writeMetalTokenText prints the join command alone on the first line, so it
// copies cleanly, then its validity and the CA pin to compare on the Mac.
func writeMetalTokenText(w io.Writer, out metalTokenOutput, loc *time.Location) error {
	_, err := fmt.Fprintf(w, "%s\nvalid for %s (until %s), single use, Mac %s in namespace %s\nCA pin: %s\n",
		out.JoinCommand, formatMetalLifetime(out.lifetime), out.ExpiresAt.In(loc).Format("2006-01-02 15:04 MST"),
		out.Mac, out.Namespace, out.CAPin)
	return err
}

func writeMetalTokenJSON(w io.Writer, out metalTokenOutput) error {
	out.ExpiresAt = out.ExpiresAt.UTC()
	return json.NewEncoder(w).Encode(out)
}

func formatMetalLifetime(d time.Duration) string {
	d = d.Round(time.Minute)
	plural := func(n int64, unit string) string {
		if n == 1 {
			return fmt.Sprintf("1 %s", unit)
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	if d >= time.Hour && d%time.Hour == 0 {
		return plural(int64(d/time.Hour), "hour")
	}
	return plural(int64(d/time.Minute), "minute")
}

// jwtLifetime is exp - iat from a JWT's claims, read without verifying it:
// both come from the API server's clock, so the printed lifetime does not
// depend on this machine's.
func jwtLifetime(token string) (time.Duration, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, false
	}
	var claims struct {
		IssuedAt  int64 `json:"iat"`
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.ExpiresAt <= claims.IssuedAt {
		return 0, false
	}
	return time.Duration(claims.ExpiresAt-claims.IssuedAt) * time.Second, true
}

type metalIssuedToken struct {
	token     string
	expiresAt time.Time
	lifetime  time.Duration
}

// metalTokenCreate creates or updates the Mac's objects in namespace and
// mints a join token bound to a fresh join Secret. Progress goes to log.
func metalTokenCreate(ctx context.Context, c client.Client, kube kubernetes.Interface, namespace, mac string,
	ttl time.Duration, log io.Writer) (*metalIssuedToken, error) {
	if _, err := c.RESTMapper().RESTMapping(
		schema.GroupKind{Group: inferencev1alpha1.GroupVersion.Group, Kind: "MetalNode"},
		inferencev1alpha1.GroupVersion.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("the cluster has no MetalNode resource: upgrade the LLMKube operator (or apply its " +
				"CRDs) first")
		}
		return nil, fmt.Errorf("look up the MetalNode resource: %w", err)
	}
	n := newMetalNames(mac)
	if err := preflightMetalOwnership(ctx, c, namespace, n); err != nil {
		return nil, err
	}
	if err := ensureSharedMetalRole(ctx, c, namespace, log); err != nil {
		return nil, err
	}
	a := metalApply{c: c, namespace: namespace, names: n, log: log}
	steps := []func(context.Context, metalApply) error{
		ensureMetalServiceAccount,
		ensureMetalRole,
		ensureMetalRoleBindings,
		ensureMetalLease,
		ensureMetalNode,
	}
	for _, step := range steps {
		if err := step(ctx, a); err != nil {
			return nil, err
		}
	}
	secret, err := recreateJoinSecret(ctx, a)
	if err != nil {
		return nil, err
	}
	return mintJoinToken(ctx, kube, namespace, n, secret, ttl)
}

// metalDerivedObjects are the Mac's objects whose ownership label decides
// whether llmkube may write (or remove may delete) them, the MetalNode
// included: a hand-applied, unlabeled one is refused like the rest.
func metalDerivedObjects(namespace string, n metalNames) []struct {
	kind string
	obj  client.Object
} {
	om := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: namespace, Name: name} }
	return []struct {
		kind string
		obj  client.Object
	}{
		{"ServiceAccount", &corev1.ServiceAccount{ObjectMeta: om(n.ServiceAccount)}},
		{"Role", &rbacv1.Role{ObjectMeta: om(n.Role)}},
		{"RoleBinding", &rbacv1.RoleBinding{ObjectMeta: om(n.RoleBinding)}},
		{"RoleBinding", &rbacv1.RoleBinding{ObjectMeta: om(n.SharedBinding)}},
		{"Lease", &coordinationv1.Lease{ObjectMeta: om(n.Lease)}},
		{"Secret", &corev1.Secret{ObjectMeta: om(n.JoinSecret)}},
		{"MetalNode", &inferencev1alpha1.MetalNode{ObjectMeta: om(n.MetalNode)}},
	}
}

// preflightMetalOwnership refuses, before anything is written, when an
// object with one of the Mac's names exists and is not labeled for this Mac.
// A refusal must leave the cluster as it was: a half-made identity (a
// ServiceAccount bound to a Role on someone else's Lease) is worse than none.
// The steps check again when they write, for objects created in between.
func preflightMetalOwnership(ctx context.Context, c client.Client, namespace string, n metalNames) error {
	for _, d := range metalDerivedObjects(namespace, n) {
		if _, err := getMetalOwned(ctx, c, d.kind, d.obj, n.Mac); err != nil {
			return err
		}
	}
	return nil
}

// checkMetalOwnership is the one ownership rule of token create, used by the
// preflight and by every step that writes: an object that exists (it was
// read from the API server, so it has a resourceVersion) must be labeled for
// mac. One that does not exist yet is mac's to create.
func checkMetalOwnership(kind string, obj client.Object, mac string) error {
	if obj.GetResourceVersion() == "" || obj.GetLabels()[inferencev1alpha1.LabelMetalNode] == mac {
		return nil
	}
	return errNotMetalManaged(kind, obj, mac)
}

// getMetalOwned reads obj by its name into obj and reports whether it
// exists, refusing one that checkMetalOwnership refuses.
func getMetalOwned(ctx context.Context, c client.Client, kind string, obj client.Object, mac string) (bool, error) {
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get %s %s/%s: %w", kind, obj.GetNamespace(), obj.GetName(), err)
	}
	return true, checkMetalOwnership(kind, obj, mac)
}

// metalApply is what each step of token create works with.
type metalApply struct {
	c         client.Client
	namespace string
	names     metalNames
	log       io.Writer
}

func logResult(log io.Writer, kind, namespace, name string, res controllerutil.OperationResult) {
	_, _ = fmt.Fprintf(log, "%s %s/%s %s\n", kind, namespace, name, res)
}

// ensureSharedMetalRole creates the shared agent Role from the embedded
// manifest when the namespace lacks it. An existing Role with other rules is
// left alone with a warning: the chart or an operator may have customized it
// (for example pinned the Secret rule to resourceNames).
func ensureSharedMetalRole(ctx context.Context, c client.Client, namespace string, log io.Writer) error {
	want, err := sharedMetalAgentRole()
	if err != nil {
		return err
	}
	got := &rbacv1.Role{}
	err = c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: want.Name}, got)
	switch {
	case apierrors.IsNotFound(err):
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      want.Name,
				Labels:    map[string]string{labelManagedBy: metalManagedBy},
			},
			Rules: want.Rules,
		}
		if err := c.Create(ctx, role); err != nil {
			return fmt.Errorf("create shared Role %s/%s: %w", namespace, want.Name, err)
		}
		logResult(log, "Role", namespace, want.Name, controllerutil.OperationResultCreated)
		return nil
	case err != nil:
		return fmt.Errorf("get shared Role %s/%s: %w", namespace, want.Name, err)
	}
	if !sameRules(got.Rules, want.Rules) {
		_, _ = fmt.Fprintf(log, "warning: Role %s/%s exists with rules that differ from "+
			"deployment/macos/metal-agent-rbac.yaml; leaving it as it is (the chart or an admin may have "+
			"customized it). Agents get what it grants.\n",
			namespace, want.Name)
		return nil
	}
	_, _ = fmt.Fprintf(log, "Role %s/%s unchanged\n", namespace, want.Name)
	return nil
}

// sameRules compares rule lists ignoring the order of rules and of the
// entries within each rule.
func sameRules(a, b []rbacv1.PolicyRule) bool {
	if len(a) != len(b) {
		return false
	}
	norm := func(rules []rbacv1.PolicyRule) []string {
		out := make([]string, 0, len(rules))
		for _, r := range rules {
			key := ""
			for _, list := range [][]string{r.APIGroups, r.Resources, r.Verbs, r.ResourceNames, r.NonResourceURLs} {
				s := slices.Clone(list)
				slices.Sort(s)
				key += strings.Join(s, ",") + "|"
			}
			out = append(out, key)
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(norm(a), norm(b))
}

// adoptCheck returns a mutate prelude for controllerutil.CreateOrUpdate that
// refuses an existing object not labeled for this Mac, then sets the labels.
func adoptCheck(kind string, obj client.Object, mac string) error {
	if err := checkMetalOwnership(kind, obj, mac); err != nil {
		return err
	}
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range metalLabels(mac) {
		labels[k] = v
	}
	obj.SetLabels(labels)
	return nil
}

func ensureMetalServiceAccount(ctx context.Context, a metalApply) error {
	c, namespace, n, log := a.c, a.namespace, a.names, a.log
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: n.ServiceAccount}}
	res, err := controllerutil.CreateOrUpdate(ctx, c, sa, func() error {
		if err := adoptCheck("ServiceAccount", sa, n.Mac); err != nil {
			return err
		}
		automount := false
		sa.AutomountServiceAccountToken = &automount
		return nil
	})
	if err != nil {
		return fmt.Errorf("ServiceAccount %s/%s: %w", namespace, n.ServiceAccount, err)
	}
	logResult(log, "ServiceAccount", namespace, n.ServiceAccount, res)
	return nil
}

func ensureMetalRole(ctx context.Context, a metalApply) error {
	c, namespace, n, log := a.c, a.namespace, a.names, a.log
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: n.Role}}
	res, err := controllerutil.CreateOrUpdate(ctx, c, role, func() error {
		if err := adoptCheck("Role", role, n.Mac); err != nil {
			return err
		}
		role.Rules = metalPerMacRules(n)
		return nil
	})
	if err != nil {
		return fmt.Errorf("Role %s/%s: %w", namespace, n.Role, err)
	}
	logResult(log, "Role", namespace, n.Role, res)
	return nil
}

// ensureMetalRoleBindings binds the Mac's ServiceAccount to its own Role and
// to the shared Role. roleRef is immutable, so a binding of ours that points
// elsewhere is deleted and recreated.
func ensureMetalRoleBindings(ctx context.Context, a metalApply) error {
	c, namespace, n, log := a.c, a.namespace, a.names, a.log
	for _, b := range []struct{ name, role string }{{n.RoleBinding, n.Role}, {n.SharedBinding, metalSharedRoleName}} {
		name := b.name
		ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: b.role}
		existing := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		found, err := getMetalOwned(ctx, c, "RoleBinding", existing, n.Mac)
		if err != nil {
			return err
		}
		if found && existing.RoleRef != ref {
			if err := c.Delete(ctx, existing); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("replace RoleBinding %s/%s: %w", namespace, name, err)
			}
			if err := waitGone(ctx, c, existing); err != nil {
				return err
			}
		}
		rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		res, err := controllerutil.CreateOrUpdate(ctx, c, rb, func() error {
			if err := adoptCheck("RoleBinding", rb, n.Mac); err != nil {
				return err
			}
			rb.RoleRef = ref
			rb.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: n.ServiceAccount, Namespace: namespace}}
			return nil
		})
		if err != nil {
			return fmt.Errorf("RoleBinding %s/%s: %w", namespace, name, err)
		}
		logResult(log, "RoleBinding", namespace, name, res)
	}
	return nil
}

// ensureMetalLease creates the Lease the agent renews. An existing Lease is
// the agent's heartbeat and report, so only its labels are touched. The
// label is what lets the MetalNode controller watch it.
func ensureMetalLease(ctx context.Context, a metalApply) error {
	c, namespace, n, log := a.c, a.namespace, a.names, a.log
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: n.Lease}}
	res, err := controllerutil.CreateOrUpdate(ctx, c, lease, func() error {
		if lease.ResourceVersion == "" {
			duration := metalLeaseDurationSeconds
			lease.Spec.LeaseDurationSeconds = &duration
		}
		return adoptCheck("Lease", lease, n.Mac)
	})
	if err != nil {
		return fmt.Errorf("Lease %s/%s: %w", namespace, n.Lease, err)
	}
	logResult(log, "Lease", namespace, n.Lease, res)
	return nil
}

// ensureMetalNode creates the MetalNode or points an existing one at the
// Mac's ServiceAccount and Lease, keeping the admin's hostIP and verifier
// placement. Like every other object of the Mac's, an existing one must be
// labeled for this Mac.
func ensureMetalNode(ctx context.Context, a metalApply) error {
	c, namespace, n, log := a.c, a.namespace, a.names, a.log
	mn := &inferencev1alpha1.MetalNode{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: n.MetalNode}}
	res, err := controllerutil.CreateOrUpdate(ctx, c, mn, func() error {
		if err := adoptCheck("MetalNode", mn, n.Mac); err != nil {
			return err
		}
		mn.Spec.ServiceAccountName = n.ServiceAccount
		mn.Spec.LeaseName = n.Lease
		return nil
	})
	if err != nil {
		return fmt.Errorf("MetalNode %s/%s: %w", namespace, n.MetalNode, err)
	}
	logResult(log, "MetalNode", namespace, n.MetalNode, res)
	return nil
}

// recreateJoinSecret deletes the Mac's join Secret if it exists, which kills
// every token bound to it, and creates a new one. It only exists as the join
// token's bound object; `llmkube metal join` deletes it to make the string
// single use.
func recreateJoinSecret(ctx context.Context, a metalApply) (*corev1.Secret, error) {
	c, namespace, n, log := a.c, a.namespace, a.names, a.log
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: n.JoinSecret}}
	found, err := getMetalOwned(ctx, c, "Secret", old, n.Mac)
	if err != nil {
		return nil, err
	}
	if found {
		if err := c.Delete(ctx, old, client.Preconditions{UID: &old.UID}); client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("delete the previous join Secret %s/%s: %w", namespace, n.JoinSecret, err)
		}
		if err := waitGone(ctx, c, old); err != nil {
			return nil, err
		}
		_, _ = fmt.Fprintf(log, "Secret %s/%s deleted: an unused previous join string no longer works "+
			"(one redeemed already is revoked only by llmkube metal remove)\n", namespace, n.JoinSecret)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: n.JoinSecret, Labels: metalLabels(n.Mac)},
		Type:       corev1.SecretTypeOpaque,
	}
	if err := c.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("create join Secret %s/%s: %w", namespace, n.JoinSecret, err)
	}
	logResult(log, "Secret", namespace, n.JoinSecret, controllerutil.OperationResultCreated)
	return secret, nil
}

// waitGone waits until obj (by name) no longer exists with obj's UID.
func waitGone(ctx context.Context, c client.Client, obj client.Object) error {
	key := client.ObjectKeyFromObject(obj)
	uid := obj.GetUID()
	probe, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("wait for %s to be deleted: cannot copy the object", key)
	}
	gone := func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, key, probe)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return uid != "" && probe.GetUID() != uid, nil
	}
	if err := wait.PollUntilContextTimeout(ctx, metalDeletePoll, metalDeleteWait, true, gone); err != nil {
		return fmt.Errorf("wait for %s to be deleted: %w", key, err)
	}
	return nil
}

// mintJoinToken asks for a token for the Mac's ServiceAccount, bound to the
// join Secret so deleting the Secret revokes it. Audiences are left to the
// API server's default.
func mintJoinToken(ctx context.Context, kube kubernetes.Interface, namespace string, n metalNames,
	secret *corev1.Secret, ttl time.Duration) (*metalIssuedToken, error) {
	seconds := int64(ttl / time.Second)
	requested := time.Now()
	tr, err := kube.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, n.ServiceAccount, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: &seconds,
			BoundObjectRef: &authenticationv1.BoundObjectReference{
				Kind:       "Secret",
				APIVersion: "v1",
				Name:       secret.Name,
				UID:        secret.UID,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("mint the join token for %s/%s: %w", namespace, n.ServiceAccount, err)
	}
	if tr.Status.Token == "" {
		return nil, fmt.Errorf("mint the join token for %s/%s: the API server returned no token", namespace, n.ServiceAccount)
	}
	expires := tr.Status.ExpirationTimestamp.Time
	lifetime, ok := jwtLifetime(tr.Status.Token)
	if !ok {
		lifetime = expires.Sub(requested)
	}
	return &metalIssuedToken{token: tr.Status.Token, expiresAt: expires, lifetime: lifetime}, nil
}
