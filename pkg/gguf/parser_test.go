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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test data builders
// ---------------------------------------------------------------------------

// writeLE wraps binary.Write for test helpers; panics on error
// (bytes.Buffer.Write never fails, but errcheck requires handling).
func writeLE(buf *bytes.Buffer, data interface{}) {
	if err := binary.Write(buf, binary.LittleEndian, data); err != nil {
		panic(fmt.Sprintf("binary.Write failed: %v", err))
	}
}

type testValue interface {
	writeWithTag(buf *bytes.Buffer)
	writeData(buf *bytes.Buffer)
	typeTag() uint32
}

type testString struct{ s string }
type testUint32 struct{ v uint32 }
type testBool struct{ v bool }
type testArray struct{ elements []testValue }

func (t testString) typeTag() uint32 { return 8 }
func (t testUint32) typeTag() uint32 { return 4 }
func (t testBool) typeTag() uint32   { return 7 }
func (t testArray) typeTag() uint32  { return 9 }

func (t testString) writeWithTag(buf *bytes.Buffer) {
	writeLE(buf, uint32(8))
	writeGGUFString(buf, t.s)
}
func (t testString) writeData(buf *bytes.Buffer) {
	writeGGUFString(buf, t.s)
}

func (t testUint32) writeWithTag(buf *bytes.Buffer) {
	writeLE(buf, uint32(4))
	writeLE(buf, t.v)
}
func (t testUint32) writeData(buf *bytes.Buffer) {
	writeLE(buf, t.v)
}

type testUint64 struct{ v uint64 }

func (t testUint64) typeTag() uint32 { return 10 }

func (t testUint64) writeWithTag(buf *bytes.Buffer) {
	writeLE(buf, uint32(10))
	writeLE(buf, t.v)
}
func (t testUint64) writeData(buf *bytes.Buffer) {
	writeLE(buf, t.v)
}

func (t testBool) writeWithTag(buf *bytes.Buffer) {
	writeLE(buf, uint32(7))
	if t.v {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
}
func (t testBool) writeData(buf *bytes.Buffer) {
	if t.v {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
}

func (t testArray) writeWithTag(buf *bytes.Buffer) {
	writeLE(buf, uint32(9)) // ARRAY type tag
	var elemType uint32 = 4 // default uint32
	if len(t.elements) > 0 {
		elemType = t.elements[0].typeTag()
	}
	writeLE(buf, elemType)
	writeLE(buf, uint64(len(t.elements)))
	for _, elem := range t.elements {
		elem.writeData(buf) // no per-element type tag
	}
}
func (t testArray) writeData(buf *bytes.Buffer) {
	// Arrays don't appear as nested elements in our tests
}

func writeGGUFString(buf *bytes.Buffer, s string) {
	writeLE(buf, uint64(len(s)))
	buf.WriteString(s)
}

type metadataEntry struct {
	key   string
	value testValue
}

// buildGGUF constructs a minimal valid GGUF byte buffer.
func buildGGUF(metadata []metadataEntry, tensorCount uint64) []byte {
	buf := &bytes.Buffer{}

	// Header
	writeLE(buf, uint32(0x46554747)) // magic
	writeLE(buf, uint32(3))          // version
	writeLE(buf, tensorCount)
	writeLE(buf, uint64(len(metadata)))

	// Metadata KV pairs
	for _, kv := range metadata {
		writeGGUFString(buf, kv.key)
		kv.value.writeWithTag(buf)
	}

	// Tensor info entries (minimal: 1D tensors of type F32)
	for i := uint64(0); i < tensorCount; i++ {
		name := fmt.Sprintf("tensor.%d", i)
		writeGGUFString(buf, name)
		writeLE(buf, uint32(1))   // n_dimensions = 1
		writeLE(buf, uint64(128)) // dimension[0] = 128
		writeLE(buf, uint32(0))   // type = F32
		writeLE(buf, i*512)       // offset
	}

	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestParseValidHeader(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(0x46554747)) // magic
	writeLE(buf, uint32(3))          // version
	writeLE(buf, uint64(10))         // tensor_count
	writeLE(buf, uint64(5))          // metadata_kv_count

	header, err := parseHeader(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if header.Version != 3 {
		t.Errorf("version = %d, want 3", header.Version)
	}
	if header.TensorCount != 10 {
		t.Errorf("tensor_count = %d, want 10", header.TensorCount)
	}
	if header.MetadataKVCount != 5 {
		t.Errorf("metadata_kv_count = %d, want 5", header.MetadataKVCount)
	}
}

func TestInvalidMagic(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(0xDEADBEEF))
	writeLE(buf, uint32(3))
	writeLE(buf, uint64(0))
	writeLE(buf, uint64(0))

	_, err := parseHeader(buf)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInvalidMagic) {
		t.Errorf("expected ErrInvalidMagic, got: %v", err)
	}
}

func TestUnsupportedVersion(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(0x46554747))
	writeLE(buf, uint32(99))
	writeLE(buf, uint64(0))
	writeLE(buf, uint64(0))

	_, err := parseHeader(buf)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("expected ErrUnsupportedVersion, got: %v", err)
	}
}

