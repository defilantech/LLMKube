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
	"reflect"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/internal/relay"
	"github.com/defilantech/llmkube/pkg/agent"
)

// maxLabels bounds the search for a vector's label count. The widest
// llmkube vector has five labels.
const maxLabels = 32

// metricsComponent is one binary's set of collectors. served, when set, is
// the registry the binary actually serves: the page is checked against it in
// both directions, so a collector registered there but missing from the list,
// or listed but never registered, fails the generator instead of the page
// silently disagreeing with /metrics.
type metricsComponent struct {
	title, intro string
	collectors   []prometheus.Collector
	served       prometheus.Gatherer
	trailer      string
}

type metricFamily struct {
	name, kind, help string
	labels           []string
}

func metricsComponents() []metricsComponent {
	return []metricsComponent{
		{
			title: "Controller",
			intro: "The operator's controller manager registers `ControllerCollectors` from " +
				sourceLink("internal/metrics/metrics.go") + " in controller-runtime's registry (" +
				sourceLink("cmd/main.go") + "), and serves them on its metrics endpoint.",
			collectors: llmkubemetrics.ControllerCollectors,
			trailer:    controllerRuntimeTrailer,
		},
		{
			title: "Foreman operator",
			intro: "The Foreman operator registers `ForemanCollectors` from " +
				sourceLink("internal/metrics/metrics.go") + " in controller-runtime's registry (" +
				sourceLink("cmd/foreman-operator/main.go") + ").",
			collectors: llmkubemetrics.ForemanCollectors,
			trailer:    controllerRuntimeTrailer,
		},
		{
			title: "Router proxy and Metal relay",
			intro: "The router-proxy binary serves `RouterRegistry` (" + sourceLink("cmd/router-proxy/main.go") +
				"). It holds `RouterCollectors` from " + sourceLink("internal/metrics/metrics.go") +
				" and the relay's `Collectors` from " + sourceLink("internal/relay/relay.go") +
				": the same binary runs the relay in front of a Metal InferenceService.",
			collectors: append(append([]prometheus.Collector{}, llmkubemetrics.RouterCollectors...), relay.Collectors...),
			served:     llmkubemetrics.RouterRegistry,
			trailer: "Besides these, it serves controller-runtime's `rest_client_*` families, " +
				"which count the proxy's own Kubernetes API calls.",
		},
		{
			title: "Metal agent",
			intro: "The Metal agent serves `AgentRegistry` at `GET /metrics` (" + sourceLink("pkg/agent/health.go") +
				"). It holds `AgentCollectors` from " + sourceLink("pkg/agent/agentmetrics.go") + ".",
			collectors: agent.AgentCollectors,
			served:     agent.AgentRegistry,
		},
	}
}

const controllerRuntimeTrailer = "The same endpoint serves controller-runtime's own families " +
	"(`controller_runtime_*`, `workqueue_*`, `rest_client_*`, `leader_election_*`, `certwatcher_*`) " +
	"and the Go runtime and process collectors (`go_*`, `process_*`), which controller-runtime documents."

