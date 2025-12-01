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
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

type queueOptions struct {
	allNamespaces bool
	namespace     string
}

func NewQueueCommand() *cobra.Command {
	opts := &queueOptions{}

	cmd := &cobra.Command{
		Use:   "queue",
		Short: "Show InferenceServices waiting for GPU resources",
		Long: `Display InferenceServices that are queued waiting for GPU resources.

Shows all services with phase 'WaitingForGPU' sorted by queue position.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueue(opts)
		},
	}

	cmd.Flags().BoolVarP(&opts.allNamespaces, "all-namespaces", "A", false, "List across all namespaces")
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")

	return cmd
}

func runQueue(opts *queueOptions) error {
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

	isvcList := &inferencev1alpha1.InferenceServiceList{}
	listOpts := []client.ListOption{}

	if !opts.allNamespaces {
		listOpts = append(listOpts, client.InNamespace(opts.namespace))
	}

	if err := k8sClient.List(ctx, isvcList, listOpts...); err != nil {
		return fmt.Errorf("failed to list InferenceServices: %w", err)
	}

	type queuedService struct {
		name          string
		namespace     string
		queuePosition int32
		waitingFor    string
		priority      string
		age           time.Duration
	}

	var queued []queuedService
	for _, isvc := range isvcList.Items {
		if isvc.Status.Phase == "WaitingForGPU" {
			priority := isvc.Spec.Priority
			if priority == "" {
				priority = "normal"
			}
			queued = append(queued, queuedService{
				name:          isvc.Name,
				namespace:     isvc.Namespace,
				queuePosition: isvc.Status.QueuePosition,
				waitingFor:    isvc.Status.WaitingFor,
				priority:      priority,
				age:           time.Since(isvc.CreationTimestamp.Time),
			})
		}
	}

	if len(queued) == 0 {
		if opts.allNamespaces {
			fmt.Println("No InferenceServices waiting for GPU resources across all namespaces")
		} else {
			fmt.Printf("No InferenceServices waiting for GPU resources in namespace %s\n", opts.namespace)
		}
		return nil
	}

	sort.Slice(queued, func(i, j int) bool {
		return queued[i].queuePosition < queued[j].queuePosition
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	if opts.allNamespaces {
		_, _ = fmt.Fprintln(w, "NAMESPACE\tNAME\tQUEUE\tWAITING FOR\tPRIORITY\tAGE")
		for _, s := range queued {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n",
				s.namespace,
				s.name,
				s.queuePosition,
				s.waitingFor,
				s.priority,
				formatDuration(s.age),
			)
		}
	} else {
		_, _ = fmt.Fprintln(w, "NAME\tQUEUE\tWAITING FOR\tPRIORITY\tAGE")
		for _, s := range queued {
			_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n",
				s.name,
				s.queuePosition,
				s.waitingFor,
				s.priority,
				formatDuration(s.age),
			)
		}
	}

	_ = w.Flush()
	return nil
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
