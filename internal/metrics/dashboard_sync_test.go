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
	"io/fs"
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

// Every dashboard directory in the repo, so a new one is covered without an
// edit: the shipped set moved under the chart, config/grafana still holds the
// rest.
var dashboardGlobs = []string{"../../*/grafana/*.json", "../../charts/*/dashboards/*.json"}

// externalPrefixes are metric namespaces owned by exporters outside this repo.
var externalPrefixes = []string{
	// Pyrra generates these from the vLLM histogram; the runtime's own vllm:
	// metrics are fixture-verified like every other runtime.
	"vllm:e2e_request_latency_seconds:",
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

	labelMatcher    = regexp.MustCompile(`\{[^}]*\}`)
	soleAggregation = regexp.MustCompile(`^(?:sum|count)(?:\s+(?:by|without)\s*\([^)]*\))?\s*\(`)
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

// panelConvention pairs a panel's own fieldConfig.defaults with the PromQL
// its own targets evaluate, so a convention can check both together instead
// of the flat, panel-agnostic list dashboardQueries returns.
type panelConvention struct {
	title     string
	panelType string
	defaults  map[string]any
	exprs     []string
}

// dashboardPanels walks a dashboard like dashboardQueries does, but stops at
// every object carrying its own fieldConfig.defaults (a panel) instead of
// flattening every "expr" found anywhere in the document. It also returns
// the dashboard's template variable names, needed by the label-matcher
// convention.
func dashboardPanels(t *testing.T, path string) ([]panelConvention, []string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var panels []panelConvention
	var walk func(node any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			if fc, ok := typed["fieldConfig"].(map[string]any); ok {
				if defaults, ok := fc["defaults"].(map[string]any); ok {
					targets, _ := typed["targets"].([]any)
					var exprs []string
					for _, target := range targets {
						if tm, ok := target.(map[string]any); ok {
							if s, ok := tm["expr"].(string); ok {
								exprs = append(exprs, s)
							}
						}
					}
					title, _ := typed["title"].(string)
					panelType, _ := typed["type"].(string)
					panels = append(panels, panelConvention{title: title, panelType: panelType, defaults: defaults, exprs: exprs})
				}
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

	var vars []string
	templating, _ := doc["templating"].(map[string]any)
	list, _ := templating["list"].([]any)
	for _, item := range list {
		variable, _ := item.(map[string]any)
		if name, ok := variable["name"].(string); ok {
			vars = append(vars, name)
		}
	}
	return panels, vars
}

// isRatioAgainstLimit reports whether expr is a bare "metric / metric"
// division: exactly one '/', no other arithmetic mixed in, not a formula
// that starts from a constant. That excludes the shapes the convention
// exempts: an error budget ("1 - (...)") that can go negative, and a
// burn-rate reference line ("14 * (1 - $objective/100)") that is unbounded.
func isRatioAgainstLimit(expr string) bool {
	expr = strings.TrimSpace(expr)
	if strings.Count(expr, "/") != 1 || strings.ContainsAny(expr, "*+") {
		return false
	}
	return expr != "" && (expr[0] < '0' || expr[0] > '9')
}

// isSoleAggregation reports whether expr's outermost operation is a single
// sum() or count() call with nothing applied outside it. A ratio-of-sums
// like sum(a)/sum(b) also starts with "sum(", but its outer operator is the
// division, so the sum() match must close at the very end of expr, not
// partway through, to count.
func isSoleAggregation(expr string) bool {
	expr = strings.TrimSpace(expr)
	loc := soleAggregation.FindStringIndex(expr)
	if loc == nil {
		return false
	}
	depth := 0
	for i := loc[1] - 1; i < len(expr); i++ {
		switch expr[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i == len(expr)-1
			}
		}
	}
	return false
}

// filtersTemplateVarInLabelMatcher reports whether any of expr's label
// matchers ({...}) references one of the dashboard's own template
// variables, e.g. {service=~"$service"}.
func filtersTemplateVarInLabelMatcher(expr string, vars []string) bool {
	for _, matcher := range labelMatcher.FindAllString(expr, -1) {
		for _, v := range vars {
			if regexp.MustCompile(`\$` + regexp.QuoteMeta(v) + `\b`).MatchString(matcher) {
				return true
			}
		}
	}
	return false
}

// dashboardConventions are the three conventions applied by hand across
// every panel in charts/llmkube/dashboards (see the "apply shared
// conventions for min/max, noValue, and series color" commit). All three
// are scoped to timeseries panels: stat and gauge panels color by
// threshold, not by series, and table panels have neither a per-series
// color nor an axis to autoscale, so none of the three read on them the way
// they do on a graph.
var dashboardConventions = []struct {
	name    string
	applies func(p panelConvention, vars []string) bool
	holds   func(p panelConvention) bool
	reason  string
}{
	{
		name: `percentunit ratio against a hard limit sets min:0 and max:1`,
		applies: func(p panelConvention, _ []string) bool {
			if p.panelType != "timeseries" || p.defaults["unit"] != "percentunit" {
				return false
			}
			return slices.ContainsFunc(p.exprs, isRatioAgainstLimit)
		},
		holds: func(p panelConvention) bool {
			min, minOK := p.defaults["min"].(float64)
			max, maxOK := p.defaults["max"].(float64)
			return minOK && min == 0 && maxOK && max == 1
		},
		reason: "reads against the limit instead of autoscaling",
	},
	{
		name: `sum()/count() as the outer PromQL operator sets noValue:"0"`,
		applies: func(p panelConvention, _ []string) bool {
			return slices.ContainsFunc(p.exprs, isSoleAggregation)
		},
		holds: func(p panelConvention) bool {
			noValue, ok := p.defaults["noValue"].(string)
			return ok && noValue == "0"
		},
		reason: `an empty vector on a fresh cluster otherwise renders "No data" instead of a real zero`,
	},
	{
		name: `a template variable inside a label matcher sets color.mode "palette-classic-by-name"`,
		applies: func(p panelConvention, vars []string) bool {
			if p.panelType != "timeseries" {
				return false
			}
			return slices.ContainsFunc(p.exprs, func(e string) bool {
				return filtersTemplateVarInLabelMatcher(e, vars)
			})
		},
		holds: func(p panelConvention) bool {
			color, _ := p.defaults["color"].(map[string]any)
			mode, _ := color["mode"].(string)
			return mode == "palette-classic-by-name"
		},
		reason: "so filtering the variable doesn't repaint the series that remain",
	},
}

// TestDashboardConventions fails when a panel matches one of the three
// conventions above but doesn't carry the fieldConfig it requires. Unlike
// TestDashboardsQueryEmittedMetrics, which flattens every dashboard into a
// bare list of expr strings, this walks panel by panel so a convention can
// see a panel's unit, color and target queries together.
//
// Scoped to charts/llmkube/dashboards: conventions were hand-applied only to
// the dashboards shipped in the chart, not to config/grafana's standalone
// pair, which predates that pass.
func TestDashboardConventions(t *testing.T) {
	dashboards, err := filepath.Glob("../../charts/*/dashboards/*.json")
	if err != nil {
		t.Fatalf("glob charts dashboards: %v", err)
	}
	if len(dashboards) == 0 {
		t.Fatalf("no dashboards matching ../../charts/*/dashboards/*.json")
	}

	for _, dashboard := range dashboards {
		panels, vars := dashboardPanels(t, dashboard)
		for _, p := range panels {
			for _, rule := range dashboardConventions {
				if !rule.applies(p, vars) || rule.holds(p) {
					continue
				}
				t.Errorf("%s panel %q violates convention %s: %s", dashboard, p.title, rule.name, rule.reason)
			}
		}
	}
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

	var dashboards []string
	for _, glob := range dashboardGlobs {
		matched, err := filepath.Glob(glob)
		if err != nil {
			t.Fatalf("glob %s: %v", glob, err)
		}
		dashboards = append(dashboards, matched...)
	}
	if len(dashboards) == 0 {
		t.Fatalf("no dashboards matching %v", dashboardGlobs)
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

	if t.Failed() {
		t.Logf("emittable: %v", slices.Sorted(maps.Keys(known)))
	}
}

// TestDocContractMetrics fails when a prose document's metrics-contract region
// names a metric that nothing produces. The region is delimited by
// <!-- metrics-contract:begin --> and <!-- metrics-contract:end --> markers so
// that "Known gaps" sections that deliberately name missing metrics are not
// flagged.
func TestDocContractMetrics(t *testing.T) {
	known := declaredNames(t)
	maps.Copy(known, chartRecordingRules(t))
	maps.Copy(known, runtimeNames(t))

	// Walk for markdown files rather than globbing: filepath.Glob has no "**"
	// operator, so "docs/**/*.md" silently means "docs/*/*.md" and would skip
	// every doc nested deeper than one directory, including
	// docs/site/guides/, where this same defect class was fixed in #1386.
	var mdFiles []string
	for _, root := range []string{"../../docs", "../../config"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".md") {
				mdFiles = append(mdFiles, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	for _, path := range mdFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		// Only process files with the contract markers.
		begin := "<!-- metrics-contract:begin -->"
		end := "<!-- metrics-contract:end -->"
		idxBegin := strings.Index(string(raw), begin)
		idxEnd := strings.Index(string(raw), end)
		if idxBegin < 0 || idxEnd < 0 || idxEnd <= idxBegin {
			continue
		}

		region := string(raw)[idxBegin+len(begin) : idxEnd]

		// Extract backticked metric names (llamacpp:*, sglang:*, vllm:*, etc.).
		backtick := regexp.MustCompile("`([a-zA-Z_:][a-zA-Z0-9_:]*)`")
		for _, m := range backtick.FindAllStringSubmatch(region, -1) {
			name := m[1]
			if emitted(name, known) {
				continue
			}
			t.Errorf("%s names %q inside metrics-contract region, which no registered collector, chart recording rule or allowlisted exporter emits",
				path, name)
		}
	}
}

// TestInferenceDashboardCoversEveryRuntime fails when a panel on the shared
// Inference Monitor reads one runtime's metrics without the others'. Such a
// panel is blank in any namespace running only the runtime it omits (#1227).
//
// Scoped to this dashboard: amd-gpu-observability is llama.cpp-specific and
// llmkube-inference reads only recording rules.
//
// The panels union runtimes with PromQL `or`, which drops a right-hand series
// whose label set already exists on the left, and rate() strips __name__. That
// is safe only because charts/llmkube/templates/inference-podmonitor.yaml relabels a
// distinct `runtime` label onto every scraped inference series, so the sum is a
// true total in a namespace running more than one runtime. Drop that relabeling
// and every panel here starts undercounting.
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
