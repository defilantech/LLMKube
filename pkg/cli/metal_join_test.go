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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metaljoin"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

// fakeMac stands in for the Mac join and leave run on: a temporary home, a
// llmkube-metal-agent on PATH, and a launchctl that records its calls and,
// like launchd, refuses to bootstrap a service that is still loaded.
type fakeMac struct {
	home        string
	agentBin    string
	llamaServer string
	calls       []string
	failOn      string
	// onBootstrap runs when launchctl bootstrap succeeds: the agent starts.
	onBootstrap func()
	loaded      bool
	// stopPolls is how many launchctl print calls a booted-out agent stays
	// loaded for while it stops.
	stopPolls int
	stopping  int
	// onAgent runs when leave runs the agent binary; its error is the run's.
	onAgent func(args []string) error
	uid     int
	// noGUISession: launchctl print gui/<uid> fails, as over SSH with nobody
	// logged in at the Mac.
	noGUISession bool
	// pid is what launchctl print reports for the loaded agent; 0 prints none.
	pid int
	// listened records join's port checks. Every port is free unless busy
	// names its address; nothing is bound unless realPorts names its port.
	listened  []string
	busy      map[string]error
	realPorts map[int]bool
	// holders is what lsof reports for a port.
	holders map[int][]portHolder
}

func newFakeMac(t *testing.T) *fakeMac {
	t.Helper()
	m := &fakeMac{home: t.TempDir(), uid: 501}
	m.agentBin = filepath.Join(t.TempDir(), "bin", metalAgentBinary)
	if err := os.MkdirAll(filepath.Dir(m.agentBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.agentBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.llamaServer = filepath.Join(filepath.Dir(m.agentBin), "llama-server")
	if err := os.WriteFile(m.llamaServer, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := newMetalHost
	newMetalHost = func() (metalHost, error) {
		return metalHost{
			goos: "darwin",
			home: m.home,
			uid:  m.uid,
			launchctl: func(_ context.Context, args ...string) (string, error) {
				call := strings.Join(args, " ")
				m.calls = append(m.calls, call)
				if m.failOn != "" && strings.HasPrefix(call, m.failOn) {
					return "", errors.New("launchctl failed")
				}
				switch args[0] {
				case "bootout":
					if !m.loaded {
						return "", errors.New("Boot-out failed: 3: No such process")
					}
					m.stopping = m.stopPolls
					m.loaded = m.stopping > 0
				case "print":
					if args[1] == "gui/"+strconv.Itoa(m.uid) {
						if m.noGUISession {
							return "", errors.New("exit status 112: Bad request.\nCould not find domain for user gui: " +
								strconv.Itoa(m.uid))
						}
						return "gui/" + strconv.Itoa(m.uid) + " = {\n}\n", nil
					}
					if !m.loaded {
						return "", errors.New("Could not find service")
					}
					if m.stopping > 0 {
						m.stopping--
						m.loaded = m.stopping > 0
					}
					// The shape of launchctl print: top-level keys one tab in.
					out := args[1] + " = {\n\tstate = running\n"
					if m.pid > 0 {
						out += "\tpid = " + strconv.Itoa(m.pid) + "\n"
					}
					return out + "\tsemaphores = {\n\t\tpid = 1\n\t}\n}\n", nil
				case "bootstrap":
					if m.loaded {
						return "", errors.New("Bootstrap failed: 5: Input/output error")
					}
					m.loaded = true
					if m.onBootstrap != nil {
						m.onBootstrap()
					}
				}
				return "", nil
			},
			lookPath: func(name string) (string, error) {
				if name == metalAgentBinary {
					return m.agentBin, nil
				}
				return "", exec.ErrNotFound
			},
			llamaServerPaths: []string{m.llamaServer},
			executable:       func() (string, error) { return filepath.Join(m.home, "llmkube"), nil },
			runAgent: func(_ context.Context, bin string, args []string, _, _ io.Writer) error {
				call := "agent " + bin + " " + strings.Join(args, " ")
				if m.loaded {
					call += " (while the agent is loaded)"
				}
				m.calls = append(m.calls, call)
				if m.onAgent != nil {
					return m.onAgent(args)
				}
				return nil
			},
			listen: func(network, address string) (net.Listener, error) {
				m.listened = append(m.listened, address)
				if err, ok := m.busy[address]; ok {
					return nil, err
				}
				_, port, err := net.SplitHostPort(address)
				if err != nil {
					t.Fatalf("join checks %q, not a host:port", address)
				}
				if n, _ := strconv.Atoi(port); m.realPorts[n] {
					return net.Listen(network, address)
				}
				return fakeListener{}, nil
			},
			portHolders: func(_ context.Context, port int) []portHolder { return m.holders[port] },
		}, nil
	}
	t.Cleanup(func() { newMetalHost = prev })
	return m
}

func (m *fakeMac) paths() metalAgentPaths { return newMetalAgentPaths(m.home) }

// fakeListener is a port join's check found free, without binding it: the
// Mac running the tests may have its own agent on the default ports.
type fakeListener struct{ net.Listener }

func (fakeListener) Close() error { return nil }

// issueJoinString runs token create for mac-a and returns the join string
// and its payload.
func issueJoinString(t *testing.T, ns string) (string, metaljoin.Payload) {
	t.Helper()
	stdout, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
	if err != nil {
		t.Fatalf("token create: %v\n%s", err, stderr)
	}
	first, _, _ := strings.Cut(stdout, "\n")
	return strings.TrimPrefix(first, "llmkube metal join "), decodeJoinLine(t, stdout)
}

// testIngressSPKI is a well-formed ingress pin for the fake agent's report.
var testIngressSPKI = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

// markVerified stands in for an agent and the MetalNode controller: the
// agent renews mac-a's Lease at renewed with a report, and the controller
// marks the MetalNode connected and Reachable at 100.64.0.2:9443 for it.
func markVerified(t *testing.T, c client.Client, ns string, renewed time.Time) {
	t.Helper()
	ctx := context.Background()
	raw, err := metalnode.Encode(metalnode.Report{IngressPort: 9443, IngressSPKI: testIngressSPKI,
		Candidates: []metalnode.Candidate{{Address: "100.64.0.2", Source: metalnode.SourceTailnet}}})
	if err != nil {
		t.Fatal(err)
	}
	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "mac-a"}, lease); err != nil {
		t.Fatal(err)
	}
	rt := metav1.NewMicroTime(renewed)
	lease.Spec.RenewTime = &rt
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	lease.Annotations[metalnode.ReportAnnotation] = raw
	if err := c.Update(ctx, lease); err != nil {
		t.Fatal(err)
	}

	mn := &inferencev1alpha1.MetalNode{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "mac-a"}, mn); err != nil {
		t.Fatal(err)
	}
	mn.Status = verifiedStatus(mn.Generation)
	if err := c.Status().Update(ctx, mn); err != nil {
		t.Fatal(err)
	}
}

