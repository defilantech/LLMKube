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

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// statusForQwopus is the Qwopus3.6-35B-A3B MTP Q6_K header of #2552 as the
// controller records it: 41 blocks, an MTP layer, an attention layer every
// 4th of the 40 below it, and SSM geometry on the 30 layers that are not
// attention. The file stores attention.head_count_kv per layer, so the scalar
// status field is 0 and the head dims are the only usable part of the shape.
func statusForQwopus() *inferencev1alpha1.GGUFMetadata {
	return &inferencev1alpha1.GGUFMetadata{
		Architecture:  "qwen35moe",
		EmbeddingSize: 2048,
		LayerCount:    41,
		HeadCount:     16,
		HeadCountKV:   0,
		KeyLength:     256,
		ValueLength:   256,
	}
}

// The first deploy of a remote hybrid has no file on this Mac, so the shape
// comes from the Model status, and the controller records the layer layout
// there with the rest of the header. Charging the status without that layout
// is what #2552 reports: 88,046,829,568 bytes of KV cache for a model that
// fits, refused before the download that would have made a header read
// possible.
func TestEstimateModelMemory_StatusLayoutSizesFirstDeploy(t *testing.T) {
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel("https://example.invalid/qwopus3.6-35b-a3b-mtp-q6_k.gguf", "30 GiB")
	meta := statusForQwopus()
	// 11 attention layers (10 interval layers and the MTP layer) of 2 KV
	// heads, 256 each way; 30 recurrent layers of 548,864 f32 values each,
	// 65,863,680 bytes per sequence.
	meta.KVHeadLayers = 22
	meta.RecurrentStatePerSeq = 65863680
	model.Status.GGUF = meta

	est, err := agent.estimateModelMemory(context.Background(), model, 262144, "", "", 2)
	if err != nil {
		t.Fatalf("estimateModelMemory: %v", err)
	}
	if want := uint64(5905580032); est.KVCacheBytes != want {
		t.Errorf("KVCacheBytes = %d, want %d (22 KV head layers, not 41 * 2)", est.KVCacheBytes, want)
	}
	if want := uint64(131727360); est.RecurrentStateBytes != want {
		t.Errorf("RecurrentStateBytes = %d, want %d (30 recurrent layers * 2 sequences)",
			est.RecurrentStateBytes, want)
	}
}

// A status an older controller wrote carries no layout, so the agent charges
// every layer by head_count_kv, the over-charge this stays compatible with
// rather than silently under-charging a model that really does keep a KV
// cache everywhere.
func TestEstimateModelMemory_StatusWithoutLayoutChargesEveryLayer(t *testing.T) {
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel("https://example.invalid/qwopus3.6-35b-a3b-mtp-q6_k.gguf", "30 GiB")
	model.Status.GGUF = statusForQwopus()

	est, err := agent.estimateModelMemory(context.Background(), model, 262144, "", "", 2)
	if err != nil {
		t.Fatalf("estimateModelMemory: %v", err)
	}
	if want := uint64(88046829568); est.KVCacheBytes != want {
		t.Errorf("KVCacheBytes = %d, want %d (41 layers by the embedding-width formula, no layout recorded)",
			est.KVCacheBytes, want)
	}
	if est.RecurrentStateBytes != 0 {
		t.Errorf("RecurrentStateBytes = %d, want 0 (no layout recorded)", est.RecurrentStateBytes)
	}
}
