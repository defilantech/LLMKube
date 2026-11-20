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

package cli

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
)

const pruneTestSource = "hf://org/video-model"

// pruneModel is a multi-file Model in namespace default.
func pruneModel(name, source string, files ...string) inferencev1alpha1.Model {
	return inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec:       inferencev1alpha1.ModelSpec{Source: source, Files: files},
	}
}

// withStagedExpansion records the glob expansion the Model controller writes,
// for the Model's current generation.
func withStagedExpansion(m inferencev1alpha1.Model, staged ...string) inferencev1alpha1.Model {
	m.Status.StagedFiles = staged
	m.Status.Conditions = []metav1.Condition{{
		Type: "Available", Status: metav1.ConditionTrue, ObservedGeneration: m.Generation,
		Reason: "Ready", LastTransitionTime: metav1.Now(),
	}}
	return m
}

// files builds a cache entry listing with one byte per file unless sized.
func files(rels ...string) []cacheFile {
	out := make([]cacheFile, len(rels))
	for i, rel := range rels {
		out[i] = cacheFile{Rel: rel, Size: int64(len(rel))}
	}
	return out
}

func rels(fs []cacheFile) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Rel
	}
	slices.Sort(out)
	return out
}

// planFor resolves the owners of the test source's key and plans its entry.
func planFor(t *testing.T, models []inferencev1alpha1.Model, present []cacheFile) entryPrunePlan {
	t.Helper()
	key := cachekey.Compute(pruneTestSource)
	owners := entryOwnersByKey(models)[key]
	if owners == nil {
		t.Fatalf("no owners resolved for key %s", key)
	}
	if owners.Refusal != "" {
		t.Fatalf("entry refused: %s", owners.Refusal)
	}
	return planEntryPrune(key, entryKeepSet(owners, nil), present)
}

func TestPlanEntryPrune_KeepsTheUnionOfModelsSharingASource(t *testing.T) {
	models := []inferencev1alpha1.Model{
		pruneModel("fp16", pruneTestSource, "model-fp16.safetensors", "config.json"),
		pruneModel("fp8", pruneTestSource, "model-fp8.safetensors", "config.json"),
		// Another source's Model must not widen this entry's keep set.
		pruneModel("other", "hf://org/other", "stale.safetensors"),
	}
	plan := planFor(t, models, files(
		"model-fp16.safetensors", "model-fp8.safetensors", "config.json",
		"model-bf16.safetensors", "stale.safetensors",
	))

	if got, want := rels(plan.Remove), []string{"model-bf16.safetensors", "stale.safetensors"}; !slices.Equal(got, want) {
		t.Fatalf("Remove = %v, want %v", got, want)
	}
	wantKept := []string{"config.json", "model-fp16.safetensors", "model-fp8.safetensors"}
	if got, want := rels(plan.Kept), wantKept; !slices.Equal(got, want) {
		t.Fatalf("Kept = %v, want %v", got, want)
	}
	if plan.Bytes != int64(len("model-bf16.safetensors")+len("stale.safetensors")) {
		t.Fatalf("Bytes = %d, want the sum of the removed files' sizes", plan.Bytes)
	}
}

func TestPlanEntryPrune_ResolvesGlobsTheWayStagingDoes(t *testing.T) {
	// Staging resolves a glob from status.stagedFiles, the expansion the
	// Model controller recorded against the repository listing.
	model := withStagedExpansion(
		pruneModel("video", pruneTestSource, "transformer/*.safetensors", "model_index.json"),
		"transformer/diffusion-00001-of-00002.safetensors",
		"transformer/diffusion-00002-of-00002.safetensors",
		"model_index.json",
	)
	plan := planFor(t, []inferencev1alpha1.Model{model}, files(
		"model_index.json",
		"transformer/diffusion-00001-of-00002.safetensors",
		"transformer/diffusion-00002-of-00002.safetensors",
		// A shard from an earlier expansion matches the glob textually but
		// is not in the recorded expansion, so staging would not fetch it.
		"transformer/diffusion-00003-of-00003.safetensors",
	))
	want := []string{"transformer/diffusion-00003-of-00003.safetensors"}
	if got := rels(plan.Remove); !slices.Equal(got, want) {
		t.Fatalf("Remove = %v, want %v", got, want)
	}
}

