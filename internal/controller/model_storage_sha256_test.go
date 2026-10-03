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
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #1965: an init-container download with Model.spec.sha256 set must verify
// the bytes before anything becomes the cache, and a rejected artifact must
// fail fast on every later start. Like the resume tests (#1765) and the
// revalidation tests (#1326), these drive the generated shell against the
// range-serving origin rather than string-matching curl claims.

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
	t.Run("mismatch keeps partial and writes marker", sha256MismatchKeepsPartialAndWritesMarker)
	t.Run("correcting spec.sha256 makes the marker inert", sha256CorrectingSpecSha256MakesTheMarkerInert)
	t.Run("warm cache verifies stamps and skips the download", sha256WarmCacheVerifiesStampsAndSkipsTheDownload)
	t.Run("stamp hit skips re-hashing", sha256StampHitSkipsReHashing)
	t.Run("corrupt warm cache without stamp fails and markers", sha256CorruptWarmCacheWithoutStampFailsAndMarkers)
	t.Run("OnChange download mismatch rejects before publish", sha256OnchangeDownloadMismatchRejectsBeforePublish)
	t.Run("OnChange unchanged skip still verifies", sha256OnchangeUnchangedSkipStillVerifies)
	t.Run("OnChange size-match corrupt rehashes and fails", sha256OnchangeSizeMatchCorruptRehashesAndFails)
	t.Run("OnChange offline keeps a verified copy", sha256OnchangeOfflineKeepsAVerifiedCopy)
	t.Run("OnChange offline rejects a corrupt copy", sha256OnchangeOfflineRejectsACorruptCopy)
}

func sha256ResumeScript() string {
	return buildModelInitCommand(false, false, true, false, true, RefreshPolicyIfNotPresent)
}

func sha256RevalidateScript() string {
	return sha256VerifyFns + remoteRevalidateScript(false, true)
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
	if got := readOrFail(t, modelPath+".sha256"); got != want {
		t.Errorf("stamp = %q, want %q", got, want)
	}
	mustNotExist(t, modelPath+".sha256-rejected")
	if p := partialsOf(t, modelPath); len(p) != 0 {
		t.Errorf("partial survived a verified publish: %v", p)
	}
}

func sha256MismatchKeepsPartialAndWritesMarker(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))

	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, wrong)
	if err == nil {
		t.Fatalf("expected failure on hash mismatch\n%s", out)
	}
	if !strings.Contains(out, "SHA256 mismatch") {
		t.Errorf("output does not report the mismatch: %s", out)
	}
	mustNotExist(t, modelPath)
	if got := readOrFail(t, modelPath+".sha256-rejected"); got != wrong {
		t.Errorf("marker = %q, want the rejected hash %q", got, wrong)
	}
	if p := partialsOf(t, modelPath); len(p) == 0 {
		t.Errorf("the partial was removed instead of kept for post-mortem")
	}

	// The next start must fail on the marker before touching the network:
	// resume would hit a byte-range error against a complete partial, and
	// an origin answering ranged requests with 200 would restart the whole
	// download every crash-loop cycle.
	o.fullFromZero.Store(0)
	o.rangeRequests.Store(0)
	out2, err2 := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, wrong)
	if err2 == nil {
		t.Fatalf("expected the marker to reject the second start\n%s", out2)
	}
	if !strings.Contains(out2, "rejected") {
		t.Errorf("second start does not name the rejection: %s", out2)
	}
	if n := o.fullFromZero.Load(); n != 0 {
		t.Errorf("rejected restart issued %d from-zero GETs, want 0\nfirst:\n%s\nsecond:\n%s", n, out, out2)
	}
	if n := o.rangeRequests.Load(); n != 0 {
		t.Errorf("rejected restart issued %d ranged GETs, want 0", n)
	}
	if p := partialsOf(t, modelPath); len(p) == 0 {
		t.Errorf("the rejected restart removed the kept partial")
	}
}

func sha256CorrectingSpecSha256MakesTheMarkerInert(t *testing.T) {
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
	mustNotExist(t, modelPath+".sha256-rejected")
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
	if got := readOrFail(t, modelPath+".sha256"); got != want {
		t.Errorf("stamp written on warm cache = %q, want %q", got, want)
	}
}

