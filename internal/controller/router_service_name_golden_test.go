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
	"os"
	"path/filepath"
	"testing"

	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestRouterProxyObjectsGoldenWithoutServiceName pins every byte of the
// objects the Proxy path renders (ConfigMap, Deployment, data-plane Service,
// admin Service) and the URLs it publishes (status.endpoint and the budget
// admin URL) for a router that does not set spec.proxy.serviceName. The
// goldens were generated on the base commit of #2279, before the field
// existed, so a drift here means an operator upgrade would change, rename, or
// roll a router that never asked for a custom Service name. Regenerate with
// -update-routerproxy-goldens only when such a change is intended, and from
// the base tree.
func TestRouterProxyObjectsGoldenWithoutServiceName(t *testing.T) {
	exposed := canonicalModelRouter()
	exposed.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{
		Port:                  80,
		Path:                  "/v1",
		Type:                  "NodePort",
		NodePort:              ptr.To[int32](30801),
		ExternalTrafficPolicy: "Local",
	}
	exposed.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{
		Replicas:  ptr.To[int32](2),
		PodLabels: map[string]string{"example.com/team": "llm"},
	}

	for _, tc := range []struct {
		name string
		mr   *inferencev1alpha1.ModelRouter
	}{
		{name: "routerproxy-objects-default.golden", mr: canonicalModelRouter()},
		{name: "routerproxy-objects-endpoint-options.golden", mr: exposed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderRouterProxyObjects(t, tc.mr)

			path := filepath.Join("testdata", tc.name)
			if *updateRouterProxyGoldens {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write golden %s: %v", tc.name, err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v", tc.name, err)
			}
			if string(got) != string(want) {
				t.Errorf("%s drifted from the golden; a router without spec.proxy.serviceName "+
					"must render exactly what it did before the field existed:\ngot:\n%s\nwanted:\n%s",
					tc.name, got, want)
			}
		})
	}
}

// renderRouterProxyObjects renders the Proxy path's pure builders for mr into
// one YAML stream. The compiled config is a fixed stub: its content belongs
// to the compile tests, and only the ConfigMap's metadata is in scope here.
func renderRouterProxyObjects(t *testing.T, mr *inferencev1alpha1.ModelRouter) []byte {
	t.Helper()
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	compiled := &compiledConfig{Hash: "deadbeef", JSON: []byte(`{"stub":true}`)}

	var out []byte
	for _, obj := range []any{
		newRouterConfigMap(mr, compiled),
		r.newRouterDeployment(mr, compiled.Hash, false),
		newRouterService(mr),
		newRouterAdminService(mr),
		map[string]string{
			"statusEndpoint": routerProxyEndpoint(mr),
			"budgetEndpoint": routerProxyBudgetEndpoint(mr),
		},
	} {
		b, err := yaml.Marshal(obj)
		if err != nil {
			t.Fatalf("marshal %T: %v", obj, err)
		}
		out = append(out, []byte("---\n")...)
		out = append(out, b...)
	}
	return out
}
