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
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
)

const (
	statusActive   = "active"
	statusOrphaned = "orphaned"
)

// cacheClearOptions carries the flags of `cache clear`.
type cacheClearOptions struct {
	model     string
	namespace string
	force     bool
	orphaned  bool
}

// CacheEntry represents a cached model
type CacheEntry struct {
	CacheKey         string
	Source           string
	Size             int64
	SizeHuman        string
	ModTime          time.Time
	ModelNames       []string // Models using this cache entry
	Status           string   // "active" or "orphaned"
	InferenceService string   // owning InferenceService; empty for shared cache
}

// NewCacheCommand creates the cache command
func NewCacheCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage the model cache",
		Long: `Manage the persistent model cache.

The model cache stores downloaded models to avoid re-downloading
when Models or InferenceServices are deleted and recreated.

Examples:
  # List cached models
  llmkube cache list

  # Clear all cached models
  llmkube cache clear

  # Clear a specific cached model by name
  llmkube cache clear --model llama-3.1-8b

  # Pre-download a catalog model to the cache
  llmkube cache preload llama-3.1-8b
`,
	}

	cmd.AddCommand(newCacheListCommand())
	cmd.AddCommand(newCacheClearCommand())
	cmd.AddCommand(newCachePreloadCommand())
	cmd.AddCommand(NewCachePrepCommand())

	return cmd
}

func newCacheListCommand() *cobra.Command {
	var namespace string
	var allNamespaces bool
	var orphanedOnly bool
	var image string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cached models",
		Long: `List all models in the persistent cache.

Shows cache entries with their size, status, and which Model resources
are using each cache entry. Each cache claim is read through a short-lived
read-only pod the command creates and deletes, so it works even when the
only other pod mounting the claim cannot run a shell. That pod runs
busybox:1.37.0 unless --cache-image or LLMKUBE_CACHE_IMAGE
names another image, which is what an air-gapped cluster needs. The command
fails when no discovered claim can be read, rather than reporting the cache
as empty; a claim that cannot be read while others can is skipped with a
warning.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCacheImage(image, func() error {
				return runCacheList(namespace, allNamespaces, orphanedOnly)
			})
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List models from all namespaces")
	cmd.Flags().BoolVar(&orphanedOnly, "orphaned", false, "Show only orphaned cache entries (no matching Model)")
	cmd.Flags().StringVar(&image, "cache-image", "",
		"Image for the short-lived pod that reads the cache claim (default busybox:1.37.0, or $"+cacheImageEnv+")")

	return cmd
}

func newCacheClearCommand() *cobra.Command {
	var opts cacheClearOptions
	var image string

	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Clear cached models",
		Long: `Remove entries from the persistent model cache.

Entries are removed through a short-lived pod that mounts the cache claim
read-write; the controller does not mount it. That pod runs busybox:1.37.0
unless --cache-image or LLMKUBE_CACHE_IMAGE names another image.

By default, clears every entry of the cache claims ` + "`cache list`" + ` discovers.
Use --model to clear a specific model's cache entry (which also reaches a
spec.modelCache.claimName its InferenceService brings), or --orphaned to
remove every entry no Model uses.

WARNING: Clearing the cache will cause models to be re-downloaded
when InferenceServices restart or new pods are created.

Examples:
  # Clear a specific model's cache entry
  llmkube cache clear --model llama-3.1-8b -n default

  # Remove every entry that no Model references
  llmkube cache clear --orphaned -n default

  # Clear all cache without confirmation
  llmkube cache clear --force -n default
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCacheImage(image, func() error {
				return runCacheClear(&opts)
			})
		},
	}

	cmd.Flags().StringVar(&opts.model, "model", "", "Clear cache for a specific model")
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().BoolVar(&opts.force, "force", false, "Force clear without confirmation")
	cmd.Flags().BoolVar(&opts.orphaned, "orphaned", false, "Clear only orphaned cache entries (no matching Model)")
	cmd.MarkFlagsMutuallyExclusive("model", "orphaned")
	cmd.Flags().StringVar(&image, "cache-image", "",
		"Image for the short-lived pod that mounts the cache claim (default busybox:1.37.0, or $"+cacheImageEnv+")")

	return cmd
}

