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
	"encoding/base64"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	federationv1alpha1 "github.com/defilantech/llmkube/api/federation/v1alpha1"
)

const (
	// fleetServiceAccountPrefix names the per-site ServiceAccount, ClusterRole,
	// and ClusterRoleBinding created for a FederatedCluster (fedcluster-<name>).
	fleetServiceAccountPrefix = "fedcluster-"

	// defaultFleetNamespace is where the per-site ServiceAccount and its RBAC
	// live on the datacenter cluster. Matches the operator's own namespace
	// convention (see llmkube-controller-manager in cache.go/delete.go).
	defaultFleetNamespace = "llmkube-system"

	// defaultHeartbeatIntervalSeconds mirrors the CRD's
	// +kubebuilder:default=30 on HeartbeatIntervalSeconds.
	defaultHeartbeatIntervalSeconds = int32(30)

	// fleetTokenExpirationSeconds is long-lived (10 years): this token is a
	// bootstrap credential embedded in a kubeconfig file carried to a remote
	// edge site, not a short-lived pod-projected token, so it must keep
	// working across the whole registration's lifetime. Token rotation is
	// out of scope for this command (see status/rotation follow-up).
	fleetTokenExpirationSeconds = int64(10 * 365 * 24 * 3600)

	// datacenterEndpointPlaceholder fills the edge-config snippet's server
	// field when neither --datacenter-endpoint nor the caller's own
	// kubeconfig host is available.
	datacenterEndpointPlaceholder = "<DATACENTER_API_SERVER_ENDPOINT>"

	// fleetStatusUnknown fills a table cell when a site has never pushed the
	// corresponding status field (nil Capacity/Inference, empty tier/phase).
	fleetStatusUnknown = "-"

	// fleetStatusNeverHeartbeat is the LAST HEARTBEAT cell for a site whose
	// LastHeartbeatTime is nil, i.e. it has never pushed status at all.
	fleetStatusNeverHeartbeat = "never"
)

// mintFleetToken mints a bearer token for a ServiceAccount via the
// TokenRequest API (Kubernetes 1.24+; no more auto-created SA token Secrets).
// It is a package-level seam: client-go's fake ServiceAccounts().CreateToken
// echoes the request object back unchanged instead of synthesizing a token
// value, so tests replace this var rather than exercising the fake
// TokenRequest subresource.
var mintFleetToken = func(ctx context.Context, kube kubernetes.Interface, namespace, name string) (string, error) {
	expirationSeconds := fleetTokenExpirationSeconds
	tr, err := kube.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, name, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: &expirationSeconds,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("mint token for service account %s/%s: %w", namespace, name, err)
	}
	return tr.Status.Token, nil
}

// fleetRegisterInput is the pure input to fleetRegister, decoupled from
// cobra flags and REST config so it is trivial to unit test.
type fleetRegisterInput struct {
	// Name is the FederatedCluster's name and the identity this site's
	// scoped token is restricted to (via ResourceNames in the ClusterRole).
	Name string
	// ResidencyTier is FederatedClusterSpec.DataResidencyTier.
	ResidencyTier string
	// HeartbeatIntervalSeconds is FederatedClusterSpec.HeartbeatIntervalSeconds.
	// Defaulted to defaultHeartbeatIntervalSeconds when <= 0.
	HeartbeatIntervalSeconds int32
	// Namespace is where the per-site ServiceAccount lives on the datacenter
	// cluster. Defaulted to defaultFleetNamespace when empty.
	Namespace string
	// DatacenterEndpoint is the datacenter API server URL embedded in the
	// returned edge-config snippet. Falls back to
	// datacenterEndpointPlaceholder when empty.
	DatacenterEndpoint string
	// CACertData is the datacenter API server's CA certificate (PEM), used
	// to embed certificate-authority-data in the edge-config snippet
	// instead of insecure-skip-tls-verify. Optional.
	CACertData []byte
}

type fleetRegisterOptions struct {
	name               string
	residency          string
	heartbeatInterval  int32
	datacenterEndpoint string
	namespace          string
}

// NewFleetCommand is the `llmkube fleet` parent command: registering and
// (a later task) inspecting the edge sites participating in federation.
func NewFleetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Manage federated edge sites",
		Long: `Register and inspect edge sites participating in LLMKube federation.

Federation lets a datacenter cluster track fleet-wide inference status and
capacity for remote edge sites (FederatedCluster objects), without those
sites ever being granted more than a narrow, per-site scoped credential.`,
	}
	cmd.AddCommand(newFleetRegisterCommand())
	cmd.AddCommand(newFleetStatusCommand())
	return cmd
}

