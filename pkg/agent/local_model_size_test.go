package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalModelSize(t *testing.T) {
	dir := t.TempDir()

	// Single-file model (e.g. a GGUF file).
	file := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(file, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := localModelSize(file); err != nil || got != 1024 {
		t.Fatalf("single file: got %d, err %v; want 1024", got, err)
	}

	// Directory model (e.g. an MLX safetensors model), including a subdir.
	modelDir := filepath.Join(dir, "mlx-model")
	if err := os.MkdirAll(filepath.Join(modelDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "a.safetensors"), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "sub", "b.safetensors"), make([]byte, 512), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := localModelSize(modelDir); err != nil || got != 2560 {
		t.Fatalf("directory model: got %d, err %v; want 2560", got, err)
	}

	// Missing path returns an error.
	if _, err := localModelSize(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Fatal("expected an error for a missing path")
	}
}

// A Hugging Face cache snapshot is a directory of relative links into the
// cache's blobs, and a model store entry is often a link to that snapshot.
// Both must count the bytes the links point to, not the links themselves.
func TestLocalModelSizeFollowsLinks(t *testing.T) {
	dir := t.TempDir()
	blobs := filepath.Join(dir, "blobs")
	snapshot := filepath.Join(dir, "snapshots", "abc123")
	for _, d := range []string{blobs, snapshot, filepath.Join(dir, "store")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, size := range map[string]int{"model.safetensors": 4096, "config.json": 100} {
		if err := os.WriteFile(filepath.Join(blobs, "sha-"+name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "..", "blobs", "sha-"+name), filepath.Join(snapshot, name)); err != nil {
			t.Fatal(err)
		}
	}
	storeEntry := filepath.Join(dir, "store", "qwen-mlx")
	if err := os.Symlink(snapshot, storeEntry); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{snapshot, storeEntry} {
		if got, err := localModelSize(path); err != nil || got != 4196 {
			t.Errorf("%s: got %d, err %v; want 4196 (the linked files, not the links)", path, got, err)
		}
	}
}

// A link that points nowhere leaves the model unsizable, so it is an error the
// caller can fall back from rather than a few bytes of link counted as weights.
func TestLocalModelSizeBrokenLinkIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.safetensors"), make([]byte, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "b.safetensors")); err != nil {
		t.Fatal(err)
	}
	if got, err := localModelSize(dir); err == nil {
		t.Fatalf("got %d and no error; want an error for the broken link", got)
	}
}

// A link back up the tree is walked once, not forever.
func TestLocalModelSizeLinkLoopTerminates(t *testing.T) {
	model := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(filepath.Join(model, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "a.safetensors"), make([]byte, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(model, filepath.Join(model, "sub", "loop")); err != nil {
		t.Fatal(err)
	}
	if got, err := localModelSize(model); err != nil || got != 64 {
		t.Fatalf("got %d, err %v; want 64", got, err)
	}
}
