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
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
)

// pruneHarness wires pruneCache to fakes: a cluster holding the shared cache
// claim and objs, a scripted purge pod, and a confirmation answer.
type pruneHarness struct {
	t         *testing.T
	client    client.Client
	clientset *fakeclientset.Clientset
	out       bytes.Buffer
	// listing is what the listing script prints.
	listing string
	// deleteOutput answers the delete script; nil echoes "removed" for every
	// path in it.
	deleteOutput func(script string) string
	// confirmed answers the prompt; onConfirm runs while it is open.
	confirmed bool
	onConfirm func()
	prompts   []string
	listCmds  []string
	delCmds   []string
}

func newPruneHarness(t *testing.T, objs ...client.Object) *pruneHarness {
	t.Helper()
	shared := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: modelCachePVCName, Namespace: "default",
		Labels: map[string]string{modelCacheLabel: modelCacheLabelValue},
	}}
	h := &pruneHarness{
		t:         t,
		client:    fake.NewClientBuilder().WithScheme(newCLIScheme()).WithObjects(append(objs, shared)...).Build(),
		clientset: fakeclientset.NewClientset(),
		confirmed: true,
	}
	fakePurgeExec(t, func(command []string) (string, error) {
		script := command[2]
		switch {
		case strings.Contains(script, "find "):
			h.listCmds = append(h.listCmds, script)
			return h.listing, nil
		case strings.Contains(script, "rm -f"):
			h.delCmds = append(h.delCmds, script)
			if h.deleteOutput != nil {
				return h.deleteOutput(script), nil
			}
			return echoRemoved(script), nil
		}
		t.Fatalf("unexpected script: %s", script)
		return "", nil
	}, &[][]string{})
	return h
}

// echoRemoved answers a delete script as if every path in its rm loops was
// removed.
func echoRemoved(script string) string {
	var out strings.Builder
	for _, part := range strings.Split(script, "for p in ")[1:] {
		list, body, _ := strings.Cut(part, "; do ")
		if !strings.HasPrefix(body, "if [ -f") {
			continue // the rmdir loop
		}
		for _, word := range shellWords(list) {
			out.WriteString("removed " + word + "\n")
		}
	}
	return out.String()
}

