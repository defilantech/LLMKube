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
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

type InferenceServiceReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	ModelCachePath       string
	ModelCacheSize       string
	ModelCacheClass      string
	ModelCacheAccessMode string
	CACertConfigMap      string
	InitContainerImage   string
}

func sanitizeDNSName(name string) string {
	return strings.ReplaceAll(name, ".", "-")
}

func isLocalModelSource(source string) bool {
	return strings.HasPrefix(source, "file://") || strings.HasPrefix(source, "/")
}

func buildModelInitCommand(isLocal, useCache bool) string {
	if useCache {
		if isLocal {
			return `mkdir -p "$CACHE_DIR" && if [ ! -f "$MODEL_PATH" ]; then echo 'Copying model from local source...'; cp /host-model/model.gguf "$MODEL_PATH" && echo 'Model copied successfully'; else echo 'Model already cached, skipping copy'; fi`
		}
		return `mkdir -p "$CACHE_DIR" && if [ ! -f "$MODEL_PATH" ]; then echo 'Downloading model...'; curl -f -L -o "$MODEL_PATH" "$MODEL_SOURCE" && echo 'Model downloaded successfully'; else echo 'Model already cached, skipping download'; fi`
	}

	if isLocal {
		return `echo 'ERROR: Local model source requires model cache to be configured.'; exit 1`
	}
	return `if [ ! -f "$MODEL_PATH" ]; then echo 'Downloading model...'; curl -f -L -o "$MODEL_PATH" "$MODEL_SOURCE" && echo 'Model downloaded successfully'; else echo 'Model already exists, skipping download'; fi`
}

func modelInitEnvVars(source, cacheDir, modelPath string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "MODEL_SOURCE", Value: source},
		{Name: "CACHE_DIR", Value: cacheDir},
		{Name: "MODEL_PATH", Value: modelPath},
	}
}

type modelStorageConfig struct {
	modelPath      string
	initContainers []corev1.Container
	volumes        []corev1.Volume
	volumeMounts   []corev1.VolumeMount
}

func buildModelStorageConfig(model *inferencev1alpha1.Model, namespace string, useCache bool, caCertConfigMap string, initContainerImage string) modelStorageConfig {
	if useCache {
		return buildCachedStorageConfig(model, caCertConfigMap, initContainerImage)
	}
	return buildEmptyDirStorageConfig(model, namespace, caCertConfigMap, initContainerImage)
}

func buildCachedStorageConfig(model *inferencev1alpha1.Model, caCertConfigMap string, initContainerImage string) modelStorageConfig {
	cacheDir := fmt.Sprintf("/models/%s", model.Status.CacheKey)
	modelPath := fmt.Sprintf("%s/model.gguf", cacheDir)

	initVolumeMounts := []corev1.VolumeMount{
		{Name: "model-cache", MountPath: "/models"},
	}

	volumes := []corev1.Volume{
		{
			Name: "model-cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: ModelCachePVCName,
					ReadOnly:  false,
				},
			},
		},
	}

	if isLocalModelSource(model.Spec.Source) {
		localPath := getLocalPath(model.Spec.Source)
		volumes = append(volumes, corev1.Volume{
			Name: "host-model",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: localPath,
					Type: func() *corev1.HostPathType { t := corev1.HostPathFile; return &t }(),
				},
			},
		})
		initVolumeMounts = append(initVolumeMounts, corev1.VolumeMount{
			Name:      "host-model",
			MountPath: "/host-model/model.gguf",
			ReadOnly:  true,
		})
	}

	cmd := buildModelInitCommand(isLocalModelSource(model.Spec.Source), true)
	env := modelInitEnvVars(model.Spec.Source, cacheDir, modelPath)
	if caCertConfigMap != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "custom-ca-cert",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: caCertConfigMap},
				},
			},
		})
		initVolumeMounts = append(initVolumeMounts, corev1.VolumeMount{
			Name:      "custom-ca-cert",
			MountPath: "/custom-certs",
			ReadOnly:  true,
		})
		cmd = fmt.Sprintf("export CURL_CA_BUNDLE=/custom-certs/$(ls /custom-certs | grep -v '^\\.' | head -n 1) && %s", cmd)
	}

	return modelStorageConfig{
		modelPath: modelPath,
		initContainers: []corev1.Container{
			{
				Name:         "model-downloader",
				Image:        initContainerImage,
				Command:      []string{"sh", "-c", cmd},
				Env:          env,
				VolumeMounts: initVolumeMounts,
			},
		},
		volumes:      volumes,
		volumeMounts: []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models", ReadOnly: true}},
	}
}

