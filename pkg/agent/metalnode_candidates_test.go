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
	"errors"
	"net"
	"reflect"
	"runtime"
	"testing"

	"github.com/defilantech/llmkube/pkg/metalnode"
)

// macInterfaces is a Mac on Wi-Fi with Tailscale and Docker Desktop: en0 on
// the LAN, a second Ethernet LAN, utun4 on the tailnet, a corporate VPN on
// utun6 with a private address, and addresses the cluster can never reach.
func macInterfaces() []hostIPCandidate {
	return []hostIPCandidate{
		{iface: "en0", ip: net.ParseIP("192.168.1.47")},
		{iface: "en0", ip: net.ParseIP("fe80::1")},
		{iface: "en7", ip: net.ParseIP("10.20.0.5")},
		{iface: "utun4", ip: net.ParseIP("100.64.0.2")},
		{iface: "utun6", ip: net.ParseIP("10.200.0.9")},
		{iface: "bridge100", ip: net.ParseIP("192.168.65.1")},
		{iface: "en0", ip: net.ParseIP("169.254.10.10")},
		{iface: "en9", ip: net.ParseIP("203.0.113.50")},
	}
}

// routeEnv is a candidateEnv for the Mac in macInterfaces whose route to the
// API server at server leaves from source.
func routeEnv(server, source string) candidateEnv {
	return candidateEnv{serverIP: net.ParseIP(server), sourceIP: net.ParseIP(source), ifaces: macInterfaces()}
}

func mnCand(addr, source string) metalnode.Candidate {
	return metalnode.Candidate{Address: addr, Source: source}
}

