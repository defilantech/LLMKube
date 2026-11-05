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
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metaljoin"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

const (
	// metalAgentLabel is the agent's launchd label, as in
	// deployment/macos/com.llmkube.metal-agent.plist.
	metalAgentLabel = "com.llmkube.metal-agent"
	// metalAgentBinary is the agent's file name in a release and in Homebrew.
	metalAgentBinary = "llmkube-metal-agent"
	// metalAgentTokenLifetimeDefault is the agent token join requests. The
	// agent renews it at half its lifetime, so a Mac that is off for longer
	// than that needs a new join string.
	metalAgentTokenLifetimeDefault = 7 * 24 * time.Hour
	metalJoinWaitDefault           = 3 * time.Minute
	metalJoinPollInterval          = 2 * time.Second
	metalJoinRequestTimeout        = 30 * time.Second
)

// defaultLlamaServerPaths mirrors the agent's search list
// (cmd/metal-agent defaultLlamaServerPaths): Apple Silicon, then Intel
// Homebrew.
var defaultLlamaServerPaths = []string{"/opt/homebrew/bin/llama-server", "/usr/local/bin/llama-server"}

// metalHost is what join and leave need from the Mac. Tests replace it.
type metalHost struct {
	goos string
	home string
	uid  int
	// launchctl runs /bin/launchctl with args and returns its output.
	launchctl func(ctx context.Context, args ...string) (string, error)
	// lookPath finds a program on PATH.
	lookPath func(string) (string, error)
	// llamaServerPaths are where the agent looks for llama-server.
	llamaServerPaths []string
	// executable is this llmkube binary's path.
	executable func() (string, error)
	// runAgent runs the Metal agent binary bin with args, streaming its
	// output to stdout and stderr.
	runAgent func(ctx context.Context, bin string, args []string, stdout, stderr io.Writer) error
	// listen binds a TCP address, for join's check that the agent's ports
	// are free; the caller closes it.
	listen func(network, address string) (net.Listener, error)
	// portHolders lists the processes listening on a TCP port, best effort.
	portHolders func(ctx context.Context, port int) []portHolder
}

var newMetalHost = func() (metalHost, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return metalHost{}, fmt.Errorf("find the home directory: %w", err)
	}
	return metalHost{
		goos: runtime.GOOS,
		home: home,
		uid:  os.Getuid(),
		launchctl: func(ctx context.Context, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return string(out), nil
		},
		lookPath:         exec.LookPath,
		llamaServerPaths: defaultLlamaServerPaths,
		executable:       os.Executable,
		runAgent: func(ctx context.Context, bin string, args []string, stdout, stderr io.Writer) error {
			c := exec.CommandContext(ctx, bin, args...)
			c.Stdout, c.Stderr = stdout, stderr
			return c.Run()
		},
		listen:      net.Listen,
		portHolders: lsofPortHolders,
	}, nil
}

// metalAgentPaths are the files join writes on the Mac.
type metalAgentPaths struct {
	// Dir is the agent's state directory (its --state-dir default), which
	// also holds the ingress identity.
	Dir        string
	Kubeconfig string
	Token      string
	Plist      string
	// PlistBackup keeps the plist join replaced. It is not next to the
	// plist: launchd reads LaunchAgents at login, and a copy there with the
	// same Label could start the agent with the old arguments, even after
	// llmkube metal leave.
	PlistBackup string
	LogDir      string
	Log         string
}

func newMetalAgentPaths(home string) metalAgentPaths {
	dir := filepath.Join(home, "Library", "Application Support", "llmkube", "metal-agent")
	logDir := filepath.Join(home, "Library", "Logs", "llmkube")
	return metalAgentPaths{
		Dir:         dir,
		Kubeconfig:  filepath.Join(dir, "kubeconfig"),
		Token:       filepath.Join(dir, "token"),
		Plist:       filepath.Join(home, "Library", "LaunchAgents", metalAgentLabel+".plist"),
		PlistBackup: filepath.Join(dir, metalAgentLabel+".plist.bak"),
		LogDir:      logDir,
		Log:         filepath.Join(logDir, "metal-agent.log"),
	}
}

type metalJoinOptions struct {
	agentBinary   string
	llamaServer   string
	tokenLifetime time.Duration
	wait          time.Duration
	force         bool
	hostIP        string
	allowlist     string
	agentArgs     []string
	// hostIPSet and allowlistSet: the flag was given, so an empty value drops
	// the one a previous join's LaunchAgent carries.
	hostIPSet    bool
	allowlistSet bool
}

