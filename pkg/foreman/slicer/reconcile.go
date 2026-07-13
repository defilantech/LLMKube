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

// Package slicer reconciles a sliced-and-unioned change against the slice
// plan's pinned shared identifiers, catching cross-slice interface drift a
// build cannot see (a dashboard querying a metric an exporter never emits, a
// slice using a config key a sibling spelled differently). Ported from the
// validated slicer experiment; the deterministic check is authoritative and
// the injected-LLM sweep is advisory.
package slicer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Drift kinds.
const (
	// DriftPinnedMissing is emitted by PinnedCheck: a pinned identifier is
	// absent from a slice that must define or reference it. Deterministic and
	// authoritative.
	DriftPinnedMissing = "pinned-missing"
	// DriftPinnedPrefix is emitted by PinnedCheck: the pin is a strict prefix
	// of an in-file token (e.g. pin "rocm_smi" against "rocm_smi_sensor").
	// The planner needs to extend the pin past the identifier boundary.
	// Deterministic and authoritative.
	DriftPinnedPrefix = "pinned-prefix"
	// DriftLLMFlagged is emitted by LLMSweep: the model flagged unpinned drift.
	// Advisory; it can be a false positive and is adjudicated by human review.
	DriftLLMFlagged = "llm-flagged"
)

// Drift is one cross-slice inconsistency.
type Drift struct {
	Identifier string
	Slice      string
	File       string
	Kind       string
}

// SharedIdentifier pins one exact string that crosses a slice boundary. The
// slice named by DefinedBy and every slice in ReferencedBy must contain the
// exact ID.
type SharedIdentifier struct {
	ID           string   `yaml:"id"`
	DefinedBy    string   `yaml:"defined_by"`
	ReferencedBy []string `yaml:"referenced_by"`
}

// Slice is one disjoint-file unit of a plan.
type Slice struct {
	Name  string   `yaml:"name"`
	Files []string `yaml:"files"`
}

// SlicePlan is the planner's output: the prose contract, the pinned shared
// identifiers, and the disjoint-file slices.
type SlicePlan struct {
	Contract          string             `yaml:"contract"`
	SharedIdentifiers []SharedIdentifier `yaml:"shared_identifiers"`
	Slices            []Slice            `yaml:"slices"`
}

// ModelCaller is the injected LLM transport: prompt in, completion out. The
// library never constructs a client or makes a network call; the experiment
// passes a curl-to-reviewer closure and Foreman passes its own model client.
type ModelCaller interface {
	Call(prompt string) (string, error)
}

// Result is the reconcile outcome. Clean is true only when there are no
// drifts of any kind; callers apply the authoritative-vs-advisory policy
// (pinned-missing hard-fails, llm-flagged-only is advisory) on the drift list.
type Result struct {
	Clean  bool
	Drifts []Drift
}

// PinnedCheck deterministically verifies that each pinned identifier appears,
// as a WHOLE TOKEN (not a substring), in at least one file of every slice that
// defines or references it. Absence yields one pinned-missing Drift for that
// (identifier, slice). If the pin is a strict prefix of an in-file token
// (e.g. pin "rocm_smi" against "rocm_smi_sensor_temperature"), a
// pinned-prefix Drift is emitted instead, so the planner knows to extend the
// pin past the identifier boundary. Unpinned strings are ignored.
func PinnedCheck(ids []SharedIdentifier, repoDir string, sliceFiles map[string][]string) []Drift {
	var drifts []Drift
	for _, si := range ids {
		slices := append([]string{si.DefinedBy}, si.ReferencedBy...)
		for _, sl := range slices {
			files := sliceFiles[sl]
			found := false
			prefix := false
			for _, f := range files {
				text := readFile(repoDir, f)
				if present(si.ID, text) {
					found = true
					break
				}
				if !prefix && isPrefixOfToken(si.ID, text) {
					prefix = true
				}
			}
			if !found {
				kind := DriftPinnedMissing
				if prefix {
					kind = DriftPinnedPrefix
				}
				drifts = append(drifts, Drift{
					Identifier: si.ID,
					Slice:      sl,
					File:       strings.Join(files, ","),
					Kind:       kind,
				})
			}
		}
	}
	return drifts
}

var fenceRE = regexp.MustCompile("(?s)```(?:yaml)?\\s*(.*?)```")

