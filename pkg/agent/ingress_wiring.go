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
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/defilantech/llmkube/pkg/agent/ingress"
)

// EventReasonRelayNotAdopted is emitted once per InferenceService per agent
// process when, in relay mode, the controller has not adopted the "<isvc>"
// Service for the relay within relayAdoptionGrace of the first "<isvc>-agent"
// registration.
const EventReasonRelayNotAdopted = "RelayNotAdopted"

const (
	// legacyBindHost is the engine bind address in legacy mode.
	legacyBindHost = "0.0.0.0"
	// relayAdoptionGrace is how long the watchdog waits after the first
	// "<isvc>-agent" registration before reporting RelayNotAdopted.
	relayAdoptionGrace = 5 * time.Minute
	// ingressTokenTTL and ingressTokenGrace configure the relay TokenStore:
	// how long a read Secret is cached, and how long a rotated-out token
	// stays valid.
	ingressTokenTTL   = 5 * time.Second
	ingressTokenGrace = 10 * time.Minute
	// daemonProbeTimeout bounds each startup probe of a shared engine
	// daemon's port (Ollama, oMLX) on the host IP.
	daemonProbeTimeout = 500 * time.Millisecond
)

// validateIngress refuses a relay-mode configuration whose ingress port is
// unset or collides with the metrics or client-proxy port. Legacy mode does
// not start the ingress, so it is not checked.
func (c MetalAgentConfig) validateIngress() error {
	if c.LegacyDirectEndpoints {
		return nil
	}
	if c.IngressPort <= 0 {
		return fmt.Errorf("--ingress-port %d must be positive", c.IngressPort)
	}
	if c.Port > 0 && c.IngressPort == c.Port {
		return fmt.Errorf("--ingress-port %d collides with --port (metrics/health)", c.IngressPort)
	}
	if c.ClientPort > 0 && c.IngressPort == c.ClientPort {
		return fmt.Errorf("--ingress-port %d collides with --client-port", c.IngressPort)
	}
	return nil
}

// resolveStateDir returns the absolute state directory: the macOS default
// under home when stateDir is empty, otherwise stateDir with a leading "~" or
// "~/" resolved against home (launchd does not expand it) and a relative path
// made absolute against the working directory. "~user" is not expanded.
func resolveStateDir(stateDir, home string) (string, error) {
	if stateDir == "" {
		if home == "" {
			return "", errors.New("--state-dir is not set and the home directory is unknown")
		}
		return filepath.Join(home, "Library", "Application Support", "llmkube", "metal-agent"), nil
	}
	if stateDir == "~" || strings.HasPrefix(stateDir, "~/") {
		if home == "" {
			return "", fmt.Errorf("--state-dir %q starts with ~ but the home directory is unknown", stateDir)
		}
		stateDir = filepath.Join(home, strings.TrimPrefix(stateDir, "~"))
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("resolve --state-dir %q: %w", stateDir, err)
	}
	return abs, nil
}

// engineBindHost is the ExecutorConfig.BindHost the agent passes to every
// executor: all interfaces in legacy mode, empty (loopback) otherwise.
func (a *MetalAgent) engineBindHost() string {
	if a.config.LegacyDirectEndpoints {
		return legacyBindHost
	}
	return ""
}

// prepareIngress configures the ingress for the agent's mode. In legacy mode
// it logs the deprecation warning and returns nil: no identity is loaded, no
// ingress is built and the registry stays the direct "<isvc>" writer. In
// relay mode it loads (or creates) the TLS identity under the state dir,
// switches the registry to "<isvc>-agent" registrations with the identity's
// SPKI pin, and returns the ingress server for Start to run. a.registry must
// be set.
func (a *MetalAgent) prepareIngress() (*ingress.Server, error) {
	if a.config.LegacyDirectEndpoints {
		a.logger.Warnw("--legacy-direct-endpoints is DEPRECATED and will be removed: inference engines bind " +
			"on all interfaces without authentication and are registered directly, so anyone who can reach " +
			"this Mac can use (and on some runtimes reconfigure) them; upgrade the LLMKube controller and drop the flag")
		return nil, nil
	}
	stateDir, err := resolveStateDir(a.config.StateDir, a.home)
	if err != nil {
		return nil, err
	}
	cert, pin, err := ingress.LoadOrCreateIdentity(filepath.Join(stateDir, "ingress"))
	if err != nil {
		return nil, fmt.Errorf("load ingress identity: %w", err)
	}
	if err := a.registry.EnableIngress(a.config.IngressPort, pin); err != nil {
		return nil, fmt.Errorf("enable ingress registration: %w", err)
	}
	a.logger.Infow("relay mode: engines bind 127.0.0.1, served through the ingress",
		"ingressPort", a.config.IngressPort, "spkiPin", pin, "stateDir", stateDir,
		"identityDir", filepath.Join(stateDir, "ingress"))
	tokens := ingress.NewTokenStore(a.config.K8sClient, ingressTokenTTL, ingressTokenGrace, time.Now,
		ingress.WithLogger(a.logger.With("subsystem", "ingress-tokens")))
	return ingress.NewServer(cert, tokens, a, a.logger.With("subsystem", "ingress")), nil
}

