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

package license

import (
	"sort"
	"strings"
)

type License struct {
	ID             string
	Name           string
	URL            string
	CommercialUse  bool
	Attribution    bool
	Redistribution bool
	Restrictions   []string
}

var db = map[string]License{
	"apache-2.0": {
		ID:             "apache-2.0",
		Name:           "Apache License 2.0",
		URL:            "https://www.apache.org/licenses/LICENSE-2.0",
		CommercialUse:  true,
		Attribution:    true,
		Redistribution: true,
	},
	"mit": {
		ID:             "mit",
		Name:           "MIT License",
		URL:            "https://opensource.org/licenses/MIT",
		CommercialUse:  true,
		Attribution:    true,
		Redistribution: true,
	},
	"llama-3.1-community": {
		ID:             "llama-3.1-community",
		Name:           "Llama 3.1 Community License Agreement",
		URL:            "https://github.com/meta-llama/llama-models/blob/main/models/llama3_1/LICENSE",
		CommercialUse:  true,
		Attribution:    true,
		Redistribution: true,
		Restrictions:   []string{"700M monthly active users limit"},
	},
	"llama-3.2-community": {
		ID:             "llama-3.2-community",
		Name:           "Llama 3.2 Community License Agreement",
		URL:            "https://github.com/meta-llama/llama-models/blob/main/models/llama3_2/LICENSE",
		CommercialUse:  true,
		Attribution:    true,
		Redistribution: true,
		Restrictions:   []string{"700M monthly active users limit"},
	},
	"llama-3.3-community": {
		ID:             "llama-3.3-community",
		Name:           "Llama 3.3 Community License Agreement",
		URL:            "https://github.com/meta-llama/llama-models/blob/main/models/llama3_3/LICENSE",
		CommercialUse:  true,
		Attribution:    true,
		Redistribution: true,
		Restrictions:   []string{"700M monthly active users limit"},
	},
	"gemma": {
		ID:             "gemma",
		Name:           "Gemma Terms of Use",
		URL:            "https://ai.google.dev/gemma/terms",
		CommercialUse:  true,
		Attribution:    true,
		Redistribution: true,
		Restrictions:   []string{"Must comply with Gemma Prohibited Use Policy"},
	},
}

func Get(id string) *License {
	l, ok := db[id]
	if !ok {
		return nil
	}
	return &l
}

func All() []License {
	licenses := make([]License, 0, len(db))
	for _, l := range db {
		licenses = append(licenses, l)
	}
	sort.Slice(licenses, func(i, j int) bool {
		return licenses[i].ID < licenses[j].ID
	})
	return licenses
}

// Normalize maps freeform license strings to known IDs using
// case-insensitive substring matching. Returns the input unchanged
// if no known license matches.
func Normalize(raw string) string {
	lower := strings.ToLower(raw)

	// Exact match first
	if _, ok := db[lower]; ok {
		return lower
	}

	// Substring matching for common variants
	patterns := []struct {
		substr string
		id     string
	}{
		{"apache", "apache-2.0"},
		{"llama 3.3", "llama-3.3-community"},
		{"llama-3.3", "llama-3.3-community"},
		{"llama 3.2", "llama-3.2-community"},
		{"llama-3.2", "llama-3.2-community"},
		{"llama 3.1", "llama-3.1-community"},
		{"llama-3.1", "llama-3.1-community"},
		{"gemma", "gemma"},
	}

	for _, p := range patterns {
		if strings.Contains(lower, p.substr) {
			return p.id
		}
	}

	return raw
}