func verifiedStatus(gen int64) inferencev1alpha1.MetalNodeStatus {
	st := inferencev1alpha1.MetalNodeStatus{ObservedGeneration: gen, HostIP: "100.64.0.2", IngressPort: 9443,
		IngressSPKI: testIngressSPKI}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: inferencev1alpha1.MetalNodeConditionAgentConnected, ObservedGeneration: gen,
		Status: metav1.ConditionTrue, Reason: inferencev1alpha1.MetalNodeReasonHeartbeatFresh, Message: "fresh"})
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: inferencev1alpha1.MetalNodeConditionReachable, ObservedGeneration: gen,
		Status: metav1.ConditionTrue, Reason: inferencev1alpha1.MetalNodeReasonDialBackSucceeded, Message: "reached"})
	return st
}

// agentStartsOnBootstrap makes the agent join starts renew its Lease and
// get verified.
func (m *fakeMac) agentStartsOnBootstrap(t *testing.T, c client.Client, ns string) {
	m.onBootstrap = func() { markVerified(t, c, ns, time.Now()) }
}

func authenticatedAs(t *testing.T, cfg *rest.Config) (string, error) {
	t.Helper()
	scheme, err := metalScheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	review := &authenticationv1.SelfSubjectReview{}
	if err := c.Create(context.Background(), review); err != nil {
		return "", err
	}
	return review.Status.UserInfo.Username, nil
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// TestMetalJoinSetsUpTheAgent runs token create and join end to end against
// a real API server: the agent's kubeconfig reads a token of the Mac's own
// ServiceAccount from a 0600 file and trusts only the cluster CA, the join
// Secret is gone so the string stops working, and the LaunchAgent starts the
// agent with renewal on.
func TestMetalJoinSetsUpTheAgent(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	joinString, p := issueJoinString(t, ns)
	mac.agentStartsOnBootstrap(t, c, ns)

	stdout, stderr, err := runMetal(t, "join", joinString, "--wait", "30s")
	if err != nil {
		t.Fatalf("join: %v\nstderr: %s", err, stderr)
	}
	paths := mac.paths()
	if !strings.Contains(stderr, "CA pin: "+p.Pin) {
		t.Errorf("join does not print the CA pin to compare: %q", stderr)
	}
	if !strings.Contains(stdout, "100.64.0.2:9443") {
		t.Errorf("join does not say where the cluster reaches the Mac: %q", stdout)
	}
	agentToken, err := os.ReadFile(paths.Token)
	if err != nil {
		t.Fatalf("read the agent's token: %v", err)
	}
	for _, secret := range []string{p.Token, string(agentToken)} {
		if strings.Contains(stdout+stderr, secret) {
			t.Error("join printed a token")
		}
	}
	for path, want := range map[string]os.FileMode{paths.Dir: 0o700, paths.Token: 0o600, paths.Kubeconfig: 0o600} {
		if got := fileMode(t, path); got != want {
			t.Errorf("%s mode %o, want %o", path, got, want)
		}
	}

	assertAgentKubeconfig(t, paths, p, ns)
	if life, ok := jwtLifetime(string(agentToken)); !ok || life < metalAgentTokenLifetimeDefault-time.Minute {
		t.Errorf("agent token lifetime %s (ok %t), want the %s default", life, ok, metalAgentTokenLifetimeDefault)
	}

	joinSecret := types.NamespacedName{Namespace: ns, Name: "llmkube-metal-mac-a-join"}
	err = c.Get(context.Background(), joinSecret, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("join Secret after join: %v, want NotFound", err)
	}
	assertMetalAgentPlist(t, mac, paths, ns)
}

// assertAgentKubeconfig checks the kubeconfig as the agent loads it: through
// clientcmd, which is also what the agent's config.GetConfig uses.
func assertAgentKubeconfig(t *testing.T, paths metalAgentPaths, p metaljoin.Payload, ns string) {
	t.Helper()
	kc, err := clientcmd.LoadFromFile(paths.Kubeconfig)
	if err != nil {
		t.Fatalf("load the agent's kubeconfig: %v", err)
	}
	ctxCfg := kc.Contexts[kc.CurrentContext]
	if ctxCfg == nil || ctxCfg.Namespace != ns {
		t.Fatalf("current context %+v, want namespace %s", ctxCfg, ns)
	}
	user, cluster := kc.AuthInfos[ctxCfg.AuthInfo], kc.Clusters[ctxCfg.Cluster]
	if user.Token != "" || user.TokenFile != paths.Token || user.ClientCertificateData != nil || user.Exec != nil {
		t.Errorf("kubeconfig user %+v, want only tokenFile %s", user, paths.Token)
	}
	wantCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.CA})
	if cluster.Server != p.Server || !bytes.Equal(cluster.CertificateAuthorityData, wantCA) ||
		cluster.InsecureSkipTLSVerify {
		t.Errorf("kubeconfig cluster %s (insecure %t), want %s with the join string's CA", cluster.Server,
			cluster.InsecureSkipTLSVerify, p.Server)
	}

	// The agent's --renew-service-account-token renews exactly this file.
	cfg, err := clientcmd.BuildConfigFromFlags("", paths.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BearerTokenFile != paths.Token {
		t.Errorf("the agent's rest config reads its token from %q, want %s", cfg.BearerTokenFile, paths.Token)
	}
	user2, err := authenticatedAs(t, cfg)
	if want := "system:serviceaccount:" + ns + ":llmkube-metal-mac-a"; err != nil || user2 != want {
		t.Errorf("the agent's kubeconfig authenticates as %q (%v), want %q", user2, err, want)
	}
}

