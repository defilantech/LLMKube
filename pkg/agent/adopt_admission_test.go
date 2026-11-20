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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The issue's admitted figure: the 0.10.17 agent charged phi-4-mini 3.3 GB.
const adoptedSeedBytes = 3_300_000_000

// adoptAdmissionAgent builds an agent that adopts engine PID 4242 through the
// Linux-safe adopt_test.go stubs, with model (when non-nil) and isvcs in its
// fake client, a 64 GiB host at the 0.75 fraction (a 48 GiB budget), and a
// buffered event recorder. sent collects every signal killFn is asked for.
func adoptAdmissionAgent(
	t *testing.T,
	model *inferencev1alpha1.Model,
	isvcs ...*inferencev1alpha1.InferenceService,
) (*MetalAgent, *record.FakeRecorder, *[]string) {
	t.Helper()
	in := adoptInspector{tokens: map[int]string{4242: "t1"}, exes: map[int]string{4242: "/bin/engine"}}
	a, sent := newAdoptAgent(t, in, true, map[int]bool{4242: true})

	b := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{})
	if model != nil {
		b = b.WithObjects(model)
	}
	for _, isvc := range isvcs {
		b = b.WithObjects(isvc)
	}
	rec := record.NewFakeRecorder(16)
	a.config.K8sClient = b.Build()
	a.config.EventRecorder = rec
	a.config.ModelStorePath = t.TempDir()
	a.memoryProvider = &mockMemoryProvider{totalBytes: cumulativeTotal}
	a.memoryFraction = 0.75
	return a, rec, sent
}

// adoptEngineRecord writes and adopts the engine record of default/<name>, as
// the previous agent process left it.
func adoptEngineRecord(t *testing.T, a *MetalAgent, isvc *inferencev1alpha1.InferenceService) {
	t.Helper()
	if err := a.engineRecords.WriteProvisional("default", "phi", runtimeLlamaServer, SpawnedProcess{
		PID: 4242, PGID: 4242, Port: 18080, Executable: "/bin/engine", BindHost: engineBindHost,
	}); err != nil {
		t.Fatal(err)
	}
	writeRunning(t, a, &ManagedProcess{
		Name: "phi", Namespace: "default", PID: 4242, Port: 18080, Runtime: runtimeLlamaServer,
		SpecHash: computeSpecHash(isvc), AdmittedBytes: adoptedSeedBytes, ModelPath: "/models/phi.gguf",
	})
	if adopted := a.adoptEngines(context.Background()); !adopted["default/phi"] {
		t.Fatal("engine phi was not adopted")
	}
}

// refreshedISVC reads default/phi, the engine every fixture here adopts, back
// from the fake client.
func refreshedISVC(t *testing.T, a *MetalAgent) *inferencev1alpha1.InferenceService {
	t.Helper()
	got := &inferencev1alpha1.InferenceService{}
	key := types.NamespacedName{Namespace: "default", Name: "phi"}
	if err := a.config.K8sClient.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get InferenceService phi: %v", err)
	}
	return got
}

// wantReestimatedCharge fails unless got is the 33.0 GiB Model's current
// estimate: 33 * 1.2 + 0.5 = 40.1 GiB through fallbackEstimate.
func wantReestimatedCharge(t *testing.T, got uint64) {
	t.Helper()
	if got < gib(40) || got > gib(40.2) {
		t.Fatalf("AdmittedBytes = %s (%d), want the re-estimated 40.1 GB", formatMemory(got), got)
	}
}

// adoptedReportClock puts a watcher and a fake clock on the agent, which is
// what observing the report's re-check needs.
func adoptedReportClock(t *testing.T, a *MetalAgent) *fakeClock {
	t.Helper()
	a.watcher = a.newWatcher()
	clk := &fakeClock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	a.now = clk.now
	a.watcher.now = clk.now
	return clk
}

