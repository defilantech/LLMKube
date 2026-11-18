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

package gguf

import "strings"

// KVLayout reads which layers of a GGUF hold a KV cache, and the state the
// others hold instead, for the files whose layout is not simply block_count
// layers of head_count_kv heads. It mirrors llama.cpp master
// (ggml-org/llama.cpp 3837bf0bc, 2026-11-10):
//
//   - A layer is recurrent when <arch>.attention.recurrent_layers says so
//     (one bool per layer, or one for all). Without that key, when the file
//     has <arch>.full_attention_interval = k, layer i is recurrent when
//     (i+1) % k != 0, except the trailing <arch>.nextn_predict_layers (MTP)
//     layers, which are attention layers (src/models/qwen35.cpp,
//     qwen35moe.cpp, qwen3next.cpp, minimax-01.cpp). Without either key, a
//     per-layer head_count_kv of 0 marks a recurrent layer when the file has
//     SSM geometry (Jamba, Nemotron-H and the Granite hybrids set
//     recurrent_layer_arr from n_head_kv(i) == 0); with no SSM geometry such
//     a layer simply has no attention (Nemotron Super's deci layers).
//   - The KV cache covers only the other layers, n_head_kv(il) heads each:
//     llama-memory-hybrid.cpp filters the attention cache on !is_recr(il),
//     and llama-kv-cache.cpp skips a filtered layer and sizes the rest by
//     n_embd_k_gqa(il) = head dim * n_head_kv(il).
//   - Each recurrent layer holds f32 conv state (d_conv-1) * (d_inner +
//     2*n_group*d_state) and SSM state d_state * d_inner per sequence
//     (llama-hparams.cpp n_embd_r and n_embd_s, the generic Mamba branch;
//     recurrent_type_r and _s are GGML_TYPE_F32 in llama-model.cpp).
//
// It returns kvHeadLayers = 0 when the layout is the plain one (nothing to
// refine), when a key is malformed, and when recurrent layers keep a state
// the generic formula does not describe (RWKV's .wkv., LFM2's .shortconv.,
// Kimi's .kda., the .ple. history) or lack SSM geometry: the estimate then
// charges every layer, which over-charges a hybrid and never under-charges
// it. Sliding-window layers are charged at the full context here; that too
// over-charges.
//
// The controller records both values on the Model status (see
// ggufMetadataFromParsed and the GGUFMetadata fields), so an agent that has
// no file on disk yet sizes the model from the header of the source rather
// than from the file it has not downloaded.
func KVLayout(f *GGUFFile) (kvHeadLayers, recurrentStatePerSeq uint64) {
	arch := f.Architecture()
	n := f.BlockCount()
	if arch == "" || n == 0 || n > maxKVLayoutLayers {
		return 0, 0
	}
	key := func(name string) (GGUFValue, bool) { return f.GetMetadata(arch + "." + name) }

	headsKV, foundHeadsKV := key("attention.head_count_kv")
	kvHeads, perLayerHeads, ok := kvLayerCounts(headsKV, foundHeadsKV, n)
	if !ok {
		return 0, 0
	}
	ssmInner, hasInner := kvCountKey(key("ssm.inner_size"))
	ssmState, hasState := kvCountKey(key("ssm.state_size"))
	hasSSM := hasInner && hasState

	recurrent := make([]bool, n)
	if v, found := key("attention.recurrent_layers"); found {
		if !kvLayerBools(v, recurrent) {
			return 0, 0
		}
	} else if v, found := key("full_attention_interval"); found {
		interval, ok := kvCount(v)
		if !ok || interval == 0 {
			return 0, 0
		}
		nextn, _ := kvCountKey(key("nextn_predict_layers"))
		if nextn > n {
			return 0, 0
		}
		for i := uint64(0); i < n-nextn; i++ {
			recurrent[i] = (i+1)%interval != 0
		}
	} else if perLayerHeads && hasSSM {
		for i := range recurrent {
			recurrent[i] = kvLayerHeads(kvHeads, i) == 0
		}
	}

	var recurrentLayers uint64
	for i, r := range recurrent {
		if r {
			recurrentLayers++
			continue
		}
		kvHeadLayers += kvLayerHeads(kvHeads, i)
	}
	if recurrentLayers == 0 && !perLayerHeads {
		return 0, 0 // block_count layers of head_count_kv heads: nothing to refine
	}
	if kvHeadLayers == 0 {
		return 0, 0 // no attention layer at all; leave it to the existing charge
	}
	if recurrentLayers == 0 {
		return kvHeadLayers, 0
	}

	// MiniMax-01's linear-attention state (n_embd_head_la in llama.cpp) needs
	// no key here: src/models/minimax-01.cpp reads no ssm.* key, so such a
	// file has no SSM geometry and takes the every-layer charge below.
	if !hasSSM || kvHasKeyPart(f, arch, ".wkv.", ".shortconv.", ".kda.", ".ple.") {
		return 0, 0
	}
	conv, _ := kvCountKey(key("ssm.conv_kernel"))
	groups, _ := kvCountKey(key("ssm.group_count"))
	if conv > 0 {
		conv--
	}
	// Real files are far inside maxKVLayoutSSMDim (Qwen3.8-27B: inner 6144,
	// state 128, 16 groups, kernel 4); bounding every factor keeps the
	// products below 2^61 with n <= 512. A file past the bound gets no layout.
	if ssmInner > maxKVLayoutSSMDim || ssmState > maxKVLayoutSSMDim ||
		conv > maxKVLayoutSSMDim || groups > maxKVLayoutSSMDim {
		return 0, 0
	}
	floatsPerLayer := conv*(ssmInner+2*groups*ssmState) + ssmState*ssmInner
	return kvHeadLayers, recurrentLayers * floatsPerLayer * 4
}