func sha256StampHitSkipsReHashing(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	want := sha256Hex(o.content())
	// Trust artifact semantics, pinned: a stamp naming the expected hash
	// accepts the file without reading it (mirrors verifyCachedDigest in
	// pkg/agent/executor.go). The file here is deliberately not the real
	// bytes; rewriting the stamp on every start would cost a full
	// multi-gigabyte hash per pod start.
	if err := os.WriteFile(modelPath, []byte("corrupt bytes"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	if err := os.WriteFile(modelPath+".sha256", []byte(want), 0o644); err != nil {
		t.Fatalf("seed stamp: %v", err)
	}

	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("stamp hit should accept the file: %v\n%s", err, out)
	}
	if !strings.Contains(out, "stamp hit") {
		t.Errorf("expected the stamp-hit skip message: %s", out)
	}
}

func sha256CorruptWarmCacheWithoutStampFailsAndMarkers(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, []byte("corrupt bytes"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	want := sha256Hex(o.content())

	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err == nil {
		t.Fatalf("expected failure for corrupt cached bytes\n%s", out)
	}
	if got := readOrFail(t, modelPath+".sha256-rejected"); got != want {
		t.Errorf("marker = %q, want %q", got, want)
	}
	mustNotExist(t, modelPath+".sha256")

	o.fullFromZero.Store(0)
	o.rangeRequests.Store(0)
	out2, err2 := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err2 == nil {
		t.Fatalf("expected the marker to reject the restart\n%s", out2)
	}
	if !strings.Contains(out2, "were rejected") {
		t.Errorf("warm-cache restart must fail on the marker, not re-hash: %s", out2)
	}
	if n := o.fullFromZero.Load() + o.rangeRequests.Load(); n != 0 {
		t.Errorf("restart against a corrupt cache issued %d GETs, want 0", n)
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
	if got := readOrFail(t, modelPath+".sha256-rejected"); got != wrong {
		t.Errorf("marker = %q, want %q", got, wrong)
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
	if err := os.WriteFile(modelPath+".sha256", []byte(want), 0o644); err != nil {
		t.Fatalf("seed stamp: %v", err)
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
}

func sha256OnchangeSizeMatchCorruptRehashesAndFails(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	// Same size as the origin (so the size-match branch fires), wrong
	// bytes, no stamp: size equality must not substitute for the hash.
	if err := os.WriteFile(modelPath, []byte(strings.Repeat("x", contentALen)), 0o644); err != nil {
		t.Fatalf("seed corrupt same-size file: %v", err)
	}
	want := sha256Hex(o.content())

	out, err := runVerifyScript(t, sha256RevalidateScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err == nil {
		t.Fatalf("expected failure: size match must not skip verification\n%s", out)
	}
	if got := readOrFail(t, modelPath+".sha256-rejected"); got != want {
		t.Errorf("marker = %q, want %q", got, want)
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
	if got := readOrFail(t, modelPath+".sha256"); got != want {
		t.Errorf("offline verification should write the stamp: %q", got)
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
}

// TestModelInitSHA256_DisabledStaysByteIdentical pins the fleet-rollout
// guarantee: for a Model without spec.sha256 the emitted init command must
// not contain any of the verify helpers, so upgrading the operator does not
// churn pod templates or change behavior for existing workloads.
func TestModelInitSHA256_DisabledStaysByteIdentical(t *testing.T) {
	tokens := []string{"llmkube_sha256_hash", "llmkube_precheck_sha256", "llmkube_check_sha256", "llmkube_publish_sha256"}
	for _, useCache := range []bool{true, false} {
		for _, isLocal := range []bool{true, false} {
			for _, isS3 := range []bool{true, false} {
				for _, policy := range []string{"", RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
					cmd := buildModelInitCommand(isLocal, isS3, useCache, false, false, policy)
					for _, tok := range tokens {
						if strings.Contains(cmd, tok) {
							t.Errorf("withSHA256=false command (useCache=%v isLocal=%v isS3=%v policy=%q) contains %q",
								useCache, isLocal, isS3, policy, tok)
						}
					}
				}
			}
		}
	}
	// The enabled path must actually wire the helpers in, or the negative
	// pin above is vacuous.
	if cmd := buildModelInitCommand(false, false, true, false, true, RefreshPolicyIfNotPresent); !strings.Contains(cmd, "llmkube_publish_sha256") || !strings.Contains(cmd, "llmkube_check_sha256") {
		t.Errorf("sha256-enabled IfNotPresent command is missing the publish/check gates")
	}
	if cmd := remoteRevalidateScript(false, true); !strings.Contains(cmd, "llmkube_precheck_sha256") || !strings.Contains(cmd, "llmkube_publish_sha256") {
		t.Errorf("sha256-enabled OnChange script is missing the precheck/publish gates")
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
			t.Errorf("MODEL_SHA256 must be absent when spec.sha256 is unset (the shell gates must no-op): %v", e)
		}
	}
}