// adoptedRecheckIn returns how long after the fake clock's now the report of
// default/phi is re-checked, failing when none is scheduled.
func adoptedRecheckIn(t *testing.T, a *MetalAgent, clk *fakeClock) time.Duration {
	t.Helper()
	a.watcher.requeueMu.Lock()
	defer a.watcher.requeueMu.Unlock()
	at, ok := a.watcher.requeued["default/phi"]
	if !ok {
		t.Fatal("no re-check is scheduled for default/phi")
	}
	return at.Sub(clk.t)
}

// adoptedRecheckQueued reports whether a re-check of default/phi is scheduled.
func adoptedRecheckQueued(t *testing.T, a *MetalAgent) bool {
	t.Helper()
	a.watcher.requeueMu.Lock()
	defer a.watcher.requeueMu.Unlock()
	_, ok := a.watcher.requeued["default/phi"]
	return ok
}

// adoptedReconcileAt consumes the re-check due for default/phi, advancing the
// fake clock to it, and reconciles phi as the watcher would.
func adoptedReconcileAt(t *testing.T, a *MetalAgent, clk *fakeClock) {
	t.Helper()
	a.watcher.requeueMu.Lock()
	if at, ok := a.watcher.requeued["default/phi"]; ok {
		clk.t = at
	}
	a.watcher.requeueMu.Unlock()
	if !a.watcher.takeRequeue("default/phi") {
		t.Fatal("no re-check is due for default/phi")
	}
	if err := a.ensureProcess(context.Background(), refreshedISVC(t, a)); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
}

