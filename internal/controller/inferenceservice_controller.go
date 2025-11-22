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

// InferenceServiceReconciler reconciles a InferenceService object
type InferenceServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// sanitizeDNSName converts a string to be DNS-1035 compliant by replacing dots with dashes
// DNS-1035 requires: lowercase alphanumeric or '-', start with alphabetic, end with alphanumeric
func sanitizeDNSName(name string) string {
	// Replace dots with dashes to make it DNS-1035 compliant
	return strings.ReplaceAll(name, ".", "-")
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *InferenceServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Fetch the InferenceService instance
	inferenceService := &inferencev1alpha1.InferenceService{}
	if err := r.Get(ctx, req.NamespacedName, inferenceService); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("InferenceService resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get InferenceService")
		return ctrl.Result{}, err
	}

	// 2. Check if the referenced Model exists and is Ready
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

	// Check if Model is Ready
	modelReady := model.Status.Phase == PhaseReady
	if !modelReady {
		log.Info("Model not ready yet", "model", model.Name, "phase", model.Status.Phase)
		return r.updateStatus(ctx, inferenceService, "Pending", false, 0, 0, "", "Waiting for Model to be Ready")
	}

	// 3. Set desired replicas (default to 1 if not specified)
	desiredReplicas := int32(1)
	if inferenceService.Spec.Replicas != nil {
		desiredReplicas = *inferenceService.Spec.Replicas
	}

	// 4. Check if this is a Metal accelerator deployment
	// For Metal, we skip Deployment creation as the Metal agent runs llama-server natively
	isMetal := model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator == "metal"
	var deployment *appsv1.Deployment
	var err error
	existingDeployment := &appsv1.Deployment{}

	if isMetal {
		log.Info("Metal accelerator detected, skipping Deployment creation (Metal agent handles native execution)")
	} else {
		// 4a. Create or update Deployment for non-Metal deployments
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
			// Update existing deployment if needed
			existingDeployment.Spec = deployment.Spec
			if err := r.Update(ctx, existingDeployment); err != nil {
				log.Error(err, "Failed to update Deployment")
				return ctrl.Result{}, err
			}
		}
	}

	// 5. Create or update Service
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

	// 6. Get the Deployment status for ready replicas
	readyReplicas := int32(0)
	if isMetal {
		// For Metal deployments, the Metal agent manages the native process
		// We'll assume it's ready if the Model is ready (Metal agent handles the rest)
		readyReplicas = desiredReplicas
	} else if deployment != nil {
		if err := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, existingDeployment); err == nil {
			readyReplicas = existingDeployment.Status.ReadyReplicas
		}
	}

	// 7. Construct endpoint URL
	endpoint := r.constructEndpoint(inferenceService, service)

	// 8. Determine phase
	phase := "Creating"
	if readyReplicas == desiredReplicas && readyReplicas > 0 {
		phase = "Ready"
	} else if readyReplicas > 0 {
		phase = "Progressing"
	}

	// 9. Update status
	return r.updateStatus(ctx, inferenceService, phase, modelReady, readyReplicas, desiredReplicas, endpoint, "")
}

// calculateTensorSplit computes the tensor split ratios for multi-GPU inference
// Returns a comma-separated string of ratios for llama.cpp --tensor-split flag
func calculateTensorSplit(gpuCount int32, sharding *inferencev1alpha1.GPUShardingSpec) string {
	if gpuCount <= 1 {
		return ""
	}

	// Check if custom layer split is specified in sharding config
	if sharding != nil && len(sharding.LayerSplit) > 0 {
		// Custom split ratios specified
		// Example: LayerSplit: ["0-15", "16-31"] means 50%/50% for 32-layer model
		// For now, we'll support even splits and weighted splits in the future
		// TODO: Parse LayerSplit ranges to calculate exact ratios
	}

	// Default: Even split across all GPUs
	// llama.cpp accepts integer ratios that will be normalized
	// Example: 2 GPUs -> "1,1" (50%/50%)
	//          4 GPUs -> "1,1,1,1" (25%/25%/25%/25%)
	// We use "1" for each GPU as it's clearer than percentages
	ratios := make([]string, gpuCount)
	for i := range ratios {
		ratios[i] = "1"
	}

	// Build comma-separated string
	result := ratios[0]
	for i := 1; i < len(ratios); i++ {
		result = fmt.Sprintf("%s,%s", result, ratios[i])
	}

	return result
}

