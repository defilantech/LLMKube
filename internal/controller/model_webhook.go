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
	"context"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// +kubebuilder:webhook:path=/mutate-inference-llmkube-dev-v1alpha1-model,mutating=true,failurePolicy=ignore,sideEffects=None,groups=inference.llmkube.dev,resources=models,verbs=create;update,versions=v1alpha1,name=mmodel.inference.llmkube.dev,admissionReviewVersions=v1

// ModelDefaulter normalizes a Model's hardware.accelerator so it does not
// contradict its GPU runtime (#1074). The CRD default for accelerator is "cpu",
// so a Model that sets gpu.runtime=vulkan but leaves accelerator unset prints
// ACCELERATOR "cpu" in `kubectl get models` even though it runs on the Vulkan
// GPU. Scheduling already keys off gpu.runtime, so this is a display/consistency
// fix, not a behavior change: the defaulter only fills in the accelerator the
// runtime implies, and only when the user left it at the cpu default.
//
// failurePolicy is Ignore: a Model apply must never be blocked because the
// defaulter is unreachable. The value is cosmetic; degrading to "no defaulting"
// is strictly better than a failed apply.
type ModelDefaulter struct{}

var _ admission.Defaulter[*inferencev1alpha1.Model] = &ModelDefaulter{}

// SetupModelWebhookWithManager registers the Model defaulting webhook.
func SetupModelWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &inferencev1alpha1.Model{}).
		WithDefaulter(&ModelDefaulter{}).
		Complete()
}

// Default applies accelerator normalization to a Model at admission.
func (d *ModelDefaulter) Default(ctx context.Context, m *inferencev1alpha1.Model) error {
	logf.FromContext(ctx).V(1).Info("defaulting Model", "name", m.Name, "namespace", m.Namespace)
	normalizeAccelerator(m)
	return nil
}

// normalizeAccelerator sets hardware.accelerator to the value the GPU runtime
// implies when the accelerator was left at the "cpu" default (or empty) on a
// GPU-enabled Model. It never overrides an explicitly-set non-cpu accelerator,
// so a deliberate choice is respected, and it only acts on runtimes that name a
// concrete accelerator (vulkan, rocm); an empty runtime is left untouched
// because it is back-compatible with the historical default and normalizing it
// would restate many pre-existing Models.
func normalizeAccelerator(m *inferencev1alpha1.Model) {
	h := m.Spec.Hardware
	if h == nil || h.GPU == nil || !h.GPU.Enabled {
		return
	}
	accel := strings.ToLower(strings.TrimSpace(h.Accelerator))
	if accel != "" && accel != "cpu" {
		return
	}
	switch h.GPU.Runtime {
	case "vulkan":
		h.Accelerator = "vulkan"
	case "rocm":
		h.Accelerator = "rocm"
	}
}