// The issue's defect: an adopted engine keeps the charge it was admitted with,
// so the next admission counts memory the engine is using as free. After the
// first reconcile the adopted engine must be charged the current estimate, and
// a second service that no longer fits next to it must be refused.
func TestAdoptedEngine_ReestimateCountsTowardCumulativeAdmission(t *testing.T) {
	model := sizedModel("33.0 GiB")
	phi := cumulativeISVC("phi")
	a, _, _ := adoptAdmissionAgent(t, model, phi)
	adoptEngineRecord(t, a, phi)

	if err := a.ensureProcess(context.Background(), phi); err != nil {
		t.Fatalf("ensureProcess on an adopted engine: %v", err)
	}

	p := a.processes["default/phi"]
	if p == nil {
		t.Fatal("the adopted engine left the process table")
	}
	if p.AdmittedBytes < gib(40) || p.AdmittedBytes > gib(40.2) {
		t.Errorf("adopted engine charged %s (%d), want the current 40.1 GB estimate",
			formatMemory(p.AdmittedBytes), p.AdmittedBytes)
	}
	if got, _ := a.committedMemoryLocked("", false); got != p.AdmittedBytes {
		t.Errorf("committed memory = %s, want the adopted engine's re-estimated charge %s",
			formatMemory(got), formatMemory(p.AdmittedBytes))
	}
	if got := testutil.ToFloat64(memoryEstimatedBytes.WithLabelValues("phi", "default")); got != float64(p.AdmittedBytes) {
		t.Errorf("memoryEstimatedBytes = %v, want the re-estimated charge %d", got, p.AdmittedBytes)
	}
	if got := testutil.ToFloat64(memoryCommittedBytes); got != float64(p.AdmittedBytes) {
		t.Errorf("memoryCommittedBytes = %v, want the re-estimated charge %d", got, p.AdmittedBytes)
	}
	// The record stays the seed the next agent adopts; only the in-memory
	// charge is refreshed.
	if rec := readRecord(t, a.engineRecords.Dir(), "phi"); rec["admittedBytes"] != float64(adoptedSeedBytes) {
		t.Errorf("engine record admittedBytes = %v, want the seed %d untouched", rec["admittedBytes"], adoptedSeedBytes)
	}

	second := cumulativeISVC("second")
	if err := a.config.K8sClient.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	err := a.checkMemoryAdmission(context.Background(), second, model, 2048, "", "")
	if err == nil {
		t.Fatalf("a second 40.1 GB model was admitted next to an adopted engine charging %s",
			formatMemory(p.AdmittedBytes))
	}
	for _, want := range []string{"40.1 GB required", "default/phi", "48.0 GB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
}

// The startup list delivers services in list order, so a service that sorts
// before an adopted engine is admitted first. Its admission must re-estimate the
// adopted engine before it measures the fit: the recorded charge is not the
// memory the engine is using (#2535).
func TestAdoptedEngine_AdmissionReestimatesAPendingEngineFirst(t *testing.T) {
	model := sizedModel("33.0 GiB")
	model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{Accelerator: "metal"}
	phi := cumulativeISVC("phi")
	alpha := cumulativeISVC("alpha")
	a, _, _ := adoptAdmissionAgent(t, model, phi, alpha)
	adoptEngineRecord(t, a, phi)

	w := a.newWatcher()
	ch := make(chan InferenceServiceEvent, 8)
	if err := w.listExisting(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	close(ch)

	var order []string
	var alphaErr error
	for ev := range ch {
		isvc := ev.InferenceService
		order = append(order, isvc.Name)
		switch isvc.Name {
		case "phi":
			if err := a.ensureProcess(context.Background(), isvc); err != nil {
				t.Fatal(err)
			}
		case "alpha":
			// The admission step of alpha's fresh start, with the inputs
			// reconcileProcess derives for it.
			cacheTypeK, cacheTypeV := resolveCacheTypes(isvc)
			alphaErr = a.checkMemoryAdmission(context.Background(), isvc, model,
				admissionContextSize(isvc), cacheTypeK, cacheTypeV)
		}
	}
	t.Logf("initial list order: %v", order)
	if len(order) != 2 || order[0] != "alpha" || order[1] != "phi" {
		t.Fatalf("initial list order = %v, want alpha admitted before the adopted phi", order)
	}

	wantReestimatedCharge(t, a.processes["default/phi"].AdmittedBytes)
	if alphaErr == nil {
		a.mu.Lock()
		committed, holders := a.committedMemoryLocked("", false)
		a.mu.Unlock()
		t.Fatalf("alpha (40.1 GB) was admitted before the adopted engine's charge was re-estimated; "+
			"committed now %s across %v", formatMemory(committed), holders)
	}
	if !strings.Contains(alphaErr.Error(), "default/phi") {
		t.Errorf("refusal %q does not name the adopted engine", alphaErr.Error())
	}
}

// An admitted service whose InferenceService cannot be read keeps the adopted
// engine at the charge admission already counts, rather than guessing (#2535).
func TestAdoptedEngine_AdmissionKeepsRecordedChargeWhenTheServiceIsUnreadable(t *testing.T) {
	model := sizedModel("33.0 GiB")
	phi := cumulativeISVC("phi")
	alpha := cumulativeISVC("alpha")
	a, _, _ := adoptAdmissionAgent(t, model, phi, alpha)
	adoptEngineRecord(t, a, phi)
	if err := a.config.K8sClient.Delete(context.Background(), phi); err != nil {
		t.Fatal(err)
	}

	cacheTypeK, cacheTypeV := resolveCacheTypes(alpha)
	if err := a.checkMemoryAdmission(context.Background(), alpha, model,
		admissionContextSize(alpha), cacheTypeK, cacheTypeV); err != nil {
		t.Fatalf("an unreadable adopted engine's InferenceService failed the admission: %v", err)
	}
	if got := a.processes["default/phi"].AdmittedBytes; got != adoptedSeedBytes {
		t.Errorf("AdmittedBytes = %d, want the recorded %d", got, adoptedSeedBytes)
	}
}

// The issue's configuration: a 12Gi Model budget that the current estimate no
// longer fits. The adopted engine keeps serving and is reported through the
// admission status and one Warning event; a second reconcile reports nothing.
func TestAdoptedEngine_OverBudgetKeepsServingAndIsReported(t *testing.T) {
	model := sizedModel("33.0 GiB")
	model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{MemoryBudget: "12Gi"}
	phi := cumulativeISVC("phi")
	phi.Spec.ContextSize = ptrInt32(131072)
	a, rec, sent := adoptAdmissionAgent(t, model, phi)
	adoptEngineRecord(t, a, phi)

	if err := a.ensureProcess(context.Background(), phi); err != nil {
		t.Fatalf("the re-estimate failed an adopted engine that must keep serving: %v", err)
	}

	p := a.processes["default/phi"]
	if p == nil || !p.Adopted || !p.Healthy || p.PID != 4242 {
		t.Fatalf("process = %+v, want the adopted engine still serving", p)
	}
	onlyProbes(t, *sent, "an adopted engine over its re-estimated budget")
	wantReestimatedCharge(t, p.AdmittedBytes)

	status, msg := schedulingStatus(t, a, "phi")
	if status != EventReasonInsufficientMemory {
		t.Errorf("SchedulingStatus = %q, want %q", status, EventReasonInsufficientMemory)
	}
	for _, want := range []string{"serving over its memory budget", "40.1 GB required", "12.0 GB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("SchedulingMessage %q does not mention %q", msg, want)
		}
	}
	events := drainEvents(rec)
	if len(events) != 1 || !strings.HasPrefix(events[0], corev1.EventTypeWarning+" "+EventReasonInsufficientMemory) {
		t.Fatalf("events = %q, want exactly one Warning %s", events, EventReasonInsufficientMemory)
	}

	if err := a.ensureProcess(context.Background(), refreshedISVC(t, a)); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Fatalf("a second reconcile reported the charge again: %q", events)
	}
	if status, _ := schedulingStatus(t, a, "phi"); status != EventReasonInsufficientMemory {
		t.Errorf("the report did not survive a second reconcile: SchedulingStatus = %q, want %q",
			status, EventReasonInsufficientMemory)
	}

	// A Model budget above the host's is clamped to it, so a 49 GiB model
	// (59.3 GB) is over the host's 48 GiB budget even though it is under the
	// Model's 100Gi: without the clamp the re-estimate would pass silently.
	t.Run("clamped host budget", func(t *testing.T) {
		clamped := sizedModel("49.0 GiB")
		clamped.Spec.Hardware = &inferencev1alpha1.HardwareSpec{MemoryBudget: "100Gi"}
		phi := cumulativeISVC("phi")
		a, rec, _ := adoptAdmissionAgent(t, clamped, phi)
		adoptEngineRecord(t, a, phi)

		if err := a.ensureProcess(context.Background(), phi); err != nil {
			t.Fatalf("the re-estimate failed an adopted engine that must keep serving: %v", err)
		}
		if status, _ := schedulingStatus(t, a, "phi"); status != EventReasonInsufficientMemory {
			t.Errorf("SchedulingStatus = %q, want %q", status, EventReasonInsufficientMemory)
		}
		reported := false
		for _, e := range drainEvents(rec) {
			if strings.HasPrefix(e, corev1.EventTypeWarning+" "+EventReasonInsufficientMemory) {
				reported = true
				if !strings.Contains(e, "clamped to this host's") {
					t.Errorf("event %q does not name the clamped host budget", e)
				}
			}
		}
		if !reported {
			t.Error("a Model budget the host cannot honor was not reported")
		}
	})
}

