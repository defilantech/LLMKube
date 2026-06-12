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

package agent

import (
	"context"
	"fmt"
	"net"
	"strings"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// ServiceRegistry manages Kubernetes Service and Endpoint resources
// to expose native Metal processes to the cluster
type ServiceRegistry struct {
	client client.Client
	hostIP string // explicit host IP; if empty, auto-detect via DNS
	logger *zap.SugaredLogger
}

// NewServiceRegistry creates a new service registry.
// If hostIP is non-empty it is used as the endpoint address registered in
// Kubernetes; otherwise the IP is auto-detected via DNS lookups
// (host.minikube.internal / host.docker.internal).
func NewServiceRegistry(k8sClient client.Client, hostIP string, logger *zap.SugaredLogger) *ServiceRegistry {
	return &ServiceRegistry{
		client: k8sClient,
		hostIP: hostIP,
		logger: logger,
	}
}

// RegisterEndpoint creates/updates a Kubernetes Service and Endpoints
// to expose the native process to the cluster
func (r *ServiceRegistry) RegisterEndpoint(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	port int,
) error {
	// Sanitize service name (replace dots with dashes for DNS-1035 compliance)
	serviceName := sanitizeServiceName(isvc.Name)

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: isvc.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.client, service, func() error {
		service.Labels = map[string]string{
			"app":                          isvc.Name,
			"llmkube.ai/managed-by":        "metal-agent",
			"llmkube.ai/inference-service": isvc.Name,
		}
		service.Annotations = map[string]string{
			"llmkube.ai/metal-accelerated": "true",
			"llmkube.ai/native-process":    "true",
		}
		service.Spec.Type = corev1.ServiceTypeClusterIP
		service.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       8080,
			TargetPort: intstr.FromInt(port),
			Protocol:   corev1.ProtocolTCP,
		}}
		// No selector: Endpoints are managed manually.
		service.Spec.Selector = nil
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create/update service: %w", err)
	}

	//nolint:staticcheck // SA1019: Endpoints API is still functional and appropriate for manual endpoint management
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: isvc.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.client, endpoints, func() error {
		endpoints.Labels = map[string]string{
			"app":                          isvc.Name,
			"llmkube.ai/managed-by":        "metal-agent",
			"llmkube.ai/inference-service": isvc.Name,
		}
		//nolint:staticcheck // SA1019: EndpointSubset still functional
		endpoints.Subsets = []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{
				IP: r.resolveHostIP(),
				TargetRef: &corev1.ObjectReference{
					Kind: "Pod",
					Name: fmt.Sprintf("%s-metal", isvc.Name),
				},
			}},
			Ports: []corev1.EndpointPort{{
				Name:     "http",
				Port:     int32(port), //nolint:gosec // G115: TCP ports fit in int32
				Protocol: corev1.ProtocolTCP,
			}},
		}}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create/update endpoints: %w", err)
	}

	r.logger.Infow("registered endpoint",
		"namespace", isvc.Namespace,
		"name", isvc.Name,
		"hostIP", r.resolveHostIP(),
		"port", port,
	)

	return nil
}

// UnregisterEndpoint removes the Service and Endpoints for a process
func (r *ServiceRegistry) UnregisterEndpoint(ctx context.Context, namespace, name string) error {
	// Sanitize service name (replace dots with dashes for DNS-1035 compliance)
	serviceName := sanitizeServiceName(name)

	// Delete Service
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
		},
	}
	if err := r.client.Delete(ctx, service); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete service: %w", err)
		}
		r.logger.Debugw(
			"service already deleted during endpoint cleanup",
			"namespace", namespace,
			"name", serviceName,
		)
	}

	// Delete Endpoints
	//nolint:staticcheck // SA1019: Endpoints API is still functional and appropriate for manual endpoint management
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
		},
	}
	if err := r.client.Delete(ctx, endpoints); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete endpoints: %w", err)
		}
		r.logger.Debugw(
			"endpoints already deleted during endpoint cleanup",
			"namespace", namespace,
			"name", serviceName,
		)
	}

	return nil
}

