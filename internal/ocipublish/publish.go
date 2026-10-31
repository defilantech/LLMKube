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
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// File maps a downloaded file to its image-root-relative serving path.
type File struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// Destination requires an explicit registry and tag so a typo cannot publish
// to Docker Hub or silently overwrite latest.
func Destination(image string) (name.Tag, error) {
	if strings.Contains(image, "://") || strings.Contains(image, "@") ||
		!strings.Contains(image, "/") || !strings.Contains(path.Base(image), ":") {
		return name.Tag{}, fmt.Errorf("destination must be registry/repository:tag")
	}
	tag, err := name.NewTag(image, name.StrictValidation)
	if err != nil {
		return name.Tag{}, fmt.Errorf("invalid destination: %w", err)
	}
	if tag.Name() != image {
		return name.Tag{}, fmt.Errorf("destination must include an explicit registry and repository")
	}
	return tag, nil
}

// Run is the operator image's publish-model subcommand. Its only output is
// the digest reference, written after the registry acknowledges publication.
func Run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("publish-model", flag.ContinueOnError)
	image := flags.String("image", "", "Registry/repository:tag")
	directory := flags.String("directory", "/models", "Downloaded files root")
	filesJSON := flags.String("files", "", "JSON file mappings")
	result := flags.String("result", "/dev/termination-log", "Digest result file")
	caDir := flags.String("ca-directory", "", "Additional CA certificates")
	if err := flags.Parse(args); err != nil {
		return err
	}
	var files []File
	if err := json.Unmarshal([]byte(*filesJSON), &files); err != nil {
		return fmt.Errorf("decode files: %w", err)
	}
	transport := remote.DefaultTransport.(*http.Transport).Clone()
	if *caDir != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(*caDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") || entry.IsDir() {
				continue
			}
			pem, err := os.ReadFile(filepath.Join(*caDir, entry.Name()))
			if err != nil {
				return err
			}
			if !roots.AppendCertsFromPEM(pem) {
				return fmt.Errorf("no CA certificates in %s", entry.Name())
			}
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	digest, err := Publish(ctx, *image, *directory, files, remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithTransport(transport))
	if err != nil {
		return err
	}
	return os.WriteFile(*result, []byte(digest), 0600)
}

// Publish streams a tar filesystem layer from disk. The two platform manifests
// share that layer: weights are architecture-independent, but CRI image pulls
// select a platform before mounting the filesystem.
func Publish(ctx context.Context, destination, directory string, files []File, options ...remote.Option) (string, error) {
	tag, err := Destination(destination)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no model files")
	}
	seen := map[string]bool{}
	for _, file := range files {
		if !fs.ValidPath(file.Path) || !fs.ValidPath(file.Name) || file.Path == "." || file.Name == "." || strings.Contains(file.Name, "\\") {
			return "", fmt.Errorf("invalid model path %q -> %q", file.Path, file.Name)
		}
		for _, part := range strings.Split(file.Name, "/") {
			if strings.HasPrefix(part, ".wh.") {
				return "", fmt.Errorf("whiteout path %q", file.Name)
			}
		}
		if seen[file.Name] {
			return "", fmt.Errorf("duplicate image path %q", file.Name)
		}
		seen[file.Name] = true
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		reader, writer := io.Pipe()
		go func() {
			tw := tar.NewWriter(writer)
			err := writeFiles(ctx, tw, root, files)
			if closeErr := tw.Close(); err == nil {
				err = closeErr
			}
			_ = writer.CloseWithError(err)
		}()
		return reader, nil
	}, tarball.WithMediaType(types.OCILayer))
	if err != nil {
		return "", fmt.Errorf("package model: %w", err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return "", err
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	index := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	for _, arch := range []string{"amd64", "arm64"} {
		config, err := img.ConfigFile()
		if err != nil {
			return "", err
		}
		config.OS, config.Architecture = "linux", arch
		platformImage, err := mutate.ConfigFile(img, config)
		if err != nil {
			return "", err
		}
		index = mutate.AppendManifests(index, mutate.IndexAddendum{
			Add: platformImage, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}},
		})
	}
	options = append(options, remote.WithContext(ctx))
	if err := remote.WriteIndex(tag, index, options...); err != nil {
		return "", fmt.Errorf("publish model: %w", err)
	}
	digest, err := index.Digest()
	if err != nil {
		return "", err
	}
	return "oci://" + tag.Context().Digest(digest.String()).Name(), nil
}

func writeFiles(ctx context.Context, tw *tar.Writer, root *os.Root, files []File) error {
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := root.Open(file.Path)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("model path %q is not a regular file", file.Path)
		}
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Name: file.Name, Mode: 0444, Size: info.Size(), Typeflag: tar.TypeReg})
		}
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
