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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// qwen3_32BShape is the attention shape of a real Qwen3-32B GGUF: 64 blocks,
// 64 attention heads, 8 KV heads, explicit 128-wide key and value heads.
var qwen3_32BShape = map[string]uint32{
	"block_count":             64,
	"context_length":          40960,
	"embedding_length":        5120,
	"feed_forward_length":     25600,
	"attention.head_count":    64,
	"attention.head_count_kv": 8,
	"attention.key_length":    128,
	"attention.value_length":  128,
}

// qwen3_32BKVAt131072 is the f16 KV cache llama-server allocates for
// qwen3_32BShape at --ctx-size 131072: 64 layers * 8 KV heads * 131072 cells
// * (128 key + 128 value dims) * 2 bytes = 32 GiB.
const qwen3_32BKVAt131072 = uint64(64 * 8 * 131072 * (128 + 128) * 2)

// buildTestGGUF returns the bytes of a minimal GGUF v3 file (header and
// metadata, no tensors) with general.architecture set to arch and each
// entry of u32 stored as a uint32 under "<arch>.<key>", the types llama.cpp's
// converters write. Layout reference: pkg/gguf/parser.go.
func buildTestGGUF(arch string, u32 map[string]uint32) []byte {
	buf := &bytes.Buffer{}
	le := func(v interface{}) { _ = binary.Write(buf, binary.LittleEndian, v) }
	str := func(s string) {
		le(uint64(len(s)))
		buf.WriteString(s)
	}

	keys := make([]string, 0, len(u32))
	for k := range u32 {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	le(uint32(0x46554747)) // magic "GGUF"
	le(uint32(3))          // version
	le(uint64(0))          // tensor count
	le(uint64(1 + len(keys)))
	str("general.architecture")
	le(uint32(8)) // string
	str(arch)
	for _, k := range keys {
		str(arch + "." + k)
		le(uint32(4)) // uint32
		le(u32[k])
	}
	return buf.Bytes()
}

// writeTestGGUF writes buildTestGGUF("qwen3", u32) to path.
func writeTestGGUF(t *testing.T, path string, u32 map[string]uint32) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buildTestGGUF("qwen3", u32), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testFileSize(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(info.Size()) //nolint:gosec // G115: a test file's size is non-negative
}

// A local GGUF carries no Status.GGUF (the controller cannot read the
// Mac's disk), so the attention shape must come from the file's own header.
// Without it the estimate is the file-size heuristic at any --ctx-size and
// the 32 GiB KV cache below is never charged.
func TestEstimateModelMemory_LocalGGUFChargesKVFromFileHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Qwen3-32B-Q4_K_M.gguf")
	writeTestGGUF(t, path, qwen3_32BShape)
	size := testFileSize(t, path)

	for _, source := range []string{path, "file://" + path} {
		t.Run(source, func(t *testing.T) {
			agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
			model := newAdmissionTestModel(source, "")

			est, err := agent.estimateModelMemory(context.Background(), model, 131072, "", "", 0)
			if err != nil {
				t.Fatalf("estimateModelMemory: %v", err)
			}
			if est.KVCacheBytes != qwen3_32BKVAt131072 {
				t.Errorf("KVCacheBytes = %d, want %d (the file header's attention shape at ctx 131072)",
					est.KVCacheBytes, qwen3_32BKVAt131072)
			}
			if want := size + qwen3_32BKVAt131072 + overheadBytes; est.TotalBytes != want {
				t.Errorf("TotalBytes = %d, want %d (weights + KV + overhead)", est.TotalBytes, want)
			}
		})
	}
}

