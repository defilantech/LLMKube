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

package v1alpha1

import (
	"fmt"
	"regexp"
	"strings"
)

// HailoSourceScheme marks a Model spec.source that names a model in Hailo's
// GenAI model zoo ("hailo://qwen2.5-instruct:1.5b"). A HEF compiled for a Hailo device
// (such as the Raspberry Pi AI HAT+ 2's Hailo-10H), served by hailo-ollama
// through the ollama runtime on the host agent. hailo-ollama pulls and stores
// it, so neither the controller nor the agent downloads anything.
const HailoSourceScheme = "hailo://"

// hailoTagPattern admits "name" or "name:tag" with no "/", so a tag cannot
// name another registry or namespace for the daemon to pull from.
var hailoTagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}(:[A-Za-z0-9._-]{1,128})?$`)

// IsHailoSource reports whether source uses HailoSourceScheme (any case).
func IsHailoSource(source string) bool {
	return len(source) >= len(HailoSourceScheme) && strings.EqualFold(source[:len(HailoSourceScheme)], HailoSourceScheme)
}

// HailoSourceTag returns the model tag in a hailo:// source. ok is false for
// any other source; err is set when ok is true but the tag is not "name" or
// "name:tag".
func HailoSourceTag(source string) (tag string, ok bool, err error) {
	if !IsHailoSource(source) {
		return "", false, nil
	}
	tag = source[len(HailoSourceScheme):]
	if !hailoTagPattern.MatchString(tag) {
		return tag, true, fmt.Errorf("source %q: %q is not a Hailo model tag (name or name:tag, no registry or namespace)",
			source, tag)
	}
	return tag, true, nil
}
