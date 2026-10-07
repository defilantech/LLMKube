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

package repomap_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/repomap"
)

// The durable localization eval: measure the coder repo-map ranker against
// ground truth mined from git history (#1906). It is opt-in because it indexes
// the llmkube checkout, so it is skipped in the normal unit run and enabled
// where the ranking matters:
//
//	LOCALIZATION_EVAL=1 go test ./pkg/foreman/agent/repomap/ -run LocalizationEval -v
//
// Corpus provenance: testdata/localization_corpus.json was mined once from
// merged commits that reference (#N), with gold = that commit's changed
// non-test .go files and query = the real issue/PR body. See
// scripts/spike/ripwire_eval.sh for the live-mining harness and
// docs/proposals/ripwire-vs-repomap-spike.md for the results it produced.

type corpusCase struct {
	ID    string   `json:"id"`
	Query string   `json:"query"`
	Gold  []string `json:"gold"`
}

type corpus struct {
	Cases []corpusCase `json:"cases"`
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("no checkout at %s: %v", root, err)
	}
	return root
}

func TestLocalizationEval(t *testing.T) {
	if os.Getenv("LOCALIZATION_EVAL") != "1" {
		t.Skip("set LOCALIZATION_EVAL=1 to measure the ranker against the mined corpus")
	}
	root := repoRoot(t)

	raw, err := os.ReadFile(filepath.Join("testdata", "localization_corpus.json"))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if len(c.Cases) == 0 {
		t.Fatal("corpus is empty")
	}

	files, err := repomap.Walk(root, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	strict := 0
	for _, tc := range c.Cases {
		scored := repomap.ScoreFiles(files, tc.Query)
		top := make(map[string]bool)
		for i := 0; i < len(scored) && i < 10; i++ {
			top[scored[i].Path] = true
		}
		hit := 0
		for _, g := range tc.Gold {
			if top[g] {
				hit++
			}
		}
		if hit == len(tc.Gold) {
			strict++
		}
	}
	t.Logf("repomap strict file@10: %d/%d cases", strict, len(c.Cases))

	// Harness self-check (bites): an unrelated query must not solve the task.
	// Strict file@10 counts a case only when ALL its gold files are in the
	// top 10; a structurally central file like cmd/main.go can drift into any
	// top 10, so the check is that no case is fully solved by a query about
	// nothing, not that no gold file ever appears.
	unrelated := repomap.ScoreFiles(files, "the quick brown fox jumped over the lazy dog while eating breakfast")
	topUnrelated := make(map[string]bool)
	for i := 0; i < len(unrelated) && i < 10; i++ {
		topUnrelated[unrelated[i].Path] = true
	}
	for _, tc := range c.Cases {
		solved := len(tc.Gold) > 0
		for _, g := range tc.Gold {
			if !topUnrelated[g] {
				solved = false
				break
			}
		}
		if solved {
			t.Errorf("unrelated query fully solved case %s; the eval is too permissive", tc.ID)
		}
	}

	// Property (bites, tree-stable): a query that literally names a gold file's
	// basename must put that file in the top 10. This survives tree drift, so
	// it is safe to assert where the absolute number is not.
	for _, tc := range c.Cases {
		scored := repomap.ScoreFiles(files, tc.Query)
		top := make(map[string]bool)
		for i := 0; i < len(scored) && i < 10; i++ {
			top[scored[i].Path] = true
		}
		for _, g := range tc.Gold {
			base := filepath.Base(g)
			if !strings.Contains(tc.Query, base) {
				continue
			}
			if !top[g] {
				t.Errorf("case %s names %q but the ranker put it outside the top 10", tc.ID, base)
			}
		}
	}
}
