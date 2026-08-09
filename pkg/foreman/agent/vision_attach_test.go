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

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// Attaching images to the first user message (#1466).
//
// Every rule here exists because the failure mode is expensive: a base64 image
// is large, it lands in the context window the fleet was carefully sized for,
// and a text-only model given image parts returns HTTP 400 rather than
// degrading. So the default is off, the caps are real, and anything unreadable
// is skipped rather than failing the task.

func writePNG(t *testing.T, dir, name string, size int) {
	t.Helper()
	// A PNG magic header plus filler: enough for the type sniff, and the size
	// is what the cap test cares about.
	data := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, make([]byte, size)...)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func visionAgent(max int64, count int32) *foremanv1alpha1.Agent {
	return &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{
		Vision: &foremanv1alpha1.VisionSpec{Enabled: true, MaxImageBytes: max, MaxImages: count},
	}}
}

func TestAttachImages_BuildsContentParts(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "shot.png", 64)

	parts, warns := buildUserContentParts("look at this", []string{"shot.png"}, dir, visionAgent(0, 0))
	if len(parts) != 2 {
		t.Fatalf("want text + image parts, got %d (warns: %v)", len(parts), warns)
	}
	if parts[0].Type != "text" || parts[0].Text != "look at this" {
		t.Errorf("first part should be the prompt text: %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Fatalf("second part should be an image: %+v", parts[1])
	}
	if !strings.HasPrefix(parts[1].ImageURL.URL, "data:image/png;base64,") {
		t.Errorf("image must be a self-contained data URL so the server fetches nothing: %.60s",
			parts[1].ImageURL.URL)
	}
}

// Off by default. An agent with no vision block must never get image parts,
// however many the payload names.
func TestAttachImages_DisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "shot.png", 64)

	for _, a := range []*foremanv1alpha1.Agent{
		{},
		{Spec: foremanv1alpha1.AgentSpec{Vision: &foremanv1alpha1.VisionSpec{Enabled: false}}},
	} {
		parts, warns := buildUserContentParts("prompt", []string{"shot.png"}, dir, a)
		if parts != nil {
			t.Errorf("vision disabled but parts were built: %+v", parts)
		}
		if len(warns) == 0 {
			t.Error("dropping images silently hides why the model cannot see them")
		}
	}
}

// No images means no parts at all, so the ordinary string-content path is used
// and nothing about existing runs changes.
func TestAttachImages_NoImagesMeansNoParts(t *testing.T) {
	parts, warns := buildUserContentParts("prompt", nil, t.TempDir(), visionAgent(0, 0))
	if parts != nil || len(warns) != 0 {
		t.Errorf("expected the plain string path: parts=%v warns=%v", parts, warns)
	}
}

func TestAttachImages_CapsSizeAndCount(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "small.png", 32)
	writePNG(t, dir, "huge.png", 8192)

	// Size cap: the oversized one is skipped, the small one survives.
	parts, warns := buildUserContentParts("p", []string{"small.png", "huge.png"}, dir, visionAgent(1024, 0))
	if got := countImageParts(parts); got != 1 {
		t.Errorf("oversized image not skipped: %d image parts", got)
	}
	if len(warns) == 0 {
		t.Error("skipping an oversized image must be reported")
	}

	// Count cap.
	writePNG(t, dir, "a.png", 32)
	writePNG(t, dir, "b.png", 32)
	parts, _ = buildUserContentParts("p", []string{"small.png", "a.png", "b.png"}, dir, visionAgent(0, 2))
	if got := countImageParts(parts); got != 2 {
		t.Errorf("count cap not applied: %d image parts", got)
	}
}

// A missing or unreadable file degrades to a warning. Failing the whole task
// because a screenshot did not get written would be worse than running without
// it, since the prompt still describes the problem.
func TestAttachImages_MissingFileDegrades(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "real.png", 32)

	parts, warns := buildUserContentParts("p", []string{"gone.png", "real.png"}, dir, visionAgent(0, 0))
	if got := countImageParts(parts); got != 1 {
		t.Errorf("the readable image should still be attached: %d image parts", got)
	}
	if len(warns) == 0 {
		t.Error("a missing image must be reported rather than silently dropped")
	}
}

// Path traversal must not read outside the workspace.
func TestAttachImages_RefusesEscapingPaths(t *testing.T) {
	dir := t.TempDir()
	parts, warns := buildUserContentParts("p", []string{"../../etc/passwd"}, dir, visionAgent(0, 0))
	if countImageParts(parts) != 0 {
		t.Error("a path escaping the workspace was read")
	}
	if len(warns) == 0 {
		t.Error("refusing an escaping path must be reported")
	}
}

// A symlink INSIDE the workspace pointing outside it must be refused. The
// lexical prefix check alone passes this: filepath.Join resolves nothing, so
// "shot.png" that happens to be a link to /etc/passwd looks perfectly confined.
//
// Reachable in practice: the coder has a bash tool and writes into this
// workspace, and cloned repositories routinely contain symlinks. Reading an
// arbitrary host file and shipping it to an inference endpoint is the
// exfiltration primitive the confinement exists to prevent.
func TestAttachImages_RefusesSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.png")
	writePNG(t, filepath.Dir(outside), "secret.png", 32)

	link := filepath.Join(dir, "innocent.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	parts, warns := buildUserContentParts("p", []string{"innocent.png"}, dir, visionAgent(0, 0))
	if countImageParts(parts) != 0 {
		t.Error("a symlink pointing outside the workspace was read and attached")
	}
	if len(warns) == 0 {
		t.Error("refusing a symlink escape must be reported")
	}
}

// A symlink that stays inside the workspace is legitimate and must still work,
// so the fix cannot simply refuse all links.
func TestAttachImages_AllowsSymlinkWithinWorkspace(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "real.png", 32)
	if err := os.Symlink(filepath.Join(dir, "real.png"), filepath.Join(dir, "alias.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	parts, warns := buildUserContentParts("p", []string{"alias.png"}, dir, visionAgent(0, 0))
	if countImageParts(parts) != 1 {
		t.Errorf("an in-workspace symlink should be readable (warns: %v)", warns)
	}
}
