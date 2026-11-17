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
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// sglangMetricsBody renders the SGLang exposition shape measured on v0.5.15:
// labeled series, the running gauge, the kv-used gauge and three counters.
func sglangMetricsBody(running, kvUsed, realtime, gen, prompt float64) string {
	return fmt.Sprintf(`# HELP sglang:num_running_reqs The number of running requests.
# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs{model_name="q"} %g
# TYPE sglang:kv_used_tokens gauge
sglang:kv_used_tokens{model_name="q"} %g
# TYPE sglang:realtime_tokens_total counter
sglang:realtime_tokens_total{model_name="q",mode="prefill_compute"} %g
# TYPE sglang:generation_tokens_total counter
sglang:generation_tokens_total{model_name="q"} %g
# TYPE sglang:prompt_tokens_total counter
sglang:prompt_tokens_total{model_name="q"} %g
`, running, kvUsed, realtime, gen, prompt)
}

func runSGLangStallProbe(t *testing.T, port int32, stateDir string) int {
	t.Helper()
	return runStallProbeCmd(t, sglangStallProbeCommand(port, 1), stateDir)
}

// TestSGLangStallProbeIdleNeverFails: no running request and no KV in use
// means idle, however long everything has been flat.
func TestSGLangStallProbeIdleNeverFails(t *testing.T) {
	port, _ := metricsStub(t, sglangMetricsBody(0, 0, 5000, 10, 4000))
	dir := t.TempDir()
	if code := runSGLangStallProbe(t, port, dir); code != 0 {
		t.Fatalf("first idle probe = exit %d, want 0", code)
	}
	advanceStallClock(t, dir, 2*time.Second)
	if code := runSGLangStallProbe(t, port, dir); code != 0 {
		t.Fatalf("idle probe after a long quiet period = exit %d, want 0", code)
	}
}

// TestSGLangStallProbeLongPrefillDoesNotFail models the measured prefill:
// num_running_reqs stays 0, the prompt and generation counters stay flat,
// kv_used_tokens and realtime_tokens_total climb per chunk. Across a span
// longer than the window the probe must stay green.
func TestSGLangStallProbeLongPrefillDoesNotFail(t *testing.T) {
	port, set := metricsStub(t, sglangMetricsBody(0, 32768, 16384, 10, 4000))
	dir := t.TempDir()
	if code := runSGLangStallProbe(t, port, dir); code != 0 {
		t.Fatalf("first probe = exit %d, want 0", code)
	}
	for i := 1; i <= 5; i++ {
		advanceStallClock(t, dir, 450*time.Millisecond)
		set(sglangMetricsBody(0, float64(32768+16384*i), float64(16384*(i+1)), 10, 4000))
		if code := runSGLangStallProbe(t, port, dir); code != 0 {
			t.Fatalf("probe during a long prefill (chunk %d) = exit %d, want 0", i, code)
		}
	}
}

// TestSGLangStallProbeHeartbeatAloneKeepsGreen: realtime_tokens_total
// advancing with kv_used_tokens flat (decode reusing allocated blocks) is
// still progress.
func TestSGLangStallProbeHeartbeatAloneKeepsGreen(t *testing.T) {
	port, set := metricsStub(t, sglangMetricsBody(1, 40000, 100, 10, 4000))
	dir := t.TempDir()
	if code := runSGLangStallProbe(t, port, dir); code != 0 {
		t.Fatalf("first probe = exit %d, want 0", code)
	}
	for i := 1; i <= 5; i++ {
		advanceStallClock(t, dir, 450*time.Millisecond)
		set(sglangMetricsBody(1, 40000, float64(100+120*i), 10, 4000))
		if code := runSGLangStallProbe(t, port, dir); code != 0 {
			t.Fatalf("probe with only the heartbeat advancing = exit %d, want 0", code)
		}
	}
}

// TestSGLangStallProbeWedgeMidPrefillFails is the SGLang-specific case: a
// wedge during prefill shows num_running_reqs 0 with KV in use, so a
// detector keyed on the running gauge alone would call it idle forever.
func TestSGLangStallProbeWedgeMidPrefillFails(t *testing.T) {
	port, _ := metricsStub(t, sglangMetricsBody(0, 98304, 81920, 10, 4000))
	dir := t.TempDir()
	if code := runSGLangStallProbe(t, port, dir); code != 0 {
		t.Fatalf("first probe = exit %d, want 0", code)
	}
	advanceStallClock(t, dir, 2*time.Second)
	if code := runSGLangStallProbe(t, port, dir); code == 0 {
		t.Fatalf("probe with KV in use and every signal flat past the window = exit 0, want non-zero (wedge mid-prefill)")
	}
}

// TestSGLangStallProbeWedgeMidDecodeFails: a running request with every
// signal flat past the window is a stall.
func TestSGLangStallProbeWedgeMidDecodeFails(t *testing.T) {
	port, _ := metricsStub(t, sglangMetricsBody(3, 120000, 500000, 900, 60000))
	dir := t.TempDir()
	if code := runSGLangStallProbe(t, port, dir); code != 0 {
		t.Fatalf("first probe = exit %d, want 0", code)
	}
	advanceStallClock(t, dir, 400*time.Millisecond)
	if code := runSGLangStallProbe(t, port, dir); code != 0 {
		t.Fatalf("frozen probe inside the window = exit %d, want 0", code)
	}
	advanceStallClock(t, dir, 2*time.Second)
	if code := runSGLangStallProbe(t, port, dir); code == 0 {
		t.Fatalf("frozen probe past the window = exit 0, want non-zero")
	}
}

