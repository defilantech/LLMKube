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

package controller

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/defilantech/llmkube/pkg/fileset"
)

// stagingCommands renders every init command variant that writes into a
// cache entry: single-file and multi-file (plain, S3 and mixed-origin), with
// and without the cache, auth and SHA-256 gates, serial and concurrent, under
// both refresh policies, plus the prefetch verifier.
func stagingCommands() map[string]string {
	cmds := map[string]string{}
	bools := []bool{false, true}
	for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
		for _, useCache := range bools {
			for _, isS3 := range bools {
				for _, auth := range bools {
					for _, sha := range bools {
						name := strings.Join([]string{policy, b2s("cache", useCache), b2s("s3", isS3), b2s("auth", auth), b2s("sha", sha)}, "/")
						for _, concurrent := range bools {
							multi := name + "/" + b2s("concurrent", concurrent)
							cmds["multi/"+multi] = buildMultiFileInitCommand(useCache, isS3, auth, sha, concurrent, policy)
							cmds["mixed/"+multi] = multiFileInitCommand(useCache, false, true, sha, concurrent, policy, true)
						}
						for _, local := range bools {
							cmds["single/"+name+"/"+b2s("local", local)] = buildModelInitCommand(local, isS3, useCache, auth, sha, policy)
						}
					}
				}
			}
		}
	}
	cmds["prefetch-verify"] = buildPrefetchVerifyCommand(false)
	cmds["prefetch-verify/sha"] = buildPrefetchVerifyCommand(true)
	return cmds
}

func b2s(name string, v bool) string {
	if v {
		return name
	}
	return "no-" + name
}

var (
	// A path variable followed by literal or variable dot segments:
	// "$dest.llmkube-size", "$1.$MODEL_SHA256.sha256-rejected",
	// "$MODEL_PATH.$key.tmp", "$1.llmkube-tmp.XXXXXX".
	stagedSuffixRe = regexp.MustCompile(`\$\{?(dest|1|2|MODEL_PATH|MODEL_PARTIAL|marker|stamp|partial)\}?((?:\.(?:\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|[A-Za-z0-9_?-]+))+)`)
	// A literal name under the entry: "$CACHE_DIR/.llmkube-download.lock".
	cacheDirNameRe = regexp.MustCompile(`\$\{?CACHE_DIR\}?/([^"'$\s;)]+)`)
	// A find pattern the sweeps delete by: -name '*.llmkube-tmp.??????'.
	findNameRe = regexp.MustCompile(`-name '([^']+)'`)
	shellVarRe = regexp.MustCompile(`\$\{?[A-Za-z_][A-Za-z0-9_]*\}?`)
)

// stagedNameSample turns a name the scripts write into a concrete file name
// as it lands in a cache entry next to artifact.
func stagedNameSample(artifact, suffix string) string {
	name := strings.ReplaceAll(suffix, "$MODEL_SHA256", strings.Repeat("ab", 32))
	name = strings.ReplaceAll(name, "XXXXXX", "Ab3dE9")
	name = shellVarRe.ReplaceAllString(name, "0123456789ab")
	name = strings.ReplaceAll(name, "?", "Z")
	return artifact + name
}

// TestStagingBookkeepingCoversEveryStagedName pins fileset.StagingBookkeeping,
// the list `llmkube cache prune` never removes, to the names the staging
// scripts actually write. A new bookkeeping file in model_storage.go that the
// list does not match fails here instead of being pruned (#2170).
func TestStagingBookkeepingCoversEveryStagedName(t *testing.T) {
	const artifact = "transformer/diffusion_pytorch_model-00001-of-00002.safetensors"
	found := map[string]string{}
	for variant, cmd := range stagingCommands() {
		for _, m := range stagedSuffixRe.FindAllStringSubmatch(cmd, -1) {
			found[stagedNameSample(artifact, m[2])] = variant + ": " + m[0]
		}
		for _, m := range cacheDirNameRe.FindAllStringSubmatch(cmd, -1) {
			found[stagedNameSample("", m[1])] = variant + ": " + m[0]
		}
		for _, m := range findNameRe.FindAllStringSubmatch(cmd, -1) {
			found[stagedNameSample("", strings.ReplaceAll(m[1], "*", artifact))] = variant + ": " + m[0]
		}
	}

	// The scan has to see the families staging is known to write, or a
	// regexp that matched nothing would pass vacuously.
	for _, want := range []string{
		".llmkube-download.lock",
		artifact + ".sha256",
		artifact + "." + strings.Repeat("ab", 32) + ".sha256-rejected",
		artifact + ".llmkube-size",
		artifact + ".0123456789ab.tmp",
		artifact + ".tmp",
		artifact + ".llmkube-tmp.Ab3dE9",
		artifact + ".llmkube-size.ZZZZZZ",
	} {
		if _, ok := found[want]; !ok {
			t.Errorf("scan did not find %q among the staged names", want)
		}
	}

	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !fileset.IsStagingBookkeeping(name) {
			t.Errorf("staging writes %q (%s), which fileset.StagingBookkeeping does not protect", name, found[name])
		}
	}

	// The artifacts themselves, which staging writes as "$CACHE_DIR/$rel",
	// must stay prunable, or a list that protects everything would pass.
	for _, artifact := range []string{artifact, "model_index.json", "tokenizer/tokenizer.model", "README.md", "model-Q4_K_M.gguf"} {
		if fileset.IsStagingBookkeeping(artifact) {
			t.Errorf("fileset.IsStagingBookkeeping(%q) = true for an artifact", artifact)
		}
	}
}

// fileset reads status.stagedFiles only for the generation the Available
// condition was written for, so the two condition names must agree.
func TestFilesetConditionAvailableMatchesController(t *testing.T) {
	if fileset.ConditionAvailable != ConditionAvailable {
		t.Fatalf("fileset.ConditionAvailable = %q, controller ConditionAvailable = %q",
			fileset.ConditionAvailable, ConditionAvailable)
	}
}
