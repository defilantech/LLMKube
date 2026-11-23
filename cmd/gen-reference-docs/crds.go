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
	"sort"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

const (
	crdDir = "config/crd/bases"
	// crdFieldDepth is how many levels below spec and status the field tables
	// go: spec.a.b.c is depth 3. InferenceService has 808 schema fields; past
	// the third level they are almost all embedded Kubernetes core types
	// (affinity terms, volume sources, probes, env valueFrom), which Kubernetes
	// documents and the YAML spells out. Three levels keep the CRDs' own nested
	// config (sglangConfig.speculative, for one) and cut InferenceService to
	// 550 rows.
	crdFieldDepth = 3
)

// crdGroupOrder puts the groups a user meets first at the top. A group not
// listed here follows, alphabetically, rather than being dropped.
var crdGroupOrder = []string{"inference.llmkube.dev", "foreman.llmkube.dev", "federation.llmkube.dev"}

type crdDoc struct {
	file string
	crd  apiextensionsv1.CustomResourceDefinition
}

func renderCRDs(root string) (string, error) {
	files, err := filepath.Glob(filepath.Join(root, crdDir, "*.yaml"))
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no CRDs under %s", filepath.Join(root, crdDir))
	}
	byGroup := map[string][]crdDoc{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(data, &crd); err != nil {
			return "", fmt.Errorf("parse %s: %w", f, err)
		}
		if crd.Kind != "CustomResourceDefinition" {
			return "", fmt.Errorf("%s is a %q, not a CustomResourceDefinition", f, crd.Kind)
		}
		g := crd.Spec.Group
		byGroup[g] = append(byGroup[g], crdDoc{file: filepath.ToSlash(filepath.Join(crdDir, filepath.Base(f))), crd: crd})
	}
	groups := orderedGroups(byGroup)

	var b strings.Builder
	b.WriteString(pageHeader("CRD reference",
		"Every LLMKube custom resource: names, printer columns, and the spec and status fields of its schema.",
		"the generated CRD manifests in "+codeSpan(crdDir+"/")))
	fmt.Fprintf(&b, "Field tables go %d levels below `spec` and `status` (`spec.a.b.c` is the deepest row). "+
		"A field with deeper fields says how many it has; those, and the CEL validation rules, "+
		"are in the CRD's YAML, linked from each section. Array items are written `[]`, map values `<key>`.\n\n",
		crdFieldDepth)

	b.WriteString(tableHeader("Kind", "Group", "Scope", "Short names"))
	for _, g := range groups {
		for _, d := range byGroup[g] {
			n := d.crd.Spec.Names
			b.WriteString(tableRow(fmt.Sprintf("[%s](#%s)", n.Kind, anchor(n.Kind)), codeSpan(g),
				string(d.crd.Spec.Scope), codeList(n.ShortNames)))
		}
	}
	b.WriteString("\n")

	for _, g := range groups {
		fmt.Fprintf(&b, "## %s\n\n", g)
		for _, d := range byGroup[g] {
			renderCRD(&b, d)
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

func orderedGroups(byGroup map[string][]crdDoc) []string {
	rank := map[string]int{}
	for i, g := range crdGroupOrder {
		rank[g] = i + 1
	}
	groups := make([]string, 0, len(byGroup))
	for g, docs := range byGroup {
		groups = append(groups, g)
		sort.Slice(docs, func(i, j int) bool { return docs[i].crd.Spec.Names.Kind < docs[j].crd.Spec.Names.Kind })
	}
	sort.Slice(groups, func(i, j int) bool {
		ri, rj := rank[groups[i]], rank[groups[j]]
		switch {
		case ri != 0 && rj != 0:
			return ri < rj
		case ri != 0 || rj != 0:
			return ri != 0
		}
		return groups[i] < groups[j]
	})
	return groups
}

func codeList(items []string) string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = codeSpan(s)
	}
	return strings.Join(out, ", ")
}

