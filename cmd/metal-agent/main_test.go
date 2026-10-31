package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"
	restclient "k8s.io/client-go/rest"

	"github.com/defilantech/llmkube/pkg/agent"
	"github.com/defilantech/llmkube/pkg/metaljoin"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

// TestMain clears the agent's system temp roots: the store tests here build
// their store in t.TempDir(), which is under /tmp wherever TMPDIR is unset
// (Linux CI), and the store check would refuse all of them. The /tmp refusal
// test sets it back explicitly.
func TestMain(m *testing.M) {
	agent.SetSystemTempRootsForTest(nil)
	os.Exit(m.Run())
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected zapcore.Level
	}{
		{name: "debug", input: "debug", expected: zapcore.DebugLevel},
		{name: "info", input: "info", expected: zapcore.InfoLevel},
		{name: "warn", input: "warn", expected: zapcore.WarnLevel},
		{name: "warning", input: "warning", expected: zapcore.WarnLevel},
		{name: "error", input: "error", expected: zapcore.ErrorLevel},
		{name: "empty defaults info", input: "", expected: zapcore.InfoLevel},
		{name: "unknown defaults info", input: "unknown", expected: zapcore.InfoLevel},
		{name: "mixed case debug", input: "DEBUG", expected: zapcore.DebugLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := parseLogLevel(tt.input)
			if got != tt.expected {
				t.Fatalf("parseLogLevel(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestResolveLlamaServerBin(t *testing.T) {
	// Save and restore the original statFunc and defaultLlamaServerPaths
	origStat := statFunc
	origPaths := defaultLlamaServerPaths
	t.Cleanup(func() {
		statFunc = origStat
		defaultLlamaServerPaths = origPaths
	})

	t.Run("explicit override is returned as-is", func(t *testing.T) {
		got, err := resolveLlamaServerBin("/custom/path/llama-server")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/custom/path/llama-server" {
			t.Fatalf("got %q, want /custom/path/llama-server", got)
		}
	})

	t.Run("finds first candidate", func(t *testing.T) {
		defaultLlamaServerPaths = []string{"/first/llama-server", "/second/llama-server"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/first/llama-server" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}

		got, err := resolveLlamaServerBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/first/llama-server" {
			t.Fatalf("got %q, want /first/llama-server", got)
		}
	})

	t.Run("falls through to second candidate", func(t *testing.T) {
		defaultLlamaServerPaths = []string{"/first/llama-server", "/second/llama-server"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/second/llama-server" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}

		got, err := resolveLlamaServerBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/second/llama-server" {
			t.Fatalf("got %q, want /second/llama-server", got)
		}
	})

	t.Run("returns error when no candidate found", func(t *testing.T) {
		defaultLlamaServerPaths = []string{"/nope/llama-server"}
		statFunc = func(string) (os.FileInfo, error) {
			return nil, errors.New("not found")
		}

		_, err := resolveLlamaServerBin("")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestResolveTensorFoldBin(t *testing.T) {
	origStat := statFunc
	origPaths := defaultTensorFoldPaths
	origHome := userHomeDir
	t.Cleanup(func() {
		statFunc = origStat
		defaultTensorFoldPaths = origPaths
		userHomeDir = origHome
	})
	userHomeDir = func() (string, error) { return "/Users/tester", nil }

	t.Run("explicit override is returned as-is", func(t *testing.T) {
		statFunc = func(string) (os.FileInfo, error) { return nil, errors.New("not found") }
		got, err := resolveTensorFoldBin("/custom/tensorfold")
		if err != nil || got != "/custom/tensorfold" {
			t.Fatalf("got %q, %v; want /custom/tensorfold", got, err)
		}
	})

	t.Run("expands ~ and finds the uv tool install first", func(t *testing.T) {
		defaultTensorFoldPaths = []string{"~/.local/bin/tensorfold", "/opt/homebrew/bin/tensorfold"}
		var probed []string
		statFunc = func(name string) (os.FileInfo, error) {
			probed = append(probed, name)
			if name == "/Users/tester/.local/bin/tensorfold" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}
		got, err := resolveTensorFoldBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/Users/tester/.local/bin/tensorfold" {
			t.Fatalf("got %q (probed %v), want /Users/tester/.local/bin/tensorfold", got, probed)
		}
	})

	t.Run("falls through to homebrew", func(t *testing.T) {
		defaultTensorFoldPaths = []string{"~/.local/bin/tensorfold", "/opt/homebrew/bin/tensorfold"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/opt/homebrew/bin/tensorfold" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}
		got, err := resolveTensorFoldBin("")
		if err != nil || got != "/opt/homebrew/bin/tensorfold" {
			t.Fatalf("got %q, %v; want /opt/homebrew/bin/tensorfold", got, err)
		}
	})

	t.Run("error names the install command and the flag", func(t *testing.T) {
		defaultTensorFoldPaths = []string{"~/.local/bin/tensorfold"}
		statFunc = func(string) (os.FileInfo, error) { return nil, errors.New("not found") }
		_, err := resolveTensorFoldBin("")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		for _, want := range []string{"uv tool install", "--tensorfold-bin"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
	})
}

func TestResolveModelStorePath(t *testing.T) {
	origHome := userHomeDir
	t.Cleanup(func() { userHomeDir = origHome })

	t.Run("empty flag defaults under the home directory", func(t *testing.T) {
		userHomeDir = func() (string, error) { return "/Users/tester", nil }
		got, err := resolveModelStorePath("")
		want := "/Users/tester/Library/Application Support/llmkube/models"
		if err != nil || got != want {
			t.Fatalf("resolveModelStorePath(\"\") = %q, %v; want %q", got, err, want)
		}
	})

	t.Run("explicit flag is kept and home is not consulted", func(t *testing.T) {
		userHomeDir = func() (string, error) { return "", errors.New("no home") }
		got, err := resolveModelStorePath("/Volumes/models")
		if err != nil || got != "/Volumes/models" {
			t.Fatalf("got %q, %v; want /Volumes/models", got, err)
		}
	})

	t.Run("a bare ~ expands to the home directory", func(t *testing.T) {
		userHomeDir = func() (string, error) { return "/Users/tester", nil }
		got, err := resolveModelStorePath("~")
		if err != nil || got != "/Users/tester" {
			t.Fatalf("resolveModelStorePath(\"~\") = %q, %v; want /Users/tester", got, err)
		}
	})

	t.Run("a leading ~/ expands against the home directory", func(t *testing.T) {
		userHomeDir = func() (string, error) { return "/Users/tester", nil }
		got, err := resolveModelStorePath("~/models")
		if err != nil || got != "/Users/tester/models" {
			t.Fatalf("resolveModelStorePath(\"~/models\") = %q, %v; want /Users/tester/models", got, err)
		}
	})

	t.Run("~ with unknown home is an error", func(t *testing.T) {
		userHomeDir = func() (string, error) { return "", errors.New("no home") }
		if got, err := resolveModelStorePath("~/models"); err == nil {
			t.Fatalf("got %q, nil; want an error", got)
		}
	})

	t.Run("a relative path is refused", func(t *testing.T) {
		userHomeDir = func() (string, error) { return "/Users/tester", nil }
		for _, rel := range []string{"models", "./models", "~user/models"} {
			got, err := resolveModelStorePath(rel)
			if err == nil {
				t.Fatalf("resolveModelStorePath(%q) = %q, nil; want an error", rel, got)
			}
			if !strings.Contains(err.Error(), "absolute") {
				t.Errorf("error %q does not say the path must be absolute", err)
			}
		}
	})

	t.Run("empty flag with unknown home is an error", func(t *testing.T) {
		userHomeDir = func() (string, error) { return "", errors.New("no home") }
		if got, err := resolveModelStorePath(""); err == nil {
			t.Fatalf("got %q, nil; want an error", got)
		}
	})
}

func TestPrepareModelStore(t *testing.T) {
	t.Run("creates a missing store and its parents 0700", func(t *testing.T) {
		base := t.TempDir()
		store := filepath.Join(base, "Application Support", "llmkube", "models")
		if _, err := prepareModelStore(store); err != nil {
			t.Fatalf("prepareModelStore = %v", err)
		}
		for _, p := range []string{store, filepath.Dir(store), filepath.Dir(filepath.Dir(store))} {
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0o700 {
				t.Errorf("%s mode = %o, want 0700", p, perm)
			}
		}
	})

	t.Run("refuses an existing group-writable store", func(t *testing.T) {
		store := filepath.Join(t.TempDir(), "models")
		if err := os.Mkdir(store, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(store, 0o775); err != nil { //nolint:gosec // G302: the fixture must be group-writable
			t.Fatal(err)
		}
		_, err := prepareModelStore(store)
		if err == nil || !strings.Contains(err.Error(), "chmod go-w") {
			t.Fatalf("prepareModelStore(0775) = %v, want a refusal naming the fix", err)
		}
	})

	t.Run("keeps an existing owned 0755 store", func(t *testing.T) {
		store := filepath.Join(t.TempDir(), "models")
		if err := os.Mkdir(store, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(store, 0o755); err != nil { //nolint:gosec // G302: a pre-existing 0755 store must stay accepted
			t.Fatal(err)
		}
		if _, err := prepareModelStore(store); err != nil {
			t.Fatalf("prepareModelStore(owned 0755) = %v, want nil", err)
		}
	})
}

// prepareModelStore refuses a store under /tmp, which the 0.10.0 plist
// pinned, even though it creates the store 0700 and owns it.
func TestPrepareModelStore_RefusesStoreUnderTmp(t *testing.T) {
	prev := agent.SetSystemTempRootsForTest([]string{"/private/tmp", "/tmp", "/private/var/tmp", "/var/tmp"})
	t.Cleanup(func() { agent.SetSystemTempRootsForTest(prev) })

	base, err := os.MkdirTemp("/tmp", "llmkube-prepare-test-")
	if err != nil {
		t.Skipf("cannot create a directory under /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	store := filepath.Join(base, "llmkube-models")
	_, err = prepareModelStore(store)
	if err == nil || !strings.Contains(err.Error(), "make install-metal-agent") {
		t.Fatalf("prepareModelStore(%s) = %v, want a refusal naming the plist re-render", store, err)
	}
}

// SetSystemTempRootsForTest is the cross-package override this package's
// store tests rely on: clearing it lets a store under /tmp through, and
// restoring the production roots refuses the same store.
func TestSetSystemTempRootsForTest_ControlsRefusal(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "llmkube-roots-seam-")
	if err != nil {
		t.Skipf("cannot create a directory under /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	store := filepath.Join(base, "models")
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}

	prev := agent.SetSystemTempRootsForTest(nil)
	t.Cleanup(func() { agent.SetSystemTempRootsForTest(prev) })
	if err := agent.CheckModelStore(store); err != nil {
		t.Fatalf("with the roots cleared, CheckModelStore(store under /tmp) = %v, want nil", err)
	}

	agent.SetSystemTempRootsForTest([]string{"/private/tmp", "/tmp", "/private/var/tmp", "/var/tmp"})
	err = agent.CheckModelStore(store)
	if err == nil || !strings.Contains(err.Error(), "/tmp") {
		t.Fatalf("with the production roots restored, CheckModelStore(store under /tmp) = %v, "+
			"want a refusal naming /tmp", err)
	}
}

// configureModelStore pins cfg.ModelStorePath to the checked, resolved
// directory: repointing the configured symlink afterwards must not change
// the path the agent (NewMetalAgent, executors) goes on to use.
func TestConfigureModelStore_PinsResolvedStore(t *testing.T) {
	base := t.TempDir()
	good := filepath.Join(base, "good")
	evil := filepath.Join(base, "evil")
	for _, d := range []string{good, evil} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "store-link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	cfg := &AgentConfig{ModelStorePath: link}
	if err := configureModelStore(cfg); err != nil {
		t.Fatalf("configureModelStore = %v", err)
	}
	want, _ := filepath.EvalSymlinks(good)
	if cfg.ModelStorePath != want {
		t.Fatalf("cfg.ModelStorePath = %q, want the resolved dir %q", cfg.ModelStorePath, want)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, link); err != nil {
		t.Fatal(err)
	}
	if cfg.ModelStorePath != want {
		t.Fatalf("cfg.ModelStorePath moved with the symlink: %q", cfg.ModelStorePath)
	}
}

func TestConfigureModelStore_RefusesRelative(t *testing.T) {
	cfg := &AgentConfig{ModelStorePath: "models"}
	if err := configureModelStore(cfg); err == nil {
		t.Fatal("configureModelStore(relative) = nil, want an error")
	}
}

func TestResolveMaxDownloadBytes(t *testing.T) {
	const total = 128 << 30
	memory := func(n uint64, err error) func() (uint64, error) {
		return func() (uint64, error) { return n, err }
	}
	for _, tc := range []struct {
		name    string
		flag    string
		total   func() (uint64, error)
		want    int64
		wantErr string
	}{
		{name: "unset defaults to total memory", flag: "", total: memory(total, nil), want: total},
		{name: "unset with unknown memory is an error", flag: "", total: memory(0, errors.New("no sysctl")),
			wantErr: "set it explicitly"},
		{name: "unset with zero memory is an error, not an uncapped agent", flag: "", total: memory(0, nil),
			wantErr: "set it explicitly"},
		{name: "0 disables the cap", flag: "0", want: 0},
		{name: "binary quantity", flag: "64Gi", want: 64 << 30},
		{name: "decimal quantity", flag: "200G", want: 200_000_000_000},
		{name: "plain byte count", flag: "137438953472", want: 137438953472},
		{name: "negative is refused", flag: "-1Gi", wantErr: "non-negative"},
		{name: "fractional bytes are refused", flag: "0.5", wantErr: "whole"},
		{name: "beyond int64 is refused", flag: "100Ei", wantErr: "whole"},
		{name: "garbage is refused", flag: "lots", wantErr: "quantity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			totalMemory := tc.total
			if totalMemory == nil {
				totalMemory = func() (uint64, error) {
					t.Fatal("total memory consulted although the flag was set")
					return 0, nil
				}
			}
			got, err := resolveMaxDownloadBytes(tc.flag, totalMemory)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolveMaxDownloadBytes(%q) = %d, %v; want an error containing %q",
						tc.flag, got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolveMaxDownloadBytes(%q) = %d, %v; want %d", tc.flag, got, err, tc.want)
			}
		})
	}
}

// TestValidateMetalNodeIngress: a --metal-node agent must refuse at startup
// exactly the ingress ports its report cannot carry, judged by the real
// encoder and decoder the reporter and controller use, so a port such as 443
// fails fast instead of silently stopping the Lease renewal.
func TestValidateMetalNodeIngress(t *testing.T) {
	for _, port := range []int32{1, 22, 443, 1023, 1024, 9443, 65535, 65536} {
		raw, err := metalnode.Encode(metalnode.Report{IngressPort: port, IngressSPKI: "pin"})
		if err == nil {
			_, err = metalnode.Decode(raw)
		}
		reportable := err == nil
		got := validateMetalNodeIngress("mac-studio", int(port), false)
		if (got == nil) != reportable {
			t.Errorf("validateMetalNodeIngress(port %d) = %v, but the report decodes: %v", port, got, reportable)
		}
		if got != nil && !strings.Contains(got.Error(), "--ingress-port") {
			t.Errorf("validateMetalNodeIngress(port %d) = %v, want it to name --ingress-port", port, got)
		}
	}
	if err := validateMetalNodeIngress("", 443, false); err != nil {
		t.Errorf("without --metal-node: %v, want nil", err)
	}
	if err := validateMetalNodeIngress("mac-studio", 443, true); err != nil {
		t.Errorf("legacy mode reports no port: %v, want nil", err)
	}
}

func TestValidateMetalNodeFlags(t *testing.T) {
	for _, tc := range []struct {
		node, lease string
		wantErr     string
	}{
		{node: "", lease: ""},
		{node: "mac-studio", lease: ""},
		{node: "mac-studio", lease: "mac-studio-heartbeat"},
		{node: "", lease: "mac-studio", wantErr: "--metal-node-lease requires --metal-node"},
		{node: "Mac_Studio", lease: "", wantErr: "--metal-node"},
		{node: "mac-studio", lease: "ns/lease", wantErr: "--metal-node-lease"},
	} {
		err := validateMetalNodeFlags(tc.node, tc.lease)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("validateMetalNodeFlags(%q, %q) = %v, want nil", tc.node, tc.lease, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("validateMetalNodeFlags(%q, %q) = %v, want an error mentioning %q", tc.node, tc.lease, err, tc.wantErr)
		}
	}
}

// TestParseEngineEnvPassthrough: --engine-env-passthrough takes exact
// variable names; a NAME=value entry, a wildcard or another malformed name
// fails startup instead of silently passing nothing through.
func TestParseEngineEnvPassthrough(t *testing.T) {
	for _, tc := range []struct {
		flag    string
		want    []string
		wantErr string
	}{
		{flag: "", want: nil},
		{flag: " , ", want: nil},
		{flag: "MLX_MAX_OPS_PER_BUFFER", want: []string{"MLX_MAX_OPS_PER_BUFFER"}},
		{flag: "HTTPS_PROXY, no_proxy ,GGML_METAL_N_CB", want: []string{"HTTPS_PROXY", "no_proxy", "GGML_METAL_N_CB"}},
		{flag: "HF_TOKEN=hf_x", wantErr: `"HF_TOKEN=hf_x"`},
		{flag: "GGML_*", wantErr: `"GGML_*"`},
		{flag: "OK,9LIVES", wantErr: `"9LIVES"`},
		{flag: "BAD-NAME", wantErr: "--engine-env-passthrough"},
	} {
		got, err := parseEngineEnvPassthrough(tc.flag)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("parseEngineEnvPassthrough(%q) = %q, %v; want an error mentioning %s", tc.flag, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("parseEngineEnvPassthrough(%q) = %q, %v; want %q", tc.flag, got, err, tc.want)
		}
	}
}

// TestTokenFileToRenew: --renew-service-account-token renews the
// kubeconfig's tokenFile, and refuses a kubeconfig without one, whose token
// a renewal could never reach.
func TestTokenFileToRenew(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		cfg     restclient.Config
		want    string
		wantErr bool
	}{
		{name: "off", cfg: restclient.Config{BearerToken: "inline"}},
		{name: "tokenFile", enabled: true, cfg: restclient.Config{BearerTokenFile: "/agent/token"}, want: "/agent/token"},
		{name: "inline token", enabled: true, cfg: restclient.Config{BearerToken: "inline"}, wantErr: true},
		{name: "client certificate", enabled: true, wantErr: true,
			cfg: restclient.Config{TLSClientConfig: restclient.TLSClientConfig{CertFile: "c"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tokenFileToRenew(tc.enabled, &tc.cfg)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("tokenFileToRenew = %q, %v; want %q, error %t", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestStopEnginesSubcommandParsesItsFlags(t *testing.T) {
	opts, err := parseStopEnginesArgs([]string{"--state-dir", "/x", "--namespace", "ns", "--ollama-port", "1234"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.StateDir != "/x" || opts.Namespace != "ns" || opts.OllamaPort != 1234 {
		t.Fatalf("unexpected options %+v", opts)
	}
	if _, err := parseStopEnginesArgs([]string{"--no-such-flag"}); err == nil {
		t.Fatal("an unknown flag must error")
	}
	if _, err := parseStopEnginesArgs([]string{"stray"}); err == nil {
		t.Fatal("a stray argument must error")
	}
}

// TestStopEnginesParsesWhatLeaveSends: llmkube metal leave builds its
// stop-engines command line with metaljoin.StopEnginesArgs from the agent's
// LaunchAgent arguments, and dispatches on its first word. This parser must
// read that line back to the same kubeconfig, namespace and state directory.
func TestStopEnginesParsesWhatLeaveSends(t *testing.T) {
	kc := flag.Lookup("kubeconfig")
	if kc == nil {
		t.Fatal("no --kubeconfig flag registered; stop-engines could not take the one leave sends")
	}
	prev := kc.Value.String()
	t.Cleanup(func() { _ = kc.Value.Set(prev) })

	agentArgs := []string{"--kubeconfig", "/Users/a/kc", "--namespace", "team", "--metal-node", "mac-a",
		"--renew-service-account-token", "--state-dir", "/Users/a/state", "--llama-server", "/opt/llama-server"}
	args := metaljoin.StopEnginesArgs(agentArgs, "/unused")
	if args[0] != metaljoin.StopEnginesCommand {
		t.Fatalf("leave sends %q first, want the subcommand main dispatches on", args[0])
	}
	opts, err := parseStopEnginesArgs(args[1:])
	if err != nil {
		t.Fatalf("parseStopEnginesArgs(%q): %v", args[1:], err)
	}
	if opts.Namespace != "team" || opts.StateDir != "/Users/a/state" || kc.Value.String() != "/Users/a/kc" {
		t.Fatalf("parsed namespace %q, state dir %q, kubeconfig %q from %q; want team, /Users/a/state, /Users/a/kc",
			opts.Namespace, opts.StateDir, kc.Value.String(), args[1:])
	}
}
