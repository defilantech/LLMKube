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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Model-transfer diagnosis. The sibling of the CUDA driver diagnosis, for the
// containers that actually fail on a fresh deploy: the init containers that
// fetch the weights.
//
// Every signature here is a failure mode seen in practice, not a guess. The
// TLS one in particular: an object store behind a private CA returns exactly
// this when the operator's CA bundle is not mounted, and the symptom (a pod
// stuck in Init with a non-zero exit) names nothing.

func TestMatchModelTransferFailure_Signatures(t *testing.T) {
	cases := []struct {
		name       string
		msg        string
		wantReason string
	}{
		{
			name: "spec.sha256 does not match the downloaded bytes",
			msg: "ERROR: SHA256 mismatch: expected d9ba44419f2a73ed1a666885066c65a235ab70f337e2b31cbb3d062a5f5b8d4b, " +
				"computed 0000000000000000000000000000000000000000000000000000000000000000; " +
				"discarded /models/k/model.gguf.a1b2c3d4e5f6.tmp; " +
				"recorded /models/k/model.gguf.d9ba44419f2a73ed1a666885066c65a235ab70f337e2b31cbb3d062a5f5b8d4b.sha256-rejected",
			wantReason: inferencev1alpha1.ReasonModelDigestMismatch,
		},
		{
			name: "spec.fileSha256 does not match a staged shard",
			msg: "ERROR: spec.fileSha256 mismatch for a.gguf; the origin served bytes that do not match the pinned digest; " +
				"fix the Model or the upload, then delete /models/k/a.gguf.d9ba44419f2a73ed1a666885066c65a235ab70f337e2b31cbb3d062a5f5b8d4b.sha256-rejected to retry",
			wantReason: ReasonModelFileDigestMismatch,
		},
		{
			name: "spec.fileSha256 publish mismatch on a fresh download",
			msg: "ERROR: spec.fileSha256 mismatch: expected d9ba44419f2a73ed1a666885066c65a235ab70f337e2b31cbb3d062a5f5b8d4b, " +
				"computed 0000000000000000000000000000000000000000000000000000000000000000",
			wantReason: ReasonModelFileDigestMismatch,
		},
		{
			// A stamp-hit success line for one pinned shard names spec.fileSha256 too;
			// it must not turn a later shard's 404 into a digest mismatch.
			name: "verified shard followed by a 404 on another shard",
			msg: "Model verified against spec.fileSha256 (stamp hit, skipped re-hashing)\n" +
				"curl: (22) The requested URL returned error: 404",
			wantReason: ReasonModelSourceNotFound,
		},
		{
			// The marker-keep line is informational (the cached copy is kept); a later
			// shard's 404 is the real failure.
			name: "kept pinned shard followed by a 404 on another shard",
			msg: "the origin served bytes for a.gguf that do not match the pinned spec.fileSha256; kept the pinned cached copy; " +
				"fix the upload, then delete /models/k/a.gguf.d9ba44419f2a73ed1a666885066c65a235ab70f337e2b31cbb3d062a5f5b8d4b.sha256-rejected to retry\n" +
				"curl: (22) The requested URL returned error: 404",
			wantReason: ReasonModelSourceNotFound,
		},
		{
			name: "pod built before the Model recorded its glob expansion",
			msg: "ERROR: StagedFilesNotCurrent - Model qwen has no spec.files glob expansion recorded for its current generation. " +
				"The controller expands globs from the source repository listing; run kubectl describe model qwen to see the cause.",
			wantReason: ReasonStagedFilesNotCurrent,
		},
		{
			name:       "pre-staged pvc artifact fails its digest",
			msg:        "ERROR: SHA256 mismatch for the pre-staged /model-source/model.gguf: expected d9ba44419f2a73ed1a666885066c65a235ab70f337e2b31cbb3d062a5f5b8d4b, computed 0000000000000000000000000000000000000000000000000000000000000000",
			wantReason: ReasonModelPVCDigestMismatch,
		},
		{
			name:       "curl 403 from a signed request with bad credentials",
			msg:        "curl: (22) The requested URL returned error: 403",
			wantReason: ReasonModelSourceUnauthorized,
		},
		{
			name:       "curl 401",
			msg:        "curl: (22) The requested URL returned error: 401 Unauthorized",
			wantReason: ReasonModelSourceUnauthorized,
		},
		{
			name:       "curl 404 for a wrong key or path",
			msg:        "curl: (22) The requested URL returned error: 404",
			wantReason: ReasonModelSourceNotFound,
		},
		{
			// Captured verbatim from curl 8.18 (the pinned init-container image)
			// against a private endpoint with no CA bundle mounted. Note the
			// wording is NOT "SSL certificate problem:", which is what older
			// curl prints; both spellings are carried in the signature list
			// because the init-container image is pinned but overridable.
			name:       "private CA not trusted, curl 8.x wording",
			msg:        "curl: (60) SSL certificate OpenSSL verify result: unable to get local issuer certificate (20)\nMore details here: https://curl.se/docs/sslcerts.html",
			wantReason: ReasonModelSourceUntrusted,
		},
		{
			name:       "private CA not trusted, older curl wording",
			msg:        "curl: (60) SSL certificate problem: unable to get local issuer certificate",
			wantReason: ReasonModelSourceUntrusted,
		},
		{
			name:       "endpoint DNS does not resolve",
			msg:        "curl: (6) Could not resolve host: nope.invalid.internal",
			wantReason: ReasonModelSourceUnreachable,
		},
		{
			name:       "endpoint refuses the connection",
			msg:        "curl: (7) Failed to connect to 10.0.0.5 port 9000: Connection refused",
			wantReason: ReasonModelSourceUnreachable,
		},
		{
			name:       "node disk or emptyDir sizeLimit exhausted",
			msg:        "sh: write error: No space left on device",
			wantReason: ReasonModelTransferOutOfSpace,
		},
		{
			// Captured from curl 8.18.0 with a /models root the downloader
			// cannot write: the mkdir of the cache key dir fails first.
			name:       "cache root the downloader cannot write",
			msg:        "mkdir: can't create directory '/models/tripwire-key': Permission denied",
			wantReason: ReasonModelCacheNotWritable,
		},
		{
			// A prefetched pod that cannot enter the cache root its prefetch
			// Job handed to another group: not a missing file (#2450).
			name: "a prefetched pod that cannot read the cache",
			msg: "ERROR: PrefetchedFilesUnreadable - uid 2000 (groups: 2000) cannot read /models/k/a.safetensors on the cache claim; " +
				"blocked at: drwxrwx--- 3 0 102 4096 Nov  5 17:38 /models",
			wantReason: ReasonPrefetchedFilesUnreadable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := matchModelTransferFailure(tc.msg)
			if !ok {
				t.Fatalf("no diagnosis for %q", tc.msg)
			}
			if f.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", f.reason, tc.wantReason)
			}
			if f.line == "" {
				t.Error("matched line is empty; the condition message would name no evidence")
			}
		})
	}
}

