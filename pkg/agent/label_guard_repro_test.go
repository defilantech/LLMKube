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

package agent

import (
	"context"
	"strings"
	"testing"
)

// GHSA-863w-vh42-mjhj: the metal-agent write policy holds only the
// llmkube-metal-* identities to its validations, because its identity match
// condition filters out every other writer. A namespace editor therefore
// writes a Service and EndpointSlice carrying llmkube.ai/managed-by=metal-agent
// itself, with its own llmkube.ai/agent-ingress-spki, and the controller reads
// that unauthenticated label as an agent registration: it enters relay mode
// pinned to the attacker's certificate and the relay presents the
// per-namespace token to it.
//
// These cases assert the registration write itself is refused for a writer
// that is not a mac agent, which is what makes the label mean "the agent wrote
// this".
func TestLabelGuard_Repro(t *testing.T) {
	e := newWritePolicyEnv(t)
	ctx := context.Background()

	t.Run("a non-agent identity cannot create a managed-by=metal-agent EndpointSlice", func(t *testing.T) {
		sl := e.slice("alpha-agent", map[string]string{
			"kubernetes.io/service-name":   "alpha-agent",
			managedByLabel:                 managedByValue,
			"llmkube.ai/inference-service": "alpha",
		})
		sl.Annotations = map[string]string{
			"llmkube.ai/agent-heartbeat":    "2026-10-09T21:39:50Z",
			"llmkube.ai/agent-ingress-spki": "yB/FoBim+Q3IgcpuCfOyhqaXbDVvj9os6wTbZNjSUXo=",
		}
		err := e.other.Create(ctx, sl)
		if err == nil {
			t.Fatalf("VULNERABLE: a ServiceAccount outside the llmkube-metal- prefix created a "+
				"managed-by=metal-agent EndpointSlice (%s); the controller reads that label as an "+
				"agent registration", sl.Name)
		}
		if !strings.Contains(err.Error(), "ValidatingAdmissionPolicy") {
			t.Fatalf("denied by something other than an admission policy: %v", err)
		}
	})

	t.Run("a non-agent identity cannot create a managed-by=metal-agent Service", func(t *testing.T) {
		svc := e.service("alpha-agent", map[string]string{
			managedByLabel:                 managedByValue,
			"llmkube.ai/inference-service": "alpha",
		})
		err := e.other.Create(ctx, svc)
		if err == nil {
			t.Fatalf("VULNERABLE: a ServiceAccount outside the llmkube-metal- prefix created a "+
				"managed-by=metal-agent Service (%s)", svc.Name)
		}
		if !strings.Contains(err.Error(), "ValidatingAdmissionPolicy") {
			t.Fatalf("denied by something other than an admission policy: %v", err)
		}
	})

	t.Run("an unrelated identity can still write unlabelled objects", func(t *testing.T) {
		svc := e.service("plain-svc", map[string]string{"app": "plain"})
		wantAllowed(t, "unlabelled create", e.other.Create(ctx, svc))
	})

	t.Run("a mac agent identity may still write the label", func(t *testing.T) {
		svc := e.service("beta-agent", map[string]string{
			managedByLabel:                 managedByValue,
			"llmkube.ai/inference-service": "beta",
		})
		wantAllowed(t, "agent create", e.agent.Create(ctx, svc))
	})
}
