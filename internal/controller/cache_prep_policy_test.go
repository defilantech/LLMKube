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
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The numeric uid is a fact about the image, not a free parameter: the
// downloader that writes the serving cache and the one that writes the
// prefetch cache must be the same user, or a later serving pod cannot write
// into a directory the prefetch created. `docker run --rm
// curlimages/curl:8.18.0 id` prints uid=101.
func TestInitContainerUIDMatchesDefaultImage(t *testing.T) {
	if DefaultInitContainerUser != 101 {
		t.Fatalf("DefaultInitContainerUser = %d, want 101 (curl_user in curlimages/curl:8.18.0)", DefaultInitContainerUser)
	}
}

func TestValidateModelCachePrep(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantErr bool
	}{
		{ModelCachePrepAlways, false},
		{ModelCachePrepNever, false},
		// `auto` is the documented follow-up, not an accepted value yet: it
		// must fail startup rather than silently behave like either.
		{"auto", true},
		{"", true},
		{"Never", true},
	} {
		err := ValidateModelCachePrep(tc.in)
		if tc.wantErr && err == nil {
			t.Errorf("ValidateModelCachePrep(%q) = nil, want an error", tc.in)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("ValidateModelCachePrep(%q) = %v, want nil", tc.in, err)
		}
	}
}

// The reported acceptance criterion: under never, a cache-backed pod has no
// model-cache-prep container and no container running as uid 0.
func TestCachedStorageConfigNever(t *testing.T) {
	cfg := buildCachedStorageConfig(cachePrepTripwireModel(), cachePrepTripwireIsvc(), resolveEffectiveModelCache(cachePrepTripwireIsvc(), cachePrepTripwireModel()), "", "", "docker.io/curlimages/curl:8.18.0", 102, ModelCachePrepNever, DefaultInitContainerUser, "")
	if got, want := containerNames(cfg.initContainers), []string{"model-downloader"}; !slices.Equal(got, want) {
		t.Fatalf("initContainers = %v, want %v", got, want)
	}
	assertNoRootContainer(t, cfg.initContainers)
}

// The multi-file branch builds its container list separately; it must honor
// the setting too.
func TestCachedStorageConfigMultiFileNever(t *testing.T) {
	model := cachePrepTripwireModel()
	model.Spec.Files = []string{"model.gguf", "mmproj.gguf"}
	cfg := buildCachedStorageConfig(model, cachePrepTripwireIsvc(), resolveEffectiveModelCache(cachePrepTripwireIsvc(), model), "", "", "docker.io/curlimages/curl:8.18.0", 102, ModelCachePrepNever, DefaultInitContainerUser, "")
	if slices.Contains(containerNames(cfg.initContainers), "model-cache-prep") {
		t.Fatalf("multi-file initContainers = %v, want no model-cache-prep", containerNames(cfg.initContainers))
	}
	if !slices.Contains(containerNames(cfg.initContainers), "model-downloader") {
		t.Fatalf("multi-file initContainers = %v, want a model-downloader", containerNames(cfg.initContainers))
	}
	assertNoRootContainer(t, cfg.initContainers)
}

func TestCachedStorageConfigAlwaysKeepsPrep(t *testing.T) {
	cfg := buildCachedStorageConfig(cachePrepTripwireModel(), cachePrepTripwireIsvc(), resolveEffectiveModelCache(cachePrepTripwireIsvc(), cachePrepTripwireModel()), "", "", "docker.io/curlimages/curl:8.18.0", 102, ModelCachePrepAlways, DefaultInitContainerUser, "")
	if got, want := containerNames(cfg.initContainers), []string{"model-cache-prep", "model-downloader"}; !slices.Equal(got, want) {
		t.Fatalf("initContainers = %v, want %v", got, want)
	}
}

func assertNoRootContainer(t *testing.T, cs []corev1.Container) {
	t.Helper()
	for i := range cs {
		sc := cs[i].SecurityContext
		if sc != nil && sc.RunAsUser != nil && *sc.RunAsUser == 0 {
			t.Errorf("container %s runs as uid 0", cs[i].Name)
		}
	}
}