// shellWords splits a list of sh words made of single-quoted runs and \'.
func shellWords(list string) []string {
	var words []string
	var cur strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(list); i++ {
		c := list[i]
		switch {
		case quoted && c == '\'':
			quoted = false
		case quoted:
			cur.WriteByte(c)
		case c == '\'':
			quoted, inWord = true, true
		case c == '\\' && i+1 < len(list):
			i++
			cur.WriteByte(list[i])
			inWord = true
		case c == ' ':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

func (h *pruneHarness) run(opts cachePruneOptions) error {
	h.t.Helper()
	if opts.namespace == "" {
		opts.namespace = "default"
	}
	return pruneCache(context.Background(), pruneEnv{
		cfg: nilRESTConfig, client: h.client, clientset: h.clientset, out: &h.out,
		confirm: func(prompt string) bool {
			h.prompts = append(h.prompts, prompt)
			if h.onConfirm != nil {
				h.onConfirm()
			}
			return h.confirmed
		},
	}, &opts)
}

func modelObj(m inferencev1alpha1.Model) *inferencev1alpha1.Model { return &m }

// listingFor renders what the listing script prints for one entry.
func listingFor(key, lock string, fileSizes map[string]int64) string {
	var b strings.Builder
	b.WriteString("entry " + key + " " + lock + "\n")
	for rel, size := range fileSizes {
		b.WriteString(strconv.FormatInt(size, 10) + " /models/" + key + "/" + rel + "\n")
	}
	return b.String()
}

var pruneKey = cachekey.Compute(pruneTestSource)

func TestPruneCache_DryRunListsAndRemovesNothing(t *testing.T) {
	h := newPruneHarness(t, modelObj(pruneModel("m", pruneTestSource, "keep.safetensors")))
	h.listing = listingFor(pruneKey, pruneLockFree, map[string]int64{
		"keep.safetensors":           100,
		"dropped.safetensors":        2048,
		"dropped.safetensors.sha256": 80,
		".llmkube-download.lock":     0,
	})

	if err := h.run(cachePruneOptions{dryRun: true}); err != nil {
		t.Fatalf("pruneCache: %v", err)
	}
	if len(h.delCmds) != 0 {
		t.Fatalf("dry run ran a delete script: %v", h.delCmds)
	}
	if len(h.prompts) != 0 {
		t.Fatalf("dry run asked for confirmation: %v", h.prompts)
	}
	out := h.out.String()
	for _, want := range []string{"remove", "dropped.safetensors", "2.0 KiB", "Dry run", "1 file"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "remove") && strings.Contains(line, ".sha256") {
			t.Errorf("output plans to remove a stamp: %q", line)
		}
	}
}

func TestPruneCache_ForceRemovesOnlyUnlistedArtifactsUnderTheLock(t *testing.T) {
	h := newPruneHarness(t, modelObj(pruneModel("m", pruneTestSource, "keep.safetensors")))
	h.listing = listingFor(pruneKey, pruneLockFree, map[string]int64{
		"keep.safetensors":            100,
		"sub/dir/dropped.safetensors": 2048,
		"it's.bin":                    1,
		"dropped.safetensors.sha256":  80,
		"partial.0123456789ab.tmp":    9,
		".llmkube-download.lock":      0,
	})

	if err := h.run(cachePruneOptions{force: true}); err != nil {
		t.Fatalf("pruneCache: %v", err)
	}
	if len(h.prompts) != 0 {
		t.Fatalf("--force asked for confirmation: %v", h.prompts)
	}
	if len(h.delCmds) != 1 {
		t.Fatalf("delete scripts = %d, want 1", len(h.delCmds))
	}
	script := h.delCmds[0]
	for _, want := range []string{
		"'/models/" + pruneKey + "/sub/dir/dropped.safetensors'",
		`'/models/` + pruneKey + `/it'\''s.bin'`,
		"'/models/" + pruneKey + "/sub/dir' '/models/" + pruneKey + "/sub'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("delete script lacks %s:\n%s", want, script)
		}
	}
	for _, never := range []string{"keep.safetensors", ".sha256'", ".tmp'", "download.lock' "} {
		if strings.Contains(script, never) {
			t.Errorf("delete script names %q:\n%s", never, script)
		}
	}
	lock := strings.Index(script, "flock -n 9")
	rm := strings.Index(script, "rm -f")
	if lock < 0 || rm < 0 || lock > rm {
		t.Errorf("delete script does not take the download lock before removing:\n%s", script)
	}
	if !strings.Contains(h.out.String(), "removed /models/"+pruneKey+"/sub/dir/dropped.safetensors") {
		t.Errorf("output does not report the removal:\n%s", h.out.String())
	}
	// Both phases deleted their pod.
	if _, err := h.clientset.CoreV1().Pods("default").Get(context.Background(),
		purgePodName(modelCachePVCName), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("purge pod left behind: err = %v", err)
	}
}

func TestPruneCache_DeclinedConfirmationRemovesNothing(t *testing.T) {
	h := newPruneHarness(t, modelObj(pruneModel("m", pruneTestSource, "keep.safetensors")))
	h.listing = listingFor(pruneKey, pruneLockFree, map[string]int64{"keep.safetensors": 1, "old.bin": 1})
	h.confirmed = false

	if err := h.run(cachePruneOptions{}); err != nil {
		t.Fatalf("pruneCache: %v", err)
	}
	if len(h.prompts) != 1 {
		t.Fatalf("prompts = %v, want one confirmation", h.prompts)
	}
	if len(h.delCmds) != 0 {
		t.Fatalf("declined prune ran a delete script: %v", h.delCmds)
	}
}

func TestPruneCache_SkipsAnEntryWhoseLockIsHeld(t *testing.T) {
	h := newPruneHarness(t, modelObj(pruneModel("m", pruneTestSource, "keep.safetensors")))
	h.listing = listingFor(pruneKey, pruneLockHeld, map[string]int64{"keep.safetensors": 1, "old.bin": 1})

	if err := h.run(cachePruneOptions{force: true}); err != nil {
		t.Fatalf("pruneCache: %v", err)
	}
	if len(h.delCmds) != 0 {
		t.Fatalf("prune ran a delete script on a locked entry: %v", h.delCmds)
	}
	if !strings.Contains(h.out.String(), "download lock is held") {
		t.Errorf("output does not say why the entry was skipped:\n%s", h.out.String())
	}
}

func TestPruneCache_SkipsAnEntryAPodIsDownloadingInto(t *testing.T) {
	pod := downloaderPod("isvc-abc", modelCachePVCName, pruneKey, stateRunning, "keep.safetensors")
	h := newPruneHarness(t, modelObj(pruneModel("m", pruneTestSource, "keep.safetensors")), &pod)
	h.listing = listingFor(pruneKey, pruneLockFree, map[string]int64{"keep.safetensors": 1, "old.bin": 1})

	if err := h.run(cachePruneOptions{force: true}); err != nil {
		t.Fatalf("pruneCache: %v", err)
	}
	if len(h.delCmds) != 0 {
		t.Fatalf("prune ran a delete script while a pod downloads: %v", h.delCmds)
	}
	if !strings.Contains(h.out.String(), "isvc-abc") {
		t.Errorf("output does not name the downloading pod:\n%s", h.out.String())
	}
}

func TestPruneCache_RechecksModelsAfterConfirmation(t *testing.T) {
	model := pruneModel("m", pruneTestSource, "keep.safetensors")
	h := newPruneHarness(t, &model)
	h.listing = listingFor(pruneKey, pruneLockFree, map[string]int64{
		"keep.safetensors": 1, "old-a.bin": 1, "old-b.bin": 1,
	})
	// While the prompt is open, someone adds old-a.bin back to the Model.
	h.onConfirm = func() {
		live := &inferencev1alpha1.Model{}
		if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(&model), live); err != nil {
			t.Fatalf("get model: %v", err)
		}
		live.Spec.Files = append(live.Spec.Files, "old-a.bin")
		if err := h.client.Update(context.Background(), live); err != nil {
			t.Fatalf("update model: %v", err)
		}
	}

	if err := h.run(cachePruneOptions{}); err != nil {
		t.Fatalf("pruneCache: %v", err)
	}
	if len(h.delCmds) != 1 {
		t.Fatalf("delete scripts = %d, want 1", len(h.delCmds))
	}
	if strings.Contains(h.delCmds[0], "old-a.bin") {
		t.Fatalf("prune removed a file a Model listed by the time it confirmed:\n%s", h.delCmds[0])
	}
	if !strings.Contains(h.delCmds[0], "old-b.bin") {
		t.Fatalf("prune dropped a still-removable file:\n%s", h.delCmds[0])
	}
}

