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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

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

// admissionBudgetOptions tunes what resolving a budget reports. A fresh
// admission publishes this host's budget; a report's re-check decides again
// whether the charge fits and must not write an Event of its own (#2535).
type admissionBudgetOptions struct {
	publishHostBudget bool
	quiet             bool
}

// resolveAdmissionBudget resolves the budget an estimate is measured against.
// A Model's memoryBudget or memoryFraction may lower this host's limit for
// itself but not raise it, unless the agent allows overrides (#2080). Errors
// are the admission failure reasons, worded as checkMemoryAdmission words them.
// opts.publishHostBudget publishes the host-budget gauge before the Model's own
// budget is resolved, so a refusal for an unresolvable Model budget still
// reports this host's limit. opts.quiet suppresses the clamp notice.
func (a *MetalAgent) resolveAdmissionBudget(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	estimate MemoryEstimate,
	opts admissionBudgetOptions,
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
	if opts.publishHostBudget {
		memoryBudgetBytes.Set(float64(hostBudget))
	}

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
		if !opts.quiet {
			a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: isvc.Namespace, Name: isvc.Name},
				corev1.EventTypeWarning, EventReasonMemoryBudgetClamped,
				"Model %s budget %s (%s) exceeds this host's budget %s (%s total * %.0f%%); clamped to it. "+
					"Start the metal-agent with --allow-model-memory-budget-override to allow it",
				model.Name, formatMemory(budget.BudgetBytes), resolved.Source,
				formatMemory(serviceBudget), formatMemory(total), a.memoryFraction*100,
			)
		}
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

// adoptedChargeReportPrefix opens the report an adopted engine's re-estimated
// charge writes when it does not fit. It is how a later fit tells this report
// from any other scheduling status, so the report can be cleared (#2535).
const adoptedChargeReportPrefix = "adopted engine is serving over its memory budget: "

// adoptedCharge is the estimate and budget an adopted engine's charge was
// re-estimated with, kept on the process so a report that charge produced can
// be re-fitted without recomputing the estimate (#2535).
type adoptedCharge struct {
	estimate MemoryEstimate
	// contextSize is the context size estimate was taken at, so a re-check
	// can word the report the way the re-estimate did.
	contextSize int
	budget      *admissionBudget
	// backoff is the wait before the next re-check of a report this charge
	// wrote; it doubles to refusalRecheckMax while the cause holds.
	backoff time.Duration
}

// reestimateAdoptedCharge charges an adopted engine the estimate a fresh
// admission of its InferenceService and Model computes today, once. Adoption
// runs before the agent reaches the API server and the engine record carries no
// context size, so the first reconcile is the earliest point the current
// estimate can be computed (#2535). It reports whether it charged the engine.
func (a *MetalAgent) reestimateAdoptedCharge(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, key string, p *ManagedProcess,
) bool {
	a.mu.Lock()
	pending := p.Adopted && !p.AdmissionReestimated
	a.mu.Unlock()
	if !pending {
		return false
	}

	model, err := a.fetchModelForReconcile(ctx, isvc)
	if err != nil {
		a.logger.Warnw("keeping an adopted engine's recorded memory charge: the Model is not available",
			"key", key, "error", err)
		return false
	}
	estimate, contextSize, err := a.admissionEstimate(ctx, isvc, model)
	if err != nil {
		a.logger.Warnw("keeping an adopted engine's recorded memory charge: the model cannot be estimated",
			"key", key, "error", err)
		return false
	}

	budget, budgetErr := a.resolveAdmissionBudget(ctx, isvc, model, estimate, admissionBudgetOptions{})
	if budgetErr != nil {
		a.logger.Warnw("adopted engine's re-estimated charge cannot be checked against its budget",
			"key", key, "error", budgetErr)
	}

	a.mu.Lock()
	if a.processes[key] != p || p.AdmissionReestimated {
		a.mu.Unlock()
		return false
	}
	previous := p.AdmittedBytes
	p.AdmittedBytes = estimate.TotalBytes
	p.AdmissionReestimated = true
	var fit admissionFit
	if budget != nil {
		fit = a.fitAdmissionLocked(key, budget, estimate)
		p.adoptedCharge = &adoptedCharge{
			estimate: estimate, contextSize: contextSize, budget: budget, backoff: refusalRecheckInitial,
		}
	}
	a.updateCommittedMetricLocked()
	a.mu.Unlock()

	memoryEstimatedBytes.WithLabelValues(isvc.Name, isvc.Namespace).Set(float64(estimate.TotalBytes))
	a.logger.Infow("re-estimated an adopted engine's memory charge",
		"key", key, "previous", formatMemory(previous), "estimate", formatMemory(estimate.TotalBytes))

	if budget == nil {
		return true
	}
	if fit.fits {
		// The engine fits now, and an earlier report may still say it does not.
		a.clearAdoptedChargeReport(ctx, isvc)
		return true
	}
	a.reportAdoptedChargeOverBudget(ctx, isvc, key,
		adoptedChargeReportPrefix+fit.message+kvNotCountedNote(estimate, contextSize),
		fit.logKV...)
	return true
}