func TestEntryOwnersByKey_RefusesAnEntryWhoseGlobIsNotResolved(t *testing.T) {
	// The recorded expansion is for an older generation: staging itself
	// refuses to run on it, and prune cannot know the file set either.
	stale := withStagedExpansion(pruneModel("video", pruneTestSource, "transformer/*.safetensors"),
		"transformer/a.safetensors")
	stale.Generation = 2
	owners := entryOwnersByKey([]inferencev1alpha1.Model{
		stale,
		pruneModel("other-files", pruneTestSource, "config.json"),
	})[cachekey.Compute(pruneTestSource)]
	if owners == nil || owners.Refusal == "" {
		t.Fatalf("owners = %+v, want a refusal for an unresolvable glob", owners)
	}
	if !strings.Contains(owners.Refusal, "video") {
		t.Fatalf("refusal %q does not name the Model", owners.Refusal)
	}
}

func TestPlanEntryPrune_Subdirectories(t *testing.T) {
	models := []inferencev1alpha1.Model{
		pruneModel("image", pruneTestSource,
			"transformer/diffusion_pytorch_model.safetensors",
			"vae/diffusion_pytorch_model.safetensors",
			"model_index.json"),
	}
	plan := planFor(t, models, files(
		"model_index.json",
		"transformer/diffusion_pytorch_model.safetensors",
		"vae/diffusion_pytorch_model.safetensors",
		"vae/diffusion_pytorch_model.bin",
		"text_encoder/model.safetensors",
		"text_encoder/config.json",
	))
	want := []string{"text_encoder/config.json", "text_encoder/model.safetensors", "vae/diffusion_pytorch_model.bin"}
	if got := rels(plan.Remove); !slices.Equal(got, want) {
		t.Fatalf("Remove = %v, want %v", got, want)
	}
}

func TestPlanEntryPrune_URLEntriesAndMmproj(t *testing.T) {
	m := pruneModel("vlm", pruneTestSource,
		"model-Q4_K_M.gguf",
		"https://example.com/releases/download/v1/extra.gguf",
	)
	m.Spec.Mmproj = "mmproj-F16.gguf"
	// A URL entry keys the directory on the file list too.
	key := cachekey.EffectiveKey(&m)
	owners := entryOwnersByKey([]inferencev1alpha1.Model{m})[key]
	if owners == nil || owners.Refusal != "" {
		t.Fatalf("owners = %+v, want a resolved entry", owners)
	}
	plan := planEntryPrune(key, entryKeepSet(owners, nil),
		files("model-Q4_K_M.gguf", "extra.gguf", "mmproj-F16.gguf", "mmproj-BF16.gguf"))
	if got, want := rels(plan.Remove), []string{"mmproj-BF16.gguf"}; !slices.Equal(got, want) {
		t.Fatalf("Remove = %v, want %v", got, want)
	}
}

// stagingWrites returns every bookkeeping file staging writes for artifact,
// spelled the way internal/controller/model_storage.go writes it.
func stagingWrites(artifact string) []string {
	digest := strings.Repeat("ab", 32)
	validator := "0123456789ab"
	return []string{
		artifact + ".sha256",                                            // llmkube_stamp_sha256
		artifact + "." + digest + ".sha256-rejected",                    // llmkube_publish_sha256 marker
		artifact + ".llmkube-size",                                      // publishFile size record
		artifact + "." + validator + ".tmp",                             // validatorDeriveAndSweep partial
		artifact + ".tmp",                                               // S3 partial
		artifact + ".sha256.llmkube-tmp.Ab3dE9",                         // stamp written through llmkube_atomic_write
		artifact + ".llmkube-size.llmkube-tmp.Ab3dE9",                   // size record temp
		artifact + "." + digest + ".sha256-rejected.llmkube-tmp.x1Y2z3", // marker temp
		artifact + ".llmkube-size.Ab3dE9",                               // pre-#2255 size temp
		artifact + "." + digest + ".sha256-rejected.Ab3dE9",             // pre-#2255 marker temp
		artifact + ".sha256.Ab3dE9",                                     // pre-#2255 stamp temp
	}
}

