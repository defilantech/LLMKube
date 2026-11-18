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
	"fmt"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// checkHailoSource refuses a hailo:// Model on a runtime other than ollama
// (RuntimeCannotServeSource) and one whose host Hailo device another service
// holds (HailoDeviceInUse). For a hailo:// Model it admits, key's start holds
// the device until the returned release runs, and a process the start stored
// holds it from then on (ManagedProcess.Hailo). release is a no-op for any
// other source.
func (a *MetalAgent) checkHailoSource(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, key string,
	model *inferencev1alpha1.Model, runtime string,
) (release func(), err error) {
	noop := func() {}
	if err := inferencev1alpha1.HailoSourceRuntimeError(model, runtime); err != nil {
		return noop, a.refuseStart(ctx, isvc, EventReasonRuntimeCannotServeSource, err.Error())
	}
	if !inferencev1alpha1.IsHailoSource(model.Spec.Source) {
		return noop, nil
	}
	if holder := a.claimHailoDevice(key); holder != "" {
		return noop, a.refuseStart(ctx, isvc, EventReasonHailoDeviceInUse, hailoDeviceInUseMessage(holder))
	}
	return func() { a.endHailoStart(key) }, nil
}

// claimHailoDevice marks key's hailo:// start as in flight and returns "", or
// returns the key of the other service that holds the host's Hailo device and
// claims nothing. A service holds it while its start is in flight, while its
// process serves a hailo:// source, and while the health monitor owes that
// process a restart. hailo-ollama serves one model on the device at a time.
func (a *MetalAgent) claimHailoDevice(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if holder := a.hailoHolderLocked(key); holder != "" {
		return holder
	}
	a.hailoStarting = key
	return ""
}

// hailoHolderLocked returns the key of a service other than key that holds
// the host's Hailo device, or "". Must be called with a.mu held.
func (a *MetalAgent) hailoHolderLocked(key string) string {
	if a.hailoStarting != "" && a.hailoStarting != key {
		return a.hailoStarting
	}
	for k, p := range a.processes {
		if k != key && p.Hailo {
			return k
		}
	}
	for k, st := range a.restarts {
		if k != key && st.pending && st.hailo {
			return k
		}
	}
	return ""
}

// endHailoStart ends key's in-flight claim. A stored process holds the device
// from here on (ManagedProcess.Hailo).
func (a *MetalAgent) endHailoStart(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hailoStarting == key {
		a.hailoStarting = ""
	}
}

// hailoDeviceInUseMessage is the HailoDeviceInUse refusal message.
func hailoDeviceInUseMessage(holder string) string {
	return fmt.Sprintf("the host's Hailo device already serves InferenceService %s; the agent serves one "+
		"hailo:// InferenceService per host: delete or scale that one to zero, or place this one on another host",
		holder)
}
