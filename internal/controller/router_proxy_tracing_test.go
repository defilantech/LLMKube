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
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var testTracingEnv = []corev1.EnvVar{
	{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://tempo.monitoring.svc:4317"},
	{Name: "OTEL_TRACES_SAMPLER", Value: "parentbased_traceidratio"},
}

func otelEnv(env []corev1.EnvVar) []corev1.EnvVar {
	var out []corev1.EnvVar
	for _, e := range env {
		if strings.HasPrefix(e.Name, "OTEL_") {
			out = append(out, e)
		}
	}
	return out
}

// TestRouterProxyTracingEnv: the controller's trace export settings reach the
// router-proxy container it renders, pooled or not, without displacing the
// env the proxy already gets; with none configured the container carries no
// OTEL_* variable, so the proxy's tracing stays off.
func TestRouterProxyTracingEnv(t *testing.T) {
	for _, hasPools := range []bool{false, true} {
		t.Run("hasPools="+strconv.FormatBool(hasPools), func(t *testing.T) {
			mr := canonicalModelRouter()
			rec := newRouterReconcilerForTest(t, mr)
			rec.RouterProxyImage = "img"
			rec.RouterProxyTracingEnv = testTracingEnv
			if err := rec.reconcileRouterDeployment(context.Background(), mr, "hash", hasPools); err != nil {
				t.Fatalf("reconcileRouterDeployment: %v", err)
			}
			env := getRouterDeployment(t, rec, mr).Spec.Template.Spec.Containers[0].Env
			if got := otelEnv(env); !reflect.DeepEqual(got, testTracingEnv) {
				t.Errorf("proxy OTEL_* env = %+v, want %+v", got, testTracingEnv)
			}
			if envValue(env, "ROUTER_NAME") != mr.Name {
				t.Errorf("proxy lost ROUTER_NAME beside the tracing env: %+v", env)
			}
		})
	}

	t.Run("not configured", func(t *testing.T) {
		mr := canonicalModelRouter()
		rec := newRouterReconcilerForTest(t, mr)
		rec.RouterProxyImage = "img"
		if err := rec.reconcileRouterDeployment(context.Background(), mr, "hash", false); err != nil {
			t.Fatalf("reconcileRouterDeployment: %v", err)
		}
		env := getRouterDeployment(t, rec, mr).Spec.Template.Spec.Containers[0].Env
		if got := otelEnv(env); got != nil {
			t.Errorf("proxy OTEL_* env = %+v with tracing not configured, want none", got)
		}
	})
}

// TestRouterProxyTracingEnvReachesAnExistingDeployment: a router created before
// the controller had tracing configured gets the env on its next reconcile,
// which is the path an operator upgrade or a chart values change takes.
func TestRouterProxyTracingEnvReachesAnExistingDeployment(t *testing.T) {
	mr := canonicalModelRouter()
	scheme := builderTestScheme(t)
	initial := (&ModelRouterReconciler{RouterProxyImage: "img"}).newRouterDeployment(mr, "hash", false)
	if err := setControllerReferenceUnblocked(mr, initial, scheme); err != nil {
		t.Fatalf("set controller reference: %v", err)
	}
	if got := otelEnv(initial.Spec.Template.Spec.Containers[0].Env); got != nil {
		t.Fatalf("seeded Deployment already has OTEL_* env: %+v", got)
	}
	rec := &ModelRouterReconciler{
		Client:                fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr, initial).Build(),
		Scheme:                scheme,
		RouterProxyImage:      "img",
		RouterProxyTracingEnv: testTracingEnv,
	}
	if err := rec.reconcileRouterDeployment(context.Background(), mr, "hash", false); err != nil {
		t.Fatalf("reconcileRouterDeployment: %v", err)
	}
	env := getRouterDeployment(t, rec, mr).Spec.Template.Spec.Containers[0].Env
	if got := otelEnv(env); !reflect.DeepEqual(got, testTracingEnv) {
		t.Errorf("existing router's OTEL_* env after reconcile = %+v, want %+v", got, testTracingEnv)
	}
}
