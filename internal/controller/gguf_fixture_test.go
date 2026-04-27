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

package controller

import (
	"bytes"
	"encoding/binary"
)

// buildMinimalGGUF constructs the bytes of a minimal-but-valid GGUF v3 file
// with a single string metadata entry "general.name" set to the supplied name
// (or no metadata at all when name is empty). Tensor count is zero. Used by
// tests that exercise the controller's GGUF metadata extraction path without
// pulling in a real model file.
//
// Layout reference: pkg/gguf/parser.go (ggufMagic, parseHeader, readValue).
func buildMinimalGGUF(name string) []byte {
	buf := &bytes.Buffer{}
	mustWriteLE(buf, uint32(0x46554747)) // magic
	mustWriteLE(buf, uint32(3))          // version
	mustWriteLE(buf, uint64(0))          // tensor_count

	if name == "" {
		mustWriteLE(buf, uint64(0)) // metadata_kv_count
		return buf.Bytes()
	}

	mustWriteLE(buf, uint64(1)) // metadata_kv_count

	// Key: "general.name" (length-prefixed string)
	key := "general.name"
	mustWriteLE(buf, uint64(len(key)))
	buf.WriteString(key)

	// Value: type tag 8 (STRING), then length-prefixed string.
	mustWriteLE(buf, uint32(8))
	mustWriteLE(buf, uint64(len(name)))
	buf.WriteString(name)

	return buf.Bytes()
}

func mustWriteLE(buf *bytes.Buffer, data interface{}) {
	if err := binary.Write(buf, binary.LittleEndian, data); err != nil {
		panic(err)
	}
}