// A Model the first reconcile cannot read leaves the recorded charge alone and
// does not mark the re-estimate done, so a later reconcile with the Model in
// hand charges the current estimate.
func TestAdoptedEngine_RetriesAfterTheModelAppears(t *testing.T) {
	model := sizedModel("33.0 GiB")
	phi := cumulativeISVC("phi")
	a, rec, _ := adoptAdmissionAgent(t, nil, phi)
	adoptEngineRecord(t, a, phi)

	if err := a.ensureProcess(context.Background(), phi); err != nil {
		t.Fatalf("a missing Model failed the reconcile: %v", err)
	}
	if got := a.processes["default/phi"].AdmittedBytes; got != adoptedSeedBytes {
		t.Fatalf("AdmittedBytes = %d, want the recorded %d while the Model is unavailable", got, adoptedSeedBytes)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Fatalf("a missing Model produced events: %q", events)
	}

	if err := a.config.K8sClient.Create(context.Background(), model); err != nil {
		t.Fatal(err)
	}
	if err := a.ensureProcess(context.Background(), refreshedISVC(t, a)); err != nil {
		t.Fatalf("reconcile with the Model present: %v", err)
	}
	wantReestimatedCharge(t, a.processes["default/phi"].AdmittedBytes)
}

// A re-estimated charge that fits writes no status and no event: the report
// belongs to an engine the current estimate no longer fits.
func TestAdoptedEngine_FittingReestimateIsNotReported(t *testing.T) {
	model := sizedModel("33.0 GiB")
	phi := cumulativeISVC("phi")
	a, rec, sent := adoptAdmissionAgent(t, model, phi)
	adoptEngineRecord(t, a, phi)

	if err := a.ensureProcess(context.Background(), phi); err != nil {
		t.Fatalf("ensureProcess on an adopted engine: %v", err)
	}
	wantReestimatedCharge(t, a.processes["default/phi"].AdmittedBytes)
	if status, msg := schedulingStatus(t, a, "phi"); status != "" || msg != "" {
		t.Errorf("SchedulingStatus = %q / %q, want no report for an engine that fits", status, msg)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("events = %q, want none for an engine that fits", events)
	}
	onlyProbes(t, *sent, "an adopted engine that fits")
}