// The KV charge must reach the admission decision: a local GGUF whose KV
// cache does not fit the budget is refused with InsufficientMemory, and
// nothing is reserved for it.
func TestCheckMemoryAdmission_LocalGGUFRefusedWhenKVCacheDoesNotFit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Qwen3-32B-Q4_K_M.gguf")
	writeTestGGUF(t, path, qwen3_32BShape)

	isvc := newAdmissionTestISVC()
	agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 32 * oneGiB},
		MemoryFraction: 0.75,
	})
	model := newAdmissionTestModel(path, "")

	err := agent.checkMemoryAdmission(context.Background(), isvc, model, 131072, "", "")
	if err == nil {
		t.Fatal("admission passed; want a refusal for a 32.5 GB estimate against a 24 GB budget")
	}

	updated := &inferencev1alpha1.InferenceService{}
	if getErr := agent.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}, updated); getErr != nil {
		t.Fatalf("re-fetch InferenceService: %v", getErr)
	}
	if updated.Status.SchedulingStatus != EventReasonInsufficientMemory {
		t.Errorf("SchedulingStatus = %q, want %q", updated.Status.SchedulingStatus, EventReasonInsufficientMemory)
	}
	if !strings.Contains(updated.Status.SchedulingMessage, "estimated 32.5 GB required") {
		t.Errorf("SchedulingMessage = %q, want it to carry the KV-inclusive estimate (32.5 GB)",
			updated.Status.SchedulingMessage)
	}
	agent.mu.Lock()
	reserved := len(agent.reservedMemory)
	agent.mu.Unlock()
	if reserved != 0 {
		t.Errorf("reservedMemory has %d entries after a refusal, want 0", reserved)
	}
}

// A downloaded source whose controller-side header read failed (no
// Status.GGUF) is charged from the agent's own store file once it is on disk.
func TestEstimateModelMemory_StoreGGUFChargesKVFromFileHeader(t *testing.T) {
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	source := "https://model-host.invalid/Qwen3-32B-Q4_K_M.gguf"
	model := newAdmissionTestModel(source, "")
	slot := modelCacheSlot(agent.config.ModelStorePath, model.Name, source)
	writeTestGGUF(t, slot, qwen3_32BShape)

	est, err := agent.estimateModelMemory(context.Background(), model, 131072, "", "", 0)
	if err != nil {
		t.Fatalf("estimateModelMemory: %v", err)
	}
	if est.KVCacheBytes != qwen3_32BKVAt131072 {
		t.Errorf("KVCacheBytes = %d, want %d (the store file's attention shape)",
			est.KVCacheBytes, qwen3_32BKVAt131072)
	}
}

// The controller's Model.status.gguf stays the first source: a downloaded
// model whose status carries a shape is charged from it, not from the store
// file, so estimates for models the controller could read do not change.
func TestEstimateModelMemory_StatusShapeTakesPrecedenceOverFileHeader(t *testing.T) {
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	source := "https://model-host.invalid/Qwen3-32B-Q4_K_M.gguf"
	model := newAdmissionTestModel(source, "")
	model.Status.GGUF = &inferencev1alpha1.GGUFMetadata{
		LayerCount: 32, EmbeddingSize: 4096, HeadCount: 32, HeadCountKV: 8, KeyLength: 128, ValueLength: 128,
	}
	writeTestGGUF(t, modelCacheSlot(agent.config.ModelStorePath, model.Name, source), qwen3_32BShape)

	est, err := agent.estimateModelMemory(context.Background(), model, 131072, "", "", 0)
	if err != nil {
		t.Fatalf("estimateModelMemory: %v", err)
	}
	// 32 layers * 8 KV heads * 131072 * (128 + 128) * 2 bytes = 16 GiB.
	if want := uint64(32 * 8 * 131072 * 256 * 2); est.KVCacheBytes != want {
		t.Errorf("KVCacheBytes = %d, want %d (the status shape, not the file's)", est.KVCacheBytes, want)
	}
}

// A local source that is a symlink (a Hugging Face cache snapshot links
// each file to a blob) is followed, as the load follows it.
func TestEstimateModelMemory_LocalGGUFSymlinkIsFollowed(t *testing.T) {
	dir := t.TempDir()
	blob := filepath.Join(dir, "blobs", "3f2a9c")
	writeTestGGUF(t, blob, qwen3_32BShape)
	link := filepath.Join(dir, "snapshots", "main", "Qwen3-32B-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(blob, link); err != nil {
		t.Fatal(err)
	}

	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	est, err := agent.estimateModelMemory(context.Background(), newAdmissionTestModel(link, ""), 131072, "", "", 0)
	if err != nil {
		t.Fatalf("estimateModelMemory: %v", err)
	}
	if est.KVCacheBytes != qwen3_32BKVAt131072 {
		t.Errorf("KVCacheBytes = %d, want %d (the linked blob's shape)", est.KVCacheBytes, qwen3_32BKVAt131072)
	}
}