func assertMetalAgentPlist(t *testing.T, mac *fakeMac, paths metalAgentPaths, ns string) {
	t.Helper()
	raw, err := os.ReadFile(paths.Plist)
	if err != nil {
		t.Fatalf("read the LaunchAgent: %v", err)
	}
	var args []string
	inArgs := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "<key>ProgramArguments</key>":
			inArgs = true
		case inArgs && line == "</array>":
			inArgs = false
		case inArgs && strings.HasPrefix(line, "<string>"):
			args = append(args, strings.TrimSuffix(strings.TrimPrefix(line, "<string>"), "</string>"))
		}
	}
	want := []string{mac.agentBin, "--kubeconfig", paths.Kubeconfig, "--namespace", ns, "--metal-node", "mac-a",
		"--renew-service-account-token", "--log-level", "info", "--llama-server", mac.llamaServer}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("agent arguments %q, want %q", args, want)
	}
	if plutil, err := exec.LookPath("plutil"); err == nil {
		if out, err := exec.Command(plutil, "-lint", paths.Plist).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint: %v: %s", err, out)
		}
	}
	wantCalls := []string{"print gui/501", "bootout gui/501/" + metalAgentLabel, "print gui/501/" + metalAgentLabel,
		"bootstrap gui/501 " + paths.Plist}
	if strings.Join(mac.calls, "; ") != strings.Join(wantCalls, "; ") {
		t.Errorf("launchctl calls %q, want %q", mac.calls, wantCalls)
	}
}

