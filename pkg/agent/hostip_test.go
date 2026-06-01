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
	"net"
	"testing"
)

func cand(iface, ip string) hostIPCandidate {
	return hostIPCandidate{iface: iface, ip: net.ParseIP(ip)}
}

// The reported failure: a Mac with LAN, Tailscale, and a vmnet bridge.
// Auto-detect must pick the Tailscale address and reject the bridge.
// Regression for defilantech/LLMKube#526.
func TestSelectHostIP_PrefersTailscaleAndExcludesBridge(t *testing.T) {
	candidates := []hostIPCandidate{
		cand("en0", "192.168.1.47"),
		cand("utun3", "100.116.176.101"),
		cand("bridge100", "192.168.65.254"),
	}

	chosen, ok, rejected := selectHostIP(candidates)
	if !ok {
		t.Fatalf("expected a chosen IP, got none")
	}
	if chosen.ip.String() != "100.116.176.101" {
		t.Fatalf("chosen = %s, want Tailscale 100.116.176.101", chosen.ip)
	}
	if !rejectedContains(rejected, "192.168.65.254") {
		t.Fatalf("expected 192.168.65.254 to be rejected, got %+v", rejected)
	}
}

// With no Tailscale interface, fall through to the primary LAN address,
// still excluding the bridge.
func TestSelectHostIP_FallsBackToLAN(t *testing.T) {
	candidates := []hostIPCandidate{
		cand("bridge100", "192.168.65.254"),
		cand("en0", "192.168.1.47"),
	}

	chosen, ok, _ := selectHostIP(candidates)
	if !ok {
		t.Fatalf("expected a chosen IP, got none")
	}
	if chosen.ip.String() != "192.168.1.47" {
		t.Fatalf("chosen = %s, want LAN 192.168.1.47", chosen.ip)
	}
}

// Docker (172.17/16) and kind/service (10.96/12) ranges are bridge/NAT
// nets and must never be chosen.
func TestSelectHostIP_ExcludesDockerAndKindNets(t *testing.T) {
	candidates := []hostIPCandidate{
		cand("docker0", "172.17.0.1"),
		cand("kind", "10.96.0.1"),
		cand("en0", "10.0.0.5"),
	}

	chosen, ok, _ := selectHostIP(candidates)
	if !ok || chosen.ip.String() != "10.0.0.5" {
		t.Fatalf("chosen = %v (ok=%v), want routable 10.0.0.5", chosen.ip, ok)
	}
}

// Only loopback and a bridge present: nothing routable, return ok=false
// so the caller can fall back rather than registering a bad address.
func TestSelectHostIP_NoRoutableInterface(t *testing.T) {
	candidates := []hostIPCandidate{
		cand("lo0", "127.0.0.1"),
		cand("bridge100", "192.168.65.7"),
		cand("llw0", "169.254.1.2"),
	}

	if _, ok, _ := selectHostIP(candidates); ok {
		t.Fatalf("expected no routable IP, but one was chosen")
	}
}

func rejectedContains(rejected []rejectedHostIP, ip string) bool {
	for _, r := range rejected {
		if r.ip == ip {
			return true
		}
	}
	return false
}
