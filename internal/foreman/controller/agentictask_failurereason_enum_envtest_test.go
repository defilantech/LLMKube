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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// Every AgenticTaskFailureReason the executor can emit has to be a member of
// the CRD failureReason enum. If it is not, the apiserver rejects the terminal
// status patch with IsInvalid and the reason never reaches status -- watcher.go
// catches the Invalid error and takes the defensive fallback, so the intended
// verdict/reason is lost. RebaseConflictUnresolved regressed exactly this way
// (#1840): the constant existed but the kubebuilder enum marker omitted it, so
// the generated CRD did not carry it. This pins the whole set against the live
// CRD so a new reason cannot ship without its enum entry.
var _ = Describe("AgenticTask failureReason enum", func() {
	allReasons := []foremanv1alpha1.AgenticTaskFailureReason{
		foremanv1alpha1.FailureAgentNotFound,
		foremanv1alpha1.FailureInferenceServiceUnavailable,
		foremanv1alpha1.FailureAuthUnavailable,
		foremanv1alpha1.FailureGitRemoteNotConfigured,
		foremanv1alpha1.FailureCloneFailed,
		foremanv1alpha1.FailureRebaseConflictUnresolved,
		foremanv1alpha1.FailureModelMisunderstood,
		foremanv1alpha1.FailureToolFailed,
		foremanv1alpha1.FailureMaxTurnsExhausted,
		foremanv1alpha1.FailureLoopSpinning,
		foremanv1alpha1.FailureConstraintViolated,
		foremanv1alpha1.FailureTimeout,
		foremanv1alpha1.FailureInfrastructureError,
		foremanv1alpha1.FailureGateFailed,
		foremanv1alpha1.FailureGateError,
		foremanv1alpha1.FailureModelReportedError,
	}

	It("accepts every executor-emitted failure reason on a status patch", func() {
		for _, reason := range allReasons {
			task := newTask("enum-" + strings.ToLower(string(reason)))
			Expect(k8sClient.Create(ctx, task)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })

			patch := client.MergeFrom(task.DeepCopy())
			task.Status.Phase = foremanv1alpha1.AgenticTaskPhaseFailed
			task.Status.FailureReason = reason
			Expect(k8sClient.Status().Patch(ctx, task, patch)).
				To(Succeed(), "reason %q is missing from the CRD failureReason enum", reason)
		}
	})
})