func newMetalJoinCommand() *cobra.Command {
	opts := &metalJoinOptions{}
	cmd := &cobra.Command{
		Use:   "join <join-string>",
		Short: "Join this Mac to the cluster with a join string",
		Long: `Set up the Metal agent on this Mac with the join string an admin created
with 'llmkube metal token create <mac>'.

join trusts only the CA in the join string (no system roots) and prints its
pin: compare it with the pin token create printed. It then mints the agent's
own token for the Mac's ServiceAccount, writes it and a kubeconfig that reads
it (both 0600) under ~/Library/Application Support/llmkube/metal-agent,
deletes the join Secret so the string cannot be used again, installs and
starts the com.llmkube.metal-agent LaunchAgent, and waits until the cluster
reports the Mac's MetalNode Reachable.

The agent renews its token at half its lifetime (--token-lifetime, 7 days by
default). A Mac that is off for longer needs a new join string. Running join
again replaces the earlier setup; 'llmkube metal leave' removes it.

--host-ip and --allowlist set the agent's --host-ip and
--inference-service-allowlist, and --agent-arg <flag>=<value> (repeatable)
passes any other agent flag, such as --agent-arg port=9091. A re-join keeps
every agent argument the previous join's LaunchAgent has that join does not
set itself, hand edits included, and prints them; a flag given now replaces
a kept value, and an empty --host-ip or --allowlist drops it.

join refuses to replace a com.llmkube.metal-agent LaunchAgent it did not
write (make install-metal-agent, a playbook), since that one carries the
Mac's own arguments; --force replaces it, without its arguments, and keeps a
copy for leave to point back to.

Before it uses the join string, join refuses to run as root, checks that
this user is logged in at the Mac (a LaunchAgent runs only in a GUI login
session), and checks that the ports the agent binds (--port 9090 and
--client-port 9999 on 127.0.0.1, --ingress-port 9443) are free or held by the
agent it replaces.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.hostIPSet = cmd.Flags().Changed("host-ip")
			opts.allowlistSet = cmd.Flags().Changed("allowlist")
			// A join string a terminal wrapped can arrive as several
			// arguments; Decode ignores the whitespace between them.
			return runMetalJoin(cmd, strings.Join(args, ""), opts)
		},
	}
	cmd.Flags().StringVar(&opts.agentBinary, "agent-binary", "",
		"Path to llmkube-metal-agent (default: on PATH, else next to this llmkube)")
	cmd.Flags().StringVar(&opts.llamaServer, "llama-server", "",
		"Path to llama-server, which the agent needs (default: Homebrew's, else on PATH)")
	cmd.Flags().DurationVar(&opts.tokenLifetime, "token-lifetime", metalAgentTokenLifetimeDefault,
		"Lifetime of the agent's token; the agent renews it at half (at least 10m)")
	cmd.Flags().DurationVar(&opts.wait, "wait", metalJoinWaitDefault,
		"How long to wait for the MetalNode to report Reachable; 0 does not wait")
	cmd.Flags().BoolVar(&opts.force, "force", false,
		"Replace a com.llmkube.metal-agent LaunchAgent that join did not write (it is kept in the state directory)")
	cmd.Flags().StringVar(&opts.hostIP, "host-ip", "",
		"IPv4 address the agent registers for the cluster to reach this Mac (agent --host-ip; default: auto-detect)")
	cmd.Flags().StringVar(&opts.allowlist, "allowlist", "",
		"Comma-separated InferenceServices this Mac serves (agent --inference-service-allowlist; default: all)")
	cmd.Flags().StringArrayVar(&opts.agentArgs, "agent-arg", nil,
		"Another agent flag as <flag>=<value>, such as port=9091 (repeatable; not the flags join sets itself)")
	return cmd
}

// metalJoinExtras checks the agent arguments given on the command line,
// before join looks at the Mac.
func metalJoinExtras(opts *metalJoinOptions) ([]metalJoinExtra, error) {
	var extras []metalJoinExtra
	seen := map[string]bool{}
	for _, raw := range opts.agentArgs {
		a, err := parseAgentArgFlag(raw)
		if err != nil {
			return nil, err
		}
		if seen[a.name] {
			return nil, fmt.Errorf("--agent-arg %s is given more than once", a.name)
		}
		seen[a.name] = true
		extras = append(extras, metalJoinExtra{arg: a})
	}
	if opts.hostIPSet {
		e := metalJoinExtra{arg: agentArg{name: "host-ip", value: opts.hostIP, hasValue: true}, drop: opts.hostIP == ""}
		if !e.drop {
			if err := validateMetalJoinHostIP(opts.hostIP); err != nil {
				return nil, err
			}
		}
		extras = append(extras, e)
	}
	if opts.allowlistSet {
		e := metalJoinExtra{arg: agentArg{name: "inference-service-allowlist", hasValue: true}, drop: opts.allowlist == ""}
		if !e.drop {
			list, err := normalizeMetalJoinAllowlist(opts.allowlist)
			if err != nil {
				return nil, err
			}
			e.arg.value = list
		}
		extras = append(extras, e)
	}
	return extras, nil
}

func runMetalJoin(cmd *cobra.Command, joinString string, opts *metalJoinOptions) error {
	if opts.tokenLifetime < 10*time.Minute {
		return fmt.Errorf("--token-lifetime %s is shorter than 10m, the shortest token the API server issues",
			opts.tokenLifetime)
	}
	if opts.wait < 0 {
		return fmt.Errorf("--wait %s is negative", opts.wait)
	}
	given, err := metalJoinExtras(opts)
	if err != nil {
		return err
	}
	host, err := newMetalHost()
	if err != nil {
		return err
	}
	if host.goos != "darwin" {
		return fmt.Errorf("llmkube metal join sets up the Metal agent and runs on the Mac, not on %s", host.goos)
	}
	if host.uid == 0 {
		// launchd runs the agent in the GUI session of the user who joins,
		// and gui/0 has none; as root, join would also write root-owned
		// files under whatever home sudo kept. Checked before the string is
		// even decoded, so it stays usable.
		return errors.New("run llmkube metal join as the user the agent runs as, not as root or with sudo; " +
			"the join string has not been used")
	}
	p, err := metaljoin.Decode(joinString)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out, log := cmd.OutOrStdout(), cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(log, "joining Mac %s in namespace %s at %s\n", p.Mac, p.Namespace, p.Server)
	_, _ = fmt.Fprintf(log, "CA pin: %s (compare it with the pin llmkube metal token create printed)\n", p.Pin)

	agentBin, err := findMetalAgentBinary(host, opts.agentBinary)
	if err != nil {
		return err
	}
	llamaServer, err := findLlamaServer(host, opts.llamaServer)
	if err != nil {
		return err
	}
	paths := newMetalAgentPaths(host.home)
	if err := checkMetalAgentPlist(paths, opts.force); err != nil {
		return err
	}
	plan, err := planMetalJoinAgentArgs(paths, p, llamaServer, given, log)
	if err != nil {
		return err
	}
	ports, err := metalAgentPorts(plan.extras)
	if err != nil {
		return err
	}
	if err := checkMetalAgentGUISession(ctx, host); err != nil {
		return err
	}
	if err := checkMetalAgentPorts(ctx, host, ports, log); err != nil {
		return err
	}
	if err := metalJoinSetup(ctx, p, opts.tokenLifetime, paths, log); err != nil {
		return err
	}
	started, err := installMetalAgent(ctx, host, paths, metalAgentArgs(agentBin, paths, p, llamaServer, plan), log)
	if err != nil {
		return err
	}
	if opts.wait == 0 {
		_, _ = fmt.Fprintf(out, "Mac %s joined; the agent is starting (log: %s)\n", p.Mac, paths.Log)
		return nil
	}
	agentClient, err := metalAgentClient(paths.Kubeconfig)
	if err != nil {
		return err
	}
	mn, err := waitMetalNodeReachable(ctx, agentClient, p.Namespace, p.Mac, started, opts.wait,
		metalJoinPollInterval, log)
	if err != nil {
		return fmt.Errorf("%w; the agent is installed and keeps trying (log: %s)", err, paths.Log)
	}
	_, _ = fmt.Fprintf(out, "Mac %s joined: the cluster reaches it at %s:%d (log: %s)\n",
		p.Mac, mn.Status.HostIP, mn.Status.IngressPort, paths.Log)
	return nil
}

// findMetalAgentBinary is --agent-binary, else llmkube-metal-agent on PATH,
// else next to this llmkube (a release archive, a Homebrew prefix). launchd
// needs an absolute path.
func findMetalAgentBinary(host metalHost, flag string) (string, error) {
	candidates := []string{flag}
	if flag == "" {
		candidates = nil
		if p, err := host.lookPath(metalAgentBinary); err == nil {
			candidates = append(candidates, p)
		}
		if self, err := host.executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(self); err == nil {
				self = resolved
			}
			candidates = append(candidates, filepath.Join(filepath.Dir(self), metalAgentBinary))
		}
	}
	if abs, ok := firstExecutable(candidates); ok {
		return abs, nil
	}
	if flag != "" {
		return "", fmt.Errorf("--agent-binary %s is not an executable file", flag)
	}
	return "", fmt.Errorf("%s not found on PATH or next to llmkube; install it "+
		"(brew install defilantech/tap/llmkube-metal-agent) or pass --agent-binary", metalAgentBinary)
}

// findLlamaServer is --llama-server, else the first of the agent's own search
// paths, else llama-server on PATH. The agent exits at startup without
// llama-server, which from here would only show as a Mac that never reports,
// so join refuses first. The agent gets the path join found: launchd's PATH
// is not the shell's.
func findLlamaServer(host metalHost, flag string) (string, error) {
	candidates := []string{flag}
	if flag == "" {
		candidates = append([]string(nil), host.llamaServerPaths...)
		if p, err := host.lookPath("llama-server"); err == nil {
			candidates = append(candidates, p)
		}
	}
	if abs, ok := firstExecutable(candidates); ok {
		return abs, nil
	}
	if flag != "" {
		return "", fmt.Errorf("--llama-server %s is not an executable file", flag)
	}
	return "", fmt.Errorf("llama-server not found in %s or on PATH, and the agent cannot start without it; "+
		"install it (brew install llama.cpp) or pass --llama-server", strings.Join(host.llamaServerPaths, ", "))
}

// firstExecutable returns the absolute path of the first candidate that is
// an executable regular file.
func firstExecutable(candidates []string) (string, bool) {
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if info, err := os.Stat(abs); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return abs, true
		}
	}
	return "", false
}

// metalJoinSetup redeems the join token: it mints the agent's own token,
// writes it and the kubeconfig, checks that they reach the MetalNode, and
// deletes the join Secret, which revokes the join token. The order means a
// failure before the delete leaves the join string usable for a retry.
func metalJoinSetup(ctx context.Context, p metaljoin.Payload, lifetime time.Duration, paths metalAgentPaths,
	log io.Writer) error {
	scheme, err := metalScheme()
	if err != nil {
		return err
	}
	joinCfg := metalJoinRESTConfig(p)
	joinCfg.BearerToken = p.Token
	joinClient, err := client.New(joinCfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	n := newMetalNames(p.Mac)

	seconds := int64(lifetime / time.Second)
	tr := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: n.ServiceAccount}}
	if err := joinClient.SubResource("token").Create(ctx, sa, tr); err != nil {
		return explainJoinTokenError(err)
	}
	if got, ok := jwtLifetime(tr.Status.Token); ok && got < lifetime-time.Minute {
		_, _ = fmt.Fprintf(log, "warning: the API server shortened the agent's token to %s (asked for %s); "+
			"the agent renews it at half that\n", got, lifetime)
	}

	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", paths.Dir, err)
	}
	if err := os.Chmod(paths.Dir, 0o700); err != nil { //nolint:gosec // G302: a directory, owner-only with its x bit
		return fmt.Errorf("restrict %s: %w", paths.Dir, err)
	}
	if err := writeFileAtomic(paths.Token, []byte(tr.Status.Token), 0o600); err != nil {
		return fmt.Errorf("write the agent's token: %w", err)
	}
	kubeconfig, err := metalAgentKubeconfig(p, paths.Token)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(paths.Kubeconfig, kubeconfig, 0o600); err != nil {
		return fmt.Errorf("write the agent's kubeconfig: %w", err)
	}
	_, _ = fmt.Fprintf(log, "wrote %s and its token (0600)\n", paths.Kubeconfig)

	agentClient, err := metalAgentClient(paths.Kubeconfig)
	if err != nil {
		return err
	}
	mnKey := client.ObjectKey{Namespace: p.Namespace, Name: n.MetalNode}
	if err := agentClient.Get(ctx, mnKey, &inferencev1alpha1.MetalNode{}); err != nil {
		return fmt.Errorf("the agent's new credentials cannot read MetalNode %s/%s: %w", p.Namespace, n.MetalNode, err)
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: n.JoinSecret}}
	if err := agentClient.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete the join Secret %s/%s, which makes the join string single use: %w; "+
			"the string keeps working until it expires, so run join again or ask an admin to delete the Secret",
			p.Namespace, n.JoinSecret, err)
	}
	_, _ = fmt.Fprintf(log, "deleted the join Secret %s/%s: the join string no longer works\n", p.Namespace, n.JoinSecret)
	return nil
}

// explainJoinTokenError turns the API server's answer to the join token into
// what the Mac's user can do about it. The join token is bound to the join
// Secret, so a used string and an expired one look the same.
func explainJoinTokenError(err error) error {
	switch {
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("the join string has expired or was already used; ask for a new one "+
			"(llmkube metal token create): %w", err)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("the join token may not mint the Mac's token; was the per-Mac Role changed? "+
			"Ask for a new join string (llmkube metal token create): %w", err)
	}
	return fmt.Errorf("mint the agent's token: %w", err)
}

// metalJoinRESTConfig is a client config that trusts only the join string's
// CA, with no credentials.
func metalJoinRESTConfig(p metaljoin.Payload) *rest.Config {
	return &rest.Config{
		Host:    p.Server,
		Timeout: metalJoinRequestTimeout,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.CA}),
			ServerName: p.TLSServerName,
		},
	}
}

// metalAgentKubeconfig is the agent's kubeconfig: the join string's server
// and CA, and the token read from tokenFile, which the agent renews in place.
func metalAgentKubeconfig(p metaljoin.Payload, tokenFile string) ([]byte, error) {
	const name = "llmkube-metal"
	user := "llmkube-metal-" + p.Mac
	kc := clientcmdapi.NewConfig()
	kc.Clusters[name] = &clientcmdapi.Cluster{
		Server:                   p.Server,
		CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.CA}),
		TLSServerName:            p.TLSServerName,
	}
	kc.AuthInfos[user] = &clientcmdapi.AuthInfo{TokenFile: tokenFile}
	kc.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: user, Namespace: p.Namespace}
	kc.CurrentContext = name
	raw, err := clientcmd.Write(*kc)
	if err != nil {
		return nil, fmt.Errorf("render the agent's kubeconfig: %w", err)
	}
	return raw, nil
}

func metalAgentClient(kubeconfig string) (client.Client, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", kubeconfig, err)
	}
	cfg.Timeout = metalJoinRequestTimeout
	scheme, err := metalScheme()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}
	return c, nil
}

// metalJoinSets are the agent flags join writes from the join string and what
// it found on the Mac.
func metalJoinSets(paths metalAgentPaths, p metaljoin.Payload, llamaServer string) map[string]agentArg {
	sets := map[string]agentArg{}
	for _, a := range []agentArg{
		{name: "kubeconfig", value: paths.Kubeconfig, hasValue: true},
		{name: "namespace", value: p.Namespace, hasValue: true},
		{name: "metal-node", value: p.Mac, hasValue: true},
		{name: "renew-service-account-token"},
		{name: "llama-server", value: llamaServer, hasValue: true},
	} {
		sets[a.name] = a
	}
	return sets
}

// planMetalJoinAgentArgs is the agent's command line beyond what join sets:
// what the previous join's LaunchAgent carries, with the flags given now
// over it. It prints what it keeps, replaces and drops.
func planMetalJoinAgentArgs(paths metalAgentPaths, p metaljoin.Payload, llamaServer string, given []metalJoinExtra,
	log io.Writer) (metalJoinArgPlan, error) {
	previous, err := previousJoinAgentArgs(paths.Plist)
	if err != nil {
		return metalJoinArgPlan{}, err
	}
	if exists, joined, _ := readMetalAgentPlist(paths.Plist); exists && !joined {
		_, _ = fmt.Fprintf(log, "join does not keep the arguments of the LaunchAgent it replaces (it keeps the file "+
			"as %s); pass the ones this Mac needs with --host-ip, --allowlist or --agent-arg\n", paths.PlistBackup)
	}
	plan := planMetalAgentArgs(previous, given, metalJoinSets(paths, p, llamaServer))
	for _, n := range plan.notes {
		_, _ = fmt.Fprintln(log, n)
	}
	return plan, nil
}

// metalAgentArgs is the agent's command line in the LaunchAgent: the flags
// join sets, then plan's.
func metalAgentArgs(agentBin string, paths metalAgentPaths, p metaljoin.Payload, llamaServer string,
	plan metalJoinArgPlan) []string {
	args := make([]string, 0, 12+len(plan.extras))
	args = append(args, agentBin,
		"--kubeconfig", paths.Kubeconfig,
		"--namespace", p.Namespace,
		"--metal-node", p.Mac,
		"--renew-service-account-token",
		"--log-level", plan.logLevel,
		"--llama-server", llamaServer,
	)
	for _, a := range plan.extras {
		args = append(args, a.String())
	}
	return args
}

// metalJoinPlistMarker marks a LaunchAgent plist join wrote. join and leave
// replace or remove only a plist that carries it: one installed another way
// (make install-metal-agent, a playbook) runs the agent with the Mac's own
// arguments, such as its model store and allowlist.
const metalJoinPlistMarker = "<!-- Written by llmkube metal join; llmkube metal leave removes it. -->"

// readMetalAgentPlist reports whether the LaunchAgent plist exists and
// whether join wrote it.
func readMetalAgentPlist(path string) (exists, joined bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read %s: %w", path, err)
	}
	return true, isJoinPlist(raw), nil
}

func isJoinPlist(raw []byte) bool { return bytes.Contains(raw, []byte(metalJoinPlistMarker)) }

// checkMetalAgentPlist refuses, before join changes anything, to replace a
// LaunchAgent join did not write, unless force.
func checkMetalAgentPlist(paths metalAgentPaths, force bool) error {
	exists, joined, err := readMetalAgentPlist(paths.Plist)
	if err != nil || !exists || joined || force {
		return err
	}
	return fmt.Errorf("%s runs a Metal agent that llmkube metal join did not install "+
		"(make install-metal-agent or a playbook), with this Mac's own arguments; join would replace it. "+
		"Pass --force to replace it (join keeps it as %s), or join another Mac", paths.Plist, paths.PlistBackup)
}

// installMetalAgent writes the LaunchAgent plist and (re)starts it. A plist
// join did not write is kept as paths.PlistBackup; an earlier join's is not,
// so a backup is never overwritten by join's own. It returns when the old
// agent had been booted out: the new agent's status is newer than that.
func installMetalAgent(ctx context.Context, host metalHost, paths metalAgentPaths, args []string,
	log io.Writer) (time.Time, error) {
	if err := os.MkdirAll(paths.LogDir, 0o700); err != nil {
		return time.Time{}, fmt.Errorf("create %s: %w", paths.LogDir, err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.Plist), 0o755); err != nil {
		return time.Time{}, fmt.Errorf("create %s: %w", filepath.Dir(paths.Plist), err)
	}
	if old, err := os.ReadFile(paths.Plist); err == nil && !isJoinPlist(old) {
		if err := os.MkdirAll(filepath.Dir(paths.PlistBackup), 0o700); err != nil {
			return time.Time{}, fmt.Errorf("create %s: %w", filepath.Dir(paths.PlistBackup), err)
		}
		if err := writeFileAtomic(paths.PlistBackup, old, 0o600); err != nil {
			return time.Time{}, fmt.Errorf("keep the previous LaunchAgent as %s: %w", paths.PlistBackup, err)
		}
		_, _ = fmt.Fprintf(log, "kept the previous LaunchAgent as %s\n", paths.PlistBackup)
	}
	plist, err := renderMetalAgentPlist(args, host.home, paths.Log)
	if err != nil {
		return time.Time{}, err
	}
	if err := writeFileAtomic(paths.Plist, plist, 0o644); err != nil {
		return time.Time{}, fmt.Errorf("write %s: %w", paths.Plist, err)
	}
	domain := "gui/" + strconv.Itoa(host.uid)
	// Not loaded yet is the usual case for bootout; anything else surfaces
	// when bootstrap fails.
	_, _ = host.launchctl(ctx, "bootout", domain+"/"+metalAgentLabel)
	_ = waitMetalAgentUnloaded(ctx, host, domain+"/"+metalAgentLabel)
	// Truncated to the Lease's renewTime precision, so a renewal in the
	// same microsecond still counts as the new agent's.
	started := time.Now().Truncate(time.Microsecond)
	if _, err := host.launchctl(ctx, "bootstrap", domain, paths.Plist); err != nil {
		return time.Time{}, fmt.Errorf("start the agent: %w; the join string is used up but the agent's "+
			"credentials are in place, so once that is fixed start it with: launchctl bootstrap %s %q",
			err, domain, paths.Plist)
	}
	_, _ = fmt.Fprintf(log, "started the %s LaunchAgent (%s)\n", metalAgentLabel, paths.Plist)
	return started, nil
}

// metalAgentUnloadTimeout bounds the wait for launchd to finish unloading the
// previous agent. launchd's default ExitTimeOut is 20 s before it kills the
// process.
var metalAgentUnloadTimeout = 30 * time.Second

const metalAgentUnloadPoll = 100 * time.Millisecond

// waitMetalAgentUnloaded waits until launchd no longer knows service, and
// reports whether it got there. bootout returns while a running agent is still
// stopping, and a bootstrap then fails with "Bootstrap failed: 5: Input/output
// error". Past the timeout it returns false; join bootstraps anyway, and the
// bootstrap error says how to start the agent.
func waitMetalAgentUnloaded(ctx context.Context, host metalHost, service string) bool {
	deadline := time.Now().Add(metalAgentUnloadTimeout)
	for {
		if _, err := host.launchctl(ctx, "print", service); err != nil {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(metalAgentUnloadPoll):
		}
	}
}

// renderMetalAgentPlist renders the LaunchAgent, matching
// deployment/macos/com.llmkube.metal-agent.plist apart from the arguments.
// Every value is XML-escaped: paths under a home directory can hold any
// character.
func renderMetalAgentPlist(args []string, home, logFile string) ([]byte, error) {
	esc := func(s string) (string, error) {
		var b bytes.Buffer
		if err := xml.EscapeText(&b, []byte(s)); err != nil {
			return "", err
		}
		return b.String(), nil
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
` + metalJoinPlistMarker + `
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + metalAgentLabel + `</string>
    <key>ProgramArguments</key>
    <array>
`)
	for _, a := range args {
		v, err := esc(a)
		if err != nil {
			return nil, err
		}
		b.WriteString("        <string>" + v + "</string>\n")
	}
	logEsc, err := esc(logFile)
	if err != nil {
		return nil, err
	}
	homeEsc, err := esc(home)
	if err != nil {
		return nil, err
	}
	b.WriteString(`    </array>
    <key>AssociatedBundleIdentifiers</key>
    <string>dev.llmkube.metal-agent</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>` + logEsc + `</string>
    <key>StandardErrorPath</key>
    <string>` + logEsc + `</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
        <key>GGML_METAL_ENABLE</key>
        <string>1</string>
    </dict>
    <key>WorkingDirectory</key>
    <string>` + homeEsc + `</string>
    <key>ThrottleInterval</key>
    <integer>30</integer>
    <key>Nice</key>
    <integer>-5</integer>
</dict>
</plist>
`)
	return []byte(b.String()), nil
}

// waitMetalNodeReachable polls the Mac's MetalNode and its Lease until they
// show the agent join started connected and verified Reachable, printing
// each change. Status left from an earlier agent does not count: on a
// re-join it stays True for minutes after that agent is gone.
func waitMetalNodeReachable(ctx context.Context, c client.Client, namespace, name string, started time.Time,
	timeout, interval time.Duration, log io.Writer) (*inferencev1alpha1.MetalNode, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, _ = fmt.Fprintf(log, "waiting up to %s for the cluster to verify it can reach this Mac\n", timeout)
	last := ""
	for {
		mn := &inferencev1alpha1.MetalNode{}
		err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, mn)
		if err == nil {
			lease := &coordinationv1.Lease{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: mn.Spec.LeaseName}, lease); err != nil {
				lease = nil
			}
			done, why := metalJoinVerified(mn, lease, started)
			state := metalNodeState(mn)
			if why != "" {
				state += "; " + why
			}
			if state != last {
				_, _ = fmt.Fprintf(log, "  %s\n", state)
				last = state
			}
			if done {
				return mn, nil
			}
		} else if ctx.Err() == nil {
			_, _ = fmt.Fprintf(log, "  get MetalNode: %v\n", err)
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("MetalNode %s/%s was not Reachable within %s (last: %s)", namespace, name, timeout, last)
			}
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// metalJoinVerified reports whether mn and its Lease show the agent started
// at started connected and verified Reachable, and if not, what it still
// waits for (empty while the conditions themselves say it). The agent stamps
// the Lease's renewTime with this Mac's clock, the clock started comes from;
// the controller copies the Lease report's ingress key and port into status.
func metalJoinVerified(mn *inferencev1alpha1.MetalNode, lease *coordinationv1.Lease,
	started time.Time) (bool, string) {
	gen := mn.Generation
	if mn.Status.ObservedGeneration != gen {
		return false, fmt.Sprintf("waiting for the controller to observe generation %d", gen)
	}
	if lease == nil || lease.Spec.RenewTime == nil || lease.Spec.RenewTime.Time.Before(started) {
		return false, "waiting for the agent this join started to renew its Lease"
	}
	current := func(condType string) bool {
		c := meta.FindStatusCondition(mn.Status.Conditions, condType)
		return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == gen
	}
	if !current(inferencev1alpha1.MetalNodeConditionAgentConnected) ||
		!current(inferencev1alpha1.MetalNodeConditionReachable) {
		return false, ""
	}
	report, err := metalnode.Decode(lease.Annotations[metalnode.ReportAnnotation])
	if err != nil {
		return false, "the agent's report on its Lease is invalid"
	}
	st := mn.Status
	if st.HostIP == "" || st.IngressSPKI != report.IngressSPKI || st.IngressPort != report.IngressPort {
		return false, "Reachable is for an earlier report; waiting for the new agent's ingress to be verified"
	}
	if st.HostIP == mn.Spec.HostIP {
		return true, ""
	}
	for _, cand := range report.Candidates {
		if cand.Address == st.HostIP {
			return true, ""
		}
	}
	return false, "Reachable names an address the agent no longer offers"
}

func metalNodeState(mn *inferencev1alpha1.MetalNode) string {
	part := func(condType string) string {
		c := meta.FindStatusCondition(mn.Status.Conditions, condType)
		if c == nil {
			return condType + " not reported yet"
		}
		return fmt.Sprintf("%s=%s (%s): %s", condType, c.Status, c.Reason, c.Message)
	}
	return part(inferencev1alpha1.MetalNodeConditionAgentConnected) + "; " +
		part(inferencev1alpha1.MetalNodeConditionReachable)
}

func newMetalLeaveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "leave",
		Short: "Stop the Metal agent on this Mac and remove what join set up",
		Long: `Stop the com.llmkube.metal-agent LaunchAgent and remove its plist and the
agent's kubeconfig and token that 'llmkube metal join' wrote. Engines outlive
the agent, so once it has exited, leave runs the agent's stop-engines to stop
the engines it started and withdraw their endpoints. If that fails, nothing is
removed; run leave again. The agent's ingress identity and model store stay. A
LaunchAgent join did not write (make install-metal-agent, a playbook) is left
running, and its engines with it.

This only cleans up the Mac: the agent's token stays valid until it expires.
To revoke it and delete the Mac's objects in the cluster, an admin runs
'llmkube metal remove <mac> -n <namespace>'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMetalLeave(cmd)
		},
	}
}