// TestMetalJoinStringIsSingleUse: once join has deleted the join Secret, the
// join token stops authenticating (after the API server's ~10 s cache of a
// successful authentication), and a second join says the string is used up.
func TestMetalJoinStringIsSingleUse(t *testing.T) {
	cfg, _, ns := metalEnvtest(t)
	newFakeMac(t)
	joinString, p := issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0"); err != nil {
		t.Fatalf("join: %v\n%s", err, stderr)
	}

	joinCfg := rest.AnonymousClientConfig(cfg)
	joinCfg.BearerToken = p.Token
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := authenticatedAs(t, joinCfg)
		if apierrors.IsUnauthorized(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the join token still authenticates 30s after join deleted its Secret (err %v)", err)
		}
		time.Sleep(time.Second)
	}
	_, _, err := runMetal(t, "join", joinString, "--wait", "0")
	if err == nil || !strings.Contains(err.Error(), "expired or was already used") {
		t.Errorf("second join: err = %v, want the string reported as used", err)
	}
}

// TestMetalJoinRefusesBeforeAnyChange: a tampered string, a missing agent
// binary, or another OS stop join before it writes a file, calls launchctl
// or redeems the token.
func TestMetalJoinRefusesBeforeAnyChange(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	joinString, p := issueJoinString(t, ns)
	tamperedString := tamperedJoinString(t, p)

	for _, tc := range []struct {
		name  string
		setup func(*fakeMac)
		args  []string
		want  string
	}{
		{"CA that does not match its pin", nil, []string{"join", tamperedString}, "pin"},
		{"no agent binary", func(m *fakeMac) { _ = os.Remove(m.agentBin) }, []string{"join", joinString}, metalAgentBinary},
		{"agent binary flag not executable", func(m *fakeMac) {
			_ = os.Chmod(m.agentBin, 0o644) //nolint:gosec // G302: not executable on purpose
		},
			[]string{"join", joinString, "--agent-binary", "AGENT"}, "not an executable"},
		{"no llama-server", func(m *fakeMac) { _ = os.Remove(m.llamaServer) }, []string{"join", joinString},
			"brew install llama.cpp"},
		{"short token lifetime", nil, []string{"join", joinString, "--token-lifetime", "5m"}, "10m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mac := newFakeMac(t)
			if tc.setup != nil {
				tc.setup(mac)
			}
			for i, a := range tc.args {
				tc.args[i] = strings.ReplaceAll(a, "AGENT", mac.agentBin)
			}
			_, _, err := runMetal(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("join: err = %v, want a refusal mentioning %q", err, tc.want)
			}
			if entries, _ := os.ReadDir(mac.home); len(entries) != 0 || len(mac.calls) != 0 {
				t.Errorf("a refused join wrote %v and ran launchctl %q", entries, mac.calls)
			}
		})
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "llmkube-metal-mac-a-join"},
		&corev1.Secret{}); err != nil {
		t.Errorf("a refused join used up the join string: get join Secret: %v", err)
	}

	prev := newMetalHost
	newMetalHost = func() (metalHost, error) { return metalHost{goos: "linux", home: t.TempDir()}, nil }
	defer func() { newMetalHost = prev }()
	if _, _, err := runMetal(t, "join", joinString); err == nil || !strings.Contains(err.Error(), "runs on the Mac") {
		t.Errorf("join on linux: err = %v, want a refusal", err)
	}
}

