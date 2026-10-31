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
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestDeletePrefetchJobPropagation: a FUSE stager Job is deleted in the
// foreground, so its replacement cannot start while the old writer's pods
// still run, even when the API server dropped spec.podReplacementPolicy
// because the JobPodReplacementPolicy feature gate is off (#2252). Single-file
// prefetch Jobs are stagers too since #2254. A Job an older operator built
// without the stager switch keeps background deletion. The Jobs come from
// buildPrefetchJob, the producer the deletion path reads back.
func TestDeletePrefetchJobPropagation(t *testing.T) {
	stagerModel := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "weights", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/m", Format: "safetensors", Files: []string{"model.safetensors"}},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: "cache"},
	}
	singleFileModel := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "gguf", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/m.gguf", Format: "gguf"},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: "cache"},
	}
	ociModel := singleFileModel.DeepCopy()
	ociModel.Spec.PrefetchOCI = &inferencev1alpha1.OCIPrefetchSpec{Image: "registry.example.com/models/test:v1"}
	for _, tc := range []struct {
		name       string
		model      *inferencev1alpha1.Model
		dropPolicy bool
		// preStager strips the stager switch, as on a Job built before the
		// single-file builder had one.
		preStager bool
		want      metav1.DeletionPropagation
	}{
		{"OCI publisher", ociModel, false, false, metav1.DeletePropagationForeground},
		{"stager", stagerModel, false, false, metav1.DeletePropagationForeground},
		{"stager read back without podReplacementPolicy", stagerModel, true, false, metav1.DeletePropagationForeground},
		{"single-file stager", singleFileModel, false, false, metav1.DeletePropagationForeground},
		{"single-file Job built without the stager switch", singleFileModel, true, true, metav1.DeletePropagationBackground},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &ModelReconciler{InitContainerImage: defaultPrefetchImage, OCIPublisherImage: "operator:test"}
			var job *batchv1.Job
			var err error
			if tc.model.Spec.PrefetchOCI != nil {
				job, _, err = r.buildOCIPrefetchJob(context.Background(), tc.model, "fingerprint")
			} else {
				job, err = r.buildPrefetchJob(context.Background(), tc.model, "", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			isStager := job.Spec.PodReplacementPolicy != nil && *job.Spec.PodReplacementPolicy == batchv1.Failed
			// Every Job buildPrefetchJob makes is a stager now; preStager
			// turns one back into what an older operator built.
			if !isStager && tc.model.Spec.PrefetchOCI == nil {
				t.Fatal("buildPrefetchJob did not build a stager (podReplacementPolicy=Failed); the cases no longer build the Jobs they name")
			}
			if tc.dropPolicy {
				job.Spec.PodReplacementPolicy = nil
			}
			if tc.preStager {
				for i := range job.Spec.Template.Spec.InitContainers {
					c := &job.Spec.Template.Spec.InitContainers[i]
					c.Env = slices.DeleteFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == "LLMKUBE_PREFETCH_STAGER" })
				}
			}

			var got []metav1.DeletionPropagation
			c := fake.NewClientBuilder().WithObjects(job).
				WithInterceptorFuncs(interceptor.Funcs{
					Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						o := &client.DeleteOptions{}
						o.ApplyOptions(opts)
						if o.PropagationPolicy == nil {
							got = append(got, "")
						} else {
							got = append(got, *o.PropagationPolicy)
						}
						return cl.Delete(ctx, obj, opts...)
					},
				}).Build()

			if err := deletePrefetchJob(context.Background(), c, job); err != nil {
				t.Fatalf("deletePrefetchJob: %v", err)
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("Delete propagation = %v, want [%s]", got, tc.want)
			}
		})
	}
}
