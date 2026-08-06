/*
Copyright 2025.

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
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The observed PyTorch wording from a real vLLM crash on a CUDA 12.4 driver.
const torchDriverTooOld = `Traceback (most recent call last):
  File "/usr/local/lib/python3.12/dist-packages/torch/cuda/__init__.py", line 412, in _lazy_init
RuntimeError: The NVIDIA driver on your system is too old (found version 12040).
Please update your GPU driver by downloading and installing a new version.`

func TestMatchCudaDriverMismatch(t *testing.T) {
	cases := []struct {
		name      string
		msg       string
		wantMatch bool
		wantIn    string
	}{
		{
			name:      "pytorch driver too old, multiline",
			msg:       torchDriverTooOld,
			wantMatch: true,
			wantIn:    "found version 12040",
		},
		{
			name:      "cuda runtime canonical wording",
			msg:       "CUDA error: CUDA driver version is insufficient for CUDA runtime version",
			wantMatch: true,
			wantIn:    "insufficient",
		},
		{
			name:      "raw error enum",
			msg:       "rpc failed: cudaErrorInsufficientDriver",
			wantMatch: true,
			wantIn:    "cudaErrorInsufficientDriver",
		},
		{
			name:      "case-insensitive",
			msg:       "THE NVIDIA DRIVER ON YOUR SYSTEM IS TOO OLD",
			wantMatch: true,
		},
		{
			// No uppercase bytes at all: asciiLower must take its
			// zero-copy path and matching must still work.
			name:      "already-lowercase input, no-copy fold path",
			msg:       "runtimeerror: the nvidia driver on your system is too old (found version 12040)",
			wantMatch: true,
			wantIn:    "found version 12040",
		},
		{
			name:      "driver-API enum spelling",
			msg:       "cuInit failed: CUDA_ERROR_INSUFFICIENT_DRIVER",
			wantMatch: true,
			wantIn:    "CUDA_ERROR_INSUFFICIENT_DRIVER",
		},
		{
			// Regression: strings.ToLower can change byte length for some
			// runes (U+023A grows from 2 to 3 bytes), so byte offsets from a
			// naive lowered copy would panic slicing the original message.
			name:      "multibyte prefix whose lowercase changes byte length",
			msg:       strings.Repeat("Ⱥ", 50) + "cudaErrorInsufficientDriver",
			wantMatch: true,
			wantIn:    "cudaErrorInsufficientDriver",
		},
		{
			name:      "control characters are sanitized out of the matched line",
			msg:       "CUDA driver version is insufficient\tfor CUDA runtime version\x1b[0m",
			wantMatch: true,
			wantIn:    "insufficient for CUDA runtime version",
		},
		{
			name:      "oom is not a driver mismatch",
			msg:       "CUDA out of memory. Tried to allocate 20.00 MiB",
			wantMatch: false,
		},
		{
			name:      "nvml skew is deliberately excluded",
			msg:       "Failed to initialize NVML: Driver/library version mismatch",
			wantMatch: false,
		},
		{
			name:      "empty message",
			msg:       "",
			wantMatch: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, ok := matchCudaDriverMismatch(tc.msg)
			if ok != tc.wantMatch {
				t.Fatalf("matchCudaDriverMismatch() match = %v, want %v", ok, tc.wantMatch)
			}
			if tc.wantIn != "" && !strings.Contains(line, tc.wantIn) {
				t.Errorf("matched line %q does not contain %q", line, tc.wantIn)
			}
			if ok && strings.Contains(line, "\n") {
				t.Errorf("matched line %q spans multiple lines", line)
			}
		})
	}
}

func TestMatchCudaDriverMismatchCapsLineLength(t *testing.T) {
	msg := "CUDA driver version is insufficient " + strings.Repeat("x", 1000)
	line, ok := matchCudaDriverMismatch(msg)
	if !ok {
		t.Fatal("expected a match")
	}
	if len(line) > 250 {
		t.Errorf("matched line not capped: len=%d", len(line))
	}

	// The cap must land on a rune boundary: place a multibyte rune across
	// the cut point and require valid UTF-8 output.
	msg = "cudaErrorInsufficientDriver " + strings.Repeat("x", 170) + strings.Repeat("€", 20)
	line, ok = matchCudaDriverMismatch(msg)
	if !ok {
		t.Fatal("expected a match")
	}
	if !utf8.ValidString(line) {
		t.Errorf("capped line is not valid UTF-8: %q", line)
	}
	if !strings.HasSuffix(line, "...") {
		t.Errorf("capped line missing ellipsis: %q", line)
	}
}

func crashPod(node, containerName, message string, exitCode int32, inLastState bool) corev1.Pod {
	term := &corev1.ContainerStateTerminated{ExitCode: exitCode, Message: message}
	cs := corev1.ContainerStatus{Name: containerName}
	if inLastState {
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
		cs.LastTerminationState = corev1.ContainerState{Terminated: term}
	} else {
		cs.State = corev1.ContainerState{Terminated: term}
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "default",
			Labels: map[string]string{
				"app":                           "svc",
				"inference.llmkube.dev/service": "svc",
			},
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}

func TestFindCudaDriverCrash(t *testing.T) {
	cases := []struct {
		name      string
		pods      []corev1.Pod
		container string
		wantFound bool
		wantNode  string
	}{
		{
			name:      "crashloop with mismatch in last termination state",
			pods:      []corev1.Pod{crashPod("node-a", "vllm", torchDriverTooOld, 1, true)},
			container: "vllm",
			wantFound: true,
			wantNode:  "node-a",
		},
		{
			name:      "fresh crash in current state, before backoff",
			pods:      []corev1.Pod{crashPod("node-b", "vllm", torchDriverTooOld, 1, false)},
			container: "vllm",
			wantFound: true,
			wantNode:  "node-b",
		},
		{
			name:      "clean exit is not a crash",
			pods:      []corev1.Pod{crashPod("node-a", "vllm", torchDriverTooOld, 0, false)},
			container: "vllm",
			wantFound: false,
		},
		{
			name:      "non-runtime container is ignored",
			pods:      []corev1.Pod{crashPod("node-a", "model-downloader", torchDriverTooOld, 1, true)},
			container: "vllm",
			wantFound: false,
		},
		{
			name:      "unrelated crash message",
			pods:      []corev1.Pod{crashPod("node-a", "vllm", "CUDA out of memory", 1, true)},
			container: "vllm",
			wantFound: false,
		},
		{
			name:      "no pods",
			pods:      nil,
			container: "vllm",
			wantFound: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node, _, _, found := findCudaDriverCrash(tc.pods, tc.container)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if found && node != tc.wantNode {
				t.Errorf("node = %q, want %q", node, tc.wantNode)
			}
		})
	}
}

func TestGfdCudaOffer(t *testing.T) {
	cases := []struct {
		name       string
		labels     map[string]string
		wantMajor  string
		wantMinor  string
		wantDriver string
	}{
		{
			name: "current GFD label names",
			labels: map[string]string{
				"nvidia.com/cuda.runtime-version.major": "12",
				"nvidia.com/cuda.runtime-version.minor": "4",
				"nvidia.com/cuda.driver-version.full":   "550.144.03",
			},
			wantMajor:  "12",
			wantMinor:  "4",
			wantDriver: "550.144.03",
		},
		{
			name: "deprecated label fallback",
			labels: map[string]string{
				"nvidia.com/cuda.runtime.major": "12",
				"nvidia.com/cuda.runtime.minor": "4",
				"nvidia.com/cuda.driver.major":  "550",
				"nvidia.com/cuda.driver.minor":  "144",
				"nvidia.com/cuda.driver.rev":    "03",
			},
			wantMajor:  "12",
			wantMinor:  "4",
			wantDriver: "550.144.03",
		},
		{
			name:   "no GFD labels",
			labels: map[string]string{"kubernetes.io/hostname": "node-a"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			major, minor, driver := gfdCudaOffer(tc.labels)
			if major != tc.wantMajor || minor != tc.wantMinor || driver != tc.wantDriver {
				t.Errorf("gfdCudaOffer() = (%q, %q, %q), want (%q, %q, %q)",
					major, minor, driver, tc.wantMajor, tc.wantMinor, tc.wantDriver)
			}
		})
	}
}

// newDriverCompatReconciler builds an InferenceServiceReconciler over a fake
// client seeded with the given objects, plus a buffered FakeRecorder for
// asserting event emission.
func newDriverCompatReconciler(t *testing.T, objs ...client.Object) (*InferenceServiceReconciler, *events.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{}).
		WithObjects(objs...).
		Build()
	recorder := events.NewFakeRecorder(16)
	return &InferenceServiceReconciler{Client: c, Scheme: scheme, Recorder: recorder}, recorder
}

func driverCompatISVC() *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "m", Runtime: RuntimeVLLM},
	}
}

func gfdNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"nvidia.com/cuda.runtime-version.major": "12",
				"nvidia.com/cuda.runtime-version.minor": "4",
				"nvidia.com/cuda.driver-version.full":   "550.144.03",
			},
		},
	}
}

// The diagnosis reads its signatures from the container's termination
// message, so the builder must opt every runtime container into the log-tail
// fallback: engines print the fatal error to the log, not /dev/termination-log.
func TestConstructDeploymentSetsTerminationMessagePolicy(t *testing.T) {
	r := &InferenceServiceReconciler{DefaultFSGroup: 102}
	isvc := sharingISvc(1, nil)
	model := sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia"})

	deployment := r.constructDeployment(isvc, model, 1)

	c := deployment.Spec.Template.Spec.Containers[0]
	if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Fatalf("TerminationMessagePolicy = %q, want %q", c.TerminationMessagePolicy, corev1.TerminationMessageFallbackToLogsOnError)
	}
}

func TestReconcileDriverCompatCondition(t *testing.T) {
	ctx := context.Background()

	t.Run("diagnoses mismatch with node detail and emits one event", func(t *testing.T) {
		isvc := driverCompatISVC()
		pod := crashPod("node-a", "vllm", torchDriverTooOld, 1, true)
		r, recorder := newDriverCompatReconciler(t, isvc, &pod, gfdNode("node-a"))

		r.reconcileDriverCompatCondition(ctx, isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if cond == nil {
			t.Fatal("expected DriverCompatible condition")
		}
		if cond.Status != metav1.ConditionFalse || cond.Reason != ReasonCUDADriverInsufficient {
			t.Fatalf("condition = %s/%s, want False/%s", cond.Status, cond.Reason, ReasonCUDADriverInsufficient)
		}
		for _, want := range []string{"node-a", "CUDA <= 12.4", "driver 550.144.03", "found version 12040", "CUDA 12.x"} {
			if !strings.Contains(cond.Message, want) {
				t.Errorf("message %q missing %q", cond.Message, want)
			}
		}
		if got := len(recorder.Events); got != 1 {
			t.Fatalf("events emitted = %d, want 1", got)
		}
		if ev := <-recorder.Events; !strings.Contains(ev, ReasonCUDADriverInsufficient) {
			t.Errorf("event %q missing reason CUDADriverInsufficient", ev)
		}

		// Second pass over the same crash: condition stays False, no new event.
		r.reconcileDriverCompatCondition(ctx, isvc)
		if got := len(recorder.Events); got != 0 {
			t.Fatalf("repeat reconcile emitted %d extra event(s), want 0", got)
		}
	})

	t.Run("no condition without a diagnosed crash", func(t *testing.T) {
		isvc := driverCompatISVC()
		healthy := crashPod("node-a", "vllm", "", 0, false)
		healthy.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		r, _ := newDriverCompatReconciler(t, isvc, &healthy)

		r.reconcileDriverCompatCondition(ctx, isvc)

		if meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible) != nil {
			t.Fatal("condition must stay absent until a mismatch is diagnosed")
		}
	})

	t.Run("unrelated crash does not flip a prior diagnosis", func(t *testing.T) {
		isvc := driverCompatISVC()
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type: ConditionDriverCompatible, Status: metav1.ConditionFalse,
			Reason: ReasonCUDADriverInsufficient, Message: "prior diagnosis",
		})
		oom := crashPod("node-a", "vllm", "CUDA out of memory", 1, true)
		r, _ := newDriverCompatReconciler(t, isvc, &oom)

		r.reconcileDriverCompatCondition(ctx, isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatal("prior diagnosis must persist while the runtime has not started")
		}
	})

	t.Run("recovers to True when the runtime container is ready", func(t *testing.T) {
		isvc := driverCompatISVC()
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type: ConditionDriverCompatible, Status: metav1.ConditionFalse,
			Reason: ReasonCUDADriverInsufficient, Message: "prior diagnosis",
		})
		ready := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p2", Namespace: "default",
				Labels: map[string]string{
					"app":                           "svc",
					"inference.llmkube.dev/service": "svc",
				},
			},
			Spec: corev1.PodSpec{NodeName: "node-a"},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "vllm", Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}},
		}
		r, _ := newDriverCompatReconciler(t, isvc, &ready)

		r.reconcileDriverCompatCondition(ctx, isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonRuntimeStarted {
			t.Fatalf("condition = %+v, want True/%s", cond, ReasonRuntimeStarted)
		}
	})

	t.Run("node gone: diagnosis from error text alone", func(t *testing.T) {
		isvc := driverCompatISVC()
		pod := crashPod("node-gone", "vllm", torchDriverTooOld, 1, true)
		r, _ := newDriverCompatReconciler(t, isvc, &pod)

		r.reconcileDriverCompatCondition(ctx, isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatal("expected False condition")
		}
		if !strings.Contains(cond.Message, "no longer exists") {
			t.Errorf("message %q missing node-gone fallback", cond.Message)
		}
	})

	t.Run("node without GFD labels", func(t *testing.T) {
		isvc := driverCompatISVC()
		pod := crashPod("node-plain", "vllm", torchDriverTooOld, 1, true)
		plain := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-plain"}}
		r, _ := newDriverCompatReconciler(t, isvc, &pod, plain)

		r.reconcileDriverCompatCondition(ctx, isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatal("expected False condition")
		}
		if !strings.Contains(cond.Message, "no GPU feature labels") {
			t.Errorf("message %q missing no-labels fallback", cond.Message)
		}
	})
}

// Recovery-path behaviors, split out to keep each test function within the
// lint complexity budget.
func TestReconcileDriverCompatConditionRecovery(t *testing.T) {
	ctx := context.Background()

	t.Run("ready container outranks its own stale crash evidence", func(t *testing.T) {
		// Regression: a container that crashed with the signature (e.g.
		// before the node's driver daemonset finished) and then started in
		// place keeps the message in LastTerminationState for the pod's
		// lifetime; once Ready, that evidence must not diagnose — or recover
		// a prior diagnosis — incorrectly.
		isvc := driverCompatISVC()
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type: ConditionDriverCompatible, Status: metav1.ConditionFalse,
			Reason: ReasonCUDADriverInsufficient, Message: "prior diagnosis",
		})
		recovered := crashPod("node-a", "vllm", torchDriverTooOld, 1, true)
		recovered.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		recovered.Status.ContainerStatuses[0].Ready = true
		r, _ := newDriverCompatReconciler(t, isvc, &recovered)

		r.reconcileDriverCompatCondition(ctx, isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonRuntimeStarted {
			t.Fatalf("condition = %+v, want True/%s despite stale LastTerminationState", cond, ReasonRuntimeStarted)
		}
	})

	t.Run("re-crash after recovery emits a second event", func(t *testing.T) {
		isvc := driverCompatISVC()
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type: ConditionDriverCompatible, Status: metav1.ConditionTrue,
			Reason: ReasonRuntimeStarted, Message: "recovered earlier",
		})
		pod := crashPod("node-a", "vllm", torchDriverTooOld, 1, true)
		r, recorder := newDriverCompatReconciler(t, isvc, &pod, gfdNode("node-a"))

		r.reconcileDriverCompatCondition(ctx, isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatal("expected a fresh False diagnosis after re-crash")
		}
		if got := len(recorder.Events); got != 1 {
			t.Fatalf("events emitted = %d, want 1 for the new episode", got)
		}
	})

	t.Run("metal-style absence of pods leaves prior diagnosis intact", func(t *testing.T) {
		isvc := driverCompatISVC()
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type: ConditionDriverCompatible, Status: metav1.ConditionFalse,
			Reason: ReasonCUDADriverInsufficient, Message: "prior diagnosis",
		})
		r, _ := newDriverCompatReconciler(t, isvc)

		r.reconcileDriverCompatCondition(ctx, isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatal("prior diagnosis must persist when no pods exist")
		}
	})

	t.Run("pod list failure only logs; no condition, no panic", func(t *testing.T) {
		isvc := driverCompatISVC()
		scheme := runtime.NewScheme()
		if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
			t.Fatalf("add inference scheme: %v", err)
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			t.Fatalf("add corev1 scheme: %v", err)
		}
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(isvc).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					return errors.New("apiserver unavailable")
				},
			}).
			Build()
		r := &InferenceServiceReconciler{Client: c, Scheme: scheme}

		r.reconcileDriverCompatCondition(ctx, isvc)

		if meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible) != nil {
			t.Fatal("a failed pod list must not produce a condition")
		}
	})

	t.Run("crash on a pod with no node name diagnoses from the error text", func(t *testing.T) {
		isvc := driverCompatISVC()
		pod := crashPod("", "vllm", torchDriverTooOld, 1, true)
		r, _ := newDriverCompatReconciler(t, isvc, &pod)

		r.reconcileDriverCompatCondition(ctx, isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatal("expected False condition")
		}
		if !strings.Contains(cond.Message, "newer GPU driver than the node provides") {
			t.Errorf("message %q missing generic diagnosis", cond.Message)
		}
	})

	t.Run("never touches phase or other conditions", func(t *testing.T) {
		isvc := driverCompatISVC()
		isvc.Status.Phase = PhaseCreating
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type: "Available", Status: metav1.ConditionTrue, Reason: "InferenceReady", Message: "serving",
		})
		pod := crashPod("node-a", "vllm", torchDriverTooOld, 1, true)
		r, _ := newDriverCompatReconciler(t, isvc, &pod, gfdNode("node-a"))

		r.reconcileDriverCompatCondition(ctx, isvc)

		if isvc.Status.Phase != PhaseCreating {
			t.Errorf("phase changed to %q; diagnosis must not touch phase", isvc.Status.Phase)
		}
		if !meta.IsStatusConditionTrue(isvc.Status.Conditions, "Available") {
			t.Error("Available condition disturbed; diagnosis must not touch it")
		}
	})
}

// Driver-skew behaviors: replicas split across nodes with different driver
// support. Readiness recovers a diagnosis only within the pod that carries
// the crash evidence — a healthy or recovered replica elsewhere must never
// mask the one still crashing.
func TestReconcileDriverCompatConditionDriverSkew(t *testing.T) {
	ctx := context.Background()

	readyPod := func(name, node string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "default",
				Labels: map[string]string{
					"app":                           "svc",
					"inference.llmkube.dev/service": "svc",
				},
			},
			Spec: corev1.PodSpec{NodeName: node},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "vllm", Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}},
		}
	}

	t.Run("healthy replica does not mask a crash-looping one", func(t *testing.T) {
		isvc := driverCompatISVC()
		good := readyPod("p-good", "node-good")
		bad := crashPod("node-bad", "vllm", torchDriverTooOld, 1, true)
		bad.Name = "p-bad"
		r, recorder := newDriverCompatReconciler(t, isvc, &good, &bad)

		r.reconcileDriverCompatCondition(ctx, isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonCUDADriverInsufficient {
			t.Fatalf("condition = %+v, want False/%s despite the healthy replica", cond, ReasonCUDADriverInsufficient)
		}
		if !strings.Contains(cond.Message, "node-bad") {
			t.Errorf("message %q must name the crashing pod's node", cond.Message)
		}
		if got := len(recorder.Events); got != 1 {
			t.Fatalf("events emitted = %d, want 1", got)
		}
	})

	t.Run("scale-up onto a healthy node does not recover a still-crashing carrier", func(t *testing.T) {
		isvc := driverCompatISVC()
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type: ConditionDriverCompatible, Status: metav1.ConditionFalse,
			Reason: ReasonCUDADriverInsufficient, Message: "prior diagnosis",
		})
		good := readyPod("p-good", "node-good")
		bad := crashPod("node-bad", "vllm", torchDriverTooOld, 1, true)
		bad.Name = "p-bad"
		r, recorder := newDriverCompatReconciler(t, isvc, &good, &bad)

		r.reconcileDriverCompatCondition(ctx, isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatalf("condition = %+v, must stay False while the carrier still crashes", cond)
		}
		if got := len(recorder.Events); got != 0 {
			t.Fatalf("events emitted = %d, want 0 for an unbroken episode", got)
		}
	})

	t.Run("active crash outranks another pod's stale recovered evidence", func(t *testing.T) {
		isvc := driverCompatISVC()
		recovered := crashPod("node-a", "vllm", torchDriverTooOld, 1, true)
		recovered.Name = "a-recovered"
		recovered.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		recovered.Status.ContainerStatuses[0].Ready = true
		crashing := crashPod("node-b", "vllm", torchDriverTooOld, 1, true)
		crashing.Name = "b-crashing"
		r, _ := newDriverCompatReconciler(t, isvc, &recovered, &crashing)

		r.reconcileDriverCompatCondition(ctx, isvc)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatalf("condition = %+v, want False from the active carrier", cond)
		}
		if !strings.Contains(cond.Message, "node-b") {
			t.Errorf("message %q must come from the active carrier's node, not the recovered one's", cond.Message)
		}
	})
}

// A node Get failure is not node deletion: the message must not claim the
// node is gone, and the error must leave a log trail.
func TestDriverCompatNodeReadFailure(t *testing.T) {
	ctx := context.Background()
	isvc := driverCompatISVC()
	pod := crashPod("node-a", "vllm", torchDriverTooOld, 1, true)

	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(isvc, &pod, gfdNode("node-a")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isNode := obj.(*corev1.Node); isNode {
					return errors.New("apiserver throttled")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &InferenceServiceReconciler{Client: c, Scheme: scheme}

	r.reconcileDriverCompatCondition(ctx, isvc)

	cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatal("expected False condition despite the node read failure")
	}
	if !strings.Contains(cond.Message, "not readable right now") {
		t.Errorf("message %q must say the node is unreadable, not gone", cond.Message)
	}
	if strings.Contains(cond.Message, "no longer exists") {
		t.Errorf("message %q claims deletion on a transient read failure", cond.Message)
	}
}
