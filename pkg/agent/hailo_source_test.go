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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestCheckHailoRuntime(t *testing.T) {
	hailo := hailoTestModel("hailo://qwen2.5-instruct:1.5b")
	if err := checkHailoRuntime(hailo, runtimeOllama); err != nil {
		t.Errorf("hailo source on ollama: %v", err)
	}
	for _, rt := range []string{runtimeLlamaServer, runtimeLlamaCPP, runtimeOMLX} {
		if err := checkHailoRuntime(hailo, rt); err == nil {
			t.Errorf("hailo source on %s: want it refused", rt)
		}
	}
	if err := checkHailoRuntime(hailoTestModel("https://x/m.gguf"), runtimeLlamaServer); err != nil {
		t.Errorf("http source on llama-server: %v", err)
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
}
