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

package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

type deployOptions struct {
	name         string
	namespace    string
	modelSource  string
	modelFormat  string
	quantization string
	replicas     int32
	accelerator  string
	gpu          bool
	gpuCount     int32
	gpuLayers    int32
	gpuMemory    string
	gpuVendor    string
	cpu          string
	memory       string
	image        string
	wait         bool
	timeout      time.Duration
}

// NewDeployCommand creates the deploy command
func NewDeployCommand() *cobra.Command {
	opts := &deployOptions{}

	cmd := &cobra.Command{
		Use:   "deploy [MODEL_NAME]",
		Short: "Deploy a local LLM inference service",
		Long: `Deploy a local LLM inference service to Kubernetes.

This command creates both a Model resource (to download and manage the model)
and an InferenceService resource (to serve the model via an OpenAI-compatible API).

Examples:
  # Deploy Phi-3 mini with CPU (default)
  llmkube deploy phi-3-mini \
    --source https://huggingface.co/microsoft/Phi-3-mini-4k-instruct-gguf/resolve/main/Phi-3-mini-4k-instruct-q4.gguf

  # Deploy Llama 3B with GPU acceleration (simple - recommended)
  llmkube deploy llama-3b --gpu \
    --source https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q8_0.gguf

  # Deploy with full GPU configuration
  llmkube deploy llama-7b --gpu \
    --source <url> \
    --gpu-count 2 \
    --gpu-layers 32 \
    --gpu-memory 16Gi \
    --quantization Q4_K_M

  # Deploy with specific resource requirements
  llmkube deploy mistral-7b \
    --source <url> \
    --cpu 4 \
    --memory 8Gi \
    --replicas 2
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			return runDeploy(opts)
		},
	}

	// Flags
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().StringVarP(&opts.modelSource, "source", "s", "", "Model source URL (GGUF format, required)")
	cmd.Flags().StringVar(&opts.modelFormat, "format", "gguf", "Model format")
	cmd.Flags().StringVarP(&opts.quantization, "quantization", "q", "", "Model quantization (e.g., Q4_K_M, Q8_0)")
	cmd.Flags().Int32VarP(&opts.replicas, "replicas", "r", 1, "Number of replicas")

	// GPU flags
	cmd.Flags().BoolVar(&opts.gpu, "gpu", false, "Enable GPU acceleration (auto-detects CUDA image)")
	cmd.Flags().StringVar(&opts.accelerator, "accelerator", "",
		"Hardware accelerator (cpu, metal, cuda, rocm) - auto-detected if --gpu is set")
	cmd.Flags().Int32Var(&opts.gpuCount, "gpu-count", 1, "Number of GPUs per pod")
	cmd.Flags().Int32Var(&opts.gpuLayers, "gpu-layers", -1,
		"Number of model layers to offload to GPU (-1 = all layers, 0 = auto)")
	cmd.Flags().StringVar(&opts.gpuMemory, "gpu-memory", "", "GPU memory request (e.g., '8Gi', '16Gi')")
	cmd.Flags().StringVar(&opts.gpuVendor, "gpu-vendor", "nvidia", "GPU vendor (nvidia, amd, intel)")

	// Resource flags
	cmd.Flags().StringVar(&opts.cpu, "cpu", "2", "CPU request (e.g., '2' or '2000m')")
	cmd.Flags().StringVar(&opts.memory, "memory", "4Gi", "Memory request (e.g., '4Gi')")
	cmd.Flags().StringVar(&opts.image, "image", "", "Custom llama.cpp server image (auto-detected based on --gpu)")

	// Behavior flags
	cmd.Flags().BoolVarP(&opts.wait, "wait", "w", true, "Wait for deployment to be ready")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 10*time.Minute, "Timeout for waiting")

	// Required flags
	if err := cmd.MarkFlagRequired("source"); err != nil {
		// This should never happen in practice as "source" is a valid flag
		panic(fmt.Sprintf("failed to mark source flag as required: %v", err))
	}

	return cmd
}

func runDeploy(opts *deployOptions) error {
	ctx := context.Background()

	// Auto-detect accelerator and image based on GPU flag
	if opts.gpu {
		if opts.accelerator == "" {
			opts.accelerator = "cuda"
			fmt.Printf("ℹ️  Auto-detected accelerator: %s\n", opts.accelerator)
		}
		if opts.image == "" {
			opts.image = "ghcr.io/ggerganov/llama.cpp:server-cuda"
			fmt.Printf("ℹ️  Auto-detected image: %s\n", opts.image)
		}
	} else {
		if opts.accelerator == "" {
			opts.accelerator = "cpu"
		}
		if opts.image == "" {
			opts.image = "ghcr.io/ggerganov/llama.cpp:server"
		}
	}

	// Get Kubernetes client
	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	// Register our custom types
	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	// Print deployment summary
	fmt.Printf("\n🚀 Deploying LLM inference service\n")
	fmt.Printf("═══════════════════════════════════════════════\n")
	fmt.Printf("Name:        %s\n", opts.name)
	fmt.Printf("Namespace:   %s\n", opts.namespace)
	fmt.Printf("Accelerator: %s\n", opts.accelerator)
	if opts.gpu {
		fmt.Printf("GPU:         %d x %s (layers: %d)\n", opts.gpuCount, opts.gpuVendor, opts.gpuLayers)
	}
	fmt.Printf("Replicas:    %d\n", opts.replicas)
	fmt.Printf("Image:       %s\n", opts.image)
	fmt.Printf("═══════════════════════════════════════════════\n\n")

	// Create Model resource
	fmt.Printf("📦 Creating Model '%s'...\n", opts.name)
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.name,
			Namespace: opts.namespace,
		},
		Spec: inferencev1alpha1.ModelSpec{
			Source:       opts.modelSource,
			Format:       opts.modelFormat,
			Quantization: opts.quantization,
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: opts.accelerator,
			},
			Resources: &inferencev1alpha1.ResourceRequirements{
				CPU:    opts.cpu,
				Memory: opts.memory,
			},
		},
	}

	// Add GPU config if enabled
	if opts.gpu {
		model.Spec.Hardware.GPU = &inferencev1alpha1.GPUSpec{
			Enabled: true,
			Count:   opts.gpuCount,
			Vendor:  opts.gpuVendor,
		}

		// Set GPU layers if specified
		if opts.gpuLayers != 0 {
			model.Spec.Hardware.GPU.Layers = opts.gpuLayers
		}

		// Set GPU memory if specified
		if opts.gpuMemory != "" {
			model.Spec.Hardware.GPU.Memory = opts.gpuMemory
		}
	}

	if err := k8sClient.Create(ctx, model); err != nil {
		return fmt.Errorf("failed to create Model: %w", err)
	}
	fmt.Printf("   ✅ Model created\n\n")

	// Create InferenceService resource
	fmt.Printf("⚙️  Creating InferenceService '%s'...\n", opts.name)
	inferenceService := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.name,
			Namespace: opts.namespace,
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: opts.name,
			Replicas: &opts.replicas,
			Image:    opts.image,
			Endpoint: &inferencev1alpha1.EndpointSpec{
				Port: 8080,
				Path: "/v1/chat/completions",
				Type: "ClusterIP",
			},
			Resources: &inferencev1alpha1.InferenceResourceRequirements{
				CPU:    opts.cpu,
				Memory: opts.memory,
			},
		},
	}

	// Add GPU resources if enabled
	if opts.gpu {
		inferenceService.Spec.Resources.GPU = opts.gpuCount
		if opts.gpuMemory != "" {
			inferenceService.Spec.Resources.GPUMemory = opts.gpuMemory
		}
	}

	if err := k8sClient.Create(ctx, inferenceService); err != nil {
		return fmt.Errorf("failed to create InferenceService: %w", err)
	}
	fmt.Printf("   ✅ InferenceService created\n")

	// Wait for resources to be ready if requested
	if opts.wait {
		fmt.Printf("\nWaiting for deployment to be ready (timeout: %s)...\n", opts.timeout)
		if err := waitForReady(ctx, k8sClient, opts.name, opts.namespace, opts.timeout); err != nil {
			return err
		}
	}

	return nil
}

func waitForReady(ctx context.Context, k8sClient client.Client, name, namespace string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	startTime := time.Now()
	lastPhase := ""

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for deployment to be ready")
		case <-ticker.C:
			// Check Model status
			model := &inferencev1alpha1.Model{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, model); err != nil {
				return fmt.Errorf("failed to get Model: %w", err)
			}

			// Check InferenceService status
			isvc := &inferencev1alpha1.InferenceService{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, isvc); err != nil {
				return fmt.Errorf("failed to get InferenceService: %w", err)
			}

			// Print status updates
			currentPhase := fmt.Sprintf("Model: %s, Service: %s (%d/%d replicas)",
				model.Status.Phase, isvc.Status.Phase, isvc.Status.ReadyReplicas, isvc.Status.DesiredReplicas)

			if currentPhase != lastPhase {
				elapsed := time.Since(startTime).Round(time.Second)
				fmt.Printf("[%s] %s\n", elapsed, currentPhase)
				lastPhase = currentPhase
			}

			// Check if ready
			if model.Status.Phase == "Ready" && isvc.Status.Phase == "Ready" {
				fmt.Printf("\n✅ Deployment ready!\n")
				fmt.Printf("═══════════════════════════════════════════════\n")
				fmt.Printf("Model:       %s\n", name)
				fmt.Printf("Size:        %s\n", model.Status.Size)
				fmt.Printf("Path:        %s\n", model.Status.Path)
				fmt.Printf("Endpoint:    %s\n", isvc.Status.Endpoint)
				fmt.Printf("Replicas:    %d/%d\n", isvc.Status.ReadyReplicas, isvc.Status.DesiredReplicas)
				fmt.Printf("═══════════════════════════════════════════════\n\n")
				fmt.Printf("🧪 To test the inference endpoint:\n\n")
				fmt.Printf("  # Port forward the service\n")
				fmt.Printf("  kubectl port-forward -n %s svc/%s 8080:8080\n\n", namespace, name)
				fmt.Printf("  # Send a test request\n")
				fmt.Printf("  curl http://localhost:8080/v1/chat/completions \\\n")
				fmt.Printf("    -H \"Content-Type: application/json\" \\\n")
				fmt.Printf("    -d '{\"messages\":[{\"role\":\"user\",\"content\":\"What is 2+2?\"}]}'\n\n")
				return nil
			}

			// Check for failures
			if model.Status.Phase == "Failed" {
				return fmt.Errorf("model deployment failed")
			}
			if isvc.Status.Phase == "Failed" {
				return fmt.Errorf("inference service deployment failed")
			}
		}
	}
}
