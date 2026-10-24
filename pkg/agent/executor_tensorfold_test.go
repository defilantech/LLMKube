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
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestBuildTensorFoldArgs(t *testing.T) {
	const dir = "/models/qwen"
	base := []string{"serve", dir, "--host", "127.0.0.1", "--port", "9123"}

	tests := []struct {
		name string
		cfg  ExecutorConfig
		want []string
	}{
		{
			name: "served name, no context",
			cfg:  ExecutorConfig{Name: "isvc", ServedModelName: "qwen38"},
			want: append(append([]string{}, base...), "--name", "qwen38", "--no-update-check"),
		},
		{
			name: "falls back to the InferenceService name",
			cfg:  ExecutorConfig{Name: "isvc"},
			want: append(append([]string{}, base...), "--name", "isvc", "--no-update-check"),
		},
		{
			name: "context size emits --context",
			cfg:  ExecutorConfig{Name: "isvc", ServedModelName: "qwen38", ContextSize: 65536},
			want: append(append([]string{}, base...),
				"--name", "qwen38", "--no-update-check", "--context", "65536"),
		},
		{
			name: "extraArgs come last",
			cfg: ExecutorConfig{Name: "isvc", ServedModelName: "qwen38", ContextSize: 4096,
				ExtraArgs: []string{"--no-thinking", "--drafter", "none"}},
			want: append(append([]string{}, base...),
				"--name", "qwen38", "--no-update-check", "--context", "4096",
				"--no-thinking", "--drafter", "none"),
		},
		{
			name: "user --name wins",
			cfg: ExecutorConfig{Name: "isvc", ServedModelName: "qwen38",
				ExtraArgs: []string{"--name", "custom"}},
			want: append(append([]string{}, base...), "--no-update-check", "--name", "custom"),
		},
		{
			name: "user --context= wins",
			cfg: ExecutorConfig{Name: "isvc", ServedModelName: "qwen38", ContextSize: 4096,
				ExtraArgs: []string{"--context=8192"}},
			want: append(append([]string{}, base...),
				"--name", "qwen38", "--no-update-check", "--context=8192"),
		},
		{
			name: "user --no-update-check is not repeated",
			cfg: ExecutorConfig{Name: "isvc", ServedModelName: "qwen38",
				ExtraArgs: []string{"--no-update-check"}},
			want: append(append([]string{}, base...), "--name", "qwen38", "--no-update-check"),
		},
		{
			name: "user --host wins",
			cfg: ExecutorConfig{Name: "isvc", ServedModelName: "qwen38",
				ExtraArgs: []string{"--host", "127.0.0.1"}},
			want: []string{"serve", dir, "--port", "9123",
				"--name", "qwen38", "--no-update-check", "--host", "127.0.0.1"},
		},
		{
			name: "user --port wins",
			cfg: ExecutorConfig{Name: "isvc", ServedModelName: "qwen38",
				ExtraArgs: []string{"--port=9123"}},
			want: []string{"serve", dir, "--host", "127.0.0.1",
				"--name", "qwen38", "--no-update-check", "--port=9123"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildTensorFoldArgs(dir, 9123, tc.cfg)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("buildTensorFoldArgs()\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestTensorFoldPort covers the port a TensorFold child binds: an ephemeral
// one unless the user pinned --port in extraArgs, in which case the health
// wait and the registered endpoint must follow the user's value or they would
// probe a port nothing listens on.
func TestTensorFoldPort(t *testing.T) {
	e := NewTensorFoldExecutor("tensorfold", t.TempDir(), newNopLogger())
	e.allocatePort = func() (int, error) { return 40001, nil }

	tests := []struct {
		name    string
		extra   []string
		want    int
		wantErr bool
	}{
		{name: "ephemeral", want: 40001},
		{name: "separate value", extra: []string{"--port", "8123"}, want: 8123},
		{name: "inline value", extra: []string{"--port=8124"}, want: 8124},
		{name: "unparsable", extra: []string{"--port=abc"}, wantErr: true},
		{name: "missing value", extra: []string{"--port"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.port(tc.extra)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("port(%q) = %d, want error", tc.extra, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("port(%q): %v", tc.extra, err)
			}
			if got != tc.want {
				t.Errorf("port(%q) = %d, want %d", tc.extra, got, tc.want)
			}
		})
	}
}

func TestTensorFoldEnv(t *testing.T) {
	got := tensorFoldEnv([]string{"HOME=/Users/x", "TENSORFOLD_NO_UPDATE_CHECK=0", "HF_HOME=/hf"})
	var n int
	for _, kv := range got {
		if strings.HasPrefix(kv, "TENSORFOLD_NO_UPDATE_CHECK=") {
			n++
			if kv != "TENSORFOLD_NO_UPDATE_CHECK=1" {
				t.Errorf("env carries %q, want TENSORFOLD_NO_UPDATE_CHECK=1", kv)
			}
		}
	}
	if n != 1 {
		t.Errorf("TENSORFOLD_NO_UPDATE_CHECK appears %d times, want 1: %q", n, got)
	}
	for _, want := range []string{"HOME=/Users/x", "HF_HOME=/hf"} {
		if !slices.Contains(got, want) {
			t.Errorf("parent env entry %q dropped: %q", want, got)
		}
	}
}

func TestTensorFoldResolveModelPath(t *testing.T) {
	e := NewTensorFoldExecutor("tensorfold", "/store", newNopLogger())
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "absolute is used as-is", source: "/Users/me/models/qwen", want: "/Users/me/models/qwen"},
		{name: "relative joins the store", source: "org/qwen-mlx", want: "/store/org/qwen-mlx"},
		{name: "empty uses the Model name", source: "", want: "/store/my-model"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := e.resolveModelPath(ExecutorConfig{ModelSource: tc.source, ModelName: "my-model"})
			if got != tc.want {
				t.Errorf("resolveModelPath(%q) = %q, want %q", tc.source, got, tc.want)
			}
		})
	}
}

// fakeTensorFold writes an executable shell script standing in for tensorfold.
func fakeTensorFold(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tensorfold")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake tensorfold: %v", err)
	}
	return path
}