// A draft model rides in the target's pod under the same operator-wide
// setting: there is no per-workload control, so a cached draft must not keep
// the root container the target dropped.
func TestDraftCachePrepFollowsPolicy(t *testing.T) {
	draft := cachePrepTripwireModel()
	draft.Name = "tripwire-draft"
	draft.Status.CacheKey = "tripwire-draft-key"
	draft.Status.Path = "/models/tripwire-draft-key/tripwire.gguf"

	isvc := cachePrepTripwireIsvc()
	isvc.Spec.SpeculativeDecoding = &inferencev1alpha1.SpeculativeDecodingSpec{
		Type: "draft-dspark", DraftModelRef: draft.Name,
	}

	r := cachePrepTripwireISvcReconciler()
	r.ModelCachePrep = ModelCachePrepNever
	dep := r.deploymentFor(isvc, cachePrepTripwireModel(), resolveEffectiveModelCache(isvc, cachePrepTripwireModel()), draft, 1, "", "")

	names := containerNames(dep.Spec.Template.Spec.InitContainers)
	if slices.Contains(names, "model-cache-prep") || slices.Contains(names, "draft-model-cache-prep") {
		t.Fatalf("initContainers = %v, want neither target nor draft prep under never", names)
	}
	if !slices.Contains(names, "model-downloader") || !slices.Contains(names, "draft-model-downloader") {
		t.Fatalf("initContainers = %v, want both downloaders", names)
	}
	assertNoRootContainer(t, dep.Spec.Template.Spec.InitContainers)
}

// A draft with no cache of its own mounts an emptyDir and never had a prep;
// under never the target still must not keep one.
func TestDraftUncachedKeepsNoPrep(t *testing.T) {
	draft := cachePrepTripwireModel()
	draft.Name = "tripwire-uncached-draft"
	draft.Status.CacheKey = ""
	draft.Status.Path = ""

	isvc := cachePrepTripwireIsvc()
	isvc.Spec.SpeculativeDecoding = &inferencev1alpha1.SpeculativeDecodingSpec{
		Type: "draft-dspark", DraftModelRef: draft.Name,
	}
	r := cachePrepTripwireISvcReconciler()
	r.ModelCachePrep = ModelCachePrepNever
	names := containerNames(r.deploymentFor(isvc, cachePrepTripwireModel(), resolveEffectiveModelCache(isvc, cachePrepTripwireModel()), draft, 1, "", "").Spec.Template.Spec.InitContainers)
	if slices.Contains(names, "model-cache-prep") || slices.Contains(names, "draft-model-cache-prep") {
		t.Fatalf("initContainers = %v, want no prep", names)
	}
	if !slices.Contains(names, "draft-model-downloader") {
		t.Fatalf("initContainers = %v, want a draft downloader", names)
	}
}

// Under never the prefetch Job must be Pod Security restricted compliant: no
// root container, pod-level seccomp, and every container carrying a numeric
// non-root uid. A named user + runAsNonRoot is refused by the kubelet, which
// envtest cannot see, so the uid is pinned here.
func TestPrefetchJobRestrictedUnderNever(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fsGrp int64
	}{
		{"default-fsgroup", 102},
		{"no-fsgroup", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := cachePrepTripwireModelReconciler()
			mr.ModelCachePrep = ModelCachePrepNever
			mr.DefaultFSGroup = tc.fsGrp
			m := cachePrepTripwireModel()
			seedPrefetchCacheKey(m)

			job, err := mr.buildPrefetchJob(t.Context(), m, "", "")
			if err != nil {
				t.Fatalf("buildPrefetchJob: %v", err)
			}
			pod := job.Spec.Template.Spec

			if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil ||
				pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Fatalf("pod securityContext = %+v, want seccompProfile RuntimeDefault", pod.SecurityContext)
			}

			all := make([]corev1.Container, 0, len(pod.InitContainers)+len(pod.Containers))
			all = append(all, pod.InitContainers...)
			all = append(all, pod.Containers...)
			if len(all) == 0 {
				t.Fatal("job pod has no containers")
			}
			for i := range all {
				c := &all[i]
				if c.Name == "model-cache-prep" {
					t.Errorf("container %s present under never", c.Name)
				}
				sc := c.SecurityContext
				if sc == nil {
					t.Errorf("container %s has no securityContext", c.Name)
					continue
				}
				if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
					t.Errorf("container %s runAsNonRoot = %v, want true", c.Name, sc.RunAsNonRoot)
				}
				if sc.RunAsUser == nil || *sc.RunAsUser != DefaultInitContainerUser {
					t.Errorf("container %s runAsUser = %v, want %d", c.Name, sc.RunAsUser, DefaultInitContainerUser)
				}
				if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
					t.Errorf("container %s allowPrivilegeEscalation = %v, want false", c.Name, sc.AllowPrivilegeEscalation)
				}
				if sc.Capabilities == nil || !slices.Contains(sc.Capabilities.Drop, corev1.Capability("ALL")) {
					t.Errorf("container %s does not drop ALL", c.Name)
				}
				if sc.Capabilities != nil && len(sc.Capabilities.Add) > 0 {
					t.Errorf("container %s adds capabilities %v", c.Name, sc.Capabilities.Add)
				}
			}
		})
	}
}