func TestPlanEntryPrune_NeverRemovesStagingBookkeeping(t *testing.T) {
	models := []inferencev1alpha1.Model{
		pruneModel("m", pruneTestSource, "transformer/model.safetensors"),
	}
	artifacts := []string{"transformer/model.safetensors", "dropped/old.safetensors", "never-staged.bin"}
	bookkeeping := make([]string, 0, 1+len(artifacts)*len(stagingWrites("")))
	bookkeeping = append(bookkeeping, ".llmkube-download.lock")
	for _, artifact := range artifacts {
		bookkeeping = append(bookkeeping, stagingWrites(artifact)...)
	}
	present := append([]string{"transformer/model.safetensors", "dropped/old.safetensors"}, bookkeeping...)

	plan := planFor(t, models, files(present...))

	if got, want := rels(plan.Remove), []string{"dropped/old.safetensors"}; !slices.Equal(got, want) {
		t.Fatalf("Remove = %v, want only the dropped artifact itself", got)
	}
	slices.Sort(bookkeeping)
	if got := rels(plan.Protected); !slices.Equal(got, bookkeeping) {
		t.Fatalf("Protected = %v\nwant %v", got, bookkeeping)
	}
}

func TestEntryOwnersByKey_LeavesSingleFileEntriesAlone(t *testing.T) {
	multi := pruneModel("multi", pruneTestSource, "model.safetensors")
	key := cachekey.EffectiveKey(&multi)
	single := inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "single", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: key},
	}
	owners := entryOwnersByKey([]inferencev1alpha1.Model{multi, single})
	if owners[key] == nil || owners[key].Refusal == "" {
		t.Fatalf("owners[%s] = %+v, want a refusal for an entry a single-file Model uses", key, owners[key])
	}

	onlySingle := single
	onlySingle.Status.CacheKey = "0123456789abcdef"
	if got := entryOwnersByKey([]inferencev1alpha1.Model{onlySingle}); len(got) != 0 {
		t.Fatalf("entryOwnersByKey(single-file only) = %v, want no entries in scope", got)
	}
}

func TestEntryKeepSet_PrefetchStatusFiles(t *testing.T) {
	m := pruneModel("m", pruneTestSource, "model-v2.safetensors")
	key := cachekey.EffectiveKey(&m)
	// A pod that consumes a prefetched claim verifies the Job's recorded
	// list until a new prefetch replaces it.
	m.Status.Prefetch = []inferencev1alpha1.PrefetchStatus{{
		ClaimName: "llmkube-model-cache", CacheKey: key, Phase: inferencev1alpha1.PrefetchPhaseSucceeded,
		Files: []string{"model-v1.safetensors"},
	}}
	owners := entryOwnersByKey([]inferencev1alpha1.Model{m})[key]
	plan := planEntryPrune(key, entryKeepSet(owners, nil),
		files("model-v1.safetensors", "model-v2.safetensors", "model-v0.safetensors"))
	if got, want := rels(plan.Remove), []string{"model-v0.safetensors"}; !slices.Equal(got, want) {
		t.Fatalf("Remove = %v, want %v", got, want)
	}
}

// downloaderPod is a serving or prefetch pod whose model-downloader stages key
// on claim.
func downloaderPod(name, claim, key string, state corev1.ContainerState, files ...string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{
				Name: "model-downloader",
				Env: []corev1.EnvVar{
					{Name: "CACHE_DIR", Value: "/models/" + key},
					{Name: "MODEL_FILES", Value: strings.Join(files, "\n")},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models"}},
			}},
			Containers: []corev1.Container{{Name: "llama-server"}},
			Volumes: []corev1.Volume{{Name: "model-cache", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			}}},
		},
		Status: corev1.PodStatus{
			Phase:                 corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "model-downloader", State: state}},
		},
	}
}

