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

// Pyrra SLO Integration exercises the --enable-pyrra-slo operator flag against
// a real Pyrra kubernetes operator: rendering a ServiceLevelObjective, Pyrra
// accepting it and generating a matching PrometheusRule, and owner-reference
// garbage collection when the InferenceService goes away. envtest
// (internal/controller/inferenceservice_slo_test.go) already proves the
// rendering logic against the vendored CRD, but it can't run the real Pyrra
// binary or observe real garbage collection, hence this Context.
//
// This lives in its own top-level Describe rather than nested in the
// "Manager" Describe (test/e2e/e2e_test.go) because Ginkgo, by default, only
// randomizes the order of top-level containers; a container's own
// BeforeAll/Its/AfterAll always run as one contiguous block before the next
// top-level container starts (see onsi/ginkgo/v2 internal/ordering.go: "by
// default only top-level containers and specs are shuffled"). That means
// "Manager"'s controller-manager Deployment is always undeployed and its
// namespace deleted (its AfterAll) before any other top-level Describe's
// specs run, regardless of shuffle order — so this Context cannot assume
// "Manager"'s operator instance is still around to patch. Instead it deploys
// and tears down its own controller-manager instance, the same way the
// "Catalog E2E Tests" and "License E2E Tests" Describes are self-contained
// rather than reusing Manager's setup.
//
// Gated on LLMKUBE_E2E_PYRRA=true (see the "Optional Environment Variables"
// note in e2e_suite_test.go) so unrelated CI jobs stay fast: it side-loads the
// Pyrra CRD/operator and a prometheus-operator CRD, and pays for a second
// full controller-manager deploy/undeploy cycle.

// pyrraCreatedMonitoringNS tracks whether this suite's BeforeAll created the
// "monitoring" namespace, so AfterAll only deletes it when it did. This suite
// assumes a throwaway Kind cluster; a pre-existing monitoring namespace
// almost certainly belongs to something else (a real kube-prometheus-stack
// install, a prior manual test run), and deleting it out from under that
// owner on teardown would be a nasty surprise. BeforeAll fails fast instead
// of silently adopting it.
var pyrraCreatedMonitoringNS bool

