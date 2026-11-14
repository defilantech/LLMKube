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
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// ggufKV is one metadata entry for buildLayoutGGUF. val is a uint32, an
// int32, a bool, a []int32 or a []bool, the types llama.cpp's converters
// write for these keys.
type ggufKV struct {
	key string
	val any
}

// buildLayoutGGUF returns a minimal GGUF v3 header (no tensors) with
// general.architecture = arch and each entry stored under "<arch>.<key>".
// Layout reference: pkg/gguf/parser.go.
func buildLayoutGGUF(t *testing.T, arch string, kvs []ggufKV) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	le := func(v any) { _ = binary.Write(buf, binary.LittleEndian, v) }
	str := func(s string) {
		le(uint64(len(s)))
		buf.WriteString(s)
	}
	const (
		typeUint32 = uint32(4)
		typeInt32  = uint32(5)
		typeBool   = uint32(7)
		typeString = uint32(8)
		typeArray  = uint32(9)
	)
	le(uint32(0x46554747)) // magic "GGUF"
	le(uint32(3))          // version
	le(uint64(0))          // tensor count
	le(uint64(1 + len(kvs)))
	str("general.architecture")
	le(typeString)
	str(arch)
	for _, kv := range kvs {
		str(arch + "." + kv.key)
		switch v := kv.val.(type) {
		case uint32:
			le(typeUint32)
			le(v)
		case int32:
			le(typeInt32)
			le(v)
		case bool:
			le(typeBool)
			le(v)
		case []int32:
			le(typeArray)
			le(typeInt32)
			le(uint64(len(v)))
			for _, e := range v {
				le(e)
			}
		case []bool:
			le(typeArray)
			le(typeBool)
			le(uint64(len(v)))
			for _, e := range v {
				le(e)
			}
		default:
			t.Fatalf("buildLayoutGGUF: unsupported value type %T for %s", kv.val, kv.key)
		}
	}
	return buf.Bytes()
}

// estimateLocalLayout writes the header to a local source and returns the
// agent's estimate for it.
func estimateLocalLayout(
	t *testing.T, arch string, kvs []ggufKV, contextSize, parallelSlots int,
) (MemoryEstimate, uint64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, buildLayoutGGUF(t, arch, kvs), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	est, err := agent.estimateModelMemory(context.Background(), newAdmissionTestModel(path, ""),
		contextSize, "", "", parallelSlots)
	if err != nil {
		t.Fatalf("estimateModelMemory: %v", err)
	}
	return est, testFileSize(t, path)
}

// qwen38_27BLayout is the header of a real Qwen3.8-27B Q4_K_M GGUF (arch
// qwen35): 64 blocks, every 4th a full-attention layer, the rest gated
// delta-net layers with SSM geometry.
var qwen38_27BLayout = []ggufKV{
	{"block_count", uint32(64)},
	{"context_length", uint32(262144)},
	{"embedding_length", uint32(5120)},
	{"attention.head_count", uint32(24)},
	{"attention.head_count_kv", uint32(4)},
	{"attention.key_length", uint32(256)},
	{"attention.value_length", uint32(256)},
	{"ssm.conv_kernel", uint32(4)},
	{"ssm.state_size", uint32(128)},
	{"ssm.group_count", uint32(16)},
	{"ssm.time_step_rank", uint32(48)},
	{"ssm.inner_size", uint32(6144)},
	{"full_attention_interval", uint32(4)},
}

