package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

type adoptInspector struct {
	tokens map[int]string
	exes   map[int]string
}

func (a adoptInspector) StartToken(pid int) (string, error) {
	if t, ok := a.tokens[pid]; ok {
		return t, nil
	}
	return "", fmt.Errorf("no process %d", pid)
}

func (a adoptInspector) Executables(pid int) ([]string, error) {
	if e, ok := a.exes[pid]; ok {
		return []string{e}, nil
	}
	return nil, fmt.Errorf("no process %d", pid)
}

// newAdoptAgent builds an agent whose records live in a temp dir, with the given
// inspector, a health checker answering healthy, and killFn recording signals.
// A PID in alive answers the existence probe (signal 0) until it is sent
// SIGTERM or SIGKILL, alone or as a group; every other PID answers ESRCH. No
// real process is ever signalled.
func newAdoptAgent(t *testing.T, in adoptInspector, healthy bool, alive map[int]bool) (*MetalAgent, *[]string) {
	t.Helper()
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient:      fake.NewClientBuilder().WithScheme(newTestScheme()).Build(),
		Namespace:      "default",
		MemoryProvider: &mockMemoryProvider{totalBytes: 128 << 30},
	})
	a.engineRecords = newEngineRecordStore(filepath.Join(t.TempDir(), "engines"), "v-test", in, nil)
	a.inspect = in
	a.healthChecker = &mockHealthChecker{result: healthy}
	live := map[int]bool{}
	for pid, ok := range alive {
		live[pid] = ok
	}
	var sent []string
	orig := killFn
	killFn = func(pid int, sig syscall.Signal) error {
		sent = append(sent, fmt.Sprintf("%d:%d", pid, sig))
		target := max(pid, -pid)
		switch {
		case sig == syscall.SIGTERM || sig == syscall.SIGKILL:
			live[target] = false
		case sig == 0 && !live[target]:
			return syscall.ESRCH
		}
		return nil
	}
	t.Cleanup(func() { killFn = orig })
	return a, &sent
}

func writeRunning(t *testing.T, a *MetalAgent, p *ManagedProcess) {
	t.Helper()
	if err := a.engineRecords.WriteRunning(p); err != nil {
		t.Fatal(err)
	}
}

// writeSpawned writes the provisional record of engine PID 4242 on port 18080,
// bound to loopback (relay mode), as the executor's OnSpawned hook would.
func writeSpawned(t *testing.T, a *MetalAgent) {
	t.Helper()
	writeSpawnedOn(t, a, engineBindHost)
}

// writeSpawnedOn is writeSpawned with the engine bound to bindHost.
func writeSpawnedOn(t *testing.T, a *MetalAgent, bindHost string) {
	t.Helper()
	if err := a.engineRecords.WriteProvisional("default", "svc", runtimeLlamaServer, SpawnedProcess{
		PID: 4242, PGID: 4242, Port: 18080, Executable: "/bin/engine", BindHost: bindHost,
	}); err != nil {
		t.Fatal(err)
	}
}

func recordExists(t *testing.T, a *MetalAgent, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(a.engineRecords.Dir(), "default_"+name+".json"))
	return !errors.Is(err, os.ErrNotExist)
}

// onlyProbes fails the test when any signal other than the existence probe was
// sent.
func onlyProbes(t *testing.T, sent []string, what string) {
	t.Helper()
	for _, s := range sent {
		if !strings.HasSuffix(s, ":0") {
			t.Fatalf("signal %s sent to %s", s, what)
		}
	}
}

