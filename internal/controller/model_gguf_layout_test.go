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

package controller

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

type layoutKey struct {
	key string
	val uint64
}

// buildTestLayoutGGUF writes a GGUF v3 header with arch.* uint64 keys and no
// tensors, enough for the metadata parse path.
func buildTestLayoutGGUF(arch string, keys []layoutKey) []byte {
	buf := &bytes.Buffer{}
	le := func(v interface{}) { _ = binary.Write(buf, binary.LittleEndian, v) }
	str := func(s string) {
		le(uint64(len(s)))
		buf.WriteString(s)
	}
	strKV := func(k, v string) {
		str(k)
		le(uint32(8)) // string type tag
		str(v)
	}

	le(uint32(0x46554747))    // magic GGUF
	le(uint32(3))             // version
	le(uint64(0))             // tensor count
	le(uint64(1 + len(keys))) // metadata kv count

	strKV("general.architecture", arch)
	for _, kv := range keys {
		str(arch + "." + kv.key)
		le(uint32(10)) // uint64 type tag
		le(kv.val)
	}
	return buf.Bytes()
}

// TestParseGGUFMetadataRecordsKVLayout pins the status carrying the layer
// layout a hybrid needs (#2552). The agent sizes a first deploy from these two
// fields because the file is not on the Mac yet; without them it charges a KV
// cache on every layer and refuses a model that fits. Every parse path (local,
// http(s) and s3) maps through ggufMetadataFromParsed, so this covers all
// three.
func TestParseGGUFMetadataRecordsKVLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hybrid.gguf")
	header := buildTestLayoutGGUF("qwen35moe", []layoutKey{
		{"block_count", 41},
		{"embedding_length", 2048},
		{"attention.head_count", 16},
		{"attention.head_count_kv", 2},
		{"attention.key_length", 256},
		{"attention.value_length", 256},
		{"full_attention_interval", 4},
		{"nextn_predict_layers", 1},
		{"ssm.conv_kernel", 4},
		{"ssm.state_size", 128},
		{"ssm.group_count", 16},
		{"ssm.inner_size", 4096},
	})
	if err := os.WriteFile(path, header, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	meta, err := (&ModelReconciler{}).parseGGUFMetadata(path)
	if err != nil {
		t.Fatalf("parseGGUFMetadata: %v", err)
	}
	if meta.LayerCount != 41 || meta.EmbeddingSize != 2048 {
		t.Fatalf("shape = (%d layers, %d wide), want (41, 2048)", meta.LayerCount, meta.EmbeddingSize)
	}
	// 30 of the 40 layers below the MTP layer are recurrent; the 10 interval
	// layers plus the MTP layer hold 11 * 2 KV head layers. Each recurrent
	// layer keeps (4-1) * (4096 + 2*16*128) + 128*4096 f32 values.
	if want := uint64(22); meta.KVHeadLayers != want {
		t.Errorf("KVHeadLayers = %d, want %d (11 attention layers * 2 heads)", meta.KVHeadLayers, want)
	}
	if want := uint64(65863680); meta.RecurrentStatePerSeq != want {
		t.Errorf("RecurrentStatePerSeq = %d, want %d (30 recurrent layers)", meta.RecurrentStatePerSeq, want)
	}

	// A dense model has the plain layout, so the status records no layout and
	// consumers keep charging block_count layers of head_count_kv heads.
	densePath := filepath.Join(t.TempDir(), "dense.gguf")
	dense := buildTestLayoutGGUF("llama", []layoutKey{
		{"block_count", 32},
		{"embedding_length", 4096},
		{"attention.head_count", 32},
		{"attention.head_count_kv", 8},
	})
	if err := os.WriteFile(densePath, dense, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	meta, err = (&ModelReconciler{}).parseGGUFMetadata(densePath)
	if err != nil {
		t.Fatalf("parseGGUFMetadata(dense): %v", err)
	}
	if meta.KVHeadLayers != 0 || meta.RecurrentStatePerSeq != 0 {
		t.Errorf("dense layout = (%d, %d), want (0, 0)", meta.KVHeadLayers, meta.RecurrentStatePerSeq)
	}
}
