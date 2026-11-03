package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const repoBaseMirror = "https://artifactory.corp.example/artifactory/api/huggingfaceml/hf-remote"

// A repo source staged with spec.files downloads from HF_ENDPOINT when the
// Secret names one, as huggingface_hub does, and from huggingface.co otherwise.
// A bare repo ID is the same repo as hf://. A source that already names a URL
// to fetch from, or is not a Hugging Face repo, is left alone.
func TestStagingRepoBase(t *testing.T) {
	tests := []struct {
		name, source, endpoint, want string
	}{
		{"bare repo id via the mirror", "nvidia/GLM-5.3-Flash-NVFP4", repoBaseMirror, repoBaseMirror + "/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/"},
		{"bare repo id without a mirror", "nvidia/GLM-5.3-Flash-NVFP4", "", "https://huggingface.co/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/"},
		{"endpoint with trailing slash", "nvidia/GLM-5.3-Flash-NVFP4", repoBaseMirror + "/", repoBaseMirror + "/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/"},
		{"hf scheme with revision via the mirror", "hf://org/repo@abc123", repoBaseMirror, repoBaseMirror + "/org/repo/resolve/abc123/"},
		{"hf scheme without a mirror", "hf://org/repo", "", "https://huggingface.co/org/repo/resolve/main/"},
		{"hf scheme with a trailing slash", "hf://org/repo/", "", "https://huggingface.co/org/repo/resolve/main/"},
		{"hf scheme with a trailing slash via the mirror", "hf://org/repo/", repoBaseMirror, repoBaseMirror + "/org/repo/resolve/main/"},
		{"bare repo id with a trailing slash", "org/repo/", "", "https://huggingface.co/org/repo/resolve/main/"},
		{"huggingface.co repo URL via the mirror", "https://huggingface.co/org/repo/resolve/main/", repoBaseMirror, repoBaseMirror + "/org/repo/resolve/main/"},
		{"mirror repo base is already a URL", repoBaseMirror + "/org/repo/resolve/main/", repoBaseMirror, repoBaseMirror + "/org/repo/resolve/main/"},
		{"s3 is not a repo", "s3://bucket/prefix", repoBaseMirror, "s3://bucket/prefix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stagingRepoBase(tt.source, tt.endpoint); got != tt.want {
				t.Errorf("stagingRepoBase(%q, %q) = %q, want %q", tt.source, tt.endpoint, got, tt.want)
			}
		})
	}
}

// The downloader for a bare repo ID with spec.files fetches from the mirror,
// with the token: the token rule is applied to the URL actually downloaded,
// not to the spec.source spelling, which for a bare ID names no host.
func TestMultiFileInitContainer_BareRepoIDViaMirror(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "glm", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          "nvidia/GLM-5.3-Flash-NVFP4",
			Format:          "safetensors",
			Files:           []string{"config.json", "tokenizer.json"},
			SourceSecretRef: &corev1.LocalObjectReference{Name: "huggingface"},
		},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "glm", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "glm", Runtime: "vllm"},
	}

	for _, useCache := range []bool{true, false} {
		cfg := buildModelStorageConfig(model, isvc, resolveEffectiveModelCache(isvc, model), "default", useCache, "", "", defaultPrefetchImage, 0, ModelCachePrepAlways, DefaultInitContainerUser, nil, repoBaseMirror)
		var downloader *corev1.Container
		for i := range cfg.initContainers {
			if cfg.initContainers[i].Name == "model-downloader" {
				downloader = &cfg.initContainers[i]
			}
		}
		if downloader == nil {
			t.Fatalf("useCache=%v: no model-downloader init container", useCache)
		}
		source := ""
		for _, e := range downloader.Env {
			if e.Name == "MODEL_SOURCE" {
				source = e.Value
			}
		}
		if want := repoBaseMirror + "/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/"; source != want {
			t.Errorf("useCache=%v: MODEL_SOURCE = %q, want %q", useCache, source, want)
		}
		script := strings.Join(downloader.Command, " ") + strings.Join(downloader.Args, " ")
		if !strings.Contains(script, authHeader) {
			t.Errorf("useCache=%v: downloader sends no bearer header to the mirror", useCache)
		}
	}
}