func buildEmptyDirStorageConfig(model *inferencev1alpha1.Model, namespace string, caCertConfigMap string, initContainerImage string) modelStorageConfig {
	modelFileName := fmt.Sprintf("%s-%s.gguf", namespace, model.Name)
	modelPath := fmt.Sprintf("/models/%s", modelFileName)

	initVolumeMounts := []corev1.VolumeMount{{Name: "model-storage", MountPath: "/models"}}
	volumes := []corev1.Volume{
		{
			Name:         "model-storage",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	cmd := buildModelInitCommand(isLocalModelSource(model.Spec.Source), false)
	env := modelInitEnvVars(model.Spec.Source, "", modelPath)
	if caCertConfigMap != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "custom-ca-cert",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: caCertConfigMap},
				},
			},
		})
		initVolumeMounts = append(initVolumeMounts, corev1.VolumeMount{
			Name:      "custom-ca-cert",
			MountPath: "/custom-certs",
			ReadOnly:  true,
		})
		cmd = fmt.Sprintf("export CURL_CA_BUNDLE=/custom-certs/$(ls /custom-certs | grep -v '^\\.' | head -n 1) && %s", cmd)
	}

	return modelStorageConfig{
		modelPath: modelPath,
		initContainers: []corev1.Container{
			{
				Name:         "model-downloader",
				Image:        initContainerImage,
				Command:      []string{"sh", "-c", cmd},
				Env:          env,
				VolumeMounts: initVolumeMounts,
			},
		},
		volumes:      volumes,
		volumeMounts: []corev1.VolumeMount{{Name: "model-storage", MountPath: "/models", ReadOnly: true}},
	}
}

const ModelCachePVCName = "llmkube-model-cache"

const PhaseWaitingForGPU = "WaitingForGPU"

func (r *InferenceServiceReconciler) ensureModelCachePVC(ctx context.Context, namespace string) error {
	log := logf.FromContext(ctx)

	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: ModelCachePVCName, Namespace: namespace}, pvc)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check for existing PVC: %w", err)
	}

	log.Info("Creating model cache PVC in namespace", "namespace", namespace)

	accessMode := corev1.ReadWriteOnce
	if r.ModelCacheAccessMode == "ReadWriteMany" {
		accessMode = corev1.ReadWriteMany
	}

	size := "100Gi"
	if r.ModelCacheSize != "" {
		size = r.ModelCacheSize
	}
	storageSize, err := resource.ParseQuantity(size)
	if err != nil {
		return fmt.Errorf("invalid cache size %q: %w", size, err)
	}

	newPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ModelCachePVCName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "llmkube",
				"app.kubernetes.io/component":  "model-cache",
				"app.kubernetes.io/managed-by": "llmkube-controller",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageSize,
				},
			},
		},
	}

	if r.ModelCacheClass != "" {
		newPVC.Spec.StorageClassName = &r.ModelCacheClass
	}

	if err := r.Create(ctx, newPVC); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create PVC: %w", err)
	}

	log.Info("Created model cache PVC", "namespace", namespace, "name", ModelCachePVCName)
	return nil
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get;list;watch

