/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// onePerNode is the #2198 shape: one proxy replica per node, enforced.
func onePerNode(mr *inferencev1alpha1.ModelRouter) []corev1.TopologySpreadConstraint {
	return []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       "kubernetes.io/hostname",
		WhenUnsatisfiable: corev1.DoNotSchedule,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: routerProxySelectorLabels(mr)},
	}}
}

func TestRouterDeploymentBuilderAffinityAndSpread(t *testing.T) {
	mr := canonicalModelRouter()
	aff := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"node-a", "node-b"}}},
		}}},
	}}
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{Replicas: ptr.To[int32](2), Affinity: aff, TopologySpreadConstraints: onePerNode(mr)}
	r := &ModelRouterReconciler{RouterProxyImage: "img"}
	spec := r.newRouterDeployment(mr, &compiledConfig{Hash: "hash"}).Spec.Template.Spec
	if !reflect.DeepEqual(spec.Affinity, aff) {
		t.Errorf("pod Affinity = %+v, want %+v", spec.Affinity, aff)
	}
	if !reflect.DeepEqual(spec.TopologySpreadConstraints, onePerNode(mr)) {
		t.Errorf("pod TopologySpreadConstraints = %+v, want the one-per-node spread", spec.TopologySpreadConstraints)
	}
}

// Unset leaves today's pod template and rollout strategy, so existing routers
// do not re-roll on upgrade.
func TestRouterDeploymentBuilderPlacementUnset(t *testing.T) {
	r := &ModelRouterReconciler{RouterProxyImage: "img"}
	base := r.newRouterDeployment(canonicalModelRouter(), &compiledConfig{Hash: "hash"})
	withProxy := canonicalModelRouter()
	withProxy.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{Replicas: ptr.To[int32](2)}
	got := r.newRouterDeployment(withProxy, &compiledConfig{Hash: "hash"})
	if got.Spec.Template.Spec.Affinity != nil || got.Spec.Template.Spec.TopologySpreadConstraints != nil {
		t.Fatalf("placement set without spec.proxy affinity or spread: %+v / %+v",
			got.Spec.Template.Spec.Affinity, got.Spec.Template.Spec.TopologySpreadConstraints)
	}
	if !reflect.DeepEqual(base.Spec.Template, got.Spec.Template) {
		t.Error("pod template differs between nil proxy and a proxy without placement")
	}
	if !reflect.DeepEqual(got.Spec.Strategy, appsv1.DeploymentStrategy{}) {
		t.Errorf("strategy = %+v, want the Kubernetes default (unset)", got.Spec.Strategy)
	}
}