// tamperedJoinString is p with its pin replaced, the way a string altered
// on the way would arrive: everything else intact, the CA no longer matching.
func tamperedJoinString(t *testing.T, p metaljoin.Payload) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"v": metaljoin.Version, "server": p.Server, "ca": p.CA,
		"pin": "sha256:" + strings.Repeat("0", 64), "tlsServerName": p.TLSServerName,
		"namespace": p.Namespace, "mac": p.Mac, "token": p.Token})
	if err != nil {
		t.Fatal(err)
	}
	return metaljoin.Prefix + base64.RawURLEncoding.EncodeToString(raw)
}

// TestMetalJoinWaitIgnoresAnEarlierAgentsStatus: on a re-join the
// MetalNode can still say connected and Reachable for the agent join just
// replaced. join must wait for the agent it started, not report success
// while that one is not running.
func TestMetalJoinWaitIgnoresAnEarlierAgentsStatus(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	newFakeMac(t)
	joinString, _ := issueJoinString(t, ns)
	markVerified(t, c, ns, time.Now().Add(-time.Minute))

	_, stderr, err := runMetal(t, "join", joinString, "--wait", "3s")
	if err == nil || !strings.Contains(err.Error(), "not Reachable within 3s") {
		t.Fatalf("join over an earlier agent's status: err = %v, want a timeout\nstderr: %s", err, stderr)
	}
	if !strings.Contains(err.Error(), "the agent this join started to renew its Lease") {
		t.Errorf("join's timeout does not say what it waited for: %v", err)
	}
}

func TestMetalJoinVerified(t *testing.T) {
	started := time.Now()
	report, err := metalnode.Encode(metalnode.Report{IngressPort: 9443, IngressSPKI: testIngressSPKI,
		Candidates: []metalnode.Candidate{{Address: "100.64.0.2", Source: metalnode.SourceTailnet}}})
	if err != nil {
		t.Fatal(err)
	}
	renewedAt := func(at time.Time) *coordinationv1.Lease {
		rt := metav1.NewMicroTime(at)
		return &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{metalnode.ReportAnnotation: report}},
			Spec:       coordinationv1.LeaseSpec{RenewTime: &rt}}
	}
	fresh := renewedAt(started.Add(time.Second))
	node := func(edit func(*inferencev1alpha1.MetalNode)) *inferencev1alpha1.MetalNode {
		mn := &inferencev1alpha1.MetalNode{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Status: verifiedStatus(2)}
		if edit != nil {
			edit(mn)
		}
		return mn
	}
	for _, tc := range []struct {
		name  string
		mn    *inferencev1alpha1.MetalNode
		lease *coordinationv1.Lease
		want  bool
		why   string
	}{
		{"the new agent, verified", node(nil), fresh, true, ""},
		{"renewed in the microsecond join started", node(nil), renewedAt(started), true, ""},
		{"Lease last renewed by the earlier agent", node(nil), renewedAt(started.Add(-time.Second)), false,
			"renew its Lease"},
		{"Lease not readable", node(nil), nil, false, "renew its Lease"},
		{"status for an earlier generation", node(func(mn *inferencev1alpha1.MetalNode) { mn.Generation = 3 }),
			fresh, false, "observe generation 3"},
		{"Reachable for an earlier generation", node(func(mn *inferencev1alpha1.MetalNode) {
			meta.FindStatusCondition(mn.Status.Conditions,
				inferencev1alpha1.MetalNodeConditionReachable).ObservedGeneration = 1
		}), fresh, false, ""},
		{"Reachable for an earlier ingress key", node(func(mn *inferencev1alpha1.MetalNode) {
			mn.Status.IngressSPKI = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
		}), fresh, false, "earlier report"},
		{"Reachable at an address the agent no longer offers", node(func(mn *inferencev1alpha1.MetalNode) {
			mn.Status.HostIP = "192.168.1.9"
		}), fresh, false, "no longer offers"},
		{"Reachable at the admin's spec.hostIP", node(func(mn *inferencev1alpha1.MetalNode) {
			mn.Status.HostIP, mn.Spec.HostIP = "192.168.1.9", "192.168.1.9"
		}), fresh, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := metalJoinVerified(tc.mn, tc.lease, started)
			if got != tc.want || (tc.why != "" && !strings.Contains(why, tc.why)) {
				t.Errorf("metalJoinVerified = %t, %q; want %t, %q", got, why, tc.want, tc.why)
			}
		})
	}
}

