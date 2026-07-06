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

// Package cachekey provides the single source of truth for deriving a model's
// cache key from its spec.source. Both the controller and the CLI consult this
// package so `serve`, `cache list`, and `delete --purge-cache` can never
// disagree about which directory on the cache PVC owns a given model.
//
// The scoped decision (Status.CacheKey wins; otherwise only a non-metal
// multi-file model derives a key from its source) lives in EffectiveKey here,
// so the controller's effectiveModelCacheKey() and the CLI's cache list /
// delete --purge-cache paths all resolve a model's key the same way. Compute()
// is the unconditional SHA256 fingerprint the derivation is built on.
package cachekey

import (
	"crypto/sha256"
	"encoding/hex"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Compute returns a stable, short fingerprint of source. It is the single
// function the controller and CLI agree on when they need to derive a cache
// key from a model source URL (or local path) without consulting status.
//
// The output is the first 16 hex characters of SHA-256(source), matching the
// historical convention used by both the controller and the CLI before this
// package existed. Keeping the prefix short keeps PVC directory names
// manageable while the full 64-char digest is still recoverable by callers
// that need it.
func Compute(source string) string {
	hash := sha256.Sum256([]byte(source))
	return hex.EncodeToString(hash[:])[:16]
}

// EffectiveKey is the single source of truth for the cache key a model
// resolves to, shared by the controller and the CLI so serve, cache
// list, and delete --purge-cache never disagree about whether a model
// is cached or which directory owns it. Status.CacheKey wins when the
// controller has set it. Otherwise only a non-metal, multi-file model
// derives a key from its source (hf:// repo IDs leave Status.CacheKey
// empty yet still cache; see the controller's effectiveModelCacheKey).
// Metal models and single-file models are not cached under a derived
// key and return "".
func EffectiveKey(model *inferencev1alpha1.Model) string {
	if model == nil {
		return ""
	}
	if model.Status.CacheKey != "" {
		return model.Status.CacheKey
	}
	multiFile := len(model.Spec.Files) > 0 || model.Spec.Mmproj != ""
	metal := model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator == "metal"
	if multiFile && !metal {
		return Compute(model.Spec.Source)
	}
	return ""
}