// The store slot of a downloaded source is written only by the agent, which
// refuses to load through a symlink there; the header read refuses it too.
func TestEstimateModelMemory_StoreSlotSymlinkIsNotFollowed(t *testing.T) {
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	source := "https://model-host.invalid/Qwen3-32B-Q4_K_M.gguf"
	model := newAdmissionTestModel(source, "")
	target := filepath.Join(t.TempDir(), "elsewhere.gguf")
	writeTestGGUF(t, target, qwen3_32BShape)
	slot := modelCacheSlot(agent.config.ModelStorePath, model.Name, source)
	if err := os.MkdirAll(filepath.Dir(slot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, slot); err != nil {
		t.Fatal(err)
	}

	est, err := agent.estimateModelMemory(context.Background(), model, 131072, "", "", 0)
	if err != nil {
		t.Fatalf("estimateModelMemory: %v", err)
	}
	if est.KVCacheBytes != 0 {
		t.Errorf("KVCacheBytes = %d, want 0: the header was read through a symlink in the store slot", est.KVCacheBytes)
	}
	if !strings.Contains(est.KVCacheNotCounted, "cannot open "+slot) {
		t.Errorf("KVCacheNotCounted = %q, want it to name the refused open of %s", est.KVCacheNotCounted, slot)
	}
}

// When no shape can be found the estimate is the file-size heuristic as
// before, and above the default context it says why the KV cache is not in
// it. At the default context it says nothing.
func TestEstimateModelMemory_NoShapeReportsWhyAboveDefaultContext(t *testing.T) {
	dir := t.TempDir()
	mlxDir := filepath.Join(dir, "Qwen3-32B-4bit")
	if err := os.MkdirAll(mlxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mlxDir, "model.safetensors"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	notGGUF := filepath.Join(dir, "model.bin")
	if err := os.WriteFile(notGGUF, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	noShape := filepath.Join(dir, "no-shape.gguf")
	writeTestGGUF(t, noShape, map[string]uint32{"context_length": 40960})

	tests := []struct {
		name, source, statusSize, want string
		size                           uint64
	}{
		{"MLX directory", mlxDir, "", "is a directory (an MLX model)", 4096},
		{"not a GGUF", notGGUF, "", "is not a GGUF file", 4096},
		{"GGUF without the shape keys", noShape, "", "has no block count or embedding length", testFileSize(t, noShape)},
		{"downloaded model not on disk yet", "https://model-host.invalid/m.gguf", "4.0 KiB",
			"the Model status has no GGUF attention shape and the model file is not on this Mac yet", 4096},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
			model := newAdmissionTestModel(tt.source, tt.statusSize)

			est, err := agent.estimateModelMemory(context.Background(), model, 131072, "", "", 0)
			if err != nil {
				t.Fatalf("estimateModelMemory: %v", err)
			}
			if fallback := fallbackEstimate(tt.size); est != (MemoryEstimate{
				WeightsBytes: fallback.WeightsBytes, OverheadBytes: fallback.OverheadBytes,
				TotalBytes: fallback.TotalBytes, KVCacheNotCounted: est.KVCacheNotCounted,
			}) {
				t.Errorf("estimate = %+v, want the file-size heuristic %+v", est, fallback)
			}
			if !strings.Contains(est.KVCacheNotCounted, tt.want) {
				t.Errorf("KVCacheNotCounted = %q, want it to contain %q", est.KVCacheNotCounted, tt.want)
			}

			atDefault, err := agent.estimateModelMemory(context.Background(), model, defaultContextSize, "", "", 0)
			if err != nil {
				t.Fatalf("estimateModelMemory at the default context: %v", err)
			}
			if atDefault.KVCacheNotCounted != "" {
				t.Errorf("at --ctx-size %d KVCacheNotCounted = %q, want empty", defaultContextSize,
					atDefault.KVCacheNotCounted)
			}
		})
	}
}

// A refusal whose estimate has no KV term says so in the status message and
// the Warning Event, so an operator reading it knows the figure is a lower
// bound.
func TestCheckMemoryAdmission_RefusalSaysKVCacheNotCounted(t *testing.T) {
	notGGUF := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(notGGUF, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	isvc := newAdmissionTestISVC()
	agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 512 << 20}, // 384 MB budget, under the 512 MiB overhead
		MemoryFraction: 0.75,
	})

	err := agent.checkMemoryAdmission(context.Background(), isvc, newAdmissionTestModel(notGGUF, ""), 131072, "", "")
	if err == nil {
		t.Fatal("admission passed; want a refusal")
	}
	updated := &inferencev1alpha1.InferenceService{}
	if getErr := agent.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}, updated); getErr != nil {
		t.Fatalf("re-fetch InferenceService: %v", getErr)
	}
	want := "the KV cache for --ctx-size 131072 is not counted (" + notGGUF + " is not a GGUF file)"
	if !strings.Contains(updated.Status.SchedulingMessage, want) {
		t.Errorf("SchedulingMessage = %q, want it to contain %q", updated.Status.SchedulingMessage, want)
	}
}

