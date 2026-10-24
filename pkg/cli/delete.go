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
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
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
	var image string

	cmd := &cobra.Command{
		Use:     "delete [NAME]",
		Aliases: []string{"del", "rm"},
		Short:   "Delete an LLM deployment",
		Long:    `Delete both the Model and InferenceService resources for a deployment.`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			return withCacheImage(image, func() error {
				return runDelete(opts)
			})
		},
	}

	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().BoolVar(&opts.purgeCache, "purge-cache", false,
		"Also purge the model from the persistent cache when deleting the deployment")
	cmd.Flags().StringVar(&image, "cache-image", "",
		"Image for the short-lived pod that purges the cache (default busybox:1.37.0, or $"+cacheImageEnv+")")

	return cmd
}

// Test seams so runDelete can be driven against a fake client and clientset.
var (
	deleteKubeConfig = config.GetConfig
	deleteClientset  = func(cfg *rest.Config) (kubernetes.Interface, error) { return kubernetes.NewForConfig(cfg) }
	newDeleteClient  = func(cfg *rest.Config) (deleteCRClient, error) {
		return client.New(cfg, client.Options{Scheme: scheme.Scheme})
	}
)

// deleteCRClient is the subset of the controller-runtime client delete uses.
type deleteCRClient = client.Client

func runDelete(opts *deleteOptions) error {
	ctx := context.Background()

	cfg, err := deleteKubeConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := newDeleteClient(cfg)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	var cacheKey string
	var cacheTargets []purgeTarget
	var cacheSvcs []inferencev1alpha1.InferenceService
	var cacheResolveErr error
	var modelClaim string
	var stagedClaims []string
	if opts.purgeCache {
		// Resolve the key, the Model's own claim and the claims before
		// deleting: the Model has no finalizer, so nothing of it can be read
		// after, and in perService mode the claim is owner-ref'd to the
		// InferenceService and is collected with it.
		purgeModel := &inferencev1alpha1.Model{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.name, Namespace: opts.namespace}, purgeModel); err != nil {
			fmt.Printf("Warning: failed to get Model for cache purge: %v\n", err)
		} else if cacheKey = cachekey.EffectiveKey(purgeModel); cacheKey != "" {
			modelClaim, stagedClaims = modelCacheClaimName(purgeModel), prefetchedClaims(purgeModel)
			cacheTargets, cacheSvcs, cacheResolveErr = resolvePurgeTargets(ctx, k8sClient, opts.namespace)
			// The targets, and each claim's access mode with them, come from
			// state read before the deletes below: the service one of them
			// removes may be what collects the claim.
			cacheTargets = targetedClaims(cacheTargets, cacheSvcs, opts.name, modelClaim, stagedClaims)
		}
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

	if err := k8sClient.Delete(ctx, model); err != nil {
		fmt.Printf("Warning: failed to delete Model: %v\n", err)
	} else {
		fmt.Printf("✓ Model '%s' deleted\n", opts.name)
	}

	if opts.purgeCache && cacheKey == "" {
		fmt.Printf("⚠️  Model has no cache key; nothing to purge\n")
	}

	// The InferenceService stays mounted on its claim until it is gone, so its
	// deletion (issued above) is what frees a strict RWO claim for the purge.
	if opts.purgeCache && cacheKey != "" {
		if cacheResolveErr != nil {
			return fmt.Errorf("deployment deleted, but purging the cache failed: %w; "+
				"run 'llmkube cache clear -n %s' to clear what remains", cacheResolveErr, opts.namespace)
		}
		targets := cacheTargets
		var owned, purged []purgeTarget
		for _, target := range targets {
			// The operator garbage-collects a claim owner-ref'd to the service
			// just deleted, and a pod mounting a terminating claim never
			// schedules.
			if target.InferenceService == opts.name {
				owned = append(owned, target)
				continue
			}
			purged = append(purged, target)
		}
		for _, target := range owned {
			fmt.Printf("Cache PVC '%s' is removed with InferenceService '%s'; not purging it separately\n",
				target.Name, opts.name)
		}
		if len(purged) == 0 {
			if len(owned) == 0 {
				fmt.Printf("⚠️  No model cache PVC found in namespace '%s'; nothing to purge\n", opts.namespace)
			}
			fmt.Printf("\n✓ Deployment '%s' deleted successfully\n", opts.name)
			return nil
		}
		clientset, err := deleteClientset(cfg)
		if err != nil {
			return fmt.Errorf("deployment deleted, but purging the cache failed: %w", err)
		}
		fmt.Printf("🗑️  Purging model cache (cache key: %s)...\n", cacheKey)
		err = purgeDeleteTargets(ctx, cfg, k8sClient, clientset, opts.namespace, opts.name, cacheKey, purged)
		if err != nil {
			return fmt.Errorf("deployment deleted, but purging the cache failed: %w; "+
				"run 'llmkube cache clear -n %s' to clear what remains", err, opts.namespace)
		}
	}

	fmt.Printf("\n✓ Deployment '%s' deleted successfully\n", opts.name)
	return nil
}