func (r *InferenceServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reconcileStart := time.Now()
	defer func() {
		llmkubemetrics.ReconcileDuration.WithLabelValues("inferenceservice").Observe(time.Since(reconcileStart).Seconds())
	}()

	log := logf.FromContext(ctx)

	inferenceService := &inferencev1alpha1.InferenceService{}
	if err := r.Get(ctx, req.NamespacedName, inferenceService); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get InferenceService")
		return ctrl.Result{}, err
	}

	model, modelReady, result, err := r.getModelForInferenceService(ctx, inferenceService)
	if err != nil || result != nil {
		if result != nil {
			return *result, err
		}
		return ctrl.Result{}, err
	}

	desiredReplicas := int32(1)
	if inferenceService.Spec.Replicas != nil {
		desiredReplicas = *inferenceService.Spec.Replicas
	}

	if model.Status.CacheKey != "" && r.ModelCachePath != "" {
		if err := r.ensureModelCachePVC(ctx, inferenceService.Namespace); err != nil {
			log.Error(err, "Failed to ensure model cache PVC exists", "namespace", inferenceService.Namespace)
			return r.updateStatusWithSchedulingInfo(ctx, inferenceService, "Failed", modelReady, 0, desiredReplicas, "", "Failed to create model cache PVC", nil)
		}
	}

	isMetal := model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator == "metal"

	deployment, readyReplicas, result, err := r.reconcileDeployment(ctx, inferenceService, model, desiredReplicas, modelReady, isMetal)
	if err != nil || result != nil {
		if result != nil {
			return *result, err
		}
		return ctrl.Result{}, err
	}

	service, result, err := r.reconcileService(ctx, inferenceService, modelReady, desiredReplicas, isMetal)
	if err != nil || result != nil {
		if result != nil {
			return *result, err
		}
		return ctrl.Result{}, err
	}

	endpoint := r.constructEndpoint(inferenceService, service)
	phase, schedulingInfo := r.determinePhase(ctx, inferenceService, readyReplicas, desiredReplicas, isMetal, deployment)

	return r.updateStatusWithSchedulingInfo(ctx, inferenceService, phase, modelReady, readyReplicas, desiredReplicas, endpoint, "", schedulingInfo)
}

func (r *InferenceServiceReconciler) getModelForInferenceService(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (*inferencev1alpha1.Model, bool, *ctrl.Result, error) {
	log := logf.FromContext(ctx)

	model := &inferencev1alpha1.Model{}
	modelName := types.NamespacedName{
		Name:      isvc.Spec.ModelRef,
		Namespace: isvc.Namespace,
	}
	if err := r.Get(ctx, modelName, model); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Referenced Model not found", "model", isvc.Spec.ModelRef)
			result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, "Failed", false, 0, 0, "", "Model not found", nil)
			return nil, false, &result, updateErr
		}
		log.Error(err, "Failed to get Model")
		return nil, false, nil, err
	}

	modelReady := model.Status.Phase == PhaseReady
	if !modelReady {
		log.Info("Model not ready yet", "model", model.Name, "phase", model.Status.Phase)
		result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, "Pending", false, 0, 0, "", "Waiting for Model to be Ready", nil)
		return nil, false, &result, updateErr
	}

	return model, modelReady, nil, nil
}

func (r *InferenceServiceReconciler) reconcileDeployment(ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, desiredReplicas int32, modelReady bool, isMetal bool) (*appsv1.Deployment, int32, *ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if isMetal {
		log.Info("Metal accelerator detected, skipping Deployment creation")
		return nil, desiredReplicas, nil, nil
	}

	deployment := r.constructDeployment(isvc, model, desiredReplicas)
	if err := controllerutil.SetControllerReference(isvc, deployment, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference for Deployment")
		return nil, 0, nil, err
	}

	existingDeployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, existingDeployment)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("Creating new Deployment", "name", deployment.Name)
		if err := r.Create(ctx, deployment); err != nil {
			log.Error(err, "Failed to create Deployment")
			result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, "Failed", modelReady, 0, desiredReplicas, "", "Failed to create Deployment", nil)
			return nil, 0, &result, updateErr
		}
		return deployment, 0, nil, nil
	} else if err != nil {
		log.Error(err, "Failed to get Deployment")
		return nil, 0, nil, err
	}

	existingDeployment.Spec = deployment.Spec
	if err := r.Update(ctx, existingDeployment); err != nil {
		log.Error(err, "Failed to update Deployment")
		return nil, 0, nil, err
	}

	return deployment, existingDeployment.Status.ReadyReplicas, nil, nil
}

func (r *InferenceServiceReconciler) reconcileService(ctx context.Context, isvc *inferencev1alpha1.InferenceService, modelReady bool, desiredReplicas int32, isMetal bool) (*corev1.Service, *ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if isMetal {
		log.Info("Metal accelerator detected, skipping Service creation (managed by Metal Agent)")
		// Return a minimal Service object so constructEndpoint can still build
		// the endpoint URL from the sanitized name.
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sanitizeDNSName(isvc.Name),
				Namespace: isvc.Namespace,
			},
		}, nil, nil
	}

	service := r.constructService(isvc)
	if err := controllerutil.SetControllerReference(isvc, service, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference for Service")
		return nil, nil, err
	}

	existingService := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, existingService)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("Creating new Service", "name", service.Name)
		if err := r.Create(ctx, service); err != nil {
			log.Error(err, "Failed to create Service")
			result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, "Failed", modelReady, 0, desiredReplicas, "", "Failed to create Service", nil)
			return nil, &result, updateErr
		}
	} else if err != nil {
		log.Error(err, "Failed to get Service")
		return nil, nil, err
	}

	return service, nil, nil
}