// TestMetalJoinKeepsALaunchAgentItDidNotWrite: a Mac set up with make
// install-metal-agent runs the agent with its own arguments. join refuses to
// replace that plist before it uses the string; with --force it replaces it,
// keeps it, and leave says how to bring it back.
func TestMetalJoinKeepsALaunchAgentItDidNotWrite(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	paths := mac.paths()
	joinString, _ := issueJoinString(t, ns)
	const production = "<plist>the Mac's own agent</plist>"
	if err := os.MkdirAll(filepath.Dir(paths.Plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Plist, []byte(production), 0o644); err != nil { //nolint:gosec // G306: a plist
		t.Fatal(err)
	}

	_, _, err := runMetal(t, "join", joinString, "--wait", "0")
	if err == nil || !strings.Contains(err.Error(), "did not install") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("join over another LaunchAgent: err = %v, want a refusal naming --force", err)
	}
	if got, _ := os.ReadFile(paths.Plist); string(got) != production || len(mac.calls) != 0 {
		t.Errorf("a refused join left the plist %q and ran launchctl %q", got, mac.calls)
	}
	if _, err := os.Stat(paths.Kubeconfig); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused join wrote the kubeconfig: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "llmkube-metal-mac-a-join"},
		&corev1.Secret{}); err != nil {
		t.Errorf("a refused join used up the join string: get join Secret: %v", err)
	}

	_, stderr, err := runMetal(t, "join", joinString, "--wait", "0", "--force")
	if err != nil {
		t.Fatalf("join --force: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "join does not keep the arguments of the LaunchAgent it replaces") {
		t.Errorf("join --force does not say the replaced LaunchAgent's arguments are not kept: %q", stderr)
	}
	if kept, err := os.ReadFile(paths.PlistBackup); err != nil || string(kept) != production {
		t.Errorf("backup %s = %q, %v; want the replaced plist", paths.PlistBackup, kept, err)
	}
	stdout, _, err := runMetal(t, "leave")
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := os.Stat(paths.Plist); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("join's plist after leave: %v, want removed", err)
	}
	if !strings.Contains(stdout, "cp \""+paths.PlistBackup+"\"") {
		t.Errorf("leave does not say how to restore the replaced LaunchAgent: %q", stdout)
	}
}

// TestMetalLeaveLeavesALaunchAgentJoinDidNotWrite: leave on a Mac whose
// agent was installed another way neither stops it nor removes its plist.
func TestMetalLeaveLeavesALaunchAgentJoinDidNotWrite(t *testing.T) {
	mac := newFakeMac(t)
	paths := mac.paths()
	const production = "<plist>the Mac's own agent</plist>"
	if err := os.MkdirAll(filepath.Dir(paths.Plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Plist, []byte(production), 0o644); err != nil { //nolint:gosec // G306: a plist
		t.Fatal(err)
	}
	stdout, _, err := runMetal(t, "leave")
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if got, _ := os.ReadFile(paths.Plist); string(got) != production || len(mac.calls) != 0 {
		t.Errorf("leave left the plist %q and ran launchctl %q; want both untouched", got, mac.calls)
	}
	if !strings.Contains(stdout, "did not install it") {
		t.Errorf("leave does not say why it left the agent: %q", stdout)
	}
}

// TestMetalJoinSaysHowToStartAfterBootstrapFails: the string is used up by
// then, so the error must say how to start the agent without a new one.
func TestMetalJoinSaysHowToStartAfterBootstrapFails(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	mac.failOn = "bootstrap"
	joinString, _ := issueJoinString(t, ns)
	_, _, err := runMetal(t, "join", joinString, "--wait", "0")
	if want := "launchctl bootstrap gui/501 \"" + mac.paths().Plist + "\""; err == nil ||
		!strings.Contains(err.Error(), want) {
		t.Errorf("join with a failing bootstrap: err = %v, want it to say %s", err, want)
	}
}

// TestMetalJoinWaitsForThePreviousAgentToStop: bootout returns while a
// running agent is still stopping, and launchd refuses a bootstrap until it
// has (seen on an M3 Air rejoining with --force). A rejoin waits for the old
// agent to unload instead of failing, and the wait is bounded.
func TestMetalJoinWaitsForThePreviousAgentToStop(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	joinString, _ := issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0"); err != nil {
		t.Fatalf("first join: %v\n%s", err, stderr)
	}

	mac.stopPolls, mac.calls = 3, nil
	joinString, _ = issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0"); err != nil {
		t.Fatalf("rejoin while the previous agent stops: %v\n%s", err, stderr)
	}
	if !mac.loaded || mac.calls[len(mac.calls)-1] != "bootstrap gui/501 "+mac.paths().Plist {
		t.Errorf("launchctl calls %q, want the new agent bootstrapped last", mac.calls)
	}

	prev := metalAgentUnloadTimeout
	metalAgentUnloadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { metalAgentUnloadTimeout = prev })
	mac.stopPolls = 1 << 30
	joinString, _ = issueJoinString(t, ns)
	start := time.Now()
	_, _, err := runMetal(t, "join", joinString, "--wait", "0")
	if err == nil || !strings.Contains(err.Error(), "launchctl bootstrap gui/501") || time.Since(start) > 10*time.Second {
		t.Errorf("rejoin while the previous agent never stops: err = %v after %s, want the bootstrap hint "+
			"once the unload wait gives up", err, time.Since(start))
	}
}

