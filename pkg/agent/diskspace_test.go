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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// freeSpaceExecutor is a MetalExecutor over a temporary model store whose
// filesystem reports free bytes free, with headroom required.
func freeSpaceExecutor(t *testing.T, free uint64, headroom int64) (*MetalExecutor, string) {
	t.Helper()
	store := t.TempDir()
	e := NewMetalExecutor("/bin/llama-server", store, newNopLogger(), WithDownloadFreeSpaceHeadroom(headroom))
	e.freeBytes = func(string) (uint64, error) { return free, nil }
	return e, store
}

// TestCopyToFileResume_RefusesWhenTheDownloadWouldFillTheDisk is #2409: a
// download whose bytes plus the headroom exceed the free space is refused
// before a byte is written, with an error the start path turns into a
// ModelDownloadNoSpace refusal.
func TestCopyToFileResume_RefusesWhenTheDownloadWouldFillTheDisk(t *testing.T) {
	e, store := freeSpaceExecutor(t, 5<<30, 10<<30)
	file := filepath.Join(store, "m", "model.gguf")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader(strings.Repeat("x", 1024))

	err := e.copyToFileResume(file+".partial", file, body, 1024, 0, "")
	var noSpace *ModelDownloadNoSpaceError
	if !errors.As(err, &noSpace) {
		t.Fatalf("copyToFileResume error = %v, want a ModelDownloadNoSpaceError", err)
	}
	if noSpace.Need != 1024 || noSpace.Headroom != 10<<30 {
		t.Errorf("error = %+v, want Need 1024 and Headroom 10GiB", noSpace)
	}
	if !strings.Contains(err.Error(), "about 5 GiB free") {
		t.Errorf("message %q, want the free space in whole GiB", err.Error())
	}
	if _, statErr := os.Stat(file + ".partial"); !os.IsNotExist(statErr) {
		t.Errorf("a partial was created (%v); the preflight must refuse before writing", statErr)
	}
}

// TestCopyToFileResume_DownloadsWhenThereIsRoom: enough free space for the
// bytes plus the headroom downloads as before.
func TestCopyToFileResume_DownloadsWhenThereIsRoom(t *testing.T) {
	e, store := freeSpaceExecutor(t, 11<<30, 10<<30)
	file := filepath.Join(store, "model.gguf")
	if err := e.copyToFileResume(file+".partial", file, strings.NewReader("abcd"), 4, 0, ""); err != nil {
		t.Fatalf("copyToFileResume: %v", err)
	}
	if got, _ := os.ReadFile(file); string(got) != "abcd" {
		t.Errorf("published file = %q, want abcd", got)
	}
}