// A qwen35 hybrid keeps a KV cache only on its full-attention layers (layer i
// with (i+1) % 4 == 0, so 16 of 64). Charging all 64 over-charged it four
// times: 34.4 GB of KV at 131072 instead of 8 GiB. The 48 recurrent layers
// each keep f32 conv state 3 * (6144 + 2*16*128) and SSM state 128 * 6144
// per sequence, for the 4 slots llama-server runs without --parallel.
func TestKVLayout_Qwen35HybridChargesOnlyFullAttentionLayers(t *testing.T) {
	est, size := estimateLocalLayout(t, "qwen35", qwen38_27BLayout, 131072, 0)

	wantKV := uint64(16 * 4 * 131072 * (256 + 256) * 2)                  // 8 GiB
	wantRecurrent := uint64(48 * (3*(6144+2*16*128) + 128*6144) * 4 * 4) // 627,572,736
	if est.KVCacheBytes != wantKV {
		t.Errorf("KVCacheBytes = %d, want %d (16 full-attention layers, not 64)", est.KVCacheBytes, wantKV)
	}
	if est.RecurrentStateBytes != wantRecurrent {
		t.Errorf("RecurrentStateBytes = %d, want %d (48 recurrent layers * 4 sequences)",
			est.RecurrentStateBytes, wantRecurrent)
	}
	if want := size + wantKV + wantRecurrent + overheadBytes; est.TotalBytes != want {
		t.Errorf("TotalBytes = %d, want %d (weights + KV + recurrent state + overhead)", est.TotalBytes, want)
	}
}

// The recurrent state is per sequence, so spec.parallelSlots above 1 scales
// it; 0 and 1 both run llama-server's automatic 4 slots.
func TestKVLayout_RecurrentStateScalesWithParallelSlots(t *testing.T) {
	perSeq := uint64(48 * (3*(6144+2*16*128) + 128*6144) * 4)
	for _, tc := range []struct{ slots, seqs int }{{0, 4}, {1, 4}, {2, 2}, {8, 8}} {
		est, _ := estimateLocalLayout(t, "qwen35", qwen38_27BLayout, 131072, tc.slots)
		if want := perSeq * uint64(tc.seqs); est.RecurrentStateBytes != want { //nolint:gosec // G115: small test values
			t.Errorf("parallelSlots %d: RecurrentStateBytes = %d, want %d (%d sequences)",
				tc.slots, est.RecurrentStateBytes, want, tc.seqs)
		}
	}
}

// An MTP model's trailing nextn layers are attention layers whatever the
// interval says (the interval applies below n_layer() = block_count -
// nextn_predict_layers). Real Qwopus3.6-35B-A3B MTP header: 41 blocks, 1 MTP
// layer, so 10 interval layers plus the MTP layer hold a KV cache and 30 are
// recurrent.
func TestKVLayout_MTPLayersAreAttentionLayers(t *testing.T) {
	layout := []ggufKV{
		{"block_count", uint32(41)},
		{"embedding_length", uint32(2048)},
		{"attention.head_count", uint32(16)},
		{"attention.head_count_kv", uint32(2)},
		{"attention.key_length", uint32(256)},
		{"attention.value_length", uint32(256)},
		{"ssm.conv_kernel", uint32(4)},
		{"ssm.state_size", uint32(128)},
		{"ssm.group_count", uint32(16)},
		{"ssm.inner_size", uint32(4096)},
		{"full_attention_interval", uint32(4)},
		{"nextn_predict_layers", uint32(1)},
	}
	est, _ := estimateLocalLayout(t, "qwen35moe", layout, 262144, 2)

	if want := uint64(11 * 2 * 262144 * (256 + 256) * 2); est.KVCacheBytes != want {
		t.Errorf("KVCacheBytes = %d, want %d (10 interval layers + 1 MTP layer)", est.KVCacheBytes, want)
	}
	if want := uint64(30 * (3*(4096+2*16*128) + 128*4096) * 4 * 2); est.RecurrentStateBytes != want {
		t.Errorf("RecurrentStateBytes = %d, want %d (30 recurrent layers * 2 sequences)", est.RecurrentStateBytes, want)
	}
}