// ReconcileOrphanEndpoints scans all Service objects labeled as managed by
// this agent and removes any whose corresponding InferenceService no longer
// exists. Intended to be called once at agent startup to clean up state left
// behind when the agent was down at the time an InferenceService was deleted.
//
// Why this is needed: the InferenceServiceWatcher only emits DELETED events
// for resources it observed in its *current* session — its `seen` map is
// reinitialized on each Watch() call. If a user deletes an InferenceService
// between agent restarts, the new agent session has no record of the prior
// resource and never invokes the cleanup path, so the K8s Service+Endpoints
// stay around forever. This reconciler closes that gap by treating the
// agent-managed-by label as the authoritative inventory of "things this
// agent created" and cross-checking each one against the live API.
//
// Returns the number of orphan endpoints actually cleaned. Errors looking up
// any individual InferenceService are logged and skipped so one transient
// failure doesn't block cleanup of unrelated orphans.
func (r *ServiceRegistry) ReconcileOrphanEndpoints(ctx context.Context, namespace string) (int, error) {
	services := &corev1.ServiceList{}
	opts := []client.ListOption{
		client.MatchingLabels{"llmkube.ai/managed-by": "metal-agent"},
	}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := r.client.List(ctx, services, opts...); err != nil {
		return 0, fmt.Errorf("list managed services: %w", err)
	}

	cleaned := 0
	for i := range services.Items {
		svc := &services.Items[i]
		isvcName := svc.Labels["llmkube.ai/inference-service"]
		if isvcName == "" {
			// Service is labeled managed-by us but missing the
			// inference-service label — should never happen given how
			// RegisterEndpoint stamps both, but skip rather than
			// guess at an owner.
			r.logger.Warnw(
				"managed service missing inference-service label, skipping reconcile",
				"namespace", svc.Namespace,
				"service", svc.Name,
			)
			continue
		}

		isvc := &inferencev1alpha1.InferenceService{}
		err := r.client.Get(ctx, types.NamespacedName{
			Namespace: svc.Namespace,
			Name:      isvcName,
		}, isvc)
		if err == nil {
			// InferenceService still exists — leave the Service+Endpoints alone.
			continue
		}
		if !apierrors.IsNotFound(err) {
			// Something else went wrong looking up the InferenceService;
			// log and move on. We'd rather leak a Service than delete one
			// whose owner-status we couldn't verify.
			r.logger.Warnw("failed to look up InferenceService for managed Service",
				"namespace", svc.Namespace,
				"service", svc.Name,
				"isvc", isvcName,
				"error", err,
			)
			continue
		}

		r.logger.Infow("cleaning up orphaned managed endpoint",
			"namespace", svc.Namespace,
			"service", svc.Name,
			"isvc", isvcName,
		)
		if err := r.UnregisterEndpoint(ctx, svc.Namespace, isvcName); err != nil {
			r.logger.Warnw("failed to unregister orphan endpoint",
				"namespace", svc.Namespace,
				"service", svc.Name,
				"error", err,
			)
			continue
		}
		cleaned++
	}
	return cleaned, nil
}

// sanitizeServiceName converts a name to be DNS-1035 compliant
// (lowercase alphanumeric characters or '-', must start with alpha, end with alphanumeric)
func sanitizeServiceName(name string) string {
	// Replace dots with dashes
	return strings.ReplaceAll(name, ".", "-")
}

// resolveHostIP returns the IP address that Kubernetes uses to reach this host.
// If an explicit hostIP was provided via --host-ip, that value is returned.
// Otherwise it inspects the host's interfaces and applies selectHostIP's
// preference order (Tailscale > primary LAN, excluding bridge/NAT ranges),
// which a remote cluster or tailnet peer can actually route to. Only when no
// routable interface exists does it fall back to the legacy DNS detection for
// co-located minikube / Docker Desktop setups. Fixes defilantech/LLMKube#526.
func (r *ServiceRegistry) resolveHostIP() string {
	if r.hostIP != "" {
		return r.hostIP
	}

	candidates := gatherHostIPCandidates()
	chosen, ok, rejected := selectHostIP(candidates)
	if ok {
		r.logger.Infow("auto-detected host IP",
			"ip", chosen.ip.String(),
			"interface", chosen.iface,
			"rejected", formatRejected(rejected),
		)
		return chosen.ip.String()
	}

	fallback := getHostIP()
	r.logger.Warnw("no routable interface for host-IP auto-detect; using co-located DNS fallback",
		"ip", fallback,
		"rejected", formatRejected(rejected),
	)
	return fallback
}

