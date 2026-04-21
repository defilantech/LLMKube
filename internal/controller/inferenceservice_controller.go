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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
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
	Recorder             events.EventRecorder
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

func boolPtr(b bool) *bool { return &b }

func inferPodSecurityContext(isvc *inferencev1alpha1.InferenceService) *corev1.PodSecurityContext {
	if isvc.Spec.PodSecurityContext != nil {
		return isvc.Spec.PodSecurityContext
	}
	return &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

func inferContainerSecurityContext(isvc *inferencev1alpha1.InferenceService) *corev1.SecurityContext {
	if isvc.Spec.SecurityContext != nil {
		return isvc.Spec.SecurityContext
	}
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// initContainerSecurityContext sets up security context for the model downloader init container.
// It inherits runAsUser/runAsGroup from podSecurityContext if specified by the user.
// Users MUST specify runAsUser/runAsGroup in their InferenceService podSecurityContext to avoid
// permission denied errors on the model volume.
func initContainerSecurityContext(isvc *inferencev1alpha1.InferenceService) *corev1.SecurityContext {
	sc := &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}

	// Inherit runAsUser/runAsGroup from podSecurityContext if specified
	if isvc != nil && isvc.Spec.PodSecurityContext != nil {
		if isvc.Spec.PodSecurityContext.RunAsUser != nil {
			sc.RunAsUser = isvc.Spec.PodSecurityContext.RunAsUser
		}
		if isvc.Spec.PodSecurityContext.RunAsGroup != nil {
			sc.RunAsGroup = isvc.Spec.PodSecurityContext.RunAsGroup
		}
	}

	return sc
}

// isLocalModelSource delegates to the shared isLocalSource helper in source.go.
func isLocalModelSource(source string) bool {
	return isLocalSource(source)
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

func buildModelStorageConfig(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService, namespace string, useCache bool, caCertConfigMap string, initContainerImage string) modelStorageConfig {
	if isPVCSource(model.Spec.Source) {
		return buildPVCStorageConfig(model)
	}
	if useCache {
		return buildCachedStorageConfig(model, isvc, caCertConfigMap, initContainerImage)
	}
	return buildEmptyDirStorageConfig(model, isvc, namespace, caCertConfigMap, initContainerImage)
}

// buildPVCStorageConfig mounts the user's PVC directly as a read-only volume.
// No init container is needed since the model is already on the PVC.
func buildPVCStorageConfig(model *inferencev1alpha1.Model) modelStorageConfig {
	claimName, modelFilePath, _ := parsePVCSource(model.Spec.Source)

	modelPath := fmt.Sprintf("/model-source/%s", modelFilePath)

	return modelStorageConfig{
		modelPath: modelPath,
		volumes: []corev1.Volume{
			{
				Name: "model-source",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: claimName,
						ReadOnly:  true,
					},
				},
			},
		},
		volumeMounts: []corev1.VolumeMount{
			{Name: "model-source", MountPath: "/model-source", ReadOnly: true},
		},
	}
}

func buildCachedStorageConfig(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService, caCertConfigMap string, initContainerImage string) modelStorageConfig {
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
				Name:            "model-downloader",
				Image:           initContainerImage,
				Command:         []string{"sh", "-c", cmd},
				Env:             env,
				VolumeMounts:    initVolumeMounts,
				SecurityContext: initContainerSecurityContext(isvc),
			},
		},
		volumes:      volumes,
		volumeMounts: []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models", ReadOnly: true}},
	}
}

