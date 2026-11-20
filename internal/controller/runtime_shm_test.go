package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestNeedsSharedMemory(t *testing.T) {
	two := int32(2)
	one := int32(1)
	tests := []struct {
		name     string
		backend  RuntimeBackend
		spec     inferencev1alpha1.InferenceServiceSpec
		gpuCount int32
		want     bool
	}{
		{"vllm single GPU", &VLLMBackend{}, inferencev1alpha1.InferenceServiceSpec{}, 1, false},
		{"vllm TP defaulted from GPU count", &VLLMBackend{}, inferencev1alpha1.InferenceServiceSpec{}, 2, true},
		{"vllm explicit TP 2 without device-plugin GPUs (DRA)", &VLLMBackend{},
			inferencev1alpha1.InferenceServiceSpec{VLLMConfig: &inferencev1alpha1.VLLMConfig{TensorParallelSize: &two}}, 0, true},
		{"vllm explicit PP 2", &VLLMBackend{},
			inferencev1alpha1.InferenceServiceSpec{VLLMConfig: &inferencev1alpha1.VLLMConfig{PipelineParallelSize: &two}}, 0, true},
		{"vllm explicit TP 1 on one GPU", &VLLMBackend{},
			inferencev1alpha1.InferenceServiceSpec{VLLMConfig: &inferencev1alpha1.VLLMConfig{TensorParallelSize: &one}}, 1, false},
		{"sglang single GPU", &SGLangBackend{}, inferencev1alpha1.InferenceServiceSpec{}, 1, false},
		{"sglang TP defaulted from GPU count", &SGLangBackend{}, inferencev1alpha1.InferenceServiceSpec{}, 2, true},
		{"sglang explicit DP 2", &SGLangBackend{},
			inferencev1alpha1.InferenceServiceSpec{SGLangConfig: &inferencev1alpha1.SGLangConfig{DataParallelSize: &two}}, 0, true},
		{"tgi single GPU", &TGIBackend{}, inferencev1alpha1.InferenceServiceSpec{}, 1, false},
		{"tgi sharded across GPUs", &TGIBackend{}, inferencev1alpha1.InferenceServiceSpec{}, 2, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := tc.backend.(SharedMemoryRequirer)
			if !ok {
				t.Fatalf("%T does not implement SharedMemoryRequirer", tc.backend)
			}
			isvc := &inferencev1alpha1.InferenceService{Spec: tc.spec}
			if got := r.NeedsSharedMemory(isvc, tc.gpuCount); got != tc.want {
				t.Errorf("NeedsSharedMemory(gpuCount=%d) = %v, want %v", tc.gpuCount, got, tc.want)
			}
		})
	}

	// Single-process runtimes do not use /dev/shm across workers and must not
	// opt in, so their pods stay exactly as before.
	for _, b := range []RuntimeBackend{&LlamaCppBackend{}, &GenericBackend{}} {
		if _, ok := b.(SharedMemoryRequirer); ok {
			t.Errorf("%T implements SharedMemoryRequirer; only multi-process runtimes should", b)
		}
	}
}

// findDevShm returns the volume mounted at /dev/shm in the serving container and
// the mount itself, or nils.
func findDevShm(pod corev1.PodSpec) (*corev1.Volume, *corev1.VolumeMount) {
	var mount *corev1.VolumeMount
	for i := range pod.Containers[0].VolumeMounts {
		if pod.Containers[0].VolumeMounts[i].MountPath == "/dev/shm" {
			mount = &pod.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		return nil, nil
	}
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == mount.Name {
			return &pod.Volumes[i], mount
		}
	}
	return nil, mount
}

