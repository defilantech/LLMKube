/*
Copyright 2026.

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
	"io"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
	"github.com/defilantech/llmkube/pkg/fileset"
)

// The states the listing script reports for an entry's download lock.
const (
	pruneLockFree    = "free"
	pruneLockHeld    = "locked"
	pruneLockNoFlock = "noflock"
)

// cachePruneOptions carries the flags of `cache prune`.
type cachePruneOptions struct {
	model     string
	namespace string
	dryRun    bool
	force     bool
}

func newCachePruneCommand() *cobra.Command {
	var opts cachePruneOptions
	var image string

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove files no Model lists from multi-file cache entries",
		Long: `Remove files from multi-file cache entries that no Model lists any more.

A Model with spec.files is cached in a directory keyed on its source, so
editing spec.files keeps the directory and leaves files dropped from the list
behind. prune removes a file only when no Model resolving to that cache entry
lists it, after resolving each Model's spec.files (globs included) and
spec.mmproj exactly as staging does. Files that a running pod's downloader
lists, or that a Model's status.prefetch records, are kept too.

It never removes what staging keeps for itself: SHA-256 stamps,
.sha256-rejected markers, .llmkube-size records, resume partials (*.tmp),
atomic-write temp files and the .llmkube-download.lock lockfile.

Only multi-file entries are in scope. Entries of single-file Models, entries a
single-file Model shares, and entries no Model uses are left alone (use
` + "`cache clear --orphaned`" + ` for those). An entry is skipped while a pod's
downloader is staging into it or another process holds its download lock,
and when a Model's file list cannot be resolved (a glob whose expansion the
Model has not recorded for its current generation).

prune always prints the files and bytes it would remove first. Without
--dry-run it then asks for confirmation, unless --force is given.

Entries are read and removed through a short-lived pod that mounts the cache
claim read-write, the same pod ` + "`cache clear`" + ` uses. It runs busybox:1.37.0
unless --cache-image or LLMKUBE_CACHE_IMAGE names another image, which needs
sh, find, stat and flock.

By default prune covers every cache claim in the namespace: the shared claim,
perService claims and any spec.modelCache.claimName. --model narrows it to
one Model's entry and the claims that Model uses.

Examples:
  # Show what would be removed
  llmkube cache prune --dry-run -n default

  # Prune one Model's entry, asking before removing anything
  llmkube cache prune --model ltx-video -n default

  # Prune every multi-file entry without confirmation
  llmkube cache prune --force -n default
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCacheImage(image, func() error {
				return runCachePrune(&opts)
			})
		},
	}

	cmd.Flags().StringVar(&opts.model, "model", "", "Prune only this Model's cache entry")
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "List the files prune would remove and exit")
	cmd.Flags().BoolVar(&opts.force, "force", false, "Remove without confirmation")
	cmd.MarkFlagsMutuallyExclusive("dry-run", "force")
	cmd.Flags().StringVar(&image, "cache-image", "",
		"Image for the short-lived pod that mounts the cache claim (default busybox:1.37.0, or $"+cacheImageEnv+")")

	return cmd
}

// pruneEnv is what a prune talks to; tests supply fakes.
type pruneEnv struct {
	cfg       *rest.Config
	client    client.Client
	clientset kubernetes.Interface
	out       io.Writer
	confirm   func(prompt string) bool
}

func runCachePrune(opts *cachePruneOptions) error {
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
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}
	return pruneCache(context.Background(), pruneEnv{
		cfg: cfg, client: k8sClient, clientset: clientset, out: os.Stdout, confirm: confirm,
	}, opts)
}

// cacheFile is one regular file of a cache entry, relative to the entry.
type cacheFile struct {
	Rel  string
	Size int64
}

// entryOwners is what the Models resolving to one cache key keep in it.
type entryOwners struct {
	Key    string
	Models []string
	// Keep holds the cache-relative names the Models stage, cleaned.
	Keep map[string]bool
	// Refusal, when set, leaves the entry alone and says why.
	Refusal string
	// multiFile records that a Model stages this entry with spec.files; an
	// entry without one is out of scope.
	multiFile bool
}

func (o *entryOwners) refuse(reason string) {
	if o.Refusal == "" {
		o.Refusal = reason
		return
	}
	o.Refusal += "; " + reason
}

// entryOwnersByKey groups models by the cache key they resolve to
// (cachekey.EffectiveKey, the rule `cache list` and staging share) and
// resolves each multi-file Model's files with fileset.ModelPlan, the function
// staging builds its download list from. Only keys a multi-file Model uses
// are returned. An entry a single-file Model also uses, or one with a Model
// whose files cannot be resolved, carries a Refusal.
func entryOwnersByKey(models []inferencev1alpha1.Model) map[string]*entryOwners {
	owners := map[string]*entryOwners{}
	get := func(key string) *entryOwners {
		o := owners[key]
		if o == nil {
			o = &entryOwners{Key: key, Keep: map[string]bool{}}
			owners[key] = o
		}
		return o
	}
	for i := range models {
		model := &models[i]
		key := cachekey.EffectiveKey(model)
		if key == "" {
			continue
		}
		o := get(key)
		o.Models = append(o.Models, model.Name)
		if !fileset.HasMultiFileStaging(model) {
			// Its file name comes from GGUF metadata the Model controller
			// parses, not from a list prune can resolve.
			o.refuse(fmt.Sprintf("single-file Model %s uses this entry", model.Name))
			continue
		}
		o.multiFile = true
		plan, err := fileset.ModelPlan(model)
		if err != nil {
			o.refuse(fmt.Sprintf("cannot resolve the files of Model %s: %v", model.Name, err))
			continue
		}
		if plan == nil {
			o.refuse(fmt.Sprintf("Model %s resolves to no files", model.Name))
			continue
		}
		for _, name := range plan.Files {
			o.Keep[path.Clean(name)] = true
		}
	}
	// A pod consuming a prefetched claim verifies the list the Job recorded
	// until a new prefetch replaces it, which can trail the spec.
	for i := range models {
		model := &models[i]
		for _, entry := range model.Status.Prefetch {
			key := entry.CacheKey
			if key == "" {
				key = cachekey.EffectiveKey(model)
			}
			if o := owners[key]; o != nil {
				for _, name := range entry.Files {
					o.Keep[path.Clean(name)] = true
				}
			}
		}
	}
	for key, o := range owners {
		if !o.multiFile {
			delete(owners, key)
			continue
		}
		sort.Strings(o.Models)
	}
	return owners
}

// podStaging is what the live pods on one claim do with one cache entry.
type podStaging struct {
	// Files are the names the pods' downloaders and verifiers list.
	Files []string
	// Busy names the pods whose downloader has not finished: staging into
	// the entry now, waiting to start, or about to retry.
	Busy []string
}

// stagingPodsOnClaim reads, from the pods that mount claim, which cache
// entries their staging init containers (model-downloader, model-verify)
// target, through the CACHE_DIR and MODEL_FILES the controller sets on them.
// It is the signal that also works on a FUSE cache, where staging takes no
// download lock. A pod that has finished (Succeeded or Failed) is ignored.
func stagingPodsOnClaim(pods []corev1.Pod, claim string) map[string]*podStaging {
	byKey := map[string]*podStaging{}
	for i := range pods {
		pod := &pods[i]
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			continue
		}
		volumes := map[string]bool{}
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
				volumes[v.Name] = true
			}
		}
		if len(volumes) == 0 {
			continue
		}
		recordContainers(byKey, pod, pod.Spec.InitContainers, pod.Status.InitContainerStatuses, volumes, true)
		recordContainers(byKey, pod, pod.Spec.Containers, pod.Status.ContainerStatuses, volumes, false)
	}
	for _, s := range byKey {
		sort.Strings(s.Busy)
		s.Busy = slices.Compact(s.Busy)
	}
	return byKey
}

func recordContainers(
	byKey map[string]*podStaging, pod *corev1.Pod, containers []corev1.Container,
	statuses []corev1.ContainerStatus, volumes map[string]bool, init bool,
) {
	for i := range containers {
		c := &containers[i]
		key, files, ok := containerCacheEntry(c, volumes)
		if !ok {
			continue
		}
		s := byKey[key]
		if s == nil {
			s = &podStaging{}
			byKey[key] = s
		}
		s.Files = append(s.Files, files...)
		// Only an init container stages; a runtime container that reads the
		// claim runs for as long as the pod serves.
		if init && !containerSucceeded(statuses, c.Name) {
			s.Busy = append(s.Busy, pod.Name)
		}
	}
}

// containerCacheEntry returns the cache key a container's CACHE_DIR names
// under its mount of the claim, and the files its MODEL_FILES lists.
func containerCacheEntry(c *corev1.Container, volumes map[string]bool) (string, []string, bool) {
	var cacheDir, modelFiles string
	for _, env := range c.Env {
		switch env.Name {
		case "CACHE_DIR":
			cacheDir = env.Value
		case "MODEL_FILES":
			modelFiles = env.Value
		}
	}
	if cacheDir == "" {
		return "", nil, false
	}
	for _, m := range c.VolumeMounts {
		if !volumes[m.Name] {
			continue
		}
		key, ok := strings.CutPrefix(path.Clean(cacheDir), strings.TrimSuffix(m.MountPath, "/")+"/")
		if !ok || key == "" || strings.Contains(key, "/") {
			continue
		}
		var files []string
		for _, line := range strings.Split(modelFiles, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				files = append(files, line)
			}
		}
		return key, files, true
	}
	return "", nil, false
}

func containerSucceeded(statuses []corev1.ContainerStatus, name string) bool {
	for _, s := range statuses {
		if s.Name == name {
			return s.State.Terminated != nil && s.State.Terminated.ExitCode == 0
		}
	}
	return false
}

// entryKeepSet is the union prune keeps in one entry on one claim: every name
// a Model resolving to the entry stages, records as prefetched, or a live pod
// on the claim lists.
func entryKeepSet(owners *entryOwners, staging *podStaging) map[string]bool {
	keep := map[string]bool{}
	if owners != nil {
		for name := range owners.Keep {
			keep[name] = true
		}
	}
	if staging != nil {
		for _, name := range staging.Files {
			keep[path.Clean(name)] = true
		}
	}
	return keep
}

// entryPrunePlan sorts the files of one cache entry.
type entryPrunePlan struct {
	Key    string
	Models []string
	// Remove are the files no keeper lists and that are not bookkeeping.
	Remove []cacheFile
	// Kept are the files a keeper lists.
	Kept []cacheFile
	// Protected are staging's own files, never removed.
	Protected []cacheFile
	// Bytes is the size of Remove.
	Bytes int64
}

// planEntryPrune decides, for every file present in an entry, whether it is
// kept (listed), protected (staging bookkeeping) or removable. A listed file
// is kept even when its name looks like bookkeeping.
func planEntryPrune(key string, keep map[string]bool, present []cacheFile) entryPrunePlan {
	plan := entryPrunePlan{Key: key}
	for _, f := range present {
		switch {
		case keep[path.Clean(f.Rel)]:
			plan.Kept = append(plan.Kept, f)
		case fileset.IsStagingBookkeeping(f.Rel):
			plan.Protected = append(plan.Protected, f)
		default:
			plan.Remove = append(plan.Remove, f)
			plan.Bytes += f.Size
		}
	}
	for _, fs := range [][]cacheFile{plan.Remove, plan.Kept, plan.Protected} {
		sort.Slice(fs, func(i, j int) bool { return fs[i].Rel < fs[j].Rel })
	}
	return plan
}

// listedEntry is one entry as the listing script read it.
type listedEntry struct {
	State string
	Files []cacheFile
}

// pruneListScript lists every regular file of each entry in keys that exists,
// as "<size> <path>" lines after an "entry <key> <lock state>" header. The
// lock is probed in a subshell, so it is released before the next entry. The
// lockfile is opened for append so NFS, which emulates flock with POSIX
// locks, can take an exclusive lock, and it is never created here.
func pruneListScript(keys []string) string {
	quoted := make([]string, len(keys))
	for i, key := range keys {
		quoted[i] = shellQuote(key)
	}
	return `for k in ` + strings.Join(quoted, " ") + `; do ` +
		`d="` + defaultModelMountPath + `/$k"; [ -d "$d" ] || continue; ` +
		`state=` + pruneLockFree + `; lock="$d/.llmkube-download.lock"; ` +
		`if [ -f "$lock" ]; then if ! command -v flock >/dev/null 2>&1; then state=` + pruneLockNoFlock + `; ` +
		`elif ! ( command exec 9>>"$lock" && flock -n 9 ); then state=` + pruneLockHeld + `; fi; fi; ` +
		`echo "entry $k $state"; find "$d" -type f -exec stat -c '%s %n' {} +; done; exit 0`
}

// parsePruneListing reads pruneListScript's output. A line naming a path
// outside the current entry, or one that does not round-trip as a clean
// relative path, is dropped: prune never removes what it cannot name exactly.
func parsePruneListing(output string) map[string]*listedEntry {
	entries := map[string]*listedEntry{}
	var current *listedEntry
	var prefix string
	for _, line := range strings.Split(output, "\n") {
		if header, ok := strings.CutPrefix(line, "entry "); ok {
			fields := strings.Fields(header)
			if len(fields) != 2 || validateCacheKey(fields[0]) != nil {
				current = nil
				continue
			}
			current = &listedEntry{State: fields[1]}
			entries[fields[0]] = current
			prefix = defaultModelMountPath + "/" + fields[0] + "/"
			continue
		}
		if current == nil {
			continue
		}
		sizeText, file, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		size, err := strconv.ParseInt(sizeText, 10, 64)
		if err != nil {
			continue
		}
		rel, ok := strings.CutPrefix(file, prefix)
		if !ok || !cleanRelativePath(rel) {
			continue
		}
		current.Files = append(current.Files, cacheFile{Rel: rel, Size: size})
	}
	return entries
}

// cleanRelativePath accepts only a path that names a file below the entry in
// exactly one way.
func cleanRelativePath(rel string) bool {
	if rel == "" || path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return false
	}
	for _, r := range rel {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// shellQuote quotes s as one single-quoted sh word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// pruneDeleteScript removes each entry's planned files while holding that
// entry's download lock, the lock every non-FUSE staging pod takes before it
// sweeps, probes or writes. An entry whose lockfile exists but cannot be
// locked reports "busy <key>" and is left alone; one with no lockfile has
// never been staged under a lock. A path is removed only if it is still a
// regular file and not a symlink, and each is reported as removed, absent or
// still present. Directories the removal emptied are removed too; rmdir
// leaves any that still hold a file.
func pruneDeleteScript(plans []entryPrunePlan) string {
	var b strings.Builder
	for _, plan := range plans {
		dir := defaultModelMountPath + "/" + plan.Key
		paths := make([]string, len(plan.Remove))
		for i, f := range plan.Remove {
			paths[i] = shellQuote(dir + "/" + f.Rel)
		}
		b.WriteString(`( lock=` + shellQuote(dir+"/.llmkube-download.lock") + `; ` +
			`if [ -f "$lock" ]; then { command exec 9>>"$lock" && flock -n 9; } || exit 3; fi; ` +
			`for p in ` + strings.Join(paths, " ") + `; do ` +
			`if [ -f "$p" ] && [ ! -L "$p" ]; then rm -f "$p"; ` +
			`if [ -e "$p" ]; then echo "still $p"; else echo "removed $p"; fi; ` +
			`else echo "absent $p"; fi; done; `)
		if dirs := emptiedDirCandidates(dir, plan.Remove); len(dirs) > 0 {
			quoted := make([]string, len(dirs))
			for i, d := range dirs {
				quoted[i] = shellQuote(d)
			}
			b.WriteString(`for p in ` + strings.Join(quoted, " ") + `; do rmdir "$p" 2>/dev/null; done; `)
		}
		b.WriteString(`exit 0 ); [ $? -ne 3 ] || echo ` + shellQuote("busy "+plan.Key) + `; `)
	}
	b.WriteString(`exit 0`)
	return b.String()
}

// emptiedDirCandidates returns the directories below dir that hold a removed
// file, deepest first, so rmdir can empty a chain bottom-up.
func emptiedDirCandidates(dir string, removed []cacheFile) []string {
	seen := map[string]bool{}
	for _, f := range removed {
		for d := path.Dir(f.Rel); d != "." && d != "/"; d = path.Dir(d) {
			seen[d] = true
		}
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/")
		if di != dj {
			return di > dj
		}
		return dirs[i] < dirs[j]
	})
	for i, d := range dirs {
		dirs[i] = dir + "/" + d
	}
	return dirs
}

// pruneDeletion is what the delete script reported.
type pruneDeletion struct {
	Removed []string
	Absent  []string
	Still   []string
	Busy    []string
}

func parsePruneDeletion(output string) pruneDeletion {
	var d pruneDeletion
	for _, line := range strings.Split(output, "\n") {
		if p, ok := strings.CutPrefix(line, "removed "); ok {
			d.Removed = append(d.Removed, p)
		} else if p, ok := strings.CutPrefix(line, "absent "); ok {
			d.Absent = append(d.Absent, p)
		} else if p, ok := strings.CutPrefix(line, "still "); ok {
			d.Still = append(d.Still, p)
		} else if k, ok := strings.CutPrefix(line, "busy "); ok {
			d.Busy = append(d.Busy, k)
		}
	}
	return d
}

// skippedEntry is an entry prune left alone, and why.
type skippedEntry struct {
	Key    string
	Reason string
}

// claimPrunePlan is the plan for one cache claim.
type claimPrunePlan struct {
	Target  purgeTarget
	Entries []entryPrunePlan
	Skipped []skippedEntry
}

func (p claimPrunePlan) removals() (files int, bytes int64) {
	for _, e := range p.Entries {
		files += len(e.Remove)
		bytes += e.Bytes
	}
	return files, bytes
}

// pruneCache plans every claim, shows the plan, and, after confirmation,
// removes the planned files. The Models and pods are read again before the
// removal, so a file a Model started listing, or an entry a download started
// on, while the prompt waited is not removed.
func pruneCache(ctx context.Context, env pruneEnv, opts *cachePruneOptions) error {
	owners, err := pruneOwners(ctx, env.client, opts)
	if err != nil {
		return err
	}
	if len(owners) == 0 {
		_, _ = fmt.Fprintf(env.out, "No multi-file cache entries in namespace '%s'.\n", opts.namespace)
		return nil
	}
	targets, err := pruneTargets(ctx, env.client, opts)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no model cache PVC found in namespace '%s'", opts.namespace)
	}

	var errs []error
	var plans []claimPrunePlan
	for _, target := range targets {
		plan, err := planClaimPrune(ctx, env, opts.namespace, target, owners)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading cache PVC %s: %w", target.Name, err))
			continue
		}
		plans = append(plans, plan)
	}
	files, bytes := reportPrunePlans(env.out, plans)

	if files == 0 {
		_, _ = fmt.Fprintln(env.out, "\nNothing to prune.")
		return errors.Join(errs...)
	}
	if opts.dryRun {
		_, _ = fmt.Fprintf(env.out, "\nDry run: would remove %s (%s); nothing was removed.\n",
			fileCount(files), formatBytes(bytes))
		return errors.Join(errs...)
	}
	if !opts.force && !env.confirm(fmt.Sprintf("\nRemove %s (%s)?", fileCount(files), formatBytes(bytes))) {
		_, _ = fmt.Fprintln(env.out, "Cancelled.")
		return errors.Join(errs...)
	}

	fresh, err := pruneOwners(ctx, env.client, opts)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	var removed int
	for _, plan := range plans {
		if n, _ := plan.removals(); n == 0 {
			continue
		}
		n, err := executeClaimPrune(ctx, env, opts.namespace, plan, fresh)
		removed += n
		if err != nil {
			errs = append(errs, fmt.Errorf("pruning cache PVC %s: %w", plan.Target.Name, err))
		}
	}
	_, _ = fmt.Fprintf(env.out, "\nRemoved %s.\n", fileCount(removed))
	return errors.Join(errs...)
}

func fileCount(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// pruneOwners resolves the entries in scope: every multi-file entry of the
// namespace's Models, or only --model's.
func pruneOwners(
	ctx context.Context, k8sClient client.Client, opts *cachePruneOptions,
) (map[string]*entryOwners, error) {
	list := &inferencev1alpha1.ModelList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(opts.namespace)); err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	owners := entryOwnersByKey(list.Items)
	if opts.model == "" {
		return owners, nil
	}
	var model *inferencev1alpha1.Model
	for i := range list.Items {
		if list.Items[i].Name == opts.model {
			model = &list.Items[i]
		}
	}
	if model == nil {
		return nil, fmt.Errorf("model '%s' not found in namespace '%s'", opts.model, opts.namespace)
	}
	if !fileset.HasMultiFileStaging(model) {
		return nil, fmt.Errorf("model '%s' has no spec.files; cache prune covers only multi-file entries", opts.model)
	}
	key := cachekey.EffectiveKey(model)
	if key == "" || owners[key] == nil {
		return nil, fmt.Errorf("model '%s' has no cache entry (not cached, or a metal model)", opts.model)
	}
	return map[string]*entryOwners{key: owners[key]}, nil
}

// pruneTargets returns the claims to prune. Namespace-wide it is every cache
// claim a purge may target: the discovered shared and perService claims and
// every spec.modelCache.claimName. With --model it is the claims that Model's
// files live on, as `cache clear --model` resolves them. Unlike `cache clear`,
// the namespace-wide mode keeps user-owned claims: an entry in scope is in use,
// so its claim is mounted, and the prune pod is pinned to that pod's node.
func pruneTargets(ctx context.Context, k8sClient client.Client, opts *cachePruneOptions) ([]purgeTarget, error) {
	targets, svcs, err := resolvePurgeTargets(ctx, k8sClient, opts.namespace)
	if err != nil {
		return nil, err
	}
	if opts.model == "" {
		return targets, nil
	}
	claim, staged := modelCacheClaims(ctx, k8sClient, opts.namespace, opts.model)
	return targetedClaims(targets, svcs, opts.model, claim, staged), nil
}

// planClaimPrune lists the in-scope entries on one claim and plans each.
func planClaimPrune(
	ctx context.Context, env pruneEnv, namespace string, target purgeTarget, owners map[string]*entryOwners,
) (claimPrunePlan, error) {
	plan := claimPrunePlan{Target: target}
	pods := &corev1.PodList{}
	if err := env.client.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return plan, fmt.Errorf("failed to list pods: %w", err)
	}
	staging := stagingPodsOnClaim(pods.Items, target.Name)

	keys := make([]string, 0, len(owners))
	for key := range owners {
		if err := validateCacheKey(key); err != nil {
			plan.Skipped = append(plan.Skipped, skippedEntry{Key: key, Reason: err.Error()})
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return plan, nil
	}

	var listed map[string]*listedEntry
	err := withPrunePod(ctx, env, namespace, target, func(ctx context.Context, podName string) error {
		output, err := purgeExec(ctx, env.cfg, env.clientset, namespace, podName, purgePodKind.container,
			[]string{"sh", "-c", pruneListScript(keys)})
		if err != nil {
			return fmt.Errorf("failed to list the cache entries: %w", err)
		}
		listed = parsePruneListing(output)
		return nil
	})
	if err != nil {
		return plan, err
	}

	for _, key := range keys {
		entry := listed[key]
		if entry == nil {
			continue // not on this claim
		}
		if reason := entrySkipReason(owners[key], staging[key], entry.State); reason != "" {
			plan.Skipped = append(plan.Skipped, skippedEntry{Key: key, Reason: reason})
			continue
		}
		p := planEntryPrune(key, entryKeepSet(owners[key], staging[key]), entry.Files)
		p.Models = owners[key].Models
		plan.Entries = append(plan.Entries, p)
	}
	return plan, nil
}

// entrySkipReason says why an entry must be left alone, or "".
func entrySkipReason(owners *entryOwners, staging *podStaging, lockState string) string {
	switch {
	case owners == nil:
		return "no Model uses it any more"
	case owners.Refusal != "":
		return owners.Refusal
	case staging != nil && len(staging.Busy) > 0:
		return "a download is running or pending (pod " + strings.Join(staging.Busy, ", ") + ")"
	case lockState == pruneLockHeld:
		return "its download lock is held, or cannot be taken"
	case lockState == pruneLockNoFlock:
		return "the cache image has no flock to check the download lock"
	case lockState != pruneLockFree:
		return "unknown lock state " + strconv.Quote(lockState)
	}
	return ""
}

// executeClaimPrune removes one claim's planned files, re-checking each entry
// against fresh Models and pods first. It returns how many files it removed.
func executeClaimPrune(
	ctx context.Context, env pruneEnv, namespace string, plan claimPrunePlan, fresh map[string]*entryOwners,
) (int, error) {
	pods := &corev1.PodList{}
	if err := env.client.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("failed to list pods: %w", err)
	}
	staging := stagingPodsOnClaim(pods.Items, plan.Target.Name)

	out := env.out
	_, _ = fmt.Fprintf(out, "\nCache PVC %s:\n", plan.Target.Name)
	var todo []entryPrunePlan
	for _, entry := range plan.Entries {
		if len(entry.Remove) == 0 {
			continue
		}
		if reason := entrySkipReason(fresh[entry.Key], staging[entry.Key], pruneLockFree); reason != "" {
			_, _ = fmt.Fprintf(out, "  %s left alone: %s\n", entry.Key, reason)
			continue
		}
		// Re-plan against the fresh keep set, removing only files that were
		// shown and are still removable.
		keep := entryKeepSet(fresh[entry.Key], staging[entry.Key])
		recheck := planEntryPrune(entry.Key, keep, entry.Remove)
		if len(recheck.Remove) > 0 {
			todo = append(todo, recheck)
		}
	}
	if len(todo) == 0 {
		return 0, nil
	}

	var deletion pruneDeletion
	err := withPrunePod(ctx, env, namespace, plan.Target, func(ctx context.Context, podName string) error {
		output, err := purgeExec(ctx, env.cfg, env.clientset, namespace, podName, purgePodKind.container,
			[]string{"sh", "-c", pruneDeleteScript(todo)})
		if err != nil {
			return fmt.Errorf("failed to remove files: %w", err)
		}
		deletion = parsePruneDeletion(output)
		return nil
	})
	if err != nil {
		return 0, err
	}

	for _, p := range deletion.Removed {
		_, _ = fmt.Fprintf(out, "  removed %s\n", p)
	}
	for _, p := range deletion.Absent {
		_, _ = fmt.Fprintf(out, "  already absent %s\n", p)
	}
	for _, key := range deletion.Busy {
		_, _ = fmt.Fprintf(out, "  %s left alone: a download took its lock\n", key)
	}

	var errs []error
	if len(deletion.Still) > 0 {
		errs = append(errs, fmt.Errorf("files still present after removal: %s", strings.Join(deletion.Still, ", ")))
	}
	if missing := unreportedPaths(todo, deletion); len(missing) > 0 {
		errs = append(errs, fmt.Errorf("no result for %s", strings.Join(missing, ", ")))
	}
	return len(deletion.Removed), errors.Join(errs...)
}

// unreportedPaths lists planned paths the script reported nothing for, other
// than those of an entry it reported busy: an aborted script is not a success.
func unreportedPaths(todo []entryPrunePlan, d pruneDeletion) []string {
	reported := map[string]bool{}
	for _, list := range [][]string{d.Removed, d.Absent, d.Still} {
		for _, p := range list {
			reported[p] = true
		}
	}
	busy := map[string]bool{}
	for _, k := range d.Busy {
		busy[k] = true
	}
	var missing []string
	for _, entry := range todo {
		if busy[entry.Key] {
			continue
		}
		for _, f := range entry.Remove {
			if p := defaultModelMountPath + "/" + entry.Key + "/" + f.Rel; !reported[p] {
				missing = append(missing, p)
			}
		}
	}
	return missing
}

// withPrunePod runs fn in the purge pod of target, then deletes the pod.
// Unlike `cache clear`'s purge pod it is placed like the `cache list`
// inspector: on the node of a pod that holds a non-shareable claim, with that
// pod's tolerations, because an entry prune works on is in use and its claim
// is usually attached.
func withPrunePod(
	ctx context.Context, env pruneEnv, namespace string, target purgeTarget,
	fn func(ctx context.Context, podName string) error,
) error {
	claim := readCacheClaim(ctx, env.client, namespace, target.Name)
	pods := &corev1.PodList{}
	if err := env.client.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}
	holder := claimHolder(pods.Items, target.Name)
	podName := purgePodName(target.Name)
	runID := purgeRunID()
	if err := secondMountBlocker(holder, target.Name, podName, purgePodKind, runID,
		target.SinglePod || claimSinglePod(claim), nil); err != nil {
		return err
	}
	node, tolerations := "", []corev1.Toleration(nil)
	if holder != nil {
		tolerations = append(tolerations, holder.Spec.Tolerations...)
		if !claimShareable(claim) {
			node = holder.Spec.NodeName
		}
	}
	name, err := ensureCacheAccessPod(ctx, env.clientset, namespace, target.Name, podName, purgePodKind, runID,
		func(runID string) (string, error) {
			return createCacheAccessPod(ctx, env.clientset, namespace, target.Name, podName,
				purgePodKind, runID, node, tolerations)
		})
	if err != nil {
		return err
	}
	defer deleteCacheAccessPod(context.Background(), env.clientset, namespace, name)
	return fn(ctx, name)
}

// reportPrunePlans prints each claim's plan and returns the totals.
func reportPrunePlans(out io.Writer, plans []claimPrunePlan) (files int, bytes int64) {
	for _, plan := range plans {
		if len(plan.Entries) == 0 && len(plan.Skipped) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(out, "\nCache PVC %s:\n", plan.Target.Name)
		for _, e := range plan.Entries {
			_, _ = fmt.Fprintf(out, "  %s (Models: %s)\n", e.Key, strings.Join(e.Models, ", "))
			for _, f := range e.Remove {
				_, _ = fmt.Fprintf(out, "    remove  %10s  %s\n", formatBytes(f.Size), f.Rel)
			}
			var keptBytes int64
			for _, f := range e.Kept {
				keptBytes += f.Size
			}
			_, _ = fmt.Fprintf(out, "    keep %d listed (%s) and %d staging bookkeeping\n",
				len(e.Kept), formatBytes(keptBytes), len(e.Protected))
		}
		for _, s := range plan.Skipped {
			_, _ = fmt.Fprintf(out, "  %s skipped: %s\n", s.Key, s.Reason)
		}
		n, b := plan.removals()
		files += n
		bytes += b
	}
	return files, bytes
}