// Across a restart, an earlier adoption's report is cleared by the re-estimate
// that fits: the status must not keep saying the engine is over its budget
// after the cause is gone (#2535).
func TestAdoptedEngine_FittingReestimateClearsEarlierReport(t *testing.T) {
	model := sizedModel("33.0 GiB")
	phi := cumulativeISVC("phi")
	phi.Status.SchedulingStatus = EventReasonInsufficientMemory
	phi.Status.SchedulingMessage = "adopted engine is serving over its memory budget: estimated 40.1 GB required, " +
		"40.1 GB of the 48.0 GB budget already committed to 1 service (default/other), 7.9 GB free"
	a, _, _ := adoptAdmissionAgent(t, model, phi)
	adoptEngineRecord(t, a, phi)

	if err := a.ensureProcess(context.Background(), refreshedISVC(t, a)); err != nil {
		t.Fatal(err)
	}
	wantReestimatedCharge(t, a.processes["default/phi"].AdmittedBytes)
	if status, msg := schedulingStatus(t, a, "phi"); status != "" || msg != "" {
		t.Errorf("the re-estimate fits (40.1 GB alone on 48 GB) but the status still reads %q / %q", status, msg)
	}
}

// The clear belongs to the adopted engine's own report: a status another
// writer set, in the same reason, is left alone (#2535).
func TestAdoptedEngine_FittingReestimateLeavesAnotherStatusAlone(t *testing.T) {
	const msg = "estimated 40.1 GB required, 40.1 GB of the 48.0 GB budget already committed to " +
		"1 service (default/other), 7.9 GB free"
	model := sizedModel("33.0 GiB")
	phi := cumulativeISVC("phi")
	phi.Status.SchedulingStatus = EventReasonInsufficientMemory
	phi.Status.SchedulingMessage = msg
	a, _, _ := adoptAdmissionAgent(t, model, phi)
	adoptEngineRecord(t, a, phi)

	if err := a.ensureProcess(context.Background(), refreshedISVC(t, a)); err != nil {
		t.Fatal(err)
	}
	if status, got := schedulingStatus(t, a, "phi"); status != EventReasonInsufficientMemory || got != msg {
		t.Errorf("SchedulingStatus = %q / %q, want the other writer's status left alone", status, got)
	}
}

