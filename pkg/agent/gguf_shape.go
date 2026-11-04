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
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/gguf"
)

// maxGGUFHeaderBytes bounds how much of a model file the agent reads for its
// attention shape. The header, metadata and tensor index precede the tensor
// data; the tokenizer arrays dominate them, and a 262144-token vocabulary
// with scores and merges is well under 16 MiB. A header past the bound is
// treated as unreadable, which leaves the file-size estimate in place. A
// variable so tests can lower it.
var maxGGUFHeaderBytes int64 = 32 << 20

// attentionShape is the model geometry the KV-cache term of the memory
// estimate is computed from (see EstimateModelMemoryWithOptions).
type attentionShape struct {
	LayerCount, EmbeddingSize uint64
	HeadCount, HeadCountKV    uint64
	KeyLength, ValueLength    uint64
}

// known reports whether the shape can size a KV cache at all: without a
// layer count and an embedding width EstimateModelMemoryWithOptions takes
// the file-size heuristic, with no KV term.
func (s attentionShape) known() bool {
	return s.LayerCount != 0 && s.EmbeddingSize != 0
}

func shapeFromStatus(meta *inferencev1alpha1.GGUFMetadata) attentionShape {
	if meta == nil {
		return attentionShape{}
	}
	return attentionShape{
		LayerCount:    meta.LayerCount,
		EmbeddingSize: meta.EmbeddingSize,
		HeadCount:     meta.HeadCount,
		HeadCountKV:   meta.HeadCountKV,
		KeyLength:     meta.KeyLength,
		ValueLength:   meta.ValueLength,
	}
}

// shapeFromParsed reads the same accessors the controller maps into
// Model.status.gguf (ggufMetadataFromParsed in
// internal/controller/model_controller.go), so a shape read here is the one
// the status path would have carried for the same file.
func shapeFromParsed(f *gguf.GGUFFile) attentionShape {
	return attentionShape{
		LayerCount:    f.BlockCount(),
		EmbeddingSize: f.EmbeddingLength(),
		HeadCount:     f.HeadCount(),
		HeadCountKV:   f.HeadCountKV(),
		KeyLength:     f.KeyLength(),
		ValueLength:   f.ValueLength(),
	}
}

// ggufShapeCache remembers the attention shape read from each model file, so
// an admission re-checked every few seconds (a refusal backs off from 5 s)
// does not re-read the header. An entry is keyed by path and holds only while
// the file's size, mtime and inode match (the inode catches a same-size file
// moved into place with its mtime preserved, as cp -p and rsync -t do). It
// also remembers which models were already logged as estimated without a KV
// charge. The zero value is ready.
type ggufShapeCache struct {
	mu      sync.Mutex
	entries map[string]ggufShapeEntry
	warned  map[string]struct{}
}

type ggufShapeEntry struct {
	size    int64
	modTime time.Time
	inode   uint64
	shape   attentionShape
	reason  string
}

// fileInode returns the inode number of info, or 0 where the platform
// reports none.
func fileInode(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino) //nolint:unconvert // Ino is uint64 on darwin and linux, not on every GOOS
	}
	return 0
}

// shapeOf returns the attention shape in the GGUF header of the file at path,
// or why there is none (a directory, not a GGUF, an unreadable header, a
// header without the shape keys). followSymlinks mirrors how the agent opens
// the file to load it: a local source follows links (a Hugging Face cache
// snapshot links each file to a blob; the allowed-roots check already
// resolved it), while the model-store slot of a downloaded source is opened
// with O_NOFOLLOW, as ensureModel refuses a link there. O_NONBLOCK keeps a
// FIFO swapped in at path from blocking the open; it is then refused as not
// a regular file. Only the header is read, at most maxGGUFHeaderBytes.
//
// For a split GGUF the path is the first shard: llama-server refuses to load
// any other (llama-model-loader.cpp, "model must be loaded with the first
// split"), and the first shard is the one that carries the model metadata.
func (c *ggufShapeCache) shapeOf(path string, followSymlinks bool) (attentionShape, string) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if !followSymlinks {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return attentionShape{}, fmt.Sprintf("cannot open %s: %v", path, pathErrorCause(err))
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	switch {
	case err != nil:
		return attentionShape{}, fmt.Sprintf("cannot stat %s: %v", path, pathErrorCause(err))
	case info.IsDir():
		return attentionShape{}, fmt.Sprintf("%s is a directory (an MLX model); "+
			"the agent reads the attention shape only from a GGUF file", path)
	case !info.Mode().IsRegular():
		return attentionShape{}, fmt.Sprintf("%s is not a regular file", path)
	}

	inode := fileInode(info)
	c.mu.Lock()
	if e, ok := c.entries[path]; ok && e.size == info.Size() && e.modTime.Equal(info.ModTime()) && e.inode == inode {
		c.mu.Unlock()
		return e.shape, e.reason
	}
	c.mu.Unlock()

	shape, reason := readGGUFShape(f, path)

	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]ggufShapeEntry)
	}
	c.entries[path] = ggufShapeEntry{
		size: info.Size(), modTime: info.ModTime(), inode: inode, shape: shape, reason: reason,
	}
	c.mu.Unlock()
	return shape, reason
}

// pathErrorCause drops the operation and path an *os.PathError repeats, for
// a message that already names the path.
func pathErrorCause(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// readGGUFShape parses the header of r with pkg/gguf, reading at most
// maxGGUFHeaderBytes, and returns its attention shape or why there is none.
func readGGUFShape(r io.Reader, path string) (attentionShape, string) {
	limited := &io.LimitedReader{R: r, N: maxGGUFHeaderBytes}
	parsed, err := gguf.Parse(bufio.NewReaderSize(limited, 64<<10))
	switch {
	case errors.Is(err, gguf.ErrInvalidMagic):
		return attentionShape{}, fmt.Sprintf("%s is not a GGUF file", path)
	case err != nil && limited.N == 0:
		return attentionShape{}, fmt.Sprintf("the GGUF header of %s is larger than the %s the agent reads",
			path, formatMemory(uint64(maxGGUFHeaderBytes))) //nolint:gosec // G115: a positive constant
	case err != nil:
		return attentionShape{}, fmt.Sprintf("the GGUF header of %s is unreadable: %v", path, err)
	}
	shape := shapeFromParsed(parsed)
	if !shape.known() {
		return attentionShape{}, fmt.Sprintf("the GGUF header of %s has no block count or embedding length "+
			"for architecture %q", path, parsed.Architecture())
	}
	return shape, ""
}

// warnOnce reports whether key is new, recording it. It keeps the
// "KV cache not counted" log to one line per model and reason instead of one
// per admission re-check.
func (c *ggufShapeCache) warnOnce(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.warned[key]; seen {
		return false
	}
	if c.warned == nil {
		c.warned = make(map[string]struct{})
	}
	c.warned[key] = struct{}{}
	return true
}
