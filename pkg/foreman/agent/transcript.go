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
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// TranscriptSchemaVersion identifies the on-disk shape of the transcript
// payload. The Kubernetes apiserver caps a single ConfigMap at 1 MiB
// total (including metadata); we leave 16 KiB of headroom for that
// metadata and the truncation marker so a runaway transcript fails
// gracefully rather than returning a 413 from the apiserver.
const (
	TranscriptSchemaVersion = "foreman.transcript.v1"

	transcriptCapBytes = (1 << 20) - (16 << 10) // 1 MiB - 16 KiB

	// transcriptDataKey is the well-known key inside the ConfigMap's Data
	// map where the JSON transcript lives. A reader doing
	//   `kubectl get cm <name> -o jsonpath='{.data.transcript\.json}'`
	// gets exactly that payload.
	transcriptDataKey = "transcript.json"

	// transcriptMetaKey carries the small summary block (turn count +
	// schema version + truncated flag) for cheap inspection without
	// parsing the full transcript.
	transcriptMetaKey = "meta.json"
)

// TranscriptDoc is the JSON payload stored at data["transcript.json"].
type TranscriptDoc struct {
	SchemaVersion string        `json:"schemaVersion"`
	TurnCount     int           `json:"turnCount"`
	Truncated     bool          `json:"truncated,omitempty"`
	Messages      []oai.Message `json:"messages"`
}

// TranscriptMeta is the small block stored at data["meta.json"] for
// cheap inspection. Reading just this avoids parsing the full
// transcript when all a tool wants is "did this fit?".
type TranscriptMeta struct {
	SchemaVersion string `json:"schemaVersion"`
	TurnCount     int    `json:"turnCount"`
	Truncated     bool   `json:"truncated"`
	BytesStored   int    `json:"bytesStored"`
}

// transcriptName returns the ConfigMap name for a task. We prefix to
// keep transcript ConfigMaps trivially discoverable
// (`kubectl get cm -l foreman.llmkube.dev/transcript-of=<task>`).
func transcriptName(taskName string) string {
	const prefix = "foreman-transcript-"
	// ConfigMap name is a DNS-1123 subdomain (max 253). Task names are
	// already a DNS-1123 label (max 63), so prefix+name comfortably fits.
	return prefix + taskName
}

