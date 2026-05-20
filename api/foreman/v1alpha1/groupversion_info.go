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

// Package v1alpha1 contains API Schema definitions for the foreman v1alpha1
// API group. Foreman is an opt-in add-on layered on top of LLMKube: it
// schedules agentic workloads (Workload, AgenticTask) across a fleet of
// machines that advertise themselves as FleetNodes. Installing LLMKube alone
// does not install or require any of these types.
//
// +kubebuilder:object:generate=true
// +groupName=foreman.llmkube.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "foreman.llmkube.dev", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	// scheme.Builder was deprecated in controller-runtime v0.24 (the rationale
	// is that api packages should have minimal deps, and scheme.Builder pulls
	// controller-runtime into api/). It still works; migrating to the
	// runtime.NewSchemeBuilder pattern is a project-wide refactor that touches
	// both API groups and is tracked as its own follow-up. Suppress the
	// deprecation here only.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // SA1019: see comment above

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
