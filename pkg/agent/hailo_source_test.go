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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func hailoTestModel(source string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:   source,
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}
}

func TestEnsureProcessRefusesHailoSourceOffOllama(t *testing.T) {
	for _, tc := range []struct {
		runtime     string
		wantRefused bool
	}{
		{runtime: runtimeLlamaCPP, wantRefused: true},
		{runtime: runtimeLlamaServer, wantRefused: true},
		{runtime: runtimeOllama},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			isvc := refusalISVC("svc")
			isvc.Spec.Runtime = tc.runtime
			model := refusalModel()
			model.Spec.Source = "hailo://qwen2.5-instruct:1.5b"
			model.Spec.Format = ""
			a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, model)
			a.executors[runtimeOllama] = ex

			err := a.ensureProcess(context.Background(), isvc)
			got := &inferencev1alpha1.InferenceService{}
			if gErr := a.config.K8sClient.Get(context.Background(),
				types.NamespacedName{Name: "svc", Namespace: "default"}, got); gErr != nil {
				t.Fatalf("get InferenceService: %v", gErr)
			}
			events := warningEvents(rec, EventReasonRuntimeCannotServeSource)
			if !tc.wantRefused {
				if got.Status.SchedulingStatus == EventReasonRuntimeCannotServeSource || len(events) != 0 {
					t.Errorf("hailo source on ollama refused: status %q, events %v, err %v",
						got.Status.SchedulingStatus, events, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), EventReasonRuntimeCannotServeSource) {
				t.Fatalf("ensureProcess error = %v, want %s", err, EventReasonRuntimeCannotServeSource)
			}
			if ex.starts != 0 {
				t.Errorf("starts = %d, want 0", ex.starts)
			}
			if got.Status.SchedulingStatus != EventReasonRuntimeCannotServeSource ||
				!strings.Contains(got.Status.SchedulingMessage, "hailo://") {
				t.Errorf("status = %q / %q, want %s naming the hailo:// source",
					got.Status.SchedulingStatus, got.Status.SchedulingMessage, EventReasonRuntimeCannotServeSource)
			}
			if len(events) != 1 {
				t.Errorf("Warning %s events = %v, want one", EventReasonRuntimeCannotServeSource, events)
			}
		})
	}
}

func TestProbeRemoteModelSizeHailo(t *testing.T) {
	lib := fakeHailoLibrary(t, map[string]string{"/blob/sha256_" + testHailoDigest: "2359716464"})
	manifests := writeHailoManifests(t, map[string]string{"qwen2.5-instruct:1.5b": testHailoDigest})
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient:            fake.NewClientBuilder().WithScheme(newTestScheme()).Build(),
		Namespace:            "default",
		AllowedDownloadHosts: []string{"127.0.0.1"},
		HailoManifestsDir:    manifests,
		HailoLibraryURL:      lib,
	})

	size, probed, err := a.probeRemoteModelSize(context.Background(), hailoTestModel("hailo://qwen2.5-instruct:1.5b"))
	if err != nil || !probed || size != 2359716464 {
		t.Errorf("probe = %d, %v, %v; want the HEF's Content-Length", size, probed, err)
	}
	_, probed, err = a.probeRemoteModelSize(context.Background(), hailoTestModel("hailo://llama3.2:3b"))
	if !probed || err == nil {
		t.Errorf("tag not in the manifests: probe = %v, %v; want a probe error", probed, err)
	}

	blocked := NewMetalAgent(MetalAgentConfig{
		K8sClient:         fake.NewClientBuilder().WithScheme(newTestScheme()).Build(),
		Namespace:         "default",
		HailoManifestsDir: manifests,
		HailoLibraryURL:   lib,
	})
	_, _, err = blocked.probeRemoteModelSize(context.Background(), hailoTestModel("hailo://qwen2.5-instruct:1.5b"))
	if err == nil {
		t.Error("a library host the download allowlist refuses (loopback): want a probe error")
	}

	notHTTP := NewMetalAgent(MetalAgentConfig{
		K8sClient:         fake.NewClientBuilder().WithScheme(newTestScheme()).Build(),
		Namespace:         "default",
		HailoManifestsDir: manifests,
		HailoLibraryURL:   "file:///srv/hailo",
	})
	_, probed, err = notHTTP.probeRemoteModelSize(context.Background(), hailoTestModel("hailo://qwen2.5-instruct:1.5b"))
	if !probed || err == nil || !strings.Contains(err.Error(), "--hailo-library-url") {
		t.Errorf("a library URL that is not http(s): probe = %v, %v; want an error naming --hailo-library-url", probed, err)
	}
}