// A report within one run is cleared once the service holding the memory is
// gone. The report's own re-check drives it: each one that still does not fit
// backs off like a start refusal, and the first that fits clears the status
// (#2535).
func TestAdoptedEngine_ReportClearsWhenTheCauseGoesAway(t *testing.T) {
	model := sizedModel("33.0 GiB")
	phi := cumulativeISVC("phi")
	a, rec, _ := adoptAdmissionAgent(t, model, phi)
	clk := adoptedReportClock(t, a)
	adoptEngineRecord(t, a, phi)
	a.processes["default/other"] = &ManagedProcess{Name: "other", Namespace: "default", AdmittedBytes: gib(40.1)}

	if err := a.ensureProcess(context.Background(), phi); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	if status, _ := schedulingStatus(t, a, "phi"); status != EventReasonInsufficientMemory {
		t.Fatalf("SchedulingStatus = %q, want the over-budget report", status)
	}
	if events := drainEvents(rec); len(events) != 1 {
		t.Fatalf("events = %q, want the report's one Warning", events)
	}
	if got := adoptedRecheckIn(t, a, clk); got != refusalRecheckInitial {
		t.Errorf("first re-check in %s, want %s", got, refusalRecheckInitial)
	}

	// The re-check still does not fit: the report stands, no new Event, and the
	// next re-check waits twice as long.
	adoptedReconcileAt(t, a, clk)
	if status, _ := schedulingStatus(t, a, "phi"); status != EventReasonInsufficientMemory {
		t.Errorf("the re-check cleared a report whose cause still holds: SchedulingStatus = %q", status)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("a re-check reported again: %q", events)
	}
	if got := adoptedRecheckIn(t, a, clk); got != 2*refusalRecheckInitial {
		t.Errorf("second re-check in %s, want %s", got, 2*refusalRecheckInitial)
	}

	// The cause is gone: the next re-check fits and clears the report.
	a.mu.Lock()
	delete(a.processes, "default/other")
	a.mu.Unlock()
	adoptedReconcileAt(t, a, clk)
	if status, msg := schedulingStatus(t, a, "phi"); status != "" || msg != "" {
		t.Fatalf("default/other is gone but phi's status still reads %q / %q", status, msg)
	}
	if adoptedRecheckQueued(t, a) {
		t.Error("a re-check was scheduled for a report that was cleared")
	}

	// A charge over its own budget cannot clear without a respawn, so it is not
	// re-checked: the report stays and nothing is scheduled after the report's
	// own first re-check.
	t.Run("own-budget shortfall is not re-checked", func(t *testing.T) {
		model := sizedModel("33.0 GiB")
		model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{MemoryBudget: "12Gi"}
		phi := cumulativeISVC("phi")
		a, _, _ := adoptAdmissionAgent(t, model, phi)
		clk := adoptedReportClock(t, a)
		adoptEngineRecord(t, a, phi)

		if err := a.ensureProcess(context.Background(), phi); err != nil {
			t.Fatalf("ensureProcess: %v", err)
		}
		if status, _ := schedulingStatus(t, a, "phi"); status != EventReasonInsufficientMemory {
			t.Fatalf("SchedulingStatus = %q, want the over-budget report", status)
		}
		adoptedReconcileAt(t, a, clk)
		if status, _ := schedulingStatus(t, a, "phi"); status != EventReasonInsufficientMemory {
			t.Errorf("SchedulingStatus = %q, want the report to stand until a respawn", status)
		}
		if adoptedRecheckQueued(t, a) {
			t.Error("an own-budget shortfall was re-checked against memory that cannot change it")
		}
	})
}

// A process this agent started is never re-estimated, whatever the Model's
// budget says about its charge: the defect is adoption, and a spawned engine
// keeps the charge its own admission gave it.
func TestAdoptedEngine_SpawnedEngineIsNotReestimated(t *testing.T) {
	model := sizedModel("33.0 GiB")
	model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{MemoryBudget: "12Gi"}
	phi := cumulativeISVC("phi")
	a, rec, sent := adoptAdmissionAgent(t, model, phi)
	a.processes["default/phi"] = &ManagedProcess{
		Name: "phi", Namespace: "default", PID: 4242, Port: 18080, Runtime: runtimeLlamaServer,
		SpecHash: computeSpecHash(phi), AdmittedBytes: adoptedSeedBytes, Healthy: true,
	}

	if err := a.ensureProcess(context.Background(), phi); err != nil {
		t.Fatalf("ensureProcess on a spawned engine: %v", err)
	}
	p := a.processes["default/phi"]
	if p == nil {
		t.Fatal("the spawned engine left the process table")
	}
	if p.AdmittedBytes != adoptedSeedBytes {
		t.Errorf("AdmittedBytes = %d, want the spawned engine's own charge %d unchanged",
			p.AdmittedBytes, adoptedSeedBytes)
	}
	if status, msg := schedulingStatus(t, a, "phi"); status != "" || msg != "" {
		t.Errorf("SchedulingStatus = %q / %q, want no report for a spawned engine", status, msg)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("events = %q, want none for a spawned engine", events)
	}
	onlyProbes(t, *sent, "a spawned engine")
}