func TestAdopt_HealthyEngineIsAdopted(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	// WriteRunning keeps Executable/PGID only from a provisional record; write one first.
	writeSpawned(t, a)
	started := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	writeRunning(t, a, &ManagedProcess{Name: "svc", Namespace: "default", PID: 4242, Port: 18080,
		Runtime: runtimeLlamaServer, SpecHash: "h", AdmittedBytes: 40 << 30, StartedAt: started,
		Priority: "high", EvictionProtection: true, ModelPath: "/m/x.gguf"})
	before := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionAdopted))

	adopted := a.adoptEngines(context.Background())
	p := a.processes["default/svc"]
	if !adopted["default/svc"] || p == nil || !p.Healthy || p.SpecHash != "h" ||
		p.AdmittedBytes != 40<<30 || p.Port != 18080 {
		t.Fatalf("adopted=%v process=%+v, want the recorded engine adopted with its fields", adopted, p)
	}
	if !p.Adopted || p.PID != 4242 || p.PGID != 4242 || p.StartToken != "t1" || !p.StartedAt.Equal(started) ||
		p.Priority != "high" || !p.EvictionProtection || p.ModelPath != "/m/x.gguf" || p.Runtime != runtimeLlamaServer {
		t.Fatalf("process = %+v, want every recorded field restored and Adopted set", p)
	}
	onlyProbes(t, *sent, "an engine that was adopted")
	if !recordExists(t, a, "svc") {
		t.Fatal("the record of an adopted engine was removed")
	}
	// The admitted estimate counts as committed, so admission sees the
	// adopted engine's memory (#2080).
	if got, _ := a.committedMemoryLocked(""); got != 40<<30 {
		t.Fatalf("committed memory = %d, want the adopted engine's %d", got, uint64(40<<30))
	}
	if d := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionAdopted)) - before; d != 1 {
		t.Fatalf("adopted counter moved by %v, want 1", d)
	}
}

func TestAdopt_ReusedPIDIsNeverSignalled(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "NEW"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	writeSpawned(t, a)
	// The record carries the OLD token: rewrite it by hand.
	rec := readRecord(t, a.engineRecords.Dir(), "svc")
	rec["startToken"] = "OLD"
	data, _ := json.Marshal(rec)
	_ = os.WriteFile(filepath.Join(a.engineRecords.Dir(), "default_svc.json"), data, 0o600)
	before := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionDiscarded))

	if a.adoptEngines(context.Background())["default/svc"] {
		t.Fatal("adopted a process whose start token changed")
	}
	onlyProbes(t, *sent, "a reused PID")
	if recordExists(t, a, "svc") {
		t.Fatal("the stale record was kept")
	}
	if d := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionDiscarded)) - before; d != 1 {
		t.Fatalf("discarded counter moved by %v, want 1", d)
	}
}

func TestAdopt_DeadPIDIsDiscarded(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, true, nil) // kill(4242, 0) answers ESRCH
	writeSpawned(t, a)

	if a.adoptEngines(context.Background())["default/svc"] || len(a.processes) != 0 {
		t.Fatal("adopted an engine whose PID is dead")
	}
	onlyProbes(t, *sent, "a dead PID")
	if recordExists(t, a, "svc") {
		t.Fatal("the record of a dead engine was kept")
	}
}

// requireStoppedAsGroup asserts adoption stopped engine 4242 as a group,
// removed its record and counted it as stopped.
func requireStoppedAsGroup(t *testing.T, a *MetalAgent, sent []string, stoppedBefore float64) {
	t.Helper()
	if !slices.Contains(sent, fmt.Sprintf("%d:%d", -4242, syscall.SIGTERM)) {
		t.Fatalf("signals = %v, want SIGTERM to process group 4242", sent)
	}
	if recordExists(t, a, "svc") {
		t.Fatal("the record of a stopped engine was kept")
	}
	if d := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionStopped)) - stoppedBefore; d != 1 {
		t.Fatalf("stopped counter moved by %v, want 1", d)
	}
}

// The start token proves the process is the one the agent forked, so a
// changed executable (a wrapper that exec'd the engine, say) makes it ours but
// not adoptable: stopped and started fresh, never discarded and orphaned.
func TestAdopt_ExecutableMismatchIsStopped(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	writeSpawned(t, a)
	a.inspect = adoptInspector{tokens: in.tokens, exes: map[int]string{4242: "/usr/bin/other"}}
	before := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionStopped))

	if a.adoptEngines(context.Background())["default/svc"] {
		t.Fatal("adopted a process running a different binary")
	}
	requireStoppedAsGroup(t, a, *sent, before)
}

// An engine whose executable changed and that could not be stopped may still
// run: its record stays for a later stop.
func TestAdopt_ExecutableMismatchFailedStopKeepsRecord(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/usr/bin/other"}}
	a, _ := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	writeSpawned(t, a)
	scripted := killFn
	killFn = func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGTERM {
			return syscall.EPERM
		}
		return scripted(pid, sig)
	}

	if a.adoptEngines(context.Background())["default/svc"] {
		t.Fatal("adopted a process running a different binary")
	}
	if !recordExists(t, a, "svc") {
		t.Fatal("the record of an engine that may still run was removed")
	}
}