func newFleetRegisterCommand() *cobra.Command {
	opts := &fleetRegisterOptions{}

	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register an edge site as a FederatedCluster",
		Long: `Register a new edge site on the datacenter cluster.

This creates a FederatedCluster object plus a least-privilege ServiceAccount,
ClusterRole, and ClusterRoleBinding scoped to ONLY that one FederatedCluster's
status subresource. It then mints a token for that ServiceAccount and prints
an edge-config snippet (kubeconfig + operator flags) for the site admin to
install on the edge cluster's operator.

Run this against the DATACENTER cluster's kubeconfig context.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetRegister(cmd, opts)
		},
	}

	cmd.Flags().StringVar(&opts.name, "name", "",
		"Unique name for the edge site's FederatedCluster (required)")
	cmd.Flags().StringVar(&opts.residency, "residency", "",
		"Data residency tier label for the site (e.g. \"eu\", \"floor-3\")")
	cmd.Flags().Int32Var(&opts.heartbeatInterval, "heartbeat-interval", defaultHeartbeatIntervalSeconds,
		"Expected edge heartbeat interval, in seconds")
	cmd.Flags().StringVar(&opts.datacenterEndpoint, "datacenter-endpoint", "",
		"Datacenter API server endpoint for the edge site to use "+
			"(e.g. https://datacenter.example.com:6443). Defaults to the "+
			"current kubeconfig context's server if not set.")
	cmd.Flags().StringVar(&opts.namespace, "namespace", defaultFleetNamespace,
		"Namespace on the datacenter cluster to create the per-site ServiceAccount in")

	if err := cmd.MarkFlagRequired("name"); err != nil {
		// Only fails if the flag name doesn't exist, which would be a
		// programmer error caught immediately by any test that constructs
		// this command.
		panic(err)
	}

	return cmd
}

func runFleetRegister(cmd *cobra.Command, opts *fleetRegisterOptions) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := federationv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	endpoint := opts.datacenterEndpoint
	if endpoint == "" {
		// Fall back to the host of the kubeconfig context this command is
		// itself running against, since `register` is run directly on the
		// datacenter cluster.
		endpoint = cfg.Host
	}

	edgeConfig, err := fleetRegister(ctx, k8sClient, kube, fleetRegisterInput{
		Name:                     opts.name,
		ResidencyTier:            opts.residency,
		HeartbeatIntervalSeconds: opts.heartbeatInterval,
		Namespace:                opts.namespace,
		DatacenterEndpoint:       endpoint,
		CACertData:               cfg.CAData,
	})
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintln(cmd.OutOrStdout(), edgeConfig); err != nil {
		return fmt.Errorf("write edge config: %w", err)
	}
	return nil
}

// fleetRegister creates the FederatedCluster plus a least-privilege
// ServiceAccount/ClusterRole/ClusterRoleBinding scoped to ONLY that one
// FederatedCluster's status subresource, mints a token for the ServiceAccount,
// and returns an edge-config snippet for the site admin. It takes no
// dependency on cobra or REST config, so it is testable with fake clients.
func fleetRegister(
	ctx context.Context,
	c client.Client,
	kube kubernetes.Interface,
	in fleetRegisterInput,
) (string, error) {
	if in.Name == "" {
		return "", fmt.Errorf("fleet register: name is required")
	}

	namespace := in.Namespace
	if namespace == "" {
		namespace = defaultFleetNamespace
	}
	interval := in.HeartbeatIntervalSeconds
	if interval <= 0 {
		interval = defaultHeartbeatIntervalSeconds
	}
	saName := fleetServiceAccountPrefix + in.Name

	fc := &federationv1alpha1.FederatedCluster{
		ObjectMeta: metav1.ObjectMeta{Name: in.Name},
		Spec: federationv1alpha1.FederatedClusterSpec{
			DisplayName:              in.Name,
			DataResidencyTier:        in.ResidencyTier,
			HeartbeatIntervalSeconds: interval,
		},
	}
	if err := c.Create(ctx, fc); err != nil {
		return "", fmt.Errorf("create FederatedCluster %q: %w", in.Name, err)
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: namespace},
	}
	if err := c.Create(ctx, sa); err != nil {
		return "", fmt.Errorf("create ServiceAccount %q: %w", saName, err)
	}

	// Least privilege: this site's token can ONLY get the FederatedCluster
	// object by its own name, and get/update/patch ITS OWN status
	// subresource. No list, no watch, no delete, no other resource, no
	// cluster-wide (unscoped) access.
	clusterRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: saName},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{federationv1alpha1.GroupVersion.Group},
				Resources:     []string{"federatedclusters/status"},
				Verbs:         []string{"get", "update", "patch"},
				ResourceNames: []string{in.Name},
			},
			{
				APIGroups:     []string{federationv1alpha1.GroupVersion.Group},
				Resources:     []string{"federatedclusters"},
				Verbs:         []string{"get"},
				ResourceNames: []string{in.Name},
			},
		},
	}
	if err := c.Create(ctx, clusterRole); err != nil {
		return "", fmt.Errorf("create ClusterRole %q: %w", saName, err)
	}

	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: saName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     saName,
		},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: namespace},
		},
	}
	if err := c.Create(ctx, clusterRoleBinding); err != nil {
		return "", fmt.Errorf("create ClusterRoleBinding %q: %w", saName, err)
	}

	token, err := mintFleetToken(ctx, kube, namespace, saName)
	if err != nil {
		return "", err
	}

	return buildEdgeConfigSnippet(in.Name, in.DatacenterEndpoint, in.CACertData, saName, namespace, token), nil
}

// buildEdgeConfigSnippet formats the kubeconfig + operator flags a site
// admin needs to bring an edge site's operator up in --federation-role=edge
// mode, pointed at this one newly-minted, narrowly-scoped credential.
func buildEdgeConfigSnippet(name, endpoint string, caData []byte, saName, namespace, token string) string {
	server := endpoint
	if server == "" {
		server = datacenterEndpointPlaceholder
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Edge site %q registered on the datacenter as FederatedCluster/%s.\n", name, name)
	fmt.Fprintf(&b, "# Save the block below as a kubeconfig file on the edge site (for example,\n")
	fmt.Fprintf(&b, "# federation-datacenter.kubeconfig), then start the edge operator with the\n")
	fmt.Fprintf(&b, "# flags shown after it.\n\n")

	fmt.Fprintf(&b, "apiVersion: v1\n")
	fmt.Fprintf(&b, "kind: Config\n")
	fmt.Fprintf(&b, "clusters:\n")
	fmt.Fprintf(&b, "- cluster:\n")
	fmt.Fprintf(&b, "    server: %s\n", server)
	if len(caData) > 0 {
		fmt.Fprintf(&b, "    certificate-authority-data: %s\n", base64.StdEncoding.EncodeToString(caData))
	} else {
		fmt.Fprintf(&b, "    insecure-skip-tls-verify: true # TODO: replace with certificate-authority-data\n")
	}
	fmt.Fprintf(&b, "  name: datacenter\n")
	fmt.Fprintf(&b, "contexts:\n")
	fmt.Fprintf(&b, "- context:\n")
	fmt.Fprintf(&b, "    cluster: datacenter\n")
	fmt.Fprintf(&b, "    user: %s\n", saName)
	fmt.Fprintf(&b, "  name: datacenter\n")
	fmt.Fprintf(&b, "current-context: datacenter\n")
	fmt.Fprintf(&b, "users:\n")
	fmt.Fprintf(&b, "- name: %s\n", saName)
	fmt.Fprintf(&b, "  user:\n")
	fmt.Fprintf(&b, "    token: %s\n\n", token)

	fmt.Fprintf(&b, "# ServiceAccount:  %s/%s\n", namespace, saName)
	fmt.Fprintf(&b, "# On the edge site's operator, set:\n")
	fmt.Fprintf(&b, "#   --federation-role=edge --federation-cluster-name=%s "+
		"--federation-datacenter-kubeconfig=<path-to-saved-file>\n", name)

	return b.String()
}

func newFleetStatusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show per-site and fleet-wide federation health",
		Long: `List every edge site registered as a FederatedCluster, with its
connection phase, GPU capacity, and inference service health, followed by a
fleet-wide summary.

Run this against the DATACENTER cluster's kubeconfig context.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetStatus(cmd)
		},
	}
	return cmd
}