func (r *InferenceServiceReconciler) determinePhase(ctx context.Context, isvc *inferencev1alpha1.InferenceService, readyReplicas, desiredReplicas int32, isMetal bool, deployment *appsv1.Deployment) (string, *SchedulingInfo) {
	log := logf.FromContext(ctx)

	if readyReplicas == desiredReplicas && readyReplicas > 0 {
		return PhaseReady, nil
	}
	if readyReplicas > 0 {
		return "Progressing", nil
	}
	if !isMetal && deployment != nil {
		schedulingInfo, err := r.getPodSchedulingInfo(ctx, isvc)
		if err != nil {
			log.Error(err, "Failed to get pod scheduling info")
		}
		if schedulingInfo != nil && schedulingInfo.Status == "InsufficientGPU" {
			return PhaseWaitingForGPU, schedulingInfo
		}
	}
	return "Creating", nil
}

// Priority class mapping from priority level to Kubernetes PriorityClass name
var priorityClassMap = map[string]string{
	"critical": "llmkube-critical",
	"high":     "llmkube-high",
	"normal":   "llmkube-normal",
	"low":      "llmkube-low",
	"batch":    "llmkube-batch",
}

// Priority values corresponding to each level
var priorityValues = map[string]int32{
	"critical": 1000000,
	"high":     100000,
	"normal":   10000,
	"low":      1000,
	"batch":    100,
}

// SchedulingInfo contains information about pod scheduling status
type SchedulingInfo struct {
	Status     string
	Message    string
	WaitingFor string
}

func (r *InferenceServiceReconciler) getPodSchedulingInfo(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (*SchedulingInfo, error) {
	podList := &corev1.PodList{}
	labels := client.MatchingLabels{
		"app":                           isvc.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}
	if err := r.List(ctx, podList, client.InNamespace(isvc.Namespace), labels); err != nil {
		return nil, err
	}

	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodPending {
			continue
		}

		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
				info := &SchedulingInfo{
					Status:  condition.Reason,
					Message: condition.Message,
				}

				if strings.Contains(condition.Message, "Insufficient nvidia.com/gpu") {
					info.Status = "InsufficientGPU"
					gpuCount := int32(0)
					if isvc.Spec.Resources != nil && isvc.Spec.Resources.GPU > 0 {
						gpuCount = isvc.Spec.Resources.GPU
					}
					info.WaitingFor = fmt.Sprintf("nvidia.com/gpu: %d", gpuCount)
				} else if strings.Contains(condition.Message, "Insufficient") {
					info.Status = "InsufficientResources"
				}

				return info, nil
			}
		}
	}

	return nil, nil
}

