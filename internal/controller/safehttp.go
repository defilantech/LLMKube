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

package controller

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// remoteHostAllowlist permits specific hostnames and CIDRs back through the
// private-range SSRF guard. Hostnames match the request URL host
// (case-insensitive); CIDRs match resolved IPs.
type remoteHostAllowlist struct {
	hosts    map[string]struct{}
	prefixes []netip.Prefix
}

// parseRemoteHostAllowlist builds an allowlist from operator-supplied entries
// (--allowed-remote-hosts). Each entry is a CIDR (10.20.0.0/16), a bare IP
// (treated as a single-address prefix), or a hostname. Blank entries are
// ignored; an unparseable CIDR falls back to hostname matching.
func parseRemoteHostAllowlist(entries []string) remoteHostAllowlist {
	al := remoteHostAllowlist{hosts: map[string]struct{}{}}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			if p, err := netip.ParsePrefix(e); err == nil {
				al.prefixes = append(al.prefixes, p)
				continue
			}
			// Not a valid CIDR; fall through and treat it as a host entry.
		}
		if ip, err := netip.ParseAddr(e); err == nil {
			al.prefixes = append(al.prefixes, netip.PrefixFrom(ip, ip.BitLen()))
			continue
		}
		al.hosts[strings.ToLower(e)] = struct{}{}
	}
	return al
}

func (al remoteHostAllowlist) hostAllowed(host string) bool {
	_, ok := al.hosts[strings.ToLower(host)]
	return ok
}

func (al remoteHostAllowlist) ipAllowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range al.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// blockedPrefixes lists non-public ranges that Go's stdlib classifiers
// (IsPrivate, IsLoopback, IsLinkLocal*) do NOT cover but that must never be
// reachable from source-derived URLs unless allowlisted. Parsed once at
// package init; ipIsBlocked runs on every dial, so no per-call parsing.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),  // CGNAT / shared address space (RFC 6598), incl. Tailscale
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments (RFC 6890)
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking (RFC 2544)
	netip.MustParsePrefix("192.88.99.0/24"), // 6to4 relay anycast (RFC 3068)
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 well-known prefix (RFC 6052)
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use (RFC 8215)
}

// v4CompatPrefix matches deprecated IPv4-compatible IPv6 addresses
// (::a.b.c.d, RFC 4291 §2.5.5.1). Some stacks route these to the embedded
// IPv4 target, so they classify by the embedded address, not as IPv6.
var v4CompatPrefix = netip.MustParsePrefix("::/96")

// ipIsBlocked reports whether an IP is in a range we refuse to connect to
// unless explicitly allowlisted: loopback, link-local (incl. 169.254.169.254
// and fe80::/10), RFC-1918 private, ULA (fc00::/7), unspecified, or any of
// blockedPrefixes (CGNAT, benchmarking, NAT64, ...). Unmap() first so
// IPv4-in-IPv6 forms (::ffff:127.0.0.1) classify as their IPv4 self;
// IPv4-compatible forms (::127.0.0.1) recurse on the embedded IPv4 address.
func ipIsBlocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsPrivate() || ip.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	// IPv4-compatible IPv6: in ::/96 but not :: or ::1 (both already returned
	// true above). Extract the embedded IPv4 address and classify by it, so
	// ::169.254.169.254 and ::127.0.0.1 are blocked like their IPv4 selves.
	// Recursion terminates: the embedded address is Is4 and cannot re-enter
	// this branch.
	if ip.Is6() && v4CompatPrefix.Contains(ip) {
		b := ip.As16()
		return ipIsBlocked(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	return false
}

// newGuardedHTTPClient returns an *http.Client whose dialer refuses to connect
// to blocked IP ranges unless the target host/IP is allowlisted. The check runs
// on the RESOLVED IPs and dials only those pinned IPs, so DNS rebinding cannot
// slip a blocked address in after the check. Every resolved IP must be
// permitted (a multi-record answer mixing one public and one loopback address
// is rejected outright). Redirect targets dial through the same guard; hops
// are capped.
func newGuardedHTTPClient(allow remoteHostAllowlist, timeout time.Duration) *http.Client {
	base := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		hostAllowed := allow.hostAllowed(host)
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else {
			addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			ips = addrs
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no addresses for host %q", host)
		}
		// Strict: every resolved IP must be permitted, so a multi-record rebind
		// (one public + one 127.0.0.1) cannot pass.
		for _, ip := range ips {
			permitted := hostAllowed || allow.ipAllowed(ip) || !ipIsBlocked(ip)
			if !permitted {
				return nil, fmt.Errorf(
					"connection to %s (%s) blocked by SSRF guard (GHSA-jw3m-8q7m-f35r); "+
						"allowlist via modelSource.allowedRemoteHosts", host, ip)
			}
		}
		// Dial the pinned, already-checked IPs in resolver order, falling back
		// across them (dual-stack hosts may not listen on every address).
		var dialErr error
		for _, ip := range ips {
			conn, err := base.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			dialErr = err
		}
		return nil, dialErr
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dial,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          10,
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			return nil
		},
	}
}