func renderCRD(b *strings.Builder, d crdDoc) {
	spec := d.crd.Spec
	kind := spec.Names.Kind
	fmt.Fprintf(b, "### %s\n\n", kind)
	var versions []string
	for _, v := range spec.Versions {
		if v.Served {
			versions = append(versions, spec.Group+"/"+v.Name)
		}
	}
	b.WriteString(tableHeader("Property", "Value"))
	b.WriteString(tableRow("Kind", codeSpan(kind)))
	b.WriteString(tableRow("API versions", codeList(versions)))
	b.WriteString(tableRow("Scope", string(spec.Scope)))
	b.WriteString(tableRow("Plural", codeSpan(spec.Names.Plural)))
	if len(spec.Names.ShortNames) > 0 {
		b.WriteString(tableRow("Short names", codeList(spec.Names.ShortNames)))
	}
	if len(spec.Names.Categories) > 0 {
		b.WriteString(tableRow("Categories", codeList(spec.Names.Categories)))
	}
	b.WriteString(tableRow("Schema", sourceLink(d.file)))
	b.WriteString("\n")

	for _, v := range spec.Versions {
		if !v.Served {
			continue
		}
		suffix := ""
		if len(versions) > 1 {
			suffix = " (" + v.Name + ")"
		}
		renderPrinterColumns(b, kind+" printer columns"+suffix, v.AdditionalPrinterColumns)
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		top := v.Schema.OpenAPIV3Schema
		for _, part := range []string{"spec", "status"} {
			if s, ok := top.Properties[part]; ok {
				renderFieldTable(b, fmt.Sprintf("%s %s%s", kind, part, suffix), part, s)
			}
		}
	}
}

func renderPrinterColumns(b *strings.Builder, heading string, cols []apiextensionsv1.CustomResourceColumnDefinition) {
	if len(cols) == 0 {
		return
	}
	fmt.Fprintf(b, "#### %s\n\n", heading)
	b.WriteString("Priority 0 columns show in `kubectl get`; the others only with `-o wide`.\n\n")
	b.WriteString(tableHeader("Name", "Type", "JSONPath", "Priority", "Description"))
	for _, c := range cols {
		b.WriteString(tableRow(tableCell(c.Name), codeSpan(c.Type), tableCell(codeSpan(c.JSONPath)),
			strconv.Itoa(int(c.Priority)), tableCell(firstParagraph(c.Description))))
	}
	b.WriteString("\n")
}

type fieldRow struct {
	path, typ, def, validation, desc string
	required                         bool
}

func renderFieldTable(b *strings.Builder, heading, root string, s apiextensionsv1.JSONSchemaProps) {
	var rows []fieldRow
	collectFields(&rows, s, root, 0)
	fmt.Fprintf(b, "#### %s\n\n", heading)
	if desc := firstParagraph(s.Description); desc != "" {
		b.WriteString(inlineText(desc) + "\n\n")
	}
	if len(rows) == 0 {
		b.WriteString("No fields.\n\n")
		return
	}
	b.WriteString(tableHeader("Field", "Type", "Required", "Default", "Validation", "Description"))
	for _, r := range rows {
		req := ""
		if r.required {
			req = "yes"
		}
		b.WriteString(tableRow(tableCell(codeSpan(r.path)), tableCell(r.typ), req,
			tableCell(codeSpan(r.def)), tableCell(r.validation), tableCell(r.desc)))
	}
	b.WriteString("\n")
}

// collectFields appends a row per field of s, in name order, down to
// crdFieldDepth levels below the root.
func collectFields(rows *[]fieldRow, s apiextensionsv1.JSONSchemaProps, prefix string, depth int) {
	props, childPrefix := childProperties(s, prefix)
	if props == nil {
		return
	}
	required := map[string]bool{}
	for _, r := range props.required {
		required[r] = true
	}
	names := make([]string, 0, len(props.fields))
	for name := range props.fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := props.fields[name]
		path := childPrefix + name
		desc := firstParagraph(f.Description)
		if depth+1 >= crdFieldDepth {
			if n := countNested(f, path); n > 0 {
				desc = joinSentences(desc, fmt.Sprintf("Has %d nested fields; see the YAML.", n))
			}
		}
		*rows = append(*rows, fieldRow{
			path:       path,
			typ:        schemaType(f),
			def:        compactJSON(f.Default),
			validation: validationText(f),
			desc:       desc,
			required:   required[name],
		})
		if depth+1 < crdFieldDepth {
			collectFields(rows, f, path, depth+1)
		}
	}
}