func TestBuildMetalNodeCandidates(t *testing.T) {
	cases := []struct {
		name string
		env  candidateEnv
		want []metalnode.Candidate
	}{
		{
			name: "co-located: API server on loopback prefers the Docker Desktop host address",
			env:  routeEnv("127.0.0.1", "127.0.0.1"),
			want: []metalnode.Candidate{
				mnCand("192.168.65.254", metalnode.SourceDockerHost),
				mnCand("192.168.1.47", metalnode.SourceLAN),
				mnCand("10.20.0.5", metalnode.SourceLAN),
				mnCand("100.64.0.2", metalnode.SourceTailnet),
				mnCand("10.200.0.9", metalnode.SourceTailnet),
			},
		},
		{
			name: "co-located on Linux: LAN first, Docker Desktop's host address last",
			env: candidateEnv{serverIP: net.ParseIP("127.0.0.1"), sourceIP: net.ParseIP("127.0.0.1"),
				ifaces: macInterfaces(), goos: "linux"},
			want: []metalnode.Candidate{
				mnCand("192.168.1.47", metalnode.SourceLAN),
				mnCand("10.20.0.5", metalnode.SourceLAN),
				mnCand("100.64.0.2", metalnode.SourceTailnet),
				mnCand("10.200.0.9", metalnode.SourceTailnet),
				mnCand("192.168.65.254", metalnode.SourceDockerHost),
			},
		},
		{
			name: "co-located on macOS: goos darwin keeps Docker Desktop's host address first",
			env: candidateEnv{serverIP: net.ParseIP("127.0.0.1"), sourceIP: net.ParseIP("127.0.0.1"),
				ifaces: macInterfaces()[:1], goos: "darwin"},
			want: []metalnode.Candidate{
				mnCand("192.168.65.254", metalnode.SourceDockerHost),
				mnCand("192.168.1.47", metalnode.SourceLAN),
			},
		},
		{
			name: "tailnet: API server in 100.64/10 puts the address on the route first",
			env:  routeEnv("100.93.59.11", "100.64.0.2"),
			want: []metalnode.Candidate{
				mnCand("100.64.0.2", metalnode.SourceTailnet),
				mnCand("10.200.0.9", metalnode.SourceTailnet),
				mnCand("192.168.1.47", metalnode.SourceLAN),
				mnCand("10.20.0.5", metalnode.SourceLAN),
			},
		},
		{
			name: "tailnet: a private API server routed over utun (subnet router) is a tailnet route",
			env:  routeEnv("10.152.0.1", "10.200.0.9"),
			want: []metalnode.Candidate{
				mnCand("10.200.0.9", metalnode.SourceTailnet),
				mnCand("100.64.0.2", metalnode.SourceTailnet),
				mnCand("192.168.1.47", metalnode.SourceLAN),
				mnCand("10.20.0.5", metalnode.SourceLAN),
			},
		},
		{
			name: "LAN: the kernel's source address toward the server comes first",
			env:  routeEnv("10.20.0.1", "10.20.0.5"),
			want: []metalnode.Candidate{
				mnCand("10.20.0.5", metalnode.SourceLAN),
				mnCand("192.168.1.47", metalnode.SourceLAN),
				mnCand("100.64.0.2", metalnode.SourceTailnet),
				mnCand("10.200.0.9", metalnode.SourceTailnet),
			},
		},
		{
			name: "remote: a public source address is still the one the kernel uses",
			env:  routeEnv("198.51.100.10", "203.0.113.50"),
			want: []metalnode.Candidate{
				mnCand("203.0.113.50", metalnode.SourceLAN),
				mnCand("192.168.1.47", metalnode.SourceLAN),
				mnCand("10.20.0.5", metalnode.SourceLAN),
				mnCand("100.64.0.2", metalnode.SourceTailnet),
				mnCand("10.200.0.9", metalnode.SourceTailnet),
			},
		},
		{
			name: "override first, never repeated",
			env: candidateEnv{override: "100.64.0.2", serverIP: net.ParseIP("10.20.0.1"),
				sourceIP: net.ParseIP("10.20.0.5"), ifaces: macInterfaces()},
			want: []metalnode.Candidate{
				mnCand("100.64.0.2", metalnode.SourceOverride),
				mnCand("10.20.0.5", metalnode.SourceLAN),
				mnCand("192.168.1.47", metalnode.SourceLAN),
				mnCand("10.200.0.9", metalnode.SourceTailnet),
			},
		},
		{
			name: "an override that is not an IP is left out rather than invalidating the report",
			env:  candidateEnv{override: "mac.local", ifaces: macInterfaces()[:1]},
			want: []metalnode.Candidate{mnCand("192.168.1.47", metalnode.SourceLAN)},
		},
		{
			name: "unknown route: LAN then tailnet",
			env:  candidateEnv{ifaces: macInterfaces()},
			want: []metalnode.Candidate{
				mnCand("192.168.1.47", metalnode.SourceLAN),
				mnCand("10.20.0.5", metalnode.SourceLAN),
				mnCand("100.64.0.2", metalnode.SourceTailnet),
				mnCand("10.200.0.9", metalnode.SourceTailnet),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildMetalNodeCandidates(tc.env)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("candidates:\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

func TestBuildMetalNodeCandidatesCapped(t *testing.T) {
	ifaces := make([]hostIPCandidate, 0, metalnode.MaxCandidates+5)
	for i := range metalnode.MaxCandidates + 5 {
		ifaces = append(ifaces, hostIPCandidate{iface: "en0", ip: net.IPv4(10, 0, 1, byte(i+1))})
	}
	if got := buildMetalNodeCandidates(candidateEnv{ifaces: ifaces}); len(got) != metalnode.MaxCandidates {
		t.Errorf("%d candidates, want the cap %d (the controller rejects a longer report)", len(got), metalnode.MaxCandidates)
	}
}

// TestGatherCandidateEnv checks the host-facing half: the server host comes
// from the kubeconfig URL, a name is resolved, and the source address is the
// one the UDP route lookup returns.
func TestGatherCandidateEnv(t *testing.T) {
	var dialed net.IP
	g := candidateGatherer{
		lookupIP: func(_ context.Context, network, host string) ([]net.IP, error) {
			if host != "kubernetes.docker.internal" {
				return nil, errors.New("unexpected lookup " + host)
			}
			return []net.IP{net.ParseIP("::1"), net.ParseIP("127.0.0.1")}, nil
		},
		sourceIP: func(dst net.IP) (net.IP, error) {
			dialed = dst
			return net.ParseIP("127.0.0.1"), nil
		},
		interfaces: macInterfaces,
	}
	env := g.gather(context.Background(), "10.9.9.9", "https://kubernetes.docker.internal:6443")
	if !env.serverIP.Equal(net.ParseIP("127.0.0.1")) || !dialed.Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("serverIP %v, dialed %v; want the IPv4 address the name resolves to", env.serverIP, dialed)
	}
	if env.override != "10.9.9.9" || len(env.ifaces) != len(macInterfaces()) {
		t.Errorf("env = %+v, want the override and the host interfaces", env)
	}
	g.goos = "linux"
	if env = g.gather(context.Background(), "", "https://kubernetes.docker.internal:6443"); env.goos != "linux" {
		t.Errorf("env.goos = %q, want the gatherer's linux", env.goos)
	}
	if got := newCandidateGatherer().goos; got != runtime.GOOS {
		t.Errorf("newCandidateGatherer().goos = %q, want runtime.GOOS %q", got, runtime.GOOS)
	}

	g.lookupIP = func(context.Context, string, string) ([]net.IP, error) {
		return nil, errors.New("must not resolve an IP literal")
	}
	env = g.gather(context.Background(), "", "192.168.1.151:16443")
	if !env.serverIP.Equal(net.ParseIP("192.168.1.151")) {
		t.Errorf("serverIP from a scheme-less host = %v, want 192.168.1.151", env.serverIP)
	}

	g.sourceIP = func(net.IP) (net.IP, error) { return nil, errors.New("no route") }
	env = g.gather(context.Background(), "", "https://192.168.1.151:16443")
	if env.sourceIP != nil {
		t.Errorf("sourceIP = %v after a failed route lookup, want nil", env.sourceIP)
	}
}