// tensorFoldFixture returns an executor over a model store holding a model
// directory, the config that resolves to it, and the per-process log path.
func tensorFoldFixture(t *testing.T, bin string) (*TensorFoldExecutor, ExecutorConfig, string) {
	t.Helper()
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, "qwen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "qwen", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewTensorFoldExecutor(bin, store, newNopLogger())
	cfg := ExecutorConfig{
		Name:            "isvc",
		Namespace:       "ns",
		ModelSource:     "qwen",
		ModelName:       "qwen-model",
		ServedModelName: "qwen38",
		ContextSize:     65536,
		ExtraArgs:       []string{"--no-thinking"},
	}
	return e, cfg, filepath.Join(store, "tensorfold-ns-isvc.log")
}

// TestTensorFoldStartProcess_MissingModelDir: the agent does not download MLX
// directories, so a missing one must fail with a message naming the path, and
// no child may be spawned.
func TestTensorFoldStartProcess_MissingModelDir(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "spawned")
	bin := fakeTensorFold(t, "touch '"+marker+"'\nexec sleep 60\n")
	e, cfg, _ := tensorFoldFixture(t, bin)
	cfg.ModelSource = "does-not-exist"

	proc, err := e.StartProcess(context.Background(), cfg)
	if err == nil {
		t.Fatalf("StartProcess succeeded without a model directory: %+v", proc)
	}
	want := filepath.Join(e.modelStorePath, "does-not-exist")
	if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "not found") {
		t.Errorf("error does not name the missing directory %s: %v", want, err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("tensorfold was spawned although the model directory is missing")
	}
}