func (r *InferenceServiceReconciler) calculateQueuePosition(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (int32, error) {
	if isvc.Status.Phase != PhaseWaitingForGPU {
		return 0, nil
	}

	allServices := &inferencev1alpha1.InferenceServiceList{}
	if err := r.List(ctx, allServices); err != nil {
		return 0, err
	}

	type queueEntry struct {
		name      string
		namespace string
		created   metav1.Time
	}

	var waitingServices []queueEntry
	for _, svc := range allServices.Items {
		if svc.Status.Phase == PhaseWaitingForGPU {
			waitingServices = append(waitingServices, queueEntry{
				name:      svc.Name,
				namespace: svc.Namespace,
				created:   svc.CreationTimestamp,
			})
		}
	}

	// Sort by creation timestamp (FIFO)
	for i := 0; i < len(waitingServices)-1; i++ {
		for j := i + 1; j < len(waitingServices); j++ {
			if waitingServices[j].created.Before(&waitingServices[i].created) {
				waitingServices[i], waitingServices[j] = waitingServices[j], waitingServices[i]
			}
		}
	}

	for pos, entry := range waitingServices {
		if entry.name == isvc.Name && entry.namespace == isvc.Namespace {
			return int32(pos + 1), nil
		}
	}

	return 0, nil
}

func (r *InferenceServiceReconciler) resolvePriorityClassName(isvc *inferencev1alpha1.InferenceService) string {
	if isvc.Spec.PriorityClassName != "" {
		return isvc.Spec.PriorityClassName
	}

	priority := isvc.Spec.Priority
	if priority == "" {
		priority = "normal"
	}

	if className, ok := priorityClassMap[priority]; ok {
		return className
	}

	return "llmkube-normal"
}

func (r *InferenceServiceReconciler) resolveEffectivePriority(isvc *inferencev1alpha1.InferenceService) int32 {
	priority := isvc.Spec.Priority
	if priority == "" {
		priority = "normal"
	}

	if value, ok := priorityValues[priority]; ok {
		return value
	}

	return priorityValues["normal"]
}

// calculateTensorSplit returns comma-separated equal ratios for llama.cpp --tensor-split flag
func calculateTensorSplit(gpuCount int32, _ *inferencev1alpha1.GPUShardingSpec) string {
	if gpuCount <= 1 {
		return ""
	}

	// TODO: Support custom layer splits from sharding.LayerSplit
	ratios := make([]string, gpuCount)
	for i := range ratios {
		ratios[i] = "1"
	}

	result := ratios[0]
	for i := 1; i < len(ratios); i++ {
		result = fmt.Sprintf("%s,%s", result, ratios[i])
	}

	return result
}

func appendContextSizeArgs(args []string, contextSize *int32) []string {
	if contextSize != nil && *contextSize > 0 {
		return append(args, "--ctx-size", fmt.Sprintf("%d", *contextSize))
	}
	return args
}

func appendParallelSlotsArgs(args []string, parallelSlots *int32) []string {
	if parallelSlots != nil && *parallelSlots > 1 {
		return append(args, "--parallel", fmt.Sprintf("%d", *parallelSlots))
	}
	return args
}

func appendFlashAttentionArgs(args []string, flashAttention *bool, gpuCount int32) []string {
	if gpuCount > 0 && flashAttention != nil && *flashAttention {
		return append(args, "--flash-attn", "on")
	}
	return args
}

func appendJinjaArgs(args []string, jinja *bool) []string {
	if jinja != nil && *jinja {
		return append(args, "--jinja")
	}
	return args
}

func (r *InferenceServiceReconciler) constructDeployment(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	replicas int32,
) *appsv1.Deployment {
	labels := map[string]string{
		"app":                           isvc.Name,
		"inference.llmkube.dev/model":   model.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}

	image := "ghcr.io/ggml-org/llama.cpp:server"
	if isvc.Spec.Image != "" {
		image = isvc.Spec.Image
	}

	port := int32(8080)
	if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		port = isvc.Spec.Endpoint.Port
	}

	useCache := model.Status.CacheKey != "" && r.ModelCachePath != ""
	storageConfig := buildModelStorageConfig(model, isvc.Namespace, useCache, r.CACertConfigMap, r.InitContainerImage)
	modelPath := storageConfig.modelPath

	args := []string{
		"--model", modelPath,
		"--host", "0.0.0.0",
		"--port", fmt.Sprintf("%d", port),
	}

	// Model spec takes precedence for GPU count
	gpuCount := int32(0)
	if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil && model.Spec.Hardware.GPU.Count > 0 {
		gpuCount = model.Spec.Hardware.GPU.Count
	} else if isvc.Spec.Resources != nil && isvc.Spec.Resources.GPU > 0 {
		gpuCount = isvc.Spec.Resources.GPU
	}

	if gpuCount > 0 {
		layers := int32(99)
		if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil && model.Spec.Hardware.GPU.Layers > 0 {
			layers = model.Spec.Hardware.GPU.Layers
		} else if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil && model.Spec.Hardware.GPU.Layers == -1 {
			layers = 99
		}
		args = append(args, "--n-gpu-layers", fmt.Sprintf("%d", layers))

		if gpuCount > 1 {
			args = append(args, "--split-mode", "layer")

			var sharding *inferencev1alpha1.GPUShardingSpec
			if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil {
				sharding = model.Spec.Hardware.GPU.Sharding
			}
			tensorSplit := calculateTensorSplit(gpuCount, sharding)
			args = append(args, "--tensor-split", tensorSplit)
		}
	}

	args = appendContextSizeArgs(args, isvc.Spec.ContextSize)
	args = appendParallelSlotsArgs(args, isvc.Spec.ParallelSlots)
	args = appendFlashAttentionArgs(args, isvc.Spec.FlashAttention, gpuCount)
	args = appendJinjaArgs(args, isvc.Spec.Jinja)

	// Enable Prometheus metrics endpoint on llama.cpp
	args = append(args, "--metrics")

	container := corev1.Container{
		Name:  "llama-server",
		Image: image,
		Args:  args,
		Ports: []corev1.ContainerPort{
			{
				Name:          "http",
				ContainerPort: port,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts: storageConfig.volumeMounts,
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/health",
					Port: intstr.FromInt32(port),
				},
			},
			InitialDelaySeconds: 30,
			PeriodSeconds:       15,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/health",
					Port: intstr.FromInt32(port),
				},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		},
	}

	if gpuCount > 0 {
		container.Resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				"nvidia.com/gpu": resource.MustParse(fmt.Sprintf("%d", gpuCount)),
			},
		}
	}

	if isvc.Spec.Resources != nil {
		if container.Resources.Limits == nil {
			container.Resources.Limits = corev1.ResourceList{}
		}
		if container.Resources.Requests == nil {
			container.Resources.Requests = corev1.ResourceList{}
		}
		if isvc.Spec.Resources.CPU != "" {
			container.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(isvc.Spec.Resources.CPU)
		}
		if isvc.Spec.Resources.Memory != "" {
			container.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(isvc.Spec.Resources.Memory)
		}
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      isvc.Name,
			Namespace: isvc.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					InitContainers:    storageConfig.initContainers,
					Containers:        []corev1.Container{container},
					Volumes:           storageConfig.volumes,
					PriorityClassName: r.resolvePriorityClassName(isvc),
				},
			},
		},
	}

	if gpuCount > 0 {
		tolerations := []corev1.Toleration{
			{
				Key:      "nvidia.com/gpu",
				Operator: corev1.TolerationOpEqual,
				Value:    "present",
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}

		if len(isvc.Spec.Tolerations) > 0 {
			tolerations = append(tolerations, isvc.Spec.Tolerations...)
		}

		deployment.Spec.Template.Spec.Tolerations = tolerations

		if len(isvc.Spec.NodeSelector) > 0 {
			deployment.Spec.Template.Spec.NodeSelector = isvc.Spec.NodeSelector
		}
	}

	return deployment
}