// WriteTranscript creates or updates a ConfigMap owner-ref'd to the
// given task carrying the loop's transcript. Returns the LocalObject
// reference the executor surfaces in Result.Extra.
//
// Over-budget transcripts are truncated from the middle. The head
// (system prompt + first turns) and tail (last turns + the
// submit_result envelope, when present) are the high-signal parts;
// dropping the middle preserves the start-and-end context a reviewer
// or a downstream evaluator most needs to see.
func WriteTranscript(
	ctx context.Context,
	c client.Client,
	task *foremanv1alpha1.AgenticTask,
	loopResult *LoopResult,
) (corev1.ObjectReference, error) {
	if task == nil || loopResult == nil {
		return corev1.ObjectReference{}, fmt.Errorf("WriteTranscript: task and loopResult required")
	}

	doc := TranscriptDoc{
		SchemaVersion: TranscriptSchemaVersion,
		TurnCount:     loopResult.Turns,
		Messages:      loopResult.Transcript,
	}
	docBytes, err := json.Marshal(doc)
	if err != nil {
		return corev1.ObjectReference{}, fmt.Errorf("WriteTranscript: marshal: %w", err)
	}
	if len(docBytes) > transcriptCapBytes {
		doc.Messages = truncateMessages(doc.Messages, loopResult.Turns)
		doc.Truncated = true
		docBytes, err = json.Marshal(doc)
		if err != nil {
			return corev1.ObjectReference{}, fmt.Errorf("WriteTranscript: marshal truncated: %w", err)
		}
		// Final guard: if even the truncated form is still over budget
		// (huge single messages), drop messages until it fits. Pathological
		// case; preserve at least the system prompt.
		for len(docBytes) > transcriptCapBytes && len(doc.Messages) > 1 {
			doc.Messages = append(doc.Messages[:1], doc.Messages[len(doc.Messages)-1])
			docBytes, _ = json.Marshal(doc)
		}
	}

	meta := TranscriptMeta{
		SchemaVersion: TranscriptSchemaVersion,
		TurnCount:     loopResult.Turns,
		Truncated:     doc.Truncated,
		BytesStored:   len(docBytes),
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return corev1.ObjectReference{}, fmt.Errorf("WriteTranscript: marshal meta: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      transcriptName(task.Name),
			Namespace: task.Namespace,
			Labels: map[string]string{
				"foreman.llmkube.dev/transcript-of": task.Name,
				"app.kubernetes.io/part-of":         "foreman",
			},
			OwnerReferences: []metav1.OwnerReference{ownerRefForTask(task)},
		},
		Data: map[string]string{
			transcriptDataKey: string(docBytes),
			transcriptMetaKey: string(metaBytes),
		},
	}

	// Idempotent upsert: create on first run, update if a prior run on
	// the same task wrote one (resumed task, retry-driven re-execute).
	existing := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: cm.Namespace, Name: cm.Name}
	getErr := c.Get(ctx, key, existing)
	switch {
	case apierrors.IsNotFound(getErr):
		if err := c.Create(ctx, cm); err != nil {
			return corev1.ObjectReference{}, fmt.Errorf("WriteTranscript: create cm: %w", err)
		}
	case getErr == nil:
		existing.Data = cm.Data
		existing.Labels = cm.Labels
		// OwnerReferences are immutable through merge patches when the
		// UID changes; for our case the task UID is stable across the
		// run, so an Update is safe.
		existing.OwnerReferences = cm.OwnerReferences
		if err := c.Update(ctx, existing); err != nil {
			return corev1.ObjectReference{}, fmt.Errorf("WriteTranscript: update cm: %w", err)
		}
	default:
		return corev1.ObjectReference{}, fmt.Errorf("WriteTranscript: get cm: %w", getErr)
	}

	return corev1.ObjectReference{
		Kind:       "ConfigMap",
		APIVersion: "v1",
		Namespace:  cm.Namespace,
		Name:       cm.Name,
	}, nil
}

// ownerRefForTask builds the OwnerReference that ties a transcript
// ConfigMap to its AgenticTask so the GC reaps it when the task is
// deleted. BlockOwnerDeletion is true so a "delete the task" call does
// not return before the cm is also marked for deletion.
func ownerRefForTask(task *foremanv1alpha1.AgenticTask) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{
		APIVersion:         foremanv1alpha1.GroupVersion.String(),
		Kind:               "AgenticTask",
		Name:               task.Name,
		UID:                task.UID,
		Controller:         &yes,
		BlockOwnerDeletion: &yes,
	}
}

// truncateMessages keeps the first message (system prompt) and as much
// of the tail as fits. It inserts a synthetic system marker noting how
// many turns were dropped so a reader can tell at a glance.
//
// v0.1 uses a fixed split: keep system + first user message + last 10
// messages. That covers the typical "what was the task" + "how did it
// end" pair and is bounded regardless of input size.
func truncateMessages(msgs []oai.Message, totalTurns int) []oai.Message {
	if len(msgs) <= 12 {
		// Nothing meaningful to drop.
		return msgs
	}
	head := msgs[:2] // system + first user prompt
	tail := msgs[len(msgs)-10:]
	dropped := len(msgs) - len(head) - len(tail)
	marker := oai.Message{
		Role: oai.RoleSystem,
		Content: fmt.Sprintf(
			"[transcript truncated: %d middle messages dropped to fit "+
				"the 1 MiB ConfigMap budget; %d turns total]",
			dropped, totalTurns),
	}
	out := make([]oai.Message, 0, len(head)+1+len(tail))
	out = append(out, head...)
	out = append(out, marker)
	out = append(out, tail...)
	return out
}