func TestReadString(t *testing.T) {
	buf := &bytes.Buffer{}
	s := "hello, gguf!"
	writeLE(buf, uint64(len(s)))
	buf.WriteString(s)

	result, err := readString(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "hello, gguf!" {
		t.Errorf("string = %q, want %q", result, "hello, gguf!")
	}
}

func TestParseStringValue(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(8)) // STRING type tag
	writeLE(buf, uint64(5)) // length
	buf.WriteString("llama")

	value, err := readValue(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, ok := AsStr(value)
	if !ok {
		t.Fatalf("expected StringVal, got %T", value)
	}
	if s != "llama" {
		t.Errorf("value = %q, want %q", s, "llama")
	}
}

func TestParseUint32Value(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(4)) // UINT32 type tag
	writeLE(buf, uint32(4096))

	value, err := readValue(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n, ok := AsU32(value)
	if !ok {
		t.Fatalf("expected Uint32Val, got %T", value)
	}
	if n != 4096 {
		t.Errorf("value = %d, want 4096", n)
	}
}

func TestTruncatedInput(t *testing.T) {
	// Only 2 bytes — not enough for a u32 magic number
	buf := bytes.NewReader([]byte{0x47, 0x47})

	_, err := parseHeader(buf)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Should be an io error (EOF or unexpected EOF)
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.EOF or io.ErrUnexpectedEOF, got: %v", err)
	}
}

func TestParseArrayValue(t *testing.T) {
	data := buildGGUF([]metadataEntry{
		{
			key: "tokenizer.ggml.tokens",
			value: testArray{elements: []testValue{
				testString{s: "hello"},
				testString{s: "world"},
				testString{s: "test"},
			}},
		},
	}, 0)

	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, ok := gguf.GetMetadata("tokenizer.ggml.tokens")
	if !ok {
		t.Fatal("metadata key not found")
	}
	arr, ok := AsArray(v)
	if !ok {
		t.Fatalf("expected ArrayVal, got %T", v)
	}
	if len(arr) != 3 {
		t.Fatalf("array len = %d, want 3", len(arr))
	}

	for i, expected := range []string{"hello", "world", "test"} {
		s, ok := AsStr(arr[i])
		if !ok {
			t.Errorf("element %d: expected StringVal, got %T", i, arr[i])
			continue
		}
		if s != expected {
			t.Errorf("element %d = %q, want %q", i, s, expected)
		}
	}
}

func TestParseBoolValue(t *testing.T) {
	data := buildGGUF([]metadataEntry{
		{key: "general.little_endian", value: testBool{v: true}},
	}, 0)

	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, ok := gguf.GetMetadata("general.little_endian")
	if !ok {
		t.Fatal("metadata key not found")
	}
	b, ok := AsBool(v)
	if !ok {
		t.Fatalf("expected BoolVal, got %T", v)
	}
	if !b {
		t.Error("expected true, got false")
	}
}