// TestMetalJoinWaitTimesOutAndSaysWhy: when the cluster never verifies the
// Mac, join fails after --wait with the conditions it last saw, and leaves
// the agent installed to keep trying.
func TestMetalJoinWaitTimesOutAndSaysWhy(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	joinString, _ := issueJoinString(t, ns)
	_, _, err := runMetal(t, "join", joinString, "--wait", "3s")
	if err == nil || !strings.Contains(err.Error(), "not Reachable within 3s") ||
		!strings.Contains(err.Error(), "keeps trying") {
		t.Fatalf("join: err = %v, want a timeout that says the agent keeps trying", err)
	}
	if _, err := os.Stat(mac.paths().Plist); err != nil {
		t.Errorf("the LaunchAgent is gone after a timed-out wait: %v", err)
	}
}

// TestMetalLeaveUndoesJoin: leave stops the LaunchAgent, removes the plist
// and the agent's credentials, keeps the rest of the state directory, and
// says how to revoke the Mac in the cluster.
func TestMetalLeaveUndoesJoin(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	joinString, _ := issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0"); err != nil {
		t.Fatalf("join: %v\n%s", err, stderr)
	}
	paths := mac.paths()
	identity := filepath.Join(paths.Dir, "ingress", "ingress-key.pem")
	if err := os.MkdirAll(filepath.Dir(identity), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	mac.calls = nil

	stdout, _, err := runMetal(t, "leave")
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	for _, f := range []string{paths.Plist, paths.Kubeconfig, paths.Token} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after leave: %v, want removed", f, err)
		}
	}
	if _, err := os.Stat(identity); err != nil {
		t.Errorf("leave removed the agent's ingress identity: %v", err)
	}
	if len(mac.calls) != 2 || mac.calls[0] != "bootout gui/501/"+metalAgentLabel ||
		mac.calls[1] != "print gui/501/"+metalAgentLabel {
		t.Errorf("launchctl calls %q, want the agent booted out and waited for", mac.calls)
	}
	if !strings.Contains(stdout, "has no stop-engines") {
		t.Errorf("leave does not say why it skipped stop-engines for an agent without it: %q", stdout)
	}
	if !strings.Contains(stdout, "llmkube metal remove mac-a -n "+ns) {
		t.Errorf("leave does not say how to revoke the Mac: %q", stdout)
	}

	stdout, _, err = runMetal(t, "leave")
	if err != nil || !strings.Contains(stdout, "has not joined") {
		t.Errorf("second leave: %q, %v; want nothing to remove", stdout, err)
	}
}