// A Model budget above the host's is clamped on the re-estimate path as it is
// on a fresh admission, and the clamp notice is the only report while the
// engine fits.
func TestAdoptedEngine_ClampedBudgetNoticeOnAFittingReestimate(t *testing.T) {
	model := sizedModel("33.0 GiB")
	model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{MemoryBudget: "100Gi"}
	phi := cumulativeISVC("phi")
	a, rec, sent := adoptAdmissionAgent(t, model, phi)
	adoptEngineRecord(t, a, phi)

	if err := a.ensureProcess(context.Background(), phi); err != nil {
		t.Fatalf("ensureProcess on an adopted engine: %v", err)
	}
	wantReestimatedCharge(t, a.processes["default/phi"].AdmittedBytes)
	events := drainEvents(rec)
	if len(events) != 1 || !strings.HasPrefix(events[0], corev1.EventTypeWarning+" "+EventReasonMemoryBudgetClamped) {
		t.Fatalf("events = %q, want exactly one Warning %s", events, EventReasonMemoryBudgetClamped)
	}
	if status, msg := schedulingStatus(t, a, "phi"); status != "" || msg != "" {
		t.Errorf("SchedulingStatus = %q / %q, want no report for an engine that fits", status, msg)
	}
	onlyProbes(t, *sent, "an adopted engine under a clamped Model budget")
}

// The re-estimate must charge exactly what a fresh admission of the same
// InferenceService and Model computes, inputs included: a re-estimate that used
// the default context size or f16 KV would land on a different figure.
func TestAdoptedEngine_ReestimateMatchesFreshAdmission(t *testing.T) {
	model := sizedModel("7.5 GiB")
	model.Status.GGUF = &inferencev1alpha1.GGUFMetadata{
		LayerCount: 32, EmbeddingSize: 4096, HeadCount: 32, HeadCountKV: 8,
		RecurrentStatePerSeq: 64 << 20,
	}
	phi := cumulativeISVC("phi")
	phi.Spec.ContextSize = ptrInt32(131072)
	phi.Spec.CacheTypeK = "q8_0"
	phi.Spec.CacheTypeV = "q8_0"
	phi.Spec.ParallelSlots = ptrInt32(2)

	fresh := phi.DeepCopy()
	freshAgent, _ := newCumulativeAgent(t, MetalAgentConfig{}, fresh)
	cacheTypeK, cacheTypeV := resolveCacheTypes(fresh)
	if err := freshAgent.checkMemoryAdmission(context.Background(), fresh, model, 131072,
		cacheTypeK, cacheTypeV); err != nil {
		t.Fatalf("a fresh admission of the same service was refused: %v", err)
	}
	want := freshAgent.reservedMemory["default/phi"].bytes

	a, _, _ := adoptAdmissionAgent(t, model, phi)
	adoptEngineRecord(t, a, phi)
	if err := a.ensureProcess(context.Background(), phi); err != nil {
		t.Fatalf("ensureProcess on an adopted engine: %v", err)
	}
	if got := a.processes["default/phi"].AdmittedBytes; got != want {
		t.Fatalf("adopted engine charged %d, want a fresh admission's %d", got, want)
	}
}