// The default stays exactly as base: only fsGroup and its change policy on
// the pod, a bare completion container, the prep first.
func TestPrefetchJobAlwaysUnchanged(t *testing.T) {
	mr := cachePrepTripwireModelReconciler()
	m := cachePrepTripwireModel()
	seedPrefetchCacheKey(m)
	job, err := mr.buildPrefetchJob(t.Context(), m, "", "")
	if err != nil {
		t.Fatalf("buildPrefetchJob: %v", err)
	}
	pod := job.Spec.Template.Spec
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 102 {
		t.Fatalf("pod securityContext = %+v, want fsGroup 102", pod.SecurityContext)
	}
	if pod.SecurityContext.FSGroupChangePolicy == nil || *pod.SecurityContext.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("fsGroupChangePolicy = %v, want OnRootMismatch", pod.SecurityContext.FSGroupChangePolicy)
	}
	if pod.SecurityContext.SeccompProfile != nil || pod.SecurityContext.RunAsNonRoot != nil || pod.SecurityContext.RunAsUser != nil {
		t.Errorf("pod securityContext gained fields under always: %+v", pod.SecurityContext)
	}
	if got, want := containerNames(pod.InitContainers), []string{"model-cache-prep", "model-downloader"}; !slices.Equal(got, want) {
		t.Errorf("initContainers = %v, want %v", got, want)
	}
	if len(pod.Containers) != 1 || pod.Containers[0].SecurityContext != nil {
		t.Errorf("prefetch-done securityContext = %+v, want nil under always", pod.Containers[0].SecurityContext)
	}
}

// What the pinned prefetch uid buys: for the default image, and for the pod
// context the docs prescribe, the prefetch downloader and the serving
// downloader are the same user. A serving pod that has to write into a
// directory the prefetch created (mode 0755, no group write) would get
// Permission denied if they diverged. A custom --init-container-image still
// leaves the serving side to the image while the prefetch stages as
// --init-container-user, so the two agree only when that flag names the
// image's uid; that is documented in docs/MODEL-CACHE.md.
func TestPrefetchDownloaderMatchesServingDownloaderUID(t *testing.T) {
	mr := cachePrepTripwireModelReconciler()
	mr.ModelCachePrep = ModelCachePrepNever
	m := cachePrepTripwireModel()
	seedPrefetchCacheKey(m)
	job, err := mr.buildPrefetchJob(t.Context(), m, "", "")
	if err != nil {
		t.Fatalf("buildPrefetchJob: %v", err)
	}
	var prefetchUID *int64
	for i := range job.Spec.Template.Spec.InitContainers {
		if job.Spec.Template.Spec.InitContainers[i].Name == "model-downloader" {
			prefetchUID = job.Spec.Template.Spec.InitContainers[i].SecurityContext.RunAsUser
		}
	}
	if prefetchUID == nil {
		t.Fatal("prefetch downloader has no runAsUser")
	}

	// The serving path leaves the downloader's uid to the image, or to the
	// pod context's runAsUser when the user set one.
	for _, tc := range []struct {
		name string
		isvc *inferencev1alpha1.InferenceService
		want *int64
	}{
		{"default image", cachePrepTripwireIsvc(), prefetchUID},
		{"pod context overrides the uid", cacheBackedIsvcWithRestrictedPodContext(), prefetchUID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := cachePrepTripwireISvcReconciler()
			sc.ModelCachePrep = ModelCachePrepNever
			dep := sc.deploymentFor(tc.isvc, m, resolveEffectiveModelCache(tc.isvc, m), nil, 1, "", "")
			var servingUID *int64
			for i := range dep.Spec.Template.Spec.InitContainers {
				if dep.Spec.Template.Spec.InitContainers[i].Name == "model-downloader" {
					servingUID = dep.Spec.Template.Spec.InitContainers[i].SecurityContext.RunAsUser
				}
			}
			if servingUID != nil {
				if *servingUID != *tc.want {
					t.Fatalf("serving downloader uid = %d, prefetch uid = %d: a serving pod could not write into what the prefetch created", *servingUID, *tc.want)
				}
				return
			}
			if *tc.want != DefaultInitContainerUser {
				t.Fatalf("serving downloader uid unset (image default), prefetch uid = %d, want the image default %d", *tc.want, DefaultInitContainerUser)
			}
		})
	}
}