// reestimatePendingAdoptedCharges re-estimates the adopted engines whose charge
// this agent has not re-estimated yet, so no admission measures its fit against
// a charge the current estimate no longer holds (#2535). It skips an engine
// whose spec changed since its InferenceService: until the reconcile that
// replaces it stops and respawns it, nothing can compute what it runs (the
// engine record holds no context size). The watcher delivers services in list
// order, so the engine that needs it may be handled after the service being
// admitted. It does not hold a.mu: each Get is I/O.
func (a *MetalAgent) reestimatePendingAdoptedCharges(ctx context.Context) {
	type pendingCharge struct {
		process  *ManagedProcess
		specHash string
	}
	a.mu.Lock()
	pending := make([]pendingCharge, 0, len(a.processes))
	for _, p := range a.processes {
		if p.Adopted && !p.AdmissionReestimated {
			pending = append(pending, pendingCharge{process: p, specHash: p.SpecHash})
		}
	}
	a.mu.Unlock()
	for _, c := range pending {
		p := c.process
		namespaced := types.NamespacedName{Namespace: p.Namespace, Name: p.Name}
		isvc := &inferencev1alpha1.InferenceService{}
		if err := a.config.K8sClient.Get(ctx, namespaced, isvc); err != nil {
			a.logger.Warnw("keeping an adopted engine's recorded memory charge: its InferenceService is not available",
				"key", namespaced.String(), "error", err)
			continue
		}
		// The reconcile that replaces this engine stops and respawns it;
		// until then the recorded charge is what the engine holds.
		if computeSpecHash(isvc) != c.specHash {
			continue
		}
		a.reestimateAdoptedCharge(ctx, isvc, namespaced.String(), p)
	}
}

// recheckAdoptedChargeReport re-checks an adopted engine's re-estimated charge
// when a report it wrote may have outlived its cause: another service's charge,
// or the Model's own budget, may have changed since (#2535). Each check reads
// the Model and re-resolves the budget the way admission does, keeping the
// estimate stored at the re-estimate. A check that fits clears the report. One
// that still fails keeps it, rewrites the message when the numbers changed, and
// arms the next check on the same backoff a start refusal uses, starting at
// refusalRecheckInitial and doubling to refusalRecheckMax. The report's own
// publishRefusal arms the first check; this arms the ones after it.
func (a *MetalAgent) recheckAdoptedChargeReport(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, key string, p *ManagedProcess,
) {
	if !hasAdoptedChargeReport(isvc) {
		return
	}
	a.mu.Lock()
	charge := p.adoptedCharge
	a.mu.Unlock()
	if charge == nil {
		return
	}

	// A Model that has gone away, or a budget that will not resolve, keeps the
	// report on the numbers that wrote it and tries again next time.
	budget, err := a.recheckAdoptedBudget(ctx, isvc, charge.estimate)
	if err != nil {
		a.logger.Warnw("keeping an adopted engine's memory report: its budget cannot be re-resolved",
			"key", key, "error", err)
	}

	a.mu.Lock()
	if budget != nil {
		charge.budget = budget
	}
	fit := a.fitAdmissionLocked(key, charge.budget, charge.estimate)
	charge.backoff = min(2*charge.backoff, refusalRecheckMax)
	wait := charge.backoff
	msg := adoptedChargeReportPrefix + fit.message + kvNotCountedNote(charge.estimate, charge.contextSize)
	a.mu.Unlock()

	if fit.fits {
		if a.clearAdoptedChargeReport(ctx, isvc) {
			return
		}
	} else {
		a.rewriteAdoptedChargeReport(ctx, isvc, msg)
	}
	a.requeueAt(key, a.clock().Add(wait))
}

