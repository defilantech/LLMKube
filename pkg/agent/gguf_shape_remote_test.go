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
	"os"
	"path/filepath"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// qwopus35BMTPSource is a remote source for a hybrid MoE model with MTP
// layers, the shape of a Qwopus3.6-35B-A3B MTP Q6_K file (#2534).
const qwopus35BMTPSource = "https://model-host.invalid/Qwopus3.6-35B-A3B-MTP-Q6_K.gguf"

// qwopus35BMTPLayout is the header of that file: 41 blocks, 1 MTP layer, so
// 10 interval attention layers plus the MTP layer hold a KV cache and 30 are
// recurrent. head_count_kv is stored per layer, 2 on the 11 attention layers
// and 0 on the 30 recurrent ones, the encoding a real file uses.
var qwopus35BMTPLayout = []ggufKV{
	{"block_count", uint32(41)},
	{"context_length", uint32(262144)},
	{"embedding_length", uint32(2048)},
	{"attention.head_count", uint32(16)},
	{"attention.head_count_kv", qwopus35BMTPSteps(41, 4, 1)},
	{"attention.key_length", uint32(256)},
	{"attention.value_length", uint32(256)},
	{"ssm.conv_kernel", uint32(4)},
	{"ssm.state_size", uint32(128)},
	{"ssm.group_count", uint32(16)},
	{"ssm.inner_size", uint32(4096)},
	{"full_attention_interval", uint32(4)},
	{"nextn_predict_layers", uint32(1)},
}

// qwopus35BMTPSteps returns a per-layer head_count_kv array of n layers
// whose last nextn are attention layers and, below them, every interval-th
// one is: 2 on those, 0 elsewhere.
func qwopus35BMTPSteps(n, interval, nextn int) []int32 {
	out := make([]int32, n)
	for i := 0; i < n-nextn; i++ {
		if (i+1)%interval == 0 {
			out[i] = 2
		}
	}
	for i := n - nextn; i < n; i++ {
		out[i] = 2
	}
	return out
}

// qwopus35BMTPStatus is the Model status shape the controller records for
// that file. It has no per-layer layout, and no KV head count, because
// HeadCountKV() reads the per-layer array as unknown (0).
var qwopus35BMTPStatus = &inferencev1alpha1.GGUFMetadata{
	LayerCount:    41,
	EmbeddingSize: 2048,
	HeadCount:     16,
	HeadCountKV:   0,
	KeyLength:     256,
	ValueLength:   256,
}

// #2534: a remote-sourced hybrid whose file is in the model store must be
// charged from the file's header, not from the Model status. The status has
// no layout and no KV head count, so it charges every one of the 41 layers
// (the embedding-width formula): 41*2*262144*512*2 bytes of KV. The header
// says 10 interval layers plus the MTP layer hold a KV cache, and 30 layers
// hold recurrent state instead.
func TestEstimateModelMemory_FileHeaderWinsOverStatusShape(t *testing.T) {
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel(qwopus35BMTPSource, "")
	slot := modelCacheSlot(agent.config.ModelStorePath, model.Name, qwopus35BMTPSource)
	if err := os.MkdirAll(filepath.Dir(slot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slot, buildLayoutGGUF(t, "qwen35moe", qwopus35BMTPLayout), 0o600); err != nil {
		t.Fatal(err)
	}
	model.Status.GGUF = qwopus35BMTPStatus

	est, err := agent.estimateModelMemory(context.Background(), model, 262144, "", "", 2)
	if err != nil {
		t.Fatalf("estimateModelMemory: %v", err)
	}

	wantKV := uint64(11 * 2 * 262144 * (256 + 256) * 2)
	if est.KVCacheBytes != wantKV {
		t.Errorf("KVCacheBytes = %d, want %d (10 interval layers + 1 MTP layer from the file's header)",
			est.KVCacheBytes, wantKV)
	}
	wantRecurrent := uint64(30 * (3*(4096+2*16*128) + 128*4096) * 4 * 2)
	if est.RecurrentStateBytes != wantRecurrent {
		t.Errorf("RecurrentStateBytes = %d, want %d (30 recurrent layers * 2 sequences)",
			est.RecurrentStateBytes, wantRecurrent)
	}
}