// Anything unrecognised must produce no diagnosis at all. Guessing a reason
// would put a confident, wrong label on a failure, which is worse than the
// generic non-zero exit the user already sees.
func TestMatchModelTransferFailure_UnknownIsNotDiagnosed(t *testing.T) {
	for _, msg := range []string{
		"",
		"Model downloaded successfully",
		"curl: (18) transfer closed with outstanding read data remaining",
		// The generic file-set message must stay unlabelled: it also covers a
		// genuinely invalid spec.files, which the Model's own condition names.
		`ERROR: InvalidFileSet - model spec.files/spec.mmproj configuration is invalid. Check file paths, directory escapes, and glob patterns.`,
	} {
		if f, ok := matchModelTransferFailure(msg); ok {
			t.Errorf("msg %q was diagnosed as %q; unknown failures must not be labelled", msg, f.reason)
		}
	}
}

func initPod(node string, statuses ...corev1.ContainerStatus) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{InitContainerStatuses: statuses},
	}
}

func terminatedInit(name string, exit int32, msg string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: name,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: msg},
		},
	}
}

func TestFindModelTransferFailure(t *testing.T) {
	t.Run("diagnoses a failed downloader", func(t *testing.T) {
		pods := []corev1.Pod{initPod("node-a",
			terminatedInit("model-downloader", 22,
				"curl: (60) SSL certificate problem: unable to get local issuer certificate"))}
		f, found := findModelTransferFailure(pods)
		if !found {
			t.Fatal("no failure found")
		}
		if f.node != "node-a" || f.reason != ReasonModelSourceUntrusted || f.line == "" {
			t.Errorf("got node=%q reason=%q line=%q", f.node, f.reason, f.line)
		}
	})

	t.Run("cache permission failure on a cache-backed pod names the setting", func(t *testing.T) {
		pod := initPod("node-a",
			terminatedInit("model-downloader", 1, "mkdir: can't create directory '/models/k': Permission denied"))
		pod.Spec.Volumes = []corev1.Volume{{Name: "model-cache", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}}}}
		f, found := findModelTransferFailure([]corev1.Pod{pod})
		if !found || f.reason != ReasonModelCacheNotWritable {
			t.Fatalf("found=%v reason=%q, want %q", found, f.reason, ReasonModelCacheNotWritable)
		}
	})

	// The remedy lands in the condition the user reads. On the fsGroup <= 0
	// profile the prep is already on, so a hint that names only
	// --model-cache-prep sends them to a knob already in the right position;
	// it must also name the uid the prep chowns to (#2204).
	t.Run("cache permission failure condition names both the prep policy and the init user", func(t *testing.T) {
		scheme := runtime.NewScheme()
		if err := corev1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"}}
		pod := initPod("node-a",
			terminatedInit("model-downloader", 1, "mkdir: can't create directory '/models/k': Permission denied"))
		pod.Name, pod.Namespace = "pod", "default"
		pod.Labels = map[string]string{"app": isvc.Name, "inference.llmkube.dev/service": isvc.Name}
		pod.Spec.Volumes = []corev1.Volume{{Name: "model-cache", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}}}}
		r := &InferenceServiceReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(&pod).Build()}
		r.reconcileModelTransferCondition(t.Context(), isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionModelTransferHealthy)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonModelCacheNotWritable {
			t.Fatalf("condition = %+v, want False/%s", cond, ReasonModelCacheNotWritable)
		}
		for _, want := range []string{"--model-cache-prep=never", "--default-fsgroup=0", "--init-container-user", "controllerManager.initContainer.user"} {
			if !strings.Contains(cond.Message, want) {
				t.Errorf("ModelCacheNotWritable message does not name %q: %s", want, cond.Message)
			}
		}
	})

	t.Run("cache permission failure on an emptyDir pod is not labelled with the cache remedy", func(t *testing.T) {
		pod := initPod("node-a",
			terminatedInit("model-downloader", 1, "mkdir: can't create directory '/models/k': Permission denied"))
		pod.Spec.Volumes = []corev1.Volume{{Name: "model-storage", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		if f, found := findModelTransferFailure([]corev1.Pod{pod}); found {
			t.Errorf("emptyDir permission failure diagnosed as %q; the cache remedy does not apply", f.reason)
		}
	})

	// The prefetched serving pod mounts the same claim read-only and runs
	// model-verify, which never writes to it: a permission denied there is
	// not a cache-ownership problem. Built through the production builder so
	// the container name is the real one.
	t.Run("cache permission failure on the read-only prefetched pod is not labelled with the cache remedy", func(t *testing.T) {
		cfg := buildPrefetchedStorageConfig(cachePrepTripwireModel(), cachePrepTripwireIsvc(), resolveEffectiveModelCache(cachePrepTripwireIsvc(), cachePrepTripwireModel()), ModelCacheModeShared,
			"docker.io/curlimages/curl:8.18.0", nil)
		if len(cfg.initContainers) != 1 || cfg.initContainers[0].Name != "model-verify" {
			t.Fatalf("prefetched initContainers = %v, want [model-verify]", containerNames(cfg.initContainers))
		}
		pod := initPod("node-a",
			terminatedInit("model-verify", 1, "sha256sum: can't open '/models/k/m.gguf': Permission denied"))
		pod.Spec.Volumes = cfg.volumes
		if f, found := findModelTransferFailure([]corev1.Pod{pod}); found {
			t.Errorf("read-only prefetched pod diagnosed as %q; model-verify never writes the cache", f.reason)
		}
	})

	// The cache signature runs last, so a message that also names a specific
	// cause keeps that cause rather than the generic cache remedy. Matched
	// directly on the message: findModelTransferFailure reads the last
	// termination before the current one, so a pod-level fixture would test
	// precedence between attempts, not between signatures.
	t.Run("a specific cause wins over the cache remedy", func(t *testing.T) {
		f, ok := matchModelTransferFailure(
			"mkdir: can't create directory '/models/k': Permission denied\n" +
				"curl: (60) SSL certificate problem: unable to get local issuer certificate")
		if !ok || f.reason != ReasonModelSourceUntrusted {
			t.Fatalf("ok=%v reason=%q, want %q", ok, f.reason, ReasonModelSourceUntrusted)
		}
	})

	t.Run("ignores a clean exit", func(t *testing.T) {
		pods := []corev1.Pod{initPod("node-a",
			terminatedInit("model-downloader", 0, "Model downloaded successfully"))}
		if _, found := findModelTransferFailure(pods); found {
			t.Error("a zero exit code must never be diagnosed as a failure")
		}
	})

	// The engine's own crash is the CUDA diagnosis's job. A signature match in
	// the runtime container must not be claimed here, or one failure gets two
	// contradictory diagnoses.
	t.Run("ignores non-init containers", func(t *testing.T) {
		pods := []corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				terminatedInit("llama-server", 1, "curl: (22) The requested URL returned error: 404"),
			}},
		}}
		if _, found := findModelTransferFailure(pods); found {
			t.Error("runtime-container output must not be diagnosed as a transfer failure")
		}
	})

	// A pod that got past its init containers is evidence the transfer works
	// now; a stale message must not keep the service diagnosed forever.
	t.Run("ignores a pod whose init containers have since succeeded", func(t *testing.T) {
		cs := terminatedInit("model-downloader", 0, "")
		cs.Ready = true
		pods := []corev1.Pod{initPod("node-a", cs)}
		if _, found := findModelTransferFailure(pods); found {
			t.Error("a completed init container must not be reported as failing")
		}
	})

	// A retried download that has since succeeded keeps the failed attempt in
	// LastTerminationState for the life of the pod. The current state is exit 0,
	// so the stale failure must not pin the condition False forever (#1536).
	t.Run("ignores a recovered retry whose last termination still failed", func(t *testing.T) {
		cs := terminatedInit("model-downloader", 0, "")
		cs.LastTerminationState = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1,
				Message:  "curl: (7) Failed to connect to 10.0.0.5 port 9000: Connection refused",
			},
		}
		pods := []corev1.Pod{initPod("node-a", cs)}
		if _, found := findModelTransferFailure(pods); found {
			t.Error("a container whose current termination is success must not be diagnosed from its last termination")
		}
	})

	// The crash-loop path the last-termination scan exists for must keep
	// working: current state is NOT a successful termination, so the failed
	// last termination is still a live signal.
	t.Run("still diagnoses a genuine crash loop from last termination", func(t *testing.T) {
		cs := corev1.ContainerStatus{
			Name: "model-downloader",
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1,
					Message:  "curl: (7) Failed to connect to 10.0.0.5 port 9000: Connection refused",
				},
			},
		}
		pods := []corev1.Pod{initPod("node-b", cs)}
		f, found := findModelTransferFailure(pods)
		if !found {
			t.Fatal("a crash-looping container must still be diagnosed from its last termination")
		}
		if f.node != "node-b" || f.reason != ReasonModelSourceUnreachable || f.line == "" {
			t.Errorf("got node=%q reason=%q line=%q", f.node, f.reason, f.line)
		}
	})
}

// The ModelCacheLockUnsupported remedy comes from the downloader's own message,
// so a log tail that lost it gets a pointer back to the log rather than a
// remedy for some other source kind (#2251).
func TestMatchModelTransferFailure_CacheLockRemedyFromMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		want string
	}{
		{"prefetchable source", "ERROR: ModelCacheLockUnsupported: FUSE cache cannot guarantee cross-node file locking for staging a.safetensors; " + cacheLockRemedyPrefetch, cacheLockRemedyPrefetch},
		{"s3 source", "ERROR: ModelCacheLockUnsupported: FUSE cache cannot guarantee cross-node file locking for staging a.safetensors; " + cacheLockRemedyS3, cacheLockRemedyS3},
		{"remedy cut from the log tail", "ERROR: ModelCacheLockUnsupported: FUSE cache cannot guarantee", cacheLockRemedyFallback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := matchModelTransferFailure(tc.msg)
			if !ok || f.reason != ReasonModelCacheLockUnsupported {
				t.Fatalf("ok=%v reason=%q, want %q", ok, f.reason, ReasonModelCacheLockUnsupported)
			}
			if f.remedy != tc.want {
				t.Errorf("remedy = %q, want %q", f.remedy, tc.want)
			}
		})
	}
}
