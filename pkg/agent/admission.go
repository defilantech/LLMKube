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

// Memory admission's estimate, budget resolution and fit check live here so the
// first reconcile after adoption can reuse them instead of modelling an
// engine's memory a second way (#2535).

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// admissionContextSize is the --ctx-size admission estimates the KV cache for:
// the InferenceService's contextSize, or the agent's default.
func admissionContextSize(isvc *inferencev1alpha1.InferenceService) int {
	if isvc.Spec.ContextSize != nil && *isvc.Spec.ContextSize > 0 {
		return int(*isvc.Spec.ContextSize)
	}
	return defaultContextSize
}

// admissionEstimate estimates an InferenceService and Model with the same
// inputs a fresh admission derives from the spec, and returns the context size
// the estimate used.
func (a *MetalAgent) admissionEstimate(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model,
) (MemoryEstimate, int, error) {
	contextSize := admissionContextSize(isvc)
	cacheTypeK, cacheTypeV := resolveCacheTypes(isvc)
	estimate, err := a.estimateModelMemory(ctx, model, contextSize, cacheTypeK, cacheTypeV,
		derefInt32(isvc.Spec.ParallelSlots))
	if err != nil {
		return MemoryEstimate{}, contextSize, err
	}
	return estimate, contextSize, nil
}

// admissionBudget is the budget an estimate is measured against, resolved from
// the Model and this host exactly as a fresh admission resolves it.
type admissionBudget struct {
	hostWide      bool
	total         uint64
	service       uint64
	host          uint64
	agentFraction float64
	resolved      ResolvedBudget
	budget        *MemoryBudget
	clamped       bool
}

// resolveAdmissionBudget resolves the budget an estimate is measured against.
// A Model's memoryBudget or memoryFraction may lower this host's limit for
// itself but not raise it, unless the agent allows overrides (#2080). Errors
// are the admission failure reasons, worded as checkMemoryAdmission words them.
func (a *MetalAgent) resolveAdmissionBudget(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	estimate MemoryEstimate,
) (*admissionBudget, error) {
	// This host's budget, the limit its owner set with --memory-fraction. Every
	// admitted model shares it, and a Model budget may not exceed it. An
	// engine outside the agent's cgroup is measured against the host-wide
	// budget, every other engine against the cgroup's.
	hostWide := runsOutsideAgentCgroup(a.resolveRuntime(isvc))
	provider := a.memoryProvider
	if hostWide {
		provider, _ = hostWideProvider(provider)
	}
	agentTotal, hostTotal, totalErr := a.memoryTotals()
	if totalErr != nil {
		return nil, fmt.Errorf("memory budget check failed: failed to get total memory: %v", totalErr)
	}
	total := agentTotal
	if hostWide {
		total = hostTotal
	}
	serviceBudget := uint64(float64(total) * a.memoryFraction)
	hostBudget := uint64(float64(hostTotal) * a.memoryFraction)

	resolved, resolveErr := ResolveMemoryBudget(model.Spec.Hardware, a.memoryFraction)
	if resolveErr != nil {
		return nil, fmt.Errorf("memory budget resolution failed: %v", resolveErr)
	}

	var budget *MemoryBudget
	switch resolved.Mode {
	case BudgetModeAbsolute:
		budget = CheckMemoryBudgetAbsolute(resolved.Bytes, estimate)
	default: // BudgetModeFraction
		var budgetErr error
		budget, budgetErr = CheckMemoryBudget(provider, estimate, resolved.Fraction)
		if budgetErr != nil {
			return nil, fmt.Errorf("memory budget check failed: %v", budgetErr)
		}
	}

	clamped := false
	if budget.BudgetBytes > serviceBudget && !a.config.AllowModelMemoryBudgetOverride {
		a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: isvc.Namespace, Name: isvc.Name},
			corev1.EventTypeWarning, EventReasonMemoryBudgetClamped,
			"Model %s budget %s (%s) exceeds this host's budget %s (%s total * %.0f%%); clamped to it. "+
				"Start the metal-agent with --allow-model-memory-budget-override to allow it",
			model.Name, formatMemory(budget.BudgetBytes), resolved.Source,
			formatMemory(serviceBudget), formatMemory(total), a.memoryFraction*100,
		)
		budget = CheckMemoryBudgetAbsolute(serviceBudget, estimate)
		clamped = true
	}

	return &admissionBudget{
		hostWide:      hostWide,
		total:         total,
		service:       serviceBudget,
		host:          hostBudget,
		agentFraction: a.memoryFraction,
		resolved:      resolved,
		budget:        budget,
		clamped:       clamped,
	}, nil
}

// shortfallMessage words why an estimate does not fit its own budget.
func (b *admissionBudget) shortfallMessage() string {
	switch {
	case b.clamped:
		return fmt.Sprintf("estimated %s required, budget %s (the Model's budget, clamped to this host's: %s total * %.0f%%)",
			formatMemory(b.budget.EstimateBytes),
			formatMemory(b.budget.BudgetBytes),
			formatMemory(b.total),
			b.agentFraction*100,
		)
	case b.resolved.Mode == BudgetModeAbsolute:
		return fmt.Sprintf("estimated %s required, budget %s (absolute from CRD)",
			formatMemory(b.budget.EstimateBytes),
			formatMemory(b.budget.BudgetBytes),
		)
	default:
		return fmt.Sprintf("estimated %s required, budget %s (%s total * %.0f%%)",
			formatMemory(b.budget.EstimateBytes),
			formatMemory(b.budget.BudgetBytes),
			formatMemory(b.budget.TotalBytes),
			b.resolved.Fraction*100,
		)
	}
}

