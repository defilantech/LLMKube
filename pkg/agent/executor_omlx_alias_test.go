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
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// fakeOMLXEnv switches the test binary into the fake oMLX daemon below when
// the executor under test spawns it.
const fakeOMLXEnv = "LLMKUBE_FAKE_OMLX_DAEMON"

// TestFakeOMLXDaemonProcess is not a test. It is the body of a fake oMLX
// daemon: the executor spawns this test binary (through the script
// fakeOMLXHelperBinary writes) as if it were `omlx serve`. It is a separate
// process so it can model the part of oMLX this change depends on: the
// daemon reads {--base-path}/model_settings.json once at startup
// (omlx/server.py builds ModelSettingsManager when the app starts), resolves
// a request's model by exact directory name first and then by model_alias
// (omlx/engine_pool.py resolve_model_id), answers 404 otherwise, and lists
// the alias in place of the directory name on /v1/models. Source: oMLX
// v0.3.7 as installed by Homebrew, unchanged through v0.7.0.
func TestFakeOMLXDaemonProcess(t *testing.T) {
	if os.Getenv(fakeOMLXEnv) != "1" {
		t.Skip("helper process for the oMLX alias tests")
	}
	args := flag.Args()
	port := flagValue(args, "--port")
	modelDir := flagValue(args, "--model-dir")
	basePath := flagValue(args, "--base-path")

	aliases := map[string]string{} // alias -> directory name
	if data, err := os.ReadFile(filepath.Join(basePath, "model_settings.json")); err == nil {
		var doc struct {
			Models map[string]map[string]any `json:"models"`
		}
		if json.Unmarshal(data, &doc) == nil {
			for dir, s := range doc.Models {
				if a, ok := s["model_alias"].(string); ok && a != "" {
					aliases[a] = dir
				}
			}
		}
	}
	dirs := map[string]bool{}
	entries, _ := os.ReadDir(modelDir)
	for _, ent := range entries {
		if _, err := os.Stat(filepath.Join(modelDir, ent.Name(), "config.json")); err == nil {
			dirs[ent.Name()] = true
		}
	}
	resolve := func(name string) (string, bool) {
		if dirs[name] {
			return name, true
		}
		dir, ok := aliases[name]
		return dir, ok
	}

	var mu sync.Mutex
	loaded := map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		data := make([]map[string]string, 0, len(dirs))
		for dir := range dirs {
			id := dir
			for a, d := range aliases {
				if d == dir {
					id = a
				}
			}
			data = append(data, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("GET /v1/models/status", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		models := make([]map[string]any, 0, len(dirs))
		for dir := range dirs {
			models = append(models, map[string]any{"id": dir, "loaded": loaded[dir]})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		dir, ok := resolve(req.Model)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"detail": "Model '" + req.Model + "' not found"})
			return
		}
		mu.Lock()
		loaded[dir] = true
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "object": "chat.completion"})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(2)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	_ = srv.Serve(ln)
	os.Exit(0)
}

// fakeOMLXHelperBinary writes a script that re-executes this test binary as
// the fake oMLX daemon, forwarding the serve arguments the executor builds.
func fakeOMLXHelperBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeOMLXEnv, "1")
	return fakeOMLXDaemon(t, "exec '"+self+"' -test.run='^TestFakeOMLXDaemonProcess$' -- \"$@\"\n")
}

// omlxAliasFixture builds an executor over a model store holding one oMLX
// model directory, with its base path in a temp dir (never the real ~/.omlx)
// and the fake daemon as its binary. The daemon is killed at cleanup.
func omlxAliasFixture(t *testing.T, dirName string) (*OMLXExecutor, string) {
	t.Helper()
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, dirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, dirName, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	port, err := allocateLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	e := NewOMLXExecutor(fakeOMLXHelperBinary(t), store, port, newNopLogger())
	e.basePath = t.TempDir()
	e.SetStartupTimeout(30 * time.Second)
	t.Cleanup(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.process != nil {
			_ = e.process.Kill()
			_, _ = e.process.Wait()
		}
	})
	return e, filepath.Join(store, dirName)
}

