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

package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Envoy AI Gateway resource shapes are mirrored from the validated homelab
// spike manifests (experiments/ai-gateway-spike/manifests/), pinned to:
//
//	Envoy Gateway    v1.8.1  (gateway.envoyproxy.io/v1alpha1 Backend)
//	Envoy AI Gateway v0.7.0  (aigateway.envoyproxy.io/v1beta1 AIServiceBackend,
//	                          AIGatewayRoute)
//
// We deliberately do NOT import the upstream Go API types. Envoy AI Gateway is
// young (v0.7.0) and pulling its module risks k8s.io/* version conflicts with
// this repo (k8s.io/* v0.34.x) plus churn. Instead the three resources are
// built as *unstructured.Unstructured by setting GVK + spec fields directly,
// reconciled with controller-runtime CreateOrUpdate, and owner-referenced to
// the InferenceService. The field shapes below are the contract; if the gateway
// CRDs evolve, update these builders and the pinned versions above.
//
// Generated shape (one set per opted-in InferenceService), mirroring the spike:
//
//	Backend (gateway.envoyproxy.io/v1alpha1):
//	  spec.endpoints[0].fqdn.hostname = <svc>.<ns>.svc.cluster.local
//	  spec.endpoints[0].fqdn.port     = <service port>
//
//	AIServiceBackend (aigateway.envoyproxy.io/v1beta1):
//	  spec.schema.name      = "OpenAI"
//	  spec.backendRef.name  = <backend name>
//	  spec.backendRef.kind  = "Backend"
//	  spec.backendRef.group = "gateway.envoyproxy.io"
//
//	AIGatewayRoute (aigateway.envoyproxy.io/v1beta1):
//	  spec.parentRefs[0] = {name, kind: Gateway, group: gateway.networking.k8s.io}
//	  spec.rules[0].matches[0].headers[0] = {type: Exact, name: x-ai-eg-model,
//	                                          value: <model name>}
//	  spec.rules[0].backendRefs[0].name   = <aiservicebackend name>
//
// The MVP targets the InferenceService's Service uniformly for both tiers (pod
// and metal), so there is no tier branching here. llmRequestCosts, priorities,
// and multi-backend fallback from the spike are deferred (see proposal 4.7).

const (
	gatewayBackendGroup       = "gateway.envoyproxy.io"
	gatewayBackendVersion     = "v1alpha1"
	gatewayBackendKind        = "Backend"
	gatewayBackendListKind    = "BackendList"
	gatewayBackendResource    = "backends"
	gatewayBackendRefGroupAPI = "gateway.networking.k8s.io"

	aiGatewayGroup              = "aigateway.envoyproxy.io"
	aiGatewayVersion            = "v1beta1"
	aiServiceBackendKind        = "AIServiceBackend"
	aiServiceBackendListKind    = "AIServiceBackendList"
	aiServiceBackendResource    = "aiservicebackends"
	aiGatewayRouteKind          = "AIGatewayRoute"
	aiGatewayRouteListKind      = "AIGatewayRouteList"
	aiGatewayRouteResource      = "aigatewayroutes"
	aiGatewayModelHeader        = "x-ai-eg-model"
	aiGatewayBackendSchemaName  = "OpenAI"
	aiGatewayRouteParentRefKind = "Gateway"
)

// backendGVK / aiServiceBackendGVK / aiGatewayRouteGVK are the GVKs of the
// generated resources. Exposed as functions so the reconciler and tests share a
// single source of truth.
func backendGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: gatewayBackendGroup, Version: gatewayBackendVersion, Kind: gatewayBackendKind}
}

func aiServiceBackendGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: aiGatewayGroup, Version: aiGatewayVersion, Kind: aiServiceBackendKind}
}

func aiGatewayRouteGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: aiGatewayGroup, Version: aiGatewayVersion, Kind: aiGatewayRouteKind}
}

// gatewayResourceName is the shared name for all three generated resources for
// a given InferenceService. Reusing one DNS-sanitized name keeps the Backend,
// AIServiceBackend, and AIGatewayRoute easy to correlate and GC together.
func gatewayResourceName(isvc *inferencev1alpha1.InferenceService) string {
	return sanitizeDNSName(isvc.Name)
}

// resolveGatewayModelName resolves the OpenAI "model" string the route matches
// on: explicit spec.endpoint.gateway.modelName, else the InferenceService's
// ModelRef, else its name.
func resolveGatewayModelName(isvc *inferencev1alpha1.InferenceService) string {
	if g := gatewaySpec(isvc); g != nil && g.ModelName != "" {
		return g.ModelName
	}
	if isvc.Spec.ModelRef != "" {
		return isvc.Spec.ModelRef
	}
	return isvc.Name
}