func TestParseFullFile(t *testing.T) {
	data := buildGGUF([]metadataEntry{
		{key: "general.architecture", value: testString{s: "llama"}},
		{key: "general.name", value: testString{s: "Llama 3.1 8B Instruct"}},
		{key: "general.file_type", value: testUint32{v: 17}},
		{key: "llama.context_length", value: testUint32{v: 131072}},
		{key: "llama.embedding_length", value: testUint32{v: 4096}},
		{key: "llama.block_count", value: testUint32{v: 32}},
		{key: "llama.attention.head_count", value: testUint32{v: 32}},
	}, 5)

	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gguf.Architecture() != "llama" {
		t.Errorf("architecture = %q, want %q", gguf.Architecture(), "llama")
	}
	if gguf.Name() != "Llama 3.1 8B Instruct" {
		t.Errorf("name = %q, want %q", gguf.Name(), "Llama 3.1 8B Instruct")
	}
	if gguf.Quantization() != "Q5_K_M" {
		t.Errorf("quantization = %q, want %q", gguf.Quantization(), "Q5_K_M")
	}
	if gguf.ContextLength() != 131072 {
		t.Errorf("context_length = %d, want 131072", gguf.ContextLength())
	}
	if gguf.EmbeddingLength() != 4096 {
		t.Errorf("embedding_length = %d, want 4096", gguf.EmbeddingLength())
	}
	if gguf.BlockCount() != 32 {
		t.Errorf("block_count = %d, want 32", gguf.BlockCount())
	}
	if gguf.HeadCount() != 32 {
		t.Errorf("head_count = %d, want 32", gguf.HeadCount())
	}
	if len(gguf.TensorInfo) != 5 {
		t.Errorf("tensor count = %d, want 5", len(gguf.TensorInfo))
	}
}

// TestGQAAttentionAccessors covers the grouped-query-attention metadata
// accessors added for the metal-agent KV estimate (#2085): scalar reads for
// both integer widths, absent keys reading as 0, and an array-valued
// head_count_kv reading as 0 rather than a guess.
func TestGQAAttentionAccessors(t *testing.T) {
	gqaMeta := []metadataEntry{
		{key: "general.architecture", value: testString{s: "llama"}},
		{key: "llama.attention.head_count", value: testUint32{v: 32}},
		{key: "llama.attention.head_count_kv", value: testUint32{v: 8}},
		{key: "llama.attention.key_length", value: testUint32{v: 128}},
		{key: "llama.attention.value_length", value: testUint32{v: 128}},
	}
	data := buildGGUF(gqaMeta, 0)
	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gguf.HeadCountKV() != 8 {
		t.Errorf("head_count_kv = %d, want 8", gguf.HeadCountKV())
	}
	if gguf.KeyLength() != 128 {
		t.Errorf("key_length = %d, want 128", gguf.KeyLength())
	}
	if gguf.ValueLength() != 128 {
		t.Errorf("value_length = %d, want 128", gguf.ValueLength())
	}

	// The same keys written as uint64 must read through AsU64 as well.
	u64Meta := []metadataEntry{
		{key: "general.architecture", value: testString{s: "llama"}},
		{key: "llama.attention.head_count", value: testUint64{v: 32}},
		{key: "llama.attention.head_count_kv", value: testUint64{v: 8}},
		{key: "llama.attention.key_length", value: testUint64{v: 128}},
		{key: "llama.attention.value_length", value: testUint64{v: 128}},
	}
	data = buildGGUF(u64Meta, 0)
	gguf, err = Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gguf.HeadCountKV() != 8 || gguf.KeyLength() != 128 || gguf.ValueLength() != 128 {
		t.Errorf("uint64 attention keys = (%d, %d, %d), want (8, 128, 128)",
			gguf.HeadCountKV(), gguf.KeyLength(), gguf.ValueLength())
	}

	// Keys absent entirely: 0, never a substitute value.
	bareMeta := []metadataEntry{
		{key: "general.architecture", value: testString{s: "llama"}},
		{key: "llama.attention.head_count", value: testUint32{v: 32}},
	}
	data = buildGGUF(bareMeta, 0)
	gguf, err = Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gguf.HeadCountKV() != 0 || gguf.KeyLength() != 0 || gguf.ValueLength() != 0 {
		t.Errorf("absent attention keys = (%d, %d, %d), want (0, 0, 0)",
			gguf.HeadCountKV(), gguf.KeyLength(), gguf.ValueLength())
	}

	// No general.architecture at all: every accessor must return 0 rather
	// than read a ".attention.key" key with an empty prefix.
	noArchMeta := []metadataEntry{
		{key: "general.name", value: testString{s: "arch-less"}},
	}
	data = buildGGUF(noArchMeta, 0)
	gguf, err = Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gguf.HeadCountKV() != 0 || gguf.KeyLength() != 0 || gguf.ValueLength() != 0 {
		t.Errorf("accessors without architecture = (%d, %d, %d), want all 0",
			gguf.HeadCountKV(), gguf.KeyLength(), gguf.ValueLength())
	}

	// head_count_kv stored per layer as an array (Jamba, Nemotron-H style):
	// must read as unknown (0), not as a guess from head_count.
	arrMeta := []metadataEntry{
		{key: "general.architecture", value: testString{s: "jamba"}},
		{key: "jamba.attention.head_count", value: testUint32{v: 32}},
		{key: "jamba.attention.head_count_kv", value: testArray{elements: []testValue{
			testUint32{v: 8}, testUint32{v: 8}, testUint32{v: 0},
		}}},
	}
	data = buildGGUF(arrMeta, 0)
	gguf, err = Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gguf.HeadCountKV() != 0 {
		t.Errorf("array head_count_kv = %d, want 0 (unknown)", gguf.HeadCountKV())
	}
}

