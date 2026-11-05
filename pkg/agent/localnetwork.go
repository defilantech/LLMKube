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

// macOS Local Network Privacy (Apple TN3179) refuses a process's connections
// to the local network until the user allows the process, and ties that
// permission to the binary, so every agent upgrade loses it. The refusal does
// not read as one: connect fails with EHOSTUNREACH ("no route to host") while
// the same address answers from Terminal. The agent therefore says what such
// a failure may be (#2422). Loopback and VPN interfaces (a tailnet) are
// exempt, so only private and link-local destinations count.

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
)

// localNetworkSettings is where macOS gives a process Local Network access.
const localNetworkSettings = "System Settings > Privacy & Security > Local Network"

// tailnetULA is the IPv6 range Tailscale and headscale assign. It is a ULA,
// but it is reached over the tailnet's VPN interface, which Local Network
// Privacy exempts, like the 100.64.0.0/10 IPv4 range they assign from.
var tailnetULA = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

// localNetworkBlockAddr reports whether err, from a connection to host,
// failed the way a macOS Local Network Privacy block fails, and the address
// that was refused: EHOSTUNREACH or ENETUNREACH to a private (RFC 1918, ULA)
// or link-local address, never loopback, a tailnet or a public address. The
// address is the one the error says was dialed (for a hostname, what it
// resolved to; through a proxy, the proxy, which is the connection macOS
// refused), else host when host is an IP. A hostname with no dialed address
// in the error is not resolved here to guess one: what it resolves to now
// need not be what the failed dial used. The classification is portable;
// whether to act on it is the caller's call, as only macOS has the block.
func localNetworkBlockAddr(err error, host string) (netip.Addr, bool) {
	if !errors.Is(err, syscall.EHOSTUNREACH) && !errors.Is(err, syscall.ENETUNREACH) {
		return netip.Addr{}, false
	}
	addr, ok := dialedAddr(err)
	if !ok {
		parsed, parseErr := netip.ParseAddr(host)
		addr, ok = parsed, parseErr == nil
	}
	if !ok || !isLocalNetworkAddr(addr) {
		return netip.Addr{}, false
	}
	return addr, true
}

// dialedAddr returns the remote address of the outermost *net.OpError in
// err's chain that names one. A proxied connection's outer OpError
// ("proxyconnect") names none and wraps the dial to the proxy.
func dialedAddr(err error) (netip.Addr, bool) {
	var op *net.OpError
	for errors.As(err, &op) {
		if tcp, ok := op.Addr.(*net.TCPAddr); ok && tcp != nil {
			if addr, ok := netip.AddrFromSlice(tcp.IP); ok {
				return addr.Unmap().WithZone(tcp.Zone), true
			}
		}
		err = op.Err
	}
	return netip.Addr{}, false
}

// isLocalNetworkAddr reports whether addr is a destination Local Network
// Privacy governs: a private or link-local unicast address outside the
// tailnet range. Loopback, CGNAT (100.64.0.0/10) and public addresses are
// none of these.
func isLocalNetworkAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if tailnetULA.Contains(addr.WithZone("")) {
		return false
	}
	return addr.IsPrivate() || addr.IsLinkLocalUnicast()
}
