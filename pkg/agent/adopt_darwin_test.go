//go:build darwin

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestAdopt_RealEngineAcrossAgents spawns a real engine through the executor
// and its record hooks, then has a second agent on the same state directory,
// with the real process inspector and health checker, adopt it and stop it
// through the adopted path.
func TestAdopt_RealEngineAcrossAgents(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a fake engine binary")
	}
	bin := filepath.Join(t.TempDir(), "llama-server")
	buildFakeEngine(t, bin)
	stateDir := t.TempDir()
	port := healthyPort(t)
	proc := startRecordedEngine(t, bin, stateDir, port)

	// Agent 2, same state directory, real inspector and health checker: adopts.
	a2 := newRealInspectorAgent(stateDir)
	if !a2.adoptEngines(context.Background())["default/real"] {
		t.Fatal("second agent did not adopt the running engine")
	}
	if got := a2.processes["default/real"]; got.PID != proc.PID || got.Port != port {
		t.Fatalf("adopted %+v, want PID %d port %d", got, proc.PID, port)
	}
	// Stop through the adopted path: group signals after the identity check.
	if err := a2.stopManagedProcess(context.Background(), a2.processes["default/real"], false); err != nil {
		t.Fatalf("stop adopted engine: %v", err)
	}
	if err := syscall.Kill(proc.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("engine still alive after the adopted stop: %v", err)
	}
}

// buildFakeEngine compiles a program that only sleeps to path. A compiled
// fake, because a #! script's exec path would be /bin/sh after `exec sleep`.
func buildFakeEngine(t *testing.T, path string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "main.go")
	prog := "package main\n\nimport \"time\"\n\nfunc main() { time.Sleep(time.Hour) }\n"
	if err := os.WriteFile(src, []byte(prog), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", path, src).CombinedOutput(); err != nil {
		t.Fatalf("build fake engine: %v\n%s", err, out)
	}
}

// startRecordedEngine has a first agent start bin as engine default/real
// through the real executor and its record hooks, with the records under
// stateDir and the health check answered on port. Both this agent and the
// ones the test builds later read the kernel, not TestMain's fake inspector.
func startRecordedEngine(t *testing.T, bin, stateDir string, port int) *ManagedProcess {
	t.Helper()
	prevFactory := processInspectorFactory
	processInspectorFactory = newProcessInspector
	t.Cleanup(func() { processInspectorFactory = prevFactory })

	records := newEngineRecordStore(filepath.Join(stateDir, "engines"), "v1", newProcessInspector(), nil)
	e, cfg, _ := childExitFixture(t, bin)
	e.SetPort(port)
	e.SetStartupTimeout(10 * time.Second)
	cfg.Name, cfg.Namespace = "real", "default"
	cfg.OnSpawned = func(sp SpawnedProcess) error {
		return records.WriteProvisional("default", "real", runtimeLlamaServer, sp)
	}
	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	// The executor tracks its child, so this stop is a no-op once the engine
	// is gone and otherwise cleans up after a failed assertion.
	t.Cleanup(func() { _ = e.StopProcess(proc.PID) })
	proc.Name, proc.Namespace, proc.Runtime = "real", "default", runtimeLlamaServer
	if err := records.WriteRunning(proc); err != nil {
		t.Fatal(err)
	}
	return proc
}

// newRealInspectorAgent is a later agent process on stateDir, with the
// inspector and health checker NewMetalAgent builds.
func newRealInspectorAgent(stateDir string) *MetalAgent {
	return NewMetalAgent(MetalAgentConfig{
		K8sClient: fake.NewClientBuilder().WithScheme(newTestScheme()).Build(), Namespace: "default",
		MemoryProvider: &mockMemoryProvider{totalBytes: 128 << 30}, StateDir: stateDir,
	})
}

// TestAdopt_EngineStartedThroughRepointedSymlink starts a real engine through
// a bin/ symlink, as Homebrew installs llama-server, then repoints the link to
// a new keg as `brew upgrade` does. The next agent still adopts the engine:
// the link it was launched through is recorded and still reported. With
// neither recorded path matching, the engine is stopped as a group rather
// than discarded, because its start token still proves it is ours.
func TestAdopt_EngineStartedThroughRepointedSymlink(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a fake engine binary")
	}
	root := t.TempDir()
	keg1 := filepath.Join(root, "Cellar", "1", "llama-server")
	keg2 := filepath.Join(root, "Cellar", "2", "llama-server")
	link := filepath.Join(root, "bin", "llama-server")
	for _, d := range []string{filepath.Dir(keg1), filepath.Dir(keg2), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	buildFakeEngine(t, keg1)
	data, err := os.ReadFile(keg1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keg2, data, 0o700); err != nil { //nolint:gosec // an executable test binary
		t.Fatal(err)
	}
	if err := os.Symlink(keg1, link); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	proc := startRecordedEngine(t, link, stateDir, healthyPort(t))
	recordDir := filepath.Join(stateDir, "engines")
	rec := readRecord(t, recordDir, "real")
	if want, _ := filepath.EvalSymlinks(keg1); rec["executable"] != want || rec["launchPath"] != link {
		t.Fatalf("recorded executable %v, launch path %v; want %q and %q",
			rec["executable"], rec["launchPath"], want, link)
	}

	// brew upgrade: the link now resolves to the new keg.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keg2, link); err != nil {
		t.Fatal(err)
	}
	if !newRealInspectorAgent(stateDir).adoptEngines(context.Background())["default/real"] {
		t.Fatal("the engine was not adopted after its bin/ link was repointed")
	}

	// Neither recorded path matches any more: still ours, so stopped.
	rec["executable"], rec["launchPath"] = "/not/the/engine", "/not/the/link"
	data, _ = json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(recordDir, "default_real.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionStopped))
	if newRealInspectorAgent(stateDir).adoptEngines(context.Background())["default/real"] {
		t.Fatal("adopted an engine whose recorded executable no longer matches")
	}
	if d := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionStopped)) - before; d != 1 {
		t.Fatalf("stopped counter moved by %v, want 1 (a token-proven engine must not be discarded)", d)
	}
	if err := syscall.Kill(proc.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("engine still alive after adoption stopped it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(recordDir, "default_real.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the record of a stopped engine was kept: %v", err)
	}
}
