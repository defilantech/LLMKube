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
	"fmt"
	"os"
	"os/user"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/defilantech/llmkube/pkg/cli"
)

// envDependent replaces a flag default that still differs between the two
// pinned environments after normalization: it came from the machine, not the
// source, and printing either value would make the page differ per host.
const envDependent = "<depends on the environment>"

// pinnedEnv is one synthetic environment the command tree is built under.
// Every variable a flag default plausibly derives from is set to a value no
// real machine has, so the page reads the same on every host and the
// synthetic value can be swapped back for a placeholder.
type pinnedEnv struct {
	home, user, tmp string
}

var cliEnvs = [2]pinnedEnv{
	{home: "/llmkube-docs-a/home", user: "llmkube-docs-user-a", tmp: "/llmkube-docs-a/tmp"},
	{home: "/llmkube-docs-b/home", user: "llmkube-docs-user-b", tmp: "/llmkube-docs-b/tmp"},
}

// unsetForCLI are variables cleared while the tree is built: each names a
// path that would otherwise override a home-derived default.
var unsetForCLI = []string{
	"KUBECONFIG", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
	"XDG_RUNTIME_DIR", "LLMKUBE_CACHE_IMAGE",
}

// renderCLI documents the llmkube command tree.
func renderCLI() (string, error) {
	return renderCLIFrom(cli.NewRootCommand)
}

// renderCLIFrom documents the tree newRoot builds. The tree is built twice
// under two synthetic environments; a default that differs between the builds
// after the synthetic values are replaced is printed as envDependent.
func renderCLIFrom(newRoot func() *cobra.Command) (string, error) {
	norm := newHostNormalizer()
	var roots [2]*cobra.Command
	for i, env := range cliEnvs {
		root, err := buildTreeUnder(env, newRoot)
		if err != nil {
			return "", err
		}
		roots[i] = root
	}
	if err := reconcileDefaults(roots[0], roots[1], norm); err != nil {
		return "", err
	}
	return renderCommandTree(roots[0], norm), nil
}

// buildTreeUnder builds the tree with env pinned and restores the process
// environment afterwards.
func buildTreeUnder(env pinnedEnv, newRoot func() *cobra.Command) (*cobra.Command, error) {
	set := map[string]string{
		"HOME": env.home, "USERPROFILE": env.home, "USER": env.user, "LOGNAME": env.user, "TMPDIR": env.tmp,
	}
	restore, err := setEnv(set, unsetForCLI)
	if err != nil {
		return nil, err
	}
	defer restore()

	root := newRoot()
	// Execute adds these; the generator never executes, so add them the same
	// way to document the tree a user's binary actually has.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	walkCommands(root, func(c *cobra.Command) { c.InitDefaultHelpFlag() })
	return root, nil
}

