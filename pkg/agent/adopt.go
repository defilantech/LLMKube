package agent

import (
	"context"
)

// Outcomes of evaluating one engine record at startup, the result label of
// engineAdoptionsTotal.
const (
	adoptionAdopted   = "adopted"
	adoptionStopped   = "stopped"
	adoptionDiscarded = "discarded"
	// adoptionInterrupted is not counted: the context ended before the record
	// could be judged, so it is left as found.
	adoptionInterrupted = ""
)

// startAdoption adopts the engines a previous agent process left running and
// seeds the watcher with their keys, so its first poll stops an adopted engine
// whose service this agent no longer watches. Start calls it before the
// watcher runs.
func (a *MetalAgent) startAdoption(ctx context.Context) map[string]bool {
	adopted := a.adoptEngines(ctx)
	if a.watcher != nil {
		a.watcher.SeedSeen(adopted)
	}
	return adopted
}

// adoptEngines reads the engine records a previous agent process left and
// adopts each engine that is still ours and serving, so it keeps serving
// instead of being reloaded (#2079). It needs no API server. An engine that is
// ours (its start token matches) but cannot be adopted, because its executable
// changed, it is not serving or it is bound for another mode, is stopped as a
// group; a record whose process is not provably ours is discarded without a
// signal. Records it cannot parse are logged and left alone. It returns the adopted keys (namespace/name),
// which the startup endpoint withdrawal skips.
func (a *MetalAgent) adoptEngines(ctx context.Context) map[string]bool {
	adopted := map[string]bool{}
	recs, errs := a.engineRecords.List()
	for _, err := range errs {
		a.logger.Warnw("ignoring engine record", "error", err)
	}
	for _, rec := range recs {
		key := rec.Namespace + "/" + rec.Name
		outcome, keepRecord := a.adoptOne(ctx, rec)
		if outcome == adoptionInterrupted {
			// The agent is stopping: leave this and every later record for
			// the next agent process.
			a.logger.Infow("engine adoption interrupted; leaving the remaining records", "key", key)
			break
		}
		engineAdoptionsTotal.WithLabelValues(outcome).Inc()
		if outcome == adoptionAdopted {
			adopted[key] = true
			a.logger.Infow("adopted engine from a previous agent process",
				"key", key, "pid", rec.PID, "port", rec.Port, "runtime", rec.Runtime)
			continue
		}
		if !keepRecord {
			if err := a.engineRecords.Remove(rec.Namespace, rec.Name); err != nil {
				a.logger.Warnw("failed to remove engine record", "key", key, "error", err)
			}
		}
		a.logger.Infow("did not adopt engine", "key", key, "pid", rec.PID, "outcome", outcome)
	}
	a.mu.Lock()
	a.adopted = adopted
	managedProcesses.Set(float64(len(a.processes)))
	a.updateCommittedMetricLocked()
	a.mu.Unlock()
	return adopted
}

// adoptOne applies the adoption decision table to one record and, for an
// adopted engine, stores its ManagedProcess. keepRecord is true for an engine
// it tried and failed to stop, which may still be running, and for a record
// the context ended before it could judge: a health probe cut short proves
// nothing, so it never stops an engine.
func (a *MetalAgent) adoptOne(ctx context.Context, rec engineRecord) (outcome string, keepRecord bool) {
	if ctx.Err() != nil {
		return adoptionInterrupted, true
	}
	switch {
	case rec.Runtime == runtimeOMLX:
		// The oMLX daemon is not adopted; its services reload.
		return adoptionDiscarded, false
	case rec.PID <= 0:
		// Only an external daemon (Ollama) has no PID: re-attach while it serves.
		if rec.Runtime != runtimeOllama {
			return adoptionDiscarded, false
		}
		if !a.serving(ctx, rec) {
			if ctx.Err() != nil {
				return adoptionInterrupted, true
			}
			return adoptionDiscarded, false
		}
	case !a.provenOurs(rec):
		return adoptionDiscarded, false
	case !a.runsRecordedExecutable(rec):
		// The start token proves the process is the one the agent forked, but
		// it now runs something else than recorded (a wrapper that exec'd the
		// engine, an interpreter flag): keeping it serving is not safe,
		// forgetting it would orphan it.
		return a.stopUnwanted(rec, "executable changed")
	case rec.BindHost != resolveBindHost(a.engineBindHost()):
		// Ours but bound for another agent mode: a legacy engine listens on
		// every interface without the relay's authentication, and a loopback
		// engine is unreachable for a legacy-mode agent. No bind host
		// recorded counts as another mode.
		return a.stopUnwanted(rec, "bound for another agent mode")
	case !a.serving(ctx, rec):
		if ctx.Err() != nil {
			return adoptionInterrupted, true
		}
		// Ours but not serving: crashed mid-load, hung, or still loading.
		return a.stopUnwanted(rec, "not serving")
	}
	a.storeAdopted(rec)
	return adoptionAdopted, false
}