type propertySet struct {
	fields   map[string]apiextensionsv1.JSONSchemaProps
	required []string
}

// childProperties finds the fields under s: its own properties, those of its
// array items, or those of its map values, with the path prefix that marks
// which. Arrays of arrays unwrap one level per [].
func childProperties(s apiextensionsv1.JSONSchemaProps, prefix string) (*propertySet, string) {
	switch {
	case len(s.Properties) > 0:
		return &propertySet{s.Properties, s.Required}, prefix + "."
	case s.Items != nil && s.Items.Schema != nil:
		return childProperties(*s.Items.Schema, prefix+"[]")
	case s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil:
		return childProperties(*s.AdditionalProperties.Schema, prefix+".<key>")
	}
	return nil, ""
}

func countNested(s apiextensionsv1.JSONSchemaProps, prefix string) int {
	props, childPrefix := childProperties(s, prefix)
	if props == nil {
		return 0
	}
	n := 0
	for name, f := range props.fields {
		n += 1 + countNested(f, childPrefix+name)
	}
	return n
}

func schemaType(s apiextensionsv1.JSONSchemaProps) string {
	switch {
	case s.XIntOrString:
		return "int or string"
	case s.Type == "array" && s.Items != nil && s.Items.Schema != nil:
		return "array of " + schemaType(*s.Items.Schema)
	case s.Type == "object" && s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil:
		return "map of " + schemaType(*s.AdditionalProperties.Schema)
	case s.Type == "object" && s.XPreserveUnknownFields != nil && *s.XPreserveUnknownFields && len(s.Properties) == 0:
		return "object (any fields)"
	case s.Type == "":
		if s.XPreserveUnknownFields != nil && *s.XPreserveUnknownFields {
			return "any"
		}
		return ""
	case s.Format != "":
		return s.Type + " (" + s.Format + ")"
	}
	return s.Type
}

// validationText lists the schema's value constraints. CEL rules are left to
// the YAML: their messages read as prose, and some run several lines.
func validationText(s apiextensionsv1.JSONSchemaProps) string {
	var parts []string
	if len(s.Enum) > 0 {
		vals := make([]string, len(s.Enum))
		for i, e := range s.Enum {
			vals[i] = codeSpan(compactJSON(&e))
		}
		parts = append(parts, "one of "+strings.Join(vals, ", "))
	}
	if s.Pattern != "" {
		parts = append(parts, "pattern "+codeSpan(s.Pattern))
	}
	if s.Minimum != nil {
		parts = append(parts, bound("min", *s.Minimum, s.ExclusiveMinimum))
	}
	if s.Maximum != nil {
		parts = append(parts, bound("max", *s.Maximum, s.ExclusiveMaximum))
	}
	lengths := []struct {
		label string
		v     *int64
	}{
		{"min length", s.MinLength}, {"max length", s.MaxLength},
		{"min items", s.MinItems}, {"max items", s.MaxItems},
		{"min properties", s.MinProperties}, {"max properties", s.MaxProperties},
	}
	for _, l := range lengths {
		if l.v != nil {
			parts = append(parts, fmt.Sprintf("%s %d", l.label, *l.v))
		}
	}
	return strings.Join(parts, "; ")
}

func bound(label string, v float64, exclusive bool) string {
	s := label + " " + strconv.FormatFloat(v, 'f', -1, 64)
	if exclusive {
		s += " (exclusive)"
	}
	return s
}

func compactJSON(j *apiextensionsv1.JSON) string {
	if j == nil {
		return ""
	}
	return string(j.Raw)
}

// firstParagraph is a description's opening paragraph on one line. Kubebuilder
// descriptions often continue with examples and caveats that do not fit a
// table cell; the YAML keeps them.
func firstParagraph(desc string) string {
	desc = strings.TrimSpace(desc)
	if i := strings.Index(desc, "\n\n"); i >= 0 {
		desc = desc[:i]
	}
	return collapseSpace(desc)
}