// The executable as launched (a Homebrew bin/ link) is recorded beside the
// resolved one; a match on either adopts.
func TestAdopt_LaunchPathMatchAdopts(t *testing.T) {
	const link = "/opt/homebrew/bin/llama-server"
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: link}}
	a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	if err := a.engineRecords.WriteProvisional("default", "svc", runtimeLlamaServer, SpawnedProcess{
		PID: 4242, PGID: 4242, Port: 18080, BindHost: engineBindHost,
		Executable: "/opt/homebrew/Cellar/llama.cpp/1/bin/llama-server", LaunchPath: link,
	}); err != nil {
		t.Fatal(err)
	}

	if !a.adoptEngines(context.Background())["default/svc"] {
		t.Fatal("did not adopt an engine whose recorded launch path matches")
	}
	onlyProbes(t, *sent, "an engine that was adopted")
}

// A token-proven engine whose executable cannot be read is not adoptable, but
// it is still ours: stopped, not discarded.
func TestAdopt_InspectionErrorIsStopped(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	writeSpawned(t, a)
	a.inspect = adoptInspector{tokens: in.tokens} // Executables fails
	before := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionStopped))

	if a.adoptEngines(context.Background())["default/svc"] {
		t.Fatal("adopted a process whose executable could not be read")
	}
	requireStoppedAsGroup(t, a, *sent, before)
}

// A record of an engine that does not lead its own process group is not one
// the agent spawned: never adopted or signalled.
func TestAdopt_RecordWithoutGroupIsDiscarded(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	// Serving, so a missing group check shows as an adoption rather than as
	// stopEngineGroup's refusal.
	a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	if err := a.engineRecords.WriteProvisional("default", "svc", runtimeLlamaServer,
		SpawnedProcess{PID: 4242, Port: 18080, Executable: "/bin/engine", BindHost: engineBindHost}); err != nil {
		t.Fatal(err)
	}

	if a.adoptEngines(context.Background())["default/svc"] {
		t.Fatal("adopted an engine with no process group")
	}
	if len(*sent) != 0 {
		t.Fatalf("signals %v sent for a record with no process group", *sent)
	}
}

func TestAdopt_CorruptAndUnknownVersionRecordsAreIgnored(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	writeSpawned(t, a)
	rec := readRecord(t, a.engineRecords.Dir(), "svc")
	rec["version"] = engineRecordVersion + 1
	data, _ := json.Marshal(rec)
	dir := a.engineRecords.Dir()
	_ = os.WriteFile(filepath.Join(dir, "default_svc.json"), data, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "default_junk.json"), []byte("{not json"), 0o600)

	if adopted := a.adoptEngines(context.Background()); len(adopted) != 0 || len(a.processes) != 0 {
		t.Fatalf("adopted %v from records it cannot read", adopted)
	}
	if len(*sent) != 0 {
		t.Fatalf("signals %v sent for records that were never parsed", *sent)
	}
}

func TestAdopt_ProvisionalNotServingIsStopped(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, false, map[int]bool{4242: true}) // health fails
	writeSpawned(t, a)
	before := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionStopped))

	if a.adoptEngines(context.Background())["default/svc"] {
		t.Fatal("adopted an engine that is not serving")
	}
	if !slices.Contains(*sent, fmt.Sprintf("%d:%d", -4242, syscall.SIGTERM)) {
		t.Fatalf("signals = %v, want SIGTERM to process group 4242", *sent)
	}
	if recordExists(t, a, "svc") {
		t.Fatal("the record of a stopped engine was kept")
	}
	if d := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionStopped)) - before; d != 1 {
		t.Fatalf("stopped counter moved by %v, want 1", d)
	}
}

