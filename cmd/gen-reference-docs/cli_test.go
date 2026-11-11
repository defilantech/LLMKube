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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/defilantech/llmkube/pkg/cli"
)

// envTree builds a tree whose flag defaults come from the environment in the
// three ways a real flag would: read at construction ($HOME), computed before
// the generator runs (initHome, like a package-level var), and not derivable
// from any variable at all (the clock).
func envTree(initHome string) func() *cobra.Command {
	return func() *cobra.Command {
		root := &cobra.Command{Use: "tool", Short: "A tool"}
		sub := &cobra.Command{Use: "sub", Short: "A subcommand", Run: func(*cobra.Command, []string) {}}
		home, _ := os.UserHomeDir()
		sub.Flags().String("config", filepath.Join(home, ".tool", "config"), "config file")
		sub.Flags().String("cache", filepath.Join(initHome, ".cache", "tool"), "cache dir, default under "+initHome)
		sub.Flags().String("tmp", filepath.Join(os.TempDir(), "tool"), "scratch dir")
		sub.Flags().String("owner", os.Getenv("USER"), "owner")
		sub.Flags().Int64("seed", time.Now().UnixNano(), "random seed")
		sub.Flags().String("static", "unchanged", "a static default")
		sub.Flags().String("scratch", "/tmp/static", "a static path under /tmp")
		sub.Flags().String("role", "coder-runner", "a static default holding a user name")
		sub.Example = "  tool sub --prompt-file /tmp/813.md"
		root.AddCommand(sub)
		return root
	}
}

func TestRenderCLINormalizesEnvironmentDefaults(t *testing.T) {
	render := func(home, user, tmp string) string {
		t.Setenv("HOME", home)
		t.Setenv("USER", user)
		t.Setenv("TMPDIR", tmp)
		out, err := renderCLIFrom(envTree(home))
		if err != nil {
			t.Fatalf("renderCLIFrom: %v", err)
		}
		return out
	}
	// /tmp is the real temp dir on a Linux runner and "runner" its user: a
	// static default or example naming either must read the same as on a Mac.
	a := render(t.TempDir(), "runner", "/tmp")
	b := render(t.TempDir(), "bob", t.TempDir())
	if a != b {
		t.Fatalf("output depends on the environment:\n--- first\n%s\n--- second\n%s", a, b)
	}
	for _, want := range []string{
		`(default "$HOME/.tool/config")`,
		`(default "$HOME/.cache/tool")`,
		"cache dir, default under $HOME",
		`(default "$TMPDIR/tool")`,
		`(default "$USER")`,
		"(default " + envDependent + ")",
		`(default "unchanged")`,
		`(default "/tmp/static")`,
		`(default "coder-runner")`,
		"tool sub --prompt-file /tmp/813.md",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("rendered flags lack %q:\n%s", want, a)
		}
	}
}

func TestRenderCLIHiddenAndDeprecated(t *testing.T) {
	newRoot := func() *cobra.Command {
		root := &cobra.Command{Use: "tool"}
		run := func(*cobra.Command, []string) {}
		root.AddCommand(
			&cobra.Command{Use: "shown", Short: "Shown", Run: run, Aliases: []string{"sh"}},
			&cobra.Command{Use: "secret", Short: "Hidden", Run: run, Hidden: true},
			&cobra.Command{Use: "old", Short: "Old", Run: run, Deprecated: "use shown instead"},
		)
		hiddenParent := &cobra.Command{Use: "internal", Hidden: true}
		hiddenParent.AddCommand(&cobra.Command{Use: "child", Short: "Under a hidden parent", Run: run})
		root.AddCommand(hiddenParent)
		return root
	}
	out, err := renderCLIFrom(newRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## tool shown", "**Aliases:** `sh`", "## tool old", "**Deprecated:** use shown instead",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, absent := range []string{"tool secret", "tool internal", "Under a hidden parent", "## tool help"} {
		if strings.Contains(out, absent) {
			t.Errorf("hidden or help command %q documented", absent)
		}
	}
}

// The page must cover every command the binary offers, and keep the help flag
// out of the per-command listings that the global-flags section replaces.
func TestRenderCLIDocumentsEveryCommand(t *testing.T) {
	out, err := renderCLI()
	if err != nil {
		t.Fatal(err)
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden {
				continue
			}
			level := "### "
			if c == c.Root() {
				level = "## "
			}
			if !strings.Contains(out, "\n"+level+sub.CommandPath()+"\n") {
				t.Errorf("no section for %q", sub.CommandPath())
			}
			for _, f := range flagsOf(sub.LocalFlags().VisitAll) {
				if !f.Hidden && !strings.Contains(out, "--"+f.Name+" ") && !strings.Contains(out, "--"+f.Name+"\n") {
					t.Errorf("flag --%s of %q not documented", f.Name, sub.CommandPath())
				}
			}
			walk(sub)
		}
	}
	walk(cli.NewRootCommand())
	// The global-flags sentence names it once; any other occurrence is a
	// per-command help flag in a listing.
	if n := strings.Count(out, "-h, --help"); n != 1 {
		t.Errorf("-h, --help appears %d times, want only in the global-flags section", n)
	}
	if !strings.Contains(out, "## llmkube completion\n") {
		t.Error("the completion command cobra adds at Execute is missing")
	}
}

func TestReplacePathOnlyAtBoundaries(t *testing.T) {
	tests := map[string]string{
		"/home/runner":              "$HOME",
		"/home/runner/.kube":        "$HOME/.kube",
		"see /home/runner/x.":       "see $HOME/x.",
		"/home/runner2/x":           "/home/runner2/x",
		"/srv/home/runner/x":        "/srv/home/runner/x",
		"a=/home/runner,b":          "a=$HOME,b",
		"/home/runner.bak":          "/home/runner.bak",
		"/home/runner /home/runner": "$HOME $HOME",
	}
	for in, want := range tests {
		if got := replacePath(in, "/home/runner", "$HOME"); got != want {
			t.Errorf("replacePath(%q) = %q, want %q", in, got, want)
		}
	}
}