// <arch>.attention.recurrent_layers, when present, is the layout and the
// interval is not consulted (llama.cpp reads it first, get_key_or_arr).
func TestKVLayout_RecurrentLayersKeyOverridesInterval(t *testing.T) {
	layout := []ggufKV{
		{"block_count", uint32(8)},
		{"embedding_length", uint32(1024)},
		{"attention.head_count", uint32(8)},
		{"attention.head_count_kv", uint32(2)},
		{"attention.key_length", uint32(128)},
		{"attention.value_length", uint32(128)},
		{"ssm.conv_kernel", uint32(4)},
		{"ssm.state_size", uint32(64)},
		{"ssm.inner_size", uint32(512)},
		{"full_attention_interval", uint32(4)}, // would make layers 3 and 7 attention
		{"attention.recurrent_layers", []bool{false, true, true, true, true, true, true, true}},
	}
	est, _ := estimateLocalLayout(t, "qwen35", layout, 4096, 0)

	if want := uint64(1 * 2 * 4096 * (128 + 128) * 2); est.KVCacheBytes != want {
		t.Errorf("KVCacheBytes = %d, want %d (only layer 0 per recurrent_layers)", est.KVCacheBytes, want)
	}
	if want := uint64(7 * (3*(512+0) + 64*512) * 4 * 4); est.RecurrentStateBytes != want {
		t.Errorf("RecurrentStateBytes = %d, want %d (7 recurrent layers, no group_count)", est.RecurrentStateBytes, want)
	}
}

// Per-layer KV head counts are summed. In a file with no SSM geometry a 0
// marks a layer with no attention, which holds nothing: Nemotron Super 49B
// (arch deci) stores int32 arrays and has such layers. Its head_count is an
// array too, so the explicit key and value lengths carry the head dims.
// Before, an array head count read as unknown and the embedding-width formula
// charged all 80 layers at 8192 wide: 343.6 GB at 131072.
func TestKVLayout_PerLayerHeadCountsAreSummed(t *testing.T) {
	layout := []ggufKV{
		{"block_count", uint32(6)},
		{"embedding_length", uint32(8192)},
		{"attention.head_count", []int32{64, 64, 0, 0, 64, 64}},
		{"attention.head_count_kv", []int32{8, 8, 0, 0, 8, 8}},
		{"attention.key_length", uint32(128)},
		{"attention.value_length", uint32(128)},
	}
	est, _ := estimateLocalLayout(t, "deci", layout, 131072, 0)

	if want := uint64(32 * 131072 * (128 + 128) * 2); est.KVCacheBytes != want {
		t.Errorf("KVCacheBytes = %d, want %d (8+8+8+8 KV heads)", est.KVCacheBytes, want)
	}
	if est.RecurrentStateBytes != 0 {
		t.Errorf("RecurrentStateBytes = %d, want 0 (no SSM geometry, so no recurrent layer)", est.RecurrentStateBytes)
	}
}

// With SSM geometry and no layout key, a per-layer KV head count of 0 is a
// recurrent layer (Jamba, Nemotron-H and the Granite hybrids), which keeps
// recurrent state instead of a KV cache.
func TestKVLayout_ZeroHeadLayersWithSSMAreRecurrent(t *testing.T) {
	layout := []ggufKV{
		{"block_count", uint32(4)},
		{"embedding_length", uint32(4096)},
		{"attention.head_count", uint32(32)},
		{"attention.head_count_kv", []int32{0, 0, 8, 0}},
		{"attention.key_length", uint32(128)},
		{"attention.value_length", uint32(128)},
		{"ssm.conv_kernel", uint32(4)},
		{"ssm.state_size", uint32(128)},
		{"ssm.group_count", uint32(8)},
		{"ssm.inner_size", uint32(8192)},
	}
	est, _ := estimateLocalLayout(t, "nemotron_h", layout, 65536, 0)

	if want := uint64(8 * 65536 * (128 + 128) * 2); est.KVCacheBytes != want {
		t.Errorf("KVCacheBytes = %d, want %d (one attention layer, 8 KV heads)", est.KVCacheBytes, want)
	}
	if want := uint64(3 * (3*(8192+2*8*128) + 128*8192) * 4 * 4); est.RecurrentStateBytes != want {
		t.Errorf("RecurrentStateBytes = %d, want %d (3 recurrent layers * 4 sequences)", est.RecurrentStateBytes, want)
	}
}