// A model that fits on its own but not next to the services already running
// carries the same note, whether the host-wide budget or the agent cgroup's
// budget refuses it.
func TestCheckMemoryAdmission_CommittedRefusalSaysKVCacheNotCounted(t *testing.T) {
	notGGUF := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(notGGUF, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	// The 4 KiB file estimates at about 0.5 GiB, which fits either budget alone.
	tests := []struct {
		name      string
		cgroupMax string
		running   float64 // GiB already admitted to an in-cgroup engine
		refusedBy string  // the budget the refusal names
	}{
		{name: "host-wide budget", cgroupMax: "max", running: 48, refusedBy: "of the 48.0 GB budget"},
		{name: "cgroup budget", cgroupMax: "4294967296", running: 2.8, refusedBy: "of the 3.0 GB budget"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isvc := runtimeISVC("late", runtimeLlamaServer)
			a := newCgroupAgent(t, tt.cgroupMax, isvc)
			a.processes["default/early"] = &ManagedProcess{
				Name: "early", Namespace: "default", Runtime: runtimeLlamaServer, AdmittedBytes: gib(tt.running),
			}

			err := a.checkMemoryAdmission(context.Background(), isvc, newAdmissionTestModel(notGGUF, ""), 131072, "", "")
			if err == nil {
				t.Fatal("admission passed; want a refusal")
			}
			_, msg := schedulingStatus(t, a, "late")
			if !strings.Contains(msg, tt.refusedBy+" already committed to 1 service (default/early)") {
				t.Fatalf("SchedulingMessage = %q, want a refusal %q held by default/early", msg, tt.refusedBy)
			}
			want := "the KV cache for --ctx-size 131072 is not counted (" + notGGUF + " is not a GGUF file)"
			if !strings.Contains(msg, want) {
				t.Errorf("SchedulingMessage = %q, want it to contain %q", msg, want)
			}
		})
	}
}

