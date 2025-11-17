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
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

type listOptions struct {
	namespace string
	resource  string
}

// NewListCommand creates the list command
func NewListCommand() *cobra.Command {
	opts := &listOptions{}

	cmd := &cobra.Command{
		Use:     "list [models|services]",
		Aliases: []string{"ls"},
		Short:   "List LLM resources",
		Long:    `List Models or InferenceServices in the specified namespace.`,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.resource = args[0]
			} else {
				opts.resource = "services" // default to services
			}
			return runList(opts)
		},
	}

	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")

	return cmd
}

func runList(opts *listOptions) error {
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

	switch opts.resource {
	case "models", "model", "mdl":
		return listModels(ctx, k8sClient, opts.namespace)
	case "services", "service", "svc", "inferenceservices":
		return listServices(ctx, k8sClient, opts.namespace)
	default:
		return fmt.Errorf("unknown resource type: %s (use 'models' or 'services')", opts.resource)
	}
}

func listModels(ctx context.Context, k8sClient client.Client, namespace string) error {
	modelList := &inferencev1alpha1.ModelList{}
	if err := k8sClient.List(ctx, modelList, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("failed to list models: %w", err)
	}

	if len(modelList.Items) == 0 {
		fmt.Printf("No models found in namespace '%s'\n", namespace)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tPHASE\tSIZE\tACCELERATOR\tAGE"); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}

	for _, model := range modelList.Items {
		age := model.CreationTimestamp.Time
		accelerator := "cpu"
		if model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator != "" {
			accelerator = model.Spec.Hardware.Accelerator
		}

		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			model.Name,
			model.Status.Phase,
			model.Status.Size,
			accelerator,
			formatAge(age),
		); err != nil {
			return fmt.Errorf("failed to write model row: %w", err)
		}
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("failed to flush output: %w", err)
	}
	return nil
}

func listServices(ctx context.Context, k8sClient client.Client, namespace string) error {
	serviceList := &inferencev1alpha1.InferenceServiceList{}
	if err := k8sClient.List(ctx, serviceList, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("failed to list inference services: %w", err)
	}

	if len(serviceList.Items) == 0 {
		fmt.Printf("No inference services found in namespace '%s'\n", namespace)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	header := "NAME\tMODEL\tPHASE\tREPLICAS\tENDPOINT"
	if _, err := fmt.Fprintln(w, header); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}

	for _, svc := range serviceList.Items {
		replicas := fmt.Sprintf("%d/%d", svc.Status.ReadyReplicas, svc.Status.DesiredReplicas)

		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			svc.Name,
			svc.Spec.ModelRef,
			svc.Status.Phase,
			replicas,
			svc.Status.Endpoint,
		); err != nil {
			return fmt.Errorf("failed to write service row: %w", err)
		}
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("failed to flush output: %w", err)
	}
	return nil
}
