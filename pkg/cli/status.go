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

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

type statusOptions struct {
	name      string
	namespace string
}

// NewStatusCommand creates the status command
func NewStatusCommand() *cobra.Command {
	opts := &statusOptions{}

	cmd := &cobra.Command{
		Use:   "status [NAME]",
		Short: "Show status of an LLM deployment",
		Long:  `Display detailed status information about a Model and InferenceService.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			return runStatus(opts)
		},
	}

	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")

	return cmd
}

func runStatus(opts *statusOptions) error {
	ctx := context.Background()

	// Get Kubernetes client
	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	// Get Model
	model := &inferencev1alpha1.Model{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: opts.name, Namespace: opts.namespace}, model); err != nil {
		return fmt.Errorf("failed to get Model: %w", err)
	}

	// Get InferenceService
	isvc := &inferencev1alpha1.InferenceService{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: opts.name, Namespace: opts.namespace}, isvc); err != nil {
		return fmt.Errorf("failed to get InferenceService: %w", err)
	}

	// Print status
	fmt.Printf("Deployment: %s\n", opts.name)
	fmt.Printf("Namespace:  %s\n\n", opts.namespace)

	fmt.Printf("MODEL STATUS:\n")
	fmt.Printf("  Phase:       %s\n", model.Status.Phase)
	fmt.Printf("  Source:      %s\n", model.Spec.Source)
	fmt.Printf("  Format:      %s\n", model.Spec.Format)
	fmt.Printf("  Size:        %s\n", model.Status.Size)
	fmt.Printf("  Path:        %s\n", model.Status.Path)
	if model.Spec.Hardware != nil {
		fmt.Printf("  Accelerator: %s\n", model.Spec.Hardware.Accelerator)
	}
	if model.Status.LastUpdated != nil {
		fmt.Printf("  Updated:     %s\n", model.Status.LastUpdated.Format("2006-01-02 15:04:05"))
	}

	fmt.Printf("\nINFERENCE SERVICE STATUS:\n")
	fmt.Printf("  Phase:           %s\n", isvc.Status.Phase)
	fmt.Printf("  Model Reference: %s\n", isvc.Spec.ModelRef)
	fmt.Printf("  Replicas:        %d/%d ready\n", isvc.Status.ReadyReplicas, isvc.Status.DesiredReplicas)
	fmt.Printf("  Endpoint:        %s\n", isvc.Status.Endpoint)
	if isvc.Status.LastUpdated != nil {
		fmt.Printf("  Updated:         %s\n", isvc.Status.LastUpdated.Format("2006-01-02 15:04:05"))
	}

	// Print conditions
	if len(model.Status.Conditions) > 0 {
		fmt.Printf("\nMODEL CONDITIONS:\n")
		for _, cond := range model.Status.Conditions {
			fmt.Printf("  %s: %s (%s) - %s\n", cond.Type, cond.Status, cond.Reason, cond.Message)
		}
	}

	if len(isvc.Status.Conditions) > 0 {
		fmt.Printf("\nSERVICE CONDITIONS:\n")
		for _, cond := range isvc.Status.Conditions {
			fmt.Printf("  %s: %s (%s) - %s\n", cond.Type, cond.Status, cond.Reason, cond.Message)
		}
	}

	return nil
}
