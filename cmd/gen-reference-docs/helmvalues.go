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
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	llmkubeValues = "charts/llmkube/values.yaml"
	foremanValues = "charts/foreman/values.yaml"
	// maxInlineDefault is the longest default shown verbatim; a longer list
	// or map is summarized by its size, and the reader follows the link to the
	// file for the content.
	maxInlineDefault = 80
)

// valueRow is one table row: a leaf key, or a map key whose own comment
// documents the keys under it.
type valueRow struct {
	path, def, desc string
}

func renderHelmValues(root string) (string, error) {
	var b strings.Builder
	b.WriteString(pageHeader("Helm values",
		"Every value the llmkube and Foreman Helm charts accept, with its default and description.",
		sourceLink(llmkubeValues)+" and "+sourceLink(foremanValues)))
	b.WriteString("Keys are written as `helm --set` paths: a dot inside a key is escaped as `\\.`. " +
		"A description is the comment above the key in values.yaml; a key without one has an empty cell. " +
		"A map whose own comment describes the keys under it gets a row with no default. " +
		"A list or map too long to show inline is summarized by its size.\n\n")

	llmkube, err := loadValues(filepath.Join(root, llmkubeValues))
	if err != nil {
		return "", err
	}
	writeValuesSections(&b, llmkube, "##", "")

	foreman, err := loadValues(filepath.Join(root, foremanValues))
	if err != nil {
		return "", err
	}
	b.WriteString("## Foreman chart\n\n")
	fmt.Fprintf(&b, "The Foreman chart is installed separately from the llmkube chart. Its values come from %s.\n\n",
		sourceLink(foremanValues))
	writeValuesSections(&b, foreman, "###", "Foreman: ")
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

func loadValues(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: top level is not a map", path)
	}
	return doc.Content[0], nil
}

// writeValuesSections writes one heading per top-level key, in file order.
// A top-level map's comment becomes the section's prose; its keys fill the
// table.
func writeValuesSections(b *strings.Builder, top *yaml.Node, level, headingPrefix string) {
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, val := top.Content[i], resolveAlias(top.Content[i+1])
		fmt.Fprintf(b, "%s %s%s\n\n", level, headingPrefix, key.Value)
		var rows []valueRow
		if isNestedMap(val) {
			if desc := commentText(key, val); desc != "" {
				b.WriteString(inlineText(desc) + "\n\n")
			}
			rows = collectValueRows(val, escapeKey(key.Value))
		} else {
			rows = []valueRow{{escapeKey(key.Value), defaultText(val), commentText(key, val)}}
		}
		b.WriteString(tableHeader("Key", "Default", "Description"))
		for _, r := range rows {
			b.WriteString(tableRow(codeSpan(r.path), r.def, tableCell(r.desc)))
		}
		b.WriteString("\n")
	}
}

func collectValueRows(m *yaml.Node, prefix string) []valueRow {
	var rows []valueRow
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, val := m.Content[i], resolveAlias(m.Content[i+1])
		path := prefix + "." + escapeKey(key.Value)
		desc := commentText(key, val)
		if isNestedMap(val) {
			if desc != "" {
				rows = append(rows, valueRow{path, "", desc})
			}
			rows = append(rows, collectValueRows(val, path)...)
			continue
		}
		rows = append(rows, valueRow{path, defaultText(val), desc})
	}
	return rows
}

// isNestedMap reports whether a value is a map with keys of its own, which
// the table descends into. An empty map is a leaf whose default is {}.
func isNestedMap(n *yaml.Node) bool {
	return n.Kind == yaml.MappingNode && len(n.Content) > 0
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// escapeKey writes a key the way helm --set addresses it.
func escapeKey(k string) string {
	return strings.ReplaceAll(k, ".", `\.`)
}

// commentText is a key's documentation: the comment block above it plus any
// comment on the same line, with the # markers stripped and the lines joined.
// Foot comments are left out: yaml.v3 hangs a commented-out example that
// follows a value there, and that is not the value's description.
func commentText(key, val *yaml.Node) string {
	var parts []string
	for _, c := range []string{key.HeadComment, key.LineComment, val.LineComment} {
		for _, line := range strings.Split(c, "\n") {
			line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
			if line != "" {
				parts = append(parts, line)
			}
		}
	}
	return strings.Join(parts, " ")
}

// defaultText renders a leaf's default as inline code, or, for a value too
// long to inline, a summary of its size.
func defaultText(n *yaml.Node) string {
	if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "\n") {
		return fmt.Sprintf("multi-line string, %d lines", strings.Count(strings.TrimRight(n.Value, "\n"), "\n")+1)
	}
	s := flowYAML(n)
	if len(s) <= maxInlineDefault {
		return strings.ReplaceAll(codeSpan(s), "|", `\|`)
	}
	switch n.Kind {
	case yaml.SequenceNode:
		return fmt.Sprintf("list of %d items", len(n.Content))
	case yaml.MappingNode:
		return fmt.Sprintf("map of %d keys", len(n.Content)/2)
	}
	return strings.ReplaceAll(codeSpan(s), "|", `\|`)
}

// flowYAML renders a node as one line of flow-style YAML, keeping each
// scalar's quoting as values.yaml writes it.
func flowYAML(n *yaml.Node) string {
	n = resolveAlias(n)
	switch n.Kind {
	case yaml.SequenceNode:
		items := make([]string, len(n.Content))
		for i, c := range n.Content {
			items[i] = flowYAML(c)
		}
		return "[" + strings.Join(items, ", ") + "]"
	case yaml.MappingNode:
		pairs := make([]string, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			pairs = append(pairs, flowYAML(n.Content[i])+": "+flowYAML(n.Content[i+1]))
		}
		return "{" + strings.Join(pairs, ", ") + "}"
	}
	switch {
	case n.Tag == "!!null":
		return "null"
	case n.Style&yaml.DoubleQuotedStyle != 0:
		return strconv.Quote(n.Value)
	case n.Style&yaml.SingleQuotedStyle != 0:
		return "'" + strings.ReplaceAll(n.Value, "'", "''") + "'"
	case n.Tag == "!!str" && n.Value == "":
		return `""`
	}
	return n.Value
}