// A non-serving engine the agent could not stop may still run, so its record
// stays for a later stop.
func TestAdopt_FailedStopKeepsTheRecord(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, _ := newAdoptAgent(t, in, false, map[int]bool{4242: true})
	writeSpawned(t, a)
	scripted := killFn
	killFn = func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGTERM {
			_ = scripted(pid, sig)
			return syscall.EPERM
		}
		return scripted(pid, sig)
	}

	if a.adoptEngines(context.Background())["default/svc"] {
		t.Fatal("adopted an engine that is not serving")
	}
	if !recordExists(t, a, "svc") {
		t.Fatal("the record of an engine that may still run was removed")
	}
}

// A health probe cut short because the agent is stopping proves nothing: the
// engine is neither stopped nor forgotten.
func TestAdopt_CancelledContextStopsNothing(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, false, map[int]bool{4242: true})
	writeSpawned(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	a.healthChecker = cancelingChecker{cancel: cancel}

	if a.adoptEngines(ctx)["default/svc"] {
		t.Fatal("adopted an engine whose health probe was cut short")
	}
	onlyProbes(t, *sent, "an engine whose health probe was cut short")
	if !recordExists(t, a, "svc") {
		t.Fatal("the record was removed although the engine was never judged")
	}
}

// cancelingChecker ends the context during the probe, as a SIGTERM to the
// agent would, and reports the probe failed.
type cancelingChecker struct{ cancel context.CancelFunc }

func (c cancelingChecker) Check(ctx context.Context, _ int, _ string) (bool, error) {
	c.cancel()
	return false, ctx.Err()
}

// An engine bound for another agent mode is ours but not wanted: a legacy
// engine on every interface must not keep serving unauthenticated beside a
// relay-mode agent, and a loopback engine is unreachable for a legacy-mode
// one. It is stopped as a group, never adopted.
func TestAdopt_BindHostChangeStopsTheEngine(t *testing.T) {
	cases := []struct {
		name     string
		legacy   bool
		recorded string
	}{
		{"legacy engine, relay agent", false, legacyBindHost},
		{"relay engine, legacy agent", true, engineBindHost},
		{"no bind host recorded", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
			a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})
			a.config.LegacyDirectEndpoints = tc.legacy
			writeSpawnedOn(t, a, tc.recorded)
			before := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionStopped))

			if a.adoptEngines(context.Background())["default/svc"] {
				t.Fatal("adopted an engine bound for another agent mode")
			}
			if !slices.Contains(*sent, fmt.Sprintf("%d:%d", -4242, syscall.SIGTERM)) {
				t.Fatalf("signals = %v, want SIGTERM to process group 4242", *sent)
			}
			if recordExists(t, a, "svc") {
				t.Fatal("the record of a stopped engine was kept")
			}
			if d := testutil.ToFloat64(engineAdoptionsTotal.WithLabelValues(adoptionStopped)) - before; d != 1 {
				t.Fatalf("stopped counter moved by %v, want 1", d)
			}
		})
	}
}

func TestAdopt_LegacyAgentAdoptsLegacyEngine(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	a.config.LegacyDirectEndpoints = true
	writeSpawnedOn(t, a, legacyBindHost)

	if !a.adoptEngines(context.Background())["default/svc"] {
		t.Fatal("legacy-mode agent did not adopt an engine bound for legacy mode")
	}
	onlyProbes(t, *sent, "an engine that was adopted")
}

// watchFixture gives the adoption agent an API with the InferenceService "svc"
// (its Model on metal), a registry and a watcher polling every 10 ms.
func watchFixture(t *testing.T, a *MetalAgent) *inferencev1alpha1.InferenceService {
	t.Helper()
	model := ggufModel(t, "svc")
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "svc"},
	}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{}).WithObjects(model, isvc).Build()
	a.config.K8sClient = c
	a.registry = NewServiceRegistry(c, "10.0.0.1", newNopLogger(), "")
	a.watcher = NewInferenceServiceWatcher(c, "default", newNopLogger())
	a.watcher.SetPollInterval(10 * time.Millisecond)
	return isvc
}

// handleUntil runs the watcher and hands each event to the agent, as Start's
// event loop does, until done reports the event it waited for was handled.
func handleUntil(t *testing.T, a *MetalAgent, done func(InferenceServiceEvent) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := make(chan InferenceServiceEvent)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		_ = a.watcher.Watch(ctx, events)
	}()
	defer func() { cancel(); <-watchDone }()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for the watcher's event")
		case ev := <-events:
			if err := a.handleEvent(ctx, ev); err != nil {
				t.Fatalf("handle %s %s: %v", ev.Type, ev.InferenceService.Name, err)
			}
			if done(ev) {
				return
			}
		}
	}
}