// The default configuration: an InferenceService with no contextSize. The
// re-estimate must charge what a fresh admission at the agent's default
// context size charges, so the KV term is not sized at zero.
func TestAdoptedEngine_DefaultContextSizeMatchesFreshAdmission(t *testing.T) {
	model := sizedModel("7.5 GiB")
	model.Status.GGUF = &inferencev1alpha1.GGUFMetadata{
		LayerCount: 32, EmbeddingSize: 4096, HeadCount: 32, HeadCountKV: 8,
		RecurrentStatePerSeq: 64 << 20,
	}
	phi := cumulativeISVC("phi")
	phi.Spec.ParallelSlots = ptrInt32(2)

	freshAgent, _ := newCumulativeAgent(t, MetalAgentConfig{}, phi.DeepCopy())
	if err := freshAgent.checkMemoryAdmission(context.Background(), phi.DeepCopy(), model,
		defaultContextSize, "", ""); err != nil {
		t.Fatalf("a fresh admission at the default context size was refused: %v", err)
	}
	want := freshAgent.reservedMemory["default/phi"].bytes

	a, _, _ := adoptAdmissionAgent(t, model, phi)
	adoptEngineRecord(t, a, phi)
	if err := a.ensureProcess(context.Background(), phi); err != nil {
		t.Fatalf("ensureProcess on an adopted engine: %v", err)
	}
	if got := a.processes["default/phi"].AdmittedBytes; got != want {
		t.Fatalf("adopted engine charged %d, want a fresh admission's %d at the default context size", got, want)
	}
}

// An unresolvable budget suppresses the report, not the charge: the current
// estimate is what admission must see next.
func TestAdoptedEngine_UnresolvableBudgetStillCharges(t *testing.T) {
	model := sizedModel("33.0 GiB")
	model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{MemoryBudget: "0"}
	phi := cumulativeISVC("phi")
	a, rec, _ := adoptAdmissionAgent(t, model, phi)
	adoptEngineRecord(t, a, phi)

	if err := a.ensureProcess(context.Background(), phi); err != nil {
		t.Fatalf("an unresolvable budget failed the reconcile: %v", err)
	}
	wantReestimatedCharge(t, a.processes["default/phi"].AdmittedBytes)
	if status, msg := schedulingStatus(t, a, "phi"); status != "" || msg != "" {
		t.Errorf("SchedulingStatus = %q / %q, want no report without a budget", status, msg)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("events = %q, want none without a budget", events)
	}
}

// An admission refused because the Model's own budget cannot be resolved still
// publishes the host budget: the gauge reports this host's limit, not the
// outcome of the check (#2535 review).
func TestAdmission_PublishesHostBudgetWhenTheModelBudgetDoesNotResolve(t *testing.T) {
	model := sizedModel("33.0 GiB")
	model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{MemoryBudget: "0"}
	isvc := cumulativeISVC("phi")
	a, _ := newCumulativeAgent(t, MetalAgentConfig{}, isvc)
	memoryBudgetBytes.Set(0)

	if err := a.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err == nil {
		t.Fatal("a Model budget that does not resolve was admitted")
	}
	if got := testutil.ToFloat64(memoryBudgetBytes); got != float64(cumulativeBudget) {
		t.Errorf("memoryBudgetBytes = %v, want the host budget %d published before the resolution failed",
			got, uint64(cumulativeBudget))
	}
}

// A Model that can neither be read nor size-probed keeps the recorded charge:
// the issue asks for the estimate the first reconcile has in hand, and an
// unreadable one must never drop the charge admission already counts.
func TestAdoptedEngine_UnreadableModelKeepsRecordedCharge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model *inferencev1alpha1.Model
	}{
		{name: "Model absent", model: nil},
		{name: "size unavailable", model: newAdmissionTestModel("https://model-host.invalid/m.gguf", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phi := cumulativeISVC("phi")
			a, rec, _ := adoptAdmissionAgent(t, tc.model, phi)
			adoptEngineRecord(t, a, phi)

			if err := a.ensureProcess(context.Background(), phi); err != nil {
				t.Fatalf("an unreadable Model failed the reconcile: %v", err)
			}
			if got := a.processes["default/phi"].AdmittedBytes; got != adoptedSeedBytes {
				t.Errorf("AdmittedBytes = %d, want the recorded %d", got, adoptedSeedBytes)
			}
			if status, msg := schedulingStatus(t, a, "phi"); status != "" || msg != "" {
				t.Errorf("SchedulingStatus = %q / %q, want no report for an unavailable estimate", status, msg)
			}
			if events := drainEvents(rec); len(events) != 0 {
				t.Errorf("events = %q, want none for an unavailable estimate", events)
			}
		})
	}
}
