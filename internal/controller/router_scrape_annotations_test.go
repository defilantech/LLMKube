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
	"net"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var scrapeAnnotationKeys = []string{"prometheus.io/scrape", "prometheus.io/path", "prometheus.io/port"}

func getRouterDeployment(t *testing.T, rec *ModelRouterReconciler, mr *inferencev1alpha1.ModelRouter) *appsv1.Deployment {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := rec.Get(context.Background(), types.NamespacedName{
		Name: routerProxyResourceName(mr.Name), Namespace: mr.Namespace,
	}, dep); err != nil {
		t.Fatalf("get router Deployment: %v", err)
	}
	return dep
}

// servedMetricsPort is the port the proxy container actually serves /metrics
// on: the port of its --metrics-bind-address argument, which must also be
// the container port named "metrics".
func servedMetricsPort(t *testing.T, dep *appsv1.Deployment) string {
	t.Helper()
	c := dep.Spec.Template.Spec.Containers[0]
	var bind string
	for i, a := range c.Args {
		if a == "--metrics-bind-address" && i+1 < len(c.Args) {
			bind = c.Args[i+1]
		}
	}
	_, port, err := net.SplitHostPort(bind)
	if err != nil {
		t.Fatalf("proxy --metrics-bind-address = %q: %v", bind, err)
	}
	for _, p := range c.Ports {
		if p.Name == "metrics" {
			if got := strconv.Itoa(int(p.ContainerPort)); got != port {
				t.Fatalf("container port \"metrics\" = %s, but the proxy binds metrics on %s", got, port)
			}
			return port
		}
	}
	t.Fatalf("proxy container has no port named \"metrics\": %+v", c.Ports)
	return ""
}

// TestRouterProxyScrapeAnnotations: with --emit-scrape-annotations the
// router-proxy pod template points an annotation scraper at the listener the
// proxy serves /metrics on; without it the template carries none of the keys.
func TestRouterProxyScrapeAnnotations(t *testing.T) {
	for _, emit := range []bool{false, true} {
		t.Run("emit="+strconv.FormatBool(emit), func(t *testing.T) {
			mr := canonicalModelRouter()
			rec := newRouterReconcilerForTest(t, mr)
			rec.RouterProxyImage = "ghcr.io/test/router-proxy:v1"
			rec.EmitScrapeAnnotations = emit
			if err := rec.reconcileRouterDeployment(context.Background(), mr, &compiledConfig{Hash: "hash"}); err != nil {
				t.Fatalf("reconcileRouterDeployment: %v", err)
			}
			dep := getRouterDeployment(t, rec, mr)
			got := dep.Spec.Template.Annotations

			if !emit {
				for _, k := range scrapeAnnotationKeys {
					if v, ok := got[k]; ok {
						t.Errorf("pod template has %s=%q with the flag off", k, v)
					}
				}
				return
			}
			want := map[string]string{
				"prometheus.io/scrape": "true",
				"prometheus.io/path":   "/metrics",
				"prometheus.io/port":   servedMetricsPort(t, dep),
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("pod template %s = %q, want %q", k, got[k], v)
				}
			}
			if got[routerProxyConfigHashAnnotation] != "hash" {
				t.Errorf("config hash annotation = %q, want it kept beside the scrape annotations", got[routerProxyConfigHashAnnotation])
			}
		})
	}
}

// TestRouterProxyScrapeAnnotationsReachAnExistingDeployment: a router created
// before the flag was turned on gets the annotations on its next reconcile,
// not only routers created afterwards.
func TestRouterProxyScrapeAnnotationsReachAnExistingDeployment(t *testing.T) {
	mr := canonicalModelRouter()
	scheme := builderTestScheme(t)
	initial := (&ModelRouterReconciler{RouterProxyImage: "img"}).newRouterDeployment(mr, &compiledConfig{Hash: "hash"})
	if err := setControllerReferenceUnblocked(mr, initial, scheme); err != nil {
		t.Fatalf("set controller reference: %v", err)
	}
	for _, k := range scrapeAnnotationKeys {
		if _, ok := initial.Spec.Template.Annotations[k]; ok {
			t.Fatalf("seeded Deployment already has %s", k)
		}
	}
	rec := &ModelRouterReconciler{
		Client:                fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr, initial).Build(),
		Scheme:                scheme,
		RouterProxyImage:      "img",
		EmitScrapeAnnotations: true,
	}
	if err := rec.reconcileRouterDeployment(context.Background(), mr, &compiledConfig{Hash: "hash"}); err != nil {
		t.Fatalf("reconcileRouterDeployment: %v", err)
	}
	dep := getRouterDeployment(t, rec, mr)
	if got, want := dep.Spec.Template.Annotations["prometheus.io/port"], servedMetricsPort(t, dep); got != want {
		t.Errorf("existing router's prometheus.io/port = %q after reconcile, want %q", got, want)
	}
	if got := dep.Spec.Template.Annotations["prometheus.io/scrape"]; got != "true" {
		t.Errorf("existing router's prometheus.io/scrape = %q after reconcile, want true", got)
	}
}

// TestRouterProxyScrapeAnnotationsLeaveWithTheFlag: turning the flag off
// removes the annotations it added from an existing router, so a router moved
// to routerPodMonitor is not scraped twice. A value the operator would not
// have set is someone else's and stays.
func TestRouterProxyScrapeAnnotationsLeaveWithTheFlag(t *testing.T) {
	mr := canonicalModelRouter()
	scheme := builderTestScheme(t)
	initial := (&ModelRouterReconciler{RouterProxyImage: "img", EmitScrapeAnnotations: true}).
		newRouterDeployment(mr, &compiledConfig{Hash: "hash"})
	initial.Spec.Template.Annotations["prometheus.io/path"] = "/custom-metrics"
	if err := setControllerReferenceUnblocked(mr, initial, scheme); err != nil {
		t.Fatalf("set controller reference: %v", err)
	}
	rec := &ModelRouterReconciler{
		Client:           fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr, initial).Build(),
		Scheme:           scheme,
		RouterProxyImage: "img",
	}
	if err := rec.reconcileRouterDeployment(context.Background(), mr, &compiledConfig{Hash: "hash"}); err != nil {
		t.Fatalf("reconcileRouterDeployment: %v", err)
	}
	got := getRouterDeployment(t, rec, mr).Spec.Template.Annotations
	for _, k := range []string{"prometheus.io/scrape", "prometheus.io/port"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s=%q kept after the flag was turned off", k, v)
		}
	}
	if got["prometheus.io/path"] != "/custom-metrics" {
		t.Errorf("prometheus.io/path = %q, want the value the operator did not set kept", got["prometheus.io/path"])
	}
}
