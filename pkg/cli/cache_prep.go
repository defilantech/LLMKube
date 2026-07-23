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
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// parseOwner parses a "uid:gid" string into two int values.
// Returns an error if the format is invalid or the integers do not parse.
func parseOwner(owner string) (int, int, error) {
	parts := strings.Split(owner, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid owner format %q: expected uid:gid", owner)
	}
	if parts[0] == "" || parts[1] == "" {
		return 0, 0, fmt.Errorf("invalid owner format %q: uid and gid must not be empty", owner)
	}
	uid, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid uid in %q: %w", owner, err)
	}
	gid, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid gid in %q: %w", owner, err)
	}
	return uid, gid, nil
}

// parseMode parses an octal mode string (e.g. "0775") into an os.FileMode.
func parseMode(mode string) (os.FileMode, error) {
	v, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid octal mode %q: %w", mode, err)
	}
	return os.FileMode(v), nil
}

// NewCachePrepCommand creates the cache-prep subcommand.
func NewCachePrepCommand() *cobra.Command {
	var owner, mode string

	cmd := &cobra.Command{
		Use:   "prep DIR",
		Short: "Prepare a model cache directory for shared access",
		Long: `Prepare a model cache directory for shared access by changing its
ownership and permissions via direct syscalls.

This command is intended to be used as the entrypoint of an init container
so that it retains its effective capabilities (CAP_CHOWN/CAP_FOWNER) without
an intermediate exec. It calls os.Chown and os.Chmod directly.

Examples:
  # Change ownership to root:102 and set group rwX
  llmkube cache prep --owner 0:102 --mode 0775 /models
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]

			// Validate target directory exists and is a directory.
			info, err := os.Stat(dir)
			if err != nil {
				return fmt.Errorf("cannot stat target directory %q: %w", dir, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("target %q is not a directory", dir)
			}

			// Parse owner.
			uid, gid, err := parseOwner(owner)
			if err != nil {
				return err
			}

			// Parse mode.
			fileMode, err := parseMode(mode)
			if err != nil {
				return err
			}

			// Perform chown.
			if err := os.Chown(dir, uid, gid); err != nil {
				return fmt.Errorf("chown %q to %d:%d: %w", dir, uid, gid, err)
			}

			// Perform chmod.
			if err := os.Chmod(dir, fileMode); err != nil {
				return fmt.Errorf("chmod %q to %o: %w", dir, fileMode, err)
			}

			fmt.Printf("Prepared %q: owner %d:%d, mode %o\n", dir, uid, gid, fileMode)
			return nil
		},
	}

	cmd.Flags().StringVar(&owner, "owner", "", "Owner in uid:gid format (e.g. 0:102)")
	cmd.Flags().StringVar(&mode, "mode", "", "Octal permission mode (e.g. 0775)")
	_ = cmd.MarkFlagRequired("owner")
	_ = cmd.MarkFlagRequired("mode")

	return cmd
}
