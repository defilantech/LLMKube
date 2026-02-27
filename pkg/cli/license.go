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
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/defilantech/llmkube/pkg/license"
)

func NewLicenseCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "license",
		Short: "View license information for models",
		Long: `View license and compliance information for LLM models.

Examples:
  # Check license for a specific catalog model
  llmkube license check llama-3.1-8b

  # List all known licenses
  llmkube license list
`,
	}

	cmd.AddCommand(NewLicenseCheckCommand())
	cmd.AddCommand(NewLicenseListCommand())

	return cmd
}

func NewLicenseCheckCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check MODEL_ID",
		Short: "Check license details for a catalog model",
		Long: `Display detailed license and compliance information for a model in the catalog.

Examples:
  llmkube license check llama-3.1-8b
  llmkube license check mistral-7b
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLicenseCheck(args[0])
		},
	}

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

func runLicenseCheck(modelID string) error {
	model, err := GetModel(modelID)
	if err != nil {
		return err
	}

	if model.License == "" {
		fmt.Printf("Model '%s' has no license information.\n", modelID)
		return nil
	}

	lic := license.Get(model.License)
	if lic == nil {
		fmt.Printf("Model:       %s\n", modelID)
		fmt.Printf("License ID:  %s\n", model.License)
		fmt.Printf("Status:      Unknown license (not in database)\n")
		return nil
	}

	fmt.Printf("\nLicense Details for '%s'\n", modelID)
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