// Prefetch stages a Model before any InferenceService exists. A bare repo ID
// with spec.files is staged by the operator, so it is prefetchable; without
// spec.files it is still the runtime's to fetch.
func TestPrefetchEligible_BareRepoID(t *testing.T) {
	model := &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{
		Source: "nvidia/GLM-5.3-Flash-NVFP4", Prefetch: true, Files: []string{"config.json"},
	}}
	if !prefetchEligible(model) {
		t.Error("bare repo ID with spec.files is not prefetchable")
	}
	model.Spec.Files = nil
	if prefetchEligible(model) {
		t.Error("bare repo ID without spec.files became prefetchable")
	}
}

// The MissingSkipModelInit warning tells a user the init container has nothing
// to fetch. With spec.files it does, so the warning would steer them into
// breaking the deployment.
func TestShouldWarnMissingSkipModelInit_MultiFileRepo(t *testing.T) {
	model := &inferencev1alpha1.Model{
		Spec:   inferencev1alpha1.ModelSpec{Source: "nvidia/GLM-5.3-Flash-NVFP4"},
		Status: inferencev1alpha1.ModelStatus{Phase: PhaseReady},
	}
	isvc := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "vllm"}}
	if !shouldWarnMissingSkipModelInit(model, isvc) {
		t.Fatal("precondition: a bare repo ID without spec.files should warn")
	}
	model.Spec.Files = []string{"config.json"}
	if shouldWarnMissingSkipModelInit(model, isvc) {
		t.Error("warned to skip the init container for a Model it stages")
	}
}

// A bare repo ID with globs expands against the mirror's listing.
func TestReconcileRuntimeResolved_BareRepoIDListsFromMirror(t *testing.T) {
	body, err := os.ReadFile("testdata/hflisting/artifactory-tree-qwen.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/artifactory/api/huggingfaceml/hf-remote/api/models/Qwen/Qwen2.5-0.5B-Instruct/tree/main" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	endpoint := srv.URL + "/artifactory/api/huggingfaceml/hf-remote"

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          "Qwen/Qwen2.5-0.5B-Instruct",
			Format:          "safetensors",
			SourceSecretRef: &corev1.LocalObjectReference{Name: "huggingface"},
			Files:           []string{"config.json", "*.json", "*.safetensors"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "huggingface", Namespace: "default"},
		Data:       map[string][]byte{"HF_ENDPOINT": []byte(endpoint)},
	}
	r := newMultiFileGateReconciler(t, model, secret)
	r.AllowedRemoteHosts = []string{"127.0.0.1"}
	if _, err := r.reconcileRuntimeResolvedSource(context.Background(), model, ""); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	stored := getModel(t, r, "qwen")
	want := "config.json generation_config.json tokenizer.json tokenizer_config.json vocab.json model.safetensors"
	if got := strings.Join(stored.Status.StagedFiles, " "); stored.Status.Phase != PhaseReady || got != want {
		t.Errorf("phase = %q, stagedFiles = %q; want Ready and %q (conditions=%+v)", stored.Status.Phase, got, want, stored.Status.Conditions)
	}
	// 997,914,508 bytes summed from the recorded tree response (#2121).
	if stored.Status.Size != "951.7 MiB" {
		t.Errorf("status.size = %q, want %q (the staged total)", stored.Status.Size, "951.7 MiB")
	}
}