// chatThroughClientProxy sends a chat completion for model through the
// agent's client proxy to the oMLX daemon on port and returns the status.
func chatThroughClientProxy(t *testing.T, port int, model string) int {
	t.Helper()
	p := NewClientProxy(&fakeBackend{addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		runtime: inferencev1alpha1.RuntimeOMLX, ok: true}, testClientProxyPort, newNopLogger())
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:9443"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec.Code
}

// TestOMLXServedNameAlias_EndToEnd is #1972: a client calling the agent with
// the served model name (what llama-server answers to via --alias, and what
// the docs tell clients to send) must reach the oMLX model, not get a 404.
// The directory name must keep working, since the ModelRouter (#1987) and
// the executor's own warmup and status checks use it.
func TestOMLXServedNameAlias_EndToEnd(t *testing.T) {
	e, modelPath := omlxAliasFixture(t, "Qwen3-8B-MLX-8bit")
	cfg := ExecutorConfig{
		Name:            "chat",
		Namespace:       "ns",
		ModelSource:     modelPath,
		ModelName:       "qwen3-8b",
		ServedModelName: "qwen3-8b",
	}
	if _, err := e.StartProcess(context.Background(), cfg); err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	for _, tc := range []struct {
		model string
		want  int
	}{
		{"qwen3-8b", http.StatusOK},
		{"Qwen3-8B-MLX-8bit", http.StatusOK},
		{"chat", http.StatusNotFound},
	} {
		if got := chatThroughClientProxy(t, e.port, tc.model); got != tc.want {
			t.Errorf("chat with model %q through the client proxy = %d, want %d", tc.model, got, tc.want)
		}
	}

	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(e.port) + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer drainAndClose(resp)
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "qwen3-8b" {
		t.Errorf("/v1/models = %+v, want the served name qwen3-8b", list.Data)
	}
}

// TestOMLXServedNameAlias_ChangeRestartsOwnedDaemon covers a modelRef change
// on a running service: the respawn calls StartProcess again with the new
// served name while the shared daemon is up. oMLX reads aliases only at
// startup, so the executor must restart the daemon it owns; otherwise the new
// name 404s until something else restarts oMLX.
func TestOMLXServedNameAlias_ChangeRestartsOwnedDaemon(t *testing.T) {
	e, modelPath := omlxAliasFixture(t, "Qwen3-8B-MLX-8bit")
	cfg := ExecutorConfig{Name: "chat", Namespace: "ns", ModelSource: modelPath,
		ModelName: "qwen3-8b", ServedModelName: "qwen3-8b"}
	first, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first StartProcess: %v", err)
	}

	cfg.ServedModelName = "qwen3-8b-v2"
	second, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second StartProcess: %v", err)
	}
	if second.PID == first.PID {
		t.Errorf("daemon PID unchanged (%d); the alias change did not restart oMLX", first.PID)
	}
	if got := chatThroughClientProxy(t, e.port, "qwen3-8b-v2"); got != http.StatusOK {
		t.Errorf("chat with the new served name = %d, want 200", got)
	}
	if got := chatThroughClientProxy(t, e.port, "qwen3-8b"); got != http.StatusNotFound {
		t.Errorf("chat with the old served name = %d, want 404", got)
	}

	// An unchanged alias must not restart the daemon again.
	third, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("third StartProcess: %v", err)
	}
	if third.PID != second.PID {
		t.Errorf("daemon restarted (%d -> %d) although the alias did not change", second.PID, third.PID)
	}
}

