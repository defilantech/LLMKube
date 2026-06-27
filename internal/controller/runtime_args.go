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

import (
	"fmt"
	"strings"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Serving modes accepted in spec.mode and reported in status.mode.
const (
	servingModeChat      = inferencev1alpha1.ServingModeChat
	servingModeEmbedding = inferencev1alpha1.ServingModeEmbedding
	servingModeRerank    = inferencev1alpha1.ServingModeRerank
)

func hasMatchingExtraArg(extraArgs []string, argName string) bool {
	arg := fmt.Sprintf("--%s", argName)
	inlineArg := fmt.Sprintf("--%s=", argName)
	for _, v := range extraArgs {
		if v == arg || strings.HasPrefix(v, inlineArg) {
			return true
		}
	}
	return false
}

// resolveServingMode returns spec.mode when set, otherwise infers it from the
// runtime flags or endpoint path. A reranker passes both --reranking and
// --embedding, so rerank is checked first. Defaults to chat.
func resolveServingMode(isvc *inferencev1alpha1.InferenceService) string {
	if isvc.Spec.Mode != "" {
		return isvc.Spec.Mode
	}
	args := append(append([]string{}, isvc.Spec.ExtraArgs...), isvc.Spec.Args...)
	switch {
	case hasMatchingExtraArg(args, "reranking"):
		return servingModeRerank
	case hasMatchingExtraArg(args, "embedding"), hasMatchingExtraArg(args, "embeddings"):
		return servingModeEmbedding
	}
	if isvc.Spec.Endpoint != nil {
		switch {
		case strings.Contains(isvc.Spec.Endpoint.Path, "/rerank"):
			return servingModeRerank
		case strings.Contains(isvc.Spec.Endpoint.Path, "/embeddings"):
			return servingModeEmbedding
		}
	}
	return servingModeChat
}
