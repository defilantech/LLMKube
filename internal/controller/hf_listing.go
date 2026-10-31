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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	hubAPIBase = "https://huggingface.co"
	// A page of the Hub tree API holds up to 1000 entries; 256 pages bounds a
	// listing well past any model repository while keeping a looping server
	// from holding a reconcile forever.
	maxHFListingPages = 256
	maxHFListingBytes = 32 << 20
)

// hfListingTimeout bounds a whole listing; the page cap bounds only its requests.
var hfListingTimeout = 2 * time.Minute

// hfListingTreeURL returns the Hub API URL that lists the repository source
// stages from. It must name the host the init container downloads from (see
// stagingRepoBase): for a repo source, hfRepoHost; for a repo base on the
// HF_ENDPOINT origin, the mirror, whose API root is the endpoint URL itself.
// Any other source has no listing.
func hfListingTreeURL(source, hfEndpoint string) (string, bool) {
	if isHFRepoSource(source) {
		repo, rev, err := parseHFSource(source)
		if err != nil || repo == "" {
			return "", false
		}
		if rev == "" {
			rev = "main"
		}
		return hfTreeURL(hfRepoHost(hfEndpoint), repo, url.PathEscape(rev)), true
	}
	if !isOnHFEndpoint(source, hfEndpoint) {
		return "", false
	}
	base := strings.TrimRight(hfEndpoint, "/")
	src, err := url.Parse(source)
	if err != nil {
		return "", false
	}
	ep, err := url.Parse(base)
	if err != nil {
		return "", false
	}
	prefix := strings.TrimRight(ep.EscapedPath(), "/") + "/"
	if !strings.HasPrefix(src.EscapedPath(), prefix) {
		return "", false
	}
	// <org>/<repo>/resolve/<rev>, the base the init container appends files to.
	segs := strings.Split(strings.Trim(src.EscapedPath()[len(prefix):], "/"), "/")
	if len(segs) != 4 || segs[2] != "resolve" || segs[0] == "" || segs[1] == "" || segs[3] == "" {
		return "", false
	}
	rev, err := url.PathUnescape(segs[3])
	if err != nil {
		return "", false
	}
	return hfTreeURL(base, segs[0]+"/"+segs[1], url.PathEscape(rev)), true
}

func hfTreeURL(base, repo, escapedRev string) string {
	return base + "/api/models/" + repo + "/tree/" + escapedRev + "?recursive=true"
}

// listHFRepoFiles returns every file entry in the repository treeURL lists,
// sorted by path, following the tree API's Link pagination. A page link must
// stay on the listing's origin, since the bearer token rides on every page
// request.
func listHFRepoFiles(ctx context.Context, client *http.Client, treeURL, token string) ([]hfTreeEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, hfListingTimeout)
	defer cancel()
	files := []hfTreeEntry{}
	next := treeURL
	for page := 0; next != ""; page++ {
		if page == maxHFListingPages {
			return nil, fmt.Errorf("listing %s: more than %d pages", treeURL, maxHFListingPages)
		}
		if !isOnHFEndpoint(next, treeURL) {
			return nil, fmt.Errorf("listing %s: page link %s leaves the listing's origin", treeURL, next)
		}
		entries, link, err := fetchHFTreePage(ctx, client, next, token)
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", treeURL, err)
		}
		for _, e := range entries {
			if e.Type == "file" {
				files = append(files, e)
			}
		}
		next = link
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// hfFilePaths returns the paths of the listed entries, or nil for no listing.
// The nil is load-bearing: ResolveFileSet reads an empty list as "the listing
// matched nothing" and a nil as "this source has no listing at all".
func hfFilePaths(entries []hfTreeEntry) []string {
	if entries == nil {
		return nil
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	return paths
}

// hfTotalSize returns the summed size of files, and false when this listing
// cannot size them. All or nothing on purpose: a partial total understates
// what staging writes, and the metal agent admits a process from this number
// before it starts, so a low total is worse than none.
func hfTotalSize(entries []hfTreeEntry, files []string) (int64, bool) {
	if len(files) == 0 || len(entries) == 0 {
		return 0, false
	}
	sizes := make(map[string]int64, len(entries))
	for _, e := range entries {
		sizes[e.Path] = int64(e.Size)
	}
	var total int64
	for _, f := range files {
		size, ok := sizes[f]
		if !ok || size <= 0 {
			return 0, false
		}
		if total > math.MaxInt64-size {
			return 0, false
		}
		total += size
	}
	return total, true
}

type hfTreeEntry struct {
	Type string     `json:"type"`
	Path string     `json:"path"`
	Size hfTreeSize `json:"size"`
}

// hfTreeSize decodes the tree API's per-file size. A value the listing cannot
// read, digit for digit, as a whole number of bytes is unknown, not an error:
// the size is extra information here, and failing the page would turn a
// listing that used to expand globs into RepoListingFailed.
type hfTreeSize int64

func (s *hfTreeSize) UnmarshalJSON(b []byte) error {
	n, ok := parseWholeBytes(string(b))
	if !ok || n < 1 {
		*s = 0
		return nil
	}
	*s = hfTreeSize(n)
	return nil
}

// parseWholeBytes reads a JSON scalar's text as a byte count. A zero fraction
// is exact ("1610612736.0" is the same byte count); anything else, including
// an exponent, needs a float or a truncation to read, and a truncated size
// understates what staging writes (#2121).
func parseWholeBytes(b string) (int64, bool) {
	s := strings.TrimSpace(strings.Trim(b, `"`))
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, true
	}
	whole, frac, ok := strings.Cut(s, ".")
	if !ok || strings.Trim(frac, "0") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func fetchHFTreePage(ctx context.Context, client *http.Client, pageURL, token string) ([]hfTreeEntry, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var entries []hfTreeEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxHFListingBytes)).Decode(&entries); err != nil {
		return nil, "", fmt.Errorf("decode tree: %w", err)
	}
	next, err := nextLink(resp.Header.Values("Link"), req.URL)
	if err != nil {
		return nil, "", err
	}
	return entries, next, nil
}

