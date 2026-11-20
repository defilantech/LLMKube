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
	"math"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// A uid that is neither the default image's (101) nor the old literal owner
// (100), so a test that passes cannot be passing by coincidence.
const customInitUser int64 = 1001

func prepCommand(t *testing.T, cs []corev1.Container, name string) string {
	t.Helper()
	for i := range cs {
		if cs[i].Name == name {
			if len(cs[i].Command) != 3 {
				t.Fatalf("%s command = %v, want sh -c <cmd>", name, cs[i].Command)
			}
			return cs[i].Command[2]
		}
	}
	t.Fatalf("no %s container in %v", name, containerNames(cs))
	return ""
}

// #2204: with fsGroup disabled the prep must hand the cache root to the uid
// the downloader runs as, which is the configured init image user unless the
// InferenceService's pod context names a runAsUser (initContainerSecurityContext
// copies it onto the downloader). Both the single-file and the multi-file
// branches render the prep, so both are covered.
func TestCachePrepOwnerIsDownloaderUID(t *testing.T) {
	withPodUser := func(uid int64) *inferencev1alpha1.InferenceService {
		isvc := cachePrepTripwireIsvc()
		isvc.Spec.PodSecurityContext = &corev1.PodSecurityContext{RunAsUser: &uid}
		return isvc
	}
	withExplicitFSGroup := func(gid int64) *inferencev1alpha1.InferenceService {
		isvc := cachePrepTripwireIsvc()
		isvc.Spec.PodSecurityContext = &corev1.PodSecurityContext{FSGroup: &gid}
		return isvc
	}
	for _, tc := range []struct {
		name           string
		isvc           *inferencev1alpha1.InferenceService
		defaultFSGroup int64
		initUser       int64
		want           string
	}{
		{"operator fsGroup disabled, configured init user", cachePrepTripwireIsvc(), 0, customInitUser,
			"chown 1001:0 /models && chmod 770 /models"},
		{"negative fsGroup is disabled too", cachePrepTripwireIsvc(), -1, customInitUser,
			"chown 1001:0 /models && chmod 770 /models"},
		{"default init user", cachePrepTripwireIsvc(), 0, DefaultInitContainerUser,
			"chown 101:0 /models && chmod 770 /models"},
		{"no InferenceService (prefetch target)", nil, 0, customInitUser,
			"chown 1001:0 /models && chmod 770 /models"},
		{"pod runAsUser wins over the init user", withPodUser(2000), 0, customInitUser,
			"chown 2000:0 /models && chmod 770 /models"},
		{"explicit fsGroup 0 on the service", withExplicitFSGroup(0), 102, customInitUser,
			"chown 1001:0 /models && chmod 770 /models"},
		// The fsGroup branch does not depend on the uid at all.
		{"fsGroup set: init user ignored", cachePrepTripwireIsvc(), 102, customInitUser,
			"chown 0:102 /models && chmod g+rwX /models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			single := buildCachedStorageConfig(cachePrepTripwireModel(), tc.isvc, resolveEffectiveModelCache(tc.isvc, cachePrepTripwireModel()), "", "",
				"docker.io/curlimages/curl:8.18.0", tc.defaultFSGroup, ModelCachePrepAlways, tc.initUser, "")
			if got := prepCommand(t, single.initContainers, "model-cache-prep"); got != tc.want {
				t.Errorf("single-file prep = %q, want %q", got, tc.want)
			}

			multiModel := cachePrepTripwireModel()
			multiModel.Spec.Files = []string{"model.gguf", "mmproj.gguf"}
			multi := buildCachedStorageConfig(multiModel, tc.isvc, resolveEffectiveModelCache(tc.isvc, multiModel), "", "",
				"docker.io/curlimages/curl:8.18.0", tc.defaultFSGroup, ModelCachePrepAlways, tc.initUser, "")
			if got := prepCommand(t, multi.initContainers, "model-cache-prep"); got != tc.want {
				t.Errorf("multi-file prep = %q, want %q", got, tc.want)
			}
		})
	}
}

