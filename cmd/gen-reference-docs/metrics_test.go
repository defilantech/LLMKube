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
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/internal/relay"
	"github.com/defilantech/llmkube/pkg/agent"
)

// A vector with no children exports nothing from Gather; describe must still
// read it, which is the whole reason the page does not just scrape registries.
func TestDescribeReadsChildlessVectors(t *testing.T) {
	tests := []struct {
		c    prometheus.Collector
		want metricFamily
	}{
		{
			prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "t_gauge", Help: "A gauge."}, []string{"zone", "app"}),
			metricFamily{name: "t_gauge", kind: "gauge", help: "A gauge.", labels: []string{"app", "zone"}},
		},
		{
			prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "t_hist", Help: "H."}, []string{"a"}),
			metricFamily{name: "t_hist", kind: "histogram", help: "H.", labels: []string{"a"}},
		},
		{
			prometheus.NewCounter(prometheus.CounterOpts{Name: "t_total", Help: "C."}),
			metricFamily{name: "t_total", kind: "counter", help: "C."},
		},
	}
	for _, tt := range tests {
		got, err := describe(tt.c)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !reflect.DeepEqual(got[0], tt.want) {
			t.Errorf("describe = %+v, want %+v", got, tt.want)
		}
	}
}

// silentCollector declares a descriptor but never emits it, like a custom
// collector the generator has no way to make produce a sample.
type silentCollector struct{ desc *prometheus.Desc }

func (s silentCollector) Describe(ch chan<- *prometheus.Desc) { ch <- s.desc }
func (silentCollector) Collect(chan<- prometheus.Metric)      {}

func TestDescribeFailsOnUnreadableCollector(t *testing.T) {
	c := silentCollector{prometheus.NewDesc("t_silent", "Never emitted.", nil, nil)}
	if _, err := describe(c); err == nil || !strings.Contains(err.Error(), "t_silent") {
		t.Fatalf("describe of a collector that emits nothing: err = %v, want one naming t_silent", err)
	}
}

func TestCheckServed(t *testing.T) {
	documented := prometheus.NewCounter(prometheus.CounterOpts{Name: "t_documented_total", Help: "D."})
	fams, err := describe(documented)
	if err != nil {
		t.Fatal(err)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(documented, collectors.NewGoCollector())
	sentence, err := checkServed(reg, fams)
	if err != nil {
		t.Fatalf("matching registry: %v", err)
	}
	if !strings.Contains(sentence, "`go_*`") || strings.Contains(sentence, "process") {
		t.Errorf("runtime sentence = %q, want the Go collector only", sentence)
	}

	extra := prometheus.NewGauge(prometheus.GaugeOpts{Name: "t_undocumented", Help: "U."})
	reg.MustRegister(extra)
	if _, err := checkServed(reg, fams); err == nil || !strings.Contains(err.Error(), "t_undocumented") {
		t.Errorf("served but undocumented: err = %v, want one naming t_undocumented", err)
	}

	if _, err := checkServed(prometheus.NewRegistry(), fams); err == nil ||
		!strings.Contains(err.Error(), "t_documented_total") {
		t.Errorf("documented but unregistered: err = %v, want one naming t_documented_total", err)
	}
}

var fqName = regexp.MustCompile(`fqName: "([^"]+)"`)

// Every family every binary registers has a row in its section. The names
// come from each collector's own descriptors, not from Gather.
func TestRenderMetricsCoversEveryCollector(t *testing.T) {
	out, err := renderMetrics()
	if err != nil {
		t.Fatal(err)
	}
	router := append(append([]prometheus.Collector{}, llmkubemetrics.RouterCollectors...), relay.Collectors...)
	sections := map[string][]prometheus.Collector{
		"## Controller":                   llmkubemetrics.ControllerCollectors,
		"## Foreman operator":             llmkubemetrics.ForemanCollectors,
		"## Router proxy and Metal relay": router,
		"## Metal agent":                  agent.AgentCollectors,
	}
	for heading, cs := range sections {
		start := strings.Index(out, heading+"\n")
		if start < 0 {
			t.Fatalf("no section %q", heading)
		}
		section := out[start+len(heading):]
		if next := strings.Index(section, "\n## "); next >= 0 {
			section = section[:next]
		}
		for _, c := range cs {
			for _, d := range describeDescs(c) {
				m := fqName.FindStringSubmatch(d.String())
				if m == nil {
					t.Fatalf("cannot read the name of %s", d)
				}
				if !strings.Contains(section, "| `"+m[1]+"` |") {
					t.Errorf("%s: no row for %s", heading, m[1])
				}
			}
		}
	}
}