func buildEmptyDirStorageConfig(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService, namespace string, caCertConfigMap string, initContainerImage string) modelStorageConfig {
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
				Name:            "model-downloader",
				Image:           initContainerImage,
				Command:         []string{"sh", "-c", cmd},
				Env:             env,
				VolumeMounts:    initVolumeMounts,
				SecurityContext: initContainerSecurityContext(isvc),
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
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

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
			return r.updateStatusWithSchedulingInfo(ctx, inferenceService, PhaseFailed, modelReady, 0, desiredReplicas, "", "Failed to create model cache PVC", nil)
		}
	}

	isMetal := model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator == "metal"

	if r.Recorder != nil && needsOffloadMemoryWarning(inferenceService) {
		r.Recorder.Eventf(inferenceService, nil, corev1.EventTypeWarning, "MissingMemoryRequest", "Reconcile",
			"CPU/KV offloading is enabled but resources.memory/hostMemory is not set; hybrid pods consume significant host RAM")
	}

	if r.Recorder != nil && model.Status.Phase == PhaseReady && model.Status.Path == "" && !needsSkipModelInit(inferenceService) {
		r.Recorder.Eventf(inferenceService, nil, corev1.EventTypeWarning, "MissingSkipModelInit", "Reconcile",
			"Model source is runtime-resolved but spec.skipModelInit is not set; init container will fail")
	}

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

	if err := r.reconcileHPA(ctx, inferenceService, inferenceService.Name, isMetal); err != nil {
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
			result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, false, 0, 0, "", "Model not found", nil)
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

	// Surface non-fatal vLLM spec problems as a status condition before we
	// build the Deployment. A failure here never blocks reconciliation — the
	// Deployment is still produced with the offending flags silently skipped
	// (see VLLMBackend.BuildArgs).
	r.reconcileVLLMSpecCondition(isvc)

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
			result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, desiredReplicas, "", "Failed to create Deployment", nil)
			return nil, 0, &result, updateErr
		}
		return deployment, 0, nil, nil
	} else if err != nil {
		log.Error(err, "Failed to get Deployment")
		return nil, 0, nil, err
	}

	existingDeployment.Spec = deployment.Spec
	// When autoscaling is enabled, let the HPA manage replicas
	if isvc.Spec.Autoscaling != nil {
		existingDeployment.Spec.Replicas = nil
	}
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
			result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, desiredReplicas, "", "Failed to create Service", nil)
			return nil, &result, updateErr
		}
	} else if err != nil {
		log.Error(err, "Failed to get Service")
		return nil, nil, err
	}

	return service, nil, nil
}

func (r *InferenceServiceReconciler) reconcileHPA(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	deploymentName string,
	isMetal bool,
) error {
	logger := logf.FromContext(ctx)
	hpaName := types.NamespacedName{
		Name:      isvc.Name,
		Namespace: isvc.Namespace,
	}

	// If autoscaling is not configured, clean up any existing HPA
	if isvc.Spec.Autoscaling == nil {
		existingHPA := &autoscalingv2.HorizontalPodAutoscaler{}
		if err := r.Get(ctx, hpaName, existingHPA); err == nil {
			logger.Info("Autoscaling removed, deleting HPA",
				"name", isvc.Name)
			if err := r.Delete(ctx, existingHPA); err != nil {
				return fmt.Errorf("failed to delete HPA: %w", err)
			}
		}
		return nil
	}

	// Skip HPA for Metal accelerator (no Deployment to scale)
	if isMetal {
		logger.Info("Skipping HPA for Metal accelerator workload")
		return nil
	}

	hpa := r.constructHPA(isvc, deploymentName)

	if err := controllerutil.SetControllerReference(
		isvc, hpa, r.Scheme,
	); err != nil {
		return fmt.Errorf(
			"failed to set controller reference on HPA: %w", err,
		)
	}

	existingHPA := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := r.Get(ctx, hpaName, existingHPA); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Creating HPA",
				"name", isvc.Name,
				"maxReplicas", isvc.Spec.Autoscaling.MaxReplicas)
			return r.Create(ctx, hpa)
		}
		return err
	}

	// Update existing HPA
	existingHPA.Spec = hpa.Spec
	return r.Update(ctx, existingHPA)
}

