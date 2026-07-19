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

package worktracker

import (
	"context"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
)

// fakeFetcher implements githubissue.Fetcher for testing.
type fakeFetcher struct {
	Issue *githubissue.Issue
	Err   error
}

func (f *fakeFetcher) Fetch(_ context.Context, _, _ string, _ int, _ string) (*githubissue.Issue, error) {
	return f.Issue, f.Err
}

func TestGitHubWorkItems_Get(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		repoSlug   string
		id         string
		fetcher    *fakeFetcher
		wantItem   *WorkItem
		wantErr    bool
		errContain string
	}{
		{
			name:     "happy path",
			repoSlug: "defilantech/llmkube",
			id:       "42",
			fetcher: &fakeFetcher{
				Issue: &githubissue.Issue{
					Number: 42,
					Title:  "Fix memory leak in agent",
					Body:   "The agent leaks goroutines on shutdown.",
					State:  "open",
					Labels: []string{"bug", "agent"},
				},
			},
			wantItem: &WorkItem{
				ID:     "42",
				Title:  "Fix memory leak in agent",
				Body:   "The agent leaks goroutines on shutdown.",
				State:  "open",
				URL:    "",
				Labels: []string{"bug", "agent"},
			},
		},
		{
			name:     "closed issue",
			repoSlug: "defilantech/llmkube",
			id:       "7",
			fetcher: &fakeFetcher{
				Issue: &githubissue.Issue{
					Number: 7,
					Title:  "Add CI badge",
					Body:   "Add a CI badge to the README.",
					State:  "closed",
					Labels: []string{"docs"},
				},
			},
			wantItem: &WorkItem{
				ID:     "7",
				Title:  "Add CI badge",
				Body:   "Add a CI badge to the README.",
				State:  "closed",
				URL:    "",
				Labels: []string{"docs"},
			},
		},
		{
			name:     "issue with no labels",
			repoSlug: "defilantech/llmkube",
			id:       "1",
			fetcher: &fakeFetcher{
				Issue: &githubissue.Issue{
					Number: 1,
					Title:  "Initial issue",
					Body:   "First issue ever.",
					State:  "open",
					Labels: nil,
				},
			},
			wantItem: &WorkItem{
				ID:     "1",
				Title:  "Initial issue",
				Body:   "First issue ever.",
				State:  "open",
				URL:    "",
				Labels: nil,
			},
		},
		{
			name:       "invalid repo slug",
			repoSlug:   "bad-slug",
			id:         "1",
			fetcher:    &fakeFetcher{},
			wantErr:    true,
			errContain: "parse repo slug",
		},
		{
			name:       "invalid id",
			repoSlug:   "defilantech/llmkube",
			id:         "not-a-number",
			fetcher:    &fakeFetcher{},
			wantErr:    true,
			errContain: "parse id",
		},
		{
			name:     "fetcher returns error",
			repoSlug: "defilantech/llmkube",
			id:       "99",
			fetcher: &fakeFetcher{
				Err: &githubissue.HTTPError{StatusCode: 404},
			},
			wantErr:    true,
			errContain: "fetch issue",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gw := &GitHubWorkItems{Fetcher: tt.fetcher}
			item, err := gw.Get(context.Background(), tt.repoSlug, tt.id)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Get() expected error containing %q, got nil", tt.errContain)
				}
				if tt.errContain != "" && !contains(err.Error(), tt.errContain) {
					t.Fatalf("Get() error = %q, want to contain %q", err, tt.errContain)
				}
				return
			}

			if err != nil {
				t.Fatalf("Get() unexpected error: %v", err)
			}

			if item == nil {
				t.Fatal("Get() returned nil WorkItem")
			}

			if item.ID != tt.wantItem.ID {
				t.Errorf("ID = %q, want %q", item.ID, tt.wantItem.ID)
			}
			if item.Title != tt.wantItem.Title {
				t.Errorf("Title = %q, want %q", item.Title, tt.wantItem.Title)
			}
			if item.Body != tt.wantItem.Body {
				t.Errorf("Body = %q, want %q", item.Body, tt.wantItem.Body)
			}
			if item.State != tt.wantItem.State {
				t.Errorf("State = %q, want %q", item.State, tt.wantItem.State)
			}
			if item.URL != tt.wantItem.URL {
				t.Errorf("URL = %q, want %q", item.URL, tt.wantItem.URL)
			}
			if len(item.Labels) != len(tt.wantItem.Labels) {
				t.Errorf("Labels length = %d, want %d", len(item.Labels), len(tt.wantItem.Labels))
			}
			for i := range item.Labels {
				if item.Labels[i] != tt.wantItem.Labels[i] {
					t.Errorf("Labels[%d] = %q, want %q", i, item.Labels[i], tt.wantItem.Labels[i])
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	if len(s) < len(substr) {
		return false
	}
	if s == substr {
		return true
	}
	return findSubstring(s, substr)
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
