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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
)

type deleteOptions struct {
	name       string
	namespace  string
	purgeCache bool
}

func NewDeleteCommand() *cobra.Command {
	opts := &deleteOptions{}

	cmd := &cobra.Command{
		Use:     "delete [NAME]",
		Aliases: []string{"del", "rm"},
		Short:   "Delete an LLM deployment",
		Long:    `Delete both the Model and InferenceService resources for a deployment.`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			return runDelete(opts)
		},
	}

	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().BoolVar(&opts.purgeCache, "purge-cache", false,
		"Also purge the model from the persistent cache when deleting the deployment")

	return cmd
}

func runDelete(opts *deleteOptions) error {
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

	fmt.Printf("Deleting InferenceService '%s' in namespace '%s'...\n", opts.name, opts.namespace)
	inferenceService := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.name,
			Namespace: opts.namespace,
		},
	}
	if err := k8sClient.Delete(ctx, inferenceService); err != nil {
		fmt.Printf("Warning: failed to delete InferenceService: %v\n", err)
	} else {
		fmt.Printf("✓ InferenceService '%s' deleted\n", opts.name)
	}

	fmt.Printf("Deleting Model '%s'...\n", opts.name)
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.name,
			Namespace: opts.namespace,
		},
	}

	// If --purge-cache is set, get the cache key before deleting the model
	var cacheKey string
	if opts.purgeCache {
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.name, Namespace: opts.namespace}, model); err != nil {
			fmt.Printf("Warning: failed to get Model for cache purge: %v\n", err)
		} else {
			cacheKey = cachekey.EffectiveKey(model)
		}
	}

	if err := k8sClient.Delete(ctx, model); err != nil {
		fmt.Printf("Warning: failed to delete Model: %v\n", err)
	} else {
		fmt.Printf("✓ Model '%s' deleted\n", opts.name)
	}

	if opts.purgeCache {
		if cacheKey != "" {
			fmt.Printf("🗑️  Purging model cache (cache key: %s)...\n", cacheKey)
			fmt.Printf("To purge the cache, run:\n")
			fmt.Printf("  kubectl exec -n llmkube-system deploy/llmkube-controller-manager -- \\\n")
			fmt.Printf("    rm -rf /models/%s\n", cacheKey)
		} else {
			fmt.Printf("⚠️  Model has no cache key; nothing to purge\n")
		}
	}

	fmt.Printf("\n✓ Deployment '%s' deleted successfully\n", opts.name)
	return nil
}
