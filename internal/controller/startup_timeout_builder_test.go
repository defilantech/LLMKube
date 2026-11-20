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
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestConstructDeployment_StartupTimeout pins spec.startupTimeout across every
// runtime: it raises only the startup probe's failureThreshold, the handler and
// period stay the runtime's own (so a port or health path set elsewhere is not
// restated), and probes stay byte-identical when the field is unset.
func TestConstructDeployment_StartupTimeout(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Format: "gguf", Source: "s3://bucket/m.gguf"},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: "k"},
	}
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	build := func(runtime string, timeout *metav1.Duration) corev1.Container {
		t.Helper()
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:       "target",
				Runtime:        runtime,
				StartupTimeout: timeout,
			},
		}
		dep := r.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "")
		if len(dep.Spec.Template.Spec.Containers) == 0 {
			t.Fatalf("%s: no container in the deployment", runtime)
		}
		return dep.Spec.Template.Spec.Containers[0]
	}
	d90 := &metav1.Duration{Duration: 90 * time.Minute}

	for _, runtime := range []string{
		"llamacpp", RuntimeVLLM, RuntimeSGLANG, "tgi", RuntimeLlamaCppRouter,
		"generic", "personaplex",
	} {
		t.Run(runtime, func(t *testing.T) {
			base, raised := build(runtime, nil), build(runtime, d90)
			if base.StartupProbe == nil || raised.StartupProbe == nil {
				t.Fatalf("startup probe missing: %+v / %+v", base.StartupProbe, raised.StartupProbe)
			}
			if base.StartupProbe.FailureThreshold != 180 {
				t.Errorf("unset startupTimeout failureThreshold = %d, want the built-in 180",
					base.StartupProbe.FailureThreshold)
			}
			if raised.StartupProbe.FailureThreshold != 540 {
				t.Errorf("90m failureThreshold = %d, want 540", raised.StartupProbe.FailureThreshold)
			}
			if !reflect.DeepEqual(base.StartupProbe.ProbeHandler, raised.StartupProbe.ProbeHandler) {
				t.Errorf("%s handler changed: %+v -> %+v", runtime,
					base.StartupProbe.ProbeHandler, raised.StartupProbe.ProbeHandler)
			}
			if base.StartupProbe.PeriodSeconds != raised.StartupProbe.PeriodSeconds {
				t.Errorf("period changed: %d -> %d",
					base.StartupProbe.PeriodSeconds, raised.StartupProbe.PeriodSeconds)
			}
			if !reflect.DeepEqual(base.LivenessProbe, raised.LivenessProbe) {
				t.Errorf("liveness probe changed: %+v -> %+v", base.LivenessProbe, raised.LivenessProbe)
			}
			if !reflect.DeepEqual(base.ReadinessProbe, raised.ReadinessProbe) {
				t.Errorf("readiness probe changed: %+v -> %+v", base.ReadinessProbe, raised.ReadinessProbe)
			}
		})
	}

	t.Run("probeOverrides.startup wins over startupTimeout", func(t *testing.T) {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:       "target",
				Runtime:        RuntimeVLLM,
				StartupTimeout: d90,
				ProbeOverrides: &inferencev1alpha1.ProbeOverrides{
					Startup: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/custom"},
						},
						PeriodSeconds:    10,
						FailureThreshold: 42,
					},
				},
			},
		}
		c := r.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec.Containers[0]
		if c.StartupProbe.FailureThreshold != 42 {
			t.Errorf("failureThreshold = %d, want the override's 42", c.StartupProbe.FailureThreshold)
		}
		if c.StartupProbe.HTTPGet == nil || c.StartupProbe.HTTPGet.Path != "/custom" {
			t.Errorf("handler = %+v, want the override's /custom", c.StartupProbe.ProbeHandler)
		}
	})

	t.Run("a zero timeout leaves the probe alone", func(t *testing.T) {
		// An object stored before the CEL rule existed can carry 0s; it must
		// not turn into a floor of one period.
		c := build(RuntimeVLLM, &metav1.Duration{Duration: 0})
		if c.StartupProbe.FailureThreshold != 180 {
			t.Errorf("failureThreshold = %d, want the built-in 180", c.StartupProbe.FailureThreshold)
		}
	})

	t.Run("a backend with its own period", func(t *testing.T) {
		orig := resolveBackend
		resolveBackend = func(*inferencev1alpha1.InferenceService) RuntimeBackend {
			return &slowPeriodBackend{}
		}
		defer func() { resolveBackend = orig }()
		c := build("slow", d90)
		if c.StartupProbe.PeriodSeconds != 30 || c.StartupProbe.FailureThreshold != 180 {
			t.Errorf("probe = %+v, want the backend's 30s period covering 90m in 180 failures", c.StartupProbe)
		}
	})

	t.Run("a backend with no startup probe does not panic", func(t *testing.T) {
		orig := resolveBackend
		resolveBackend = func(*inferencev1alpha1.InferenceService) RuntimeBackend {
			return &archAwareBackend{}
		}
		defer func() { resolveBackend = orig }()
		c := build("arch-aware", d90)
		if c.StartupProbe != nil {
			t.Errorf("startup probe = %+v, want none", c.StartupProbe)
		}
	})
}

// slowPeriodBackend is a RuntimeBackend whose startup probe runs on a 30s
// period, to prove startupTimeout divides by the probe's period, not a constant.
type slowPeriodBackend struct{ archAwareBackend }

func (b *slowPeriodBackend) BuildProbes(int32) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
	return &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health"}},
		PeriodSeconds:    30,
		TimeoutSeconds:   5,
		FailureThreshold: 60,
	}, nil, nil
}
