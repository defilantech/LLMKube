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

package ocipublish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Changing tar names, omitting selected files or using raw artifact layers
// breaks the bytes pulled back from the real registry handler.
func TestPublishFilesystem(t *testing.T) {
	server := httptest.NewServer(registry.New())
	defer server.Close()
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "download"), 0700); err != nil {
		t.Fatal(err)
	}
	files := []File{{"download/ns-model.gguf", "model.gguf"}, {"download/shard.gguf", "shards/part.gguf"}, {"download/projector.gguf", "mmproj.gguf"}}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(directory, f.Path), []byte(f.Name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	destination := strings.TrimPrefix(server.URL, "http://") + "/models/test:v1"
	digest, err := Publish(context.Background(), destination, directory, files)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.NewDigest(strings.TrimPrefix(digest, "oci://"))
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		img, err := remote.Image(ref, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: arch}))
		if err != nil {
			t.Fatal(err)
		}
		layers, err := img.Layers()
		if err != nil || len(layers) != len(files) {
			t.Fatalf("want one layer per file (%d), got %d: %v", len(files), len(layers), err)
		}
		got := map[string]string{}
		for i, layer := range layers {
			mt, err := layer.MediaType()
			if err != nil || mt != types.OCILayer {
				t.Fatalf("layer media type=%s err=%v", mt, err)
			}
			stream, err := layer.Uncompressed()
			if err != nil {
				t.Fatal(err)
			}
			tr := tar.NewReader(stream)
			h, err := tr.Next()
			if err != nil {
				t.Fatal(err)
			}
			if h.Name != files[i].Name {
				t.Fatalf("layer %d contains %s, want %s", i, h.Name, files[i].Name)
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			got[h.Name] = string(data)
			if _, err := tr.Next(); err != io.EOF {
				t.Fatalf("layer %d contains more than one file: %v", i, err)
			}
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if len(got) != len(files) {
			t.Fatalf("image files: %v", got)
		}
		for _, f := range files {
			if got[f.Name] != f.Name {
				t.Fatalf("missing/wrong image file %q: %v", f.Name, got)
			}
		}
	}
}

func TestPublishRejectsUnsafeFiles(t *testing.T) {
	for _, file := range []File{{"../secret", "model.gguf"}, {"model", "../escape"}, {"model", "/absolute"}, {"model", ".wh.model"}, {"model", "nested/.wh..wh..opq"}} {
		t.Run(file.Name+file.Path, func(t *testing.T) {
			if _, err := Publish(context.Background(), "registry.example.com/models/test:v1", t.TempDir(), []File{file}); err == nil {
				t.Fatal("accepted unsafe path")
			}
		})
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "model")); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(context.Background(), "registry.example.com/models/test:v1", root, []File{{"model", "model.gguf"}}); err == nil {
		t.Fatal("published escaped symlink")
	}
}

func TestRunRegistryAuthAndFailure(t *testing.T) {
	handler := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "writer" || pass != "token" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "model"), []byte("weights"), 0600); err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(directory, "result")
	host := strings.TrimPrefix(server.URL, "http://")
	destination := host + "/models/private:v1"
	args := []string{"--image=" + destination, "--directory=" + directory, `--files=[{"path":"model","name":"model.gguf"}]`, "--result=" + result}
	config := t.TempDir()
	t.Setenv("DOCKER_CONFIG", config)
	if err := Run(context.Background(), args); err == nil {
		t.Fatal("anonymous publication succeeded")
	}
	if _, err := os.Stat(result); !os.IsNotExist(err) {
		t.Fatalf("failure wrote result: %v", err)
	}
	credentials := `{"auths":{"` + host + `":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("writer:token")) + `"}}}`
	if err := os.WriteFile(filepath.Join(config, "config.json"), []byte(credentials), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.NewDigest(strings.TrimPrefix(string(data), "oci://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Index(ref, remote.WithAuth(&authn.Basic{Username: "writer", Password: "token"})); err != nil {
		t.Fatal(err)
	}
}

// Removing NoCompression must change the actual registry blob, even for very
// compressible fixture weights. The media type must still describe gzip.
func TestPublishStoredGzip(t *testing.T) {
	server := httptest.NewServer(registry.New())
	defer server.Close()
	directory := t.TempDir()
	weights := bytes.Repeat([]byte("weights"), 20000)
	if err := os.WriteFile(filepath.Join(directory, "model"), weights, 0600); err != nil {
		t.Fatal(err)
	}
	destination := strings.TrimPrefix(server.URL, "http://") + "/models/stored:v1"
	digest, err := Publish(context.Background(), destination, directory, []File{{"model", "model.gguf"}})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.NewDigest(strings.TrimPrefix(digest, "oci://"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := remote.Image(ref)
	if err != nil {
		t.Fatal(err)
	}
	layers, err := img.Layers()
	if err != nil || len(layers) != 1 {
		t.Fatalf("layers=%v err=%v", layers, err)
	}
	mt, err := layers[0].MediaType()
	if err != nil || mt != types.OCILayer {
		t.Fatalf("media type=%s err=%v", mt, err)
	}
	blob, err := layers[0].Compressed()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := blob.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	var expected bytes.Buffer
	writer, err := gzip.NewWriterLevel(&expected, gzip.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, expected.Bytes()) {
		t.Fatalf("published layer uses compression: got %d bytes, want %d stored-gzip bytes", len(data), expected.Len())
	}
}

func TestPublishReusesUnchangedFileLayers(t *testing.T) {
	handler := registry.New()
	var mu sync.Mutex
	uploads := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/") {
			mu.Lock()
			uploads[r.URL.Query().Get("digest")]++
			mu.Unlock()
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	directory := t.TempDir()
	files := []File{{"first", "first.gguf"}, {"second", "second.gguf"}, {"projector", "mmproj.gguf"}}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(directory, file.Path), []byte(file.Name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	destination := strings.TrimPrefix(server.URL, "http://") + "/models/reuse:v1"
	publish := func() []v1.Descriptor {
		t.Helper()
		digest, err := Publish(context.Background(), destination, directory, files)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := name.NewDigest(strings.TrimPrefix(digest, "oci://"))
		if err != nil {
			t.Fatal(err)
		}
		img, err := remote.Image(ref)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := img.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		if len(manifest.Layers) != len(files) {
			t.Fatalf("want %d layers, got %d", len(files), len(manifest.Layers))
		}
		return manifest.Layers
	}
	before := publish()
	if err := os.WriteFile(filepath.Join(directory, "second"), []byte("new shard"), 0600); err != nil {
		t.Fatal(err)
	}
	after := publish()
	mu.Lock()
	defer mu.Unlock()
	for i := range before {
		if i == 1 {
			if before[i].Digest == after[i].Digest {
				t.Fatal("changed file reused stale layer")
			}
		} else if before[i].Digest != after[i].Digest {
			t.Fatalf("unchanged file %s changed digest", files[i].Name)
		}
		if got := uploads[after[i].Digest.String()]; got != 1 {
			t.Fatalf("layer for %s uploaded %d times, want once", files[i].Name, got)
		}
	}
}