// With a hard one-per-node rule and more than one replica, a surge pod can
// never schedule, so the rollout replaces one replica at a time instead.
func TestRouterDeploymentStrategyForHardPlacement(t *testing.T) {
	noSurge := appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       ptr.To(intstr.FromInt32(0)),
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
		},
	}
	antiAffinity := func(mr *inferencev1alpha1.ModelRouter) *corev1.Affinity {
		return &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey:   "kubernetes.io/hostname",
				LabelSelector: &metav1.LabelSelector{MatchLabels: routerProxySelectorLabels(mr)},
			}},
		}}
	}
	cases := []struct {
		name     string
		proxy    func(mr *inferencev1alpha1.ModelRouter) *inferencev1alpha1.RouterProxySpec
		hasPools bool
		want     appsv1.DeploymentStrategy
	}{
		{"hard spread, two replicas", func(mr *inferencev1alpha1.ModelRouter) *inferencev1alpha1.RouterProxySpec {
			return &inferencev1alpha1.RouterProxySpec{Replicas: ptr.To[int32](2), TopologySpreadConstraints: onePerNode(mr)}
		}, false, noSurge},
		{"required anti-affinity, two replicas", func(mr *inferencev1alpha1.ModelRouter) *inferencev1alpha1.RouterProxySpec {
			return &inferencev1alpha1.RouterProxySpec{Replicas: ptr.To[int32](2), Affinity: antiAffinity(mr)}
		}, false, noSurge},
		{"soft spread keeps the default", func(mr *inferencev1alpha1.ModelRouter) *inferencev1alpha1.RouterProxySpec {
			tsc := onePerNode(mr)
			tsc[0].WhenUnsatisfiable = corev1.ScheduleAnyway
			return &inferencev1alpha1.RouterProxySpec{Replicas: ptr.To[int32](2), TopologySpreadConstraints: tsc}
		}, false, appsv1.DeploymentStrategy{}},
		{"one replica keeps the default", func(mr *inferencev1alpha1.ModelRouter) *inferencev1alpha1.RouterProxySpec {
			return &inferencev1alpha1.RouterProxySpec{Replicas: ptr.To[int32](1), TopologySpreadConstraints: onePerNode(mr)}
		}, false, appsv1.DeploymentStrategy{}},
		{"ModelPools pin one replica, so the default", func(mr *inferencev1alpha1.ModelRouter) *inferencev1alpha1.RouterProxySpec {
			return &inferencev1alpha1.RouterProxySpec{Replicas: ptr.To[int32](2), TopologySpreadConstraints: onePerNode(mr)}
		}, true, appsv1.DeploymentStrategy{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := canonicalModelRouter()
			mr.Spec.Proxy = tc.proxy(mr)
			r := &ModelRouterReconciler{RouterProxyImage: "img"}
			got := r.newRouterDeployment(mr, &compiledConfig{Hash: "hash", HasPools: tc.hasPools}).Spec.Strategy
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("strategy = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Placement and the strategy reach the live Deployment, and removing them
// takes them back off.
func TestReconcileRouterDeploymentAppliesPlacement(t *testing.T) {
	mr := canonicalModelRouter()
	scheme := builderTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr).Build()
	rec := &ModelRouterReconciler{Client: c, Scheme: scheme, RouterProxyImage: "img"}
	ctx := context.Background()
	key := types.NamespacedName{Name: routerProxyResourceName(mr.Name), Namespace: mr.Namespace}
	live := func() *appsv1.Deployment {
		dep := &appsv1.Deployment{}
		if err := c.Get(ctx, key, dep); err != nil {
			t.Fatal(err)
		}
		return dep
	}

	if err := rec.reconcileRouterDeployment(ctx, mr, &compiledConfig{Hash: "hash"}); err != nil {
		t.Fatal(err)
	}
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{Replicas: ptr.To[int32](2), TopologySpreadConstraints: onePerNode(mr)}
	if err := rec.reconcileRouterDeployment(ctx, mr, &compiledConfig{Hash: "hash"}); err != nil {
		t.Fatal(err)
	}
	dep := live()
	if len(dep.Spec.Template.Spec.TopologySpreadConstraints) != 1 {
		t.Errorf("live spread = %+v, want the one-per-node constraint", dep.Spec.Template.Spec.TopologySpreadConstraints)
	}
	if ru := dep.Spec.Strategy.RollingUpdate; ru == nil || ru.MaxSurge == nil || ru.MaxSurge.IntValue() != 0 {
		t.Errorf("live strategy = %+v, want maxSurge 0", dep.Spec.Strategy)
	}

	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{Replicas: ptr.To[int32](2)}
	if err := rec.reconcileRouterDeployment(ctx, mr, &compiledConfig{Hash: "hash"}); err != nil {
		t.Fatal(err)
	}
	dep = live()
	if dep.Spec.Template.Spec.TopologySpreadConstraints != nil {
		t.Errorf("live spread after removal = %+v, want none", dep.Spec.Template.Spec.TopologySpreadConstraints)
	}
	if ru := dep.Spec.Strategy.RollingUpdate; ru != nil && ru.MaxSurge != nil && ru.MaxSurge.IntValue() == 0 {
		t.Errorf("live strategy after removal = %+v, want the no-surge rule gone", dep.Spec.Strategy)
	}
}