// TestCopyToFileResume_ResumeNeedsOnlyTheRemainingBytes: a resume counts the
// bytes still to fetch, not the whole file, so a nearly finished download
// that fits is not refused.
func TestCopyToFileResume_ResumeNeedsOnlyTheRemainingBytes(t *testing.T) {
	const size = 3 << 30
	e, store := freeSpaceExecutor(t, (10<<30)+(1<<30), 10<<30)
	file := filepath.Join(store, "model.gguf")
	partial := file + ".partial"
	if err := os.WriteFile(partial, []byte("ab"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 3 GiB declared with all but 2 bytes on disk: the whole file would not
	// fit above the headroom, the 2 remaining bytes do.
	var noSpace *ModelDownloadNoSpaceError
	if err := e.copyToFileResume(partial, file, strings.NewReader("cd"), size, size-2, ""); errors.As(err, &noSpace) {
		t.Fatalf("a resume with 2 bytes left was refused for space: %v", err)
	}
}

// enospcReader returns some bytes, then fails the way a full disk fails a
// write.
type enospcReader struct{ sent bool }

func (r *enospcReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, &os.PathError{Op: "write", Path: "model.gguf.partial", Err: syscall.ENOSPC}
	}
	r.sent = true
	return copy(p, "abcd"), nil
}

// TestCopyToFileResume_DiskFullDuringCopyKeepsThePartial: a filesystem that
// fills up during the copy is the same refusal, and the partial stays so the
// download resumes once space is freed.
func TestCopyToFileResume_DiskFullDuringCopyKeepsThePartial(t *testing.T) {
	e, store := freeSpaceExecutor(t, 1<<40, 0)
	file := filepath.Join(store, "model.gguf")
	err := e.copyToFileResume(file+".partial", file, &enospcReader{}, 0, 0, "")
	var noSpace *ModelDownloadNoSpaceError
	if !errors.As(err, &noSpace) || !noSpace.DuringCopy {
		t.Fatalf("error = %v, want a ModelDownloadNoSpaceError during the copy", err)
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("error = %v no longer carries the filesystem's ENOSPC", err)
	}
	if got, _ := os.ReadFile(file + ".partial"); string(got) != "abcd" {
		t.Errorf("partial = %q, want the 4 bytes written before the disk filled", got)
	}
}

// TestCopyToFileResume_UnknownFreeSpaceDoesNotBlock: a filesystem whose free
// space cannot be read does not stop a download.
func TestCopyToFileResume_UnknownFreeSpaceDoesNotBlock(t *testing.T) {
	e, store := freeSpaceExecutor(t, 0, 10<<30)
	e.freeBytes = func(string) (uint64, error) { return 0, errors.New("statfs: not supported") }
	file := filepath.Join(store, "model.gguf")
	if err := e.copyToFileResume(file+".partial", file, strings.NewReader("ab"), 2, 0, ""); err != nil {
		t.Fatalf("copyToFileResume with unknown free space: %v", err)
	}
}

// TestEnsureProcess_NoSpaceRefusesWithItsOwnReason: the start path records a
// no-space download as a ModelDownloadNoSpace refusal on the
// InferenceService, which the controller keeps (MetalAgentRefusalReasons).
func TestEnsureProcess_NoSpaceRefusesWithItsOwnReason(t *testing.T) {
	f := newRestartFixture(t, "full-disk")
	f.agent.mu.Lock()
	delete(f.agent.processes, f.key)
	f.agent.mu.Unlock()
	// Wrapped as ensureModel wraps every download error.
	f.exec.startErr = fmt.Errorf("failed to download model: %w", &ModelDownloadNoSpaceError{
		Path: "/store/m/model.gguf", Need: 4 << 30, Headroom: 10 << 30, Free: 2 << 30})

	_ = f.agent.ensureProcess(context.Background(), f.isvc)

	got := &inferencev1alpha1.InferenceService{}
	key := types.NamespacedName{Namespace: f.isvc.Namespace, Name: f.isvc.Name}
	if err := f.client.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.SchedulingStatus != inferencev1alpha1.ReasonModelDownloadNoSpace {
		t.Errorf("schedulingStatus = %q, want %s", got.Status.SchedulingStatus, inferencev1alpha1.ReasonModelDownloadNoSpace)
	}
	if !strings.Contains(got.Status.SchedulingMessage, "--download-free-space-headroom") {
		t.Errorf("schedulingMessage = %q, want it to name the headroom flag", got.Status.SchedulingMessage)
	}
}

// TestReportModelStore_SetsUsedAndFreeGauges: the store gauges report the
// bytes in the model store and the filesystem's free space.
func TestReportModelStore_SetsUsedAndFreeGauges(t *testing.T) {
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, "m"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]int{"m/model.gguf": 3000, "m/model.gguf.sha256": 100, "llama-server-ns-svc.log": 50} {
		if err := os.WriteFile(filepath.Join(store, name), make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink in the store (a hand-placed model) is neither followed nor
	// counted: the file it names lives elsewhere.
	outside := filepath.Join(t.TempDir(), "elsewhere.gguf")
	if err := os.WriteFile(outside, make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(store, "m", "linked.gguf")); err != nil {
		t.Fatal(err)
	}
	reportModelStore(store, func(string) (uint64, error) { return 42 << 30, nil }, newNopLogger())

	if got := testutil.ToFloat64(modelStoreUsedBytes); got != 3150 {
		t.Errorf("model store used bytes = %v, want 3150", got)
	}
	if got := testutil.ToFloat64(modelStoreFreeBytes); got != float64(42<<30) {
		t.Errorf("model store free bytes = %v, want %v", got, float64(42<<30))
	}
}

var _ io.Reader = (*enospcReader)(nil)

// TestNewMetalAgent_PassesTheFreeSpaceHeadroomToTheExecutor: the
// --download-free-space-headroom value reaches the llama.cpp executor that
// downloads models.
func TestNewMetalAgent_PassesTheFreeSpaceHeadroomToTheExecutor(t *testing.T) {
	a := NewMetalAgent(MetalAgentConfig{
		LlamaServerBin:            "/bin/llama-server",
		ModelStorePath:            t.TempDir(),
		DownloadFreeSpaceHeadroom: 7 << 30,
	})
	a.buildExecutors()
	exec, ok := a.executors[runtimeLlamaServer].(*MetalExecutor)
	if !ok {
		t.Fatalf("llama-server executor = %T, want *MetalExecutor", a.executors[runtimeLlamaServer])
	}
	if exec.freeSpaceHeadroom != 7<<30 {
		t.Errorf("executor headroom = %d, want %d", exec.freeSpaceHeadroom, int64(7<<30))
	}
}
