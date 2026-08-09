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
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// Image input for coder agents (#1466).
//
// The motivating case: a coder writes a page, something renders wrong, and the
// only way anyone has caught that so far is a human screenshotting it and
// describing the defect in prose. Attaching the screenshot itself lets a
// vision-capable model see what it built. The fleet's own coder already
// supports this (it ships a projector); the harness was the missing half.

const (
	// defaultMaxImageBytes caps each image AFTER base64 encoding. 4MiB is
	// roughly a full-page screenshot at 2x; beyond that the image is
	// competing with the conversation for the context window.
	defaultMaxImageBytes int64 = 4 << 20

	// defaultMaxImages bounds how many images ride on one message.
	defaultMaxImages int32 = 4
)

// visionEnabled reports whether this Agent opted into image input.
//
// Explicit rather than inferred: an Agent points at a base URL, and the
// harness cannot introspect an arbitrary endpoint to learn whether a
// multimodal projector is loaded. Guessing wrong sends image parts to a
// text-only model, which is an HTTP 400 rather than a graceful degrade.
func visionEnabled(agent *foremanv1alpha1.Agent) bool {
	return agent != nil && agent.Spec.Vision != nil && agent.Spec.Vision.Enabled
}

func visionLimits(agent *foremanv1alpha1.Agent) (maxBytes int64, maxImages int32) {
	maxBytes, maxImages = defaultMaxImageBytes, defaultMaxImages
	if agent == nil || agent.Spec.Vision == nil {
		return maxBytes, maxImages
	}
	if agent.Spec.Vision.MaxImageBytes > 0 {
		maxBytes = agent.Spec.Vision.MaxImageBytes
	}
	if agent.Spec.Vision.MaxImages > 0 {
		maxImages = agent.Spec.Vision.MaxImages
	}
	return maxBytes, maxImages
}

// buildUserContentParts renders the user prompt plus any attached images as
// multimodal content parts, or returns nil to mean "use the ordinary string
// content path".
//
// Returning nil for the no-image case matters: it keeps every existing run
// byte-identical on the wire, so this feature cannot regress text-only agents.
//
// Every failure here degrades to a warning rather than an error. A screenshot
// that did not get written should not fail a task whose prompt still describes
// the problem in words; running without the picture is strictly better than
// not running.
func buildUserContentParts(
	prompt string, images []string, workspace string, agent *foremanv1alpha1.Agent,
) (parts []oai.ContentPart, warnings []string) {
	if len(images) == 0 {
		return nil, nil
	}
	if !visionEnabled(agent) {
		return nil, []string{fmt.Sprintf(
			"%d image(s) attached but this agent has vision disabled "+
				"(set spec.vision.enabled); they were not sent to the model", len(images))}
	}

	maxBytes, maxImages := visionLimits(agent)
	parts = []oai.ContentPart{{Type: oai.ContentPartText, Text: prompt}}

	var attached int32
	for _, rel := range images {
		if attached >= maxImages {
			warnings = append(warnings, fmt.Sprintf(
				"image %q skipped: already at the %d-image limit", rel, maxImages))
			continue
		}
		abs, err := resolveInWorkspace(workspace, rel)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("image %q skipped: %v", rel, err))
			continue
		}
		raw, err := os.ReadFile(abs) //nolint:gosec // path is confined to the workspace above
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("image %q skipped: %v", rel, err))
			continue
		}
		encoded := base64.StdEncoding.EncodeToString(raw)
		// Measured after encoding, because that is what actually travels and
		// what the context window pays for; base64 inflates by about a third.
		if int64(len(encoded)) > maxBytes {
			warnings = append(warnings, fmt.Sprintf(
				"image %q skipped: %d encoded bytes exceeds the %d-byte limit",
				rel, len(encoded), maxBytes))
			continue
		}
		parts = append(parts, oai.ContentPart{
			Type: oai.ContentPartImageURL,
			ImageURL: &oai.ImageURL{
				URL: fmt.Sprintf("data:%s;base64,%s", sniffImageMIME(raw), encoded),
			},
		})
		attached++
	}

	// Nothing survived: fall back to the string path rather than sending a
	// lone text part, so the request stays in the shape every backend agrees on.
	if attached == 0 {
		return nil, warnings
	}
	return parts, warnings
}

// resolveInWorkspace joins rel onto workspace and refuses anything that escapes
// it. Payload-supplied paths are operator input, but a task that can read
// arbitrary host files and ship them to an inference endpoint is a data-
// exfiltration primitive, so the confinement is enforced rather than assumed.
func resolveInWorkspace(workspace, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	// Lexical check first: cheap, touches no filesystem, and it rejects the
	// obvious "../../etc/passwd" form.
	abs := filepath.Join(root, rel)
	if !withinRoot(abs, root) {
		return "", fmt.Errorf("path escapes the workspace")
	}

	// Then the real one. filepath.Join resolves nothing, so a symlink sitting
	// inside the workspace and pointing outside it passes the check above
	// while still reading an arbitrary host file. The coder has a bash tool
	// and writes into this workspace, and cloned repositories routinely carry
	// symlinks, so that is reachable rather than theoretical.
	//
	// The root is resolved too, because it is very often a link itself
	// (/tmp -> /private/tmp on macOS), and comparing a resolved child against
	// an unresolved root would reject every legitimate path.
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	absReal, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if !withinRoot(absReal, rootReal) {
		return "", fmt.Errorf("path escapes the workspace through a symlink")
	}
	// Returning the resolved path means the read cannot follow a link that
	// gets swapped in afterwards; a parent directory replaced between this
	// check and the read would still win, which needs openat2/RESOLVE_BENEATH
	// and is Linux-only. Not worth it for a workspace the agent owns.
	return absReal, nil
}

func withinRoot(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+string(os.PathSeparator))
}

// sniffImageMIME picks the data-URL media type from the file's own bytes
// rather than its extension, since a screenshot written by a tool may be named
// anything. Falls back to image/png, the format every renderer here emits.
func sniffImageMIME(data []byte) string {
	mime := http.DetectContentType(data)
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	if !strings.HasPrefix(mime, "image/") {
		return "image/png"
	}
	return mime
}
