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

package v1alpha1

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
)

// TestConstants verifies the documented constant values match the spec.
func TestConstants(t *testing.T) {
	t.Run("AnnotationAgentHeartbeat", func(t *testing.T) {
		if got := AnnotationAgentHeartbeat; got != "llmkube.ai/agent-heartbeat" {
			t.Errorf("AnnotationAgentHeartbeat = %q; want %q", got, "llmkube.ai/agent-heartbeat")
		}
	})

	t.Run("AnnotationAgentVersion", func(t *testing.T) {
		if got := AnnotationAgentVersion; got != "llmkube.ai/agent-version" {
			t.Errorf("AnnotationAgentVersion = %q; want %q", got, "llmkube.ai/agent-version")
		}
	})

	t.Run("DefaultAgentHeartbeatInterval", func(t *testing.T) {
		if got := DefaultAgentHeartbeatInterval; got != 30*time.Second {
			t.Errorf("DefaultAgentHeartbeatInterval = %v; want %v", got, 30*time.Second)
		}
	})

	t.Run("DefaultAgentHeartbeatTimeout", func(t *testing.T) {
		if got := DefaultAgentHeartbeatTimeout; got != 3*time.Minute {
			t.Errorf("DefaultAgentHeartbeatTimeout = %v; want %v", got, 3*time.Minute)
		}
	})
}

// TestGroupVersion verifies the package-level GroupVersion constant.
func TestGroupVersion(t *testing.T) {
	if got := GroupVersion.Group; got != "inference.llmkube.dev" {
		t.Errorf("GroupVersion.Group = %q; want %q", got, "inference.llmkube.dev")
	}
	if got := GroupVersion.Version; got != "v1alpha1" {
		t.Errorf("GroupVersion.Version = %q; want %q", got, "v1alpha1")
	}
}

// TestAddToScheme verifies AddToScheme can be called without error and
// registers the expected types.
func TestAddToScheme(t *testing.T) {
	scheme, err := SchemeBuilder.Build()
	if err != nil {
		t.Fatalf("SchemeBuilder.Build: %v", err)
	}

	// All three root types should be registered.
	for _, tc := range []struct {
		obj  runtime.Object
		kind string
	}{
		{&Model{}, "Model"},
		{&InferenceService{}, "InferenceService"},
		{&ModelRouter{}, "ModelRouter"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			gvks, _, err := scheme.ObjectKinds(tc.obj)
			if err != nil {
				t.Fatalf("scheme.ObjectKinds(%s): %v", tc.kind, err)
			}
			if len(gvks) == 0 {
				t.Fatalf("%s not registered in scheme", tc.kind)
			}
			if gvks[0].Group != "inference.llmkube.dev" {
				t.Errorf("%s GVK group = %q; want %q", tc.kind, gvks[0].Group, "inference.llmkube.dev")
			}
			if gvks[0].Version != "v1alpha1" {
				t.Errorf("%s GVK version = %q; want %q", tc.kind, gvks[0].Version, "v1alpha1")
			}
			if gvks[0].Kind != tc.kind {
				t.Errorf("%s GVK kind = %q; want %q", tc.kind, gvks[0].Kind, tc.kind)
			}
		})
	}
}

// TestModelRouterDataPlaneConstants verifies the documented data plane
// constant values.
func TestModelRouterDataPlaneConstants(t *testing.T) {
	if got := ModelRouterDataPlaneProxy; got != "Proxy" {
		t.Errorf("ModelRouterDataPlaneProxy = %q; want %q", got, "Proxy")
	}
	if got := ModelRouterDataPlaneGateway; got != "Gateway" {
		t.Errorf("ModelRouterDataPlaneGateway = %q; want %q", got, "Gateway")
	}
}

// TestDefaultRouteStrategyConstants verifies the documented default route
// strategy constant values.
func TestDefaultRouteStrategyConstants(t *testing.T) {
	if got := DefaultRouteStrategyStatic; got != "Static" {
		t.Errorf("DefaultRouteStrategyStatic = %q; want %q", got, "Static")
	}
	if got := DefaultRouteStrategyBackendNameMatch; got != "BackendNameMatch" {
		t.Errorf("DefaultRouteStrategyBackendNameMatch = %q; want %q", got, "BackendNameMatch")
	}
}