// constructDeployment builds a Deployment for the InferenceService
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

	// Get image from spec or use default
	image := "ghcr.io/ggerganov/llama.cpp:server"
	if isvc.Spec.Image != "" {
		image = isvc.Spec.Image
	}

	// Get endpoint port
	port := int32(8080)
	if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		port = isvc.Spec.Endpoint.Port
	}

	// Construct model filename from source URL
	modelFileName := fmt.Sprintf("%s-%s.gguf", isvc.Namespace, model.Name)
	modelPath := fmt.Sprintf("/models/%s", modelFileName)

	// Build init container to download the model
	initContainer := corev1.Container{
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
	}

	// Build container args
	args := []string{
		"--model", modelPath,
		"--host", "0.0.0.0",
		"--port", fmt.Sprintf("%d", port),
	}

	// Determine GPU count from either Model or InferenceService spec
	// Model spec takes precedence as it defines the hardware requirements
	gpuCount := int32(0)
	if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil && model.Spec.Hardware.GPU.Count > 0 {
		gpuCount = model.Spec.Hardware.GPU.Count
	} else if isvc.Spec.Resources != nil && isvc.Spec.Resources.GPU > 0 {
		gpuCount = isvc.Spec.Resources.GPU
	}

	// Add GPU configuration if GPU is requested
	if gpuCount > 0 {
		// Determine number of layers to offload
		layers := int32(99) // default: offload all layers (max available)
		if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil && model.Spec.Hardware.GPU.Layers > 0 {
			// Use specified layer count from Model
			layers = model.Spec.Hardware.GPU.Layers
		} else if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil && model.Spec.Hardware.GPU.Layers == -1 {
			// -1 means auto-detect (use 99 for llama.cpp)
			layers = 99
		}
		args = append(args, "--n-gpu-layers", fmt.Sprintf("%d", layers))

		// Add multi-GPU configuration if using more than 1 GPU
		if gpuCount > 1 {
			// Set split mode to layer-based sharding
			args = append(args, "--split-mode", "layer")

			// Calculate and add tensor split ratios
			var sharding *inferencev1alpha1.GPUShardingSpec
			if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil {
				sharding = model.Spec.Hardware.GPU.Sharding
			}
			tensorSplit := calculateTensorSplit(gpuCount, sharding)
			args = append(args, "--tensor-split", tensorSplit)
		}
	}

	// Build container
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
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "model-storage",
				MountPath: "/models",
				ReadOnly:  true,
			},
		},
	}

	// Add GPU resource requirements if specified
	// Use the gpuCount variable calculated earlier (from Model or InferenceService spec)
	if gpuCount > 0 {
		container.Resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				"nvidia.com/gpu": resource.MustParse(fmt.Sprintf("%d", gpuCount)),
			},
		}
	}

	// Add CPU/Memory if specified
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
					InitContainers: []corev1.Container{initContainer},
					Containers:     []corev1.Container{container},
					Volumes: []corev1.Volume{
						{
							Name: "model-storage",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}

	// Add GPU tolerations and node selector if GPU is requested
	// Use the gpuCount variable calculated earlier
	if gpuCount > 0 {
		// Start with base NVIDIA toleration (works on all clouds)
		tolerations := []corev1.Toleration{
			{
				Key:      "nvidia.com/gpu",
				Operator: corev1.TolerationOpEqual,
				Value:    "present",
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}

		// Merge in user-provided tolerations from InferenceService spec
		// This allows cloud-specific tolerations (e.g., spot instances, preemptible VMs)
		if len(isvc.Spec.Tolerations) > 0 {
			tolerations = append(tolerations, isvc.Spec.Tolerations...)
		}

		deployment.Spec.Template.Spec.Tolerations = tolerations

		// Apply user-provided node selector from InferenceService spec
		// This allows cloud-specific node selection (e.g., specific node pools)
		if len(isvc.Spec.NodeSelector) > 0 {
			deployment.Spec.Template.Spec.NodeSelector = isvc.Spec.NodeSelector
		}
	}

	return deployment
}

// constructService builds a Service for the InferenceService
func (r *InferenceServiceReconciler) constructService(isvc *inferencev1alpha1.InferenceService) *corev1.Service {
	// Sanitize the service name to be DNS-1035 compliant (replace dots with dashes)
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

// constructEndpoint builds the endpoint URL for the InferenceService
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

	// For ClusterIP, use internal DNS name
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d%s", svc.Name, svc.Namespace, port, path)
}

// updateStatus updates the InferenceService status
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

	// Set conditions based on phase
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

// SetupWithManager sets up the controller with the Manager.
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

// findInferenceServicesForModel finds all InferenceServices that reference a given Model
func (r *InferenceServiceReconciler) findInferenceServicesForModel(ctx context.Context, obj client.Object) []reconcile.Request {
	model := obj.(*inferencev1alpha1.Model)

	// List all InferenceServices in the same namespace
	inferenceServiceList := &inferencev1alpha1.InferenceServiceList{}
	if err := r.List(ctx, inferenceServiceList, client.InNamespace(model.Namespace)); err != nil {
		return []reconcile.Request{}
	}

	// Find InferenceServices that reference this Model
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