// runIngress serves srv on all interfaces until ctx ends. Any exit before
// that (a bind failure included) is reported on fatalErrChan.
func (a *MetalAgent) runIngress(ctx context.Context, srv *ingress.Server, fatalErrChan chan<- error) {
	err := srv.Start(ctx, fmt.Sprintf(":%d", a.config.IngressPort))
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		err = errors.New("ingress stopped unexpectedly")
	}
	select {
	case fatalErrChan <- fmt.Errorf("ingress on port %d: %w", a.config.IngressPort, err):
	default:
		a.logger.Errorw("ingress failed and the fatal channel is full", "error", err)
	}
}

// warnIfDaemonsExposed warns, for each shared engine daemon the agent may not
// have started itself (Ollama, oMLX), when it answers on the host IP. Such a
// daemon bypasses the ingress entirely: it is reachable without a relay token.
func (a *MetalAgent) warnIfDaemonsExposed() {
	a.warnIfDaemonExposed(runtimeOllama, a.config.OllamaPort, 11434,
		"the Ollama daemon answers on the host IP without authentication, bypassing the ingress; "+
			"set OLLAMA_HOST=127.0.0.1 for the Ollama daemon and restart it")
	a.warnIfDaemonExposed(runtimeOMLX, a.config.OMLXPort, 8000,
		"the oMLX daemon answers on the host IP without authentication, bypassing the ingress; "+
			"restart the oMLX daemon (stop it and let the agent start it) so it binds 127.0.0.1")
}

// warnIfDaemonExposed logs msg at Warn when runtime has an executor and a TCP
// connect to port (defaultPort when zero) on the host IP succeeds.
func (a *MetalAgent) warnIfDaemonExposed(runtime string, port, defaultPort int, msg string) {
	if _, ok := a.executors[runtime]; !ok {
		return
	}
	if port == 0 {
		port = defaultPort
	}
	hostIP := a.registry.resolveHostIP()
	if hostIP == "" {
		return
	}
	addr := net.JoinHostPort(hostIP, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, daemonProbeTimeout)
	if err != nil {
		return
	}
	_ = conn.Close()
	a.logger.Warnw(msg, "addr", addr)
}

// Route implements ingress.Router: the route of the process this agent runs
// for namespace/name, with Ready mirroring the health monitor's view.
func (a *MetalAgent) Route(namespace, name string) (ingress.Route, bool) {
	key := types.NamespacedName{Namespace: namespace, Name: name}.String()
	a.mu.RLock()
	defer a.mu.RUnlock()
	p, ok := a.processes[key]
	if !ok || p == nil {
		return ingress.Route{}, false
	}
	return ingress.Route{Runtime: p.Runtime, Port: p.Port, Ready: p.Healthy}, true
}

// ServesNamespace implements ingress.Router: whether any process this agent
// runs belongs to namespace.
func (a *MetalAgent) ServesNamespace(namespace string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, p := range a.processes {
		if p != nil && p.Namespace == namespace {
			return true
		}
	}
	return false
}

// relayActive reports whether the agent registers through the ingress.
func (a *MetalAgent) relayActive() bool {
	return !a.config.LegacyDirectEndpoints && a.registry != nil && a.registry.relayMode()
}

// noteRelayRegistered records the first successful "<isvc>-agent"
// registration for key. No-op in legacy mode.
func (a *MetalAgent) noteRelayRegistered(key string) {
	if !a.relayActive() {
		return
	}
	a.relayMu.Lock()
	defer a.relayMu.Unlock()
	if _, ok := a.relayRegisteredAt[key]; !ok {
		a.relayRegisteredAt[key] = a.now()
	}
}