// A glob in spec.files no longer turns prefetch off (#2136): the prefetch path
// expands it from the repository listing, builds the Job from the expanded
// set, and holds the Model in Downloading until the Job completes. Ready then
// carries the expansion for the serving pods.
func TestReconcile_GlobWithPrefetchStagesExpandedFiles(t *testing.T) {
	body, err := os.ReadFile("testdata/hflisting/artifactory-tree-qwen.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/models/Qwen/Qwen2.5-0.5B-Instruct/tree/main") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	endpoint := srv.URL + "/artifactory/api/huggingfaceml/hf-remote"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "huggingface", Namespace: "default"},
		Data:       map[string][]byte{"HF_ENDPOINT": []byte(endpoint)},
	}
	const expanded = "config.json generation_config.json tokenizer.json tokenizer_config.json vocab.json model.safetensors"

	for _, source := range []string{"Qwen/Qwen2.5-0.5B-Instruct", "hf://Qwen/Qwen2.5-0.5B-Instruct"} {
		t.Run(source, func(t *testing.T) {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default", Generation: 1},
				Spec: inferencev1alpha1.ModelSpec{
					Source:          source,
					Format:          "safetensors",
					Prefetch:        true,
					SourceSecretRef: &corev1.LocalObjectReference{Name: "huggingface"},
					Files:           []string{"config.json", "*.json", "*.safetensors"},
				},
			}
			if !prefetchEligible(model) {
				t.Fatal("a Model with a glob in spec.files is not prefetch-eligible")
			}
			r := newMultiFileGateReconciler(t, model, secret)
			r.AllowedRemoteHosts = []string{"127.0.0.1"}
			r.InitContainerImage = defaultPrefetchImage
			r.Scheme = r.Client.Scheme()
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "qwen", Namespace: "default"}}
			ctx := context.Background()

			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			stored := getModel(t, r, "qwen")
			if stored.Status.Phase != PhaseDownloading {
				t.Fatalf("phase = %q, want Downloading while the prefetch Job runs (conditions=%+v)", stored.Status.Phase, stored.Status.Conditions)
			}
			var job batchv1.Job
			if err := r.Get(ctx, types.NamespacedName{Name: prefetchJobName(model), Namespace: "default"}, &job); err != nil {
				t.Fatalf("prefetch Job: %v", err)
			}
			if got := strings.ReplaceAll(prefetchJobFiles(&job), "\n", " "); got != expanded {
				t.Errorf("Job stages %q, want the expansion %q", got, expanded)
			}
			if len(stored.Status.Prefetch) != 1 || stored.Status.Prefetch[0].Phase != inferencev1alpha1.PrefetchPhaseRunning ||
				stored.Status.Prefetch[0].ClaimName != ModelCachePVCName || stored.Status.Prefetch[0].JobName != job.Name {
				t.Errorf("status.prefetch = %+v, want one Running entry for %q driven by %q", stored.Status.Prefetch, ModelCachePVCName, job.Name)
			}

			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			if err := r.Status().Update(ctx, &job); err != nil {
				t.Fatalf("complete Job: %v", err)
			}
			for i := 0; i < 2; i++ {
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatalf("reconcile after completion %d: %v", i, err)
				}
			}
			stored = getModel(t, r, "qwen")
			if got := strings.Join(stored.Status.StagedFiles, " "); stored.Status.Phase != PhaseReady || got != expanded {
				t.Errorf("phase = %q, stagedFiles = %q; want Ready and %q (conditions=%+v)", stored.Status.Phase, got, expanded, stored.Status.Conditions)
			}
			if len(stored.Status.Prefetch) != 1 || stored.Status.Prefetch[0].Phase != inferencev1alpha1.PrefetchPhaseSucceeded {
				t.Errorf("status.prefetch = %+v, want one Succeeded entry", stored.Status.Prefetch)
			}
		})
	}
}

// A listing that fails leaves the Model Degraded with the cause and starts no
// Job: a Job built from an unexpanded glob would stage a literal pattern.
func TestReconcile_GlobWithPrefetchListingFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	endpoint := srv.URL + "/artifactory/api/huggingfaceml/hf-remote"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "huggingface", Namespace: "default"},
		Data:       map[string][]byte{"HF_ENDPOINT": []byte(endpoint)},
	}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          "Qwen/Qwen2.5-0.5B-Instruct",
			Format:          "safetensors",
			Prefetch:        true,
			SourceSecretRef: &corev1.LocalObjectReference{Name: "huggingface"},
			Files:           []string{"config.json", "*.safetensors"},
		},
	}
	r := newMultiFileGateReconciler(t, model, secret)
	r.AllowedRemoteHosts = []string{"127.0.0.1"}
	r.InitContainerImage = defaultPrefetchImage
	r.Scheme = r.Client.Scheme()
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "qwen", Namespace: "default"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter != time.Minute {
		t.Errorf("requeue = %v, want a minute as the runtime-resolved path retries a listing", result.RequeueAfter)
	}
	stored := getModel(t, r, "qwen")
	cond := meta.FindStatusCondition(stored.Status.Conditions, ConditionDegraded)
	if stored.Status.Phase != PhaseFailed || cond == nil || cond.Reason != "RepoListingFailed" || !strings.Contains(cond.Message, "401") {
		t.Errorf("phase = %q, degraded = %+v; want Failed with RepoListingFailed naming the 401", stored.Status.Phase, cond)
	}
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("a prefetch Job was created from an unexpanded glob: %+v", jobs.Items)
	}
}