// nextLink returns the absolute rel="next" target from RFC 8288 Link headers,
// or "" when there is none. Targets are delimited by angle brackets, not
// commas, since a cursor in the target's query may contain a comma. An
// unparseable next target fails the listing rather than truncating it.
func nextLink(headers []string, base *url.URL) (string, error) {
	for _, h := range headers {
		rest := h
		for {
			lt := strings.IndexByte(rest, '<')
			if lt < 0 {
				break
			}
			gt := strings.IndexByte(rest[lt:], '>')
			if gt < 0 {
				break
			}
			target := rest[lt+1 : lt+gt]
			rest = rest[lt+gt+1:]
			params := rest
			if i := strings.IndexByte(rest, '<'); i >= 0 {
				params = rest[:i]
			}
			if !linkRelIsNext(params) {
				continue
			}
			u, err := base.Parse(target)
			if err != nil {
				return "", fmt.Errorf("parse next link: %w", err)
			}
			return u.String(), nil
		}
	}
	return "", nil
}

func linkRelIsNext(params string) bool {
	for _, p := range strings.Split(params, ";") {
		k, v, _ := strings.Cut(strings.Trim(strings.TrimSpace(p), ","), "=")
		if strings.EqualFold(strings.TrimSpace(k), "rel") && strings.Trim(strings.TrimSpace(strings.TrimRight(v, ", ")), `"`) == "next" {
			return true
		}
	}
	return false
}

// repoFileListing lists the Model's source repository when spec.files holds a
// glob, and returns nil when no glob needs one. A source with no listing also
// returns nil, leaving ResolveFileSet to reject the glob.
func (r *ModelReconciler) repoFileListing(ctx context.Context, model *inferencev1alpha1.Model) ([]hfTreeEntry, error) {
	if !filesHaveGlob(model.Spec.Files) {
		return nil, nil
	}
	hfEndpoint := hfEndpointFromSecret(ctx, r.Client, model)
	treeURL, ok := hfListingTreeURL(model.Spec.Source, hfEndpoint)
	if !ok {
		return nil, nil
	}
	token := ""
	if isHFAuthSourceForEndpoint(treeURL, hfEndpoint) {
		token = r.hfTokenFromSecret(ctx, model)
	}
	return listHFRepoFiles(ctx, r.metadataClient(), treeURL, token)
}

// hfTokenFromSecret returns HF_TOKEN from the Model's sourceSecretRef, or ""
// when there is none: an ungated repository lists without one. Trimmed like
// the metal-agent's read (resolveHFAuth in pkg/agent/s3.go) and
// hfEndpointFromSecret; keep the three in step.
func (r *ModelReconciler) hfTokenFromSecret(ctx context.Context, model *inferencev1alpha1.Model) string {
	if r.Client == nil || model.Spec.SourceSecretRef == nil || model.Spec.SourceSecretRef.Name == "" {
		return ""
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: model.Spec.SourceSecretRef.Name, Namespace: model.Namespace}, secret); err != nil {
		return ""
	}
	return strings.TrimSpace(string(secret.Data["HF_TOKEN"]))
}

func filesHaveGlob(files []string) bool {
	for _, f := range files {
		if !strings.Contains(f, "://") && hasGlob(f) {
			return true
		}
	}
	return false
}

// stagingRepoBase returns the URL multi-file staging fetches "<base>/<file>"
// from. A Hugging Face repo source (hf://, a bare repo ID, or a huggingface.co
// repo URL) resolves on HF_ENDPOINT when the Secret names one, as
// huggingface_hub does, and on huggingface.co otherwise. Any other source
// already is that URL, or is not a repo, and is returned unchanged.
func stagingRepoBase(source, hfEndpoint string) string {
	if !isHFRepoSource(source) {
		return source
	}
	repo, rev, err := parseHFSource(source)
	if err != nil || repo == "" {
		return source
	}
	if rev == "" {
		rev = "main"
	}
	return hfRepoHost(hfEndpoint) + "/" + repo + "/resolve/" + url.PathEscape(rev) + "/"
}

// hfRepoHost is the host a repo source resolves on: the HF_ENDPOINT URL when it
// names an origin, huggingface.co otherwise. A scheme-less HF_ENDPOINT names no
// origin, matching the token gate, which would not send it the token either.
func hfRepoHost(hfEndpoint string) string {
	ep := strings.TrimRight(hfEndpoint, "/")
	if isOnHFEndpoint(ep, ep) {
		return ep
	}
	return hubAPIBase
}