// admissionFit is the outcome of the fit check: whether the estimate fits on
// its own and next to committed memory, the refusal message when it does not,
// the log fields that go with that message, and the committed total the pass
// log reports.
type admissionFit struct {
	fits      bool
	message   string
	logKV     []interface{}
	committed uint64
}

// fitAdmissionLocked reports whether estimate fits on its own and next to every
// running and starting process. Caller holds a.mu: the fit and the reservation
// it guards share one critical section. key is excluded from committed memory
// because its own charge is being replaced.
func (a *MetalAgent) fitAdmissionLocked(key string, b *admissionBudget, estimate MemoryEstimate) admissionFit {
	if !b.budget.Fits {
		return admissionFit{message: b.shortfallMessage(), logKV: []interface{}{"source", b.resolved.Source}}
	}

	// The model fits on its own; it must also fit next to every running and
	// starting process. A respawn is checked against everything but its own
	// key: its previous engine is already stopped. An override-raised Model
	// budget raises the shared limit for this admission too. Every service
	// counts against the host-wide budget; a service in the agent's cgroup
	// must also fit next to the other in-cgroup services under the cgroup's
	// budget.
	committed, holders := a.committedMemoryLocked(key, false)
	if msg, ok := memoryRefusal(estimate.TotalBytes, committed, max(b.host, b.budget.BudgetBytes), holders); !ok {
		return admissionFit{message: msg, logKV: []interface{}{"committed", formatMemory(committed)}}
	}
	if !b.hostWide {
		inCgroup, inCgroupHolders := a.committedMemoryLocked(key, true)
		capacity := max(b.service, b.budget.BudgetBytes)
		if msg, ok := memoryRefusal(estimate.TotalBytes, inCgroup, capacity, inCgroupHolders); !ok {
			return admissionFit{message: msg, logKV: []interface{}{"committed", formatMemory(inCgroup)}}
		}
	}
	return admissionFit{fits: true, committed: committed}
}

// reestimateAdoptedCharge charges an adopted engine the estimate a fresh
// admission of its InferenceService and Model computes today, once. Adoption
// runs before the agent reaches the API server and the engine record carries no
// context size, so the first reconcile is the earliest point the current
// estimate can be computed (#2535).
func (a *MetalAgent) reestimateAdoptedCharge(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, key string, p *ManagedProcess,
) {
	a.mu.Lock()
	pending := p.Adopted && !p.AdmissionReestimated
	a.mu.Unlock()
	if !pending {
		return
	}

	model, err := a.fetchModelForReconcile(ctx, isvc)
	if err != nil {
		a.logger.Warnw("keeping an adopted engine's recorded memory charge: the Model is not available",
			"key", key, "error", err)
		return
	}
	estimate, contextSize, err := a.admissionEstimate(ctx, isvc, model)
	if err != nil {
		a.logger.Warnw("keeping an adopted engine's recorded memory charge: the model cannot be estimated",
			"key", key, "error", err)
		return
	}

	budget, budgetErr := a.resolveAdmissionBudget(ctx, isvc, model, estimate)
	if budgetErr != nil {
		a.logger.Warnw("adopted engine's re-estimated charge cannot be checked against its budget",
			"key", key, "error", budgetErr)
	}

	a.mu.Lock()
	if a.processes[key] != p || p.AdmissionReestimated {
		a.mu.Unlock()
		return
	}
	previous := p.AdmittedBytes
	p.AdmittedBytes = estimate.TotalBytes
	p.AdmissionReestimated = true
	a.updateCommittedMetricLocked()
	var fit admissionFit
	if budget != nil {
		fit = a.fitAdmissionLocked(key, budget, estimate)
	}
	a.mu.Unlock()

	memoryEstimatedBytes.WithLabelValues(isvc.Name, isvc.Namespace).Set(float64(estimate.TotalBytes))
	a.logger.Infow("re-estimated an adopted engine's memory charge",
		"key", key, "previous", formatMemory(previous), "estimate", formatMemory(estimate.TotalBytes))

	if budget != nil && !fit.fits {
		a.reportAdoptedChargeOverBudget(ctx, isvc, key,
			"adopted engine is serving over its memory budget: "+fit.message+kvNotCountedNote(estimate, contextSize),
			fit.logKV...)
	}
}

// reportAdoptedChargeOverBudget reports a serving engine whose re-estimated
// charge does not fit, through the status and Warning event admission refusals
// use. The text says serving over budget, not refused: the engine keeps
// running.
func (a *MetalAgent) reportAdoptedChargeOverBudget(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, key, msg string, logKV ...interface{},
) {
	a.logger.Warnw("adopted engine is serving over its memory budget",
		append([]interface{}{"key", key, "reason", msg}, logKV...)...)
	prev := statusBeforeRefusal(isvc)
	isvc.Status.SchedulingStatus = EventReasonInsufficientMemory
	isvc.Status.SchedulingMessage = msg
	if a.publishRefusal(ctx, isvc, prev) {
		a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: isvc.Namespace, Name: isvc.Name},
			corev1.EventTypeWarning, EventReasonInsufficientMemory, "%s", msg)
	}
}
