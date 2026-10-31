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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These tests start each runtime's fake engine through its real spawn site,
// with values planted in the agent's own environment, and read back the
// environment the child actually received. They pin what every engine sees:
// the allowlist, the runtime's explicit variables, and configured
// passthrough names, and nothing from the agent's credentials or overrides.

// engineEnvForwardedVar is the name the tests configure as passthrough;
// the _SUFFIX variant checks it is matched as an exact name, not a prefix.
const engineEnvForwardedVar = "LLMKUBE_TEST_ENGINE_FORWARDED"

// plantedAgentEnv is set in the agent's environment for every spawn test.
// No proxy variable is planted here: net/http caches the proxy environment
// once per process, so a planted proxy could leak into later tests. The
// proxy case is covered by the buildEngineEnv unit tests. DYLD_* is planted
// for completeness, but on macOS SIP strips it before the fake /bin/sh
// starts, so only the unit tests prove it is filtered.
var plantedAgentEnv = map[string]string{
	"HF_TOKEN":               "planted",
	"HUGGING_FACE_HUB_TOKEN": "planted",
	"AWS_ACCESS_KEY_ID":      "planted",
	"AWS_SECRET_ACCESS_KEY":  "planted",
	"KUBECONFIG":             "/planted/kubeconfig",
	"DYLD_INSERT_LIBRARIES":  "/x",
	"PYTHONPATH":             "/planted",
	"LLAMA_ARG_HOST":         "0.0.0.0",
	"LLAMA_API_KEY":          "planted",
}

// shellAddedEnv are variables /bin/sh sets in its own environment, so they
// show up in a fake engine's `env` without the executor passing them.
var shellAddedEnv = []string{"PWD", "OLDPWD", "SHLVL", "_"}

// plantAgentEnv sets the planted values, a locale variable, HF_HOME, and the
// passthrough variable (plus a longer name sharing its prefix) in the agent's
// environment. explicit names the runtime's own variables; each is first set
// to a value the executor must replace.
func plantAgentEnv(t *testing.T, explicit ...string) {
	t.Helper()
	for k, v := range plantedAgentEnv {
		t.Setenv(k, v)
	}
	t.Setenv("LC_CTYPE", "UTF-8")
	t.Setenv("HF_HOME", "/hf-home")
	t.Setenv(engineEnvForwardedVar, "passed")
	t.Setenv(engineEnvForwardedVar+"_SUFFIX", "prefix-leak")
	for _, kv := range explicit {
		name, _, _ := strings.Cut(kv, "=")
		t.Setenv(name, "inherited-must-be-replaced")
	}
}

// assertEngineEnv checks the environment a fake engine recorded with `env`.
func assertEngineEnv(t *testing.T, got []string, explicit ...string) {
	t.Helper()
	names := envNames(got)
	for k := range plantedAgentEnv {
		if slices.Contains(names, k) {
			t.Errorf("engine inherited %s from the agent", k)
		}
	}
	if slices.Contains(names, engineEnvForwardedVar+"_SUFFIX") {
		t.Errorf("passthrough %s matched %s_SUFFIX as a prefix", engineEnvForwardedVar, engineEnvForwardedVar)
	}
	for _, want := range []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LC_CTYPE=UTF-8",
		"HF_HOME=/hf-home",
		engineEnvForwardedVar + "=passed",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("engine env lacks %q", want)
		}
	}
	for _, want := range explicit {
		name, _, _ := strings.Cut(want, "=")
		var seen []string
		for _, kv := range got {
			if strings.HasPrefix(kv, name+"=") {
				seen = append(seen, kv)
			}
		}
		if len(seen) != 1 || seen[0] != want {
			t.Errorf("engine %s entries = %q, want exactly [%q]", name, seen, want)
		}
	}
	// Nothing beyond the allowlist, LC_*, the passthrough name, the runtime's
	// explicit variables and what the shell adds itself.
	for _, name := range names {
		if engineEnvInherits(name, []string{engineEnvForwardedVar}) ||
			slices.Contains(shellAddedEnv, name) ||
			slices.ContainsFunc(explicit, func(kv string) bool { return strings.HasPrefix(kv, name+"=") }) {
			continue
		}
		t.Errorf("engine env carries unexpected %s", name)
	}
}