// checkRelayAdoption is the RelayNotAdopted watchdog, run from the heartbeat
// loop. For each served InferenceService first registered at least
// relayAdoptionGrace ago, it reads the "<isvc>" Service: when that Service is
// missing or has no selector, the controller has not adopted it for the
// relay (it predates relay support) and one Warning Event is emitted per
// InferenceService per agent process. A read error other than NotFound is
// inconclusive and only logged at debug. It must not be called with a.mu
// held.
func (a *MetalAgent) checkRelayAdoption(ctx context.Context) {
	if !a.relayActive() {
		return
	}
	a.mu.RLock()
	served := make([]types.NamespacedName, 0, len(a.processes))
	for _, p := range a.processes {
		if p != nil {
			served = append(served, types.NamespacedName{Namespace: p.Namespace, Name: p.Name})
		}
	}
	a.mu.RUnlock()

	now := a.now()
	for _, nn := range served {
		key := nn.String()
		a.relayMu.Lock()
		registeredAt, registered := a.relayRegisteredAt[key]
		sent := a.relayNotAdoptedSent[key]
		a.relayMu.Unlock()
		if !registered || sent || now.Sub(registeredAt) < relayAdoptionGrace {
			continue
		}

		svcName := sanitizeServiceName(nn.Name)
		svc := &corev1.Service{}
		err := a.config.K8sClient.Get(ctx, types.NamespacedName{Namespace: nn.Namespace, Name: svcName}, svc)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			a.logger.Debugw("relay adoption check: failed to read Service",
				"namespace", nn.Namespace, "service", svcName, "error", err)
			continue
		case len(svc.Spec.Selector) > 0:
			continue
		}

		a.relayMu.Lock()
		a.relayNotAdoptedSent[key] = true
		a.relayMu.Unlock()
		a.logger.Warnw("the controller has not adopted the InferenceService Service for the relay",
			"namespace", nn.Namespace, "service", svcName)
		a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: nn.Namespace, Name: nn.Name},
			corev1.EventTypeWarning, EventReasonRelayNotAdopted,
			"the controller has not adopted Service %s/%s for the relay; upgrade the LLMKube controller, "+
				"or run the agent with --legacy-direct-endpoints", nn.Namespace, svcName)
	}
}

// prepareRegistry puts the registry in its mode and then cleans up endpoint
// objects earlier agent processes left behind. Start calls it once, after
// a.registry and a.watcher are set and before anything is registered. The
// order matters: prepareIngress must switch the registry to relay mode first,
// because the inherited-endpoint withdrawal names objects through the
// registry's mode. The steps are:
//
//  1. prepareIngress: relay mode loads the identity and enables the ingress;
//     legacy mode leaves the registry the direct "<isvc>" writer.
//  2. Legacy mode only: delete this agent's own "<isvc>-agent" objects, so a
//     relay-to-legacy switch does not leave a relay registration behind.
//  3. The orphan sweep: agent-owned Services (and same-named slices) whose
//     InferenceService is gone, by their own names, in either mode.
//  4. Withdraw inherited endpoints: this process serves only the engines it
//     adopted, so every other one is withdrawn.
func (a *MetalAgent) prepareRegistry(ctx context.Context) (*ingress.Server, error) {
	srv, err := a.prepareIngress()
	if err != nil {
		return nil, err
	}
	if a.config.LegacyDirectEndpoints && a.watcher != nil {
		n, err := a.registry.RemoveRelayRegistrations(ctx, a.config.Namespace, a.watcher.owns)
		if err != nil {
			a.logger.Warnw("legacy mode: relay registration cleanup failed", "error", err)
		} else if n > 0 {
			a.logger.Infow("legacy mode: removed relay registrations left by a relay-mode agent", "count", n)
		}
	}
	// Reconcile orphaned Service+EndpointSlice objects from prior agent
	// sessions. The watcher's `seen` map starts fresh each Watch() call, so
	// InferenceServices deleted while the agent was down don't trigger the
	// cleanup path. This pass closes that gap by treating the agent
	// managed-by label as the authoritative inventory and cross-checking each
	// Service against the API.
	if cleaned, err := a.registry.ReconcileOrphanEndpoints(ctx, a.config.Namespace); err != nil {
		a.logger.Warnw("orphan endpoint reconciliation failed", "error", err)
	} else if cleaned > 0 {
		a.logger.Infow("cleaned up orphaned endpoints from prior sessions", "count", cleaned)
	}
	// Apart from the engines it adopted (#2079), this process serves nothing
	// yet, so any other slice a previous agent process left Ready points at a
	// child that is gone. Withdraw those before the watcher starts; adopted
	// engines keep theirs, and each successful ensureProcess re-registers
	// Ready (#1918).
	a.withdrawInheritedEndpoints(ctx)
	return srv, nil
}