func runMetalLeave(cmd *cobra.Command) error {
	host, err := newMetalHost()
	if err != nil {
		return err
	}
	if host.goos != "darwin" {
		return fmt.Errorf("llmkube metal leave runs on the Mac, not on %s", host.goos)
	}
	if host.uid == 0 {
		// leave runs the agent binary its LaunchAgent names, a path the user
		// can write; as root that would run it with root's privileges.
		return errors.New("run llmkube metal leave as the user the agent runs as, not as root or with sudo")
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	paths := newMetalAgentPaths(host.home)
	// Read which Mac this was before the kubeconfig goes, for the hint.
	mac, namespace := "<mac>", "<namespace>"
	if kc, err := clientcmd.LoadFromFile(paths.Kubeconfig); err == nil {
		if ctxCfg := kc.Contexts[kc.CurrentContext]; ctxCfg != nil {
			namespace = ctxCfg.Namespace
			if m, ok := strings.CutPrefix(ctxCfg.AuthInfo, "llmkube-metal-"); ok {
				mac = m
			}
		}
	}
	exists, joined, err := readMetalAgentPlist(paths.Plist)
	if err != nil {
		return err
	}
	files := []string{paths.Kubeconfig, paths.Token}
	domain := "gui/" + strconv.Itoa(host.uid)
	switch {
	case joined:
		service := domain + "/" + metalAgentLabel
		if _, err := host.launchctl(ctx, "bootout", service); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "the agent was not running (%v)\n", err)
		}
		if err := stopMetalEngines(ctx, host, paths, service, out, cmd.ErrOrStderr()); err != nil {
			return err
		}
		files = append([]string{paths.Plist}, files...)
	case exists:
		_, _ = fmt.Fprintf(out, "left %s running: llmkube metal join did not install it\n", paths.Plist)
	}
	removed := 0
	for _, f := range files {
		err := os.Remove(f)
		switch {
		case err == nil:
			removed++
			_, _ = fmt.Fprintf(out, "removed %s\n", f)
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("remove %s: %w", f, err)
		}
	}
	if removed == 0 {
		_, _ = fmt.Fprintln(out, "nothing to remove: this Mac has not joined")
		return nil
	}
	_, _ = fmt.Fprintf(out, "left the cluster on this Mac; to revoke its token and delete its objects, "+
		"an admin runs: llmkube metal remove %s -n %s\n", mac, namespace)
	if _, err := os.Stat(paths.PlistBackup); joined && err == nil {
		_, _ = fmt.Fprintf(out, "join had replaced a LaunchAgent; to start it again: cp %q %q && launchctl bootstrap %s %q\n",
			paths.PlistBackup, paths.Plist, domain, paths.Plist)
	}
	return nil
}

