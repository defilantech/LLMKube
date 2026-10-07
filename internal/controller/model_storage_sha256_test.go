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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// #1965: an init-container download with Model.spec.sha256 set must verify
// the bytes before anything becomes the cache, and a failed verify must
// discard the partial, fail the start, and leave a digest-keyed
// <file>.<sha256>.sha256-rejected marker that bounds later retries until the
// pin is corrected instead of re-downloading on every kubelet backoff
// (a407e5c7e). Like the resume tests (#1765) and the revalidation tests
// (#1326), these drive the generated shell against the range-serving origin
// rather than string-matching curl claims.

// runVerifyScript runs a generated init script under sh with MODEL_SHA256
// set, mirroring runInitScript but returning the output and exit error for
// the rejection cases.
func runVerifyScript(t *testing.T, script, modelSource, modelPath, sha256val string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(),
		"MODEL_SOURCE="+modelSource,
		"MODEL_PATH="+modelPath,
		"CACHE_DIR="+filepath.Dir(modelPath),
		"MODEL_SHA256="+sha256val,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runVerifyScriptEnv is runVerifyScript plus extra NAME=value entries, for the
// S3 (AWS_*) and Hugging Face (HF_TOKEN) branches that carry credentials.
func runVerifyScriptEnv(t *testing.T, script, modelSource, modelPath, sha256val string, extraEnv ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(),
		"MODEL_SOURCE="+modelSource,
		"MODEL_PATH="+modelPath,
		"CACHE_DIR="+filepath.Dir(modelPath),
		"MODEL_SHA256="+sha256val,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// s3TestEnv points the generated S3 branch at a test origin. The bytes are what
// matter; the SigV4 signature is ignored by the stub, exactly as an origin that
// only checks the signature on a real bucket would not be exercised here. The
// point of these cases is the verify-then-publish path, not signing (which
// executor_s3_test.go covers for the agent's own signer).
func s3TestEnv(endpoint string) []string {
	return []string{
		"AWS_ENDPOINT_URL=" + endpoint,
		"AWS_REGION=us-east-1",
		"AWS_ACCESS_KEY_ID=AKIAEXAMPLE0000000",
		"AWS_SECRET_ACCESS_KEY=secretaccesskeyvalue0000000000000",
		"S3_BUCKET=bucket",
		"S3_KEY=model.gguf",
	}
}

func requireInitShellEnvironment(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available; skipping behavioral sha256 test")
	}
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("abc"), 0o644); err != nil {
		t.Fatalf("probe file: %v", err)
	}
	if out, err := exec.Command("stat", "-c", "%s", probe).Output(); err != nil || strings.TrimSpace(string(out)) != "3" {
		t.Skip("host stat lacks the -c size format (script targets the busybox/Linux init image)")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum not available; skipping behavioral sha256 test")
	}
}

func readOrFail(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be absent", path)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

// stampTriple is what llmkube_stamp_sha256 writes: the digest, the file
// size and its mtime, space separated. A stamp hit requires all three to
// still describe the file on disk.
//
// These are the same bytes the metal agent writes: pkg/agent's writeSHA256Stamp
// emits `<lowercase digest> <size> <mtime-seconds>` with no trailing newline
// (sha256StampValue, pinned by TestWriteSHA256Stamp_MatchesShellFormat), so the
// stamp-hit subtests below double as the cross-writer acceptance case. Changing
// this shape must land with a change to that test.
func stampTriple(t *testing.T, path, digest string) string {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fmt.Sprintf("%s %d %d", digest, fi.Size(), fi.ModTime().Unix())
}

func writeStamp(t *testing.T, path, digest string) {
	t.Helper()
	if err := os.WriteFile(path+".sha256", []byte(stampTriple(t, path, digest)), 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
}

// mustStamp asserts the sidecar is the digest+size+mtime triple describing the
// file, the format both the init container and the metal agent write (#1980).
func mustStamp(t *testing.T, path, digest string) {
	t.Helper()
	if got := readOrFail(t, path+".sha256"); got != stampTriple(t, path, digest) {
		t.Errorf("stamp = %q, want %q", got, stampTriple(t, path, digest))
	}
}

func partialsOf(t *testing.T, modelPath string) []string {
	t.Helper()
	matches, err := filepath.Glob(modelPath + ".*.tmp")
	if err != nil {
		t.Fatalf("glob partials: %v", err)
	}
	return matches
}

func TestModelInitSHA256_Behavioral(t *testing.T) {
	requireInitShellEnvironment(t)
	t.Run("download publishes stamp", sha256DownloadPublishesStamp)
	t.Run("mismatch discards the partial and records the rejection", sha256MismatchDiscardsPartialAndRecordsRejection)
	t.Run("correcting spec.sha256 retries cleanly", sha256CorrectingSpecSha256RetriesCleanly)
	t.Run("warm cache verifies stamps and skips the download", sha256WarmCacheVerifiesStampsAndSkipsTheDownload)
	t.Run("stamp hit skips re-hashing", sha256StampHitSkipsReHashing)
	t.Run("corrupt warm cache is replaced and refetched", sha256CorruptWarmCacheIsReplacedAndRefetched)
	t.Run("a rejection marker bounds later starts", sha256RejectionMarkerBoundsRetries)
	t.Run("a wrong pin does not delete a co-tenant's cache", sha256WrongPinDoesNotDeleteACoTenantsCache)
	t.Run("a co-tenant publish does not clear another's rejection", sha256CoTenantPublishDoesNotClearAnotherModelsRejection)
	t.Run("empty digest fails closed", sha256EmptyDigestFailsClosed)
	t.Run("OnChange download mismatch rejects before publish", sha256OnchangeDownloadMismatchRejectsBeforePublish)
	t.Run("OnChange unchanged skip still verifies", sha256OnchangeUnchangedSkipStillVerifies)
	t.Run("OnChange size-match corrupt is replaced and refetched", sha256OnchangeSizeMatchCorruptIsReplacedAndRefetched)
	t.Run("OnChange moved upstream keeps the pinned copy and skips re-download", sha256OnchangeMovedUpstreamKeepsPinnedCopyAndSkipsRedownload)
	t.Run("OnChange offline keeps a verified copy", sha256OnchangeOfflineKeepsAVerifiedCopy)
	t.Run("OnChange offline rejects a corrupt copy", sha256OnchangeOfflineRejectsACorruptCopy)
	t.Run("S3 download publishes a stamp", sha256S3DownloadPublishesStamp)
	t.Run("S3 mismatch discards the partial and records the rejection", sha256S3MismatchDiscardsPartialAndRecordsRejection)
	t.Run("S3 warm cache with a stamp skips the transfer", sha256S3WarmCacheSkipsTheTransfer)
	t.Run("S3 uncached download publishes a stamp", sha256S3UncachedDownloadPublishesStamp)
	t.Run("HF auth sends the token and publishes", sha256HFAuthSendsTokenAndPublishes)
	t.Run("HF auth mismatch discards the partial and records the rejection", sha256HFAuthMismatchDiscardsPartialAndRecordsRejection)
}

// The local (file://) branch is not exercised behaviorally: its command copies
// from a hardcoded /host-model/model.gguf, which a non-root CI runner cannot
// create. It stays covered by the wiring and golden tests; only its publish
// step (mv or llmkube_publish_sha256) is byte-identical to the branches here.

func sha256ResumeScript() string {
	return buildModelInitCommand(false, false, true, false, true, RefreshPolicyIfNotPresent)
}

func sha256RevalidateScript() string {
	// Composed through the builder so the hoisted fail-closed guard is in the
	// script, as it is in the real init container.
	return buildModelInitCommand(false, false, false, false, true, RefreshPolicyOnChange)
}

func sha256DownloadPublishesStamp(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	want := sha256Hex(o.content())

	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("download with matching sha256 failed: %v\n%s", err, out)
	}
	if got := readOrFail(t, modelPath); got != string(o.content()) {
		t.Errorf("published bytes are wrong")
	}
	if got := readOrFail(t, modelPath+".sha256"); got != stampTriple(t, modelPath, want) {
		t.Errorf("stamp = %q, want %q", got, stampTriple(t, modelPath, want))
	}
	mustNotExist(t, modelPath+"."+want+".sha256-rejected")
	if p := partialsOf(t, modelPath); len(p) != 0 {
		t.Errorf("partial survived a verified publish: %v", p)
	}
}

func sha256MismatchDiscardsPartialAndRecordsRejection(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))

	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, wrong)
	if err == nil {
		t.Fatalf("expected failure on hash mismatch\n%s", out)
	}
	if !strings.Contains(out, "spec.sha256 mismatch") {
		t.Errorf("output does not report the mismatch: %s", out)
	}
	if !strings.Contains(out, "discarded") {
		t.Errorf("output does not report the discard: %s", out)
	}
	mustNotExist(t, modelPath)
	mustExist(t, modelPath+"."+wrong+".sha256-rejected")
	if p := partialsOf(t, modelPath); len(p) != 0 {
		t.Errorf("the rejected partial was not discarded: %v", p)
	}
}