// maxKVLayoutSSMDim bounds each SSM dimension KVLayout multiplies.
const maxKVLayoutSSMDim = 1 << 16

// maxKVLayoutLayers bounds the per-layer arrays KVLayout builds; llama.cpp's
// own limit is LLAMA_MAX_LAYERS, 512.
const maxKVLayoutLayers = 512

// kvLayerHeads returns layer i's KV head count from kvLayerCounts' result.
func kvLayerHeads(heads []uint64, i int) uint64 {
	if len(heads) == 1 {
		return heads[0]
	}
	return heads[i]
}

// kvLayerCounts reads a per-layer count the way llama.cpp's get_key_or_arr
// does: a scalar applies to every layer, an array gives one value per layer.
// perLayer reports the array form. ok is false when the key is absent,
// negative, or an array whose length is not one per layer.
func kvLayerCounts(v GGUFValue, found bool, n uint64) (counts []uint64, perLayer, ok bool) {
	if !found {
		return nil, false, false
	}
	if c, ok := kvCount(v); ok {
		return []uint64{c}, false, true
	}
	arr, isArr := v.(ArrayVal)
	if !isArr || uint64(len(arr.Values)) != n {
		return nil, false, false
	}
	counts = make([]uint64, len(arr.Values))
	for i, e := range arr.Values {
		c, ok := kvCount(e)
		if !ok {
			return nil, false, false
		}
		counts[i] = c
	}
	return counts, true, true
}

// kvLayerBools fills out from a bool, or an array of one bool per layer.
func kvLayerBools(v GGUFValue, out []bool) bool {
	if b, ok := v.(BoolVal); ok {
		for i := range out {
			out[i] = b.Value
		}
		return true
	}
	arr, isArr := v.(ArrayVal)
	if !isArr || len(arr.Values) != len(out) {
		return false
	}
	for i, e := range arr.Values {
		b, ok := e.(BoolVal)
		if !ok {
			return false
		}
		out[i] = b.Value
	}
	return true
}

// kvCount reads a non-negative integer of any width. GGUF converters write
// unsigned values for most counts but signed ones for some per-layer arrays
// (Nemotron Super's head counts are int32).
func kvCount(v GGUFValue) (uint64, bool) {
	if u, ok := AsU64(v); ok {
		return u, true
	}
	var s int64
	switch x := v.(type) {
	case Int8Val:
		s = int64(x.Value)
	case Int16Val:
		s = int64(x.Value)
	case Int32Val:
		s = int64(x.Value)
	case Int64Val:
		s = x.Value
	default:
		return 0, false
	}
	if s < 0 {
		return 0, false
	}
	return uint64(s), true
}

// kvCountKey is kvCount for a lookup that may have missed.
func kvCountKey(v GGUFValue, found bool) (uint64, bool) {
	if !found {
		return 0, false
	}
	return kvCount(v)
}

// kvHasKeyPart reports whether any <arch>.* key contains one of parts.
func kvHasKeyPart(f *GGUFFile, arch string, parts ...string) bool {
	for _, kv := range f.Metadata {
		if !strings.HasPrefix(kv.Key, arch+".") {
			continue
		}
		for _, p := range parts {
			if strings.Contains(kv.Key, p) {
				return true
			}
		}
	}
	return false
}
