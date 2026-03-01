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

	"github.com/spf13/cobra"
)

var (
	// Version is set during build via ldflags or updated by release-please
	Version = "0.4.21" // x-release-please-version
	// GitCommit is set during build
	GitCommit = "unknown"
	// BuildDate is set during build
	BuildDate = "unknown"
)

// NewVersionCommand creates the version command
func NewVersionCommand() *cobra.Command {
	var checkUpdate bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Long:  `Display version, git commit, and build date information.`,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("llmkube version %s\n", Version)
			fmt.Printf("  git commit: %s\n", GitCommit)
			fmt.Printf("  build date: %s\n", BuildDate)

			if checkUpdate {
				fmt.Println("\nChecking for updates...")
				latestVersion, err := fetchLatestVersion()
				if err != nil {
					fmt.Printf("  Unable to check for updates: %v\n", err)
					return
				}

				current := normalizeVersion(Version)
				latest := normalizeVersion(latestVersion)

				if current == latest {
					fmt.Printf("  ✅ You're running the latest version!\n")
				} else if current < latest {
					fmt.Printf("  ⚠️  New version available: %s\n", latestVersion)
					fmt.Printf("     Update with: brew upgrade llmkube\n")
					fmt.Printf("     Or download from: https://github.com/defilantech/LLMKube/releases/latest\n")
				} else {
					fmt.Printf("  ℹ️  You're running a development version\n")
				}
			}
		},
	}

	cmd.Flags().BoolVarP(&checkUpdate, "check", "c", false, "Check for updates")

	return cmd
}
