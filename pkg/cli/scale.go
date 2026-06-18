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
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

type scaleOptions struct {
	name      string
	namespace string
	replicas  int32
}

func NewScaleCommand() *cobra.Command {
	opts := &scaleOptions{}

	cmd := &cobra.Command{
		Use:   "scale [NAME]",
		Short: "Scale an InferenceService deployment",
		Long: `Scale an existing InferenceService deployment in place.

This command changes the replica count of an InferenceService without
deleting and redeploying. Use --replicas 0 to stop a deployment (the
Model and config are preserved) or --replicas N>0 to scale back up.

Examples:
  # Scale down to zero (stop the deployment)
  llmkube scale gemma-fable5-q6k --replicas 0

  # Scale back up to 1 replica
  llmkube scale gemma-fable5-q6k --replicas 1

  # Scale to multiple replicas
  llmkube scale qwen-3-32b --replicas 3 --namespace production`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			return runScale(opts)
		},
	}

	cmd.Flags().Int32VarP(&opts.replicas, "replicas", "r", 1, "Desired number of replicas")
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")

	return cmd
}

func runScale(opts *scaleOptions) error {
	ctx := context.Background()

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

	isvc := &inferencev1alpha1.InferenceService{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: opts.name, Namespace: opts.namespace}, isvc); err != nil {
		return fmt.Errorf("failed to get InferenceService '%s': %w", opts.name, err)
	}

	// Build a strategic merge patch for spec.replicas
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"replicas": opts.replicas,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	if err := k8sClient.Patch(ctx, isvc, client.RawPatch(types.MergePatchType, patchBytes)); err != nil {
		return fmt.Errorf("failed to scale InferenceService '%s': %w", opts.name, err)
	}

	fmt.Printf("Scaled InferenceService '%s' to %d replicas\n", opts.name, opts.replicas)
	return nil
}