// gatherHostIPCandidates enumerates the host's up, non-loopback interfaces
// and returns their unicast addresses as host-IP candidates.
func gatherHostIPCandidates() []hostIPCandidate {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []hostIPCandidate
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			out = append(out, hostIPCandidate{iface: iface.Name, ip: ip})
		}
	}
	return out
}

// formatRejected renders rejected candidates as "iface=ip (reason)" strings
// for a single structured log field.
func formatRejected(rejected []rejectedHostIP) []string {
	out := make([]string, 0, len(rejected))
	for _, r := range rejected {
		out = append(out, fmt.Sprintf("%s=%s (%s)", r.iface, r.ip, r.reason))
	}
	return out
}

// hostIPCandidate is a usable address discovered on a host network interface.
type hostIPCandidate struct {
	iface string
	ip    net.IP
}

// rejectedHostIP records a candidate the selector skipped and why, so the
// chosen-vs-rejected decision is visible in logs without a debug rerun.
type rejectedHostIP struct {
	iface  string
	ip     string
	reason string
}

// selectHostIP applies the host-IP preference policy to a candidate list:
// Tailscale (100.64.0.0/10 CGNAT) first, then any other routable IPv4,
// excluding loopback, link-local, and bridge/NAT ranges (lima/colima
// vmnet, Docker, kind/service nets). Returns ok=false when nothing
// routable is found so the caller can fall back. Pure for unit testing.
func selectHostIP(candidates []hostIPCandidate) (chosen hostIPCandidate, ok bool, rejected []rejectedHostIP) {
	var tailscale, lan *hostIPCandidate
	for i := range candidates {
		c := candidates[i]
		ip4 := c.ip.To4()
		switch {
		case ip4 == nil:
			rejected = append(rejected, rejectedHostIP{c.iface, c.ip.String(), "not IPv4"})
		case c.ip.IsLoopback():
			rejected = append(rejected, rejectedHostIP{c.iface, c.ip.String(), "loopback"})
		case c.ip.IsLinkLocalUnicast():
			rejected = append(rejected, rejectedHostIP{c.iface, c.ip.String(), "link-local"})
		case inAnyNet(ip4, excludedHostNets):
			rejected = append(rejected, rejectedHostIP{c.iface, c.ip.String(), "bridge/NAT range"})
		case tailscaleCGNAT.Contains(ip4):
			if tailscale == nil {
				cc := c
				tailscale = &cc
			}
		default:
			if lan == nil {
				cc := c
				lan = &cc
			}
		}
	}
	switch {
	case tailscale != nil:
		return *tailscale, true, rejected
	case lan != nil:
		return *lan, true, rejected
	default:
		return hostIPCandidate{}, false, rejected
	}
}

// tailscaleCGNAT is the 100.64.0.0/10 carrier-grade NAT range Tailscale
// assigns to tailnet nodes; preferred because a remote cluster joined to
// the same tailnet can always reach it.
var tailscaleCGNAT = mustParseCIDR("100.64.0.0/10")

// excludedHostNets are bridge / NAT / service ranges a remote cluster or
// peer cannot route to: lima/colima/Docker-Desktop vmnet, the Docker
// default bridge, and the kind / Kubernetes service CIDR.
var excludedHostNets = []*net.IPNet{
	mustParseCIDR("192.168.65.0/24"), // lima / colima / Docker Desktop vmnet
	mustParseCIDR("172.17.0.0/16"),   // Docker default bridge
	mustParseCIDR("10.96.0.0/12"),    // kind / Kubernetes service CIDR
}

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("invalid CIDR %q: %v", s, err))
	}
	return n
}

func inAnyNet(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// getHostIP returns the auto-detected IP address that Kubernetes can use to
// reach the host machine. For minikube, this is typically
// host.minikube.internal which resolves to 192.168.65.254.
func getHostIP() string {
	// Try to resolve host.minikube.internal (for minikube)
	if ips, err := net.LookupIP("host.minikube.internal"); err == nil && len(ips) > 0 {
		return ips[0].String()
	}

	// Fallback: Try to resolve host.docker.internal (for Docker Desktop)
	if ips, err := net.LookupIP("host.docker.internal"); err == nil && len(ips) > 0 {
		return ips[0].String()
	}

	// Final fallback: Use a common default for minikube
	return "192.168.65.254"
}