// TestTensorFoldStartProcess_ChildExitFailsFast: TensorFold refuses an
// unsupported checkpoint by printing why and exiting. The startup wait must
// return at once with the exit status and that message, not sit out a
// 10-minute startup timeout.
func TestTensorFoldStartProcess_ChildExitFailsFast(t *testing.T) {
	bin := fakeTensorFold(t, "echo 'error: unsupported model family: llama4' >&2\nexit 1\n")
	e, cfg, logPath := tensorFoldFixture(t, bin)
	e.SetStartupTimeout(60 * time.Second)

	start := time.Now()
	proc, err := e.StartProcess(context.Background(), cfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("StartProcess succeeded with a child that exited; process: %+v", proc)
	}
	if elapsed > 5*time.Second {
		t.Errorf("StartProcess took %s against a 60s timeout; it waited on a dead child", elapsed)
	}
	for _, want := range []string{"unsupported model family: llama4", "exit status 1", logPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	info, statErr := os.Stat(logPath)
	if statErr != nil {
		t.Fatalf("per-process log not written: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file mode = %o, want 600", perm)
	}
	if e.trackedChild(0) != nil || len(e.children) != 0 {
		t.Errorf("exited child left tracked: %v", e.children)
	}
}

// TestTensorFoldStartProcess_LogTruncatedOnStart: the per-process log holds
// only the current run, so a stale failure from a previous spawn cannot be
// mistaken for this one.
func TestTensorFoldStartProcess_LogTruncatedOnStart(t *testing.T) {
	bin := fakeTensorFold(t, "echo 'fresh run' >&2\nexit 2\n")
	e, cfg, logPath := tensorFoldFixture(t, bin)
	if err := os.WriteFile(logPath, []byte("stale failure from last run\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := e.StartProcess(context.Background(), cfg); err == nil {
		t.Fatal("StartProcess succeeded with a child that exited")
	}
	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "stale failure") || !strings.Contains(string(data), "fresh run") {
		t.Errorf("log not truncated on start: %q", data)
	}
}

// TestTensorFoldStartProcess_HealthyChildThenStop spawns a fake child that
// records the argv and environment it was given, checks both reach the
// process, and then stops it through the reaper.
func TestTensorFoldStartProcess_HealthyChildThenStop(t *testing.T) {
	out := t.TempDir()
	argsFile := filepath.Join(out, "args")
	envFile := filepath.Join(out, "env")
	bin := fakeTensorFold(t,
		"for a in \"$@\"; do echo \"$a\"; done > '"+argsFile+"'\n"+
			"echo \"$TENSORFOLD_NO_UPDATE_CHECK\" > '"+envFile+"'\n"+
			"exec sleep 60\n")
	e, cfg, logPath := tensorFoldFixture(t, bin)
	port := healthyPort(t)
	e.allocatePort = func() (int, error) { return port, nil }
	e.SetStartupTimeout(10 * time.Second)

	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess with a healthy child: %v", err)
	}
	if !proc.Healthy || proc.Port != port {
		t.Errorf("process = %+v, want healthy on port %d", proc, port)
	}
	wantPath := filepath.Join(e.modelStorePath, "qwen")
	if proc.ModelPath != wantPath {
		t.Errorf("ModelPath = %q, want %q", proc.ModelPath, wantPath)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("per-process log not created: %v", err)
	}

	gotArgs := readLines(t, argsFile)
	wantArgs := buildTensorFoldArgs(wantPath, port, cfg)
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("child argv\n got %q\nwant %q", gotArgs, wantArgs)
	}
	if env := readLines(t, envFile); len(env) != 1 || env[0] != "1" {
		t.Errorf("child TENSORFOLD_NO_UPDATE_CHECK = %q, want \"1\"", env)
	}

	exit := e.trackedChild(proc.PID)
	if exit == nil {
		t.Fatal("started child is not tracked")
	}
	start := time.Now()
	if err := e.StopProcess(proc.PID); err != nil {
		t.Fatalf("StopProcess on a live tracked child: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("StopProcess took %s; SIGTERM should end sleep at once", elapsed)
	}
	select {
	case <-exit.done:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper did not observe the child's exit")
	}
	if got := exit.status(); got != "signal: terminated" {
		t.Errorf("reaped status = %q, want %q (a second waiter raced the reaper)", got, "signal: terminated")
	}
	if e.trackedChild(proc.PID) != nil {
		t.Error("StopProcess left the child tracked")
	}
}

// TestTensorFoldStopProcess_ChildAlreadyExited: a child that died after it
// became healthy has been reaped, so its PID may be reused. StopProcess must
// succeed without signalling.
func TestTensorFoldStopProcess_ChildAlreadyExited(t *testing.T) {
	bin := fakeTensorFold(t, "exec sleep 60\n")
	e, cfg, _ := tensorFoldFixture(t, bin)
	port := healthyPort(t)
	e.allocatePort = func() (int, error) { return port, nil }
	e.SetStartupTimeout(10 * time.Second)

	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess with a healthy child: %v", err)
	}
	exit := e.trackedChild(proc.PID)
	if exit == nil {
		t.Fatal("started child is not tracked")
	}
	if err := syscall.Kill(proc.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exit.done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not observe the child's exit")
	}
	if err := e.StopProcess(proc.PID); err != nil {
		t.Errorf("StopProcess on an already-exited child: %v", err)
	}
}

