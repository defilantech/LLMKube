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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// rangeChunkSize is how much we fetch per Range request when lazily reading a
// remote GGUF. The header + metadata KV + tensor-info section is small relative
// to the tensor data, so a few of these chunks normally cover all the metadata.
const rangeChunkSize = 1 << 20 // 1 MiB

// ParseReaderAt parses the GGUF header, metadata, and tensor info from an
// io.ReaderAt. Tensor data is never read. This is the core entry point; the
// streaming Parse and the remote ParseFromURL both ultimately produce the same
// GGUFFile.
//
// size is the total number of addressable bytes in r (e.g. the file or object
// length). It bounds reads so a truncated or lying source cannot run away.
func ParseReaderAt(r io.ReaderAt, size int64) (*GGUFFile, error) {
	return Parse(io.NewSectionReader(r, 0, size))
}

// ParseFromURL reads GGUF metadata from a remote http(s) URL without
// downloading the whole (potentially multi-GB) file. It first tries HTTP Range
// requests, lazily fetching only the byte ranges the metadata/tensor-info
// section needs (that section precedes the tensor data). If the server does not
// support Range, it falls back to streaming the body and stops once the
// metadata section has been consumed.
func ParseFromURL(ctx context.Context, url string) (*GGUFFile, error) {
	return parseFromURL(ctx, http.DefaultClient, url)
}

// ParseFromURLWithClient is ParseFromURL with a caller-supplied *http.Client.
// Callers that fetch attacker-influenced URLs (e.g. the LLMKube controller
// reading Model.spec.source) use this to route every request — HEAD probe,
// Range GETs, and the streaming fallback — through an SSRF-guarded client.
func ParseFromURLWithClient(ctx context.Context, client *http.Client, url string) (*GGUFFile, error) {
	return parseFromURL(ctx, client, url)
}

func parseFromURL(ctx context.Context, client *http.Client, url string) (*GGUFFile, error) {
	size, rangeOK, err := probeURL(ctx, client, url)
	if err != nil {
		return nil, err
	}

	if rangeOK && size > 0 {
		ra := &rangeReaderAt{ctx: ctx, client: client, url: url, size: size}
		f, err := ParseReaderAt(ra, size)
		if err == nil {
			return f, nil
		}
		// A range-backed parse should normally succeed; if it does not, fall
		// through to streaming so a server that mishandles Range still works.
	}

	return parseFromURLStreaming(ctx, client, url)
}

// probeURL discovers the object size and whether the server supports Range. It
// uses a HEAD request; servers that do not implement HEAD or omit the headers
// cause a fall back to streaming (rangeOK=false).
func probeURL(ctx context.Context, client *http.Client, url string) (size int64, rangeOK bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, false, fmt.Errorf("building HEAD request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("HEAD %s: %w", url, err)
	}
	defer closeBody(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// HEAD not supported / not allowed — let the caller stream.
		return 0, false, nil
	}

	rangeOK = strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, perr := strconv.ParseInt(cl, 10, 64); perr == nil {
			size = n
		}
	}
	return size, rangeOK, nil
}

// parseFromURLStreaming GETs the URL and parses from the response body. The
// parser only consumes the header/metadata/tensor-info prefix, so once Parse
// returns we close the body and the rest of the (large) tensor data is never
// transferred — the server stops writing when we close the connection.
func parseFromURLStreaming(ctx context.Context, client *http.Client, url string) (*GGUFFile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building GET request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	// Close (not drain) on return: parsing consumes only the header prefix and
	// abandoning the unread tensor data is the whole point of a header-only
	// read. Closing without draining lets the server stop writing.
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
	}

	return Parse(resp.Body)
}

// closeBody closes a response body. It intentionally does NOT drain the
// remaining bytes: for the header-only read we want to abandon the unread
// tensor data, not pull it over the wire to enable connection reuse.
func closeBody(rc io.Closer) {
	_ = rc.Close()
}

// ---------------------------------------------------------------------------
// rangeReaderAt — lazy io.ReaderAt backed by HTTP Range GETs
// ---------------------------------------------------------------------------

// rangeReaderAt implements io.ReaderAt by issuing HTTP Range requests on demand.
// It caches the most recently fetched chunk so the sequential reads the GGUF
// parser performs coalesce into a small number of requests rather than one per
// field.
type rangeReaderAt struct {
	ctx    context.Context
	client *http.Client
	url    string
	size   int64

	// Cached chunk covering [chunkOff, chunkOff+len(chunk)).
	chunk    []byte
	chunkOff int64
	haveData bool
}

func (r *rangeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("gguf: negative offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}

	n := 0
	for n < len(p) {
		if err := r.ctx.Err(); err != nil {
			return n, err
		}
		cur := off + int64(n)
		if cur >= r.size {
			return n, io.EOF
		}
		if !r.cacheCovers(cur) {
			if err := r.fetchChunk(cur); err != nil {
				return n, err
			}
		}
		idx := cur - r.chunkOff
		copied := copy(p[n:], r.chunk[idx:])
		n += copied
	}
	return n, nil
}

func (r *rangeReaderAt) cacheCovers(off int64) bool {
	return r.haveData && off >= r.chunkOff && off < r.chunkOff+int64(len(r.chunk))
}

// fetchChunk fetches a chunk starting at off via a Range GET and caches it.
func (r *rangeReaderAt) fetchChunk(off int64) error {
	end := off + rangeChunkSize - 1
	if end >= r.size {
		end = r.size - 1
	}

	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return fmt.Errorf("building range request: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("range GET %s: %w", r.url, err)
	}
	defer closeBody(resp.Body)

	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("range GET %s: server did not honor Range (status %s)", r.url, resp.Status)
	}

	buf, err := io.ReadAll(io.LimitReader(resp.Body, end-off+1))
	if err != nil {
		return fmt.Errorf("reading range body: %w", err)
	}
	// A 206 with no bytes would otherwise cache an empty chunk and spin the
	// ReadAt loop forever (it never advances). Treat it as an error.
	if len(buf) == 0 {
		return fmt.Errorf("range GET %s: server returned no bytes for %d-%d", r.url, off, end)
	}

	r.chunk = buf
	r.chunkOff = off
	r.haveData = true
	return nil
}