// prefetchJobFiles returns the newline-separated file list the Job's
// downloader stages, or "" when the Job has no downloader.
func prefetchJobFiles(job *batchv1.Job) string {
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name != "model-downloader" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "MODEL_FILES" {
				return e.Value
			}
		}
	}
	return ""
}

// A source copied from the Hub with a trailing slash is the same repository as
// the plain spelling, so the glob expands from the plain listing path and the
// Model reaches Ready. The handler answers 404 for anything but the exact
// single-slash path, as huggingface.co does: a doubled slash after the repo id
// 404s both the tree API and the resolve path, so a doubled path here surfaces
// as a Failed Model rather than a passing test.
func TestReconcile_TrailingSlashRepoSource(t *testing.T) {
	body, err := os.ReadFile("testdata/hflisting/artifactory-tree-qwen.json")
	if err != nil {
		t.Fatal(err)
	}
	const wantPath = "/artifactory/api/huggingfaceml/hf-remote/api/models/Qwen/Qwen2.5-0.5B-Instruct/tree/main"
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.EscapedPath())
		if r.URL.EscapedPath() != wantPath {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	endpoint := srv.URL + "/artifactory/api/huggingfaceml/hf-remote"

	for _, source := range []string{"hf://Qwen/Qwen2.5-0.5B-Instruct/", "Qwen/Qwen2.5-0.5B-Instruct/"} {
		t.Run(source, func(t *testing.T) {
			gotPaths = nil
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default", Generation: 1},
				Spec: inferencev1alpha1.ModelSpec{
					Source:          source,
					Format:          "safetensors",
					SourceSecretRef: &corev1.LocalObjectReference{Name: "huggingface"},
					Files:           []string{"config.json", "*.json", "*.safetensors"},
				},
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "huggingface", Namespace: "default"},
				Data:       map[string][]byte{"HF_ENDPOINT": []byte(endpoint)},
			}
			r := newMultiFileGateReconciler(t, model, secret)
			r.AllowedRemoteHosts = []string{"127.0.0.1"}
			if _, err := r.reconcileRuntimeResolvedSource(context.Background(), model, ""); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			stored := getModel(t, r, "qwen")
			want := "config.json generation_config.json tokenizer.json tokenizer_config.json vocab.json model.safetensors"
			got := strings.Join(stored.Status.StagedFiles, " ")
			if stored.Status.Phase != PhaseReady || got != want {
				t.Errorf("phase = %q, stagedFiles = %q; want Ready and %q (conditions=%+v, requested paths=%v)",
					stored.Status.Phase, got, want, stored.Status.Conditions, gotPaths)
			}
			if !containsString(gotPaths, wantPath) {
				t.Errorf("listing requested %v; want the canonical path %q", gotPaths, wantPath)
			}
		})
	}
}

// #2121: a runtime-resolved Model with nothing to list or probe has no
// measured size, so status.size stays empty. A literal "0" reads as a real
// measurement to every consumer.
func TestReconcile_RuntimeResolvedRepoIDSizeUnset(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "tiny", Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "TinyLlama/TinyLlama-1.1B-Chat-v1.0",
		},
	}
	r := newMultiFileGateReconciler(t, model)
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "tiny", Namespace: "default"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	stored := getModel(t, r, "tiny")
	if stored.Status.Phase != PhaseReady {
		t.Fatalf("phase = %q, want Ready; conditions=%+v", stored.Status.Phase, stored.Status.Conditions)
	}
	if stored.Status.Size != "" {
		t.Errorf("status.size = %q, want it unset for a Model the controller never sized", stored.Status.Size)
	}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