const sweepPrompt = `You are reviewing code slices written independently and then unioned. ` +
	`Flag any SEMANTIC interface drift BETWEEN slices: a name, signature, key, ` +
	`or value one slice uses that a sibling slice expects to be different.

Return ONLY a YAML list, each item {identifier: <string>, slice: <slice name>, reason: <short>}. ` +
	`If there is no drift, return exactly: NONE

Contract:
%s

Union diff:
%s
`

// LLMSweep asks the injected model to flag semantic drift the pins did not
// cover, returning llm-flagged Drifts. It degrades open: any transport error,
// or an empty / "NONE" / unparseable / non-list completion, yields no drifts
// and never fails. A model outage therefore never blocks reconciliation.
func LLMSweep(unionDiff, contract string, model ModelCaller) []Drift {
	// Render the prompt before the transport call so a prompt-formatting bug is
	// a loud programmer error, not a silent degrade-open no-op.
	prompt := fmt.Sprintf(sweepPrompt, contract, unionDiff)
	resp, err := model.Call(prompt)
	if err != nil {
		return nil
	}
	resp = strings.TrimSpace(resp)
	if resp == "" || strings.HasPrefix(strings.ToUpper(resp), "NONE") {
		return nil
	}
	body := resp
	if m := fenceRE.FindStringSubmatch(resp); m != nil {
		body = m[1]
	}
	var items []map[string]interface{}
	if err := yaml.Unmarshal([]byte(body), &items); err != nil {
		return nil
	}
	var drifts []Drift
	for _, it := range items {
		id, ok := it["identifier"]
		if !ok {
			continue
		}
		slice := ""
		if s, ok := it["slice"]; ok {
			slice = fmt.Sprintf("%v", s)
		}
		drifts = append(drifts, Drift{
			Identifier: fmt.Sprintf("%v", id),
			Slice:      slice,
			File:       "",
			Kind:       DriftLLMFlagged,
		})
	}
	return drifts
}

// Reconcile runs PinnedCheck then LLMSweep and reports whether the union is
// interface-consistent. Clean is true only when neither produced a drift.
func Reconcile(plan SlicePlan, repoDir, unionDiff string, model ModelCaller) Result {
	sliceFiles := make(map[string][]string, len(plan.Slices))
	for _, s := range plan.Slices {
		sliceFiles[s.Name] = s.Files
	}
	drifts := PinnedCheck(plan.SharedIdentifiers, repoDir, sliceFiles)
	drifts = append(drifts, LLMSweep(unionDiff, plan.Contract, model)...)
	return Result{Clean: len(drifts) == 0, Drifts: drifts}
}

// readFile returns the contents of repoDir/path, or "" if it cannot be read
// (a missing file is not present, not an error).
func readFile(repoDir, path string) string {
	b, err := os.ReadFile(filepath.Join(repoDir, path))
	if err != nil {
		return ""
	}
	return string(b)
}

// present reports whether id occurs in text as a whole token: a match bounded
// on both sides by a non-identifier character (or string edge). This is NOT
// substring containment. A pin like "rocm_smi_gpu_temp" must not be satisfied
// by the different metric "rocm_smi_gpu_temperature" (superstring) or by a
// "rocm_smi_gpu_temp_degC" suffix; both are distinct identifiers.
func present(id, text string) bool {
	if id == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(text[i:], id)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(id)
		beforeOK := start == 0 || !isIdentByte(text[start-1])
		afterOK := end == len(text) || !isIdentByte(text[end])
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
	}
}

// isPrefixOfToken reports whether id occurs at the start of a token in text
// (bounded on the left by a non-identifier character or string edge, but
// followed by an identifier character). This catches the case where the pin
// is a strict prefix of an in-file identifier, e.g. pin "rocm_smi" against
// "rocm_smi_sensor_temperature": the pin is not a whole token, but the
// planner can fix it by extending the pin past the identifier boundary.
func isPrefixOfToken(id, text string) bool {
	if id == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(text[i:], id)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(id)
		beforeOK := start == 0 || !isIdentByte(text[start-1])
		if !beforeOK {
			i = start + 1
			continue
		}
		if end < len(text) && isIdentByte(text[end]) {
			return true
		}
		i = start + 1
	}
}

// isIdentByte reports whether b is an identifier character ([A-Za-z0-9_]).
func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z')
}
