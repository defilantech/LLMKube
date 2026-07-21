//go:build e2e
// +build e2e

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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/defilantech/llmkube/test/utils"
)

// GPUQuota Multi-tenancy exercises the InferenceService GPUQuota validating
// webhook against a real API server: an over-quota InferenceService is rejected
// at admission with the quota's denial message, an in-quota InferenceService is
// admitted, and the GPUQuota status counter aggregates the admitted usage.
//
// envtest (internal/controller/inferenceservice_quota_webhook_test.go) already
// proves the decision logic (gpuCount/vramBytes/priority branches) against a
// fake client, but it cannot prove the webhook is actually wired: that the
// ValidatingWebhookConfiguration ships, that cert-manager injects the CA, and
// that a real `kubectl apply` is blocked at admission. It also cannot observe
// the controller's status aggregation across a real reconcile. Hence this
// Context.
//
// Unlike the make-deploy Contexts (Manager, ModelRouter Cluster), this one
// installs via Helm: the GPUQuota validating webhook ships ONLY in the chart,
// gated on multitenancy.enabled. config/default (what `make deploy` applies)
// keeps all webhook + cert-manager sections commented out, so a kustomize
// deploy would leave the webhook unregistered and every admission would pass.
// The chart self-signs its serving cert (genSignedCert) and bakes the caBundle
// straight into the ValidatingWebhookConfiguration, so no cert-manager is
// needed and the webhook is live the moment the controller pod is Ready.
//
// Self-contained like the "Pyrra SLO Integration" and "Catalog E2E Tests"
// Describes (its own install/uninstall): Ginkgo tears down each top-level
// container before the next runs, so this Context cannot assume another
// container's operator is still up. Because it uses Helm rather than the
// shared kustomize deploy, it runs in its own dedicated CI job (test-e2e.yml,
// test-e2e-quota) via `-ginkgo.focus='GPUQuota Multi-tenancy'`, not the main
// make-deploy merge-gate job.
//
// Gated on LLMKUBE_E2E_QUOTA=true (see the "Optional Environment Variables"
// note in e2e_suite_test.go).

