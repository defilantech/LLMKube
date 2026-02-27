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
	"github.com/spf13/cobra"
)

// NewRootCommand creates the root command for llmkube CLI
func NewRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "llmkube",
		Short: "Kubernetes for Local LLMs",
		Long: `LLMKube: Treating intelligence as a workload.
Scale, secure, and observe local LLMs like microservices.

Deploy and manage local LLM inference services on Kubernetes with
built-in observability, SLO enforcement, and edge-native capabilities.`,
		SilenceUsage: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Check for updates on every command (uses cache to avoid slowdown)
			CheckForUpdate()
		},
	}

	// Add subcommands
	cmd.AddCommand(NewDeployCommand())
	cmd.AddCommand(NewListCommand())
	cmd.AddCommand(NewDeleteCommand())
	cmd.AddCommand(NewStatusCommand())
	cmd.AddCommand(NewQueueCommand())
	cmd.AddCommand(NewVersionCommand())
	cmd.AddCommand(NewCatalogCommand())
	cmd.AddCommand(NewBenchmarkCommand())
	cmd.AddCommand(NewCacheCommand())
	cmd.AddCommand(NewInspectCommand())
	cmd.AddCommand(NewLicenseCommand())

	return cmd
}