func sha256CorrectingSpecSha256RetriesCleanly(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))
	if _, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, wrong); err == nil {
		t.Fatalf("expected the first start to fail")
	}

	want := sha256Hex(o.content())
	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("after correcting the expected hash the download should proceed: %v\n%s", err, out)
	}
	if got := readOrFail(t, modelPath); got != string(o.content()) {
		t.Errorf("published bytes are wrong after recovery")
	}
	// The marker is keyed on the expected digest, so the corrected pin looks
	// for a different file and the wrong pin's marker is left inert.
	mustExist(t, modelPath+"."+wrong+".sha256-rejected")
	mustNotExist(t, modelPath+"."+want+".sha256-rejected")
}

func sha256WarmCacheVerifiesStampsAndSkipsTheDownload(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, o.content(), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	want := sha256Hex(o.content())

	o.fullFromZero.Store(0)
	o.rangeRequests.Store(0)
	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("verified warm cache failed: %v\n%s", err, out)
	}
	if n := o.fullFromZero.Load() + o.rangeRequests.Load(); n != 0 {
		t.Errorf("verified warm cache issued %d GETs, want 0", n)
	}
	if got := readOrFail(t, modelPath+".sha256"); got != stampTriple(t, modelPath, want) {
		t.Errorf("stamp written on warm cache = %q, want %q", got, stampTriple(t, modelPath, want))
	}
}

func sha256StampHitSkipsReHashing(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	want := sha256Hex(o.content())
	// Trust artifact semantics, pinned: a stamp whose digest, size and mtime
	// all still describe the file accepts it without reading it. The file
	// here is deliberately not the real bytes; re-hashing on every start
	// would cost a full multi-gigabyte hash per pod start.
	if err := os.WriteFile(modelPath, []byte("corrupt bytes"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	writeStamp(t, modelPath, want)

	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("stamp hit should accept the file: %v\n%s", err, out)
	}
	if !strings.Contains(out, "stamp hit") {
		t.Errorf("expected the stamp-hit skip message: %s", out)
	}
}

func sha256CorruptWarmCacheIsReplacedAndRefetched(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, []byte("corrupt bytes"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	want := sha256Hex(o.content())

	// A cache file that fails the re-hash was corrupted outside any download.
	// The check leaves it in place (the cache dir might be shared with a
	// co-tenant Model), and the same start's download branch replaces it with
	// the verified bytes, mirroring the corrupt cache-file recovery in
	// pkg/agent/executor.go. No rejection marker: the marker accuses the
	// origin, not the disk.
	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("corrupt warm cache must self-heal: %v\n%s", err, out)
	}
	if got := readOrFail(t, modelPath); got != string(o.content()) {
		t.Errorf("cache was not replaced with the verified bytes")
	}
	if got := readOrFail(t, modelPath+".sha256"); got != stampTriple(t, modelPath, want) {
		t.Errorf("stamp after self-heal = %q, want %q", got, stampTriple(t, modelPath, want))
	}
	mustNotExist(t, modelPath+"."+want+".sha256-rejected")
}

func sha256RejectionMarkerBoundsRetries(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))

	if out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, wrong); err == nil {
		t.Fatalf("expected the first start to fail on the digest\n%s", out)
	}
	mustExist(t, modelPath+"."+wrong+".sha256-rejected")

	// The marker bounds the retries: the next start fails on the hoisted
	// guard before any transfer, so a wrong multi-gigabyte pin cannot
	// re-download the whole artifact on every kubelet backoff. This mirrors
	// the metal agent's digestMismatchMemo.
	o.fullFromZero.Store(0)
	o.rangeRequests.Store(0)
	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, wrong)
	if err == nil {
		t.Fatalf("the wrong digest must still fail the start\n%s", out)
	}
	if n := o.fullFromZero.Load() + o.rangeRequests.Load(); n != 0 {
		t.Errorf("the second start issued %d GETs; the rejection marker did not bound the retry\n%s", n, out)
	}
}