func TestFileTypeName(t *testing.T) {
	tests := []struct {
		fileType uint32
		want     string
	}{
		{0, "F32"},
		{1, "F16"},
		{2, "Q4_0"},
		{3, "Q4_1"},
		{7, "Q8_0"},
		{8, "Q5_0"},
		{9, "Q5_1"},
		{10, "Q2_K"},
		{11, "Q3_K_S"},
		{12, "Q3_K_M"},
		{13, "Q3_K_L"},
		{14, "Q4_K_S"},
		{15, "Q4_K_M"},
		{16, "Q5_K_S"},
		{17, "Q5_K_M"},
		{18, "Q6_K"},
		{19, "IQ2_XXS"},
		{20, "IQ2_XS"},
		{21, "IQ3_XXS"},
		{22, "IQ1_S"},
		{23, "IQ4_NL"},
		{24, "IQ3_S"},
		{25, "IQ2_S"},
		{26, "IQ4_XS"},
		{27, "IQ3_M"},
		{28, "IQ1_M"},
		{29, "BF16"},
		{30, "Q4_0_4_4"},
		{31, "Q4_0_4_8"},
		{32, "Q4_0_8_8"},
		{999, "Unknown"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("type_%d", tt.fileType), func(t *testing.T) {
			got := FileTypeName(tt.fileType)
			if got != tt.want {
				t.Errorf("FileTypeName(%d) = %q, want %q", tt.fileType, got, tt.want)
			}
		})
	}
}

func TestConvenienceMethods(t *testing.T) {
	data := buildGGUF([]metadataEntry{
		{key: "general.architecture", value: testString{s: "phi"}},
		{key: "general.file_type", value: testUint32{v: 15}},
		{key: "general.license", value: testString{s: "Apache-2.0"}},
		{key: "phi.context_length", value: testUint32{v: 2048}},
		{key: "phi.embedding_length", value: testUint32{v: 2560}},
		{key: "phi.block_count", value: testUint32{v: 24}},
		{key: "phi.attention.head_count", value: testUint32{v: 32}},
	}, 0)

	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gguf.Architecture() != "phi" {
		t.Errorf("architecture = %q, want %q", gguf.Architecture(), "phi")
	}
	if gguf.Quantization() != "Q4_K_M" {
		t.Errorf("quantization = %q, want %q", gguf.Quantization(), "Q4_K_M")
	}
	if gguf.ContextLength() != 2048 {
		t.Errorf("context_length = %d, want 2048", gguf.ContextLength())
	}
	if gguf.EmbeddingLength() != 2560 {
		t.Errorf("embedding_length = %d, want 2560", gguf.EmbeddingLength())
	}
	if gguf.BlockCount() != 24 {
		t.Errorf("block_count = %d, want 24", gguf.BlockCount())
	}
	if gguf.HeadCount() != 32 {
		t.Errorf("head_count = %d, want 32", gguf.HeadCount())
	}
	if gguf.License() != "Apache-2.0" {
		t.Errorf("license = %q, want %q", gguf.License(), "Apache-2.0")
	}
}

func TestParseEmptyGGUF(t *testing.T) {
	data := buildGGUF(nil, 0)
	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gguf.Header.Version != 3 {
		t.Errorf("version = %d, want 3", gguf.Header.Version)
	}
	if gguf.Header.TensorCount != 0 {
		t.Errorf("tensor_count = %d, want 0", gguf.Header.TensorCount)
	}
	if len(gguf.Metadata) != 0 {
		t.Errorf("metadata len = %d, want 0", len(gguf.Metadata))
	}
	if len(gguf.TensorInfo) != 0 {
		t.Errorf("tensor_info len = %d, want 0", len(gguf.TensorInfo))
	}
}