// A recurrent layout whose state the generic Mamba formula does not describe
// (RWKV, LFM2, Kimi KDA, the PLE history) or that has no SSM geometry gets no
// layout: every layer stays charged, the existing over-charge, never an
// under-charge.
func TestKVLayout_UndescribedRecurrentStateKeepsChargingEveryLayer(t *testing.T) {
	allLayers := uint64(64 * 4 * 131072 * (256 + 256) * 2)
	withoutSSM := make([]ggufKV, 0, len(qwen38_27BLayout))
	for _, kv := range qwen38_27BLayout {
		if kv.key == "ssm.inner_size" {
			continue
		}
		withoutSSM = append(withoutSSM, kv)
	}
	cases := map[string][]ggufKV{
		"no ssm.inner_size": withoutSSM,
		"wkv key":           append(append([]ggufKV{}, qwen38_27BLayout...), ggufKV{"wkv.head_size", uint32(64)}),
		"shortconv key":     append(append([]ggufKV{}, qwen38_27BLayout...), ggufKV{"shortconv.l_cache", uint32(3)}),
		"kda key":           append(append([]ggufKV{}, qwen38_27BLayout...), ggufKV{"kda.head_dim", uint32(128)}),
		"ple key":           append(append([]ggufKV{}, qwen38_27BLayout...), ggufKV{"ple.conv_kernel", uint32(4)}),
		"zero interval":     replaceKV(qwen38_27BLayout, "full_attention_interval", uint32(0)),
		"short layout array": append(append([]ggufKV{}, qwen38_27BLayout...),
			ggufKV{"attention.recurrent_layers", []bool{true}}),
	}
	for name, layout := range cases {
		t.Run(name, func(t *testing.T) {
			est, _ := estimateLocalLayout(t, "qwen35", layout, 131072, 0)
			if est.KVCacheBytes != allLayers {
				t.Errorf("KVCacheBytes = %d, want %d (all 64 layers, as before)", est.KVCacheBytes, allLayers)
			}
			if est.RecurrentStateBytes != 0 {
				t.Errorf("RecurrentStateBytes = %d, want 0", est.RecurrentStateBytes)
			}
		})
	}
}

func replaceKV(kvs []ggufKV, key string, val any) []ggufKV {
	out := append([]ggufKV{}, kvs...)
	for i := range out {
		if out[i].key == key {
			out[i].val = val
		}
	}
	return out
}

// The layout fields reach the KV term only through EstimateOptions; with
// KVHeadLayers set, HeadCount and HeadCountKV are needed only to derive a
// missing head dim, and a missing one with no scalar head count falls back to
// the embedding-width formula as before.
func TestEstimateModelMemoryWithOptions_KVHeadLayers(t *testing.T) {
	const file = uint64(10 << 30)
	est := EstimateModelMemoryWithOptions(file, 6, 8192, 1024, EstimateOptions{
		KVHeadLayers: 32, KeyLength: 128, ValueLength: 128,
	})
	if want := uint64(32 * 1024 * 256 * 2); est.KVCacheBytes != want {
		t.Errorf("explicit dims: KVCacheBytes = %d, want %d", est.KVCacheBytes, want)
	}

	est = EstimateModelMemoryWithOptions(file, 6, 8192, 1024, EstimateOptions{KVHeadLayers: 32})
	if want := uint64(6 * 8192 * 1024 * 4); est.KVCacheBytes != want {
		t.Errorf("no dims, no head count: KVCacheBytes = %d, want %d (embedding-width formula)", est.KVCacheBytes, want)
	}

	est = EstimateModelMemoryWithOptions(file, 6, 8192, 1024, EstimateOptions{
		KVHeadLayers: 32, KeyLength: 128, ValueLength: 128, RecurrentStateBytes: 1 << 20,
	})
	if want := file + uint64(32*1024*256*2) + 1<<20 + overheadBytes; est.TotalBytes != want {
		t.Errorf("TotalBytes = %d, want %d (recurrent state included)", est.TotalBytes, want)
	}

	est = EstimateModelMemoryWithOptions(file, 6, 8192, 1024, EstimateOptions{
		KVHeadLayers: 32, KeyLength: 128, ValueLength: 128, RecurrentStateBytes: math.MaxUint64,
	})
	if want := fallbackEstimate(file); est != want {
		t.Errorf("overflowing recurrent state: got %+v, want the overflow fallback %+v", est, want)
	}
}
