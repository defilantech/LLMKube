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
)

type InferenceServiceReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	ModelCachePath       string
	ModelCacheSize       string
	ModelCacheClass      string
	ModelCacheAccessMode string
}

func sanitizeDNSName(name string) string {
	return strings.ReplaceAll(name, ".", "-")
}

const ModelCachePVCName = "llmkube-model-cache"

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

func (r *InferenceServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	inferenceService := &inferencev1alpha1.InferenceService{}
	if err := r.Get(ctx, req.NamespacedName, inferenceService); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get InferenceService")
		return ctrl.Result{}, err
	}

	model := &inferencev1alpha1.Model{}
	modelName := types.NamespacedName{
		Name:      inferenceService.Spec.ModelRef,
		Namespace: inferenceService.Namespace,
	}
	if err := r.Get(ctx, modelName, model); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Referenced Model not found", "model", inferenceService.Spec.ModelRef)
			return r.updateStatus(ctx, inferenceService, "Failed", false, 0, 0, "", "Model not found")
		}
		log.Error(err, "Failed to get Model")
		return ctrl.Result{}, err
	}

	modelReady := model.Status.Phase == PhaseReady
	if !modelReady {
		log.Info("Model not ready yet", "model", model.Name, "phase", model.Status.Phase)
		return r.updateStatus(ctx, inferenceService, "Pending", false, 0, 0, "", "Waiting for Model to be Ready")
	}

	desiredReplicas := int32(1)
	if inferenceService.Spec.Replicas != nil {
		desiredReplicas = *inferenceService.Spec.Replicas
	}

	if model.Status.CacheKey != "" && r.ModelCachePath != "" {
		if err := r.ensureModelCachePVC(ctx, inferenceService.Namespace); err != nil {
			log.Error(err, "Failed to ensure model cache PVC exists", "namespace", inferenceService.Namespace)
			return r.updateStatus(ctx, inferenceService, "Failed", modelReady, 0, desiredReplicas, "", "Failed to create model cache PVC")
		}
	}

	// Metal accelerator uses native llama-server via Metal agent instead of k8s Deployment
	isMetal := model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator == "metal"
	var deployment *appsv1.Deployment
	var err error
	existingDeployment := &appsv1.Deployment{}

	if isMetal {
		log.Info("Metal accelerator detected, skipping Deployment creation")
	} else {
		deployment = r.constructDeployment(inferenceService, model, desiredReplicas)
		if err := controllerutil.SetControllerReference(inferenceService, deployment, r.Scheme); err != nil {
			log.Error(err, "Failed to set controller reference for Deployment")
			return ctrl.Result{}, err
		}

		err = r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, existingDeployment)
		if err != nil && apierrors.IsNotFound(err) {
			log.Info("Creating new Deployment", "name", deployment.Name)
			if err := r.Create(ctx, deployment); err != nil {
				log.Error(err, "Failed to create Deployment")
				return r.updateStatus(ctx, inferenceService, "Failed", modelReady, 0, desiredReplicas, "", "Failed to create Deployment")
			}
		} else if err != nil {
			log.Error(err, "Failed to get Deployment")
			return ctrl.Result{}, err
		} else {
			existingDeployment.Spec = deployment.Spec
			if err := r.Update(ctx, existingDeployment); err != nil {
				log.Error(err, "Failed to update Deployment")
				return ctrl.Result{}, err
			}
		}
	}

	service := r.constructService(inferenceService)
	if err := controllerutil.SetControllerReference(inferenceService, service, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference for Service")
		return ctrl.Result{}, err
	}

	existingService := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, existingService)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("Creating new Service", "name", service.Name)
		if err := r.Create(ctx, service); err != nil {
			log.Error(err, "Failed to create Service")
			return r.updateStatus(ctx, inferenceService, "Failed", modelReady, 0, desiredReplicas, "", "Failed to create Service")
		}
	} else if err != nil {
		log.Error(err, "Failed to get Service")
		return ctrl.Result{}, err
	}

	readyReplicas := int32(0)
	if isMetal {
		// Metal agent manages native process, assume ready if Model is ready
		readyReplicas = desiredReplicas
	} else if deployment != nil {
		if err := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, existingDeployment); err == nil {
			readyReplicas = existingDeployment.Status.ReadyReplicas
		}
	}

	endpoint := r.constructEndpoint(inferenceService, service)

	phase := "Creating"
	if readyReplicas == desiredReplicas && readyReplicas > 0 {
		phase = "Ready"
	} else if readyReplicas > 0 {
		phase = "Progressing"
	}

	return r.updateStatus(ctx, inferenceService, phase, modelReady, readyReplicas, desiredReplicas, endpoint, "")
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

	image := "ghcr.io/ggerganov/llama.cpp:server"
	if isvc.Spec.Image != "" {
		image = isvc.Spec.Image
	}

	port := int32(8080)
	if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		port = isvc.Spec.Endpoint.Port
	}

	useCache := model.Status.CacheKey != "" && r.ModelCachePath != ""
	var modelPath string
	var initContainers []corev1.Container
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount

	if useCache {
		cacheDir := fmt.Sprintf("/models/%s", model.Status.CacheKey)
		modelPath = fmt.Sprintf("%s/model.gguf", cacheDir)

		initContainers = []corev1.Container{
			{
				Name:  "model-downloader",
				Image: "curlimages/curl:latest",
				Command: []string{
					"sh",
					"-c",
					fmt.Sprintf(
						"mkdir -p %s && if [ ! -f %s ]; then echo 'Downloading model from %s...'; curl -L -o %s '%s' && echo 'Model downloaded successfully'; else echo 'Model already cached, skipping download'; fi",
						cacheDir,
						modelPath,
						model.Spec.Source,
						modelPath,
						model.Spec.Source,
					),
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "model-cache",
						MountPath: "/models",
					},
				},
			},
		}

		volumes = []corev1.Volume{
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
		volumeMounts = []corev1.VolumeMount{
			{
				Name:      "model-cache",
				MountPath: "/models",
				ReadOnly:  true,
			},
		}
	} else {
		modelFileName := fmt.Sprintf("%s-%s.gguf", isvc.Namespace, model.Name)
		modelPath = fmt.Sprintf("/models/%s", modelFileName)

		initContainers = []corev1.Container{
			{
				Name:  "model-downloader",
				Image: "curlimages/curl:latest",
				Command: []string{
					"sh",
					"-c",
					fmt.Sprintf(
						"if [ ! -f %s ]; then echo 'Downloading model from %s...'; curl -L -o %s '%s' && echo 'Model downloaded successfully'; else echo 'Model already exists, skipping download'; fi",
						modelPath,
						model.Spec.Source,
						modelPath,
						model.Spec.Source,
					),
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "model-storage",
						MountPath: "/models",
					},
				},
			},
		}
		volumes = []corev1.Volume{
			{
				Name: "model-storage",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		}
		volumeMounts = []corev1.VolumeMount{
			{
				Name:      "model-storage",
				MountPath: "/models",
				ReadOnly:  true,
			},
		}
	}

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
		VolumeMounts: volumeMounts,
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
					InitContainers: initContainers,
					Containers:     []corev1.Container{container},
					Volumes:        volumes,
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

func (r *InferenceServiceReconciler) updateStatus(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	phase string,
	modelReady bool,
	readyReplicas int32,
	desiredReplicas int32,
	endpoint string,
	errorMsg string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	now := metav1.Now()
	isvc.Status.Phase = phase
	isvc.Status.ModelReady = modelReady
	isvc.Status.ReadyReplicas = readyReplicas
	isvc.Status.DesiredReplicas = desiredReplicas
	isvc.Status.Endpoint = endpoint
	isvc.Status.LastUpdated = &now

	var condition metav1.Condition
	switch phase {
	case "Ready":
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
			&inferencev1alpha1.Model{},
			handler.EnqueueRequestsFromMapFunc(r.findInferenceServicesForModel),
		).
		Named("inferenceservice").
		Complete(r)
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