// sha256WrongPinDoesNotDeleteACoTenantsCache pins #1971 review: the cache
// directory is keyed on the source alone, so two Models on one source share
// MODEL_PATH. A mis-pinned Model must fail without destroying the correct
// co-tenant's verified file and stamp.
func sha256WrongPinDoesNotDeleteACoTenantsCache(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	correct := sha256Hex(o.content())
	wrong := sha256Hex([]byte("a hash from another artifact"))

	// Model B pins correctly and publishes the shared cache entry.
	if out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, correct); err != nil {
		t.Fatalf("co-tenant publish failed: %v\n%s", err, out)
	}
	bStamp := readOrFail(t, modelPath+".sha256")

	// Model A pins the same source to a different digest and fails.
	if out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, wrong); err == nil {
		t.Fatalf("the wrong pin must fail the start\n%s", out)
	}
	if got := readOrFail(t, modelPath); got != string(o.content()) {
		t.Errorf("Model A deleted or changed Model B's cache file")
	}
	if got := readOrFail(t, modelPath+".sha256"); got != bStamp {
		t.Errorf("Model A deleted or changed Model B's stamp: got %q, want %q", got, bStamp)
	}
	mustExist(t, modelPath+"."+wrong+".sha256-rejected")
}

// sha256CoTenantPublishDoesNotClearAnotherModelsRejection keeps the marker
// keyed on the expected digest: a co-tenant's successful publish must not
// remove a different Model's rejection marker, or the mis-pinned Model would
// re-download on its next start.
func sha256CoTenantPublishDoesNotClearAnotherModelsRejection(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	correct := sha256Hex(o.content())
	wrong := sha256Hex([]byte("a hash from another artifact"))

	if out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, wrong); err == nil {
		t.Fatalf("the wrong pin must fail the start\n%s", out)
	}
	mustExist(t, modelPath+"."+wrong+".sha256-rejected")

	if out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, correct); err != nil {
		t.Fatalf("the correct co-tenant must still publish: %v\n%s", err, out)
	}
	mustExist(t, modelPath+"."+wrong+".sha256-rejected")
}

func sha256EmptyDigestFailsClosed(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	// The gated command must abort with no MODEL_SHA256 rather than transfer
	// unchecked (a gate that skips when its input is missing is a bypass).
	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, "")
	if err == nil {
		t.Fatalf("gated script with empty MODEL_SHA256 must fail\n%s", out)
	}
	if !strings.Contains(out, "refusing to transfer unchecked") {
		t.Errorf("expected the fail-closed message: %s", out)
	}
	mustNotExist(t, modelPath)
	if p := partialsOf(t, modelPath); len(p) != 0 {
		t.Errorf("fail-closed script left artifacts: %v", p)
	}
}

func sha256OnchangeDownloadMismatchRejectsBeforePublish(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))

	out, err := runVerifyScript(t, sha256RevalidateScript(), o.srv.URL+"/model.gguf", modelPath, wrong)
	if err == nil {
		t.Fatalf("OnChange download must not publish unverified bytes\n%s", out)
	}
	mustNotExist(t, modelPath)
	mustExist(t, modelPath+"."+wrong+".sha256-rejected")
	if p := partialsOf(t, modelPath); len(p) != 0 {
		t.Errorf("the rejected partial was not discarded: %v", p)
	}
}

