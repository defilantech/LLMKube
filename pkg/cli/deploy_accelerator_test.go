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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// A Mac with the metal agent installed runs `llmkube deploy --gpu` against a
// cluster whose GPUs are NVIDIA and whose namespace has no MetalNode. The
// accelerator follows the cluster, so the Model is CUDA, not Metal (#2414).
// The agent on PATH is what the CLI used to read: on a Mac it picked Metal
// and the service waited forever for an agent.
func TestDeployGPUFromAMacToAnNVIDIAClusterPicksCUDA(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	ctx := context.Background()

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "llmkube-metal-agent"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-" + ns}}
	if err := c.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), node) })
	node.Status.Capacity = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}
	node.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}
	if err := c.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}

	var err error
	stdout := captureStdout(t, func() {
		cmd := NewDeployCommand()
		cmd.SetArgs([]string{"mac-to-nvidia", "-n", ns, "--gpu",
			"--source", "https://example.com/model.gguf", "--wait=false"})
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, stdout)
	}

	var model inferencev1alpha1.Model
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "mac-to-nvidia"}, &model); err != nil {
		t.Fatal(err)
	}
	if got := model.Spec.Hardware.Accelerator; got != acceleratorCUDA {
		t.Errorf("Model accelerator = %q, want %q; deploy printed:\n%s", got, acceleratorCUDA, stdout)
	}
	if !strings.Contains(stdout, "--accelerator") {
		t.Errorf("deploy did not say how to override the accelerator; it printed:\n%s", stdout)
	}
}

func testMetalNode(namespace string, reachable metav1.ConditionStatus) *inferencev1alpha1.MetalNode {
	return &inferencev1alpha1.MetalNode{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "studio"},
		Status: inferencev1alpha1.MetalNodeStatus{Conditions: []metav1.Condition{{
			Type:               inferencev1alpha1.MetalNodeConditionReachable,
			Status:             reachable,
			Reason:             inferencev1alpha1.MetalNodeReasonDialBackSucceeded,
			LastTransitionTime: metav1.Now(),
		}}},
	}
}

func testNode(name, nvidiaGPUs string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	n.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("16")}
	if nvidiaGPUs != "" {
		n.Status.Allocatable[nvidiaGPUResource] = resource.MustParse(nvidiaGPUs)
	}
	return n
}

// failList makes List fail for one list type, the way a namespace-scoped
// user's client or a cluster without the MetalNode CRD does.
func failList[L client.ObjectList](err error) interceptor.Funcs {
	return interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := list.(L); ok {
			return err
		}
		return c.List(ctx, list, opts...)
	}}
}

