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
	"strings"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// shadowBuilderRouter is weightedBuilderRouter with its third rule switched to
// shadow: local-qwen serves, local-gemma receives copies (#2327).
func shadowBuilderRouter() (*inferencev1alpha1.ModelRouter, []*inferencev1alpha1.InferenceService) {
	mr, isvcs := weightedBuilderRouter(nil, nil)
	mr.Spec.Rules[2].Route.Strategy = shadowStrategy
	return mr, isvcs
}

// TestCompileRouterConfigRejectsPooledShadowTarget: a copy to a ModelPool
// member would have to swap it into the pool's GPU slot, so the compile fails
// with a message naming the rule and the pool. A pooled backend that serves
// (listed first) is fine.
func TestCompileRouterConfigRejectsPooledShadowTarget(t *testing.T) {
	mr, isvcs := shadowBuilderRouter()
	pool := modelPoolFor("heavy-slot", "gemma-judge", "other-model")
	r := newRouterReconcilerForTest(t, mr, isvcs[0], isvcs[1], anthropicKeySecret(), pool)
	_, err := r.compileRouterConfig(context.Background(), mr)
	if err == nil {
		t.Fatal("compileRouterConfig succeeded, want an error for a pooled shadow target")
	}
	for _, want := range []string{"split", "local-gemma", "heavy-slot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	mr, isvcs = shadowBuilderRouter()
	pool = modelPoolFor("heavy-slot", "qwen3-coder", "other-model")
	r = newRouterReconcilerForTest(t, mr, isvcs[0], isvcs[1], anthropicKeySecret(), pool)
	if _, err := r.compileRouterConfig(context.Background(), mr); err != nil {
		t.Errorf("pooled serving backend: %v, want nil", err)
	}
}

func TestValidateModelRouterShadowRule(t *testing.T) {
	for _, tc := range []struct {
		backends []string
		wantErr  bool
	}{
		{backends: []string{testRouterLocalBackend, testRouterCloudBackend}},
		{backends: []string{testRouterLocalBackend}, wantErr: true},
	} {
		mr := validRouter()
		mr.Spec.Rules = append(mr.Spec.Rules, inferencev1alpha1.RouterRule{
			Name:  "mirror",
			Route: inferencev1alpha1.RuleRoute{Backends: tc.backends, Strategy: shadowStrategy},
		})
		var found bool
		for _, e := range validateModelRouter(mr) {
			if e.Field == "spec.rules[2].route.backends" && strings.Contains(e.Message, "at least two backends") {
				found = true
				continue
			}
			t.Errorf("%v: unexpected validation error: %s", tc.backends, e)
		}
		if found != tc.wantErr {
			t.Errorf("%v: shadow rule rejected = %v, want %v", tc.backends, found, tc.wantErr)
		}
	}
}
