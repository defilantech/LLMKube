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
	"net"
	"net/url"
	"strings"

	"github.com/defilantech/llmkube/pkg/metalnode"
)

// dockerDesktopHostIP is the address pods on Docker Desktop (kind, the
// built-in Kubernetes) reach the host at. It is stable across network changes,
// unlike the host's LAN or tailnet address.
const dockerDesktopHostIP = "192.168.65.254"

// candidateEnv is what the MetalNode address classifier looks at.
type candidateEnv struct {
	// override is --host-ip.
	override string
	// serverIP is the API server's address; nil when it is unknown.
	serverIP net.IP
	// sourceIP is the address the kernel would send from toward serverIP;
	// nil when it is unknown.
	sourceIP net.IP
	// ifaces are the host's up, non-loopback interface addresses.
	ifaces []hostIPCandidate
}

// buildMetalNodeCandidates orders the addresses the cluster might reach this
// host at, for the controller's dial-back to try in turn. The route to the API
// server decides the order:
//
//   - --host-ip first, as an override (still verified);
//   - co-located (the API server is on loopback, a kind or Docker Desktop
//     cluster on this host): Docker Desktop's host address, then LAN, then
//     tailnet;
//   - tailnet (the server is in 100.64.0.0/10, or the route to it leaves
//     through a utun interface): the address on that route, then the other
//     tailnet addresses, then LAN;
//   - otherwise: the kernel's source address toward the server, then the
//     other private LAN addresses, then tailnet.
//
// "tailnet" here means any utun (VPN) interface or a 100.64.0.0/10 address.
// The list is capped at metalnode.MaxCandidates. Pure, for unit tests.
func buildMetalNodeCandidates(env candidateEnv) []metalnode.Candidate {
	var out []metalnode.Candidate
	seen := map[string]bool{}
	add := func(ip net.IP, source string) {
		if ip == nil || len(out) >= metalnode.MaxCandidates {
			return
		}
		key := ip.String()
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, metalnode.Candidate{Address: key, Source: source})
	}

	// A hostname --host-ip still works for EndpointSlice registration, but a
	// report may carry only IP addresses, so it is not offered here.
	if ip := net.ParseIP(env.override); ip != nil {
		add(ip, metalnode.SourceOverride)
	}

	var lan, tailnet []net.IP
	sourceIface := ""
	for _, c := range env.ifaces {
		if env.sourceIP != nil && c.ip.Equal(env.sourceIP) {
			sourceIface = c.iface
		}
		switch classifyCandidate(c) {
		case metalnode.SourceLAN:
			lan = append(lan, c.ip)
		case metalnode.SourceTailnet:
			tailnet = append(tailnet, c.ip)
		}
	}
	addAll := func(ips []net.IP, source string) {
		for _, ip := range ips {
			add(ip, source)
		}
	}

	switch {
	case env.serverIP != nil && env.serverIP.IsLoopback():
		add(net.ParseIP(dockerDesktopHostIP), metalnode.SourceDockerHost)
		addAll(lan, metalnode.SourceLAN)
		addAll(tailnet, metalnode.SourceTailnet)
	case env.serverIP != nil && (isTailnetIP(env.serverIP) || isTailnetIP(env.sourceIP) || isVPNInterface(sourceIface)):
		if usableSource(env.sourceIP) {
			add(env.sourceIP, metalnode.SourceTailnet)
		}
		addAll(tailnet, metalnode.SourceTailnet)
		addAll(lan, metalnode.SourceLAN)
	default:
		if env.serverIP != nil && usableSource(env.sourceIP) {
			add(env.sourceIP, metalnode.SourceLAN)
		}
		addAll(lan, metalnode.SourceLAN)
		addAll(tailnet, metalnode.SourceTailnet)
	}
	return out
}

// classifyCandidate sorts an interface address into tailnet, lan, or ""
// (never offered: IPv6, loopback, link-local, the bridge/NAT ranges
// selectHostIP also skips, and public addresses not on the route to the
// server).
func classifyCandidate(c hostIPCandidate) string {
	ip4 := c.ip.To4()
	switch {
	case ip4 == nil, ip4.IsLoopback(), ip4.IsLinkLocalUnicast(), inAnyNet(ip4, excludedHostNets):
		return ""
	case isVPNInterface(c.iface) || tailscaleCGNAT.Contains(ip4):
		return metalnode.SourceTailnet
	case ip4.IsPrivate():
		return metalnode.SourceLAN
	default:
		return ""
	}
}

// usableSource reports whether the kernel's source address is one a pod
// could dial: IPv4, not loopback or link-local.
func usableSource(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() && !ip4.IsUnspecified()
}

func isTailnetIP(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && tailscaleCGNAT.Contains(ip4)
}

func isVPNInterface(name string) bool {
	return strings.HasPrefix(name, "utun")
}

// candidateGatherer collects a candidateEnv from the host. Its functions are
// fields so a test can stand in for DNS, the route lookup and the
// interfaces.
type candidateGatherer struct {
	lookupIP   func(ctx context.Context, network, host string) ([]net.IP, error)
	sourceIP   func(dst net.IP) (net.IP, error)
	interfaces func() []hostIPCandidate
}

func newCandidateGatherer() candidateGatherer {
	return candidateGatherer{
		lookupIP:   net.DefaultResolver.LookupIP,
		sourceIP:   udpSourceIP,
		interfaces: gatherHostIPCandidates,
	}
}

// gather resolves the API server host from the kubeconfig's server URL and
// asks the kernel which source address it would use toward it.
func (g candidateGatherer) gather(ctx context.Context, override, apiServer string) candidateEnv {
	env := candidateEnv{override: override, ifaces: g.interfaces()}
	host := apiServerHost(apiServer)
	if host == "" {
		return env
	}
	if ip := net.ParseIP(host); ip != nil {
		env.serverIP = ip
	} else if ips, err := g.lookupIP(ctx, "ip", host); err == nil {
		env.serverIP = preferIPv4(ips)
	}
	if env.serverIP == nil {
		return env
	}
	if src, err := g.sourceIP(env.serverIP); err == nil {
		env.sourceIP = src
	}
	return env
}

// apiServerHost extracts the host from a rest.Config Host, which may or may
// not carry a scheme.
func apiServerHost(server string) string {
	if server == "" {
		return ""
	}
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func preferIPv4(ips []net.IP) net.IP {
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip
		}
	}
	if len(ips) > 0 {
		return ips[0]
	}
	return nil
}

// udpSourceIP learns the kernel's source address toward dst by "connecting"
// a UDP socket: connect on UDP only picks a route, it sends nothing.
func udpSourceIP(dst net.IP) (net.IP, error) {
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: dst, Port: 443})
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).IP, nil
}
