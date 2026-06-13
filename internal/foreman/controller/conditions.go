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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// setCondition upserts a condition by type into conds. If a condition with
// the same Type already exists it is replaced in-place; otherwise the new
// condition is appended. Callers are responsible for setting
// c.LastTransitionTime before calling.
func setCondition(conds *[]metav1.Condition, c metav1.Condition) {
	for i, existing := range *conds {
		if existing.Type == c.Type {
			(*conds)[i] = c
			return
		}
	}
	*conds = append(*conds, c)
}

// hasCondition reports whether conds already holds a condition that matches c
// on the fields the reconcilers manage: Type, Status, and Reason. This lets
// callers skip a no-op status patch when nothing meaningful has changed,
// avoiding spurious LastTransitionTime-only updates.
func hasCondition(conds []metav1.Condition, c metav1.Condition) bool {
	for i := range conds {
		if conds[i].Type == c.Type {
			return conds[i].Status == c.Status && conds[i].Reason == c.Reason
		}
	}
	return false
}