// stopUnwanted stops an engine that is ours but not adopted, as a group. Its
// record goes once the engine is confirmed stopped; one that may still run
// keeps its record for a later stop.
func (a *MetalAgent) stopUnwanted(rec engineRecord, why string) (outcome string, keepRecord bool) {
	err := stopEngineGroup(rec.PID, rec.PGID, rec.StartToken, a.inspect)
	if err != nil && !engineStopped(err) {
		a.logger.Warnw("failed to stop an engine the agent does not adopt; keeping its record",
			"key", rec.Namespace+"/"+rec.Name, "pid", rec.PID, "reason", why, "error", err)
		return adoptionStopped, true
	}
	a.logger.Infow("stopped an engine the agent does not adopt",
		"key", rec.Namespace+"/"+rec.Name, "pid", rec.PID, "reason", why)
	return adoptionStopped, false
}

// provenOurs reports whether the record's PID is alive and is still the
// process the agent forked: same kernel start token (exec does not change it)
// and leader of the recorded process group. Every engine the agent spawns
// leads its own group, so a record without one is malformed (and
// stopEngineGroup would refuse it anyway). Any lookup error counts as not
// ours. It sends only the existence probe.
func (a *MetalAgent) provenOurs(rec engineRecord) bool {
	if rec.PGID != rec.PID {
		return false
	}
	if killFn(rec.PID, 0) != nil {
		return false
	}
	tok, err := a.inspect.StartToken(rec.PID)
	return err == nil && rec.StartToken != "" && tok == rec.StartToken
}

// runsRecordedExecutable reports whether a proven-ours engine still runs the
// program it was recorded with, under either recorded path. Adoption requires
// it; a failed lookup counts as a mismatch.
func (a *MetalAgent) runsRecordedExecutable(rec engineRecord) bool {
	exes, err := a.inspect.Executables(rec.PID)
	return err == nil && executableMatches(exes, rec.executablePaths()...)
}

// serving probes the runtime's health endpoint on the recorded port, as the
// health monitor does.
func (a *MetalAgent) serving(ctx context.Context, rec engineRecord) bool {
	ok, err := a.healthChecker.Check(ctx, rec.Port, healthProbePath(rec.Runtime))
	return err == nil && ok
}

// storeAdopted inserts the ManagedProcess of an adopted engine with every
// recorded field.
func (a *MetalAgent) storeAdopted(rec engineRecord) {
	a.mu.Lock()
	a.processes[rec.Namespace+"/"+rec.Name] = &ManagedProcess{
		Name: rec.Name, Namespace: rec.Namespace, PID: rec.PID, PGID: rec.PGID, StartToken: rec.StartToken,
		Port: rec.Port, ModelPath: rec.ModelPath, ModelID: rec.ModelID, StartedAt: rec.StartedAt, Healthy: true,
		SpecHash: rec.SpecHash, Priority: rec.Priority, EvictionProtection: rec.EvictionProtection,
		Runtime: rec.Runtime, AdmittedBytes: rec.AdmittedBytes, Adopted: true,
	}
	a.mu.Unlock()
	processHealthy.WithLabelValues(rec.Name, rec.Namespace).Set(1)
}
