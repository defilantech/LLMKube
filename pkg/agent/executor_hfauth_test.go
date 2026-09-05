package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func hfTestExecutor(t *testing.T, store string) *MetalExecutor {
	t.Helper()
	lg, _ := zap.NewDevelopment()
	return &MetalExecutor{modelStorePath: store, logger: lg.Sugar()}
}

// A gated repository 401s without a bearer token. This asserts the token
// reaches the first hop.
func TestDownloadFile_SendsBearerToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("weights"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "model.gguf")
	if err := hfTestExecutor(t, dir).downloadFile(t.Context(), srv.URL+"/model.gguf", dst, "hf_secret"); err != nil {
		t.Fatalf("download: %v", err)
	}
	if got != "Bearer hf_secret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer hf_secret")
	}
	if b, _ := os.ReadFile(dst); string(b) != "weights" {
		t.Errorf("body = %q", string(b))
	}
}

// No token means no header at all, rather than an empty one, so an ungated
// repository behaves exactly as it did before this change.
func TestDownloadFile_NoTokenSendsNoHeader(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Authorization"]
		_, _ = w.Write([]byte("weights"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "m.gguf")
	if err := hfTestExecutor(t, dir).downloadFile(t.Context(), srv.URL+"/m.gguf", dst, ""); err != nil {
		t.Fatalf("download: %v", err)
	}
	if present {
		t.Error("Authorization header sent with no token")
	}
}

// The one that matters. Hugging Face answers a weights request with a redirect
// to its CDN on a different host; the token must not follow, or a credential
// for huggingface.co is handed to an unrelated origin. net/http strips it on a
// cross-host redirect, and this pins that behaviour so a future switch to a
// custom client or a CheckRedirect override cannot silently remove it.
func TestDownloadFile_TokenNotForwardedAcrossHosts(t *testing.T) {
	var cdnAuth string
	var cdnHit bool
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHit = true
		cdnAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("weights-from-cdn"))
	}))
	defer cdn.Close()

	var originAuth string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, cdn.URL+"/blob", http.StatusFound)
	}))
	defer origin.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "model.gguf")
	if err := hfTestExecutor(t, dir).downloadFile(t.Context(), origin.URL+"/model.gguf", dst, "hf_secret"); err != nil {
		t.Fatalf("download: %v", err)
	}
	if originAuth != "Bearer hf_secret" {
		t.Errorf("origin Authorization = %q, want the token on the first hop", originAuth)
	}
	if !cdnHit {
		t.Fatal("redirect was not followed, so the test proves nothing")
	}
	if cdnAuth != "" {
		t.Errorf("token leaked across hosts: CDN saw %q", cdnAuth)
	}
	if b, _ := os.ReadFile(dst); string(b) != "weights-from-cdn" {
		t.Errorf("body = %q, want the redirected content", string(b))
	}
}

// hf:// is NOT in this table. The metal downloader cannot fetch that scheme at
// all, so a predicate that accepted it would be authenticating a request that
// never gets made; TestDownloadFile_HFSchemeUnsupported pins that instead.
func TestIsHFAuthHost(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{"https://huggingface.co/org/repo/resolve/main/m.gguf", true},
		{"https://HuggingFace.CO/org/repo/resolve/main/m.gguf", true},
		{"https://www.huggingface.co/org/repo/resolve/main/m.gguf", true},
		{"https://WWW.HuggingFace.co/org/repo/resolve/main/m.gguf", true},
		{"http://huggingface.co/org/repo/resolve/main/m.gguf", true},
		{"https://huggingface.co.evil.example/org/repo/m.gguf", false},
		{"https://nothuggingface.co/org/repo/m.gguf", false},
		{"https://cdn.example.com/m.gguf", false},
		{"hf://meta-llama/Llama-Guard-4-12B", false},
		{"s3://models/org/repo/m.gguf", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isHFAuthHost(tc.source); got != tc.want {
			t.Errorf("isHFAuthHost(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// The metal path cannot fetch hf:// today: nothing here rewrites it to a
// resolve URL the way the init container's normalize_hf_source does, so the
// request fails on the scheme long before a token would matter. Pinned so the
// gap is visible and a future fix has to come through this test.
func TestDownloadFile_HFSchemeUnsupported(t *testing.T) {
	dir := t.TempDir()
	err := hfTestExecutor(t, dir).downloadFile(
		t.Context(), "hf://meta-llama/Llama-Guard-4-12B", filepath.Join(dir, "m.gguf"), "tok")
	if err == nil {
		t.Fatal("hf:// unexpectedly succeeded; if the metal path learned to resolve it, " +
			"isHFAuthHost should accept hf:// too and this test should change")
	}
	if !strings.Contains(err.Error(), "unsupported protocol scheme") {
		t.Errorf("want an unsupported-scheme error, got: %v", err)
	}
}