func sha256OnchangeUnchangedSkipStillVerifies(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	want := sha256Hex(o.content())
	if err := os.WriteFile(modelPath, o.content(), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	// A single-field stamp from an older release: the triple comparison
	// treats it as a miss, re-hashes once and rewrites the stamp in the
	// current format.
	if err := os.WriteFile(modelPath+".sha256", []byte(want), 0o644); err != nil {
		t.Fatalf("seed legacy stamp: %v", err)
	}

	o.fullFromZero.Store(0)
	o.rangeRequests.Store(0)
	out, err := runVerifyScript(t, sha256RevalidateScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("OnChange with a stamped warm cache failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("expected the unchanged skip: %s", out)
	}
	if n := o.fullFromZero.Load() + o.rangeRequests.Load(); n != 0 {
		t.Errorf("stamped unchanged skip issued %d GETs, want 0", n)
	}
	if got := readOrFail(t, modelPath+".sha256"); got != stampTriple(t, modelPath, want) {
		t.Errorf("legacy stamp should be rewritten as the triple: got %q, want %q", got, stampTriple(t, modelPath, want))
	}
}

func sha256OnchangeSizeMatchCorruptIsReplacedAndRefetched(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	// Same size as the origin (so the size-match branch fires), wrong
	// bytes, no stamp: size equality must not substitute for the hash. The
	// stamp check gates the skip, fails, and the same run's download branch
	// replaces the corrupt bytes with the verified ones.
	if err := os.WriteFile(modelPath, []byte(strings.Repeat("x", contentALen)), 0o644); err != nil {
		t.Fatalf("seed corrupt same-size file: %v", err)
	}
	want := sha256Hex(o.content())

	out, err := runVerifyScript(t, sha256RevalidateScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("size-match with corrupt bytes must self-heal, not fail the start: %v\n%s", err, out)
	}
	if got := readOrFail(t, modelPath); got != string(o.content()) {
		t.Errorf("cache was not replaced with the verified bytes")
	}
	mustNotExist(t, modelPath+"."+want+".sha256-rejected")
}

// sha256OnchangeMovedUpstreamKeepsPinnedCopyAndSkipsRedownload drives the
// moved-upstream case: the pinned artifact is still the right thing to serve,
// so the run keeps it and exits 0, names the digest mismatch rather than
// blaming reachability, and the marker makes the next start skip the HEAD and
// the full re-download.
func sha256OnchangeMovedUpstreamKeepsPinnedCopyAndSkipsRedownload(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	pin := sha256Hex(o.content())
	if err := os.WriteFile(modelPath, o.content(), 0o644); err != nil {
		t.Fatalf("seed pinned cache: %v", err)
	}
	writeStamp(t, modelPath, pin)
	// The upstream moves to different bytes of a different size.
	o.version.Store("B")

	out, err := runVerifyScript(t, sha256RevalidateScript(), o.srv.URL+"/model.gguf", modelPath, pin)
	if err != nil {
		t.Fatalf("moved upstream with a valid pinned copy must exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "kept the pinned cached copy") {
		t.Errorf("expected the pinned-copy message, got: %s", out)
	}
	if got := readOrFail(t, modelPath); got != strings.Repeat("A", contentALen) {
		t.Errorf("the pinned copy was not kept")
	}
	mustExist(t, modelPath+"."+pin+".sha256-rejected")

	o.fullFromZero.Store(0)
	o.rangeRequests.Store(0)
	out, err = runVerifyScript(t, sha256RevalidateScript(), o.srv.URL+"/model.gguf", modelPath, pin)
	if err != nil {
		t.Fatalf("the second start must keep serving the pinned copy: %v\n%s", err, out)
	}
	if n := o.fullFromZero.Load() + o.rangeRequests.Load(); n != 0 {
		t.Errorf("the marker did not skip the re-download: %d GETs on the second start\n%s", n, out)
	}
}

func sha256OnchangeOfflineKeepsAVerifiedCopy(t *testing.T) {
	srv := httptest.NewServer(nil)
	dead := srv.URL
	srv.Close()
	o := newRangeOrigin(t, true)
	t.Cleanup(func() { _ = o }) // keep o for content(), its server is unused here
	want := sha256Hex(o.content())
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, o.content(), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Air-gapped restart: hashing is local, so the "kept cached copy"
	// fallback must still verify before exiting 0.
	out, err := runVerifyScript(t, sha256RevalidateScript(), dead+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("offline verified copy should exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "kept cached copy") {
		t.Errorf("expected the offline fallback message: %s", out)
	}
	if got := readOrFail(t, modelPath+".sha256"); got != stampTriple(t, modelPath, want) {
		t.Errorf("offline verification should write the stamp: got %q, want %q", got, stampTriple(t, modelPath, want))
	}
}

func sha256OnchangeOfflineRejectsACorruptCopy(t *testing.T) {
	srv := httptest.NewServer(nil)
	dead := srv.URL
	srv.Close()
	o := newRangeOrigin(t, true)
	_ = o
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, []byte("corrupt bytes"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	out, err := runVerifyScript(t, sha256RevalidateScript(), dead+"/model.gguf", modelPath, sha256Hex([]byte("expected")))
	if err == nil {
		t.Fatalf("offline fallback must not exit 0 on bytes that fail the check\n%s", out)
	}
	// The failing bytes are left in place (the cache dir might be shared with
	// a co-tenant Model); what matters is that the offline fallback refused
	// to exit 0 on them. A later start with a reachable origin replaces them.
	mustExist(t, modelPath)
}

func sha256S3DownloadPublishesStamp(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	want := sha256Hex(o.content())
	script := buildModelInitCommand(false, true, true, false, true, RefreshPolicyIfNotPresent)

	out, err := runVerifyScriptEnv(t, script, "s3://bucket/model.gguf", modelPath, want, s3TestEnv(o.srv.URL)...)
	if err != nil {
		t.Fatalf("S3 download with matching sha256 failed: %v\n%s", err, out)
	}
	if got := readOrFail(t, modelPath); got != string(o.content()) {
		t.Errorf("S3 published bytes are wrong")
	}
	mustStamp(t, modelPath, want)
	mustNotExist(t, modelPath+"."+want+".sha256-rejected")
}

func sha256S3MismatchDiscardsPartialAndRecordsRejection(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))
	script := buildModelInitCommand(false, true, true, false, true, RefreshPolicyIfNotPresent)

	out, err := runVerifyScriptEnv(t, script, "s3://bucket/model.gguf", modelPath, wrong, s3TestEnv(o.srv.URL)...)
	if err == nil {
		t.Fatalf("S3 mismatch must fail the start\n%s", out)
	}
	mustNotExist(t, modelPath)
	mustExist(t, modelPath+"."+wrong+".sha256-rejected")
	if p := partialsOf(t, modelPath); len(p) != 0 {
		t.Errorf("the rejected S3 partial was not discarded: %v", p)
	}
}

func sha256S3WarmCacheSkipsTheTransfer(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, o.content(), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	want := sha256Hex(o.content())
	writeStamp(t, modelPath, want)
	script := buildModelInitCommand(false, true, true, false, true, RefreshPolicyIfNotPresent)

	o.fullFromZero.Store(0)
	o.rangeRequests.Store(0)
	out, err := runVerifyScriptEnv(t, script, "s3://bucket/model.gguf", modelPath, want, s3TestEnv(o.srv.URL)...)
	if err != nil {
		t.Fatalf("S3 warm cache failed: %v\n%s", err, out)
	}
	if n := o.fullFromZero.Load() + o.rangeRequests.Load(); n != 0 {
		t.Errorf("S3 warm cache issued %d GETs, want 0", n)
	}
}

func sha256S3UncachedDownloadPublishesStamp(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	want := sha256Hex(o.content())
	script := buildModelInitCommand(false, true, false, false, true, RefreshPolicyIfNotPresent)

	out, err := runVerifyScriptEnv(t, script, "s3://bucket/model.gguf", modelPath, want, s3TestEnv(o.srv.URL)...)
	if err != nil {
		t.Fatalf("uncached S3 download with matching sha256 failed: %v\n%s", err, out)
	}
	if got := readOrFail(t, modelPath); got != string(o.content()) {
		t.Errorf("uncached S3 published bytes are wrong")
	}
	mustStamp(t, modelPath, want)
}

func sha256HFAuthSendsTokenAndPublishes(t *testing.T) {
	o := newRangeOrigin(t, true)
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			auth.Store(a)
		}
		o.handle(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	want := sha256Hex(o.content())
	script := buildModelInitCommand(false, false, true, true, true, RefreshPolicyIfNotPresent)

	out, err := runVerifyScriptEnv(t, script, srv.URL+"/model.gguf", modelPath, want, "HF_TOKEN=test-token")
	if err != nil {
		t.Fatalf("HF-auth download with matching sha256 failed: %v\n%s", err, out)
	}
	if got, _ := auth.Load().(string); got != "Bearer test-token" {
		t.Errorf("Authorization header at the origin = %q, want %q", got, "Bearer test-token")
	}
	if got := readOrFail(t, modelPath); got != string(o.content()) {
		t.Errorf("HF-auth published bytes are wrong")
	}
	mustStamp(t, modelPath, want)
}

func sha256HFAuthMismatchDiscardsPartialAndRecordsRejection(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))
	script := buildModelInitCommand(false, false, true, true, true, RefreshPolicyIfNotPresent)

	out, err := runVerifyScriptEnv(t, script, o.srv.URL+"/model.gguf", modelPath, wrong, "HF_TOKEN=test-token")
	if err == nil {
		t.Fatalf("HF-auth mismatch must fail the start\n%s", out)
	}
	mustNotExist(t, modelPath)
	mustExist(t, modelPath+"."+wrong+".sha256-rejected")
	if p := partialsOf(t, modelPath); len(p) != 0 {
		t.Errorf("the rejected HF-auth partial was not discarded: %v", p)
	}
}

// TestModelMultiFileSHA256_Behavioral drives the multi-file loop with a
// partially pinned staging set: the pinned file is verified and stamped, the
// unpinned file is staged exactly as before, and a pinned mismatch fails the
// start and records the digest-keyed rejection (#1978).
func TestModelMultiFileSHA256_Behavioral(t *testing.T) {
	requireInitShellEnvironment(t)
	t.Run("pinned file verified, unpinned staged", multiFileSHA256PinnedAndUnpinned)
	t.Run("pinned mismatch discards and records the rejection", multiFileSHA256MismatchRejects)
	t.Run("pinned warm cache skips the transfer", multiFileSHA256WarmCacheSkips)
	t.Run("gated command with no digest env fails closed", multiFileSHA256FailsClosed)
	t.Run("S3 pinned file verified and published", multiFileSHA256S3Publishes)
	t.Run("OnChange online mismatch rejects before publish", multiFileSHA256OnchangeDownloadMismatchRejects)
	t.Run("IfNotPresent corrupt warm cache is replaced", multiFileSHA256IfNotPresentCorruptCacheReplaced)
	t.Run("OnChange size-match corrupt is replaced", multiFileSHA256OnchangeSizeMatchCorruptReplaced)
	t.Run("OnChange offline keeps a verified pinned copy", multiFileSHA256OnchangeOfflineKeepsAVerifiedCopy)
	t.Run("OnChange offline rejects a corrupt pinned copy", multiFileSHA256OnchangeOfflineRejectsACorruptCopy)
	t.Run("marker bounds a repeat IfNotPresent start", multiFileSHA256MarkerBoundsIfNotPresent)
	t.Run("marker bounds a repeat OnChange start", multiFileSHA256MarkerBoundsOnChange)
	t.Run("orphan digest entry fails closed", multiFileSHA256OrphanDigestFailsClosed)
	t.Run("whitespace-padded digest key fails closed", multiFileSHA256WhitespaceKeyFailsClosed)
}

func multiFileSHA256FailsClosed(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	primary := filepath.Join(dir, "model.gguf")
	script := buildMultiFileInitCommand(true, false, false, true, RefreshPolicyIfNotPresent)

	out, err := runVerifyScriptEnv(t, script, o.srv.URL, primary, "", "MODEL_FILES=model.gguf")
	if err == nil {
		t.Fatalf("gated multi-file command with no MODEL_FILE_SHA256 must fail\n%s", out)
	}
	if !strings.Contains(out, "refusing to transfer unchecked") {
		t.Errorf("expected the fail-closed message: %s", out)
	}
	mustNotExist(t, primary)
}

func multiFileSHA256PinnedAndUnpinned(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	primary := filepath.Join(dir, "model.gguf")
	secondary := filepath.Join(dir, "other.gguf")
	want := sha256Hex(o.content())
	script := buildMultiFileInitCommand(true, false, false, true, RefreshPolicyIfNotPresent)

	out, err := runVerifyScriptEnv(t, script, o.srv.URL, primary, "",
		"MODEL_FILES=model.gguf\nother.gguf",
		"MODEL_FILE_SHA256="+want+" model.gguf")
	if err != nil {
		t.Fatalf("pinned multi-file download failed: %v\n%s", err, out)
	}
	if got := readOrFail(t, primary); got != string(o.content()) {
		t.Errorf("pinned primary bytes are wrong")
	}
	mustStamp(t, primary, want)
	if got := readOrFail(t, secondary); got != string(o.content()) {
		t.Errorf("unpinned secondary was not staged")
	}
	mustNotExist(t, secondary+".sha256")
	mustNotExist(t, primary+"."+want+".sha256-rejected")
}

func multiFileSHA256MismatchRejects(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	primary := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))
	script := buildMultiFileInitCommand(true, false, false, true, RefreshPolicyIfNotPresent)

	out, err := runVerifyScriptEnv(t, script, o.srv.URL, primary, "",
		"MODEL_FILES=model.gguf",
		"MODEL_FILE_SHA256="+wrong+" model.gguf")
	if err == nil {
		t.Fatalf("pinned multi-file mismatch must fail the start\n%s", out)
	}
	mustNotExist(t, primary)
	mustExist(t, primary+"."+wrong+".sha256-rejected")
	if p := partialsOf(t, primary); len(p) != 0 {
		t.Errorf("the rejected multi-file partial was not discarded: %v", p)
	}
}

