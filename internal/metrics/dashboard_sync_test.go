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

const (
	prometheusRuleTpl  = "../../charts/llmkube/templates/prometheusrule.yaml"
	runtimeMetricsGlob = "testdata/*-metrics.txt"
)

var dashboardDirs = []string{"../../config/grafana", "../../charts/llmkube/dashboards"}

// externalPrefixes are metric namespaces owned by exporters outside this repo.
var externalPrefixes = []string{
	"vllm:",        // vLLM runtime, and the Pyrra rules derived from it
	"up:",          // Pyrra burn-rate rules over scrape liveness
	"DCGM_FI_DEV_", // NVIDIA dcgm-exporter
	"amdgpu_",      // amdgpu-sysfs exporter
	"node_",        // node-exporter
}

// promqlWords are the identifiers a PromQL expression can hold that are not
// metric names. Anything left over is treated as a metric selector.
var promqlWords = strings.Fields(`
abs absent absent_over_time and avg avg_over_time bool bottomk by ceil changes
clamp clamp_max clamp_min count count_over_time count_values day_of_month
day_of_week days_in_month deg delta deriv exp floor group_left group_right
histogram_quantile holt_winters hour idelta ignoring increase inf irate
label_join label_replace label_values last_over_time ln log10 log2 max
max_over_time min min_over_time minute month nan offset on or pi predict_linear
present_over_time quantile quantile_over_time rad rate resets round scalar sgn
sort sort_desc sqrt start stddev stddev_over_time stdvar stdvar_over_time sum
sum_over_time time timestamp topk unless vector without year
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
	}
	identifier = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)
	// Desc keeps fqName unexported; String() is the only accessor.
	descFQName = regexp.MustCompile(`fqName: "([^"]+)"`)
	recordRule = regexp.MustCompile(`(?m)^\s*- record:\s*(\S+)`)
)

// declaredNames returns the fqName of every collector this repo registers.
// AllCollectors is the controller's set; the Metal agent binary registers 19
// more into its own registry, and *prometheus.Registry is itself a Collector.
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

// runtimeNames returns the metric names the inference runtimes' own servers
// expose. Each fixture holds one name per line; the file set is the runtime set.
func runtimeNames(t *testing.T) map[string]bool {
	t.Helper()

	files, err := filepath.Glob(runtimeMetricsGlob)
	if err != nil {
		t.Fatalf("glob %s: %v", runtimeMetricsGlob, err)
	}
	if len(files) == 0 {
		t.Fatalf("no runtime metric fixtures matching %s", runtimeMetricsGlob)
	}

	names := map[string]bool{}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				names[line] = true
			}
		}
	}
	return names
}

// metricNames extracts the metric selectors from a PromQL expression: strip
// every construct that can hold a non-metric identifier, then keep the
// identifiers PromQL itself does not define.
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
			for key, child := range typed {
				if s, ok := child.(string); ok && key == "expr" {
					queries = append(queries, s)
					continue
				}
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

// TestDashboardsQueryEmittedMetrics fails when a shipped dashboard queries a
// name nothing produces. Such a panel renders empty, which is indistinguishable
// from an idle cluster (#786, #1223, #1226).
//
// It checks that a name can be emitted, not that it ever is: GPUQueueWaitDuration
// is declared and registered with no Observe() call anywhere, and passes.
func TestDashboardsQueryEmittedMetrics(t *testing.T) {
	known := declaredNames(t)
	maps.Copy(known, chartRecordingRules(t))
	maps.Copy(known, runtimeNames(t))

	for _, dir := range dashboardDirs {
		dashboards, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		if len(dashboards) == 0 {
			t.Fatalf("no dashboards under %s", dir)
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

	if t.Failed() {
		t.Logf("emittable: %v", slices.Sorted(maps.Keys(known)))
	}
}

// TestInferenceDashboardCoversEveryRuntime fails when a panel on the shared
// Inference Monitor reads one runtime's metrics without the others'. Such a
// panel is blank in any namespace running only the runtime it omits (#1227).
//
// Scoped to this dashboard: amd-gpu-observability is llama.cpp-specific and
// llmkube-inference reads only recording rules.
func TestInferenceDashboardCoversEveryRuntime(t *testing.T) {
	const dashboard = "../../config/grafana/llmkube-inference-dashboard.json"

	// Runtimes come from the fixtures, so a new one widens the gate on drop-in.
	want := map[string]bool{}
	for name := range runtimeNames(t) {
		if prefix, _, found := strings.Cut(name, ":"); found {
			want[prefix] = true
		}
	}

	checked := 0
	for _, query := range dashboardQueries(t, dashboard) {
		runtimes := map[string]bool{}
		for _, name := range metricNames(query) {
			if prefix, _, found := strings.Cut(name, ":"); found && want[prefix] {
				runtimes[prefix] = true
			}
		}
		if len(runtimes) == 0 {
			continue
		}

		checked++
		if len(runtimes) != len(want) {
			t.Errorf("%s reads %v only, so the panel is blank on every other runtime\n\tquery: %s",
				dashboard, slices.Sorted(maps.Keys(runtimes)), query)
		}
	}

	// Without this the test passes vacuously once the panels are gone.
	if checked == 0 {
		t.Fatalf("%s has no runtime metric queries", dashboard)
	}
}