// TestSetOMLXModelAlias_PreservesOperatorSettings feeds a settings file in
// the shape oMLX itself writes (ModelSettingsManager._save: version plus a
// models map of non-null ModelSettings fields) and checks the agent only
// touches model_alias.
func TestSetOMLXModelAlias_PreservesOperatorSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_settings.json")
	orig := `{
  "version": 1,
  "models": {
    "Qwen3-8B-MLX-8bit": {"temperature": 0.6, "is_pinned": true, "chat_template_kwargs": {"enable_thinking": false}},
    "Other-4bit": {"model_alias": "qwen3-8b", "max_tokens": 512}
  },
  "future_key": {"kept": true}
}`
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := setOMLXModelAlias(path, "Qwen3-8B-MLX-8bit", "qwen3-8b")
	if err != nil || !changed {
		t.Fatalf("setOMLXModelAlias = %v, %v; want changed", changed, err)
	}

	var got map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("rewritten file is not JSON: %v\n%s", err, data)
	}
	models := got["models"].(map[string]any)
	target := models["Qwen3-8B-MLX-8bit"].(map[string]any)
	if target["model_alias"] != "qwen3-8b" || target["temperature"] != 0.6 || target["is_pinned"] != true ||
		target["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
		t.Errorf("target entry = %v, want alias added and other settings kept", target)
	}
	other := models["Other-4bit"].(map[string]any)
	if _, ok := other["model_alias"]; ok || other["max_tokens"] != float64(512) {
		t.Errorf("other entry = %v, want the duplicate alias removed and max_tokens kept", other)
	}
	if got["version"] != float64(1) || got["future_key"].(map[string]any)["kept"] != true {
		t.Errorf("top-level keys not preserved: %v", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("file mode = %v, want the original 0644", fi.Mode().Perm())
	}

	before, _ := os.ReadFile(path)
	changed, err = setOMLXModelAlias(path, "Qwen3-8B-MLX-8bit", "qwen3-8b")
	after, _ := os.ReadFile(path)
	if err != nil || changed || string(before) != string(after) {
		t.Errorf("second identical call: changed=%v err=%v, file rewritten=%v; want a no-op",
			changed, err, string(before) != string(after))
	}
}

// TestRegisterAlias_ServedNameEqualsDirectoryClearsStaleAlias: when the
// served name is the directory name itself, no alias is needed, and a stale
// one from an earlier modelRef must go so /v1/models lists the name clients
// should send.
func TestRegisterAlias_ServedNameEqualsDirectoryClearsStaleAlias(t *testing.T) {
	e := NewOMLXExecutor("omlx", t.TempDir(), 0, newNopLogger())
	e.basePath = t.TempDir()
	path := filepath.Join(e.basePath, omlxModelSettingsFile)

	if changed, err := e.registerAliasLocked("m-4bit", "old-name"); err != nil || !changed {
		t.Fatalf("register old-name: changed=%v err=%v", changed, err)
	}
	if changed, err := e.registerAliasLocked("m-4bit", "m-4bit"); err != nil || !changed {
		t.Fatalf("register directory name: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "old-name") {
		t.Errorf("stale alias left in settings: %s", data)
	}
}

// TestRegisterAlias_InvalidSettingsFileFailsWithoutClobbering: a settings
// file oMLX's operator broke must surface as an error, not be overwritten
// with only the agent's alias.
func TestRegisterAlias_InvalidSettingsFileFailsWithoutClobbering(t *testing.T) {
	e := NewOMLXExecutor("omlx", t.TempDir(), 0, newNopLogger())
	e.basePath = t.TempDir()
	path := filepath.Join(e.basePath, omlxModelSettingsFile)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.registerAliasLocked("m-4bit", "m"); err == nil {
		t.Error("registerAliasLocked accepted an unparseable settings file")
	}
	if data, _ := os.ReadFile(path); string(data) != "{not json" {
		t.Errorf("settings file was rewritten: %q", data)
	}
}

// TestBuildOMLXServeArgs_BasePath pins that the daemon is told which base
// path to read model_settings.json from, so the agent's alias file and the
// daemon's agree.
func TestBuildOMLXServeArgs_BasePath(t *testing.T) {
	args := buildOMLXServeArgs("/models", 8000, omlxServeConfig{basePath: "/Users/x/.omlx"})
	if got := flagValue(args, "--base-path"); got != "/Users/x/.omlx" {
		t.Errorf("--base-path = %q, want /Users/x/.omlx (args %v)", got, args)
	}
	if hasFlag(buildOMLXServeArgs("/models", 8000, omlxServeConfig{}), "--base-path") {
		t.Error("--base-path emitted with an empty base path")
	}
}