func renderMetrics() (string, error) {
	var b strings.Builder
	b.WriteString(pageHeader("Metrics",
		"Every Prometheus metric the LLMKube controller, Foreman operator, router proxy, relay and Metal agent export.",
		"the collectors each binary registers"))
	b.WriteString("Every family is listed, including a labeled family that has no series until its first event. " +
		"Labels are listed in alphabetical order.\n\n")

	for _, comp := range metricsComponents() {
		families, err := describeAll(comp.collectors)
		if err != nil {
			return "", fmt.Errorf("%s: %w", comp.title, err)
		}
		trailer := comp.trailer
		if comp.served != nil {
			runtime, err := checkServed(comp.served, families)
			if err != nil {
				return "", fmt.Errorf("%s: %w", comp.title, err)
			}
			trailer = joinSentences(runtime, trailer)
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", comp.title, comp.intro)
		b.WriteString(tableHeader("Metric", "Type", "Labels", "Description"))
		for _, f := range families {
			labels := make([]string, len(f.labels))
			for i, l := range f.labels {
				labels[i] = codeSpan(l)
			}
			b.WriteString(tableRow(codeSpan(f.name), f.kind, strings.Join(labels, ", "), tableCell(f.help)))
		}
		b.WriteString("\n")
		if trailer != "" {
			b.WriteString(trailer + "\n\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

func joinSentences(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + " " + b
}

// describeAll reads every family the collectors export, sorted by name. A
// collector listed twice in one component (none is today) is read once.
func describeAll(cs []prometheus.Collector) ([]metricFamily, error) {
	seen := map[string]bool{}
	var out []metricFamily
	for _, c := range cs {
		fams, err := describe(c)
		if err != nil {
			return nil, err
		}
		for _, f := range fams {
			if !seen[f.name] {
				seen[f.name] = true
				out = append(out, f)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// describe gathers c alone from a fresh registry. A vector exports nothing
// until it has a child, so one is created first; the generator is a
// throwaway process, so the extra series it leaves on the shared collector
// reach no scrape. Every descriptor c declares must come back as a family,
// or the collector is reported rather than skipped.
func describe(c prometheus.Collector) ([]metricFamily, error) {
	descs := describeDescs(c)
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		return nil, fmt.Errorf("register %s: %w", descNames(descs), err)
	}
	if err := ensureChild(c); err != nil {
		return nil, fmt.Errorf("%s: %w", descNames(descs), err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		return nil, fmt.Errorf("gather %s: %w", descNames(descs), err)
	}
	if len(mfs) != len(descs) {
		return nil, fmt.Errorf("%s declares %d descriptors but gathered %d families; "+
			"the generator cannot read this collector type", descNames(descs), len(descs), len(mfs))
	}
	out := make([]metricFamily, 0, len(mfs))
	for _, mf := range mfs {
		out = append(out, familyOf(mf))
	}
	return out, nil
}

func familyOf(mf *dto.MetricFamily) metricFamily {
	f := metricFamily{
		name: mf.GetName(),
		kind: strings.ToLower(strings.ReplaceAll(mf.GetType().String(), "_", " ")),
		help: mf.GetHelp(),
	}
	if ms := mf.GetMetric(); len(ms) > 0 {
		for _, l := range ms[0].GetLabel() {
			f.labels = append(f.labels, l.GetName())
		}
	}
	return f
}

func describeDescs(c prometheus.Collector) []*prometheus.Desc {
	ch := make(chan *prometheus.Desc)
	go func() {
		c.Describe(ch)
		close(ch)
	}()
	var descs []*prometheus.Desc
	for d := range ch {
		descs = append(descs, d)
	}
	return descs
}

func descNames(descs []*prometheus.Desc) string {
	names := make([]string, len(descs))
	for i, d := range descs {
		names[i] = d.String()
	}
	return strings.Join(names, ", ")
}

// ensureChild gives a vector one child. The *Vec types share no interface
// with a typed GetMetricWithLabelValues, so the method is found by name; the
// label count is the first one it accepts. A collector without the method
// (a plain counter or gauge) needs no child.
func ensureChild(c prometheus.Collector) error {
	m := reflect.ValueOf(c).MethodByName("GetMetricWithLabelValues")
	if !m.IsValid() {
		return nil
	}
	for n := 0; n <= maxLabels; n++ {
		out := m.CallSlice([]reflect.Value{reflect.ValueOf(make([]string, n))})
		if errv := out[len(out)-1]; errv.IsNil() {
			return nil
		}
	}
	return fmt.Errorf("no label count up to %d is accepted", maxLabels)
}

// checkServed compares the families a binary's registry serves with the ones
// its collector list documents, and reports the runtime collectors it serves
// besides, as one sentence. describe has already given every listed vector a
// child, and those are the same collector values the registry holds, so a
// listed family missing from the gather was never registered there.
func checkServed(served prometheus.Gatherer, documented []metricFamily) (string, error) {
	mfs, err := served.Gather()
	if err != nil {
		return "", fmt.Errorf("gather the served registry: %w", err)
	}
	want := map[string]bool{}
	for _, f := range documented {
		want[f.name] = true
	}
	var goRuntime, process bool
	for _, mf := range mfs {
		name := mf.GetName()
		switch {
		case want[name]:
			delete(want, name)
		case strings.HasPrefix(name, "go_"):
			goRuntime = true
		case strings.HasPrefix(name, "process_"):
			process = true
		default:
			return "", fmt.Errorf("%s is served but missing from the collector list the page is generated from", name)
		}
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for name := range want {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return "", fmt.Errorf("listed but not registered in the served registry: %s", strings.Join(missing, ", "))
	}
	switch {
	case goRuntime && process:
		return "The endpoint also serves the Go runtime and process collectors (`go_*`, `process_*`).", nil
	case goRuntime:
		return "The endpoint also serves the Go runtime collector (`go_*`).", nil
	case process:
		return "The endpoint also serves the process collector (`process_*`).", nil
	}
	return "", nil
}