// The parsed shape is cached per file and reused while size, mtime and inode
// are unchanged; a change to any of them makes the next admission read the
// header again.
func TestEstimateModelMemory_HeaderShapeCachedBySizeAndMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.gguf")
	writeTestGGUF(t, path, qwen3_32BShape)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel(path, "")
	kv := func() uint64 {
		t.Helper()
		est, err := agent.estimateModelMemory(context.Background(), model, 131072, "", "", 0)
		if err != nil {
			t.Fatalf("estimateModelMemory: %v", err)
		}
		return est.KVCacheBytes
	}
	if got := kv(); got != qwen3_32BKVAt131072 {
		t.Fatalf("first estimate KV = %d, want %d", got, qwen3_32BKVAt131072)
	}

	// Same byte length, half the KV heads, original mtime restored: a
	// re-read would see 4 heads, the cache still answers 8.
	halved := map[string]uint32{}
	for k, v := range qwen3_32BShape {
		halved[k] = v
	}
	halved["attention.head_count_kv"] = 4
	writeTestGGUF(t, path, halved)
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := kv(); got != qwen3_32BKVAt131072 {
		t.Errorf("estimate with unchanged size and mtime KV = %d, want the cached %d", got, qwen3_32BKVAt131072)
	}

	later := info.ModTime().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if got := kv(); got != qwen3_32BKVAt131072/2 {
		t.Errorf("estimate after an mtime change KV = %d, want %d (re-read, 4 KV heads)", got, qwen3_32BKVAt131072/2)
	}

	// A same-size file moved into place with the mtime preserved (cp -p,
	// rsync -t) is a new inode, so it is read again too.
	quartered := map[string]uint32{}
	for k, v := range qwen3_32BShape {
		quartered[k] = v
	}
	quartered["attention.head_count_kv"] = 2
	replacement := filepath.Join(filepath.Dir(path), ".model.gguf.tmp")
	writeTestGGUF(t, replacement, quartered)
	if err := os.Chtimes(replacement, later, later); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if got := kv(); got != qwen3_32BKVAt131072/4 {
		t.Errorf("estimate after a same-size, same-mtime replace KV = %d, want %d (re-read, 2 KV heads)",
			got, qwen3_32BKVAt131072/4)
	}
}

// The header read is bounded: a header longer than maxGGUFHeaderBytes is
// not read to its end, and the estimate keeps the file-size heuristic.
func TestEstimateModelMemory_HeaderReadIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.gguf")
	writeTestGGUF(t, path, qwen3_32BShape)
	prev := maxGGUFHeaderBytes
	maxGGUFHeaderBytes = 64 // the shape keys start past byte 64
	t.Cleanup(func() { maxGGUFHeaderBytes = prev })

	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	est, err := agent.estimateModelMemory(context.Background(), newAdmissionTestModel(path, ""), 131072, "", "", 0)
	if err != nil {
		t.Fatalf("estimateModelMemory: %v", err)
	}
	if est.KVCacheBytes != 0 {
		t.Errorf("KVCacheBytes = %d, want 0: the header was read past the bound", est.KVCacheBytes)
	}
	if !strings.Contains(est.KVCacheNotCounted, "is larger than the") {
		t.Errorf("KVCacheNotCounted = %q, want it to name the read bound", est.KVCacheNotCounted)
	}
}

// A FIFO at the source path has no writer; opening it for the header read
// must not block admission.
func TestEstimateModelMemory_FIFOSourceDoesNotBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel(path, "1.0 GiB")

	type result struct {
		est MemoryEstimate
		err error
	}
	done := make(chan result, 1)
	go func() {
		est, err := agent.estimateModelMemory(context.Background(), model, 131072, "", "", 0)
		done <- result{est, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("estimateModelMemory: %v", r.err)
		}
		if !strings.Contains(r.est.KVCacheNotCounted, "is not a regular file") {
			t.Errorf("KVCacheNotCounted = %q, want it to say the FIFO is not a regular file", r.est.KVCacheNotCounted)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("estimateModelMemory blocked opening a FIFO")
	}
}

// The missing-KV warning is logged once per model and reason, not on every
// re-check of a refused or re-admitted service.
func TestEstimateModelMemory_KVNotCountedLoggedOnce(t *testing.T) {
	notGGUF := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(notGGUF, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zapcore.WarnLevel)
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{Logger: zap.New(core).Sugar()})
	model := newAdmissionTestModel(notGGUF, "")

	for range 3 {
		if _, err := agent.estimateModelMemory(context.Background(), model, 131072, "", "", 0); err != nil {
			t.Fatalf("estimateModelMemory: %v", err)
		}
	}
	matched := logs.FilterMessageSnippet("does not count the KV cache").All()
	if len(matched) != 1 {
		t.Fatalf("got %d KV-not-counted warnings over 3 estimates, want 1: %v", len(matched), logs.All())
	}
	if got := matched[0].ContextMap()["reason"]; got != notGGUF+" is not a GGUF file" {
		t.Errorf("logged reason = %v, want %q", got, notGGUF+" is not a GGUF file")
	}
}