func TestMissingMetadataReturnsZero(t *testing.T) {
	data := buildGGUF(nil, 0)
	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gguf.Architecture() != "" {
		t.Errorf("architecture = %q, want empty", gguf.Architecture())
	}
	if gguf.Name() != "" {
		t.Errorf("name = %q, want empty", gguf.Name())
	}
	if gguf.Quantization() != "" {
		t.Errorf("quantization = %q, want empty", gguf.Quantization())
	}
	if gguf.ContextLength() != 0 {
		t.Errorf("context_length = %d, want 0", gguf.ContextLength())
	}
	if gguf.License() != "" {
		t.Errorf("license = %q, want empty", gguf.License())
	}
}

func TestParseTensorInfo(t *testing.T) {
	data := buildGGUF(nil, 3)
	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gguf.TensorInfo) != 3 {
		t.Fatalf("tensor count = %d, want 3", len(gguf.TensorInfo))
	}

	if gguf.TensorInfo[0].Name != "tensor.0" {
		t.Errorf("tensor[0].name = %q, want %q", gguf.TensorInfo[0].Name, "tensor.0")
	}
	if len(gguf.TensorInfo[0].Dimensions) != 1 || gguf.TensorInfo[0].Dimensions[0] != 128 {
		t.Errorf("tensor[0].dimensions = %v, want [128]", gguf.TensorInfo[0].Dimensions)
	}
	if gguf.TensorInfo[0].Type != GGMLTypeF32 {
		t.Errorf("tensor[0].type = %v, want F32", gguf.TensorInfo[0].Type)
	}
	if gguf.TensorInfo[0].Offset != 0 {
		t.Errorf("tensor[0].offset = %d, want 0", gguf.TensorInfo[0].Offset)
	}

	if gguf.TensorInfo[1].Name != "tensor.1" {
		t.Errorf("tensor[1].name = %q, want %q", gguf.TensorInfo[1].Name, "tensor.1")
	}
	if gguf.TensorInfo[1].Offset != 512 {
		t.Errorf("tensor[1].offset = %d, want 512", gguf.TensorInfo[1].Offset)
	}

	if gguf.TensorInfo[2].Name != "tensor.2" {
		t.Errorf("tensor[2].name = %q, want %q", gguf.TensorInfo[2].Name, "tensor.2")
	}
	if gguf.TensorInfo[2].Offset != 1024 {
		t.Errorf("tensor[2].offset = %d, want 1024", gguf.TensorInfo[2].Offset)
	}
}

func TestRejectEmptyFile(t *testing.T) {
	_, err := Parse(bytes.NewReader([]byte{}))
	if err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
}

func TestRejectOversizedString(t *testing.T) {
	buf := &bytes.Buffer{}
	// Write a string length that exceeds the safety limit
	writeLE(buf, uint64(maxStringLength+1))

	_, err := readString(buf)
	if err == nil {
		t.Fatal("expected error for oversized string, got nil")
	}
	if !errors.Is(err, ErrSizeLimitExceeded) {
		t.Errorf("expected ErrSizeLimitExceeded, got: %v", err)
	}
}

func TestRejectOversizedArray(t *testing.T) {
	buf := &bytes.Buffer{}
	// Type tag: ARRAY
	writeLE(buf, uint32(9))
	// Element type: UINT32
	writeLE(buf, uint32(4))
	// Count: exceeds limit
	writeLE(buf, uint64(maxArrayCount+1))

	_, err := readValue(buf)
	if err == nil {
		t.Fatal("expected error for oversized array, got nil")
	}
	if !errors.Is(err, ErrSizeLimitExceeded) {
		t.Errorf("expected ErrSizeLimitExceeded, got: %v", err)
	}
}

// An array of arrays is refused at the first nested element type, before any
// element is read: ggml's own reader refuses it as an invalid type, so no
// file llama-server loads carries one, and following the nesting recursed
// once per 12 header bytes with no bound (a fatal stack overflow, which
// recover cannot catch).
func TestRejectNestedArray(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(9)) // ARRAY
	for range 100_000 {
		writeLE(buf, uint32(9)) // element type: ARRAY
		writeLE(buf, uint64(1)) // count
	}
	before := buf.Len()

	_, err := readValue(buf)
	if !errors.Is(err, ErrNestedArray) {
		t.Fatalf("readValue(array of arrays) error = %v, want ErrNestedArray", err)
	}
	// The outer type tag and the first element type, nothing past them.
	if consumed := before - buf.Len(); consumed != 8 {
		t.Errorf("consumed %d bytes, want 8 (the outer type tag and the nested element type)", consumed)
	}
}

