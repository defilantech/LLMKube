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

package metrics

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/defilantech/llmkube/pkg/agent"
)

const prometheusRuleTpl = "../../charts/llmkube/templates/prometheusrule.yaml"

// Every grafana directory in the repo, so a new one is covered without an edit.
const dashboardGlob = "../../*/grafana/*.json"

// externalPrefixes are exporters config/grafana/SETUP.md tells operators to
// install. An exporter this repo does not document is a phantom metric, not an
// external one: otel_span_metrics was deleted rather than listed here.
var externalPrefixes = []string{
	"vllm:",        // vLLM runtime, and the Pyrra rules derived from it
	"up:",          // Pyrra burn-rate rules over scrape liveness
	"DCGM_FI_DEV_", // NVIDIA dcgm-exporter
	"amdgpu_",      // amdgpu-sysfs exporter
	"node_",        // node-exporter
}

// promqlWords are the identifiers PromQL allows outside a call: operators and
// modifiers. Function names need no listing, a call is stripped by its "(".
var promqlWords = strings.Fields(`
and atan2 bool by group_left group_right ignoring inf nan offset on or unless without
`)

// llamacppNames is what the llama.cpp server exposes on /metrics under
// --metrics. Refresh with:
//
//	curl -s $POD:8080/metrics | grep -oP '^# TYPE \K\S+' | sort
var llamacppNames = strings.Fields(`
llamacpp:n_busy_slots_per_decode llamacpp:n_decode_total llamacpp:n_tokens_max
llamacpp:predicted_tokens_seconds llamacpp:prompt_seconds_total
llamacpp:prompt_tokens_seconds llamacpp:prompt_tokens_total
llamacpp:requests_deferred llamacpp:requests_processing
llamacpp:tokens_predicted_seconds_total llamacpp:tokens_predicted_total
`)

var (
	// label_values(metric, label) selects on metric; label_values(label) does not.
	labelValues = regexp.MustCompile(`label_values\(\s*(?:(.*),)?\s*\w+\s*\)`)
	// Order matters: Grafana variables come out before the label matchers they
	// are glued into, e.g. up:sum${window_suffix}{slo=~"$slo"}.
	exprNoise = []*regexp.Regexp{
		regexp.MustCompile(`\$\{[^}]*\}`), // ${datasource}
		regexp.MustCompile(`\$\w+`),       // $namespace
		regexp.MustCompile(`"[^"]*"`),     // string literals
		regexp.MustCompile(`\{[^}]*\}`),   // label matchers
		regexp.MustCompile(`\[[^\]]*\]`),  // range selectors
		regexp.MustCompile(`\b(?:by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`),
		// Last: an identifier before "(" is a function, never a metric.
		regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*\s*\(`),
	}
	identifier = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)
	// Desc keeps fqName unexported; String() is the only accessor.
	descFQName = regexp.MustCompile(`fqName: "([^"]+)"`)
	recordRule = regexp.MustCompile(`(?m)^\s*- record:\s*(\S+)`)
)

// declaredNames returns the fqName of every collector this repo registers,
// controller plus Metal agent.
func declaredNames(t *testing.T) map[string]bool {
	t.Helper()

	registered := append(slices.Clone(AllCollectors), agent.AgentRegistry)

	descs := make(chan *prometheus.Desc)
	go func() {
		defer close(descs)
		for _, collector := range registered {
			collector.Describe(descs)
		}
	}()

	names := map[string]bool{}
	for desc := range descs {
		m := descFQName.FindStringSubmatch(desc.String())
		if m == nil {
			t.Fatalf("no fqName in Desc %s", desc)
		}
		names[m[1]] = true
	}
	return names
}

// chartRecordingRules returns the names the chart's PrometheusRule records.
// These are the only llmkube: series that exist.
func chartRecordingRules(t *testing.T) map[string]bool {
	t.Helper()

	tpl, err := os.ReadFile(prometheusRuleTpl)
	if err != nil {
		t.Fatalf("read %s: %v", prometheusRuleTpl, err)
	}

	names := map[string]bool{}
	for _, m := range recordRule.FindAllStringSubmatch(string(tpl), -1) {
		names[m[1]] = true
	}
	if len(names) == 0 {
		t.Fatalf("no recording rules in %s", prometheusRuleTpl)
	}
	return names
}

// metricNames extracts the metric selectors from a PromQL expression.
func metricNames(expr string) []string {
	expr = labelValues.ReplaceAllString(expr, " $1 ")
	for _, noise := range exprNoise {
		expr = noise.ReplaceAllString(expr, " ")
	}

	var names []string
	for _, token := range identifier.FindAllString(expr, -1) {
		if !slices.Contains(promqlWords, token) {
			names = append(names, token)
		}
	}
	return names
}

// dashboardQueries returns every PromQL string a dashboard evaluates: panel
// target expressions and the queries behind its template variables.
func dashboardQueries(t *testing.T, path string) []string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var queries []string
	var walk func(node any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			if s, ok := typed["expr"].(string); ok {
				queries = append(queries, s)
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(doc)

	// Only type=query variables hold PromQL; custom, textbox and datasource
	// variables hold literal option lists.
	templating, _ := doc["templating"].(map[string]any)
	list, _ := templating["list"].([]any)
	for _, item := range list {
		variable, _ := item.(map[string]any)
		if variable["type"] != "query" {
			continue
		}
		switch query := variable["query"].(type) {
		case string:
			queries = append(queries, query)
		case map[string]any:
			if s, ok := query["query"].(string); ok {
				queries = append(queries, s)
			}
		}
	}
	return queries
}

func emitted(name string, known map[string]bool) bool {
	if known[name] {
		return true
	}
	// Dashboards select histogram series; the Desc carries only the base name.
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if base := strings.TrimSuffix(name, suffix); base != name && known[base] {
			return true
		}
	}
	for _, prefix := range externalPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// TestDashboardsQueryEmittedMetrics checks a queried name is declared, not that
// anything ever writes it, so it catches a misspelled or renamed series (#1223,
// #1226) but not an inert one. It would not have caught #786, where
// llmkube_inference_ttft_seconds was declared and registered with no Observe
// call; that class is only fixed by deleting the declaration.
func TestDashboardsQueryEmittedMetrics(t *testing.T) {
	known := declaredNames(t)
	maps.Copy(known, chartRecordingRules(t))
	for _, name := range llamacppNames {
		known[name] = true
	}

	dashboards, err := filepath.Glob(dashboardGlob)
	if err != nil {
		t.Fatalf("glob %s: %v", dashboardGlob, err)
	}
	if len(dashboards) == 0 {
		t.Fatalf("no dashboards under %s", dashboardGlob)
	}

	for _, dashboard := range dashboards {
		reported := map[string]bool{}
		for _, query := range dashboardQueries(t, dashboard) {
			for _, name := range metricNames(query) {
				if emitted(name, known) || reported[name] {
					continue
				}
				reported[name] = true
				t.Errorf("%s queries %q, which no registered collector, chart recording rule or allowlisted exporter emits\n\tquery: %s",
					dashboard, name, query)
			}
		}
	}
}
