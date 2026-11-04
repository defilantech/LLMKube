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
	"fmt"
	"net"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

// followsMetalNodeHostIP reports whether the agent registers the address its
// MetalNode's dial-back verified (#2399): with --metal-node, in relay mode
// (the dial-back proves the ingress the relay dials) and without --host-ip,
// which is registered verbatim whatever the MetalNode says.
func (a *MetalAgent) followsMetalNodeHostIP() bool {
	return a.config.MetalNode != "" && a.config.HostIP == "" && a.ingressPin != "" &&
		a.registry != nil && a.registry.relayMode()
}

// followMetalNodeHostIP is the MetalNode reporter's follow hook. It points
// the registry at the address metalNodeHostIP picks and, when that moves the
// agent's endpoints, has the heartbeat loop re-register them now rather than
// at its next tick.
func (a *MetalAgent) followMetalNodeHostIP(mn *inferencev1alpha1.MetalNode, report metalnode.Report) {
	ip, why := a.metalNodeHostIP(mn, report)
	if a.registry.followHostIP(ip, why) {
		a.wakeHeartbeat()
	}
}

// metalNodeHostIP picks the address to register, and says why, from the
// MetalNode just read (nil when it does not exist) and the report about to
// go out:
//
//   - the address the cluster verified for this agent's ingress (see
//     verifiedHostIP), which is the one the relay can reach;
//   - once verification is withdrawn (a failed recheck, or an expired
//     result), the last address it verified, for as long as
//     that address is still on one of this Mac's up interfaces. The first
//     address the dial-back tries can be the one that just failed, so moving
//     there would turn a transient recheck failure into an outage;
//   - before any verification, or once the last verified address has left
//     the Mac's interfaces, the first address the dial-back tries:
//     spec.hostIP, else the report's first candidate (buildMetalNodeCandidates
//     order). Registering what the check tries first means a first candidate
//     that verifies needs no switch;
//   - else "", auto-detect.
//
// The last verified address lives in memory only, so a restarted agent
// starts from the MetalNode as it finds it. Only IPv4 qualifies: the
// agent's EndpointSlices are IPv4. Called only from the reporter's
// goroutine.
func (a *MetalAgent) metalNodeHostIP(mn *inferencev1alpha1.MetalNode, report metalnode.Report) (string, string) {
	pending := "the MetalNode does not exist"
	if mn != nil {
		ip, reason := verifiedHostIP(mn, a.ingressPin, a.config.IngressPort)
		if ip != "" {
			a.lastVerified = lastVerifiedHostIP{ip: ip}
			return ip, fmt.Sprintf("MetalNode %s/%s verified it (Reachable=True)", mn.Namespace, mn.Name)
		}
		pending = reason
	}
	if last := a.lastVerified.ip; last != "" {
		if a.registry.onLocalInterface(last) {
			if !a.lastVerified.keptLogged {
				a.lastVerified.keptLogged = true
				a.logger.Infow("keeping the last verified host IP after verification was withdrawn",
					"hostIP", last, "reason", pending)
			}
			return last, "the last verified address, kept after verification was withdrawn: " + pending
		}
		a.lastVerified = lastVerifiedHostIP{}
		pending = fmt.Sprintf("the last verified address %s is no longer on any of this Mac's interfaces; %s",
			last, pending)
	}
	if mn != nil {
		if ip := ipv4(mn.Spec.HostIP); ip != "" {
			return ip, "spec.hostIP, the first address the dial-back tries; not verified: " + pending
		}
	}
	for _, c := range report.Candidates {
		if ip := ipv4(c.Address); ip != "" {
			return ip, fmt.Sprintf("the first address the dial-back tries (%s); not verified: %s", c.Source, pending)
		}
	}
	return "", "no verified address and no candidate address to try first: " + pending
}

// lastVerifiedHostIP is the last address the MetalNode verified for this
// agent, which metalNodeHostIP keeps registering after verification is
// withdrawn while the address stays local.
type lastVerifiedHostIP struct {
	ip string
	// keptLogged is set once keeping ip after a withdrawal has been logged,
	// so each withdrawal is logged once.
	keptLogged bool
}

// verifiedHostIP returns status.hostIP when the controller verified it for
// the ingress this agent serves, else "" and why not. The controller keeps
// hostIP only while Reachable is True, and withdraws both when the pin or
// port changes; requiring status to carry the agent's own pin and port also
// covers the moment before it has seen a new one (a new identity, say).
func verifiedHostIP(mn *inferencev1alpha1.MetalNode, pin string, port int) (string, string) {
	st := mn.Status
	c := meta.FindStatusCondition(st.Conditions, inferencev1alpha1.MetalNodeConditionReachable)
	switch {
	case c == nil:
		return "", "Reachable is not reported yet"
	case c.Status != metav1.ConditionTrue:
		return "", fmt.Sprintf("Reachable=%s (%s)", c.Status, c.Reason)
	case st.HostIP == "":
		return "", "status.hostIP is empty"
	case pin == "" || st.IngressSPKI != pin || int(st.IngressPort) != port:
		return "", "status.hostIP was verified for another ingress key or port"
	}
	ip := ipv4(st.HostIP)
	if ip == "" {
		return "", fmt.Sprintf("status.hostIP %s is not IPv4, and the agent's EndpointSlices are", st.HostIP)
	}
	return ip, ""
}

// ipv4 returns s in dotted form when it is an IPv4 address, else "".
func ipv4(s string) string {
	if ip := net.ParseIP(s).To4(); ip != nil {
		return ip.String()
	}
	return ""
}
