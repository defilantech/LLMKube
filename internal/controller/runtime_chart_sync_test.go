package controller

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	runtimeTypesPath  = "../../api/v1alpha1/inferenceservice_types.go"
	prometheusRuleTpl = "../../charts/llmkube/templates/prometheusrule.yaml"
)

// runtimeEnumValues returns the runtimes the CRD accepts. The kubebuilder Enum
// marker is the one place a new runtime must be declared for the API to accept
// it, so it is the source of truth for "every runtime that can exist".
func runtimeEnumValues(t *testing.T) []string {
	t.Helper()

	src, err := os.ReadFile(runtimeTypesPath)
	if err != nil {
		t.Fatalf("read %s: %v", runtimeTypesPath, err)
	}

	lines := strings.Split(string(src), "\n")
	field := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "Runtime string ") {
			field = i
			break
		}
	}
	if field < 0 {
		t.Fatalf("no `Runtime string` field in %s", runtimeTypesPath)
	}

	// Nearest marker above the field: the file holds several Enums.
	const marker = "// +kubebuilder:validation:Enum="
	for i := field; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, marker) {
			return strings.Split(strings.TrimPrefix(trimmed, marker), ";")
		}
	}
	t.Fatalf("no Enum marker above the Runtime field in %s", runtimeTypesPath)
	return nil
}

// chartRestartRuleContainers returns the container names the chart's
// llmkube:inference:restarts:rate5m recording rule selects.
func chartRestartRuleContainers(t *testing.T) []string {
	t.Helper()

	tpl, err := os.ReadFile(prometheusRuleTpl)
	if err != nil {
		t.Fatalf("read %s: %v", prometheusRuleTpl, err)
	}

	m := regexp.MustCompile(`container=~"([^"]+)"`).FindSubmatch(tpl)
	if m == nil {
		t.Fatalf("no container=~\"...\" matcher in %s", prometheusRuleTpl)
	}
	return strings.Split(string(m[1]), "|")
}

// TestChartRestartRuleCoversEveryBackend keeps the chart's restart recording
// rule in step with the Go backends. It shipped matching runtime names instead
// of container names, and nothing failed: a rule that selects nothing looks
// like a cluster with no restarts (#1223). Set equality catches both a new
// backend missing from the chart and a stale name left behind.
func TestChartRestartRuleCoversEveryBackend(t *testing.T) {
	// "" is a valid spec.Runtime and resolves to llamacpp.
	want := map[string]bool{}
	for _, runtime := range append(runtimeEnumValues(t), "") {
		isvc := &inferencev1alpha1.InferenceService{}
		isvc.Spec.Runtime = runtime
		want[resolveBackend(isvc).ContainerName()] = true
	}

	got := map[string]bool{}
	for _, container := range chartRestartRuleContainers(t) {
		got[container] = true
	}

	for name := range want {
		if !got[name] {
			t.Errorf("container %q is a backend ContainerName() but is not selected by the chart restart rule in %s", name, prometheusRuleTpl)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("container %q is selected by the chart restart rule in %s but no backend returns it", name, prometheusRuleTpl)
		}
	}

	if t.Failed() {
		t.Logf("backends: %v", sortedKeys(want))
		t.Logf("chart:    %v", sortedKeys(got))
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