// The accelerator --gpu picks is a function of the target cluster: metal only
// with a Reachable MetalNode in the namespace and no NVIDIA GPU capacity, cuda
// in every other case, including when the cluster cannot be asked (#2414).
func TestChooseGPUAccelerator(t *testing.T) {
	const ns = "team"
	noCRD := &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: inferencev1alpha1.GroupVersion.Group, Kind: "MetalNode"},
		SearchedVersions: []string{inferencev1alpha1.GroupVersion.Version},
	}
	forbidden := func(plural string) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: plural}, "",
			errString("User cannot list resource "+plural))
	}
	tests := []struct {
		name       string
		objects    []client.Object
		funcs      interceptor.Funcs
		want       string
		wantReason []string
	}{
		{
			name:       "Reachable MetalNode and no NVIDIA GPUs picks metal",
			objects:    []client.Object{testMetalNode(ns, metav1.ConditionTrue), testNode("cpu-1", "")},
			want:       acceleratorMetal,
			wantReason: []string{"1 Reachable MetalNode", "no node advertises nvidia.com/gpu"},
		},
		{
			name:       "NVIDIA GPU nodes and no MetalNode pick cuda",
			objects:    []client.Object{testNode("gpu-1", "8")},
			want:       acceleratorCUDA,
			wantReason: []string{"no Reachable MetalNode in namespace team"},
		},
		{
			name:       "both pick cuda and say a Mac is there",
			objects:    []client.Object{testMetalNode(ns, metav1.ConditionTrue), testNode("gpu-1", "8")},
			want:       acceleratorCUDA,
			wantReason: []string{"1 node(s) advertise nvidia.com/gpu", "also has 1 Reachable MetalNode"},
		},
		{
			name:       "neither picks cuda",
			objects:    []client.Object{testNode("cpu-1", "")},
			want:       acceleratorCUDA,
			wantReason: []string{"no Reachable MetalNode"},
		},
		{
			name:       "a MetalNode that is not Reachable does not count",
			objects:    []client.Object{testMetalNode(ns, metav1.ConditionFalse), testNode("cpu-1", "")},
			want:       acceleratorCUDA,
			wantReason: []string{"no Reachable MetalNode"},
		},
		{
			name:       "a MetalNode in another namespace does not count",
			objects:    []client.Object{testMetalNode("elsewhere", metav1.ConditionTrue), testNode("cpu-1", "")},
			want:       acceleratorCUDA,
			wantReason: []string{"no Reachable MetalNode in namespace team"},
		},
		{
			name:       "zero allocatable nvidia.com/gpu is no NVIDIA capacity",
			objects:    []client.Object{testMetalNode(ns, metav1.ConditionTrue), testNode("gpu-drained", "0")},
			want:       acceleratorMetal,
			wantReason: []string{"no node advertises nvidia.com/gpu"},
		},
		{
			name:       "no MetalNode CRD picks cuda",
			objects:    []client.Object{testNode("cpu-1", "")},
			funcs:      failList[*inferencev1alpha1.MetalNodeList](noCRD),
			want:       acceleratorCUDA,
			wantReason: []string{"could not list MetalNodes", "no matches for kind"},
		},
		{
			name:       "MetalNode list forbidden picks cuda",
			objects:    []client.Object{testMetalNode(ns, metav1.ConditionTrue)},
			funcs:      failList[*inferencev1alpha1.MetalNodeList](forbidden("metalnodes")),
			want:       acceleratorCUDA,
			wantReason: []string{"could not list MetalNodes", "forbidden"},
		},
		{
			name:       "node list forbidden with a Reachable MetalNode picks cuda",
			objects:    []client.Object{testMetalNode(ns, metav1.ConditionTrue)},
			funcs:      failList[*corev1.NodeList](forbidden("nodes")),
			want:       acceleratorCUDA,
			wantReason: []string{"1 Reachable MetalNode", "listing nodes for nvidia.com/gpu failed", "forbidden"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newCLIScheme()).WithObjects(tt.objects...).
				WithInterceptorFuncs(tt.funcs).Build()
			got := chooseGPUAccelerator(context.Background(), c, ns)
			if got.accelerator != tt.want {
				t.Errorf("accelerator = %q (%s), want %q", got.accelerator, got.reason, tt.want)
			}
			for _, want := range tt.wantReason {
				if !strings.Contains(got.reason, want) {
					t.Errorf("reason %q lacks %q", got.reason, want)
				}
			}
		})
	}
}

// resolveAcceleratorAndImage asks the cluster only when --gpu leaves the
// accelerator open, takes its answer, and prints it with the override.
func TestResolveAcceleratorAsksTheClusterOnlyWhenOpen(t *testing.T) {
	asked := 0
	pick := func() acceleratorChoice {
		asked++
		return acceleratorChoice{accelerator: acceleratorMetal, reason: "a Mac and no NVIDIA GPUs"}
	}

	opts := &deployOptions{gpu: true, gpuVendor: defaultGPUVendor, pickGPUAccelerator: pick}
	out := captureStdout(t, func() { resolveAcceleratorAndImage(opts) })
	if opts.accelerator != acceleratorMetal || asked != 1 {
		t.Errorf("accelerator = %q after %d cluster lookups, want metal after 1", opts.accelerator, asked)
	}
	for _, want := range []string{"Accelerator: metal (a Mac and no NVIDIA GPUs)", "--accelerator",
		"brew install defilantech/tap/llmkube-metal-agent", "llmkube metal join"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "make install-metal-agent") {
		t.Errorf("output still points at a source checkout:\n%s", out)
	}

	for _, explicit := range []*deployOptions{
		{gpu: true, accelerator: acceleratorCUDA, pickGPUAccelerator: pick},
		{accelerator: acceleratorMetal, pickGPUAccelerator: pick},
		{pickGPUAccelerator: pick},
	} {
		asked = 0
		want := explicit.accelerator
		if want == "" {
			want = acceleratorCPU
		}
		resolveAcceleratorAndImage(explicit)
		if asked != 0 || explicit.accelerator != want {
			t.Errorf("gpu=%v accelerator=%q: asked the cluster %d times and chose %q, want no lookup and %q",
				explicit.gpu, want, asked, explicit.accelerator, want)
		}
	}
}