func (r *InferenceServiceReconciler) constructHPA(
	isvc *inferencev1alpha1.InferenceService,
	deploymentName string,
) *autoscalingv2.HorizontalPodAutoscaler {
	autoscaling := isvc.Spec.Autoscaling

	minReplicas := int32(1)
	if autoscaling.MinReplicas != nil {
		minReplicas = *autoscaling.MinReplicas
	}

	// Build metrics list
	var metrics []autoscalingv2.MetricSpec

	if len(autoscaling.Metrics) == 0 {
		// Use the runtime's default metric if available
		backend := resolveBackend(isvc)
		metricName := "llamacpp:requests_processing"
		if hp, ok := backend.(HPAMetricProvider); ok && hp.DefaultHPAMetric() != "" {
			metricName = hp.DefaultHPAMetric()
		}
		targetValue := resource.MustParse("2")
		metrics = []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.PodsMetricSourceType,
				Pods: &autoscalingv2.PodsMetricSource{
					Metric: autoscalingv2.MetricIdentifier{
						Name: metricName,
					},
					Target: autoscalingv2.MetricTarget{
						Type:         autoscalingv2.AverageValueMetricType,
						AverageValue: &targetValue,
					},
				},
			},
		}
	} else {
		for _, m := range autoscaling.Metrics {
			switch m.Type {
			case "Pods":
				var target autoscalingv2.MetricTarget
				if m.TargetAverageValue != nil {
					val := resource.MustParse(*m.TargetAverageValue)
					target = autoscalingv2.MetricTarget{
						Type:         autoscalingv2.AverageValueMetricType,
						AverageValue: &val,
					}
				}
				metrics = append(metrics, autoscalingv2.MetricSpec{
					Type: autoscalingv2.PodsMetricSourceType,
					Pods: &autoscalingv2.PodsMetricSource{
						Metric: autoscalingv2.MetricIdentifier{
							Name: m.Name,
						},
						Target: target,
					},
				})
			case "Resource":
				if m.TargetAverageUtilization != nil {
					metrics = append(
						metrics,
						autoscalingv2.MetricSpec{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								Name: corev1.ResourceName(m.Name),
								Target: autoscalingv2.MetricTarget{
									Type:               autoscalingv2.UtilizationMetricType,
									AverageUtilization: m.TargetAverageUtilization,
								},
							},
						},
					)
				}
			}
		}
	}

	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      isvc.Name,
			Namespace: isvc.Namespace,
			Labels: map[string]string{
				"app":                           isvc.Name,
				"inference.llmkube.dev/service": isvc.Name,
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
			MinReplicas: &minReplicas,
			MaxReplicas: autoscaling.MaxReplicas,
			Metrics:     metrics,
		},
	}
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
			return int32(pos + 1), nil //nolint:gosec // G115: queue index bounded by waitingServices size
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

func needsOffloadMemoryWarning(isvc *inferencev1alpha1.InferenceService) bool {
	needsRAM := (isvc.Spec.MoeCPUOffload != nil && *isvc.Spec.MoeCPUOffload) ||
		(isvc.Spec.NoKvOffload != nil && *isvc.Spec.NoKvOffload)
	memorySet := isvc.Spec.Resources != nil && (isvc.Spec.Resources.Memory != "" || isvc.Spec.Resources.HostMemory != "")
	return needsRAM && !memorySet
}

func needsSkipModelInit(isvc *inferencev1alpha1.InferenceService) bool {
	return isvc.Spec.SkipModelInit != nil && *isvc.Spec.SkipModelInit
}

// reconcileVLLMSpecCondition sets or clears the VLLMSpecValid status condition
// based on ValidateVLLMConfig. This is informational only — it does not block
// Deployment creation. The controller's main Status().Update at the end of
// reconcile persists the condition.
func (r *InferenceServiceReconciler) reconcileVLLMSpecCondition(isvc *inferencev1alpha1.InferenceService) {
	if isvc.Spec.Runtime != RuntimeVLLM {
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionVLLMSpecValid)
		return
	}
	reason, message := ValidateVLLMConfig(isvc)
	now := metav1.NewTime(time.Now())
	if reason == "" {
		// Only emit a True condition when we previously set a False one; no
		// need to churn the status for services that have always been valid.
		if existing := meta.FindStatusCondition(isvc.Status.Conditions, ConditionVLLMSpecValid); existing != nil {
			meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
				Type:               ConditionVLLMSpecValid,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: isvc.Generation,
				LastTransitionTime: now,
				Reason:             "ConfigValid",
				Message:            "vLLM configuration is valid",
			})
		}
		return
	}
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionVLLMSpecValid,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: isvc.Generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
}

