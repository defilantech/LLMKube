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

package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Pyrra ServiceLevelObjective coordinates. Built as unstructured (like the
// Envoy AI Gateway resources) so the operator carries no Pyrra Go dependency
// and starts cleanly on clusters without the CRD.
const (
	pyrraGroup   = "pyrra.dev"
	pyrraVersion = "v1alpha1"
	pyrraSLOKind = "ServiceLevelObjective"

	// sloISvcLabel marks a rendered SLO with its owning InferenceService so
	// stale resources (rename, indicator switch, spec.slo removal) can be
	// listed and deleted; owner references only garbage-collect on
	// InferenceService deletion.
	sloISvcLabel = "inference.llmkube.dev/inferenceservice"
)

func pyrraSLOGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: pyrraGroup, Version: pyrraVersion, Kind: pyrraSLOKind}
}

func pyrraSLOListGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: pyrraGroup, Version: pyrraVersion, Kind: pyrraSLOKind + "List"}
}

// sloLatencyMetrics maps a runtime to the histogram series backing latency
// SLOs. Only runtimes that actually export a request-latency histogram are
// present; llama.cpp exports counters/gauges only (verified against
// tools/server/README.md upstream), so it has no entry.
var sloLatencyMetrics = map[string]struct {
	bucket string
	count  string
}{
	"vllm": {
		bucket: "vllm:e2e_request_latency_seconds_bucket",
		count:  "vllm:e2e_request_latency_seconds_count",
	},
}

// sloIndicatorSupported reports whether the runtime can back the indicator
// with real data. Availability rides scrape success (`up`), which every
// pod-backed runtime has; latency needs a histogram entry above.
func sloIndicatorSupported(runtime, indicator string) bool {
	switch indicator {
	case "availability":
		return true
	case "latency":
		_, ok := sloLatencyMetrics[runtime]
		return ok
	default:
		return false
	}
}

// sloResourceName returns spec.slo.name or the documented default
// "<inferenceservice-name>-<indicator>".
func sloResourceName(isvc *inferencev1alpha1.InferenceService) string {
	if isvc.Spec.SLO.Name != "" {
		return isvc.Spec.SLO.Name
	}
	return fmt.Sprintf("%s-%s", isvc.Name, isvc.Spec.SLO.Indicator)
}

// sloSelector are the label matchers identifying this InferenceService's
// scraped series. The PodMonitor relabelings promote the operator-managed pod
// labels to target labels, so `service` and `namespace` exist on every series
// including `up` (charts/llmkube/templates/inference-podmonitor.yaml).
func sloSelector(isvc *inferencev1alpha1.InferenceService) string {
	return fmt.Sprintf(`namespace=%q,service=%q`, isvc.Namespace, isvc.Name)
}

// newServiceLevelObjective renders the Pyrra resource for the
// InferenceService's spec.slo. Callers must have verified non-nil spec.SLO
// and checked sloIndicatorSupported first. Programmer errors (violating these
// contracts) panic with descriptive messages; API errors are impossible due to
// the enum validation on the CRD.
func newServiceLevelObjective(isvc *inferencev1alpha1.InferenceService) *unstructured.Unstructured {
	slo := isvc.Spec.SLO
	sel := sloSelector(isvc)

	var indicator map[string]interface{}
	switch slo.Indicator {
	case "latency":
		m, ok := sloLatencyMetrics[isvc.Spec.Runtime]
		if !ok {
			panic(fmt.Sprintf("newServiceLevelObjective: no latency metrics for runtime %q; caller must check sloIndicatorSupported", isvc.Spec.Runtime))
		}
		indicator = map[string]interface{}{
			"latency": map[string]interface{}{
				"success": map[string]interface{}{
					"metric": fmt.Sprintf(`%s{%s,le=%q}`, m.bucket, sel, slo.LatencyThreshold),
				},
				"total": map[string]interface{}{
					"metric": fmt.Sprintf(`%s{%s}`, m.count, sel),
				},
			},
		}
	case "availability":
		indicator = map[string]interface{}{
			"bool_gauge": map[string]interface{}{
				"metric": fmt.Sprintf(`up{%s}`, sel),
			},
		}
	default:
		panic(fmt.Sprintf("newServiceLevelObjective: unknown indicator %q; caller must check sloIndicatorSupported", slo.Indicator))
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(pyrraSLOGVK())
	u.SetName(sloResourceName(isvc))
	u.SetNamespace(isvc.Namespace)
	u.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by": "llmkube",
		sloISvcLabel:                   isvc.Name,
	})
	u.Object["spec"] = map[string]interface{}{
		"target":    slo.Objective,
		"window":    slo.Window,
		"indicator": indicator,
	}
	return u
}
