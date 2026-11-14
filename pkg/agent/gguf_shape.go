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
	"strings"
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
// treated as unreadable, which leaves the file-size estimate in place. It is
// a variable so tests can lower it.
var maxGGUFHeaderBytes int64 = 32 << 20

// attentionShape is the model geometry the KV-cache term of the memory
// estimate is computed from (see EstimateModelMemoryWithOptions).
type attentionShape struct {
	LayerCount, EmbeddingSize uint64
	HeadCount, HeadCountKV    uint64
	KeyLength, ValueLength    uint64

	// KVHeadLayers and RecurrentStatePerSeq come only from a GGUF header
	// whose layer layout differs from LayerCount layers of HeadCountKV heads
	// (see kvLayoutFromParsed); both are 0 otherwise, and always for a shape
	// from the Model status, which records no layout.
	KVHeadLayers         uint64
	RecurrentStatePerSeq uint64
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
	s := attentionShape{
		LayerCount:    f.BlockCount(),
		EmbeddingSize: f.EmbeddingLength(),
		HeadCount:     f.HeadCount(),
		HeadCountKV:   f.HeadCountKV(),
		KeyLength:     f.KeyLength(),
		ValueLength:   f.ValueLength(),
	}
	s.KVHeadLayers, s.RecurrentStatePerSeq = kvLayoutFromParsed(f)
	return s
}

// kvLayoutFromParsed reads which layers of a GGUF hold a KV cache, and the
// state the others hold instead, for the files whose layout is not simply
// block_count layers of head_count_kv heads. It mirrors llama.cpp master
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
func kvLayoutFromParsed(f *gguf.GGUFFile) (kvHeadLayers, recurrentStatePerSeq uint64) {
	arch := f.Architecture()
	n := f.BlockCount()
	if arch == "" || n == 0 || n > maxGGUFLayoutLayers {
		return 0, 0
	}
	key := func(name string) (gguf.GGUFValue, bool) { return f.GetMetadata(arch + "." + name) }

	headsKV, foundHeadsKV := key("attention.head_count_kv")
	kvHeads, perLayerHeads, ok := ggufLayerCounts(headsKV, foundHeadsKV, n)
	if !ok {
		return 0, 0
	}
	ssmInner, hasInner := ggufCountKey(key("ssm.inner_size"))
	ssmState, hasState := ggufCountKey(key("ssm.state_size"))
	hasSSM := hasInner && hasState

	recurrent := make([]bool, n)
	if v, found := key("attention.recurrent_layers"); found {
		if !ggufLayerBools(v, recurrent) {
			return 0, 0
		}
	} else if v, found := key("full_attention_interval"); found {
		interval, ok := ggufCount(v)
		if !ok || interval == 0 {
			return 0, 0
		}
		nextn, _ := ggufCountKey(key("nextn_predict_layers"))
		if nextn > n {
			return 0, 0
		}
		for i := uint64(0); i < n-nextn; i++ {
			recurrent[i] = (i+1)%interval != 0
		}
	} else if perLayerHeads && hasSSM {
		for i := range recurrent {
			recurrent[i] = layerHeads(kvHeads, i) == 0
		}
	}

	var recurrentLayers uint64
	for i, r := range recurrent {
		if r {
			recurrentLayers++
			continue
		}
		kvHeadLayers += layerHeads(kvHeads, i)
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
	if !hasSSM || ggufHasKeyPart(f, arch, ".wkv.", ".shortconv.", ".kda.", ".ple.") {
		return 0, 0
	}
	conv, _ := ggufCountKey(key("ssm.conv_kernel"))
	groups, _ := ggufCountKey(key("ssm.group_count"))
	if conv > 0 {
		conv--
	}
	// Real files are far inside maxSSMDim (Qwen3.8-27B: inner 6144, state
	// 128, 16 groups, kernel 4); bounding every factor keeps the products
	// below 2^61 with n <= 512. A file past the bound gets no layout.
	if ssmInner > maxSSMDim || ssmState > maxSSMDim || conv > maxSSMDim || groups > maxSSMDim {
		return 0, 0
	}
	floatsPerLayer := conv*(ssmInner+2*groups*ssmState) + ssmState*ssmInner
	return kvHeadLayers, recurrentLayers * floatsPerLayer * 4
}

// maxSSMDim bounds each SSM dimension kvLayoutFromParsed multiplies.
const maxSSMDim = 1 << 16

// maxGGUFLayoutLayers bounds the per-layer arrays kvLayoutFromParsed builds;
// llama.cpp's own limit is LLAMA_MAX_LAYERS, 512.
const maxGGUFLayoutLayers = 512

// layerHeads returns layer i's KV head count from ggufLayerCounts' result.
func layerHeads(heads []uint64, i int) uint64 {
	if len(heads) == 1 {
		return heads[0]
	}
	return heads[i]
}

// ggufLayerCounts reads a per-layer count the way llama.cpp's
// get_key_or_arr does: a scalar applies to every layer, an array gives one
// value per layer. perLayer reports the array form. ok is false when the key
// is absent, negative, or an array whose length is not one per layer.
func ggufLayerCounts(v gguf.GGUFValue, found bool, n uint64) (counts []uint64, perLayer, ok bool) {
	if !found {
		return nil, false, false
	}
	if c, ok := ggufCount(v); ok {
		return []uint64{c}, false, true
	}
	arr, isArr := v.(gguf.ArrayVal)
	if !isArr || uint64(len(arr.Values)) != n {
		return nil, false, false
	}
	counts = make([]uint64, len(arr.Values))
	for i, e := range arr.Values {
		c, ok := ggufCount(e)
		if !ok {
			return nil, false, false
		}
		counts[i] = c
	}
	return counts, true, true
}

// ggufLayerBools fills out from a bool, or an array of one bool per layer.
func ggufLayerBools(v gguf.GGUFValue, out []bool) bool {
	if b, ok := v.(gguf.BoolVal); ok {
		for i := range out {
			out[i] = b.Value
		}
		return true
	}
	arr, isArr := v.(gguf.ArrayVal)
	if !isArr || len(arr.Values) != len(out) {
		return false
	}
	for i, e := range arr.Values {
		b, ok := e.(gguf.BoolVal)
		if !ok {
			return false
		}
		out[i] = b.Value
	}
	return true
}

// ggufCount reads a non-negative integer of any width. GGUF converters write
// unsigned values for most counts but signed ones for some per-layer arrays
// (Nemotron Super's head counts are int32).
func ggufCount(v gguf.GGUFValue) (uint64, bool) {
	if u, ok := gguf.AsU64(v); ok {
		return u, true
	}
	var s int64
	switch x := v.(type) {
	case gguf.Int8Val:
		s = int64(x.Value)
	case gguf.Int16Val:
		s = int64(x.Value)
	case gguf.Int32Val:
		s = int64(x.Value)
	case gguf.Int64Val:
		s = x.Value
	default:
		return 0, false
	}
	if s < 0 {
		return 0, false
	}
	return uint64(s), true
}

// ggufCountKey is ggufCount for a lookup that may have missed.
func ggufCountKey(v gguf.GGUFValue, found bool) (uint64, bool) {
	if !found {
		return 0, false
	}
	return ggufCount(v)
}

// ggufHasKeyPart reports whether any <arch>.* key contains one of parts.
func ggufHasKeyPart(f *gguf.GGUFFile, arch string, parts ...string) bool {
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