func multiFileSHA256WarmCacheSkips(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	primary := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(primary, o.content(), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	want := sha256Hex(o.content())
	writeStamp(t, primary, want)
	script := buildMultiFileInitCommand(true, false, false, true, RefreshPolicyIfNotPresent)

	o.fullFromZero.Store(0)
	o.rangeRequests.Store(0)
	out, err := runVerifyScriptEnv(t, script, o.srv.URL, primary, "",
		"MODEL_FILES=model.gguf",
		"MODEL_FILE_SHA256="+want+" model.gguf")
	if err != nil {
		t.Fatalf("pinned multi-file warm cache failed: %v\n%s", err, out)
	}
	if n := o.fullFromZero.Load() + o.rangeRequests.Load(); n != 0 {
		t.Errorf("pinned multi-file warm cache issued %d GETs, want 0", n)
	}
}

func multiFileSHA256OnchangeOfflineKeepsAVerifiedCopy(t *testing.T) {
	srv := httptest.NewServer(nil)
	dead := srv.URL
	srv.Close()
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	primary := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(primary, o.content(), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	want := sha256Hex(o.content())
	script := buildMultiFileInitCommand(true, false, false, true, RefreshPolicyOnChange)

	// Air-gapped restart of an OnChange Model: the unreachable-origin fallback
	// must still verify the cached file before keeping it.
	out, err := runVerifyScriptEnv(t, script, dead, primary, "",
		"MODEL_FILES=model.gguf",
		"MODEL_FILE_SHA256="+want+" model.gguf")
	if err != nil {
		t.Fatalf("offline verified pinned copy should exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "kept cached copy") {
		t.Errorf("expected the offline fallback message: %s", out)
	}
	mustStamp(t, primary, want)
}

func multiFileSHA256OnchangeOfflineRejectsACorruptCopy(t *testing.T) {
	srv := httptest.NewServer(nil)
	dead := srv.URL
	srv.Close()
	dir := t.TempDir()
	primary := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(primary, []byte("corrupt bytes"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	want := sha256Hex([]byte("expected other bytes"))
	script := buildMultiFileInitCommand(true, false, false, true, RefreshPolicyOnChange)

	out, err := runVerifyScriptEnv(t, script, dead, primary, "",
		"MODEL_FILES=model.gguf",
		"MODEL_FILE_SHA256="+want+" model.gguf")
	if err == nil {
		t.Fatalf("offline fallback must not exit 0 on bytes that fail the pin\n%s", out)
	}
	if strings.Contains(out, "kept cached copy") {
		t.Errorf("the corrupt cached copy was reported kept: %s", out)
	}
	mustExist(t, primary)
	mustNotExist(t, primary+"."+want+".sha256-rejected")
}

// multiFileSHA256S3Publishes drives the S3 multi-file branch against the same
// stub origin the single-file S3 cases use. The publish path is distinct from
// the http path ($dest.tmp vs $MODEL_PARTIAL), so it needs its own behavioral
// case to pin the verify-then-publish route there.
func multiFileSHA256S3Publishes(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	primary := filepath.Join(dir, "model.gguf")
	want := sha256Hex(o.content())
	script := buildMultiFileInitCommand(true, true, false, true, RefreshPolicyIfNotPresent)

	out, err := runVerifyScriptEnv(t, script, "s3://bucket", primary, "",
		append([]string{
			"MODEL_FILES=model.gguf",
			"MODEL_FILE_SHA256=" + want + " model.gguf",
		}, s3TestEnv(o.srv.URL)...)...)
	if err != nil {
		t.Fatalf("S3 multi-file download with matching digest failed: %v\n%s", err, out)
	}
	if got := readOrFail(t, primary); got != string(o.content()) {
		t.Errorf("S3 multi-file published bytes are wrong")
	}
	mustStamp(t, primary, want)
	mustNotExist(t, primary+"."+want+".sha256-rejected")
}

func multiFileSHA256OnchangeDownloadMismatchRejects(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	primary := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))
	script := buildMultiFileInitCommand(true, false, false, true, RefreshPolicyOnChange)

	out, err := runVerifyScriptEnv(t, script, o.srv.URL, primary, "",
		"MODEL_FILES=model.gguf",
		"MODEL_FILE_SHA256="+wrong+" model.gguf")
	if err == nil {
		t.Fatalf("OnChange download must not publish unverified bytes\n%s", out)
	}
	mustNotExist(t, primary)
	mustExist(t, primary+"."+wrong+".sha256-rejected")
	if p := partialsOf(t, primary); len(p) != 0 {
		t.Errorf("the rejected multi-file partial was not discarded: %v", p)
	}
}

// multiFileSHA256IfNotPresentCorruptCacheReplaced pins the gated cache probe:
// an existing file that fails the per-file digest must not be served just
// because it exists, and the same start must replace it with verified bytes.
func multiFileSHA256IfNotPresentCorruptCacheReplaced(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	primary := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(primary, []byte("corrupt bytes"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	want := sha256Hex(o.content())
	script := buildMultiFileInitCommand(true, false, false, true, RefreshPolicyIfNotPresent)

	out, err := runVerifyScriptEnv(t, script, o.srv.URL, primary, "",
		"MODEL_FILES=model.gguf",
		"MODEL_FILE_SHA256="+want+" model.gguf")
	if err != nil {
		t.Fatalf("IfNotPresent corrupt warm cache must self-heal: %v\n%s", err, out)
	}
	if got := readOrFail(t, primary); got != string(o.content()) {
		t.Errorf("cache was not replaced with the verified bytes")
	}
	mustStamp(t, primary, want)
}

func multiFileSHA256OnchangeSizeMatchCorruptReplaced(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	primary := filepath.Join(dir, "model.gguf")
	// Same size as the origin so the size-match branch fires, wrong bytes and
	// no stamp: size equality alone must not skip the digest check.
	if err := os.WriteFile(primary, []byte(strings.Repeat("x", contentALen)), 0o644); err != nil {
		t.Fatalf("seed corrupt same-size file: %v", err)
	}
	want := sha256Hex(o.content())
	script := buildMultiFileInitCommand(true, false, false, true, RefreshPolicyOnChange)

	out, err := runVerifyScriptEnv(t, script, o.srv.URL, primary, "",
		"MODEL_FILES=model.gguf",
		"MODEL_FILE_SHA256="+want+" model.gguf")
	if err != nil {
		t.Fatalf("size-match with corrupt bytes must self-heal: %v\n%s", err, out)
	}
	if got := readOrFail(t, primary); got != string(o.content()) {
		t.Errorf("cache was not replaced with the verified bytes")
	}
	mustNotExist(t, primary+"."+want+".sha256-rejected")
}

// multiFileMarkerBounds drives two starts of the same failed pin: the first
// writes the digest-keyed rejection marker, and the second must fail on the
// marker before any transfer, leaving the cached file untouched. Without the
// guard a CrashLoopBackOff re-downloads the whole pinned shard on every
// restart.
func multiFileMarkerBounds(t *testing.T, policy string) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	primary := filepath.Join(dir, "model.gguf")
	stale := []byte("stale corrupt bytes")
	if err := os.WriteFile(primary, stale, 0o644); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}
	wrong := sha256Hex([]byte("a hash from another artifact"))
	script := buildMultiFileInitCommand(true, false, false, true, policy)

	if out, err := runVerifyScriptEnv(t, script, o.srv.URL, primary, "",
		"MODEL_FILES=model.gguf",
		"MODEL_FILE_SHA256="+wrong+" model.gguf"); err == nil {
		t.Fatalf("the first start must fail the wrong pin\n%s", out)
	}
	mustExist(t, primary+"."+wrong+".sha256-rejected")
	if got := readOrFail(t, primary); got != string(stale) {
		t.Fatalf("the rejected start changed the cached file: %q", got)
	}

	o.fullFromZero.Store(0)
	o.rangeRequests.Store(0)
	out, err := runVerifyScriptEnv(t, script, o.srv.URL, primary, "",
		"MODEL_FILES=model.gguf",
		"MODEL_FILE_SHA256="+wrong+" model.gguf")
	if err == nil {
		t.Fatalf("the second start must fail on the marker\n%s", out)
	}
	if !strings.Contains(out, "spec.fileSha256 mismatch for") || !strings.Contains(out, "sha256-rejected to retry") {
		t.Errorf("the second start does not name the marker message: %s", out)
	}
	if n := o.fullFromZero.Load() + o.rangeRequests.Load(); n != 0 {
		t.Errorf("the second start issued %d GETs; the marker did not bound the retry\n%s", n, out)
	}
	if got := readOrFail(t, primary); got != string(stale) {
		t.Errorf("the marker-hit start changed the cached file: %q", got)
	}
	mustExist(t, primary+"."+wrong+".sha256-rejected")
}

func multiFileSHA256MarkerBoundsIfNotPresent(t *testing.T) {
	multiFileMarkerBounds(t, RefreshPolicyIfNotPresent)
}

func multiFileSHA256MarkerBoundsOnChange(t *testing.T) {
	multiFileMarkerBounds(t, RefreshPolicyOnChange)
}

// multiFileSHA256OrphanDigestFailsClosed pins the fail-closed precheck: a
// declared digest for a path that is not one of $MODEL_FILES must abort rather
// than silently leave that file unpinned. The entry is injected directly into
// the env, bypassing admission.
func multiFileSHA256OrphanDigestFailsClosed(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	primary := filepath.Join(dir, "model.gguf")
	want := sha256Hex(o.content())
	script := buildMultiFileInitCommand(true, false, false, true, RefreshPolicyIfNotPresent)

	out, err := runVerifyScriptEnv(t, script, o.srv.URL, primary, "",
		"MODEL_FILES=model.gguf",
		"MODEL_FILE_SHA256="+want+" other.gguf")
	if err == nil {
		t.Fatalf("a declared digest for an unstaged path must fail closed\n%s", out)
	}
	if !strings.Contains(out, "not staged files") {
		t.Errorf("expected the orphan-path message: %s", out)
	}
	mustNotExist(t, primary)
}

// multiFileSHA256WhitespaceKeyFailsClosed pins that the init precheck itself
// refuses a digest key with leading or trailing whitespace, independently of
// the CEL rule that also rejects it at admission. Such a key resolves to no
// digest in llmkube_file_digest, so without the precheck the file would stage
// unpinned. Injected directly into the env, bypassing admission.
func multiFileSHA256WhitespaceKeyFailsClosed(t *testing.T) {
	o := newRangeOrigin(t, true)
	want := sha256Hex(o.content())
	script := buildMultiFileInitCommand(true, false, false, true, RefreshPolicyIfNotPresent)
	// The staged name is padded the same way as the key, so the orphan check
	// alone would pass and llmkube_file_digest (which splits on spaces) would
	// resolve no digest: the file would stage unpinned.
	for name, rel := range map[string]string{
		"trailing space": "model.gguf ",
		"leading space":  " model.gguf",
		"trailing tab":   "model.gguf\t",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			primary := filepath.Join(dir, "model.gguf")
			out, err := runVerifyScriptEnv(t, script, o.srv.URL, primary, "",
				"MODEL_FILES="+rel,
				"MODEL_FILE_SHA256="+want+" "+rel)
			if err == nil {
				t.Fatalf("a whitespace-padded digest key must fail closed\n%s", out)
			}
			if !strings.Contains(out, "whitespace") {
				t.Errorf("expected the whitespace-key message: %s", out)
			}
			mustNotExist(t, primary)
		})
	}
}

// TestModelPVCSHA256_Behavioral runs the verify-only command a pvc:// Model's
// init container runs (#1979). The command reads $MODEL_PATH, so it points at
// a temp file here instead of a mount; it must verify and must write no stamp,
// because the mount it really runs against is read-only.
func TestModelPVCSHA256_Behavioral(t *testing.T) {
	requireInitShellEnvironment(t)
	t.Run("matching bytes verify without stamping", pvcSHA256MatchingVerifies)
	t.Run("mismatch fails the init container", pvcSHA256MismatchFails)
	t.Run("missing path names the missing path", pvcSHA256MissingPath)
	t.Run("a directory names the directory", pvcSHA256Directory)
	t.Run("unreadable file names the permission problem", pvcSHA256Unreadable)
}

func pvcSHA256MatchingVerifies(t *testing.T) {
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	body := []byte("pre-staged-model-bytes")
	if err := os.WriteFile(modelPath, body, 0o644); err != nil {
		t.Fatalf("seed pvc file: %v", err)
	}
	want := sha256Hex(body)
	out, err := runVerifyScript(t, buildPVCVerifyCommand(), "pvc://models/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("matching pvc verify failed: %v\n%s", err, out)
	}
	mustNotExist(t, modelPath+".sha256")
}

func pvcSHA256MismatchFails(t *testing.T) {
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, []byte("corrupt"), 0o644); err != nil {
		t.Fatalf("seed pvc file: %v", err)
	}
	wrong := sha256Hex([]byte("expected-other-bytes"))
	out, err := runVerifyScript(t, buildPVCVerifyCommand(), "pvc://models/model.gguf", modelPath, wrong)
	if err == nil {
		t.Fatalf("pvc mismatch must fail the init container\n%s", out)
	}
	if !strings.Contains(out, "SHA256 mismatch for the pre-staged") {
		t.Errorf("expected the mismatch message: %s", out)
	}
}