// stopMetalEngines runs the joined agent's stop-engines: its engines outlive
// it (#2079), so leaving would otherwise keep every model loaded and its
// endpoints registered. It waits for the agent to exit first, since a running
// agent can start an engine after stop-engines has read the records, and runs
// before the kubeconfig goes, which stop-engines withdraws endpoints with.
// Nothing is removed when it fails, so leave can be run again.
func stopMetalEngines(ctx context.Context, host metalHost, paths metalAgentPaths, service string,
	out, errOut io.Writer) error {
	if !waitMetalAgentUnloaded(ctx, host, service) {
		return fmt.Errorf("the agent is still running %s after launchctl bootout, so its engines were not stopped "+
			"and nothing was removed; run llmkube metal leave again once launchctl print %s no longer finds it",
			metalAgentUnloadTimeout, service)
	}
	raw, err := os.ReadFile(paths.Plist)
	if err != nil {
		return fmt.Errorf("read %s: %w", paths.Plist, err)
	}
	args, err := metalAgentPlistArgs(raw)
	if err == nil && len(args) == 0 {
		err = errors.New("ProgramArguments is empty")
	}
	if err != nil {
		return fmt.Errorf("read the agent's command line from %s: %w", paths.Plist, err)
	}
	bin := args[0]
	supported, err := agentHasStopEngines(bin)
	switch {
	case errors.Is(err, os.ErrNotExist):
		_, _ = fmt.Fprintf(errOut, "WARNING: the agent binary %s is gone, so the engines it started were not stopped; "+
			"check for engines still running (their records are under %s)\n", bin, filepath.Join(paths.Dir, "engines"))
		return nil
	case err != nil:
		return fmt.Errorf("read the agent binary %s: %w", bin, err)
	case !supported:
		// It would read stop-engines as a stray argument and start as an
		// agent. An agent that old stops its engines when it exits.
		_, _ = fmt.Fprintf(out, "%s has no stop-engines; an agent that old stops its engines when it exits\n", bin)
		return nil
	}
	stopArgs := metaljoin.StopEnginesArgs(args[1:], paths.Kubeconfig)
	_, _ = fmt.Fprintf(out, "stopping the engines the agent started: %s %s\n", bin, strings.Join(stopArgs, " "))
	if err := host.runAgent(ctx, bin, stopArgs, out, errOut); err != nil {
		return fmt.Errorf("%s stop-engines: %w; an engine may still be running, so nothing was removed. "+
			"Check its output above, then run llmkube metal leave again", bin, err)
	}
	return nil
}

// agentHasStopEngines reports whether the agent binary at path has the
// stop-engines subcommand, without running it: an agent from before it parses
// the word as a stray argument and starts serving.
func agentHasStopEngines(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return bytes.Contains(raw, []byte(metaljoin.StopEnginesCommand)), nil
}

// metalAgentPlistArgs returns the ProgramArguments of a LaunchAgent plist.
func metalAgentPlistArgs(raw []byte) ([]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var (
		key, text      string
		inArgs, inText bool
		args           []string
	)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no ProgramArguments")
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			inText, text = true, ""
			if t.Name.Local == "array" && key == "ProgramArguments" {
				inArgs = true
			}
		case xml.CharData:
			if inText {
				text += string(t)
			}
		case xml.EndElement:
			inText = false
			switch {
			case t.Name.Local == "key":
				key = text
			case inArgs && t.Name.Local == "string":
				args = append(args, text)
			case inArgs && t.Name.Local == "array":
				return args, nil
			}
		}
	}
}

// writeFileAtomic replaces path with data through a temporary file in the
// same directory: the agent re-reads its token and launchd its plist, and
// neither may see a partial write.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
