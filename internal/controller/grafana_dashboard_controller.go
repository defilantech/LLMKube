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
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	dashboardControllerName = "grafana-dashboard"

	// dashboardRuntimeAnnotation names the runtime a candidate dashboard reads.
	// The chart stamps it on each manifest it defers; a candidate without it is
	// unconditional and always published.
	dashboardRuntimeAnnotation = "dashboards.llmkube.dev/requires-runtime"

	// managedByLabel scopes deletion to what this operator published. A
	// hand-authored GrafanaDashboard is never a deletion candidate.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "llmkube"
)

// GrafanaDashboardReconciler publishes the runtime dashboards the chart
// deferred, keeping the published set equal to the runtimes actually serving.
//
// The chart cannot make this call: which runtimes exist is an InferenceService
// fact that does not exist at `helm install` time, and a dashboard for a
// runtime nobody serves renders blank, which reads as an idle cluster rather
// than an absent one (#1227). So the chart renders each conditional
// GrafanaDashboard into a source ConfigMap instead of applying it, and this
// reconciler applies and removes them as InferenceServices come and go.
//
// Deferring the whole manifest rather than a set of knobs keeps every CR field
// (instanceSelector, folder, datasources, ...) owned by values.yaml: this
// controller decides only whether a dashboard exists, never what is in it.
type GrafanaDashboardReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Source is the ConfigMap of candidate manifests, one YAML per key, set
	// from --grafana-dashboard-source=<namespace>/<name>. Zero value disables
	// the reconciler: charts that publish every dashboard directly, and any
	// non-Helm install, never create it.
	Source types.NamespacedName

	detectorOnce sync.Once
	detector     *crdDetector
}

// +kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanadashboards,verbs=get;list;watch;create;update;patch;delete