// An adopted engine whose InferenceService still exists but is no longer this
// agent's (here: left off the allowlist) is stopped by the first poll.
func TestAdopt_UnwatchedServiceIsStoppedOnFirstPoll(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	watchFixture(t, a)
	a.watcher.SetNameAllowlist([]string{"another-isvc"})
	writeSpawned(t, a)
	writeRunning(t, a, &ManagedProcess{Name: "svc", Namespace: "default", PID: 4242, Port: 18080,
		Runtime: runtimeLlamaServer, SpecHash: "h"})
	if !a.startAdoption(context.Background())["default/svc"] {
		t.Fatal("engine not adopted")
	}

	handleUntil(t, a, func(ev InferenceServiceEvent) bool { return ev.Type == EventTypeDeleted })

	if _, ok := a.processes["default/svc"]; ok {
		t.Fatal("the adopted engine of an unwatched service is still managed")
	}
	if !slices.Contains(*sent, fmt.Sprintf("%d:%d", -4242, syscall.SIGTERM)) {
		t.Fatalf("signals = %v, want SIGTERM to process group 4242", *sent)
	}
	if recordExists(t, a, "svc") {
		t.Fatal("the record of the stopped engine was kept")
	}
}

// An adopted engine whose service is still owned with the same spec keeps
// running through the first list and poll.
func TestAdopt_OwnedServiceIsNotRestarted(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	isvc := watchFixture(t, a)
	writeSpawned(t, a)
	writeRunning(t, a, &ManagedProcess{Name: "svc", Namespace: "default", PID: 4242, Port: 18080,
		Runtime: runtimeLlamaServer, SpecHash: computeSpecHash(isvc)})
	if !a.startAdoption(context.Background())["default/svc"] {
		t.Fatal("engine not adopted")
	}

	// The first list sends Created; the seeded key makes the first poll send Updated.
	handleUntil(t, a, func(ev InferenceServiceEvent) bool { return ev.Type == EventTypeUpdated })

	if p := a.processes["default/svc"]; p == nil || p.PID != 4242 || !p.Adopted {
		t.Fatalf("process = %+v, want the adopted engine kept", p)
	}
	onlyProbes(t, *sent, "an owned engine with an unchanged spec")
	if !recordExists(t, a, "svc") {
		t.Fatal("the record of the kept engine was removed")
	}
}

func TestAdopt_OMLXRecordsAreDiscarded(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{777: "t"}, exes: map[int]string{777: "/bin/omlx"}}
	a, sent := newAdoptAgent(t, in, true, map[int]bool{777: true})
	writeRunning(t, a, &ManagedProcess{Name: "o", Namespace: "default", PID: 777, Runtime: runtimeOMLX, ModelID: "m"})
	if a.adoptEngines(context.Background())["default/o"] || len(*sent) != 0 {
		t.Fatalf("oMLX record adopted or signalled (signals %v)", *sent)
	}
	if recordExists(t, a, "o") {
		t.Fatal("the oMLX record was kept")
	}
}

func TestAdopt_OllamaIsReattachedWhenHealthy(t *testing.T) {
	a, sent := newAdoptAgent(t, adoptInspector{}, true, nil)
	writeRunning(t, a, &ManagedProcess{Name: "l", Namespace: "default", PID: 0, Port: 11434,
		Runtime: runtimeOllama, ModelID: "llama3:8b"})
	if !a.adoptEngines(context.Background())["default/l"] {
		t.Fatal("healthy Ollama service not re-attached")
	}
	if p := a.processes["default/l"]; p == nil || p.ModelID != "llama3:8b" || p.PID != 0 {
		t.Fatalf("re-attached %+v, want the Ollama model with no PID", p)
	}
	if got := a.healthChecker.(*mockHealthChecker).probed[11434]; got != "/" {
		t.Fatalf("probed %q on the Ollama port, want Ollama's health path /", got)
	}
	if len(*sent) != 0 {
		t.Fatalf("signals %v sent for an external daemon", *sent)
	}
}