var _ = Describe("GPUQuota Multi-tenancy", Ordered, func() {
	const (
		quotaTestNs = "e2e-quota-test"
		quotaName   = "e2e-quota"

		// modelRef value for the test InferenceServices. It need not resolve to
		// a real Model: the only webhook on InferenceService is the quota
		// validator, which decides off spec.resources.gpu and never reads the
		// Model, so admission (the thing under test) does not depend on it.
		modelName = "quota-test-model"
		// The over-quota InferenceService requests more GPUs than the quota's
		// cap, so its admission is always rejected and it is never created.
		isvcOverName = "quota-over-isvc"
		// The in-quota InferenceService fits within the cap and is admitted.
		isvcInName = "quota-in-isvc"

		// The quota caps the namespace at a single GPU. The over-quota ISVC
		// requests four; the in-quota ISVC requests one.
		quotaGPUCount = 1
		overGPUCount  = 4
		inGPUCount    = 1

		// Substring of the webhook denial the API server surfaces to the
		// caller. Matches inferenceservice_quota_webhook.go's
		//   fmt.Errorf("GPUQuota %q denied: %s", ...)
		// wrapping quota.Decide's "would exceed gpuCount ..." reason.
		gpuCountDenialSubstr = "denied: would exceed gpuCount"
	)

	BeforeAll(func() {
		if os.Getenv("LLMKUBE_E2E_QUOTA") != "true" {
			Skip("LLMKUBE_E2E_QUOTA not set; GPUQuota multi-tenancy e2e requires " +
				"the chart installed with multitenancy.enabled=true (Helm)")
		}

		By("installing LLMKube via Helm with multitenancy (GPUQuota webhook) enabled")
		// projectImage was built and side-loaded into kind by BeforeSuite;
		// pullPolicy=Never forces the pod to use that image rather than
		// pulling. multitenancy.enabled=true adds the GPUQuota validating
		// webhook (webhook.enabled defaults true). modelCache.enabled=false so
		// --wait does not block on a PVC bind unrelated to the webhook. Split
		// projectImage into repository/tag so it stays in sync with
		// BeforeSuite rather than duplicating the literal.
		imgParts := strings.SplitN(projectImage, ":", 2)
		imgRepo, imgTag := imgParts[0], imgParts[1]
		cmd := exec.Command("helm", "install", "llmkube", "./charts/llmkube",
			"-n", namespace, "--create-namespace",
			"--set", "multitenancy.enabled=true",
			"--set", "controllerManager.image.repository="+imgRepo,
			"--set", "controllerManager.image.tag="+imgTag,
			"--set", "controllerManager.image.pullPolicy=Never",
			"--set", "modelCache.enabled=false",
			"--wait", "--timeout", "5m")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install the LLMKube Helm release")

		By("creating the quota test namespace")
		cmd = exec.Command("kubectl", "create", "ns", quotaTestNs)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create the quota test namespace")

		By("creating a namespace-scoped GPUQuota capping the namespace at one GPU")
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: GPUQuota
metadata:
  name: %s
  namespace: %s
spec:
  namespaceRef: %s
  gpuCount: %d
`, quotaName, quotaTestNs, quotaTestNs, quotaGPUCount))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create GPUQuota")
	})

	AfterAll(func() {
		if os.Getenv("LLMKUBE_E2E_QUOTA") != "true" {
			return
		}

		By("cleaning up the quota test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", quotaTestNs, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("uninstalling the LLMKube Helm release")
		cmd = exec.Command("helm", "uninstall", "llmkube", "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("removing the manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	It("rejects an InferenceService that would exceed the quota's gpuCount", func() {
		By("applying an InferenceService requesting more GPUs than the quota allows")
		// Eventually, not a one-shot: helm --wait returns once the pod is Ready,
		// but with failurePolicy=Fail the API server rejects every admission
		// during the brief window before the webhook Service's endpoints are
		// populated, with a generic webhook-call error rather than the quota
		// denial. Retrying until the specific quota-denial substring appears
		// both waits out that warmup and proves the denial is the quota's, not a
		// transient wiring failure. The request is always rejected (4 > 1), so
		// no InferenceService is ever created.
		applyOverQuota := func(g Gomega) {
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: %s
  namespace: %s
spec:
  modelRef: %s
  resources:
    gpu: %d
`, isvcOverName, quotaTestNs, modelName, overGPUCount))
			output, err := utils.Run(cmd)
			g.Expect(err).To(HaveOccurred(), "expected admission to reject the over-quota InferenceService")
			g.Expect(output).To(ContainSubstring(gpuCountDenialSubstr),
				"expected the GPUQuota gpuCount denial message")
		}
		Eventually(applyOverQuota, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("confirming the rejected InferenceService was not created")
		cmd := exec.Command("kubectl", "get", "inferenceservice", isvcOverName,
			"-n", quotaTestNs, "--ignore-not-found", "-o", "jsonpath={.metadata.name}")
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(output).To(BeEmpty(), "over-quota InferenceService should not exist")
	})

	It("admits an InferenceService within the quota and aggregates it into status.usedGPUCount", func() {
		By("applying an InferenceService that fits within the quota")
		// The webhook is already warm (the previous spec's Eventually observed
		// a real quota denial), so this should be admitted promptly; Eventually
		// only guards against the final moments of rollout/injection lag.
		applyInQuota := func(g Gomega) {
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: %s
  namespace: %s
spec:
  modelRef: %s
  resources:
    gpu: %d
`, isvcInName, quotaTestNs, modelName, inGPUCount))
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "in-quota InferenceService should be admitted")
		}
		Eventually(applyInQuota, 1*time.Minute, 3*time.Second).Should(Succeed())

		By("waiting for the GPUQuota reconciler to aggregate the admitted GPU into status.usedGPUCount")
		verifyUsedGPUCount := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "gpuquota", quotaName,
				"-n", quotaTestNs, "-o", "jsonpath={.status.usedGPUCount}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(Equal(fmt.Sprintf("%d", inGPUCount)),
				"status.usedGPUCount should reflect the admitted InferenceService")
		}
		Eventually(verifyUsedGPUCount, 1*time.Minute, 3*time.Second).Should(Succeed())
	})
})
