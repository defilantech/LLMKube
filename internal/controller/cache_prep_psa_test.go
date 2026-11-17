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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Ground truth for the pod security posture: the API server's own Pod Security
// admission, not our reading of the standard. Every case below runs the pod
// template the production builders emit through a real admission check in a
// namespace that enforces `restricted`, so a change that only looks compliant
// (a field on the wrong level, a capability nobody needed) is rejected here.

// restrictedPodSecurityVersion pins the enforced policy version instead of
// `latest`, so a future envtest bump cannot silently change what the cases
// assert. 1.37 is the assets version dev and CI run.
const restrictedPodSecurityVersion = "v1.37"

// psaNamespace creates a namespace labeled to enforce Pod Security restricted
// and makes it the current test namespace, so a fixture that names testNS
// lands there.
func psaNamespace(t *testing.T, c client.Client) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "llmkube-psa-restricted-",
		Labels: map[string]string{
			"pod-security.kubernetes.io/enforce":         "restricted",
			"pod-security.kubernetes.io/enforce-version": restrictedPodSecurityVersion,
		},
	}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("create restricted namespace: %v", err)
	}
	testNS = ns.Name
}

// psaPriorityClass creates the chart's llmkube-normal PriorityClass once per
// shared control plane: a directly created pod carrying an unknown
// priorityClassName is rejected, so a missing class would make every "admitted"
// case in this file pass for the wrong reason.
func psaPriorityClass(t *testing.T, c client.Client) {
	t.Helper()
	pc := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "llmkube-normal"}, Value: 1000}
	if err := c.Create(context.Background(), pc); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create llmkube-normal PriorityClass: %v", err)
	}
}

// submitPodTemplate creates a Pod from a generated pod template in the current
// namespace and returns the admission error, with the caller's template
// metadata stripped the way a Deployment or Job controller would strip it.
func submitPodTemplate(c client.Client, tmpl corev1.PodTemplateSpec) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "psa-probe-",
			Namespace:    testNS,
			Labels:       tmpl.Labels,
		},
		Spec: tmpl.Spec,
	}
	return c.Create(context.Background(), pod)
}

// cacheBackedServingTemplate is the pod template of a cache-backed
// InferenceService Deployment, built through the production path.
func cacheBackedServingTemplate(r *InferenceServiceReconciler, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) corev1.PodTemplateSpec {
	return r.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template
}

// cacheBackedIsvcWithRestrictedPodContext is the user configuration
// docs/MODEL-CACHE.md tells a `restricted` namespace to supply, verbatim: a
// numeric non-root uid the downloader inherits, the fsGroup a supplied context
// has to restate, and a runtime-default seccomp profile. The serving
// container's context is left to the operator default, as the docs assume.
func cacheBackedIsvcWithRestrictedPodContext() *inferencev1alpha1.InferenceService {
	uid, gid := int64(101), int64(102)
	isvc := cachePrepTripwireIsvc()
	isvc.Spec.PodSecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot:        boolPtr(true),
		RunAsUser:           &uid,
		FSGroup:             &gid,
		FSGroupChangePolicy: fsGroupChangeOnRootMismatch(),
		SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	return isvc
}

// The reported symptom: on the default configuration no cache-backed pod is
// admitted in a namespace that enforces `restricted`, because the prep runs as
// uid 0 with CHOWN added.
func TestRestrictedNamespaceRejectsDefaultCachePod(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	psaPriorityClass(t, c)
	psaNamespace(t, c)

	err := submitPodTemplate(c, cacheBackedServingTemplate(cachePrepTripwireISvcReconciler(), cachePrepTripwireIsvc(), cachePrepTripwireModel()))
	if err == nil {
		t.Fatalf("a cache-backed pod with the default policy was admitted into a restricted namespace; the root model-cache-prep container is supposed to be rejected here")
	}
	assertPodSecurityForbidden(t, err, "model-cache-prep")
}

// The user-facing goal: the same pod, admitted, with the prep turned off and
// the pod security context the docs prescribe.
func TestCacheBackedPodAdmittedInRestrictedWithPrepHidden(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	psaPriorityClass(t, c)
	psaNamespace(t, c)

	r := cachePrepTripwireISvcReconciler()
	r.ModelCachePrep = ModelCachePrepNever
	err := submitPodTemplate(c, cacheBackedServingTemplate(r, cacheBackedIsvcWithRestrictedPodContext(), cachePrepTripwireModel()))
	if err != nil {
		t.Fatalf("a cache-backed pod with prep: never was rejected in a restricted namespace: %v", err)
	}
}

// The prefetch Job is the other cache-backed pod the operator creates. Under
// always its prep is rejected (via the serving case above, which shares the
// container); under never the Job's own security context must be enough.
func TestPrefetchPodAdmittedInRestrictedWithPrepHidden(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	psaPriorityClass(t, c)
	psaNamespace(t, c)

	mr := cachePrepTripwireModelReconciler()
	mr.ModelCachePrep = ModelCachePrepNever
	m := cachePrepTripwireModel()
	seedPrefetchCacheKey(m)
	job, err := mr.buildPrefetchJob(t.Context(), m, "", "")
	if err != nil {
		t.Fatalf("buildPrefetchJob: %v", err)
	}
	if err := submitPodTemplate(c, job.Spec.Template); err != nil {
		t.Fatalf("the prefetch Job pod with prep: never was rejected in a restricted namespace: %v", err)
	}
}

// And the control for the Job itself: with the prep it is rejected, so the
// admitted case above is not passing because the Job was never checked.
func TestRestrictedNamespaceRejectsPrefetchJobWithPrep(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	psaPriorityClass(t, c)
	psaNamespace(t, c)

	mr := cachePrepTripwireModelReconciler()
	m := cachePrepTripwireModel()
	seedPrefetchCacheKey(m)
	job, err := mr.buildPrefetchJob(t.Context(), m, "", "")
	if err != nil {
		t.Fatalf("buildPrefetchJob: %v", err)
	}
	assertPodSecurityForbidden(t, submitPodTemplate(c, job.Spec.Template), "model-cache-prep")
}

// assertPodSecurityForbidden fails unless err is a Pod Security rejection that
// names want/delegates past a container. It also proves the namespace is
// actually enforcing: a plain RBAC or validation error would not mention
// PodSecurity.
func assertPodSecurityForbidden(t *testing.T, err error, container string) {
	t.Helper()
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected a Forbidden (Pod Security) error, got: %v", err)
	}
	msg := err.Error()
	if !containsFold(msg, "PodSecurity") {
		t.Fatalf("the rejection is not a Pod Security violation: %v", err)
	}
	if !containsFold(msg, container) {
		t.Fatalf("the rejection does not name container %q: %s", container, msg)
	}
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(asciiLower(haystack), asciiLower(needle))
}
