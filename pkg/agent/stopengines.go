package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// DefaultOllamaPort is the port the agent expects Ollama on when none is set.
const DefaultOllamaPort = 11434

// endpointWithdrawTimeout bounds each endpoint withdrawal, so an unreachable
// API server delays stopping the engines by at most this per service. A var
// so tests can shorten it.
var endpointWithdrawTimeout = 10 * time.Second

// StopEnginesConfig configures RunStopEngines.
type StopEnginesConfig struct {
	StateDir string
	// Namespace is only logged; every record carries its own namespace and
	// each one is stopped, whatever this says.
	Namespace string
	// K8sClient withdraws each service's endpoint before its engine stops.
	// Nil skips the withdrawal.
	K8sClient client.Client
	// OllamaPort is the Ollama daemon's port; 0 means DefaultOllamaPort.
	OllamaPort int
	Logger     *zap.SugaredLogger
}

// RunStopEngines stops every engine recorded under <stateDir>/engines without
// a running agent: it withdraws the service's endpoint (best effort), stops the
// engine, and removes the record once the engine is confirmed stopped. The
// agent must already be stopped, or its health monitor can respawn an engine.
// The returned error joins every record it could not clear.
func RunStopEngines(ctx context.Context, cfg StopEnginesConfig) error {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	cfg.Logger = logger // the Ollama executor logs through it
	home, _ := os.UserHomeDir()
	stateDir, err := resolveStateDir(cfg.StateDir, home)
	if err != nil {
		return err
	}
	inspect := newProcessInspector()
	store := newEngineRecordStore(filepath.Join(stateDir, "engines"), "", inspect, nil)
	recs, listErrs := store.List()
	logger.Infow("stopping recorded engines", "records", store.Dir(), "count", len(recs), "namespace", cfg.Namespace)
	errs := listErrs
	var registry *ServiceRegistry
	if cfg.K8sClient != nil {
		registry = NewServiceRegistry(cfg.K8sClient, "", logger, "")
	}
	for _, rec := range recs {
		key := rec.Namespace + "/" + rec.Name
		if registry != nil {
			withdrawEndpoint(ctx, registry, rec, logger)
		}
		if err := stopRecordedEngine(ctx, cfg, rec, inspect); err != nil {
			logger.Warnw("could not stop engine; keeping its record", "key", key, "pid", rec.PID, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		if err := store.Remove(rec.Namespace, rec.Name); err != nil {
			errs = append(errs, err)
			continue
		}
		logger.Infow("stopped engine", "key", key, "pid", rec.PID, "runtime", rec.Runtime)
	}
	return errors.Join(errs...)
}

// withdrawEndpoint flips the service's endpoint to not ready. It is best
// effort and bounded by endpointWithdrawTimeout: a failure is logged and the
// engine is stopped regardless.
func withdrawEndpoint(ctx context.Context, registry *ServiceRegistry, rec engineRecord, logger *zap.SugaredLogger) {
	ctx, cancel := context.WithTimeout(ctx, endpointWithdrawTimeout)
	defer cancel()
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Namespace: rec.Namespace, Name: rec.Name}}
	if _, err := registry.WithdrawEndpointIfPresent(ctx, isvc); err != nil {
		logger.Warnw("could not withdraw the endpoint", "key", rec.Namespace+"/"+rec.Name, "error", err)
	}
}

// stopRecordedEngine stops one recorded engine and returns nil only when it is
// confirmed stopped (or was already gone), so the caller may drop the record.
func stopRecordedEngine(ctx context.Context, cfg StopEnginesConfig, rec engineRecord, inspect processInspector) error {
	switch {
	case rec.Runtime == runtimeOMLX:
		// The oMLX daemon is not adopted, so it is not signalled either.
		return nil
	case rec.PID <= 0:
		if rec.Runtime != runtimeOllama {
			return nil
		}
		return unloadOllamaModel(ctx, cfg, rec.ModelID)
	case rec.PGID != rec.PID:
		return fmt.Errorf("record has pgid %d for pid %d: not a group leader, left alone", rec.PGID, rec.PID)
	}
	// stopEngineGroup signals only after the start token proves the PID is
	// the process the agent forked: a dead PID or one another program now
	// holds is already gone (record removed), while an engine whose
	// executable changed since it was recorded is still ours and is stopped.
	if err := stopEngineGroup(rec.PID, rec.PGID, rec.StartToken, inspect); !engineStopped(err) {
		return err
	}
	return nil
}

// unloadOllamaModel unloads a re-attached Ollama service's model. A daemon
// that refuses the connection is not running, so no model is loaded and the
// record can go.
func unloadOllamaModel(ctx context.Context, cfg StopEnginesConfig, modelID string) error {
	port := cfg.OllamaPort
	if port == 0 {
		port = DefaultOllamaPort
	}
	err := NewOllamaExecutor(port, cfg.Logger).UnloadModel(ctx, modelID)
	if errors.Is(err, syscall.ECONNREFUSED) {
		cfg.Logger.Infow("Ollama is not running; no model to unload", "port", port, "modelID", modelID)
		return nil
	}
	return err
}