// An array header's count is untrusted: a header that claims the maximum
// element count and then ends must not allocate storage for all of them
// before the first element is read.
func TestArrayCountDoesNotPreallocate(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(9)) // ARRAY
	writeLE(buf, uint32(0)) // element type: UINT8
	writeLE(buf, uint64(maxArrayCount))
	data := buf.Bytes()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := readValue(bytes.NewReader(data))
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("readValue of a truncated array succeeded, want an error")
	}
	// maxArrayCount interface slots are 160 MB; the capped preallocation
	// is 1 MiB.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Errorf("a 16-byte truncated array header allocated %d bytes, want under 16 MiB", alloc)
	}
}

func TestRejectOversizedDimensions(t *testing.T) {
	buf := &bytes.Buffer{}
	// Tensor name
	writeGGUFString(buf, "bad_tensor")
	// n_dimensions: exceeds limit
	writeLE(buf, uint32(maxDimensions+1))

	_, err := parseTensorInfo(buf)
	if err == nil {
		t.Fatal("expected error for oversized dimensions, got nil")
	}
	if !errors.Is(err, ErrSizeLimitExceeded) {
		t.Errorf("expected ErrSizeLimitExceeded, got: %v", err)
	}
}

func TestGGMLTypeString(t *testing.T) {
	if GGMLTypeF32.String() != "F32" {
		t.Errorf("F32.String() = %q", GGMLTypeF32.String())
	}
	if GGMLTypeQ4K.String() != "Q4_K" {
		t.Errorf("Q4K.String() = %q", GGMLTypeQ4K.String())
	}
	unknown := GGMLType(999)
	if unknown.String() != "Unknown(999)" {
		t.Errorf("Unknown.String() = %q", unknown.String())
	}
}

// ---------------------------------------------------------------------------
// Remote header-only read
// ---------------------------------------------------------------------------

// fixtureMetadata is the metadata used by the remote-read fixtures, shared so
// the local-vs-remote comparison asserts on the same expected values.
func fixtureMetadata() []metadataEntry {
	return []metadataEntry{
		{key: "general.architecture", value: testString{s: "llama"}},
		{key: "general.name", value: testString{s: "Llama 3.1 8B Instruct"}},
		{key: "general.file_type", value: testUint32{v: 17}},
		{key: "llama.context_length", value: testUint32{v: 131072}},
		{key: "llama.embedding_length", value: testUint32{v: 4096}},
		{key: "llama.block_count", value: testUint32{v: 32}},
		{key: "llama.attention.head_count", value: testUint32{v: 32}},
	}
}

// buildGGUFWithTensorData builds a GGUF whose header+metadata+tensor-info is
// small but whose total file size is large (tensor data padding). This lets a
// header-only remote read prove it transfers far fewer bytes than the file.
func buildGGUFWithTensorData(metadata []metadataEntry, tensorCount uint64, tensorDataBytes int) []byte {
	header := buildGGUF(metadata, tensorCount)
	out := make([]byte, len(header)+tensorDataBytes)
	copy(out, header)
	// Padding stays zero — it is never read by a header-only parse.
	return out
}

func TestParseFromURL_HeaderOnly(t *testing.T) {
	// Small header section, ~4 MB of trailing tensor data.
	const tensorDataBytes = 4 << 20
	data := buildGGUFWithTensorData(fixtureMetadata(), 5, tensorDataBytes)

	// Local parse is the ground truth.
	local, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("local parse failed: %v", err)
	}

	srv, served, supportsRange := newRangeFileServer(t, data)
	defer srv.Close()
	_ = supportsRange

	remote, err := ParseFromURL(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("ParseFromURL failed: %v", err)
	}

	assertSameMetadata(t, local, remote)

	got := served()
	if got >= int64(len(data)) {
		t.Fatalf("served %d bytes, expected materially fewer than file size %d", got, len(data))
	}
	if got >= int64(len(data))/2 {
		t.Fatalf("served %d bytes, expected < half of file size %d (not header-only)", got, len(data))
	}
}