func pvcSHA256MissingPath(t *testing.T) {
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "absent.gguf")
	out, err := runVerifyScript(t, buildPVCVerifyCommand(), "pvc://models/absent.gguf", modelPath, sha256Hex([]byte("x")))
	if err == nil {
		t.Fatalf("a missing pvc path must fail the init container\n%s", out)
	}
	if !strings.Contains(out, "does not exist on the mounted volume") {
		t.Errorf("expected the missing-path message: %s", out)
	}
}

func pvcSHA256Directory(t *testing.T) {
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(modelPath, 0o755); err != nil {
		t.Fatalf("seed directory: %v", err)
	}
	out, err := runVerifyScript(t, buildPVCVerifyCommand(), "pvc://models/a-directory", modelPath, sha256Hex([]byte("x")))
	if err == nil {
		t.Fatalf("a pvc directory must fail the init container\n%s", out)
	}
	if !strings.Contains(out, "is a directory, not a file") {
		t.Errorf("expected the directory message: %s", out)
	}
}

func pvcSHA256Unreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permission bits; the unreadable branch cannot be provoked")
	}
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "locked.gguf")
	if err := os.WriteFile(modelPath, []byte("x"), 0o000); err != nil {
		t.Fatalf("seed unreadable file: %v", err)
	}
	out, err := runVerifyScript(t, buildPVCVerifyCommand(), "pvc://models/locked.gguf", modelPath, sha256Hex([]byte("x")))
	if err == nil {
		t.Fatalf("an unreadable pvc file must fail the init container\n%s", out)
	}
	if !strings.Contains(out, "is not readable by the init container") {
		t.Errorf("expected the unreadable message: %s", out)
	}
}