// gatewaySpec returns the gateway opt-in block, or nil when unset.
func gatewaySpec(isvc *inferencev1alpha1.InferenceService) *inferencev1alpha1.GatewaySpec {
	if isvc.Spec.Endpoint == nil {
		return nil
	}
	return isvc.Spec.Endpoint.Gateway
}

// gatewayEnabled reports whether this InferenceService opted into gateway
// exposure.
func gatewayEnabled(isvc *inferencev1alpha1.InferenceService) bool {
	g := gatewaySpec(isvc)
	return g != nil && g.Enabled
}

// gatewayServicePort is the port the generated Backend targets on the
// InferenceService's Service. Mirrors constructService's default (8080) and
// honors spec.endpoint.port when set.
func gatewayServicePort(isvc *inferencev1alpha1.InferenceService) int64 {
	if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		return int64(isvc.Spec.Endpoint.Port)
	}
	return 8080
}

// gatewayServiceFQDN is the cluster-internal DNS name of the InferenceService's
// Service. The Backend resolves this fqdn -> ClusterIP -> kube-proxy, which is
// how the spike reaches both in-cluster pods and the metal-agent's selectorless
// Service uniformly.
func gatewayServiceFQDN(isvc *inferencev1alpha1.InferenceService) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", sanitizeDNSName(isvc.Name), isvc.Namespace)
}

// newBackend builds the gateway.envoyproxy.io Backend pointing at the
// InferenceService's Service. Lives in the InferenceService namespace.
func newBackend(isvc *inferencev1alpha1.InferenceService) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(backendGVK())
	u.SetName(gatewayResourceName(isvc))
	u.SetNamespace(isvc.Namespace)
	u.Object["spec"] = map[string]interface{}{
		"endpoints": []interface{}{
			map[string]interface{}{
				"fqdn": map[string]interface{}{
					"hostname": gatewayServiceFQDN(isvc),
					"port":     gatewayServicePort(isvc),
				},
			},
		},
	}
	return u
}

// newAIServiceBackend builds the aigateway.envoyproxy.io AIServiceBackend that
// wraps the Backend with the OpenAI schema. Lives in the InferenceService
// namespace (same as its Backend; the route in the same namespace references
// it).
func newAIServiceBackend(isvc *inferencev1alpha1.InferenceService) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(aiServiceBackendGVK())
	u.SetName(gatewayResourceName(isvc))
	u.SetNamespace(isvc.Namespace)
	u.Object["spec"] = map[string]interface{}{
		"schema": map[string]interface{}{
			"name": aiGatewayBackendSchemaName,
		},
		"backendRef": map[string]interface{}{
			"name":  gatewayResourceName(isvc),
			"kind":  gatewayBackendKind,
			"group": gatewayBackendGroup,
		},
	}
	return u
}

// newAIGatewayRoute builds the one-per-InferenceService AIGatewayRoute: a single
// model-name match rule routing to the AIServiceBackend, attached to the
// referenced Gateway. Lives in the InferenceService namespace; cross-namespace
// attachment to a Gateway in another namespace is a documented prerequisite
// (the Gateway listener's allowedRoutes must permit this namespace).
func newAIGatewayRoute(isvc *inferencev1alpha1.InferenceService) *unstructured.Unstructured {
	g := gatewaySpec(isvc)
	parentRef := map[string]interface{}{
		"name":  g.GatewayRef.Name,
		"kind":  aiGatewayRouteParentRefKind,
		"group": gatewayBackendRefGroupAPI,
	}
	if g.GatewayRef.Namespace != "" {
		parentRef["namespace"] = g.GatewayRef.Namespace
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(aiGatewayRouteGVK())
	u.SetName(gatewayResourceName(isvc))
	u.SetNamespace(isvc.Namespace)
	u.Object["spec"] = map[string]interface{}{
		"parentRefs": []interface{}{parentRef},
		"rules": []interface{}{
			map[string]interface{}{
				"matches": []interface{}{
					map[string]interface{}{
						"headers": []interface{}{
							map[string]interface{}{
								"type":  "Exact",
								"name":  aiGatewayModelHeader,
								"value": resolveGatewayModelName(isvc),
							},
						},
					},
				},
				"backendRefs": []interface{}{
					map[string]interface{}{
						"name": gatewayResourceName(isvc),
					},
				},
			},
		},
	}
	return u
}
