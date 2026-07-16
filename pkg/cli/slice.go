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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sigsyaml "sigs.k8s.io/yaml"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"gopkg.in/yaml.v3"
)

// slicePlan mirrors the planner's YAML output (snake_case, matching the slicer
// experiment's PLANNER_PROMPT). It is the input to `llmkube foreman slice`.
type slicePlan struct {
	Issue             int32          `yaml:"issue"`
	Repo              string         `yaml:"repo"`
	Contract          string         `yaml:"contract"`
	SharedIdentifiers []planSharedID `yaml:"shared_identifiers"`
	Slices            []planSlice    `yaml:"slices"`
}

type planSharedID struct {
	ID           string   `yaml:"id"`
	DefinedBy    string   `yaml:"defined_by"`
	ReferencedBy []string `yaml:"referenced_by"`
}

type planSlice struct {
	Name  string   `yaml:"name"`
	Files []string `yaml:"files"`
	Task  string   `yaml:"task"`
}

type sliceOptions struct {
	planFile       string
	repo           string
	plannerURL     string
	plannerModel   string
	plannerToken   string
	repomapFile    string
	namespace      string
	coderAgent     string
	integrateAgent string
	reconcileAgent string
	verifyAgent    string
	baseBranch     string
	dryRun         bool
}

// newSliceCommand renders a sliced Workload from a slice plan and applies it.
func newSliceCommand() *cobra.Command {
	opts := &sliceOptions{}
	cmd := &cobra.Command{
		Use:   "slice [ISSUE]",
		Short: "Plan an issue into disjoint slices and render a sliced Workload",
		Long: `Render a sliced Workload and apply it, from either a pre-made plan or by
planning an issue.

The pipeline is one issue-fix step per disjoint slice, then an integrate step
that unions the slice branches, then a reconcile step that checks the union
against the plan's pinned shared identifiers, then a verify step that runs the
clean-room build+envtest gate on the integrated branch (skip with
--verify-agent "" for non-code slicing).

Two input modes:
  --plan FILE          render from a slice plan the planner already produced.
  ISSUE --planner-url  plan the issue with a model, then render.

Examples:

  # Render a pre-made plan, dry-run:
  llmkube foreman slice --plan slice-plan-700.yaml --dry-run

  # Plan issue 700 with a local model, then apply:
  llmkube foreman slice 700 --repo defilantech/LLMKube \
    --planner-url http://localhost:18080 --planner-model ornith-35b \
    --repomap /tmp/repomap.txt --coder-agent coder-metal`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSlice(cmd.Context(), cmd.OutOrStdout(), args, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.planFile, "plan", "", "Path to a slice plan YAML (skips planning)")
	f.StringVar(&opts.repo, "repo", "", "Target repo as owner/name (required when planning an issue)")
	f.StringVar(&opts.plannerURL, "planner-url", "",
		"OpenAI-compatible base URL of the planner model (required when planning an issue)")
	f.StringVar(&opts.plannerModel, "planner-model", "", "Planner model name to send in the request")
	f.StringVar(&opts.plannerToken, "planner-token", "",
		"Bearer token sent to --planner-url; falls back to PLANNER_TOKEN env. Omit for direct InferenceService")
	f.StringVar(&opts.repomapFile, "repomap", "", "Path to a repository map file to give the planner")
	f.StringVar(&opts.namespace, "namespace", "default", "Namespace to create the Workload in")
	f.StringVar(&opts.coderAgent, "coder-agent", "coder-metal", "Agent that runs each slice's issue-fix step")
	f.StringVar(&opts.integrateAgent, "integrate-agent", "integrate", "Deterministic agent that runs the integrate step")
	f.StringVar(&opts.reconcileAgent, "reconcile-agent", "reconcile", "Deterministic agent that runs the reconcile step")
	f.StringVar(&opts.verifyAgent, "verify-agent", "gate",
		"Deterministic gate Agent that build+envtest-verifies the integrated branch after reconcile "+
			"(must exist in the target namespace); empty string skips verify for non-code slicing")
	f.StringVar(&opts.baseBranch, "base-branch", "main", "Base branch the slices are cut from and unioned onto")
	f.BoolVar(&opts.dryRun, "dry-run", false, "Print the rendered Workload without applying it")
	return cmd
}

func runSlice(ctx context.Context, out io.Writer, args []string, opts *sliceOptions) error {
	plan, err := resolveSlicePlan(ctx, args, opts)
	if err != nil {
		return err
	}
	if err := validateSlicePlan(plan); err != nil {
		return err
	}
	wl := buildSliceWorkload(plan, opts)

	if opts.dryRun {
		b, err := sigsyaml.Marshal(wl)
		if err != nil {
			return fmt.Errorf("marshal workload: %w", err)
		}
		_, err = out.Write(b)
		return err
	}

	c, err := newForemanClient()
	if err != nil {
		return err
	}
	if err := c.Create(ctx, wl); err != nil {
		return fmt.Errorf("create workload %s/%s: %w", wl.Namespace, wl.Name, err)
	}
	_, _ = fmt.Fprintf(out, "created Workload %s/%s (%d slices)\n", wl.Namespace, wl.Name, len(plan.Slices))
	return nil
}

// resolveSlicePlan produces the plan from either --plan FILE or by planning an
// ISSUE argument with the planner model.
func resolveSlicePlan(ctx context.Context, args []string, opts *sliceOptions) (slicePlan, error) {
	if opts.planFile != "" {
		return loadSlicePlan(opts.planFile)
	}
	if len(args) == 1 {
		issue, err := strconv.ParseInt(args[0], 10, 32)
		if err != nil || issue <= 0 {
			return slicePlan{}, fmt.Errorf("ISSUE must be a positive integer, got %q", args[0])
		}
		if opts.repo == "" || opts.plannerURL == "" {
			return slicePlan{}, fmt.Errorf("planning an issue requires --repo and --planner-url")
		}
		if opts.plannerToken == "" {
			opts.plannerToken = os.Getenv("PLANNER_TOKEN")
		}
		return planIssue(ctx, int32(issue), opts, httpPlannerCall(opts.plannerURL, opts.plannerModel, opts.plannerToken))
	}
	return slicePlan{}, fmt.Errorf("provide --plan FILE or an ISSUE with --repo and --planner-url")
}

func loadSlicePlan(path string) (slicePlan, error) {
	var p slicePlan
	if path == "" {
		return p, fmt.Errorf("--plan is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return p, fmt.Errorf("read plan %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("parse plan %s: %w", path, err)
	}
	return p, nil
}

// validateSlicePlan enforces the invariants the render relies on: an issue, a
// repo, at least one slice, and DISJOINT files (no file owned by two slices).
// It also rejects a pinned identifier that ends with a trailing underscore,
// because the reconcile check matches pins as whole tokens and a trailing
// underscore can never be a token boundary, so such a pin can never be
// satisfied. Catching this at plan-validation time surfaces a clear planner
// error instead of a misleading pinned-missing GATE-FAIL later. (The broader
// prefix-pin case, which needs the file content the reconcile step has, is
// tracked in a follow-up.)
func validateSlicePlan(p slicePlan) error {
	if p.Issue <= 0 {
		return fmt.Errorf("plan has no issue number")
	}
	if p.Repo == "" {
		return fmt.Errorf("plan has no repo")
	}
	if len(p.Slices) == 0 {
		return fmt.Errorf("plan has no slices")
	}
	owner := map[string]string{}
	for _, s := range p.Slices {
		if s.Name == "" {
			return fmt.Errorf("a slice has no name")
		}
		for _, f := range s.Files {
			if prev, ok := owner[f]; ok && prev != s.Name {
				return fmt.Errorf("file %q is owned by both slices %q and %q (slices must be disjoint)", f, prev, s.Name)
			}
			owner[f] = s.Name
		}
	}
	for _, id := range p.SharedIdentifiers {
		if id.ID == "" {
			return fmt.Errorf("a shared identifier has no id")
		}
		if len(id.ID) > 0 && id.ID[len(id.ID)-1] == '_' {
			return fmt.Errorf(
				"shared identifier %q ends with a trailing underscore; "+
					"the reconcile check matches whole tokens and a trailing "+
					"underscore can never be a token boundary, so this pin can "+
					"never be satisfied",
				id.ID,
			)
		}
	}
	return nil
}

// buildSliceWorkload renders the Workload: one issue-fix step per slice, then an
// integrate step (dependsOn every slice), then a reconcile step (dependsOn
// integrate), then (unless --verify-agent is empty) a verify step (dependsOn
// reconcile) that build+envtest-gates the integrated branch (#1137). The
// integration branch and each slice branch follow the
// foreman/slicer-<issue>-<runid>/<name> convention, where <runid> is a
// per-Workload hex segment that makes re-runs of the same issue never collide
// with branches left on the fork by an earlier run.
func buildSliceWorkload(p slicePlan, opts *sliceOptions) *foremanv1alpha1.Workload {
	runID := sliceRunID()
	integBranch := fmt.Sprintf("foreman/slicer-%d-%s/integ", p.Issue, runID)

	steps := make([]foremanv1alpha1.PipelineStep, 0, len(p.Slices)+3)
	sliceNames := make([]string, 0, len(p.Slices))
	integSlices := make([]foremanv1alpha1.SliceRef, 0, len(p.Slices))
	reconSlices := make([]foremanv1alpha1.SliceRef, 0, len(p.Slices))

	for _, s := range p.Slices {
		branch := fmt.Sprintf("foreman/slicer-%d-%s/%s", p.Issue, runID, s.Name)
		steps = append(steps, foremanv1alpha1.PipelineStep{
			Name:     s.Name,
			Kind:     foremanv1alpha1.AgenticTaskKindIssueFix,
			AgentRef: corev1.LocalObjectReference{Name: opts.coderAgent},
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:       p.Repo,
				Issue:      p.Issue,
				Branch:     branch,
				BaseBranch: opts.baseBranch,
				Prompt:     buildSlicePrompt(p, s),
			},
		})
		sliceNames = append(sliceNames, s.Name)
		integSlices = append(integSlices, foremanv1alpha1.SliceRef{Name: s.Name, Branch: branch, Files: s.Files})
		reconSlices = append(reconSlices, foremanv1alpha1.SliceRef{Name: s.Name, Files: s.Files})
	}

	steps = append(steps, foremanv1alpha1.PipelineStep{
		Name:     "integrate",
		Kind:     foremanv1alpha1.AgenticTaskKindIntegrate,
		AgentRef: corev1.LocalObjectReference{Name: opts.integrateAgent},
		Payload: foremanv1alpha1.AgenticTaskPayload{
			Repo:       p.Repo,
			Branch:     integBranch,
			BaseBranch: opts.baseBranch,
			Slices:     integSlices,
		},
		DependsOn: sliceNames,
	})

	ids := make([]foremanv1alpha1.SharedIdentifier, 0, len(p.SharedIdentifiers))
	for _, si := range p.SharedIdentifiers {
		ids = append(ids, foremanv1alpha1.SharedIdentifier{
			ID:           si.ID,
			DefinedBy:    si.DefinedBy,
			ReferencedBy: si.ReferencedBy,
		})
	}
	steps = append(steps, foremanv1alpha1.PipelineStep{
		Name:     "reconcile",
		Kind:     foremanv1alpha1.AgenticTaskKindReconcile,
		AgentRef: corev1.LocalObjectReference{Name: opts.reconcileAgent},
		Payload: foremanv1alpha1.AgenticTaskPayload{
			Repo:              p.Repo,
			Branch:            integBranch,
			Slices:            reconSlices,
			SharedIdentifiers: ids,
			Contract:          p.Contract,
		},
		DependsOn: []string{"integrate"},
	})

	// verify step: a clean-room build + envtest gate on the integrated branch,
	// so a sliced Workload cannot reach terminal PASS while the merged union
	// fails to build or fails envtest (#1137). integrate (git union) and
	// reconcile (static pinned-identifier check) validate the merge and the
	// interface, but neither compiles the code; per-slice envtest is no
	// substitute because coupled slices cannot build in isolation, so the
	// union is the only meaningful thing to test. dependsOn reconcile so it
	// runs only after the merge and pins check out. An empty --verify-agent
	// opts out (non-code slicing has nothing to build).
	if opts.verifyAgent != "" {
		steps = append(steps, foremanv1alpha1.PipelineStep{
			Name:     "verify",
			Kind:     foremanv1alpha1.AgenticTaskKindVerify,
			AgentRef: corev1.LocalObjectReference{Name: opts.verifyAgent},
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:       p.Repo,
				Issue:      p.Issue,
				Branch:     integBranch,
				BaseBranch: opts.baseBranch,
			},
			DependsOn: []string{"reconcile"},
		})
	}

	return &foremanv1alpha1.Workload{
		TypeMeta: metav1.TypeMeta{
			APIVersion: foremanv1alpha1.GroupVersion.String(),
			Kind:       "Workload",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("slicer-%d", p.Issue),
			Namespace: opts.namespace,
		},
		Spec: foremanv1alpha1.WorkloadSpec{
			Intent:   fmt.Sprintf("slicer: issue #%d in %d slices", p.Issue, len(p.Slices)),
			Repo:     p.Repo,
			Pipeline: steps,
		},
	}
}

// buildSlicePrompt assembles one slice's coder prompt: the file scope, the
// shared contract, and the slice's scoped task.
func buildSlicePrompt(p slicePlan, s planSlice) string {
	files := ""
	for _, f := range s.Files {
		files += "  - " + f + "\n"
	}
	return fmt.Sprintf(
		"You are implementing ONE SLICE of issue #%d. Touch ONLY these files "+
			"(create or edit); do not touch any other file:\n%s\n"+
			"Shared contract (all slices agree on this):\n%s\n\n"+
			"Your slice:\n%s\n\n"+
			"When your slice's files are done and verified once, submit_result GO.",
		p.Issue, files, p.Contract, s.Task,
	)
}

// sliceRunID returns an 8-char hex segment unique per invocation. It scopes
// slicer branches so re-runs of the same issue never collide with branches
// left on the fork by an earlier run. Stable across slices of one Workload so
// integrate/reconcile resolve the same refs.
func sliceRunID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is essentially impossible; surface it loudly
		// rather than silently degrading to a collision-prone fallback.
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b[:])
}
