/*
Copyright 2025.

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

package cli

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/yaml"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
)

// Labels stamped on every task a single dispatch invocation creates, so the
// batch can be queried, watched, and cleaned up as a unit.
const (
	dispatchRunLabel   = "foreman.llmkube.dev/dispatch-run"
	dispatchIssueLabel = "foreman.llmkube.dev/issue"
	dispatchAgentLabel = "foreman.llmkube.dev/agent"
)

// Best-effort writers for progress/UI output: the destination is the command's
// stdout (or a test buffer), where a write error is neither actionable nor worth
// threading through every call site.
func fprintf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
func fprintln(w io.Writer, a ...any)               { _, _ = fmt.Fprintln(w, a...) }
func fprint(w io.Writer, a ...any)                 { _, _ = fmt.Fprint(w, a...) }

// dispatchOptions holds the resolved flags for one `foreman dispatch` run.
type dispatchOptions struct {
	repo         string
	agents       []string
	namespace    string
	timeoutSecs  int32
	baseBranch   string
	branchPrefix string
	promptFiles  []string
	noWait       bool
	pollInterval time.Duration
	dryRun       bool
}

// taskAssignment pairs an issue with the coder Agent that will run it.
type taskAssignment struct {
	Issue int
	Agent string
}

// taskResult is the terminal outcome of one dispatched task, used for the
// summary table and the exit code.
type taskResult struct {
	Issue    int
	Agent    string
	Node     string
	Phase    foremanv1alpha1.AgenticTaskPhase
	Verdict  foremanv1alpha1.AgenticTaskVerdict
	Branch   string
	Reason   foremanv1alpha1.AgenticTaskFailureReason
	Duration time.Duration
}

// NewForemanCommand is the `llmkube foreman` group: operate the Foreman
// agentic-coding harness from the CLI.
func NewForemanCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "foreman",
		Short: "Operate the Foreman agentic-coding harness",
		Long: `Operate the Foreman agentic-coding harness.

Foreman runs coder and reviewer agents as AgenticTasks against a fleet of
heterogeneous executors. These subcommands create and watch those tasks.`,
	}
	cmd.AddCommand(newDispatchCommand())
	return cmd
}

func newDispatchCommand() *cobra.Command {
	opts := &dispatchOptions{}
	cmd := &cobra.Command{
		Use:   "dispatch ISSUE [ISSUE...]",
		Short: "Fan a batch of issues across coder agents and watch them run",
		Long: `Create one AgenticTask per issue, round-robined across the named coder
Agents, then watch them all to completion and print a summary.

Round-robining across Agents bound to different boxes is what runs the batch
concurrently: each coder Agent is model- and box-specific, so naming two
Agents spreads the issues across both. Each issue's prompt is fetched from
GitHub (title + body) unless overridden with --prompt-file.

Examples:
  # Two issues, one per box, watched to completion:
  llmkube foreman dispatch --repo defilantech/LLMKube \
    --agents gateway-coder-amd,coder-metal 813 854

  # Override one issue's prompt; create and detach:
  llmkube foreman dispatch --repo defilantech/LLMKube --agents coder-metal \
    --prompt-file 813=/tmp/813.md --no-wait 813 854`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDispatch(cmd.Context(), cmd.OutOrStdout(), args, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.repo, "repo", "", "Target repository as owner/repo (required)")
	f.StringSliceVar(&opts.agents, "agents", nil, "Comma-separated coder Agent names to round-robin across (required)")
	f.StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace to create the AgenticTasks in")
	f.Int32Var(&opts.timeoutSecs, "timeout", 1800, "Per-task timeout in seconds")
	f.StringVar(&opts.baseBranch, "base-branch", "", "Base branch the coder branches from (payload.baseBranch)")
	f.StringVar(&opts.branchPrefix, "branch-prefix", "", "Prefix for the coder's working branch (payload.branchPrefix)")
	f.StringArrayVar(&opts.promptFiles, "prompt-file", nil, "Override an issue's prompt: ISSUE=PATH (repeatable)")
	f.BoolVar(&opts.noWait, "no-wait", false, "Create the tasks and exit without watching")
	f.DurationVar(&opts.pollInterval, "poll-interval", 5*time.Second, "How often to poll task status while watching")
	f.BoolVar(&opts.dryRun, "dry-run", false, "Print the AgenticTasks that would be created and exit")
	return cmd
}

// runDispatch wires the live dependencies (kube client + GitHub fetcher) and
// executes the dispatch. The testable seams (assignment, task build, watch,
// summary) are exercised directly in unit tests.
func runDispatch(ctx context.Context, out io.Writer, args []string, opts *dispatchOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	owner, repo, err := githubissue.ParseRepo(opts.repo)
	if err != nil {
		return fmt.Errorf("--repo: %w", err)
	}
	if len(opts.agents) == 0 {
		return fmt.Errorf("--agents is required (one or more coder Agent names)")
	}
	issues, err := parseIssues(args)
	if err != nil {
		return err
	}
	overrides, err := parsePromptOverrides(opts.promptFiles)
	if err != nil {
		return err
	}

	// Resolve each issue's prompt: an explicit override wins; otherwise fetch
	// the title + body from GitHub.
	fetcher := githubissue.NewClient()
	token := os.Getenv("GITHUB_TOKEN")
	prompts := make(map[int]string, len(issues))
	for _, n := range issues {
		if p, ok := overrides[n]; ok {
			prompts[n] = p
			continue
		}
		iss, ferr := fetcher.Fetch(ctx, owner, repo, n, token)
		if ferr != nil {
			return fmt.Errorf(
				"fetch issue #%d from %s/%s: %w (set GITHUB_TOKEN or pass --prompt-file %d=<path>)",
				n, owner, repo, ferr, n)
		}
		prompts[n] = formatIssuePrompt(iss)
	}

	runID := newRunID()
	assignments := assignAgents(issues, opts.agents)
	tasks := make([]*foremanv1alpha1.AgenticTask, 0, len(assignments))
	for _, a := range assignments {
		tasks = append(tasks, buildTask(opts.namespace, runID, opts.repo, a, prompts[a.Issue], opts))
	}

	if opts.dryRun {
		return printTasksYAML(out, tasks)
	}

	c, err := newForemanClient()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if err := c.Create(ctx, t); err != nil {
			return fmt.Errorf("create AgenticTask %s: %w", t.Name, err)
		}
		fprintf(out, "created %s (issue #%d -> agent %s)\n", t.Name, t.Spec.Payload.Issue, t.Spec.AgentRef.Name)
	}

	if opts.noWait {
		fprintf(out, "\n%d task(s) created with label %s=%s. Watch with:\n  kubectl -n %s get agentictasks -l %s=%s\n",
			len(tasks), dispatchRunLabel, runID, opts.namespace, dispatchRunLabel, runID)
		return nil
	}

	fprintf(out, "\nwatching %d task(s) (Ctrl-C to stop watching; tasks keep running)\n", len(tasks))
	results, werr := watchTasks(ctx, c, out, opts.namespace, tasks, opts.pollInterval)
	renderSummary(out, results)
	if werr != nil {
		// Interrupted: report what we have, do not mask as success.
		fprintf(out, "\nstopped watching: %v\n  resume: kubectl -n %s get agentictasks -l %s=%s\n",
			werr, opts.namespace, dispatchRunLabel, runID)
		return werr
	}
	return exitErrFromResults(results)
}

// --- pure, unit-tested helpers ---

// parseIssues converts CLI args to positive issue numbers, preserving order and
// dropping duplicates.
func parseIssues(args []string) ([]int, error) {
	seen := map[int]bool{}
	out := make([]int, 0, len(args))
	for _, a := range args {
		n, err := strconv.Atoi(strings.TrimSpace(a))
		if err != nil || n <= 0 || n > math.MaxInt32 {
			return nil, fmt.Errorf("invalid issue number %q: must be a positive integer", a)
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no issues given")
	}
	return out, nil
}

// assignAgents round-robins issues across agents in order.
func assignAgents(issues []int, agents []string) []taskAssignment {
	out := make([]taskAssignment, len(issues))
	for i, iss := range issues {
		out[i] = taskAssignment{Issue: iss, Agent: agents[i%len(agents)]}
	}
	return out
}

// parsePromptOverrides reads each ISSUE=PATH spec into a map of issue ->
// file contents.
func parsePromptOverrides(specs []string) (map[int]string, error) {
	out := map[int]string{}
	for _, s := range specs {
		key, path, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("--prompt-file %q: expected ISSUE=PATH", s)
		}
		n, err := strconv.Atoi(strings.TrimSpace(key))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("--prompt-file %q: %q is not a valid issue number", s, key)
		}
		b, err := os.ReadFile(strings.TrimSpace(path))
		if err != nil {
			return nil, fmt.Errorf("--prompt-file %q: %w", s, err)
		}
		out[n] = string(b)
	}
	return out, nil
}

// formatIssuePrompt renders a fetched issue into the prompt body the coder
// triages: the title as a heading followed by the issue body.
func formatIssuePrompt(iss *githubissue.Issue) string {
	body := strings.TrimSpace(iss.Body)
	if body == "" {
		body = "(no description provided)"
	}
	return fmt.Sprintf("# %s\n\n%s\n", strings.TrimSpace(iss.Title), body)
}

// taskName is the deterministic AgenticTask name for an issue within a run.
func taskName(issue int, runID string) string {
	return fmt.Sprintf("dispatch-%d-%s", issue, runID)
}

// buildTask constructs the AgenticTask for one assignment.
func buildTask(
	namespace, runID, repo string, a taskAssignment, prompt string, opts *dispatchOptions,
) *foremanv1alpha1.AgenticTask {
	return &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName(a.Issue, runID),
			Namespace: namespace,
			Labels: map[string]string{
				dispatchRunLabel:   runID,
				dispatchIssueLabel: strconv.Itoa(a.Issue),
				dispatchAgentLabel: a.Agent,
			},
		},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind:           foremanv1alpha1.AgenticTaskKindIssueFix,
			AgentRef:       &corev1.LocalObjectReference{Name: a.Agent},
			TimeoutSeconds: opts.timeoutSecs,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:         repo,
				Issue:        int32(a.Issue), //nolint:gosec // G115: issue numbers are positive and bounded by parseIssues
				Prompt:       prompt,
				BaseBranch:   opts.baseBranch,
				BranchPrefix: opts.branchPrefix,
				Agent:        a.Agent,
			},
		},
	}
}

// isTerminalPhase reports whether a task has reached a terminal state.
func isTerminalPhase(p foremanv1alpha1.AgenticTaskPhase) bool {
	return p == foremanv1alpha1.AgenticTaskPhaseSucceeded || p == foremanv1alpha1.AgenticTaskPhaseFailed
}

// taskSucceeded reports whether a terminal task is a clean success: it reached
// Succeeded with a GO or GATE-PASS verdict. Anything else (Failed, NO-GO,
// INCOMPLETE, GATE-FAIL, GATE-ERROR) is a failure for exit-code purposes.
func taskSucceeded(t *foremanv1alpha1.AgenticTask) bool {
	if t.Status.Phase != foremanv1alpha1.AgenticTaskPhaseSucceeded {
		return false
	}
	switch t.Status.Verdict {
	case foremanv1alpha1.AgenticTaskVerdictGo, foremanv1alpha1.AgenticTaskVerdictGatePass:
		return true
	default:
		return false
	}
}

// resultFromTask snapshots a task's terminal status into a taskResult.
func resultFromTask(t *foremanv1alpha1.AgenticTask) taskResult {
	r := taskResult{
		Issue:   int(t.Spec.Payload.Issue),
		Node:    t.Status.AssignedNode,
		Phase:   t.Status.Phase,
		Verdict: t.Status.Verdict,
		Branch:  t.Status.Branch,
		Reason:  t.Status.FailureReason,
	}
	if t.Spec.AgentRef != nil {
		r.Agent = t.Spec.AgentRef.Name
	}
	if t.Status.StartedAt != nil && t.Status.FinishedAt != nil {
		r.Duration = t.Status.FinishedAt.Sub(t.Status.StartedAt.Time)
	}
	return r
}

// exitErrFromResults returns a non-nil error when any task did not cleanly
// succeed, so the command exits non-zero and is usable in scripts.
func exitErrFromResults(results []taskResult) error {
	bad := make([]string, 0, len(results))
	for _, r := range results {
		ok := r.Phase == foremanv1alpha1.AgenticTaskPhaseSucceeded &&
			(r.Verdict == foremanv1alpha1.AgenticTaskVerdictGo || r.Verdict == foremanv1alpha1.AgenticTaskVerdictGatePass)
		if !ok {
			bad = append(bad, fmt.Sprintf("#%d", r.Issue))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%d of %d task(s) did not succeed: %s", len(bad), len(results), strings.Join(bad, " "))
	}
	return nil
}

// renderSummary writes the aggregate table, sorted by issue number.
func renderSummary(out io.Writer, results []taskResult) {
	sorted := append([]taskResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Issue < sorted[j].Issue })

	fprintln(out, "\nSummary:")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fprintln(w, "ISSUE\tAGENT\tNODE\tPHASE\tVERDICT\tBRANCH\tDURATION")
	for _, r := range sorted {
		verdict := string(r.Verdict)
		if verdict == "" {
			verdict = "-"
		}
		if r.Reason != "" {
			verdict = fmt.Sprintf("%s (%s)", verdict, r.Reason)
		}
		branch := r.Branch
		if branch == "" {
			branch = "-"
		}
		dur := "-"
		if r.Duration > 0 {
			dur = r.Duration.Round(time.Second).String()
		}
		node := r.Node
		if node == "" {
			node = "-"
		}
		fprintf(w, "#%d\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Issue, r.Agent, node, r.Phase, verdict, branch, dur)
	}
	_ = w.Flush()
}

// --- live wiring ---

func newRunID() string {
	// time-based, sortable, unique per invocation; runtime use of time.Now is
	// fine here (this is the CLI, not a replayable workflow).
	return time.Now().Format("20060102-150405")
}

// newForemanClient builds a controller-runtime client with the foreman scheme
// registered, using the ambient kubeconfig.
func newForemanClient() (client.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}
	if err := foremanv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return nil, fmt.Errorf("failed to register foreman scheme: %w", err)
	}
	return client.New(cfg, client.Options{Scheme: scheme.Scheme})
}

// watchTasks polls the given tasks until all reach a terminal phase or the
// context is cancelled. It prints a one-line progress update each poll and
// returns the terminal results. A non-nil error means the watch was
// interrupted (ctx cancelled); the returned results are then partial.
func watchTasks(
	ctx context.Context, c client.Client, out io.Writer, namespace string,
	tasks []*foremanv1alpha1.AgenticTask, poll time.Duration,
) ([]taskResult, error) {
	if poll <= 0 {
		poll = 5 * time.Second
	}
	names := make([]string, len(tasks))
	for i, t := range tasks {
		names[i] = t.Name
	}
	for {
		fetched := make([]*foremanv1alpha1.AgenticTask, 0, len(names))
		done := 0
		phases := map[string]int{}
		for _, name := range names {
			var t foremanv1alpha1.AgenticTask
			if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &t); err != nil {
				return nil, fmt.Errorf("get task %s: %w", name, err)
			}
			tc := t
			fetched = append(fetched, &tc)
			phase := string(t.Status.Phase)
			if phase == "" {
				phase = string(foremanv1alpha1.AgenticTaskPhasePending)
			}
			phases[phase]++
			if isTerminalPhase(t.Status.Phase) {
				done++
			}
		}
		fprintf(out, "\r%-80s", fmt.Sprintf("  %d/%d done  %s", done, len(names), formatPhaseCounts(phases)))

		if done == len(names) {
			fprintln(out)
			results := make([]taskResult, 0, len(fetched))
			for _, t := range fetched {
				results = append(results, resultFromTask(t))
			}
			return results, nil
		}

		select {
		case <-ctx.Done():
			fprintln(out)
			results := make([]taskResult, 0, len(fetched))
			for _, t := range fetched {
				results = append(results, resultFromTask(t))
			}
			return results, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// formatPhaseCounts renders the per-phase tally deterministically.
func formatPhaseCounts(phases map[string]int) string {
	keys := make([]string, 0, len(phases))
	for k := range phases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, phases[k]))
	}
	return strings.Join(parts, " ")
}

// printTasksYAML renders the tasks as a multi-document YAML stream for --dry-run.
func printTasksYAML(out io.Writer, tasks []*foremanv1alpha1.AgenticTask) error {
	for i, t := range tasks {
		t.APIVersion = foremanv1alpha1.GroupVersion.String()
		t.Kind = "AgenticTask"
		b, err := yaml.Marshal(t)
		if err != nil {
			return fmt.Errorf("marshal task %s: %w", t.Name, err)
		}
		if i > 0 {
			fprintln(out, "---")
		}
		fprint(out, string(b))
	}
	return nil
}
