package agent

import (
	"context"
	"errors"
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

// adoptEngines reads the records a previous agent left and adopts engines
// that are still ours and either serving or previously finished loading.
// A running engine that misses its probe is left to the health monitor.
// Engines with a changed executable or bind host, and provisional engines
// that are not serving, are stopped as a group. Unproven identities are
// discarded without a signal; unparseable records are logged and left alone.
// It needs no API server and returns the adopted keys for the watcher.
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
	healthy := true
	switch {
	case rec.Version == legacyEngineRecordVersion:
		// An engine of an agent from before process groups is never adopted.
		return a.retireLegacy(rec)
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
	case !inRecordedGroup(rec):
		// Ours by its start token, but the kernel does not put it in the
		// group it leads on record: a group stop would miss it, and dropping
		// the record would orphan it. stopEngineGroup refuses it without a
		// signal, so the record stays for a later attempt.
		return a.stopUnwanted(rec, "not in its recorded process group")
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
		// Only a running record proves loading finished; let the monitor
		// handle a busy engine without delaying agent startup.
		if rec.State != engineRecordRunning {
			return a.stopUnwanted(rec, "not serving")
		}
		healthy = false
	}
	a.storeAdopted(rec, healthy)
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

// retireLegacy stops the engine of a version-1 record if it still runs and is
// provably the recorded process, and drops the record once it is gone. One
// that cannot be stopped keeps its record for the next attempt.
func (a *MetalAgent) retireLegacy(rec engineRecord) (outcome string, keepRecord bool) {
	if rec.PID <= 0 {
		return adoptionDiscarded, false
	}
	err := stopLegacyEngine(rec.PID, rec.StartToken, a.inspect)
	switch {
	case err == nil, engineStopped(err) && !errors.Is(err, errProcessAlreadyGone):
		// Signalled and stopped, by SIGTERM or after the grace period.
		a.logger.Infow("stopped the engine of a record from an older agent",
			"key", rec.Namespace+"/"+rec.Name, "pid", rec.PID)
		return adoptionStopped, false
	case errors.Is(err, errProcessAlreadyGone):
		return adoptionDiscarded, false
	default:
		a.logger.Warnw("failed to stop the engine of a record from an older agent; keeping its record",
			"key", rec.Namespace+"/"+rec.Name, "pid", rec.PID, "error", err)
		return adoptionStopped, true
	}
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

// inRecordedGroup reports whether the kernel puts a proven-ours engine in the
// process group its record says it leads.
func inRecordedGroup(rec engineRecord) bool {
	pgid, err := getpgidFn(rec.PID)
	return err == nil && pgid == rec.PGID
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
func (a *MetalAgent) storeAdopted(rec engineRecord, healthy bool) {
	a.mu.Lock()
	a.processes[rec.Namespace+"/"+rec.Name] = &ManagedProcess{
		Name: rec.Name, Namespace: rec.Namespace, PID: rec.PID, PGID: rec.PGID, StartToken: rec.StartToken,
		Port: rec.Port, ModelPath: rec.ModelPath, ModelID: rec.ModelID, StartedAt: rec.StartedAt, Healthy: healthy,
		SpecHash: rec.SpecHash, Priority: rec.Priority, EvictionProtection: rec.EvictionProtection,
		Runtime: rec.Runtime, AdmittedBytes: rec.AdmittedBytes, Adopted: true,
	}
	a.mu.Unlock()
	value := 0.0
	if healthy {
		value = 1
	}
	processHealthy.WithLabelValues(rec.Name, rec.Namespace).Set(value)
}
