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
	"os"

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
	namespace      string
	coderAgent     string
	integrateAgent string
	reconcileAgent string
	baseBranch     string
	dryRun         bool
}

// newSliceCommand renders a sliced Workload from a slice plan and applies it.
func newSliceCommand() *cobra.Command {
	opts := &sliceOptions{}
	cmd := &cobra.Command{
		Use:   "slice --plan FILE",
		Short: "Render a sliced Workload from a slice plan and apply it",
		Long: `Render a Workload from a slice plan (the planner's output) and apply it.

The plan decomposes one issue into disjoint-file slices. This command renders a
Workload whose pipeline is: one issue-fix step per slice, then an integrate step
that unions the slice branches, then a reconcile step that checks the union
against the plan's pinned shared identifiers.

Examples:

  # Dry-run: print the rendered Workload without applying it:
  llmkube foreman slice --plan slice-plan-700.yaml --dry-run

  # Apply against a cluster:
  llmkube foreman slice --plan slice-plan-700.yaml --coder-agent coder-metal`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSlice(cmd.Context(), cmd.OutOrStdout(), opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.planFile, "plan", "", "Path to the slice plan YAML (required)")
	f.StringVar(&opts.namespace, "namespace", "default", "Namespace to create the Workload in")
	f.StringVar(&opts.coderAgent, "coder-agent", "coder-metal", "Agent that runs each slice's issue-fix step")
	f.StringVar(&opts.integrateAgent, "integrate-agent", "integrate", "Deterministic agent that runs the integrate step")
	f.StringVar(&opts.reconcileAgent, "reconcile-agent", "reconcile", "Deterministic agent that runs the reconcile step")
	f.StringVar(&opts.baseBranch, "base-branch", "main", "Base branch the slices are cut from and unioned onto")
	f.BoolVar(&opts.dryRun, "dry-run", false, "Print the rendered Workload without applying it")
	_ = cmd.MarkFlagRequired("plan")
	return cmd
}

func runSlice(ctx context.Context, out io.Writer, opts *sliceOptions) error {
	plan, err := loadSlicePlan(opts.planFile)
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
	return nil
}

// buildSliceWorkload renders the Workload: one issue-fix step per slice, then an
// integrate step (dependsOn every slice), then a reconcile step (dependsOn
// integrate). The integration branch and each slice branch follow the
// foreman/slicer-<issue>/<name> convention.
func buildSliceWorkload(p slicePlan, opts *sliceOptions) *foremanv1alpha1.Workload {
	integBranch := fmt.Sprintf("foreman/slicer-%d/integ", p.Issue)

	steps := make([]foremanv1alpha1.PipelineStep, 0, len(p.Slices)+2)
	sliceNames := make([]string, 0, len(p.Slices))
	integSlices := make([]foremanv1alpha1.SliceRef, 0, len(p.Slices))
	reconSlices := make([]foremanv1alpha1.SliceRef, 0, len(p.Slices))

	for _, s := range p.Slices {
		branch := fmt.Sprintf("foreman/slicer-%d/%s", p.Issue, s.Name)
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