// The reconciler field is what --init-container-user sets; the serving
// Deployment must carry it on the target's prep and the draft's (which the
// merge then drops as a duplicate), and an unset field must render the flag
// default rather than uid 0.
func TestServingPrepUsesReconcilerInitContainerUser(t *testing.T) {
	draft := cachePrepTripwireModel()
	draft.Name = "tripwire-draft"
	draft.Status.CacheKey = "tripwire-draft-key"
	draft.Status.Path = "/models/tripwire-draft-key/tripwire.gguf"
	isvc := cachePrepTripwireIsvc()
	isvc.Spec.SpeculativeDecoding = &inferencev1alpha1.SpeculativeDecodingSpec{
		Type: "draft-dspark", DraftModelRef: draft.Name,
	}

	for _, tc := range []struct {
		name     string
		initUser int64
		want     string
	}{
		{"configured", customInitUser, "chown 1001:0 /models && chmod 770 /models"},
		{"unset field renders the default", 0, "chown 101:0 /models && chmod 770 /models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := cachePrepTripwireISvcReconciler()
			r.DefaultFSGroup = 0
			r.InitContainerUser = tc.initUser
			inits := r.deploymentFor(isvc, cachePrepTripwireModel(), resolveEffectiveModelCache(isvc, cachePrepTripwireModel()), draft, 1, "", "").Spec.Template.Spec.InitContainers
			if got := prepCommand(t, inits, "model-cache-prep"); got != tc.want {
				t.Errorf("target prep = %q, want %q", got, tc.want)
			}
			// The draft's prep is dropped only when identical to the target's
			// (mergeStorageConfigs), so a draft prep chowning to another uid
			// would surface here as a second, conflicting chown of /models.
			if slices.Contains(containerNames(inits), "draft-model-cache-prep") {
				t.Errorf("draft prep %q rendered next to the target prep %q: the two chown the shared root to different owners",
					prepCommand(t, inits, "draft-model-cache-prep"), prepCommand(t, inits, "model-cache-prep"))
			}
		})
	}
}

// The prefetch Job stages into the same cache, so it must give the root the
// same owner the serving prep does, and under never its containers must run
// as that same uid (the serving downloader runs as the image's user).
func TestPrefetchJobUsesReconcilerInitContainerUser(t *testing.T) {
	t.Run("always: prep owner", func(t *testing.T) {
		mr := cachePrepTripwireModelReconciler()
		mr.DefaultFSGroup = 0
		mr.InitContainerUser = customInitUser
		m := cachePrepTripwireModel()
		seedPrefetchCacheKey(m)
		job, err := mr.buildPrefetchJob(t.Context(), m, "", "")
		if err != nil {
			t.Fatalf("buildPrefetchJob: %v", err)
		}
		want := "chown 1001:0 /models && chmod 770 /models"
		if got := prepCommand(t, job.Spec.Template.Spec.InitContainers, "model-cache-prep"); got != want {
			t.Errorf("prefetch prep = %q, want %q", got, want)
		}
	})

	t.Run("never: runAsUser", func(t *testing.T) {
		mr := cachePrepTripwireModelReconciler()
		mr.ModelCachePrep = ModelCachePrepNever
		mr.InitContainerUser = customInitUser
		m := cachePrepTripwireModel()
		seedPrefetchCacheKey(m)
		job, err := mr.buildPrefetchJob(t.Context(), m, "", "")
		if err != nil {
			t.Fatalf("buildPrefetchJob: %v", err)
		}
		pod := job.Spec.Template.Spec
		all := append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...)
		for i := range all {
			sc := all[i].SecurityContext
			if sc == nil || sc.RunAsUser == nil {
				t.Errorf("container %s has no runAsUser, want %d", all[i].Name, customInitUser)
				continue
			}
			if *sc.RunAsUser != customInitUser {
				t.Errorf("container %s runAsUser = %d, want %d", all[i].Name, *sc.RunAsUser, customInitUser)
			}
		}
	})
}

func TestValidateInitContainerUser(t *testing.T) {
	for _, tc := range []struct {
		uid     int64
		wantErr bool
	}{
		{-1, true},
		// Root is refused: under never it sits next to runAsNonRoot.
		{0, true},
		{1, false},
		{DefaultInitContainerUser, false},
		{1000680000, false}, // an OpenShift namespace-range uid
		{math.MaxInt32, false},
		{math.MaxInt32 + 1, true},
	} {
		err := ValidateInitContainerUser(tc.uid)
		if tc.wantErr && err == nil {
			t.Errorf("ValidateInitContainerUser(%d) = nil, want an error", tc.uid)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("ValidateInitContainerUser(%d) = %v, want nil", tc.uid, err)
		}
	}
}