func TestRenderMetalAgentPlistEscapes(t *testing.T) {
	home := t.TempDir()
	odd := filepath.Join(home, "R&D <models>", metalAgentBinary)
	raw, err := renderMetalAgentPlist([]string{odd, "--namespace", "macs"}, home, filepath.Join(home, "a&b.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "R&amp;D &lt;models&gt;") || strings.Contains(string(raw), "R&D <models>") {
		t.Errorf("a path with & and < is not escaped:\n%s", raw)
	}
	if plutil, err := exec.LookPath("plutil"); err == nil {
		path := filepath.Join(home, "agent.plist")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint: %v: %s", err, out)
		}
	}
}

func TestFindMetalAgentBinary(t *testing.T) {
	// Resolved, as findMetalAgentBinary resolves the llmkube symlink (macOS's
	// temp directory is under the /var symlink).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	onPath := exe("path/" + metalAgentBinary)
	beside := exe("cellar/bin/" + metalAgentBinary)
	llmkube := exe("cellar/bin/llmkube")
	link := filepath.Join(dir, "brew-bin-llmkube")
	if err := os.Symlink(llmkube, link); err != nil {
		t.Fatal(err)
	}
	host := func(path string) metalHost {
		return metalHost{
			lookPath: func(string) (string, error) {
				if path == "" {
					return "", exec.ErrNotFound
				}
				return path, nil
			},
			executable: func() (string, error) { return link, nil },
		}
	}
	if got, err := findMetalAgentBinary(host(onPath), ""); err != nil || got != onPath {
		t.Errorf("on PATH: %q, %v; want %s", got, err, onPath)
	}
	if got, err := findMetalAgentBinary(host(""), ""); err != nil || got != beside {
		t.Errorf("next to the symlinked llmkube: %q, %v; want %s", got, err, beside)
	}
	if _, err := findMetalAgentBinary(host(""), filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing --agent-binary was accepted")
	}
}

func TestFindLlamaServer(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	brew := exe("homebrew/llama-server")
	onPath := exe("path/llama-server")
	flagged := exe("built/llama-server")
	host := func(searched []string, path string) metalHost {
		return metalHost{
			llamaServerPaths: searched,
			lookPath: func(string) (string, error) {
				if path == "" {
					return "", exec.ErrNotFound
				}
				return path, nil
			},
		}
	}
	missing := filepath.Join(dir, "nowhere/llama-server")
	for _, tc := range []struct {
		name string
		host metalHost
		flag string
		want string
	}{
		{"the agent's search path first", host([]string{missing, brew}, onPath), "", brew},
		{"else PATH", host([]string{missing}, onPath), "", onPath},
		{"the flag over both", host([]string{brew}, onPath), flagged, flagged},
	} {
		if got, err := findLlamaServer(tc.host, tc.flag); err != nil || got != tc.want {
			t.Errorf("%s: %q, %v; want %s", tc.name, got, err, tc.want)
		}
	}
	if _, err := findLlamaServer(host([]string{missing}, ""), ""); err == nil ||
		!strings.Contains(err.Error(), "brew install llama.cpp") {
		t.Errorf("no llama-server anywhere: err = %v, want an install hint", err)
	}
	if _, err := findLlamaServer(host([]string{brew}, ""), missing); err == nil {
		t.Error("a missing --llama-server was accepted")
	}
}

// TestInstallMetalAgentKeepsThePreviousPlistOutsideLaunchAgents: rejoining
// keeps the plist it replaces, and not in LaunchAgents, where launchd could
// load the old copy at login, even after leave.
func TestInstallMetalAgentKeepsThePreviousPlistOutsideLaunchAgents(t *testing.T) {
	mac := newFakeMac(t)
	host, err := newMetalHost()
	if err != nil {
		t.Fatal(err)
	}
	paths := mac.paths()
	launchAgents := filepath.Dir(paths.Plist)
	if err := os.MkdirAll(launchAgents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Plist, []byte("previous"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The second install replaces join's own plist, which must not
	// overwrite the backup of the one join replaced first.
	for range 2 {
		if _, err := installMetalAgent(context.Background(), host, paths, []string{mac.agentBin}, io.Discard); err != nil {
			t.Fatalf("install: %v", err)
		}
	}
	if kept, err := os.ReadFile(paths.PlistBackup); err != nil || string(kept) != "previous" {
		t.Errorf("backup %s = %q, %v; want the previous plist", paths.PlistBackup, kept, err)
	}
	entries, err := os.ReadDir(launchAgents)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(paths.Plist) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("LaunchAgents holds %q, want only %s", names, filepath.Base(paths.Plist))
	}
}