var (
	stateRunning = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	stateDone    = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	stateFailed  = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
)

func TestStagingPodsOnClaim(t *testing.T) {
	key := cachekey.Compute(pruneTestSource)
	const claim = "llmkube-model-cache"

	serving := downloaderPod("serving", claim, key, stateDone, "old-list.safetensors")
	serving.Status.Phase = corev1.PodRunning
	downloading := downloaderPod("downloading", claim, key, stateRunning, "new.safetensors")
	crashLooping := downloaderPod("crashloop", claim, key, stateFailed, "new.safetensors")
	unstarted := downloaderPod("unstarted", claim, key, corev1.ContainerState{}, "new.safetensors")
	unstarted.Status.InitContainerStatuses = nil
	otherClaim := downloaderPod("other-claim", "isvc-model-cache", key, stateRunning, "x.safetensors")
	finishedJob := downloaderPod("job", claim, key, stateRunning, "y.safetensors")
	finishedJob.Status.Phase = corev1.PodFailed

	got := stagingPodsOnClaim([]corev1.Pod{serving, downloading, crashLooping, unstarted, otherClaim, finishedJob}, claim)
	staging := got[key]
	if staging == nil {
		t.Fatalf("stagingPodsOnClaim = %v, want an entry for %s", got, key)
	}
	if want := []string{"crashloop", "downloading", "unstarted"}; !slices.Equal(staging.Busy, want) {
		t.Fatalf("Busy = %v, want %v", staging.Busy, want)
	}
	keep := entryKeepSet(nil, staging)
	for _, rel := range []string{"old-list.safetensors", "new.safetensors"} {
		if !keep[rel] {
			t.Errorf("keep set lacks %s, which a live pod on the claim lists", rel)
		}
	}
	for _, rel := range []string{"x.safetensors", "y.safetensors"} {
		if keep[rel] {
			t.Errorf("keep set has %s from a pod that is finished or on another claim", rel)
		}
	}
}

func TestStagingPodsOnClaim_ServingContainerIsNotADownload(t *testing.T) {
	key := cachekey.Compute(pruneTestSource)
	pod := downloaderPod("serving", "llmkube-model-cache", key, stateDone, "a.safetensors")
	pod.Status.Phase = corev1.PodRunning
	// A runtime container mounting the claim runs for as long as the pod
	// serves; only an init container stages.
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "CACHE_DIR", Value: "/models/" + key}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models", ReadOnly: true}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "llama-server", State: stateRunning}}

	if got := stagingPodsOnClaim([]corev1.Pod{pod}, "llmkube-model-cache")[key]; got == nil || len(got.Busy) != 0 {
		t.Fatalf("staging = %+v, want the entry known and not busy", got)
	}
}

func TestParsePruneListing(t *testing.T) {
	key := "0123456789abcdef"
	output := strings.Join([]string{
		"entry " + key + " free",
		"12 /models/" + key + "/model.safetensors",
		"7 /models/" + key + "/sub dir/with space.json",
		"5 /models/" + key + "/../escape",
		"5 /models/fedcba9876543210/elsewhere",
		"garbage line",
		"entry fedcba9876543210 locked",
		"3 /models/fedcba9876543210/a",
		"",
	}, "\n")
	got := parsePruneListing(output)
	e := got[key]
	if e == nil || e.State != "free" {
		t.Fatalf("entry %s = %+v, want state free", key, e)
	}
	if want := []string{"model.safetensors", "sub dir/with space.json"}; !slices.Equal(rels(e.Files), want) {
		t.Fatalf("files = %v, want %v (paths outside the entry dropped)", rels(e.Files), want)
	}
	if e.Files[0].Size != 12 {
		t.Fatalf("size = %d, want 12", e.Files[0].Size)
	}
	if other := got["fedcba9876543210"]; other == nil || other.State != "locked" || len(other.Files) != 1 {
		t.Fatalf("second entry = %+v, want locked with one file", other)
	}
}