func TestConstructDeployment_DevShm(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Format: "safetensors", Source: "hf://org/repo"},
	}
	svc := func(runtime string, gpu int32, mutate func(*inferencev1alpha1.InferenceServiceSpec)) *inferencev1alpha1.InferenceService {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime:   runtime,
				Resources: &inferencev1alpha1.InferenceResourceRequirements{GPU: gpu},
			},
		}
		if mutate != nil {
			mutate(&isvc.Spec)
		}
		return isvc
	}
	tests := []struct {
		name      string
		isvc      *inferencev1alpha1.InferenceService
		wantShm   bool
		wantLimit string // expected sizeLimit when wantShm
	}{
		{"vllm 2 GPUs, no memory limit", svc(RuntimeVLLM, 2, nil), true, "16Gi"},
		{"vllm 1 GPU", svc(RuntimeVLLM, 1, nil), false, ""},
		{"sglang 2 GPUs", svc(RuntimeSGLANG, 2, nil), true, "16Gi"},
		{"tgi 2 GPUs", svc("tgi", 2, nil), true, "16Gi"},
		{"llamacpp 2 GPUs", svc("llamacpp", 2, nil), false, ""},
		{"vllm 2 GPUs, 8Gi memory caps at half", svc(RuntimeVLLM, 2, func(s *inferencev1alpha1.InferenceServiceSpec) {
			s.Resources.Memory = "8Gi"
		}), true, "4Gi"},
		{"vllm 2 GPUs, memoryLimit 64Gi caps at 16Gi", svc(RuntimeVLLM, 2, func(s *inferencev1alpha1.InferenceServiceSpec) {
			s.Resources.Memory = "32Gi"
			s.Resources.MemoryLimit = "64Gi"
		}), true, "16Gi"},
		{"vllm 2 GPUs, memoryLimit 12Gi caps at 6Gi", svc(RuntimeVLLM, 2, func(s *inferencev1alpha1.InferenceServiceSpec) {
			s.Resources.Memory = "8Gi"
			s.Resources.MemoryLimit = "12Gi"
		}), true, "6Gi"},
		{"multiNode ranks use the host /dev/shm", svc(RuntimeVLLM, 2, func(s *inferencev1alpha1.InferenceServiceSpec) {
			s.MultiNode = &inferencev1alpha1.MultiNodeSpec{}
		}), false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
			pod := r.deploymentFor(tc.isvc, model, resolveEffectiveModelCache(tc.isvc, model), nil, 1, "", "").Spec.Template.Spec
			vol, mount := findDevShm(pod)
			if !tc.wantShm {
				if mount != nil {
					t.Fatalf("got a /dev/shm mount (volume %q), want none", mount.Name)
				}
				return
			}
			if mount == nil || vol == nil {
				t.Fatalf("no /dev/shm volume and mount; volumes=%v", pod.Volumes)
			}
			if vol.EmptyDir == nil || vol.EmptyDir.Medium != corev1.StorageMediumMemory {
				t.Fatalf("/dev/shm volume = %+v, want a memory-backed emptyDir", vol.VolumeSource)
			}
			want := resource.MustParse(tc.wantLimit)
			if vol.EmptyDir.SizeLimit == nil || vol.EmptyDir.SizeLimit.Cmp(want) != 0 {
				t.Errorf("sizeLimit = %v, want %s", vol.EmptyDir.SizeLimit, tc.wantLimit)
			}
		})
	}

	// A user-supplied /dev/shm mount (the documented workaround) wins: the
	// operator adds no second volume or mount, and the user's stays as given.
	t.Run("user /dev/shm mount wins", func(t *testing.T) {
		userLimit := resource.MustParse("2Gi")
		isvc := svc(RuntimeVLLM, 4, func(s *inferencev1alpha1.InferenceServiceSpec) {
			s.ExtraVolumes = []corev1.Volume{{Name: "shm", VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &userLimit}}}}
			s.ExtraVolumeMounts = []corev1.VolumeMount{{Name: "shm", MountPath: "/dev/shm"}}
		})
		r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
		pod := r.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec
		mounts := 0
		for _, m := range pod.Containers[0].VolumeMounts {
			if m.MountPath == "/dev/shm" {
				mounts++
			}
		}
		if mounts != 1 {
			t.Fatalf("%d mounts at /dev/shm, want exactly the user's one", mounts)
		}
		vol, mount := findDevShm(pod)
		if mount.Name != "shm" || vol == nil || vol.EmptyDir.SizeLimit.Cmp(userLimit) != 0 {
			t.Errorf("/dev/shm = %+v from volume %+v, want the user's 2Gi volume \"shm\"", mount, vol)
		}
		for _, v := range pod.Volumes {
			if v.Name == devShmVolumeName {
				t.Errorf("operator volume %q added alongside the user's /dev/shm", devShmVolumeName)
			}
		}
	})
}
