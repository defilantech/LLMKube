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
	"os"
	"strings"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// gateTier classifies a gate check by how its failure is handled.
type gateTier int

const (
	// tierBlock: a failure fails the gate and is fed back to the coder loop.
	tierBlock gateTier = iota
	// tierAdvisory: a failure does not fail the gate; it is surfaced to the
	// reviewer and audit record via Result.Extra["gateAdvisories"].
	tierAdvisory
)

// advisory is a non-blocking gate finding handed to the reviewer.
type advisory struct {
	Check  string `json:"check"`
	Detail string `json:"detail"`
}

// gateCheck is one registered verification check. lang scopes a check to a
// language preset; the empty value means language-agnostic. The registry is
// invoked only from the Go verifier path today, so lang is metadata for a
// future generic-gate wiring, not yet used for filtering.
type gateCheck struct {
	name string
	tier gateTier
	lang foremanv1alpha1.GateLanguage
	fn   func(ctx context.Context, workspace string, run commandRunner) (failed bool, output string)
}

// gateCheckEnabled reports whether a check is on. Default is on; set
// FOREMAN_<UPPERSNAKE(name)>_GATE=0 to disable a single check in the field.
func gateCheckEnabled(name string) bool {
	key := "FOREMAN_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_GATE"
	return os.Getenv(key) != "0"
}

// runGateChecks runs each enabled check and splits results by tier.
func runGateChecks(
	ctx context.Context, workspace string, run commandRunner, checks []gateCheck,
) (blocking []checkFailure, advisories []advisory) {
	for _, c := range checks {
		if !gateCheckEnabled(c.name) {
			continue
		}
		failed, out := c.fn(ctx, workspace, run)
		if !failed {
			continue
		}
		switch c.tier {
		case tierAdvisory:
			advisories = append(advisories, advisory{Check: c.name, Detail: out})
		default:
			blocking = append(blocking, checkFailure{name: c.name, output: out})
		}
	}
	return blocking, advisories
}