// servingPodsOf reports whether a pod belongs to name, the InferenceService
// this run deleted: its cascade is asynchronous, so its serving pod can still
// be mounting the claim when the purge looks.
//
// The proof is the owner chain the deployment controller builds: the pod's
// controller ReplicaSet, whose own controller is the Deployment named after the
// InferenceService. A ReplicaSet whose name merely starts with name and a
// hyphen is not proof, because the Deployment of a service called "<name>-v2"
// produces "<name>-v2-<hash>" names too.
func servingPodsOf(
	ctx context.Context, clientset kubernetes.Interface, namespace, name string,
) func(*corev1.Pod) bool {
	verdicts := map[string]bool{}
	return func(pod *corev1.Pod) bool {
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.Kind != "ReplicaSet" || owner.Name == "" {
			return false
		}
		if verdict, ok := verdicts[owner.Name]; ok {
			return verdict
		}
		// Unreadable, because the ReplicaSet is already collected or the read
		// is forbidden, is not evidence of another Deployment: waiting is what
		// base did, and refusing fails a purge that would otherwise succeed.
		// A readable ReplicaSet must be proven owned by the Deployment named
		// after the service, since one under no Deployment holds the claim
		// just as hard.
		verdict := true
		if set, err := clientset.AppsV1().ReplicaSets(namespace).Get(ctx, owner.Name, metav1.GetOptions{}); err == nil {
			verdict = false
			if controller := metav1.GetControllerOf(set); controller != nil &&
				controller.Kind == "Deployment" && controller.Name == name {
				verdict = true
			}
		}
		verdicts[owner.Name] = verdict
		return verdict
	}
}

// purgeDeleteTargets removes cacheKey from every resolved claim, purging each
// claim on its own so one failure is reported with the claim it belongs to.
func purgeDeleteTargets(
	ctx context.Context, cfg *rest.Config, k8sClient client.Client, clientset kubernetes.Interface,
	namespace, deletedService, cacheKey string, targets []purgeTarget,
) error {
	if len(targets) == 0 {
		return fmt.Errorf("no model cache PVC found")
	}

	releasing := servingPodsOf(ctx, clientset, namespace, deletedService)
	var errs []error
	var outcomes []purgeOutcome
	for _, target := range targets {
		outcome, err := purgeCacheKeysReleasing(ctx, cfg, k8sClient, clientset, namespace, target,
			releasing, []string{cacheKey})
		if err != nil {
			errs = append(errs, fmt.Errorf("cache PVC %s: %w", target.Name, err))
			continue
		}
		outcomes = append(outcomes, outcome)
	}
	reportPurgeOutcomes(outcomes)
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if keysAbsentEverywhere([]string{cacheKey}, outcomes) {
		fmt.Printf("%s is not on any cache claim in namespace '%s'.\n", cacheKey, namespace)
	}
	return nil
}