func (r *InferenceServiceReconciler) constructService(isvc *inferencev1alpha1.InferenceService) *corev1.Service {
	serviceName := sanitizeDNSName(isvc.Name)

	labels := map[string]string{
		"app":                           isvc.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}

	port := int32(8080)
	if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		port = isvc.Spec.Endpoint.Port
	}

	serviceType := corev1.ServiceTypeClusterIP
	if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Type != "" {
		switch isvc.Spec.Endpoint.Type {
		case "NodePort":
			serviceType = corev1.ServiceTypeNodePort
		case "LoadBalancer":
			serviceType = corev1.ServiceTypeLoadBalancer
		}
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: isvc.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     serviceType,
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       port,
					TargetPort: intstr.FromInt(int(port)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

func (r *InferenceServiceReconciler) constructEndpoint(isvc *inferencev1alpha1.InferenceService, svc *corev1.Service) string {
	port := int32(8080)
	path := "/v1/chat/completions"

	if isvc.Spec.Endpoint != nil {
		if isvc.Spec.Endpoint.Port > 0 {
			port = isvc.Spec.Endpoint.Port
		}
		if isvc.Spec.Endpoint.Path != "" {
			path = isvc.Spec.Endpoint.Path
		}
	}

	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d%s", svc.Name, svc.Namespace, port, path)
}

func (r *InferenceServiceReconciler) updateStatusWithSchedulingInfo(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	phase string,
	modelReady bool,
	readyReplicas int32,
	desiredReplicas int32,
	endpoint string,
	errorMsg string,
	schedulingInfo *SchedulingInfo,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	now := metav1.Now()
	previousPhase := isvc.Status.Phase
	isvc.Status.Phase = phase
	isvc.Status.ModelReady = modelReady
	isvc.Status.ReadyReplicas = readyReplicas
	isvc.Status.DesiredReplicas = desiredReplicas
	isvc.Status.Endpoint = endpoint
	isvc.Status.LastUpdated = &now

	isvc.Status.EffectivePriority = r.resolveEffectivePriority(isvc)

	// Update phase gauge metric
	llmkubemetrics.InferenceServicePhase.WithLabelValues(isvc.Name, isvc.Namespace, phase).Set(1)
	if previousPhase != "" && previousPhase != phase {
		llmkubemetrics.InferenceServicePhase.WithLabelValues(isvc.Name, isvc.Namespace, previousPhase).Set(0)
	}

	// Track time-to-ready using creation timestamp
	if phase == PhaseReady && previousPhase != PhaseReady {
		readyDuration := time.Since(isvc.CreationTimestamp.Time).Seconds()
		llmkubemetrics.InferenceServiceReadyDuration.WithLabelValues(isvc.Name, isvc.Namespace).Observe(readyDuration)
		llmkubemetrics.ReconcileTotal.WithLabelValues("inferenceservice", "success").Inc()
	}

	if schedulingInfo != nil {
		isvc.Status.SchedulingStatus = schedulingInfo.Status
		isvc.Status.SchedulingMessage = schedulingInfo.Message
		isvc.Status.WaitingFor = schedulingInfo.WaitingFor
	} else {
		isvc.Status.SchedulingStatus = ""
		isvc.Status.SchedulingMessage = ""
		isvc.Status.WaitingFor = ""
	}

	if phase == PhaseWaitingForGPU {
		queuePos, err := r.calculateQueuePosition(ctx, isvc)
		if err != nil {
			log.Error(err, "Failed to calculate queue position")
		}
		isvc.Status.QueuePosition = queuePos
		llmkubemetrics.GPUQueueDepth.Set(float64(queuePos))
	} else {
		isvc.Status.QueuePosition = 0
	}

	var condition metav1.Condition
	switch phase {
	case PhaseReady:
		condition = metav1.Condition{
			Type:               "Available",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             "InferenceReady",
			Message:            "Inference service is ready and serving requests",
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, condition)
		meta.RemoveStatusCondition(&isvc.Status.Conditions, "Progressing")
		meta.RemoveStatusCondition(&isvc.Status.Conditions, "Degraded")
		meta.RemoveStatusCondition(&isvc.Status.Conditions, "GPUAvailable")

	case "Progressing", "Creating":
		condition = metav1.Condition{
			Type:               "Progressing",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             "Creating",
			Message:            fmt.Sprintf("Creating inference service (%d/%d replicas ready)", readyReplicas, desiredReplicas),
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, condition)

	case PhaseWaitingForGPU:
		condition = metav1.Condition{
			Type:               "GPUAvailable",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             "InsufficientGPU",
			Message:            fmt.Sprintf("Waiting for GPU resources: %s", isvc.Status.WaitingFor),
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, condition)

		progressCondition := metav1.Condition{
			Type:               "Progressing",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             "WaitingForGPU",
			Message:            fmt.Sprintf("Queued at position %d waiting for GPU", isvc.Status.QueuePosition),
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, progressCondition)

	case "Failed":
		condition = metav1.Condition{
			Type:               "Degraded",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             "Failed",
			Message:            errorMsg,
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, condition)
		meta.RemoveStatusCondition(&isvc.Status.Conditions, "Available")

	case "Pending":
		condition = metav1.Condition{
			Type:               "Progressing",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             "Pending",
			Message:            errorMsg,
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, condition)
	}

	if err := r.Status().Update(ctx, isvc); err != nil {
		log.Error(err, "Failed to update InferenceService status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *InferenceServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.InferenceService{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.findInferenceServiceForPod),
		).
		Watches(
			&inferencev1alpha1.Model{},
			handler.EnqueueRequestsFromMapFunc(r.findInferenceServicesForModel),
		).
		Named("inferenceservice").
		Complete(r)
}

func (r *InferenceServiceReconciler) findInferenceServiceForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	pod := obj.(*corev1.Pod)

	serviceName, ok := pod.Labels["inference.llmkube.dev/service"]
	if !ok {
		return nil
	}

	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name:      serviceName,
				Namespace: pod.Namespace,
			},
		},
	}
}

func (r *InferenceServiceReconciler) findInferenceServicesForModel(ctx context.Context, obj client.Object) []reconcile.Request {
	model := obj.(*inferencev1alpha1.Model)

	inferenceServiceList := &inferencev1alpha1.InferenceServiceList{}
	if err := r.List(ctx, inferenceServiceList, client.InNamespace(model.Namespace)); err != nil {
		return []reconcile.Request{}
	}

	var requests []reconcile.Request
	for _, isvc := range inferenceServiceList.Items {
		if isvc.Spec.ModelRef == model.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      isvc.Name,
					Namespace: isvc.Namespace,
				},
			})
		}
	}

	return requests
}
