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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MCPConfig configures the agent's MCP (Model Context Protocol) client.
// Optional; when nil or Enabled=false the agent runs with no MCP tools.
type MCPConfig struct {
	// Enabled turns the MCP client on for this agent (opt-in; default off).
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// CallTimeout bounds a single MCP tool call. Defaults to 30s when zero.
	// +optional
	CallTimeout metav1.Duration `json:"callTimeout,omitempty"`

	// MaxResultBytes caps an MCP tool result before truncation. Defaults to
	// 32768 when zero.
	// +optional
	MaxResultBytes int `json:"maxResultBytes,omitempty"`

	// Servers is the set of MCP servers to connect to.
	// +optional
	Servers []MCPServer `json:"servers,omitempty"`
}

// MCPServer configures one MCP server the agent connects to. Its tools
// are namespaced as mcp/<name>/<tool> so identically-named tools across
// servers do not collide in the model-facing tool whitelist.
type MCPServer struct {
	// Name namespaces this server's tools as mcp/<name>/<tool>.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Transport is the MCP transport. Phase 1 supports "http" only.
	// +kubebuilder:validation:Enum=http
	// +kubebuilder:validation:Required
	Transport string `json:"transport"`

	// URL is the MCP server endpoint (streamable HTTP).
	// +kubebuilder:validation:Required
	URL string `json:"url"`

	// Headers are extra HTTP headers (e.g. auth), sourced from secrets.
	// +optional
	Headers []MCPHeader `json:"headers,omitempty"`

	// AllowedTools whitelists tool names this server may expose ("*" or
	// empty = all).
	// +optional
	AllowedTools []string `json:"allowedTools,omitempty"`
}

// MCPHeader is one HTTP header attached to every request an MCPServer
// receives, with its value sourced from a Secret key so credentials never
// live in the CR spec.
type MCPHeader struct {
	// Name is the HTTP header name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// ValueFrom sources the header value from a Secret key.
	// +optional
	ValueFrom *corev1.SecretKeySelector `json:"valueFrom,omitempty"`
}