// Reconcile republishes the whole candidate set on every event rather than
// diffing the one InferenceService that changed. The published set is a
// function of which runtimes exist cluster-wide, so a single service's
// deletion can be the event that retires a dashboard still wanted by its
// siblings — there is no per-service answer to cache, and a full sweep is one
// List against a handful of candidates.
func (r *GrafanaDashboardReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName(dashboardControllerName)

	if r.Source.Name == "" {
		return ctrl.Result{}, nil
	}
	present, err := r.dashboardCRDPresent(log)
	if err != nil || !present {
		return ctrl.Result{}, err
	}

	candidates, err := r.candidates(ctx)
	if err != nil || len(candidates) == 0 {
		return ctrl.Result{}, err
	}

	serving, err := r.servingRuntimes(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, candidate := range candidates {
		runtimeName := candidate.GetAnnotations()[dashboardRuntimeAnnotation]
		if runtimeName == "" || serving[runtimeName] {
			if err := r.publish(ctx, candidate); err != nil {
				return ctrl.Result{}, err
			}
			continue
		}
		if err := r.retire(ctx, candidate, log); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// candidates parses the chart's source ConfigMap into the manifests it
// deferred. A missing ConfigMap means the chart published directly (or the
// release predates this feature), which is not an error.
func (r *GrafanaDashboardReconciler) candidates(ctx context.Context) ([]*unstructured.Unstructured, error) {
	source := &corev1.ConfigMap{}
	if err := r.Get(ctx, r.Source, source); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	// Map iteration order is random and each candidate is applied
	// independently, but sorting keeps logs and test expectations stable.
	keys := make([]string, 0, len(source.Data))
	for key := range source.Data {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	candidates := make([]*unstructured.Unstructured, 0, len(keys))
	for _, key := range keys {
		manifest := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(source.Data[key]), &manifest.Object); err != nil {
			// A corrupt key is the chart's bug, not a transient error;
			// requeueing forever would not fix it. Skip it loudly instead so
			// the other dashboards still reconcile.
			logf.FromContext(ctx).Error(err, "skipping unparsable dashboard candidate",
				"configMap", r.Source.String(), "key", key)
			continue
		}
		if manifest.GetName() == "" {
			continue
		}
		candidates = append(candidates, manifest)
	}
	return candidates, nil
}

// servingRuntimes reports which runtimes have an InferenceService, across
// every namespace: the dashboards are fleet-wide, and their queries already
// scope by namespace through a Grafana variable.
func (r *GrafanaDashboardReconciler) servingRuntimes(ctx context.Context) (map[string]bool, error) {
	list := &inferencev1alpha1.InferenceServiceList{}
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	serving := map[string]bool{}
	for i := range list.Items {
		// Stopped and Suspended tear the workload down, so the series stop
		// too and the dashboard would go blank while the object lingers.
		// Every other phase either has pods now (Ready) or is on its way to
		// them, and a dashboard that arrives a reconcile early is harmless.
		switch list.Items[i].Status.Phase {
		case inferencev1alpha1.PhaseStopped, inferencev1alpha1.PhaseSuspended:
			continue
		}
		// Same helper the pod's runtime label comes from, so the set matches
		// the `runtime` label the dashboards' queries are grouped by.
		serving[runtimeNameLabel(&list.Items[i])] = true
	}
	return serving, nil
}

// publish creates-or-updates a candidate, overwriting spec drift. No owner
// reference: a dashboard outlives any single InferenceService that justified
// it, so owner-ref GC would delete it the moment one of several services on
// that runtime went away. retire handles removal instead.
func (r *GrafanaDashboardReconciler) publish(ctx context.Context, candidate *unstructured.Unstructured) error {
	desiredSpec, _, err := unstructured.NestedMap(candidate.Object, "spec")
	if err != nil {
		return err
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(candidate.GroupVersionKind())
	live.SetName(candidate.GetName())
	live.SetNamespace(candidate.GetNamespace())

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, live, func() error {
		live.Object["spec"] = desiredSpec
		live.SetLabels(candidate.GetLabels())
		// The requires-runtime annotation rides along so `kubectl get -o yaml`
		// answers why a dashboard is here, not just that it is.
		live.SetAnnotations(candidate.GetAnnotations())
		return nil
	})
	return err
}

// retire deletes a published dashboard whose runtime is no longer served,
// leaving anything this operator did not publish alone.
func (r *GrafanaDashboardReconciler) retire(
	ctx context.Context,
	candidate *unstructured.Unstructured,
	log logr.Logger,
) error {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(candidate.GroupVersionKind())
	key := types.NamespacedName{Namespace: candidate.GetNamespace(), Name: candidate.GetName()}
	if err := r.Get(ctx, key, live); err != nil {
		return client.IgnoreNotFound(err)
	}
	if live.GetLabels()[managedByLabel] != managedByValue {
		return nil
	}
	if err := r.Delete(ctx, live); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	log.Info("retired dashboard, no InferenceService on its runtime",
		"dashboard", key.String(), "runtime", candidate.GetAnnotations()[dashboardRuntimeAnnotation])
	return nil
}

// dashboardCRDPresent reports whether grafana-operator's CRD is registered,
// delegating to the shared crdDetector (positive detection cached; re-checks
// while absent so an operator installed after startup is picked up).
func (r *GrafanaDashboardReconciler) dashboardCRDPresent(log logr.Logger) (bool, error) {
	r.detectorOnce.Do(func() {
		r.detector = newCRDDetector(dashboardControllerName, []schema.GroupVersionKind{grafanaDashboardGVK()})
	})
	return r.detector.Present(r.Client, log)
}

func grafanaDashboardGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "grafana.integreatly.org",
		Version: "v1beta1",
		Kind:    "GrafanaDashboard",
	}
}

// ParseDashboardSource splits the --grafana-dashboard-source flag. An empty
// value disables the reconciler; anything else must name both halves, so a
// typo fails at startup rather than silently publishing nothing.
func ParseDashboardSource(flag string) (types.NamespacedName, error) {
	if strings.TrimSpace(flag) == "" {
		return types.NamespacedName{}, nil
	}
	namespace, name, found := strings.Cut(flag, "/")
	if !found || namespace == "" || name == "" {
		return types.NamespacedName{}, fmt.Errorf("expected <namespace>/<name>, got %q", flag)
	}
	return types.NamespacedName{Namespace: namespace, Name: name}, nil
}

// SetupWithManager watches InferenceServices. Like the SLO reconciler it does
// not Owns() the published resource: the grafana.integreatly.org CRD may be
// absent, and an Owns watch on an unregistered kind fails manager startup.
func (r *GrafanaDashboardReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.InferenceService{}).
		Named(dashboardControllerName).
		Complete(r)
}