func TestModelInitEnvVars_ModelFileSHA256(t *testing.T) {
	files := []string{"a.gguf", "b.gguf", "sub/c.gguf"}
	a := strings.Repeat("A", 64)
	c := strings.Repeat("c", 64)
	envs := multiFileInitEnvVars("https://example.com/repo", "/models/k", files, map[string]inferencev1alpha1.SHA256Digest{
		"a.gguf":     inferencev1alpha1.SHA256Digest(a),
		"sub/c.gguf": inferencev1alpha1.SHA256Digest(c),
	})
	var got string
	var found bool
	for _, e := range envs {
		if e.Name == "MODEL_FILE_SHA256" {
			got, found = e.Value, true
		}
	}
	if !found {
		t.Fatalf("MODEL_FILE_SHA256 not injected: %v", envs)
	}
	want := strings.ToLower(a) + " a.gguf\n" + strings.ToLower(c) + " sub/c.gguf"
	if got != want {
		t.Errorf("MODEL_FILE_SHA256 = %q, want %q (files order, digest first, lowercased)", got, want)
	}
	for _, e := range multiFileInitEnvVars("https://example.com/repo", "/models/k", files, nil) {
		if e.Name == "MODEL_FILE_SHA256" {
			t.Errorf("MODEL_FILE_SHA256 must be absent with no digests: %v", e)
		}
	}
}

func TestModelInitEnvVars_ModelSHA256(t *testing.T) {
	upper := "D9BA44419F2A73ED1A666885066C65A235AB70F337E2B31CBB3D062A5F5B8D4B"
	envs := modelInitEnvVars("https://example.com/model.gguf", "/models/k", "/models/k/model.gguf", upper)
	var got string
	var found bool
	for _, e := range envs {
		if e.Name == "MODEL_SHA256" {
			got, found = e.Value, true
		}
	}
	if !found {
		t.Fatalf("MODEL_SHA256 not injected: %v", envs)
	}
	if got != strings.ToLower(upper) {
		t.Errorf("MODEL_SHA256 = %q, want lowercased %q", got, strings.ToLower(upper))
	}
	for _, e := range modelInitEnvVars("https://example.com/model.gguf", "/models/k", "/models/k/model.gguf", "") {
		if e.Name == "MODEL_SHA256" {
			t.Errorf("MODEL_SHA256 must be absent when spec.sha256 is unset (gated commands are not built for such Models either): %v", e)
		}
	}
}
