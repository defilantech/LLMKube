package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/hfsource"
)

// The operator's own downloads had no way to authenticate: only s3:// sources
// got credentials, so every gated or private Hugging Face repository failed
// with 401 for GGUF models, for spec.files staging, and for prefetch (#1750).
// These pin the two halves of the fix: the header is emitted for huggingface.co
// sources, and for nothing else. An empty HF_ENDPOINT names no mirror, so these
// rows are the behaviour that predates #1900; the mirror rows live in
// pkg/hfsource and in the prefetch mirror test.

func TestIsHFAuthSource(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   bool
	}{
		{"hf scheme", "hf://meta-llama/Llama-Guard-4-12B", true},
		{"hf scheme with revision", "hf://meta-llama/Llama-Guard-4-12B@abc123", true},
		{"hf scheme upper", "HF://meta-llama/Llama-Guard-4-12B", true},
		{"resolved https url", "https://huggingface.co/org/repo/resolve/main/model.gguf", true},
		{"host case folded", "https://HuggingFace.CO/org/repo/resolve/main/model.gguf", true},
		{"www prefix", "https://www.huggingface.co/org/repo/resolve/main/model.gguf", true},
		// The host is case-insensitive per RFC 3986, including the www. label.
		// Folding after the trim left this false and downloaded unauthenticated.
		{"www prefix upper", "https://WWW.HuggingFace.co/org/repo/resolve/main/model.gguf", true},
		// The gate exists to keep the token off other hosts. A lookalike host is
		// the case that matters: a prefix match on "huggingface.co" without the
		// trailing slash would leak the token to an attacker-controlled domain.
		{"lookalike host", "https://huggingface.co.evil.example/org/repo/model.gguf", false},
		{"substring host", "https://nothuggingface.co/org/repo/model.gguf", false},
		{"other host", "https://cdn.example.com/model.gguf", false},
		{"s3 source", "s3://models/org/repo/model.gguf", false},
		{"local source", "/host-model/model.gguf", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHFAuthSourceForEndpoint(tc.source, ""); got != tc.want {
				t.Errorf("isHFAuthSourceForEndpoint(%q, \"\") = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}

// The host predicates must agree on every input, because each HF branch gates
// on isHuggingFaceURL and then calls through to hfURLPathSegments. When they
// disagreed, an uppercase host was a Hugging Face URL to the first and not a
// Hugging Face URL to the second, producing a source that is neither an HF repo
// nor a plain remote HTTP file, a combination the Model controller's classifier
// has no case for. Nothing failed when they drifted, which is why this exists.
func TestHFHostPredicatesAgree(t *testing.T) {
	sources := []string{
		"https://huggingface.co/Qwen/Qwen3-8B/resolve/main/model.gguf",
		"https://www.huggingface.co/Qwen/Qwen3-8B/resolve/main/model.gguf",
		"https://WWW.HuggingFace.co/Qwen/Qwen3-8B/resolve/main/model.gguf",
		"https://HUGGINGFACE.CO/Qwen/Qwen3-8B/resolve/main/model.gguf",
		"http://huggingface.co/Qwen/Qwen3-8B/resolve/main/model.gguf",
		"https://huggingface.co.evil.example/Qwen/Qwen3-8B/model.gguf",
		"https://cdn.example.com/model.gguf",
		"s3://models/Qwen/model.gguf",
		"",
	}
	for _, src := range sources {
		_, segOK := hfsource.HFURLPathSegments(src)
		if got := isHuggingFaceURL(src); got != segOK {
			t.Errorf("%q: isHuggingFaceURL=%v but hfURLPathSegments ok=%v; the two must agree",
				src, got, segOK)
		}
	}
}

// The repo path is case-sensitive even though the host is not, so folding the
// host must not reach the path.
func TestHFPathSegmentsPreserveCase(t *testing.T) {
	segs, ok := hfsource.HFURLPathSegments("https://WWW.HuggingFace.co/Qwen/Qwen3-8B/resolve/main/model.gguf")
	if !ok {
		t.Fatal("uppercase host rejected")
	}
	if len(segs) < 2 || segs[0] != "Qwen" || segs[1] != "Qwen3-8B" {
		t.Errorf("repo case not preserved: %v", segs)
	}
}

// authHeader is the exact text a gated fetch must carry. Asserting the whole
// string rather than a substring keeps a malformed header (a missing space
// after "Bearer", say) from passing.
const authHeader = `-H "Authorization: Bearer ${HF_TOKEN}"`

func TestBuildModelInitCommand_HFAuth(t *testing.T) {
	for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
		for _, useCache := range []bool{true, false} {
			t.Run(policy+"/cache="+boolStr(useCache), func(t *testing.T) {
				withAuth := buildModelInitCommand(false, false, useCache, true, false, policy)
				if !strings.Contains(withAuth, authHeader) {
					t.Errorf("HF source: no bearer header in:\n%s", withAuth)
				}
				if !strings.Contains(withAuth, "hf_curl()") {
					t.Error("HF source: the hf_curl definition is missing, so the script would fail with 'not found'")
				}
				// Defining the wrapper is not enough: the transfer has to call
				// it. A script that defines hf_curl and then runs plain curl
				// downloads unauthenticated and still contains every string a
				// laxer assertion would look for.
				if strings.Count(withAuth, "hf_curl ") == 0 {
					t.Errorf("HF source defines hf_curl but never invokes it:\n%s", withAuth)
				}
				for _, plainCall := range []string{"; curl -", "$(curl -", "if curl -"} {
					if strings.Contains(withAuth, plainCall) {
						t.Errorf("HF source still has an unauthenticated transfer (%q):\n%s", plainCall, withAuth)
					}
				}
				// --location-trusted would carry the token across the LFS redirect
				// to the CDN, which is exactly what must not happen.
				if strings.Contains(withAuth, "--location-trusted") {
					t.Error("--location-trusted sends the token to the redirect target")
				}

				plain := buildModelInitCommand(false, false, useCache, false, false, policy)
				if strings.Contains(plain, "HF_TOKEN") {
					t.Errorf("non-HF source leaks the token into:\n%s", plain)
				}
				if strings.Contains(plain, "hf_curl") {
					t.Error("non-HF source should call curl directly")
				}
			})
		}
	}
}

func TestBuildMultiFileInitCommand_HFAuth(t *testing.T) {
	for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
		t.Run(policy, func(t *testing.T) {
			withAuth := buildMultiFileInitCommand(true, false, true, policy)
			if !strings.Contains(withAuth, authHeader) {
				t.Errorf("HF multi-file: no bearer header in:\n%s", withAuth)
			}
			if !strings.Contains(withAuth, "hf_curl()") {
				t.Error("HF multi-file: hf_curl definition missing")
			}
			if strings.Count(withAuth, "hf_curl ") == 0 {
				t.Errorf("HF multi-file defines hf_curl but never invokes it:\n%s", withAuth)
			}
			for _, plainCall := range []string{"; curl -", "$(curl -", "if curl -"} {
				if strings.Contains(withAuth, plainCall) {
					t.Errorf("HF multi-file still has an unauthenticated transfer (%q):\n%s", plainCall, withAuth)
				}
			}

			plain := buildMultiFileInitCommand(true, false, false, policy)
			if strings.Contains(plain, "HF_TOKEN") {
				t.Errorf("non-HF multi-file leaks the token into:\n%s", plain)
			}

			// s3:// keeps signing with sigv4 and must not gain a bearer header
			// even when the source would otherwise qualify.
			s3 := buildMultiFileInitCommand(true, true, false, policy)
			if strings.Contains(s3, "HF_TOKEN") {
				t.Error("s3 multi-file should authenticate with sigv4 only")
			}
			if !strings.Contains(s3, "--aws-sigv4") {
				t.Error("s3 multi-file lost its sigv4 signing")
			}
		})
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func newHFSecretClient(t *testing.T, objs ...*corev1.Secret) *ModelReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		builder = builder.WithObjects(o)
	}
	return &ModelReconciler{Client: builder.Build()}
}

// hfEndpointFromSecret decides whether a non-Hugging-Face source is a mirror
// (#1900), so every unavailable case must resolve to "no mirror" rather than
// failing the reconcile: an absent Secret, a missing key, an empty ref, a nil
// model and a nil client (unit tests build the reconciler bare) each mean the
// ungated download that worked before this change.
func TestHFEndpointFromSecret(t *testing.T) {
	model := func(ref *corev1.LocalObjectReference) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://mirror.corp.example/m.gguf", SourceSecretRef: ref},
		}
	}
	withKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hf", Namespace: "default"},
		Data:       map[string][]byte{"HF_ENDPOINT": []byte("https://mirror.corp.example/repo")},
	}
	withoutKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-only", Namespace: "default"},
		Data:       map[string][]byte{"AWS_ENDPOINT_URL": []byte("https://s3.example")},
	}
	// An env-projected Secret value routinely carries a trailing newline
	// (kubectl --from-file, a base64 from echo). A control character makes the
	// value parse as no URL at all, so the gate would close silently and the
	// mirror would answer 401 with nothing pointing at the cause.
	withNewline := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hf-nl", Namespace: "default"},
		Data:       map[string][]byte{"HF_ENDPOINT": []byte("https://mirror.corp.example/repo\n")},
	}

	tests := []struct {
		name  string
		r     *ModelReconciler
		model *inferencev1alpha1.Model
		want  string
	}{
		{"nil client", &ModelReconciler{}, model(&corev1.LocalObjectReference{Name: "hf"}), ""},
		{"nil model", newHFSecretClient(t, withKey), nil, ""},
		{"no secret ref", newHFSecretClient(t, withKey), model(nil), ""},
		{"empty secret name", newHFSecretClient(t, withKey), model(&corev1.LocalObjectReference{}), ""},
		{"secret absent", newHFSecretClient(t, withKey), model(&corev1.LocalObjectReference{Name: "missing"}), ""},
		{"key absent", newHFSecretClient(t, withoutKey), model(&corev1.LocalObjectReference{Name: "aws-only"}), ""},
		{"key present", newHFSecretClient(t, withKey), model(&corev1.LocalObjectReference{Name: "hf"}), "https://mirror.corp.example/repo"},
		{"value with trailing newline", newHFSecretClient(t, withNewline), model(&corev1.LocalObjectReference{Name: "hf-nl"}), "https://mirror.corp.example/repo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hfEndpointFromSecret(context.Background(), tc.r.Client, tc.model); got != tc.want {
				t.Errorf("hfEndpointFromSecret() = %q, want %q", got, tc.want)
			}
		})
	}
}