func runFleetStatus(cmd *cobra.Command) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := federationv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	return fleetStatus(ctx, k8sClient, cmd.OutOrStdout())
}

// fleetStatus lists all FederatedClusters, sorts them by name for
// deterministic output, and writes a per-site table (NAME, TIER, PHASE, LAST
// HEARTBEAT, GPUS, SERVICES) followed by a fleet-wide summary footer that
// aggregates GPU capacity, service health, and a count of sites by phase.
//
// A site that has never pushed status (nil Capacity/Inference) renders as
// dashes in its row rather than panicking, and contributes zero to the
// fleet-wide sums.
func fleetStatus(ctx context.Context, c client.Client, w io.Writer) error {
	list := &federationv1alpha1.FederatedClusterList{}
	if err := c.List(ctx, list); err != nil {
		return fmt.Errorf("failed to list FederatedClusters: %w", err)
	}

	sites := list.Items
	sort.Slice(sites, func(i, j int) bool { return sites[i].Name < sites[j].Name })

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tTIER\tPHASE\tLAST HEARTBEAT\tGPUS\tSERVICES"); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}

	var totals fleetStatusTotals
	for _, site := range sites {
		if err := writeFleetStatusRow(tw, site, &totals); err != nil {
			return err
		}
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("failed to flush table: %w", err)
	}

	return writeFleetStatusSummary(w, len(sites), totals)
}