func readEnvFile(t *testing.T, path string) []string {
	t.Helper()
	return strings.Split(strings.TrimRight(string(childFile(t, path)), "\n"), "\n")
}

var llamaServerExplicitEnv = []string{
	"GGML_METAL_ENABLE=1",
	"GGML_METAL_PATH_RESOURCES=/usr/local/share/llama.cpp",
}

// fakeLlamaServerEnvScript records the environment of a --help probe and of a
// serving start in separate files, then exits.
func fakeLlamaServerEnvScript(helpEnv, serveEnv string) string {
	return "if [ \"$1\" = --help ]; then\n" +
		recordCmd("env", helpEnv) +
		"exit 0\nfi\n" +
		recordCmd("env", serveEnv) +
		"exit 3\n"
}

func TestMetalStartProcess_EngineEnv(t *testing.T) {
	plantAgentEnv(t, llamaServerExplicitEnv...)
	out := t.TempDir()
	envFile := filepath.Join(out, "serve-env")
	bin := fakeLlamaServer(t, fakeLlamaServerEnvScript(filepath.Join(out, "help-env"), envFile))
	e, cfg, _ := childExitFixture(t, bin)
	e.SetEngineEnvPassthrough([]string{engineEnvForwardedVar})

	_, _ = e.StartProcess(context.Background(), cfg)
	assertEngineEnv(t, readEnvFile(t, envFile), llamaServerExplicitEnv...)
}

// The --help probe runs the operator's llama-server binary too, so it gets
// the same environment a serving start does.
func TestMetalHelpProbe_EngineEnv(t *testing.T) {
	plantAgentEnv(t, llamaServerExplicitEnv...)
	out := t.TempDir()
	helpEnv := filepath.Join(out, "help-env")
	bin := fakeLlamaServer(t, fakeLlamaServerEnvScript(helpEnv, filepath.Join(out, "serve-env")))
	e := NewMetalExecutor(bin, t.TempDir(), newNopLogger())
	e.SetEngineEnvPassthrough([]string{engineEnvForwardedVar})

	e.supportsLoadMode(context.Background())
	assertEngineEnv(t, readEnvFile(t, helpEnv), llamaServerExplicitEnv...)
}

func TestMLXServerStartProcess_EngineEnv(t *testing.T) {
	plantAgentEnv(t)
	envFile := filepath.Join(t.TempDir(), "env")
	e, cfg, _ := mlxServerFixture(t, fakeMLXServer(t, recordCmd("env", envFile)+"exit 3\n"), 0)
	e.SetEngineEnvPassthrough([]string{engineEnvForwardedVar})

	_, _ = e.StartProcess(context.Background(), cfg)
	assertEngineEnv(t, readEnvFile(t, envFile))
}

func TestVLLMSwiftStartProcess_EngineEnv(t *testing.T) {
	plantAgentEnv(t)
	envFile := filepath.Join(t.TempDir(), "env")
	e, cfg, _ := vllmSwiftFixture(t, fakeVLLMSwift(t, recordCmd("env", envFile)+"exit 3\n"))
	e.SetEngineEnvPassthrough([]string{engineEnvForwardedVar})

	_, _ = e.StartProcess(context.Background(), cfg)
	assertEngineEnv(t, readEnvFile(t, envFile))
}