var _ = Describe("Pyrra SLO Integration", Ordered, func() {
	const (
		sloTestNs    = "e2e-slo-test"
		monitoringNs = "monitoring"

		// Pinned so the suite is reproducible across CI runs. Same Pyrra tag
		// as the vendored envtest CRD fixture (test/crd/pyrra).
		pyrraSLOCRDURL = "https://raw.githubusercontent.com/pyrra-dev/pyrra/v0.10.1/" +
			"examples/kubernetes/manifests/setup/pyrra-slo-CustomResourceDefinition.yaml"
		// Pyrra's kubernetes operator writes PrometheusRules, so the CRD must
		// exist even though this suite runs no Prometheus. Large CRD;
		// prometheus-operator's own docs call for --server-side apply here to
		// avoid the client-side "last-applied-configuration" annotation
		// blowing past the 256KB limit.
		prometheusRuleCRDURL = "https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.76.0/" +
			"example/prometheus-operator-crd/monitoring.coreos.com_prometheusrules.yaml"
		pyrraManifestBaseURL = "https://raw.githubusercontent.com/pyrra-dev/pyrra/v0.10.1/examples/kubernetes/manifests/"

		modelName = "slo-test-model"
		isvcName  = "slo-test-isvc"
		// Documented default for spec.slo.name: "<inferenceservice-name>-<indicator>".
		sloName = isvcName + "-availability"
	)

	// Deployment + RBAC for Pyrra's "kubernetes" operator mode, which watches
	// ServiceLevelObjective and reconciles a PrometheusRule per SLO. Its
	// webhooks default to disabled (DisableWebhooks defaults true upstream),
	// so no cert-manager dependency here. Excludes the pyrra-api* manifests
	// (UI/API server) and ServiceMonitor, which this suite doesn't need.
	pyrraKubernetesManifests := []string{
		"pyrra-kubernetesServiceAccount.yaml",
		"pyrra-kubernetesClusterRole.yaml",
		"pyrra-kubernetesClusterRoleBinding.yaml",
		"pyrra-kubernetesDeployment.yaml",
	}

	BeforeAll(func() {
		if os.Getenv("LLMKUBE_E2E_PYRRA") != "true" {
			Skip("LLMKUBE_E2E_PYRRA not set; Pyrra SLO e2e requires the Pyrra " +
				"CRD and kubernetes operator side-loaded into kind")
		}

		By("applying the Pyrra ServiceLevelObjective CRD (v0.10.1)")
		cmd := exec.Command("kubectl", "apply", "-f", pyrraSLOCRDURL)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply the Pyrra SLO CRD")

		By("applying the prometheus-operator PrometheusRule CRD (v0.76.0)")
		cmd = exec.Command("kubectl", "apply", "--server-side", "-f", prometheusRuleCRDURL)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply the PrometheusRule CRD")

		By("checking the monitoring namespace does not already exist")
		// This suite expects a throwaway Kind cluster. A pre-existing
		// "monitoring" namespace means either a real monitoring stack is
		// installed here or a previous run of this suite did not tear down
		// cleanly; either way, this suite must not adopt (and later delete)
		// a namespace it did not create.
		cmd = exec.Command("kubectl", "get", "ns", monitoringNs)
		if _, err = utils.Run(cmd); err == nil {
			Fail(fmt.Sprintf("namespace %q already exists; this suite requires a throwaway "+
				"Kind cluster and refuses to adopt a pre-existing monitoring namespace", monitoringNs))
		}

		By("creating the monitoring namespace for the Pyrra kubernetes operator")
		cmd = exec.Command("kubectl", "create", "ns", monitoringNs)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create the monitoring namespace")
		pyrraCreatedMonitoringNS = true

		By("deploying the Pyrra kubernetes operator (v0.10.1 example manifests)")
		for _, m := range pyrraKubernetesManifests {
			cmd = exec.Command("kubectl", "apply", "-f", pyrraManifestBaseURL+m)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Pyrra manifest %s", m)
		}

		By("waiting for the Pyrra kubernetes operator to become Available")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/pyrra-kubernetes",
			"-n", monitoringNs, "--timeout=120s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Pyrra kubernetes operator never became Available")

		By("creating the LLMKube manager namespace")
		// Retried: if the "Manager" Describe (e2e_test.go) ran earlier in
		// this shuffle, its AfterAll fired an async `kubectl delete ns` right
		// before this Context started; namespace termination can lag a
		// couple of seconds behind that call returning.
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "create", "ns", namespace)
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
		}, 90*time.Second, 2*time.Second).Should(Succeed(), "Failed to create the manager namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("patching the manager Deployment to add --enable-pyrra-slo")
		// The Helm/kustomize toggle (charts/llmkube/templates/deployment.yaml,
		// values.pyrra.enabled) is off by default and config/default is shared
		// with every other e2e Context, so rather than forking the kustomize
		// manifests this appends the flag directly to the already-deployed
		// Deployment's args and lets the rollout pick it up — equivalent to
		// `helm upgrade --set pyrra.enabled=true` for this suite's purposes.
		cmd = exec.Command("kubectl", "patch", "deployment", "llmkube-controller-manager",
			"-n", namespace, "--type=json",
			"-p", `[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--enable-pyrra-slo"}]`)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to patch the manager Deployment args")

		By("waiting for the patched controller-manager rollout to complete")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/llmkube-controller-manager",
			"-n", namespace, "--timeout=120s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "controller-manager rollout with --enable-pyrra-slo never completed")

		By("creating the SLO test namespace")
		cmd = exec.Command("kubectl", "create", "ns", sloTestNs)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create the SLO test namespace")

		By("seeding a stub Model for the InferenceService's modelRef")
		// Mirrors the ModelRouter Reconciliation Context's stub-model
		// (e2e_test.go): the SLO reconciler renders off spec.slo alone and
		// never gates on the Model reaching Ready, so a Model that never
		// downloads is sufficient here.
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: %s
  namespace: %s
spec:
  source: file:///tmp/stub.gguf
`, modelName, sloTestNs))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to seed stub Model")
	})

	AfterAll(func() {
		if os.Getenv("LLMKUBE_E2E_PYRRA") != "true" {
			return
		}

		By("cleaning up the SLO test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", sloTestNs, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing the manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("removing the Pyrra kubernetes operator")
		// The ClusterRole/ClusterRoleBinding are cluster-scoped and survive a
		// namespace delete, so tear down every applied manifest explicitly
		// before deleting the namespace itself.
		for _, m := range pyrraKubernetesManifests {
			cmd = exec.Command("kubectl", "delete", "-f", pyrraManifestBaseURL+m, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		if pyrraCreatedMonitoringNS {
			cmd = exec.Command("kubectl", "delete", "ns", monitoringNs, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}

		By("removing the PrometheusRule and Pyrra SLO CRDs")
		cmd = exec.Command("kubectl", "delete", "-f", prometheusRuleCRDURL, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "-f", pyrraSLOCRDURL, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	It("renders the Pyrra SLO, Pyrra generates a PrometheusRule, and SLOReady goes True", func() {
		By("applying an InferenceService with spec.slo.objective set")
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: %s
  namespace: %s
spec:
  modelRef: %s
  slo:
    objective: "99.5"
`, isvcName, sloTestNs, modelName))
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply InferenceService")

		By("waiting for the ServiceLevelObjective to exist, owned by the InferenceService")
		verifySLO := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "servicelevelobjective", sloName,
				"-n", sloTestNs, "-o", "jsonpath={.metadata.ownerReferences[0].name}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal(isvcName))
		}
		Eventually(verifySLO, 2*time.Minute).Should(Succeed())

		By("waiting for Pyrra to generate the matching PrometheusRule")
		// Existence is the point of this assertion: it proves Pyrra's
		// kubernetes operator accepted the rendered ServiceLevelObjective
		// (indicator selector, target, window) well-formed enough to compile
		// into recording/alerting rules, without needing a running
		// Prometheus to scrape any of it.
		verifyPrometheusRule := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "prometheusrule", sloName,
				"-n", sloTestNs, "-o", "jsonpath={.metadata.name}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal(sloName))
		}
		Eventually(verifyPrometheusRule, 2*time.Minute).Should(Succeed())

		By("waiting for InferenceService status.conditions[SLOReady] to be True")
		verifyReady := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "inferenceservice", isvcName,
				"-n", sloTestNs, "-o",
				`jsonpath={.status.conditions[?(@.type=="SLOReady")].status}`)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("True"))
		}
		Eventually(verifyReady, 1*time.Minute).Should(Succeed())
	})

	It("garbage-collects the ServiceLevelObjective when the InferenceService is deleted", func() {
		By("deleting the InferenceService")
		cmd := exec.Command("kubectl", "delete", "inferenceservice", isvcName, "-n", sloTestNs)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete InferenceService")

		By("waiting for the owner-referenced ServiceLevelObjective to be garbage-collected")
		verifySLOGone := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "servicelevelobjective", sloName,
				"-n", sloTestNs, "--ignore-not-found", "-o", "jsonpath={.metadata.name}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(BeEmpty())
		}
		Eventually(verifySLOGone, 1*time.Minute).Should(Succeed())
	})
})