func TestTensorFoldSetStartupTimeout(t *testing.T) {
	e := NewTensorFoldExecutor("tensorfold", "/models", newNopLogger())
	if e.startupTimeout != DefaultTensorFoldStartupTimeout {
		t.Errorf("default startupTimeout = %v, want %v", e.startupTimeout, DefaultTensorFoldStartupTimeout)
	}
	e.SetStartupTimeout(3 * time.Minute)
	if e.startupTimeout != 3*time.Minute {
		t.Errorf("after Set(3m) = %v", e.startupTimeout)
	}
	e.SetStartupTimeout(0)
	if e.startupTimeout != DefaultTensorFoldStartupTimeout {
		t.Errorf("after Set(0) = %v, want default", e.startupTimeout)
	}
}

func TestValidateRuntimeFormat_TensorFold(t *testing.T) {
	agent := NewMetalAgent(MetalAgentConfig{
		K8sClient: fake.NewClientBuilder().WithScheme(newTestScheme()).Build(),
	})
	tests := []struct {
		format  string
		wantErr bool
	}{
		{format: "mlx"},
		{format: "gguf", wantErr: true},
		{format: "", wantErr: true}, // empty is treated as gguf
	}
	for _, tc := range tests {
		t.Run("format="+tc.format, func(t *testing.T) {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
				Spec:       inferencev1alpha1.ModelSpec{Format: tc.format},
			}
			err := agent.validateRuntimeFormat(model, "tensorfold")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateRuntimeFormat(%q, tensorfold) passed, want error", tc.format)
				}
				if !strings.Contains(err.Error(), "tensorfold runtime") {
					t.Errorf("error does not name the tensorfold runtime: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("validateRuntimeFormat(%q, tensorfold): %v", tc.format, err)
			}
		})
	}
}

// TestBuildExecutors_TensorFold registers the executor under the literal CRD
// value "tensorfold" when the binary is configured or --runtime selects it,
// and leaves it out otherwise so a CR asking for it gets "no executor".
func TestBuildExecutors_TensorFold(t *testing.T) {
	tests := []struct {
		name    string
		bin     string
		runtime string
		want    bool
		wantBin string
	}{
		{name: "bin set", bin: "/Users/me/.local/bin/tensorfold", want: true, wantBin: "/Users/me/.local/bin/tensorfold"},
		{name: "runtime flag, bin from PATH", runtime: "tensorfold", want: true, wantBin: "tensorfold"},
		{name: "neither", runtime: "llama-server", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := NewMetalAgent(MetalAgentConfig{
				K8sClient:                fake.NewClientBuilder().WithScheme(newTestScheme()).Build(),
				ModelStorePath:           "/tmp/test-models",
				LlamaServerBin:           "/usr/local/bin/llama-server",
				Runtime:                  tc.runtime,
				TensorFoldBin:            tc.bin,
				TensorFoldStartupTimeout: 7 * time.Minute,
			})
			agent.buildExecutors()
			exec, ok := agent.executors["tensorfold"]
			if ok != tc.want {
				t.Fatalf("executors[tensorfold] registered = %v, want %v", ok, tc.want)
			}
			if !tc.want {
				return
			}
			tf, isTF := exec.(*TensorFoldExecutor)
			if !isTF {
				t.Fatalf("executors[tensorfold] is %T, want *TensorFoldExecutor", exec)
			}
			if tf.bin != tc.wantBin {
				t.Errorf("bin = %q, want %q", tf.bin, tc.wantBin)
			}
			if tf.startupTimeout != 7*time.Minute {
				t.Errorf("startupTimeout = %v, want the configured 7m", tf.startupTimeout)
			}
		})
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func TestTensorFoldStartProcess_OnSpawnedErrorKillsChild(t *testing.T) {
	bin := fakeTensorFold(t, "exec sleep 60\n")
	e, cfg, _ := tensorFoldFixture(t, bin)
	port := healthyPort(t)
	e.allocatePort = func() (int, error) { return port, nil }
	e.SetStartupTimeout(10 * time.Second)
	requireOnSpawnedErrorKillsChild(t, cfg, func(c ExecutorConfig) (*ManagedProcess, error) {
		return e.StartProcess(context.Background(), c)
	})
}

func TestTensorFoldStartProcess_EngineHasItsOwnProcessGroup(t *testing.T) {
	bin := fakeTensorFold(t, "exec sleep 60\n")
	e, cfg, _ := tensorFoldFixture(t, bin)
	port := healthyPort(t)
	e.allocatePort = func() (int, error) { return port, nil }
	e.SetStartupTimeout(10 * time.Second)
	var sp SpawnedProcess
	cfg.OnSpawned = func(s SpawnedProcess) error { sp = s; return nil }
	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	t.Cleanup(func() { _ = e.StopProcess(proc.PID) })
	requireOwnProcessGroup(t, proc.PID, sp)
}