// recheckAdoptedBudget reads the Model and resolves the budget it sets today
// with the function admission uses, so a host-wide or cgroup limit that moved
// is picked up too. It resolves quietly: the re-check writes no Event.
func (a *MetalAgent) recheckAdoptedBudget(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, estimate MemoryEstimate,
) (*admissionBudget, error) {
	model, err := a.fetchModelForReconcile(ctx, isvc)
	if err != nil {
		return nil, err
	}
	return a.resolveAdmissionBudget(ctx, isvc, model, estimate, admissionBudgetOptions{quiet: true})
}

// rewriteAdoptedChargeReport updates a standing report's message when a
// re-check resolved a budget that words it differently, so the status quotes
// the numbers the Model has now. It emits no Event and leaves any other status
// alone.
func (a *MetalAgent) rewriteAdoptedChargeReport(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, msg string,
) {
	namespaced := types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}
	fresh := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(ctx, namespaced, fresh); err != nil {
		a.logger.Warnw("failed to fetch InferenceService to update an adopted engine's memory report",
			"key", namespaced.String(), "error", err)
		return
	}
	if !hasAdoptedChargeReport(fresh) || fresh.Status.SchedulingMessage == msg {
		return
	}
	fresh.Status.SchedulingMessage = msg
	if err := a.config.K8sClient.Status().Update(ctx, fresh); err != nil {
		a.logger.Warnw("failed to update an adopted engine's memory report",
			"key", namespaced.String(), "error", err)
	}
}

// hasAdoptedChargeReport reports whether isvc carries the report an adopted
// engine's re-estimated charge writes.
func hasAdoptedChargeReport(isvc *inferencev1alpha1.InferenceService) bool {
	return isvc.Status.SchedulingStatus == EventReasonInsufficientMemory &&
		strings.HasPrefix(isvc.Status.SchedulingMessage, adoptedChargeReportPrefix)
}

// clearAdoptedChargeReport clears that report once a fit passes: its cause is
// gone. It reads the object fresh because the controller may have written since
// the list, and leaves any other status alone. It reports whether no report of
// ours is left on the object.
func (a *MetalAgent) clearAdoptedChargeReport(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService,
) bool {
	if !hasAdoptedChargeReport(isvc) {
		return true
	}
	namespaced := types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}
	fresh := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(ctx, namespaced, fresh); err != nil {
		a.logger.Warnw("failed to fetch InferenceService to clear an adopted engine's memory report",
			"key", namespaced.String(), "error", err)
		return false
	}
	if !hasAdoptedChargeReport(fresh) {
		return true
	}
	fresh.Status.SchedulingStatus = ""
	fresh.Status.SchedulingMessage = ""
	if err := a.config.K8sClient.Status().Update(ctx, fresh); err != nil {
		a.logger.Warnw("failed to clear an adopted engine's memory report",
			"key", namespaced.String(), "error", err)
		return false
	}
	return true
}

// reportAdoptedChargeOverBudget reports a serving engine whose re-estimated
// charge does not fit, through the status and Warning event admission refusals
// use. The text says serving over budget, not refused: the engine keeps
// running. It writes a freshly read object: the copy the watcher listed may be
// behind the controller's own write, which the status update would refuse.
func (a *MetalAgent) reportAdoptedChargeOverBudget(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, key, msg string, logKV ...interface{},
) {
	a.logger.Warnw("adopted engine is serving over its memory budget",
		append([]interface{}{"key", key, "reason", msg}, logKV...)...)
	namespaced := types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}
	fresh := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(ctx, namespaced, fresh); err != nil {
		a.logger.Warnw("failed to fetch InferenceService to report an adopted engine over budget",
			"key", key, "error", err)
		return
	}
	prev := statusBeforeRefusal(fresh)
	fresh.Status.SchedulingStatus = EventReasonInsufficientMemory
	fresh.Status.SchedulingMessage = msg
	if a.publishRefusal(ctx, fresh, prev) {
		a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: fresh.Namespace, Name: fresh.Name},
			corev1.EventTypeWarning, EventReasonInsufficientMemory, "%s", msg)
	}
}
