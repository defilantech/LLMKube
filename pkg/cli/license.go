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
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/license"
)

func NewLicenseCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "license",
		Short: "View license information for models",
		Long: `View license and compliance information for LLM models.

Examples:
  # Check license for a deployed model
  llmkube license check my-model

  # Check license in a specific namespace
  llmkube license check my-model -n my-namespace

  # List all known licenses
  llmkube license list
`,
	}

	cmd.AddCommand(NewLicenseCheckCommand())
	cmd.AddCommand(NewLicenseListCommand())

	return cmd
}

func NewLicenseCheckCommand() *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "check MODEL_NAME",
		Short: "Check license details for a deployed model",
		Long: `Display detailed license and compliance information for a deployed model.

License information is extracted from the GGUF file metadata when the model
is downloaded by the controller. The model must be in Ready state.

Examples:
  llmkube license check my-llama-model
  llmkube license check my-model -n production
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLicenseCheck(args[0], namespace)
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")

	return cmd
}

func NewLicenseListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all known LLM licenses",
		Long: `Display a summary of all known LLM licenses in the database.

Examples:
  llmkube license list
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLicenseList()
		},
	}

	return cmd
}

func runLicenseCheck(modelName, namespace string) error {
	ctx := context.Background()

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	model := &inferencev1alpha1.Model{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, model); err != nil {
		return fmt.Errorf("failed to get Model '%s' in namespace '%s': %w", modelName, namespace, err)
	}

	if model.Status.GGUF == nil || model.Status.GGUF.License == "" {
		if model.Status.Phase != "Ready" && model.Status.Phase != "Cached" {
			fmt.Printf("Model '%s' is still %s — license metadata is not yet available.\n",
				modelName, strings.ToLower(model.Status.Phase))
			return nil
		}
		fmt.Printf("Model '%s' has no license information in its GGUF metadata.\n", modelName)
		return nil
	}

	licenseID := model.Status.GGUF.License
	lic := license.Get(licenseID)
	if lic == nil {
		fmt.Printf("Model:       %s\n", modelName)
		fmt.Printf("License ID:  %s\n", licenseID)
		fmt.Printf("Status:      Unknown license (not in database)\n")
		return nil
	}

	fmt.Printf("\nLicense Details for '%s'\n", modelName)
	fmt.Printf("═══════════════════════════════════════════════════════════════════════\n\n")
	fmt.Printf("License:         %s\n", lic.Name)
	fmt.Printf("SPDX ID:         %s\n", lic.ID)
	fmt.Printf("Commercial Use:  %s\n", formatBool(lic.CommercialUse))
	fmt.Printf("Attribution:     %s\n", formatBool(lic.Attribution))
	fmt.Printf("Redistribution:  %s\n", formatBool(lic.Redistribution))

	if len(lic.Restrictions) > 0 {
		fmt.Printf("\nRestrictions:\n")
		for _, r := range lic.Restrictions {
			fmt.Printf("  - %s\n", r)
		}
	} else {
		fmt.Printf("\nNo special restrictions.\n")
	}

	fmt.Printf("\nFull License: %s\n\n", lic.URL)

	return nil
}

func runLicenseList() error {
	licenses := license.All()

	fmt.Printf("\nKnown LLM Licenses\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════════════\n\n")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tCOMMERCIAL\tRESTRICTIONS")
	_, _ = fmt.Fprintln(w, "──\t────\t──────────\t────────────")

	for _, lic := range licenses {
		restrictions := "None"
		if len(lic.Restrictions) > 0 {
			restrictions = strings.Join(lic.Restrictions, "; ")
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			lic.ID,
			lic.Name,
			formatBool(lic.CommercialUse),
			restrictions,
		)
	}

	_ = w.Flush()
	fmt.Printf("\n")

	return nil
}

func formatBool(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}