// TestSGLangStallProbeMissingHeartbeatFailsOpen: without
// realtime_tokens_total the detector cannot tell a long prefill from a
// wedge, so it must not restart.
func TestSGLangStallProbeMissingHeartbeatFailsOpen(t *testing.T) {
	body := strings.Join(nonMatchingLines(sglangMetricsBody(2, 50000, 0, 10, 4000), "sglang:realtime_tokens_total"), "\n")
	port, _ := metricsStub(t, body)
	dir := t.TempDir()
	if code := runSGLangStallProbe(t, port, dir); code != 0 {
		t.Fatalf("first probe = exit %d, want 0", code)
	}
	advanceStallClock(t, dir, 2*time.Second)
	if code := runSGLangStallProbe(t, port, dir); code != 0 {
		t.Fatalf("frozen probe without the heartbeat = exit %d, want 0 (fail open)", code)
	}
}

func nonMatchingLines(body, metric string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, metric) {
			out = append(out, line)
		}
	}
	return out
}

// TestSGLangCustomizeProbes pins the wiring: off leaves today's probes
// byte-identical; on swaps only the liveness probe for the exact rendered
// SGLang command.
func TestSGLangCustomizeProbes(t *testing.T) {
	const port = int32(30000)
	enabled, disabled := true, false
	custom := int32(90)
	tests := []struct {
		name       string
		cfg        *inferencev1alpha1.SGLangConfig
		wantExec   bool
		wantWindow int32
	}{
		{name: "no sglangConfig", cfg: nil},
		{name: "no stallDetection", cfg: &inferencev1alpha1.SGLangConfig{}},
		{name: "enabled nil", cfg: &inferencev1alpha1.SGLangConfig{StallDetection: &inferencev1alpha1.SGLangStallDetection{}}},
		{name: "enabled false", cfg: &inferencev1alpha1.SGLangConfig{StallDetection: &inferencev1alpha1.SGLangStallDetection{Enabled: &disabled}}},
		{name: "enabled default window", cfg: &inferencev1alpha1.SGLangConfig{StallDetection: &inferencev1alpha1.SGLangStallDetection{Enabled: &enabled}}, wantExec: true, wantWindow: 120},
		{name: "enabled custom window", cfg: &inferencev1alpha1.SGLangConfig{StallDetection: &inferencev1alpha1.SGLangStallDetection{Enabled: &enabled, WindowSeconds: &custom}}, wantExec: true, wantWindow: 90},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &SGLangBackend{}
			baseStartup, baseLiveness, baseReadiness := b.BuildProbes(port)
			startup, liveness, readiness := b.BuildProbes(port)
			isvc := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "sglang", SGLangConfig: tc.cfg}}
			b.CustomizeProbes(isvc, port, startup, liveness, readiness)

			if !reflect.DeepEqual(startup, baseStartup) || !reflect.DeepEqual(readiness, baseReadiness) {
				t.Errorf("startup or readiness probe changed; only liveness may change")
			}
			if !tc.wantExec {
				if !reflect.DeepEqual(liveness, baseLiveness) {
					t.Errorf("liveness probe changed with stall detection off: %+v", liveness)
				}
				return
			}
			if liveness.Exec == nil || liveness.HTTPGet != nil {
				t.Fatalf("liveness probe = %+v, want an exec probe only", liveness)
			}
			if want := sglangStallProbeCommand(port, tc.wantWindow); !reflect.DeepEqual(liveness.Exec.Command, want) {
				t.Errorf("exec command differs from the rendered sglangStallProbeCommand")
			}
			script := liveness.Exec.Command[2]
			for _, want := range []string{"localhost:30000/metrics", "sglang:realtime_tokens_total", "sglang:kv_used_tokens", fmt.Sprintf("WINDOW = %d", tc.wantWindow)} {
				if !strings.Contains(script, want) {
					t.Errorf("rendered SGLang stall script missing %q", want)
				}
			}
			if strings.Contains(script, "vllm:") {
				t.Errorf("SGLang stall script references vLLM metrics")
			}
		})
	}
}

// TestConstructDeployment_SGLangStallLivenessAndOverrides renders through
// the real deployment path and pins that probeOverrides.liveness wins.
func TestConstructDeployment_SGLangStallLivenessAndOverrides(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Format: "safetensors", Source: "pvc://models/m"},
	}
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	enabled := true
	newISvc := func() *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "target",
				Runtime:  "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					StallDetection: &inferencev1alpha1.SGLangStallDetection{Enabled: &enabled},
				},
			},
		}
	}
	container := func(dep *appsv1.Deployment) corev1.Container {
		for _, c := range dep.Spec.Template.Spec.Containers {
			if c.Name == "sglang" {
				return c
			}
		}
		t.Fatal("sglang container not found in deployment")
		return corev1.Container{}
	}

	t.Run("stall detection renders the exec liveness probe", func(t *testing.T) {
		c := container(r.deploymentFor(newISvc(), model, resolveEffectiveModelCache(newISvc(), model), nil, 1, "", ""))
		if c.LivenessProbe == nil || c.LivenessProbe.Exec == nil {
			t.Fatalf("liveness probe = %+v, want an exec probe", c.LivenessProbe)
		}
	})
	t.Run("probeOverrides.liveness still wins", func(t *testing.T) {
		isvc := newISvc()
		isvc.Spec.ProbeOverrides = &inferencev1alpha1.ProbeOverrides{
			Liveness: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/custom-health"}}},
		}
		c := container(r.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", ""))
		if c.LivenessProbe.Exec != nil || c.LivenessProbe.HTTPGet == nil || c.LivenessProbe.HTTPGet.Path != "/custom-health" {
			t.Fatalf("liveness probe = %+v, want the user's /custom-health override", c.LivenessProbe)
		}
	})
}