func TestParseFromURL_NoRangeSupport(t *testing.T) {
	const tensorDataBytes = 4 << 20
	data := buildGGUFWithTensorData(fixtureMetadata(), 5, tensorDataBytes)

	local, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("local parse failed: %v", err)
	}

	srv, _, _ := newNoRangeFileServer(t, data)
	defer srv.Close()

	// Count bytes the client actually pulls through the body, independent of
	// how aggressively the server (or the loopback socket buffer) pushes. This
	// is what the parser controls: it stops reading once the metadata and
	// tensor-info section is consumed and never touches the tensor data.
	var consumed atomic.Int64
	client := &http.Client{Transport: &countingTransport{rt: http.DefaultTransport, n: &consumed}}

	remote, err := parseFromURL(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("ParseFromURL (no range) failed: %v", err)
	}

	assertSameMetadata(t, local, remote)

	got := consumed.Load()
	if got >= int64(len(data)) {
		t.Fatalf("client consumed %d bytes, expected to stop before reading the full file %d", got, len(data))
	}
	if got >= int64(len(data))/2 {
		t.Fatalf("client consumed %d bytes, expected < half of file size %d (not header-only)", got, len(data))
	}
}

// countingTransport wraps an http.RoundTripper and counts the bytes read from
// every response body it returns.
type countingTransport struct {
	rt http.RoundTripper
	n  *atomic.Int64
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	resp.Body = &countingReadCloser{rc: resp.Body, n: c.n}
	return resp, nil
}

type countingReadCloser struct {
	rc io.ReadCloser
	n  *atomic.Int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingReadCloser) Close() error { return c.rc.Close() }

func assertSameMetadata(t *testing.T, want, got *GGUFFile) {
	t.Helper()
	if got.Architecture() != want.Architecture() {
		t.Errorf("architecture = %q, want %q", got.Architecture(), want.Architecture())
	}
	if got.Name() != want.Name() {
		t.Errorf("name = %q, want %q", got.Name(), want.Name())
	}
	if got.Quantization() != want.Quantization() {
		t.Errorf("quantization = %q, want %q", got.Quantization(), want.Quantization())
	}
	if got.ContextLength() != want.ContextLength() {
		t.Errorf("context_length = %d, want %d", got.ContextLength(), want.ContextLength())
	}
	if got.EmbeddingLength() != want.EmbeddingLength() {
		t.Errorf("embedding_length = %d, want %d", got.EmbeddingLength(), want.EmbeddingLength())
	}
	if got.BlockCount() != want.BlockCount() {
		t.Errorf("block_count = %d, want %d", got.BlockCount(), want.BlockCount())
	}
	if got.HeadCount() != want.HeadCount() {
		t.Errorf("head_count = %d, want %d", got.HeadCount(), want.HeadCount())
	}
	if len(got.TensorInfo) != len(want.TensorInfo) {
		t.Errorf("tensor info len = %d, want %d", len(got.TensorInfo), len(want.TensorInfo))
	}
}

// newRangeFileServer serves data with full HTTP Range support and counts body
// bytes written. Returns the server, a func reporting bytes served, and whether
// the server advertised range support.
func newRangeFileServer(t *testing.T, data []byte) (*httptest.Server, func() int64, bool) {
	t.Helper()
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &countingResponseWriter{ResponseWriter: w, n: &served}
		http.ServeContent(cw, r, "model.gguf", time.Time{}, bytes.NewReader(data))
	}))
	return srv, served.Load, true
}

