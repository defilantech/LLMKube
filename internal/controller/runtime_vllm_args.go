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

package controller

// Argument builders for the vllm runtime. Each helper takes the current
// args slice plus the relevant CRD field and returns the appended slice (or
// the unchanged slice when the field is unset or not applicable). Kept as
// free functions so they are trivially testable in isolation and can be
// composed in any order from the deployment builder.

func hasMaxNumSeqsArgs(args []string) bool {
    for _, v := range args {
        if v == "--max-num-seqs" {
            return true
        }
    }
    return false
}