func (r *InferenceServiceReconciler) constructDeployment(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	replicas int32,
) *appsv1.Deployment {
	backend := resolveBackend(isvc)

	labels := map[string]string{
		"app":                           isvc.Name,
		"inference.llmkube.dev/model":   model.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}

	image := backend.DefaultImage()
	if isvc.Spec.Image != "" {
		image = isvc.Spec.Image
	}

	port := backend.DefaultPort()
	if isvc.Spec.ContainerPort != nil {
		port = *isvc.Spec.ContainerPort
	} else if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		port = isvc.Spec.Endpoint.Port
	}

	skipInit := isvc.Spec.SkipModelInit != nil && *isvc.Spec.SkipModelInit

	var storageConfig modelStorageConfig
	var modelPath string
	if backend.NeedsModelInit() && !skipInit {
		useCache := model.Status.CacheKey != "" && r.ModelCachePath != ""
		storageConfig = buildModelStorageConfig(model, isvc, isvc.Namespace, useCache, r.CACertConfigMap, r.InitContainerImage)
		modelPath = storageConfig.modelPath
	}

	args := backend.BuildArgs(isvc, model, modelPath, port)

	startupProbe, livenessProbe, readinessProbe := backend.BuildProbes(port)
	if isvc.Spec.ProbeOverrides != nil {
		if isvc.Spec.ProbeOverrides.Startup != nil {
			startupProbe = isvc.Spec.ProbeOverrides.Startup
		}
		if isvc.Spec.ProbeOverrides.Liveness != nil {
			livenessProbe = isvc.Spec.ProbeOverrides.Liveness
		}
		if isvc.Spec.ProbeOverrides.Readiness != nil {
			readinessProbe = isvc.Spec.ProbeOverrides.Readiness
		}
	}

	container := corev1.Container{
		Name:            backend.ContainerName(),
		Image:           image,
		SecurityContext: inferContainerSecurityContext(isvc),
		Ports: []corev1.ContainerPort{
			{
				Name:          "http",
				ContainerPort: port,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts:   storageConfig.volumeMounts,
		StartupProbe:   startupProbe,
		LivenessProbe:  livenessProbe,
		ReadinessProbe: readinessProbe,
	}

	// Set command/args based on runtime
	if len(isvc.Spec.Command) > 0 {
		container.Command = isvc.Spec.Command
	} else if cb, ok := backend.(CommandBuilder); ok {
		container.Command = cb.BuildCommand()
	}
	if args != nil {
		container.Args = args
	}

	// Add runtime-generated env vars, then user-specified env vars (user wins on conflict)
	if eb, ok := backend.(EnvBuilder); ok {
		container.Env = append(container.Env, eb.BuildEnv(isvc)...)
	}
	if len(isvc.Spec.Env) > 0 {
		container.Env = append(container.Env, isvc.Spec.Env...)
	}

	gpuCount := resolveGPUCount(isvc, model)

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
		if isvc.Spec.Resources.HostMemory != "" {
			container.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(isvc.Spec.Resources.HostMemory)
		} else if isvc.Spec.Resources.Memory != "" {
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
					SecurityContext:   inferPodSecurityContext(isvc),
					InitContainers:    storageConfig.initContainers,
					Containers:        []corev1.Container{container},
					Volumes:           storageConfig.volumes,
					PriorityClassName: r.resolvePriorityClassName(isvc),
					ImagePullSecrets:  isvc.Spec.ImagePullSecrets,
				},
			},
		},
	}

	if gpuCount > 0 {
		// Use Recreate strategy for GPU workloads to prevent deadlock:
		// RollingUpdate requires the new pod to be Ready before terminating the old,
		// but the new pod cannot schedule if the old pod holds the only available GPU(s).
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RecreateDeploymentStrategyType,
		}

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

// nolint:unparam // ctrl.Result is always zero but callers take &result to signal status-update path
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

	case PhaseFailed:
		condition = metav1.Condition{
			Type:               ConditionDegraded,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             PhaseFailed,
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
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
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