// newNoRangeFileServer serves data WITHOUT range support: it ignores the Range
// header and streams the full body, counting bytes the client actually reads
// (the handler stops writing once the client closes the connection).
func newNoRangeFileServer(t *testing.T, data []byte) (*httptest.Server, func() int64, bool) {
	t.Helper()
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		// Deliberately no Accept-Ranges; write in chunks so a client that stops
		// reading early causes us to stop writing (broken pipe).
		const chunk = 64 * 1024
		for off := 0; off < len(data); off += chunk {
			end := off + chunk
			if end > len(data) {
				end = len(data)
			}
			n, err := w.Write(data[off:end])
			served.Add(int64(n))
			if err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	return srv, served.Load, false
}

type countingResponseWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// TestReadValueScalarTypes pins each scalar decode branch of readValueData:
// both the wire width and the concrete decoded value. A wrong-width read or a
// wrong *Val return type fails its row, not merely the "no error" check.
func TestReadValueScalarTypes(t *testing.T) {
	cases := []struct {
		name   string
		encode func(*bytes.Buffer)
		want   GGUFValue
	}{
		{"uint8", func(b *bytes.Buffer) { writeLE(b, valueTypeUint8); writeLE(b, uint8(200)) }, Uint8Val{Value: 200}},
		{"int8", func(b *bytes.Buffer) { writeLE(b, valueTypeInt8); writeLE(b, int8(-100)) }, Int8Val{Value: -100}},
		{"uint16", func(b *bytes.Buffer) { writeLE(b, valueTypeUint16); writeLE(b, uint16(50000)) }, Uint16Val{Value: 50000}},
		{"int16", func(b *bytes.Buffer) { writeLE(b, valueTypeInt16); writeLE(b, int16(-30000)) }, Int16Val{Value: -30000}},
		{"uint32", func(b *bytes.Buffer) { writeLE(b, valueTypeUint32); writeLE(b, uint32(4096)) }, Uint32Val{Value: 4096}},
		{"int32", func(b *bytes.Buffer) { writeLE(b, valueTypeInt32); writeLE(b, int32(-123456)) }, Int32Val{Value: -123456}},
		{"float32", func(b *bytes.Buffer) { writeLE(b, valueTypeFloat32); writeLE(b, float32(1.5)) }, Float32Val{Value: 1.5}},
		{"bool true", func(b *bytes.Buffer) { writeLE(b, valueTypeBool); writeLE(b, uint8(1)) }, BoolVal{Value: true}},
		{"bool false on a non-1 byte", func(b *bytes.Buffer) {
			writeLE(b, valueTypeBool)
			writeLE(b, uint8(2))
		}, BoolVal{Value: true}},
		{"string", func(b *bytes.Buffer) {
			writeLE(b, valueTypeString)
			writeLE(b, uint64(5))
			b.WriteString("llama")
		}, StringVal{Value: "llama"}},
		{"uint64", func(b *bytes.Buffer) {
			writeLE(b, valueTypeUint64)
			writeLE(b, uint64(1)<<40)
		}, Uint64Val{Value: 1 << 40}},
		{"int64", func(b *bytes.Buffer) {
			writeLE(b, valueTypeInt64)
			writeLE(b, -(int64(1) << 40))
		}, Int64Val{Value: -(int64(1) << 40)}},
		{"float64", func(b *bytes.Buffer) { writeLE(b, valueTypeFloat64); writeLE(b, float64(2.5)) }, Float64Val{Value: 2.5}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			tc.encode(buf)

			got, err := readValue(buf)
			if err != nil {
				t.Fatalf("readValue(%s): unexpected error: %v", tc.name, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("readValue(%s) = %#v, want %#v", tc.name, got, tc.want)
			}
		})
	}
}

// TestReadValueUnknownType pins the default branch: an unrecognised type tag
// must surface ErrUnknownValueType rather than decode garbage or return nil.
func TestReadValueUnknownType(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(99))

	_, err := readValue(buf)
	if !errors.Is(err, ErrUnknownValueType) {
		t.Fatalf("readValue(unknown type) error = %v, want ErrUnknownValueType", err)
	}
}

// TestUnsignedAccessors pins the accessor switch arms: the unsigned widths
// convert, and the mis-typed inputs are rejected rather than silently zeroed.
func TestUnsignedAccessors(t *testing.T) {
	unsigned := []struct {
		name string
		in   GGUFValue
		want uint64
	}{
		{"uint8", Uint8Val{Value: 7}, 7},
		{"uint16", Uint16Val{Value: 700}, 700},
		{"uint32", Uint32Val{Value: 70000}, 70000},
		{"uint64", Uint64Val{Value: 1 << 40}, 1 << 40},
	}
	for _, tc := range unsigned {
		got, ok := AsU64(tc.in)
		if !ok {
			t.Errorf("AsU64(%s) reported not-ok, want a value", tc.name)
			continue
		}
		if got != tc.want {
			t.Errorf("AsU64(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}

	rejected := []struct {
		name string
		call func() bool
	}{
		{"AsU64 rejects a string", func() bool { _, ok := AsU64(StringVal{Value: "x"}); return ok }},
		{"AsU32 rejects a uint64", func() bool { _, ok := AsU32(Uint64Val{Value: 1}); return ok }},
		{"AsStr rejects a uint32", func() bool { _, ok := AsStr(Uint32Val{Value: 1}); return ok }},
		{"AsBool rejects a uint8", func() bool { _, ok := AsBool(Uint8Val{Value: 1}); return ok }},
	}
	for _, tc := range rejected {
		if tc.call() {
			t.Errorf("%s: accepted, want rejected", tc.name)
		}
	}
}
