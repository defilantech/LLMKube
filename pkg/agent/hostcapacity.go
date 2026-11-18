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

package agent

import (
	"strconv"
	"strings"

	"github.com/defilantech/llmkube/pkg/metalnode"
)

// maxChipBytes matches the bound the MetalNode controller enforces on
// free-text report fields.
const maxChipBytes = 128

// HostCapacity is the hardware the agent reports to its MetalNode.
type HostCapacity struct {
	MemoryBytes  int64
	Chip         string
	GPUCores     int32
	MetalVersion string
}

// printablePrefix drops every character the controller would reject
// (metalnode.Printable: controls, bidi and other format characters) and
// truncates to limit bytes on a rune boundary.
func printablePrefix(s string, limit int) string {
	var b strings.Builder
	for _, r := range s {
		if !metalnode.Printable(r) {
			continue
		}
		if b.Len()+len(string(r)) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// MetalVersionString renders platform.Capabilities.MetalVersion ("Metal 4");
// zero (unknown) is empty.
func MetalVersionString(v int) string {
	if v <= 0 {
		return ""
	}
	return "Metal " + strconv.Itoa(v)
}