func TestAdopt_UnhealthyOllamaIsDiscarded(t *testing.T) {
	a, sent := newAdoptAgent(t, adoptInspector{}, false, nil)
	writeRunning(t, a, &ManagedProcess{Name: "l", Namespace: "default", PID: 0, Port: 11434,
		Runtime: runtimeOllama, ModelID: "llama3:8b"})
	if a.adoptEngines(context.Background())["default/l"] || len(*sent) != 0 {
		t.Fatalf("unhealthy Ollama service re-attached or signalled (signals %v)", *sent)
	}
	if recordExists(t, a, "l") {
		t.Fatal("the record of an unhealthy Ollama service was kept")
	}
}

func TestAdopt_RunsWithoutAPIServer(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, _ := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	a.config.K8sClient = nil // adoption must not touch the API
	writeSpawned(t, a)
	if !a.adoptEngines(context.Background())["default/svc"] {
		t.Fatal("adoption needed the API server")
	}
}

// An adopted engine is not this process's child: stopping it signals the
// recorded group after re-proving its identity, not the executor's PID path.
func TestStopManagedProcess_AdoptedEngineStopsTheGroup(t *testing.T) {
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})
	writeSpawned(t, a)
	a.adoptEngines(context.Background())
	p := a.processes["default/svc"]
	if p == nil {
		t.Fatal("engine not adopted")
	}

	if err := a.stopManagedProcess(context.Background(), p, true); err != nil {
		t.Fatalf("stop adopted engine: %v", err)
	}
	if !slices.Contains(*sent, fmt.Sprintf("%d:%d", -4242, syscall.SIGTERM)) {
		t.Fatalf("signals = %v, want SIGTERM to process group 4242", *sent)
	}
}

// A re-attached Ollama service has no PID; stopping it unloads its model
// through the executor as before.
func TestStopManagedProcess_AdoptedOllamaUnloadsThroughExecutor(t *testing.T) {
	unloaded := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/api/generate" {
			unloaded <- body.Model
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	a, sent := newAdoptAgent(t, adoptInspector{}, true, nil)
	a.executors[runtimeOllama] = NewOllamaExecutor(port, newNopLogger())
	writeRunning(t, a, &ManagedProcess{Name: "l", Namespace: "default", Port: port,
		Runtime: runtimeOllama, ModelID: "llama3:8b"})
	a.adoptEngines(context.Background())
	p := a.processes["default/l"]
	if p == nil || !p.Adopted {
		t.Fatalf("Ollama service not re-attached: %+v", p)
	}

	if err := a.stopManagedProcess(context.Background(), p, true); err != nil {
		t.Fatalf("stop re-attached Ollama service: %v", err)
	}
	select {
	case m := <-unloaded:
		if m != "llama3:8b" {
			t.Fatalf("unloaded %q, want llama3:8b", m)
		}
	default:
		t.Fatal("the Ollama model was not unloaded")
	}
	if len(*sent) != 0 {
		t.Fatalf("signals %v sent for an external daemon", *sent)
	}
}

// The startup withdrawal leaves an adopted engine's endpoint Ready: it is still
// serving, and the heartbeat refreshes it.
func TestWithdrawInheritedEndpoints_SkipsAdopted(t *testing.T) {
	const adopted, gone = "adopted-isvc", "gone-isvc"
	isvcFor := func(name string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: name},
		}
	}
	adoptedSvc, adoptedSlice := inheritedEndpoint(adopted)
	goneSvc, goneSlice := inheritedEndpoint(gone)
	agent, c := newWithdrawTestAgent(t, nil,
		ggufModel(t, adopted), ggufModel(t, gone), isvcFor(adopted), isvcFor(gone),
		adoptedSvc, adoptedSlice, goneSvc, goneSlice)
	agent.watcher = NewInferenceServiceWatcher(c, "default", newNopLogger())
	agent.adopted = map[string]bool{"default/" + adopted: true}

	agent.withdrawInheritedEndpoints(context.Background())

	if ready, _ := endpointReady(t, c, adopted); !ready {
		t.Errorf("startup withdrawal withdrew the endpoint of an adopted engine")
	}
	if ready, _ := endpointReady(t, c, gone); ready {
		t.Errorf("startup withdrawal left the endpoint of a service with no engine Ready")
	}
}