// fleetStatusTotals accumulates the fleet-wide sums and per-phase site counts
// across every FederatedCluster while the per-site table is rendered.
type fleetStatusTotals struct {
	gpusAllocatable int32
	gpusTotal       int32
	servicesReady   int32
	servicesFailed  int32
	servicesTotal   int32
	phaseCounts     map[string]int
}

func writeFleetStatusRow(tw io.Writer, site federationv1alpha1.FederatedCluster, totals *fleetStatusTotals) error {
	tier := site.Spec.DataResidencyTier
	if tier == "" {
		tier = fleetStatusUnknown
	}
	phase := site.Status.Phase
	if phase == "" {
		phase = fleetStatusUnknown
	}
	if totals.phaseCounts == nil {
		totals.phaseCounts = map[string]int{}
	}
	totals.phaseCounts[phase]++

	heartbeat := fleetStatusNeverHeartbeat
	if site.Status.LastHeartbeatTime != nil {
		heartbeat = formatAge(site.Status.LastHeartbeatTime.Time)
	}

	gpus := fleetStatusUnknown + "/" + fleetStatusUnknown
	if capacity := site.Status.Capacity; capacity != nil {
		gpus = fmt.Sprintf("%d/%d", capacity.GPUsAllocatable, capacity.GPUsTotal)
		totals.gpusAllocatable += capacity.GPUsAllocatable
		totals.gpusTotal += capacity.GPUsTotal
	}

	services := strings.Join([]string{fleetStatusUnknown, fleetStatusUnknown, fleetStatusUnknown}, "/")
	if inference := site.Status.Inference; inference != nil {
		services = fmt.Sprintf("%d/%d/%d", inference.ServicesReady, inference.ServicesFailed, inference.ServicesTotal)
		totals.servicesReady += inference.ServicesReady
		totals.servicesFailed += inference.ServicesFailed
		totals.servicesTotal += inference.ServicesTotal
	}

	if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
		site.Name, tier, phase, heartbeat, gpus, services,
	); err != nil {
		return fmt.Errorf("failed to write row for site %q: %w", site.Name, err)
	}
	return nil
}

func writeFleetStatusSummary(w io.Writer, siteCount int, totals fleetStatusTotals) error {
	if _, err := fmt.Fprintf(w, "\nFleet-wide: %d site(s) (%d %s, %d %s, %d %s)\n",
		siteCount,
		totals.phaseCounts[federationv1alpha1.FederatedClusterConnected], federationv1alpha1.FederatedClusterConnected,
		totals.phaseCounts[federationv1alpha1.FederatedClusterStale], federationv1alpha1.FederatedClusterStale,
		totals.phaseCounts[federationv1alpha1.FederatedClusterUnreachable], federationv1alpha1.FederatedClusterUnreachable,
	); err != nil {
		return fmt.Errorf("failed to write fleet-wide summary: %w", err)
	}
	if _, err := fmt.Fprintf(w, "GPUs: %d/%d allocatable/total\n", totals.gpusAllocatable, totals.gpusTotal); err != nil {
		return fmt.Errorf("failed to write GPU summary: %w", err)
	}
	if _, err := fmt.Fprintf(w, "Services: %d/%d/%d ready/failed/total\n",
		totals.servicesReady, totals.servicesFailed, totals.servicesTotal,
	); err != nil {
		return fmt.Errorf("failed to write service summary: %w", err)
	}
	return nil
}