func TestPruneCache_ReportsAnEntryThatBecameBusy(t *testing.T) {
	h := newPruneHarness(t, modelObj(pruneModel("m", pruneTestSource, "keep.safetensors")))
	h.listing = listingFor(pruneKey, pruneLockFree, map[string]int64{"keep.safetensors": 1, "old.bin": 1})
	h.deleteOutput = func(string) string { return "busy " + pruneKey + "\n" }

	if err := h.run(cachePruneOptions{force: true}); err != nil {
		t.Fatalf("pruneCache: %v", err)
	}
	if !strings.Contains(h.out.String(), "a download took its lock") {
		t.Errorf("output does not report the busy entry:\n%s", h.out.String())
	}
	if !strings.Contains(h.out.String(), "Removed 0 files") {
		t.Errorf("output claims removals:\n%s", h.out.String())
	}
}

func TestPruneCache_FailsWhenAFileSurvivesOrIsUnreported(t *testing.T) {
	for name, output := range map[string]string{
		"still":      "still /models/" + pruneKey + "/old.bin\n",
		"unreported": "",
	} {
		t.Run(name, func(t *testing.T) {
			h := newPruneHarness(t, modelObj(pruneModel("m", pruneTestSource, "keep.safetensors")))
			h.listing = listingFor(pruneKey, pruneLockFree, map[string]int64{"keep.safetensors": 1, "old.bin": 1})
			h.deleteOutput = func(string) string { return output }
			if err := h.run(cachePruneOptions{force: true}); err == nil {
				t.Fatalf("pruneCache succeeded although old.bin was not removed")
			}
		})
	}
}

func TestPruneCache_ModelFlag(t *testing.T) {
	single := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "single", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: "0123456789abcdef"},
	}
	other := pruneModel("other", "hf://org/other", "a.safetensors")
	h := newPruneHarness(t, single, modelObj(pruneModel("m", pruneTestSource, "keep.safetensors")), &other)

	if err := h.run(cachePruneOptions{model: "single", dryRun: true}); err == nil ||
		!strings.Contains(err.Error(), "multi-file") {
		t.Fatalf("pruneCache(--model single-file) = %v, want a multi-file scope error", err)
	}

	h.listing = listingFor(pruneKey, pruneLockFree, map[string]int64{"keep.safetensors": 1, "old.bin": 1})
	if err := h.run(cachePruneOptions{model: "m", dryRun: true}); err != nil {
		t.Fatalf("pruneCache(--model m): %v", err)
	}
	if len(h.listCmds) != 1 || strings.Contains(h.listCmds[0], cachekey.EffectiveKey(&other)) {
		t.Fatalf("--model listed other entries: %v", h.listCmds)
	}
}

func TestPruneListScript_ProbesTheLockWithoutCreatingIt(t *testing.T) {
	script := pruneListScript([]string{pruneKey})
	for _, want := range []string{
		`if [ -f "$lock" ]`,   // never creates a lockfile
		`9>>"$lock"`,          // append: no truncation, write mode for NFS
		"flock -n 9",          // non-blocking: report, do not wait
		"find \"$d\" -type f", // regular files only, symlinks not followed
		"'" + pruneKey + "'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("listing script lacks %s:\n%s", want, script)
		}
	}
	if strings.Contains(script, "rm ") {
		t.Errorf("listing script removes something:\n%s", script)
	}
}
