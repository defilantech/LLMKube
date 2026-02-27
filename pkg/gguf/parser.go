/*
Copyright 2025.

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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Magic number: bytes [G, G, U, F] = [0x47, 0x47, 0x55, 0x46] read as little-endian u32.
const ggufMagic uint32 = 0x46554747

// Safety limits to prevent OOM from malicious/corrupt files.
const (
	maxStringLength  = 10 * 1024 * 1024 // 10 MB
	maxArrayCount    = 10_000_000       // 10M elements (large vocabs can be ~200K)
	maxDimensions    = 16               // GGML supports up to 4D in practice
	maxPreallocCount = 65536            // Cap pre-allocation from untrusted header counts
)

// Sentinel errors.
var (
	ErrInvalidMagic       = errors.New("invalid GGUF magic number")
	ErrUnsupportedVersion = errors.New("unsupported GGUF version")
	ErrUnknownValueType   = errors.New("unknown metadata value type")
	ErrSizeLimitExceeded  = errors.New("size limit exceeded")
)

// Value type constants as stored in the GGUF file format.
const (
	valueTypeUint8   uint32 = 0
	valueTypeInt8    uint32 = 1
	valueTypeUint16  uint32 = 2
	valueTypeInt16   uint32 = 3
	valueTypeUint32  uint32 = 4
	valueTypeInt32   uint32 = 5
	valueTypeFloat32 uint32 = 6
	valueTypeBool    uint32 = 7
	valueTypeString  uint32 = 8
	valueTypeArray   uint32 = 9
	valueTypeUint64  uint32 = 10
	valueTypeInt64   uint32 = 11
	valueTypeFloat64 uint32 = 12
)

// ---------------------------------------------------------------------------
// GGUFValue — metadata value interface
// ---------------------------------------------------------------------------

// GGUFValue represents a metadata value from a GGUF file.
type GGUFValue interface {
	String() string
}

type Uint8Val struct{ Value uint8 }
type Int8Val struct{ Value int8 }
type Uint16Val struct{ Value uint16 }
type Int16Val struct{ Value int16 }
type Uint32Val struct{ Value uint32 }
type Int32Val struct{ Value int32 }
type Float32Val struct{ Value float32 }
type BoolVal struct{ Value bool }
type StringVal struct{ Value string }
type ArrayVal struct{ Values []GGUFValue }
type Uint64Val struct{ Value uint64 }
type Int64Val struct{ Value int64 }
type Float64Val struct{ Value float64 }

func (v Uint8Val) String() string   { return fmt.Sprintf("%d", v.Value) }
func (v Int8Val) String() string    { return fmt.Sprintf("%d", v.Value) }
func (v Uint16Val) String() string  { return fmt.Sprintf("%d", v.Value) }
func (v Int16Val) String() string   { return fmt.Sprintf("%d", v.Value) }
func (v Uint32Val) String() string  { return fmt.Sprintf("%d", v.Value) }
func (v Int32Val) String() string   { return fmt.Sprintf("%d", v.Value) }
func (v Float32Val) String() string { return fmt.Sprintf("%g", v.Value) }
func (v BoolVal) String() string    { return fmt.Sprintf("%t", v.Value) }
func (v StringVal) String() string  { return v.Value }
func (v ArrayVal) String() string   { return fmt.Sprintf("[%d elements]", len(v.Values)) }
func (v Uint64Val) String() string  { return fmt.Sprintf("%d", v.Value) }
func (v Int64Val) String() string   { return fmt.Sprintf("%d", v.Value) }
func (v Float64Val) String() string { return fmt.Sprintf("%g", v.Value) }

// AsStr returns the string value if this is a StringVal.
func AsStr(v GGUFValue) (string, bool) {
	if s, ok := v.(StringVal); ok {
		return s.Value, true
	}
	return "", false
}

// AsU64 returns the value as uint64, accepting any unsigned integer type.
func AsU64(v GGUFValue) (uint64, bool) {
	switch val := v.(type) {
	case Uint64Val:
		return val.Value, true
	case Uint32Val:
		return uint64(val.Value), true
	case Uint16Val:
		return uint64(val.Value), true
	case Uint8Val:
		return uint64(val.Value), true
	default:
		return 0, false
	}
}

// AsU32 returns the value if this is a Uint32Val.
func AsU32(v GGUFValue) (uint32, bool) {
	if val, ok := v.(Uint32Val); ok {
		return val.Value, true
	}
	return 0, false
}

// AsBool returns the value if this is a BoolVal.
func AsBool(v GGUFValue) (bool, bool) {
	if val, ok := v.(BoolVal); ok {
		return val.Value, true
	}
	return false, false
}

// AsArray returns the values if this is an ArrayVal.
func AsArray(v GGUFValue) ([]GGUFValue, bool) {
	if val, ok := v.(ArrayVal); ok {
		return val.Values, true
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// GGMLType — tensor data types
// ---------------------------------------------------------------------------

// GGMLType represents the data type of tensor weights.
type GGMLType uint32

const (
	GGMLTypeF32    GGMLType = 0
	GGMLTypeF16    GGMLType = 1
	GGMLTypeQ4_0   GGMLType = 2
	GGMLTypeQ4_1   GGMLType = 3
	GGMLTypeQ5_0   GGMLType = 6
	GGMLTypeQ5_1   GGMLType = 7
	GGMLTypeQ8_0   GGMLType = 8
	GGMLTypeQ8_1   GGMLType = 9
	GGMLTypeQ2K    GGMLType = 10
	GGMLTypeQ3K    GGMLType = 11
	GGMLTypeQ4K    GGMLType = 12
	GGMLTypeQ5K    GGMLType = 13
	GGMLTypeQ6K    GGMLType = 14
	GGMLTypeQ8K    GGMLType = 15
	GGMLTypeIQ2XXS GGMLType = 16
	GGMLTypeIQ2XS  GGMLType = 17
	GGMLTypeIQ3XXS GGMLType = 18
	GGMLTypeIQ1S   GGMLType = 19
	GGMLTypeIQ4NL  GGMLType = 20
	GGMLTypeIQ3S   GGMLType = 21
	GGMLTypeIQ2S   GGMLType = 22
	GGMLTypeIQ4XS  GGMLType = 23
	GGMLTypeI8     GGMLType = 24
	GGMLTypeI16    GGMLType = 25
	GGMLTypeI32    GGMLType = 26
	GGMLTypeI64    GGMLType = 27
	GGMLTypeF64    GGMLType = 28
	GGMLTypeIQ1M   GGMLType = 29
	GGMLTypeBF16   GGMLType = 30
)

var ggmlTypeNames = map[GGMLType]string{
	GGMLTypeF32: "F32", GGMLTypeF16: "F16", GGMLTypeQ4_0: "Q4_0", GGMLTypeQ4_1: "Q4_1",
	GGMLTypeQ5_0: "Q5_0", GGMLTypeQ5_1: "Q5_1", GGMLTypeQ8_0: "Q8_0", GGMLTypeQ8_1: "Q8_1",
	GGMLTypeQ2K: "Q2_K", GGMLTypeQ3K: "Q3_K", GGMLTypeQ4K: "Q4_K", GGMLTypeQ5K: "Q5_K",
	GGMLTypeQ6K: "Q6_K", GGMLTypeQ8K: "Q8_K",
	GGMLTypeIQ2XXS: "IQ2_XXS", GGMLTypeIQ2XS: "IQ2_XS", GGMLTypeIQ3XXS: "IQ3_XXS",
	GGMLTypeIQ1S: "IQ1_S", GGMLTypeIQ4NL: "IQ4_NL", GGMLTypeIQ3S: "IQ3_S",
	GGMLTypeIQ2S: "IQ2_S", GGMLTypeIQ4XS: "IQ4_XS",
	GGMLTypeI8: "I8", GGMLTypeI16: "I16", GGMLTypeI32: "I32", GGMLTypeI64: "I64",
	GGMLTypeF64: "F64", GGMLTypeIQ1M: "IQ1_M", GGMLTypeBF16: "BF16",
}

func (t GGMLType) String() string {
	if name, ok := ggmlTypeNames[t]; ok {
		return name
	}
	return fmt.Sprintf("Unknown(%d)", uint32(t))
}

// GGMLTypeFromID converts a raw uint32 to a GGMLType.
func GGMLTypeFromID(id uint32) GGMLType {
	return GGMLType(id)
}

// ---------------------------------------------------------------------------
// Parsed structures
// ---------------------------------------------------------------------------

// GGUFHeader is the fixed-size header at the start of every GGUF file.
type GGUFHeader struct {
	Version         uint32
	TensorCount     uint64
	MetadataKVCount uint64
}

// MetadataKV is a single key-value pair from the GGUF metadata section.
type MetadataKV struct {
	Key   string
	Value GGUFValue
}

// TensorInfo describes a single tensor (not the tensor data itself).
type TensorInfo struct {
	Name       string
	Dimensions []uint64
	Type       GGMLType
	Offset     uint64
}

// GGUFFile is a fully parsed GGUF file (header + metadata + tensor info).
// Does NOT load tensor data — only the metadata headers.
type GGUFFile struct {
	Header     GGUFHeader
	Metadata   []MetadataKV
	TensorInfo []TensorInfo
}

// ---------------------------------------------------------------------------
// Main parser
// ---------------------------------------------------------------------------

// Parse reads a GGUF file from any reader (file, buffer, network stream).
// This only reads the header, metadata, and tensor info — NOT the tensor data.
func Parse(r io.Reader) (*GGUFFile, error) {
	header, err := parseHeader(r)
	if err != nil {
		return nil, err
	}

	metadata := make([]MetadataKV, 0, min(header.MetadataKVCount, maxPreallocCount))
	for i := uint64(0); i < header.MetadataKVCount; i++ {
		kv, err := parseMetadataKV(r)
		if err != nil {
			return nil, fmt.Errorf("metadata kv %d: %w", i, err)
		}
		metadata = append(metadata, kv)
	}

	tensorInfo := make([]TensorInfo, 0, min(header.TensorCount, maxPreallocCount))
	for i := uint64(0); i < header.TensorCount; i++ {
		ti, err := parseTensorInfo(r)
		if err != nil {
			return nil, fmt.Errorf("tensor info %d: %w", i, err)
		}
		tensorInfo = append(tensorInfo, ti)
	}

	return &GGUFFile{
		Header:     *header,
		Metadata:   metadata,
		TensorInfo: tensorInfo,
	}, nil
}

// ---------------------------------------------------------------------------
// Header parsing
// ---------------------------------------------------------------------------

func parseHeader(r io.Reader) (*GGUFHeader, error) {
	var magic uint32
	if err := binary.Read(r, binary.LittleEndian, &magic); err != nil {
		return nil, fmt.Errorf("reading magic: %w", err)
	}
	if magic != ggufMagic {
		return nil, fmt.Errorf("%w: expected 0x46554747, got 0x%08X", ErrInvalidMagic, magic)
	}

	var version uint32
	if err := binary.Read(r, binary.LittleEndian, &version); err != nil {
		return nil, fmt.Errorf("reading version: %w", err)
	}
	if version < 2 || version > 3 {
		return nil, fmt.Errorf("%w: %d (supported: 2, 3)", ErrUnsupportedVersion, version)
	}

	var tensorCount uint64
	if err := binary.Read(r, binary.LittleEndian, &tensorCount); err != nil {
		return nil, fmt.Errorf("reading tensor count: %w", err)
	}

	var metadataKVCount uint64
	if err := binary.Read(r, binary.LittleEndian, &metadataKVCount); err != nil {
		return nil, fmt.Errorf("reading metadata kv count: %w", err)
	}

	return &GGUFHeader{
		Version:         version,
		TensorCount:     tensorCount,
		MetadataKVCount: metadataKVCount,
	}, nil
}

// ---------------------------------------------------------------------------
// String parsing
// ---------------------------------------------------------------------------

// readString reads a GGUF string: u64 length followed by that many UTF-8 bytes.
func readString(r io.Reader) (string, error) {
	var length uint64
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return "", fmt.Errorf("reading string length: %w", err)
	}
	if length > maxStringLength {
		return "", fmt.Errorf("%w: string length %d exceeds maximum %d", ErrSizeLimitExceeded, length, maxStringLength)
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("reading string data: %w", err)
	}

	return string(buf), nil
}

// ---------------------------------------------------------------------------
// Metadata parsing
// ---------------------------------------------------------------------------

func parseMetadataKV(r io.Reader) (MetadataKV, error) {
	key, err := readString(r)
	if err != nil {
		return MetadataKV{}, fmt.Errorf("reading key: %w", err)
	}

	value, err := readValue(r)
	if err != nil {
		return MetadataKV{}, fmt.Errorf("reading value for %q: %w", key, err)
	}

	return MetadataKV{Key: key, Value: value}, nil
}

// readValue reads a type tag (u32) followed by value data.
func readValue(r io.Reader) (GGUFValue, error) {
	var valueType uint32
	if err := binary.Read(r, binary.LittleEndian, &valueType); err != nil {
		return nil, fmt.Errorf("reading value type: %w", err)
	}
	return readValueData(r, valueType)
}

// readValueData reads value data for a known type (without reading the type tag).
// Separated from readValue because array elements share a type tag declared
// once in the array header.
//
//nolint:gocyclo // Type dispatch on 13 GGUF value types is inherently branchy.
func readValueData(r io.Reader, valueType uint32) (GGUFValue, error) {
	switch valueType {
	case valueTypeUint8:
		var v uint8
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return Uint8Val{Value: v}, nil

	case valueTypeInt8:
		var v int8
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return Int8Val{Value: v}, nil

	case valueTypeUint16:
		var v uint16
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return Uint16Val{Value: v}, nil

	case valueTypeInt16:
		var v int16
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return Int16Val{Value: v}, nil

	case valueTypeUint32:
		var v uint32
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return Uint32Val{Value: v}, nil

	case valueTypeInt32:
		var v int32
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return Int32Val{Value: v}, nil

	case valueTypeFloat32:
		var v float32
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return Float32Val{Value: v}, nil

	case valueTypeBool:
		var v uint8
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return BoolVal{Value: v != 0}, nil

	case valueTypeString:
		s, err := readString(r)
		if err != nil {
			return nil, err
		}
		return StringVal{Value: s}, nil

	case valueTypeArray:
		var elemType uint32
		if err := binary.Read(r, binary.LittleEndian, &elemType); err != nil {
			return nil, fmt.Errorf("reading array element type: %w", err)
		}
		var count uint64
		if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
			return nil, fmt.Errorf("reading array count: %w", err)
		}
		if count > maxArrayCount {
			return nil, fmt.Errorf("%w: array count %d exceeds maximum %d", ErrSizeLimitExceeded, count, maxArrayCount)
		}
		values := make([]GGUFValue, 0, count)
		for i := uint64(0); i < count; i++ {
			v, err := readValueData(r, elemType)
			if err != nil {
				return nil, fmt.Errorf("array element %d: %w", i, err)
			}
			values = append(values, v)
		}
		return ArrayVal{Values: values}, nil

	case valueTypeUint64:
		var v uint64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return Uint64Val{Value: v}, nil

	case valueTypeInt64:
		var v int64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return Int64Val{Value: v}, nil

	case valueTypeFloat64:
		var v float64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return Float64Val{Value: v}, nil

	default:
		return nil, fmt.Errorf("%w: %d", ErrUnknownValueType, valueType)
	}
}

// ---------------------------------------------------------------------------
// Tensor info parsing
// ---------------------------------------------------------------------------

func parseTensorInfo(r io.Reader) (TensorInfo, error) {
	name, err := readString(r)
	if err != nil {
		return TensorInfo{}, fmt.Errorf("reading tensor name: %w", err)
	}

	var nDimensions uint32
	if err := binary.Read(r, binary.LittleEndian, &nDimensions); err != nil {
		return TensorInfo{}, fmt.Errorf("reading dimension count: %w", err)
	}
	if nDimensions > maxDimensions {
		return TensorInfo{}, fmt.Errorf(
			"%w: dimension count %d exceeds maximum %d",
			ErrSizeLimitExceeded, nDimensions, maxDimensions,
		)
	}

	dimensions := make([]uint64, nDimensions)
	for i := uint32(0); i < nDimensions; i++ {
		if err := binary.Read(r, binary.LittleEndian, &dimensions[i]); err != nil {
			return TensorInfo{}, fmt.Errorf("reading dimension %d: %w", i, err)
		}
	}

	var typeID uint32
	if err := binary.Read(r, binary.LittleEndian, &typeID); err != nil {
		return TensorInfo{}, fmt.Errorf("reading tensor type: %w", err)
	}

	var offset uint64
	if err := binary.Read(r, binary.LittleEndian, &offset); err != nil {
		return TensorInfo{}, fmt.Errorf("reading tensor offset: %w", err)
	}

	return TensorInfo{
		Name:       name,
		Dimensions: dimensions,
		Type:       GGMLTypeFromID(typeID),
		Offset:     offset,
	}, nil
}

// ---------------------------------------------------------------------------
// Convenience methods on GGUFFile
// ---------------------------------------------------------------------------

// GetMetadata looks up a metadata value by key.
func (f *GGUFFile) GetMetadata(key string) (GGUFValue, bool) {
	for _, kv := range f.Metadata {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return nil, false
}

// Architecture returns the model architecture (e.g., "llama", "mistral", "phi").
func (f *GGUFFile) Architecture() string {
	v, ok := f.GetMetadata("general.architecture")
	if !ok {
		return ""
	}
	s, ok := AsStr(v)
	if !ok {
		return ""
	}
	return s
}

// Name returns the model name as stored in the file.
func (f *GGUFFile) Name() string {
	v, ok := f.GetMetadata("general.name")
	if !ok {
		return ""
	}
	s, ok := AsStr(v)
	if !ok {
		return ""
	}
	return s
}

// Quantization returns the human-readable quantization name (e.g., "Q4_K_M").
func (f *GGUFFile) Quantization() string {
	v, ok := f.GetMetadata("general.file_type")
	if !ok {
		return ""
	}
	ft, ok := AsU32(v)
	if !ok {
		return ""
	}
	return FileTypeName(ft)
}

// ContextLength returns the model's context length (max tokens).
func (f *GGUFFile) ContextLength() uint64 {
	arch := f.Architecture()
	if arch == "" {
		return 0
	}
	v, ok := f.GetMetadata(arch + ".context_length")
	if !ok {
		return 0
	}
	n, ok := AsU64(v)
	if !ok {
		return 0
	}
	return n
}

// EmbeddingLength returns the embedding dimension size.
func (f *GGUFFile) EmbeddingLength() uint64 {
	arch := f.Architecture()
	if arch == "" {
		return 0
	}
	v, ok := f.GetMetadata(arch + ".embedding_length")
	if !ok {
		return 0
	}
	n, ok := AsU64(v)
	if !ok {
		return 0
	}
	return n
}

// BlockCount returns the number of transformer layers/blocks.
func (f *GGUFFile) BlockCount() uint64 {
	arch := f.Architecture()
	if arch == "" {
		return 0
	}
	v, ok := f.GetMetadata(arch + ".block_count")
	if !ok {
		return 0
	}
	n, ok := AsU64(v)
	if !ok {
		return 0
	}
	return n
}

// HeadCount returns the number of attention heads.
func (f *GGUFFile) HeadCount() uint64 {
	arch := f.Architecture()
	if arch == "" {
		return 0
	}
	v, ok := f.GetMetadata(arch + ".attention.head_count")
	if !ok {
		return 0
	}
	n, ok := AsU64(v)
	if !ok {
		return 0
	}
	return n
}

// License returns the license identifier from the GGUF metadata.
func (f *GGUFFile) License() string {
	v, ok := f.GetMetadata("general.license")
	if !ok {
		return ""
	}
	s, ok := AsStr(v)
	if !ok {
		return ""
	}
	return s
}

// ---------------------------------------------------------------------------
// File type → quantization name mapping
// ---------------------------------------------------------------------------

var fileTypeNames = map[uint32]string{
	0: "F32", 1: "F16", 2: "Q4_0", 3: "Q4_1",
	7: "Q8_0", 8: "Q5_0", 9: "Q5_1",
	10: "Q2_K", 11: "Q3_K_S", 12: "Q3_K_M", 13: "Q3_K_L",
	14: "Q4_K_S", 15: "Q4_K_M", 16: "Q5_K_S", 17: "Q5_K_M",
	18: "Q6_K",
	19: "IQ2_XXS", 20: "IQ2_XS", 21: "IQ3_XXS", 22: "IQ1_S",
	23: "IQ4_NL", 24: "IQ3_S", 25: "IQ2_S", 26: "IQ4_XS",
	27: "IQ3_M", 28: "IQ1_M", 29: "BF16",
	30: "Q4_0_4_4", 31: "Q4_0_4_8", 32: "Q4_0_8_8",
}

// FileTypeName maps the general.file_type uint32 to a human-readable quantization name.
func FileTypeName(fileType uint32) string {
	if name, ok := fileTypeNames[fileType]; ok {
		return name
	}
	return "Unknown"
}
