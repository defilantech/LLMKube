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

import (
	"bytes"
	"testing"
)

// layoutFile parses a header carrying arch.* metadata, the shape the
// controller and the agent both read a layout from.
func layoutFile(t *testing.T, arch string, kvs []metadataEntry) *GGUFFile {
	t.Helper()
	entries := append([]metadataEntry{
		{key: "general.architecture", value: testString{s: arch}},
	}, kvs...)
	f, err := Parse(bytes.NewReader(buildGGUF(entries, 0)))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return f
}

// A qwen35 hybrid keeps a KV cache only on its full-attention layers: layer i
// with (i+1) % 4 == 0, so 16 of 64, and the 48 others hold recurrent state
// instead. The controller records both numbers on the Model status, so this is
// the value a first deploy is sized from.
func TestKVLayout_HybridKVCacheCoversFullAttentionLayers(t *testing.T) {
	f := layoutFile(t, "qwen35", []metadataEntry{
		{key: "qwen35.block_count", value: testUint32{v: 64}},
		{key: "qwen35.embedding_length", value: testUint32{v: 5120}},
		{key: "qwen35.attention.head_count", value: testUint32{v: 24}},
		{key: "qwen35.attention.head_count_kv", value: testUint32{v: 4}},
		{key: "qwen35.attention.key_length", value: testUint32{v: 256}},
		{key: "qwen35.attention.value_length", value: testUint32{v: 256}},
		{key: "qwen35.ssm.conv_kernel", value: testUint32{v: 4}},
		{key: "qwen35.ssm.state_size", value: testUint32{v: 128}},
		{key: "qwen35.ssm.group_count", value: testUint32{v: 16}},
		{key: "qwen35.ssm.inner_size", value: testUint32{v: 6144}},
		{key: "qwen35.full_attention_interval", value: testUint32{v: 4}},
	})

	kvHeadLayers, recurrentStatePerSeq := KVLayout(f)
	if want := uint64(16 * 4); kvHeadLayers != want {
		t.Errorf("kvHeadLayers = %d, want %d (16 full-attention layers of 4 KV heads)", kvHeadLayers, want)
	}
	if want := uint64(48 * (3*(6144+2*16*128) + 128*6144) * 4); recurrentStatePerSeq != want {
		t.Errorf("recurrentStatePerSeq = %d, want %d (48 recurrent layers of f32 conv + SSM state)",
			recurrentStatePerSeq, want)
	}
}

// A dense model has the plain layout and records nothing extra: the KV term
// stays block_count layers of head_count_kv heads.
func TestKVLayout_DenseModelHasNoLayout(t *testing.T) {
	f := layoutFile(t, "llama", []metadataEntry{
		{key: "llama.block_count", value: testUint32{v: 32}},
		{key: "llama.embedding_length", value: testUint32{v: 4096}},
		{key: "llama.attention.head_count", value: testUint32{v: 32}},
		{key: "llama.attention.head_count_kv", value: testUint32{v: 8}},
		{key: "llama.attention.key_length", value: testUint32{v: 128}},
		{key: "llama.attention.value_length", value: testUint32{v: 128}},
	})

	if kvHeadLayers, recurrentStatePerSeq := KVLayout(f); kvHeadLayers != 0 || recurrentStatePerSeq != 0 {
		t.Errorf("KVLayout = (%d, %d), want (0, 0) for a dense model",
			kvHeadLayers, recurrentStatePerSeq)
	}
}

// <arch>.attention.recurrent_layers is a per-layer bool array, and it is the
// layout whenever the file carries it: the interval is not consulted.
func TestKVLayout_RecurrentLayersArrayMarksTheLayout(t *testing.T) {
	recurrent := []testValue{
		testBool{v: false},
		testBool{v: true}, testBool{v: true}, testBool{v: true}, testBool{v: true},
		testBool{v: true}, testBool{v: true}, testBool{v: true},
	}
	f := layoutFile(t, "qwen35", []metadataEntry{
		{key: "qwen35.block_count", value: testUint32{v: 8}},
		{key: "qwen35.embedding_length", value: testUint32{v: 1024}},
		{key: "qwen35.attention.head_count", value: testUint32{v: 8}},
		{key: "qwen35.attention.head_count_kv", value: testUint32{v: 2}},
		{key: "qwen35.attention.key_length", value: testUint32{v: 128}},
		{key: "qwen35.attention.value_length", value: testUint32{v: 128}},
		{key: "qwen35.ssm.conv_kernel", value: testUint32{v: 4}},
		{key: "qwen35.ssm.state_size", value: testUint32{v: 64}},
		{key: "qwen35.ssm.inner_size", value: testUint32{v: 512}},
		{key: "qwen35.full_attention_interval", value: testUint32{v: 4}}, // would mark layers 3 and 7
		{key: "qwen35.attention.recurrent_layers", value: testArray{elements: recurrent}},
	})

	kvHeadLayers, recurrentStatePerSeq := KVLayout(f)
	if want := uint64(1 * 2); kvHeadLayers != want {
		t.Errorf("kvHeadLayers = %d, want %d (only layer 0 is an attention layer)", kvHeadLayers, want)
	}
	if want := uint64(7 * (3*(512+0) + 64*512) * 4); recurrentStatePerSeq != want {
		t.Errorf("recurrentStatePerSeq = %d, want %d (7 recurrent layers, no group_count)",
			recurrentStatePerSeq, want)
	}
}