func setEnv(set map[string]string, unset []string) (func(), error) {
	type saved struct {
		val string
		ok  bool
	}
	prev := map[string]saved{}
	for k := range set {
		v, ok := os.LookupEnv(k)
		prev[k] = saved{v, ok}
	}
	for _, k := range unset {
		v, ok := os.LookupEnv(k)
		prev[k] = saved{v, ok}
	}
	restore := func() {
		for k, s := range prev {
			if s.ok {
				_ = os.Setenv(k, s.val)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}
	for k, v := range set {
		if err := os.Setenv(k, v); err != nil {
			restore()
			return nil, fmt.Errorf("pin %s: %w", k, err)
		}
	}
	for _, k := range unset {
		if err := os.Unsetenv(k); err != nil {
			restore()
			return nil, fmt.Errorf("unset %s: %w", k, err)
		}
	}
	return restore, nil
}

// hostNormalizer swaps machine-specific strings for placeholders.
//
// The synthetic values buildTreeUnder pins are unique, so they are replaced
// anywhere. The machine's own values are not: a default computed at package
// init, before anything is pinned, holds the real home directory, and os/user
// reads the password database rather than $HOME. The real home is replaced as
// the leading path of a default, and in text only at path boundaries and when
// it is at least two components deep, since "/root" is a prefix of other
// paths. The real temp directory is not replaced at all: on Linux it is /tmp,
// which help text names literally, and replacing it would make the page
// differ between a Mac and CI. The host and user names are replaced only as a
// whole default, since a user name such as "runner" can be part of a static
// one.
type hostNormalizer struct {
	synthetic []pathSub
	realHomes []string
	exactDefs map[string]string
}

type pathSub struct{ from, to string }

func newHostNormalizer() *hostNormalizer {
	n := &hostNormalizer{exactDefs: map[string]string{}}
	for _, env := range cliEnvs {
		n.synthetic = append(n.synthetic, pathSub{env.home, "$HOME"}, pathSub{env.tmp, "$TMPDIR"})
		n.exactDefs[env.user] = "$USER"
	}
	addHome := func(h string) {
		h = strings.TrimRight(h, "/")
		if h != "" && !slices.Contains(n.realHomes, h) {
			n.realHomes = append(n.realHomes, h)
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		addHome(h)
	}
	if u, err := user.Current(); err == nil {
		addHome(u.HomeDir)
		n.exactDefs[u.Username] = "$USER"
	}
	if h, err := os.Hostname(); err == nil && h != "" && h != "localhost" {
		n.exactDefs[h] = "<hostname>"
	}
	// Longest first, so a home nested in another keeps its own name.
	sort.SliceStable(n.realHomes, func(i, j int) bool { return len(n.realHomes[i]) > len(n.realHomes[j]) })
	return n
}

func (n *hostNormalizer) text(s string) string {
	for _, p := range n.synthetic {
		s = strings.ReplaceAll(s, p.from, p.to)
	}
	for _, h := range n.realHomes {
		if strings.Count(h, "/") >= 2 {
			s = replacePath(s, h, "$HOME")
		}
	}
	return s
}

func (n *hostNormalizer) defValue(s string) string {
	if to, ok := n.exactDefs[s]; ok {
		return to
	}
	s = n.text(s)
	for _, h := range n.realHomes {
		if s == h || strings.HasPrefix(s, h+"/") {
			return "$HOME" + s[len(h):]
		}
	}
	return s
}

// replacePath replaces path in s where it stands as a whole path: not
// preceded by a path character and followed by a separator or a non-path
// character, so /home/runner does not match inside /home/runner2.
func replacePath(s, path, to string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, path)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(path)
		before := i == 0 || !isPathChar(s[i-1])
		after := end == len(s) || s[end] == '/' || !isPathChar(s[end])
		b.WriteString(s[:i])
		if before && after {
			b.WriteString(to)
		} else {
			b.WriteString(path)
		}
		s = s[end:]
	}
}

func isPathChar(c byte) bool {
	return isAlnum(c) || c == '/' || c == '.' || c == '_' || c == '-'
}

// reconcileDefaults normalizes every flag default in a and replaces the ones
// that still disagree with b's. The two trees come from the same constructor,
// so they have the same shape; a mismatch means the constructor itself is
// nondeterministic, which the page cannot document.
func reconcileDefaults(a, b *cobra.Command, norm *hostNormalizer) error {
	if a.CommandPath() != b.CommandPath() || len(a.Commands()) != len(b.Commands()) {
		return fmt.Errorf("command tree differs between builds at %q", a.CommandPath())
	}
	// LocalFlags visits each flag once, at the command that declares it.
	for _, fa := range flagsOf(a.LocalFlags().VisitAll) {
		fb := b.LocalFlags().Lookup(fa.Name)
		if fb == nil {
			return fmt.Errorf("flag --%s of %q missing from the second build", fa.Name, a.CommandPath())
		}
		da, db := norm.defValue(fa.DefValue), norm.defValue(fb.DefValue)
		if da != db {
			da = envDependent
		}
		fa.DefValue = da
		fa.Usage = norm.text(fa.Usage)
	}
	for i, ca := range a.Commands() {
		if err := reconcileDefaults(ca, b.Commands()[i], norm); err != nil {
			return err
		}
	}
	return nil
}

// flagsOf lists the flags a FlagSet's VisitAll visits. Inferring the element
// type from the method value keeps spf13/pflag an indirect dependency.
func flagsOf[F any](visitAll func(func(F))) []F {
	var out []F
	visitAll(func(f F) { out = append(out, f) })
	return out
}

func walkCommands(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range c.Commands() {
		walkCommands(sub, fn)
	}
}

// documented reports whether a command gets a section: hidden commands and
// cobra's own help command are left out; deprecated ones stay, marked.
func documented(c *cobra.Command) bool {
	if c.Hidden || c.Name() == "help" {
		return false
	}
	return c.Runnable() || len(documentedSubcommands(c)) > 0
}

func renderCommandTree(root *cobra.Command, norm *hostNormalizer) string {
	var b strings.Builder
	b.WriteString(pageHeader("CLI reference",
		"Every llmkube command, with its usage, examples and flags.",
		"the command tree in "+sourceLink("pkg/cli/root.go")))

	b.WriteString("## Commands\n\n")
	b.WriteString(tableHeader("Command", "Description"))
	for _, c := range root.Commands() {
		if !documented(c) {
			continue
		}
		path := c.CommandPath()
		b.WriteString(tableRow(fmt.Sprintf("[`%s`](#%s)", path, anchor(path)), tableCell(norm.text(c.Short))))
	}
	b.WriteString("\n## Global flags\n\n")
	b.WriteString("Every command accepts `-h, --help`, which prints its help and exits.")
	if globals := strings.TrimRight(root.PersistentFlags().FlagUsagesWrapped(0), "\n"); globals != "" {
		b.WriteString(" Every command also accepts these flags:\n\n")
		b.WriteString(codeBlock("text", globals))
	} else {
		b.WriteString(" The root command defines no other flags that its subcommands inherit.\n\n")
	}

	walkCommands(root, func(c *cobra.Command) {
		if c == root || !documented(c) || hiddenAncestor(c) {
			return
		}
		renderCommand(&b, c, root, norm)
	})
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func hiddenAncestor(c *cobra.Command) bool {
	for p := c.Parent(); p != nil; p = p.Parent() {
		if p.Hidden {
			return true
		}
	}
	return false
}

func renderCommand(b *strings.Builder, c, root *cobra.Command, norm *hostNormalizer) {
	level := "###"
	if c.Parent() == root {
		level = "##"
	}
	fmt.Fprintf(b, "%s %s\n\n", level, c.CommandPath())
	if c.Deprecated != "" {
		fmt.Fprintf(b, "**Deprecated:** %s\n\n", inlineText(collapseSpace(norm.text(c.Deprecated))))
	}
	if c.Short != "" {
		b.WriteString(inlineText(collapseSpace(norm.text(c.Short))) + "\n\n")
	}
	// Long help is written for a terminal: hard-wrapped, with indented example
	// blocks a Markdown renderer would turn into headings or code. A text
	// block shows it exactly as `--help` does.
	if long := strings.TrimSpace(norm.text(c.Long)); long != "" {
		b.WriteString(codeBlock("text", long))
	}

	b.WriteString("**Usage**\n\n")
	var usage []string
	if c.Runnable() {
		usage = append(usage, c.UseLine())
	}
	if c.HasAvailableSubCommands() {
		usage = append(usage, c.CommandPath()+" [command]")
	}
	b.WriteString(codeBlock("text", strings.Join(usage, "\n")))

	if len(c.Aliases) > 0 {
		aliases := make([]string, len(c.Aliases))
		for i, a := range c.Aliases {
			aliases[i] = codeSpan(a)
		}
		fmt.Fprintf(b, "**Aliases:** %s\n\n", strings.Join(aliases, ", "))
	}
	if ex := strings.TrimSpace(norm.text(c.Example)); ex != "" {
		b.WriteString("**Examples**\n\n")
		b.WriteString(codeBlock("bash", dedent(norm.text(c.Example))))
	}
	if subs := documentedSubcommands(c); len(subs) > 0 {
		b.WriteString("**Subcommands:** " + strings.Join(subs, ", ") + "\n\n")
	}
	local, inherited := flagUsages(c, root)
	if local != "" {
		b.WriteString("**Flags**\n\n")
		b.WriteString(codeBlock("text", local))
	}
	if inherited != "" {
		b.WriteString("**Flags inherited from parent commands**\n\n")
		b.WriteString(codeBlock("text", inherited))
	}
	b.WriteString("See also [global flags](#global-flags).\n\n")
}

func documentedSubcommands(c *cobra.Command) []string {
	var out []string
	for _, sub := range c.Commands() {
		if documented(sub) {
			out = append(out, fmt.Sprintf("[`%s`](#%s)", sub.Name(), anchor(sub.CommandPath())))
		}
	}
	return out
}

// flagUsages renders c's own flags and those it inherits from commands between
// it and the root. The help flag and the root's persistent flags are hidden
// while it renders: the global-flags section documents them once. They are
// hidden only here because UseLine prints "[flags]" for a command whose only
// flag is help, as the binary's own --help does.
func flagUsages(c, root *cobra.Command) (local, inherited string) {
	hide := flagsOf(root.PersistentFlags().VisitAll)
	if h := c.Flags().Lookup("help"); h != nil {
		hide = append(hide, h)
	}
	was := make([]bool, len(hide))
	for i, f := range hide {
		was[i], f.Hidden = f.Hidden, true
	}
	defer func() {
		for i, f := range hide {
			f.Hidden = was[i]
		}
	}()
	local = strings.TrimRight(c.LocalFlags().FlagUsagesWrapped(0), "\n")
	inherited = strings.TrimRight(c.InheritedFlags().FlagUsagesWrapped(0), "\n")
	return local, inherited
}