func newCachePreloadCommand() *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "preload MODEL_ID",
		Short: "Pre-download a model to the cache",
		Long: `Pre-download a catalog model to the persistent cache.

This allows you to download models before deploying them, useful for:
- Air-gapped environments (pre-populate cache)
- Reducing deployment time (model already cached)
- Bandwidth management (download during off-peak hours)

Examples:
  # Preload a catalog model
  llmkube cache preload llama-3.1-8b

  # Preload to a specific namespace
  llmkube cache preload llama-3.1-8b -n production
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCachePreload(args[0], namespace)
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")

	return cmd
}

func runCacheList(namespace string, allNamespaces bool, orphanedOnly bool) error {
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

	modelList := &inferencev1alpha1.ModelList{}
	listOpts := []client.ListOption{}
	if !allNamespaces {
		listOpts = append(listOpts, client.InNamespace(namespace))
	}

	if err := k8sClient.List(ctx, modelList, listOpts...); err != nil {
		return fmt.Errorf("failed to list models: %w", err)
	}

	cacheEntries := make(map[string]*CacheEntry)
	seedCacheEntries(modelList.Items, allNamespaces, cacheEntries)

	// Inspect actual PVC contents (only for single-namespace mode)
	var pvcInspected bool
	if !allNamespaces {
		pvcEntries, err := inspectPVCCache(ctx, cfg, k8sClient, namespace)
		if err != nil {
			return fmt.Errorf("cannot read the model cache: %w", err)
		}
		if pvcEntries != nil {
			pvcInspected = true
			for _, pe := range pvcEntries {
				if entry, exists := cacheEntries[pe.CacheKey]; exists {
					entry.Size = pe.SizeBytes
					entry.SizeHuman = formatBytes(pe.SizeBytes)
					entry.InferenceService = pe.InferenceService
				} else {
					cacheEntries[pe.CacheKey] = &CacheEntry{
						CacheKey:         pe.CacheKey,
						Size:             pe.SizeBytes,
						SizeHuman:        formatBytes(pe.SizeBytes),
						Status:           statusOrphaned,
						InferenceService: pe.InferenceService,
					}
				}
			}
		}
	}

	if orphanedOnly {
		for key, entry := range cacheEntries {
			if entry.Status != statusOrphaned {
				delete(cacheEntries, key)
			}
		}
	}

	if len(cacheEntries) == 0 {
		if orphanedOnly {
			fmt.Println("No orphaned cache entries found.")
		} else {
			fmt.Println("No cache entries found.")
		}
		return nil
	}

	fmt.Printf("\nModel Cache Entries\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════════════════════\n")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if pvcInspected {
		_, _ = fmt.Fprintln(w, "CACHE KEY\tSTATUS\tSIZE\tISVC\tMODELS\tSOURCE")
	} else {
		_, _ = fmt.Fprintln(w, "CACHE KEY\tSIZE\tMODELS\tSOURCE")
	}

	var activeCount, orphanedCount int
	var totalBytes int64
	for _, entry := range cacheEntries {
		models := strings.Join(entry.ModelNames, ", ")
		if len(models) > 40 {
			models = models[:37] + "..."
		}

		source := entry.Source
		if len(source) > 50 {
			source = "..." + source[len(source)-47:]
		}

		size := entry.SizeHuman
		if size == "" {
			size = "-"
		}

		isvc := entry.InferenceService
		if isvc == "" {
			isvc = "shared"
		}

		if entry.Status == statusOrphaned {
			orphanedCount++
		} else {
			activeCount++
		}
		totalBytes += entry.Size

		if pvcInspected {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", entry.CacheKey, entry.Status, size, isvc, models, source)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", entry.CacheKey, size, models, source)
		}
	}
	_ = w.Flush()

	if pvcInspected {
		fmt.Printf("\nTotal: %d cache entries (%d active, %d orphaned), %s used\n",
			len(cacheEntries), activeCount, orphanedCount, formatBytes(totalBytes))
	} else {
		fmt.Printf("\nTotal: %d cache entries, %d models\n", len(cacheEntries), len(modelList.Items))
	}

	return nil
}

// seedCacheEntries records one active entry per cache key. `cache list` and
// `cache clear --orphaned` share it so the two never disagree about which
// entries a Model still references.
func seedCacheEntries(models []inferencev1alpha1.Model, allNamespaces bool, entries map[string]*CacheEntry) {
	for _, model := range models {
		cacheKey := cachekey.EffectiveKey(&model)
		if cacheKey == "" {
			// metal / single-file runtime-resolved models are never cached
			// under a derived key; skip them so they do not show up as
			// phantom entries.
			continue
		}

		entry, exists := entries[cacheKey]
		if !exists {
			entry = &CacheEntry{
				CacheKey:   cacheKey,
				Source:     model.Spec.Source,
				ModelNames: []string{},
				Status:     statusActive,
			}
			entries[cacheKey] = entry
		}

		modelName := model.Name
		if allNamespaces {
			modelName = fmt.Sprintf("%s/%s", model.Namespace, model.Name)
		}
		entry.ModelNames = append(entry.ModelNames, modelName)

		if model.Status.Size != "" {
			entry.SizeHuman = model.Status.Size
		}
	}
}

// confirm prints prompt and reads a yes/no answer from stdin.
func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	var response string
	_, _ = fmt.Scanln(&response)
	response = strings.ToLower(strings.TrimSpace(response))
	return response == "y" || response == "yes"
}

func runCacheClear(opts *cacheClearOptions) error {
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

	var keys []string
	active := map[string]bool{}

	switch {
	case opts.model != "":
		model := &inferencev1alpha1.Model{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.model, Namespace: opts.namespace}, model); err != nil {
			return fmt.Errorf("failed to get model '%s': %w", opts.model, err)
		}
		key, err := purgeKey(model, opts.model)
		if err != nil {
			return err
		}
		keys = append(keys, key)

		if !opts.force {
			if !confirm(fmt.Sprintf(
				"This will clear the cache entry for model '%s' (cache key: %s)\n"+
					"The model will be re-downloaded when the InferenceService restarts.\nContinue?", opts.model, key)) {
				fmt.Println("Cancelled.")
				return nil
			}
		}
	case opts.orphaned:
		if !opts.force {
			if !confirm("This will remove every cache entry that no Model references.") {
				fmt.Println("Cancelled.")
				return nil
			}
		}
	default:
		if !opts.force {
			if !confirm("This will clear ALL cached models.\n" +
				"All models will be re-downloaded when InferenceServices restart.\nContinue?") {
				fmt.Println("Cancelled.")
				return nil
			}
		}
	}

	targets, svcs, err := resolvePurgeTargets(ctx, k8sClient, opts.namespace)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no model cache PVC found in namespace '%s'", opts.namespace)
	}
	if len(keys) > 0 {
		// A named model's files live on the claim its own InferenceService
		// mounts, not on every cache claim in the namespace.
		claim, staged := modelCacheClaims(ctx, k8sClient, opts.namespace, opts.model)
		targets = targetedClaims(targets, svcs, opts.model, claim, staged)
	} else {
		// The namespace-wide modes act on what `cache list` discovers. A
		// user's claimName is not a discovery target: it may be mounted by an
		// RWO service that fails the second attach, and the sweep would stall
		// on it.
		targets = discoveredClaims(targets)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	if opts.orphaned {
		list := &inferencev1alpha1.ModelList{}
		if err := k8sClient.List(ctx, list, client.InNamespace(opts.namespace)); err != nil {
			return fmt.Errorf("failed to list models: %w", err)
		}
		entries := make(map[string]*CacheEntry)
		seedCacheEntries(list.Items, false, entries)
		for key := range entries {
			active[key] = true
		}
	}

	var errs []error
	var outcomes []purgeOutcome
	for _, target := range targets {
		var outcome purgeOutcome
		var err error
		switch {
		case len(keys) > 0:
			outcome, err = purgeCacheKeys(ctx, cfg, k8sClient, clientset, opts.namespace, target, keys)
		case opts.orphaned:
			outcome, err = purgeOrphanedEntries(ctx, cfg, k8sClient, clientset, opts.namespace, target, active)
		default:
			outcome, err = purgeAllEntries(ctx, cfg, k8sClient, clientset, opts.namespace, target)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("purging cache PVC %s: %w", target.Name, err))
			continue
		}
		outcomes = append(outcomes, outcome)
	}

	reportPurgeOutcomes(outcomes)

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if keysAbsentEverywhere(keys, outcomes) {
		// The per-claim report already said "already absent"; say what it
		// means for the command as a whole.
		fmt.Printf("\n%s is not in the cache of namespace '%s'; nothing was removed.\n", keys[0], opts.namespace)
	}
	return nil
}

// keysAbsentEverywhere reports whether the single requested key was absent on
// every PVC, so the caller can say so instead of printing a bare success.
func keysAbsentEverywhere(keys []string, outcomes []purgeOutcome) bool {
	if len(keys) != 1 {
		return false
	}
	want := defaultModelMountPath + "/" + keys[0]
	for _, outcome := range outcomes {
		for _, path := range outcome.Removed {
			if path == want {
				return false
			}
		}
	}
	return true
}

// reportPurgeOutcomes prints what each PVC's purge did.
func reportPurgeOutcomes(outcomes []purgeOutcome) {
	var total int
	for _, outcome := range outcomes {
		if outcome.isEmpty() {
			continue
		}
		fmt.Printf("\nCache PVC %s:\n", outcome.PVC)
		for _, path := range outcome.Removed {
			fmt.Printf("  removed %s\n", path)
			total++
		}
		for _, path := range outcome.Absent {
			fmt.Printf("  already absent %s\n", path)
		}
		for _, name := range outcome.Skipped {
			fmt.Printf("  left %s (not a cache key directory)\n", name)
		}
	}
	if total > 0 {
		fmt.Printf("\nRemoved %d cache entr", total)
		if total == 1 {
			fmt.Println("y.")
		} else {
			fmt.Println("ies.")
		}
	}
}

func runCachePreload(modelID, namespace string) error {
	ctx := context.Background()

	// Get model from catalog
	catalogModel, err := GetModel(modelID)
	if err != nil {
		return fmt.Errorf("model '%s' not found in catalog: %w", modelID, err)
	}

	fmt.Printf("Preloading model: %s\n", catalogModel.Name)
	fmt.Printf("Source: %s\n", catalogModel.Source)
	fmt.Printf("Size: %s (estimated)\n\n", catalogModel.Resources.Memory)

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

	// Check if model already exists
	existingModel := &inferencev1alpha1.Model{}
	err = k8sClient.Get(ctx, client.ObjectKey{Name: modelID, Namespace: namespace}, existingModel)
	if err == nil {
		if existingModel.Status.Phase == phaseReady {
			fmt.Printf("Model '%s' already exists and is ready (cached).\n", modelID)
			fmt.Printf("Cache key: %s\n", existingModel.Status.CacheKey)
			return nil
		}
		fmt.Printf("Model '%s' exists but is in phase '%s'. Waiting for it to become ready...\n",
			modelID, existingModel.Status.Phase)
	} else {
		// Create the Model resource to trigger download
		fmt.Printf("Creating Model resource to trigger download...\n")

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{
				Name:      modelID,
				Namespace: namespace,
				Labels: map[string]string{
					"llmkube.dev/preload": "true",
				},
			},
			Spec: inferencev1alpha1.ModelSpec{
				Source:       catalogModel.Source,
				Format:       "gguf",
				Quantization: catalogModel.Quantization,
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "cpu", // Preload doesn't need GPU
				},
				Resources: &inferencev1alpha1.ResourceRequirements{
					CPU:    catalogModel.Resources.CPU,
					Memory: catalogModel.Resources.Memory,
				},
			},
		}

		if err := k8sClient.Create(ctx, model); err != nil {
			return fmt.Errorf("failed to create Model: %w", err)
		}
		fmt.Printf("Model resource created.\n")
	}

	// Wait for model to be ready
	fmt.Printf("\nDownloading model (this may take a while)...\n")
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	timeout := time.After(30 * time.Minute)

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for model download")
		case <-ticker.C:
			model := &inferencev1alpha1.Model{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: modelID, Namespace: namespace}, model); err != nil {
				continue
			}

			switch model.Status.Phase {
			case phaseReady:
				fmt.Printf("\n✅ Model preloaded successfully!\n")
				fmt.Printf("   Cache key: %s\n", model.Status.CacheKey)
				fmt.Printf("   Size: %s\n", model.Status.Size)
				fmt.Printf("   Path: %s\n", model.Status.Path)
				fmt.Printf("\nYou can now deploy this model without waiting for download:\n")
				fmt.Printf("  llmkube deploy %s --gpu\n", modelID)
				return nil
			case phaseFailed:
				return fmt.Errorf("model download failed")
			case "Downloading":
				fmt.Printf(".")
			}
		}
	}
}