func TestTensorFoldStartProcess_EngineEnv(t *testing.T) {
	explicit := tensorFoldNoUpdateCheckEnv + "=1"
	plantAgentEnv(t, explicit)
	envFile := filepath.Join(t.TempDir(), "env")
	e, cfg, _ := tensorFoldFixture(t, fakeTensorFold(t, recordCmd("env", envFile)+"exit 3\n"))
	e.SetEngineEnvPassthrough([]string{engineEnvForwardedVar})

	_, _ = e.StartProcess(context.Background(), cfg)
	assertEngineEnv(t, readEnvFile(t, envFile), explicit)
}

func TestOMLXStartProcess_EngineEnv(t *testing.T) {
	plantAgentEnv(t)
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, "m"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "m", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "env")
	port, err := allocateLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	e := NewOMLXExecutor(fakeOMLXDaemon(t, recordCmd("env", envFile)+"exit 1\n"), store, port, newNopLogger())
	e.SetEngineEnvPassthrough([]string{engineEnvForwardedVar})
	e.SetStartupTimeout(2 * time.Second)

	_, _ = e.StartProcess(context.Background(), ExecutorConfig{Name: "isvc", Namespace: "ns", ModelName: "m"})
	assertEngineEnv(t, readEnvFile(t, envFile))
}

// TestBuildExecutors_WiresEngineEnvPassthrough starts every runtime the agent
// builds from its config and checks MetalAgentConfig.EngineEnvPassthrough
// reached each child: the per-runtime tests above set the list on the
// executor directly, so they cannot see a runtime buildExecutors forgot.
func TestBuildExecutors_WiresEngineEnvPassthrough(t *testing.T) {
	plantAgentEnv(t)
	store := t.TempDir()
	for _, f := range []string{"m/model.gguf", "mlx/config.json"} {
		p := filepath.Join(store, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	envFile := func(rt string) string { return filepath.Join(out, rt+"-env") }
	fakeBin := func(rt string) string {
		path := filepath.Join(t.TempDir(), rt)
		body := "#!/bin/sh\n[ \"$1\" = --help ] && exit 0\n" + recordCmd("env", envFile(rt)) + "exit 3\n"
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	omlxPort, err := allocateLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient:            fake.NewClientBuilder().WithScheme(newTestScheme()).Build(),
		Namespace:            "ns",
		ModelStorePath:       store,
		LlamaServerBin:       fakeBin(runtimeLlamaServer),
		OMLXBin:              fakeBin(runtimeOMLX),
		OMLXPort:             omlxPort,
		OMLXStartupTimeout:   2 * time.Second,
		VLLMSwiftBin:         fakeBin(runtimeVLLMSwift),
		MLXServerBin:         fakeBin(runtimeMLXServer),
		TensorFoldBin:        fakeBin(runtimeTensorFold),
		EngineEnvPassthrough: []string{engineEnvForwardedVar},
	})
	a.buildExecutors()

	llamaCfg := ExecutorConfig{
		Name: "isvc", Namespace: "ns", ModelSource: "file:///models/model.gguf", ModelName: "m", ContextSize: 4096,
	}
	mlxCfg := ExecutorConfig{Name: "isvc", Namespace: "ns", ModelSource: "mlx", ModelName: "mlx"}
	for _, rt := range []string{runtimeLlamaServer, runtimeOMLX, runtimeVLLMSwift, runtimeMLXServer, runtimeTensorFold} {
		t.Run(rt, func(t *testing.T) {
			exec, ok := a.executors[rt]
			if !ok {
				t.Fatalf("no executor registered for %s", rt)
			}
			cfg := mlxCfg
			switch rt {
			case runtimeLlamaServer:
				cfg = llamaCfg
			case runtimeOMLX:
				cfg.ModelSource = "" // oMLX takes a source as a path as is; the name joins the store
			}
			_, _ = exec.StartProcess(context.Background(), cfg)
			if got := readEnvFile(t, envFile(rt)); !slices.Contains(got, engineEnvForwardedVar+"=passed") {
				t.Errorf("%s did not receive passthrough %s: %q", rt, engineEnvForwardedVar, got)
			}
		})
	}
}
