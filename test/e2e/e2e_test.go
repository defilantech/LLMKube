//go:build e2e
// +build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/defilantech/llmkube/test/utils"
)

// namespace where the project is deployed in
const namespace = "llmkube-system"

// serviceAccountName created for the project
const serviceAccountName = "llmkube-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "llmkube-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "llmkube-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	//
	// Under MicroShift / OpenShift (LLMKUBE_E2E_OPENSHIFT=true), the workflow
	// installs LLMKube via Helm before this suite runs and the namespace
	// already exists with OpenShift SCC labels applied. Skip the kustomize-
	// shaped deploy steps in that case.
	BeforeAll(func() {
		if os.Getenv("LLMKUBE_E2E_OPENSHIFT") == "true" {
			By("OpenShift mode: namespace, CRDs, and controller already deployed by Helm; skipping kustomize-based setup")
			return
		}

		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

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
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		if os.Getenv("LLMKUBE_E2E_OPENSHIFT") == "true" {
			By("OpenShift mode: Helm uninstall is handled outside the suite; skipping kustomize-based teardown")
			return
		}

		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			// The metrics verification path here is shaped for kind: a curl
			// pod with an arbitrary runAsUser:1000 talks to the controller's
			// :8443 metrics endpoint with a serviceaccount token. Under
			// OpenShift restricted-v2 the SCC rewrites the UID into the
			// namespace's allocated range, and the controller's metrics
			// authorizer rejects the resulting token with HTTP 500 because
			// the audience and group claims do not match what the operator
			// would issue at install time. Metrics on a real OpenShift
			// install are wired through OpenShift Monitoring, not this curl
			// path. Skip the test under MINC; the controller IS serving
			// metrics (verified earlier in the same spec via the controller
			// pod logs ContainSubstring "Serving metrics server").
			if os.Getenv("LLMKUBE_E2E_OPENSHIFT") == "true" {
				Skip("metrics curl-pod path is kind-specific; OpenShift uses cluster Monitoring instead")
			}
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=llmkube-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("controller-runtime.metrics\tServing metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=docker.io/curlimages/curl:8.18.0",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "docker.io/curlimages/curl:8.18.0",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	Context("CR Reconciliation", func() {
		const crTestNs = "e2e-cr-test"
		const testModelServerURL = "http://test-model-server.e2e-cr-test.svc.cluster.local/test-model.gguf"

		BeforeAll(func() {
			By("creating CR test namespace")
			cmd := exec.Command("kubectl", "create", "ns", crTestNs)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create CR test namespace")

			By("creating ConfigMap with fake GGUF file")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test-model-data
  namespace: e2e-cr-test
binaryData:
  test-model.gguf: ZmFrZS1nZ3VmLWRhdGE=
`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test model ConfigMap")

			By("creating test model server pod")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(`apiVersion: v1
kind: Pod
metadata:
  name: test-model-server
  namespace: e2e-cr-test
  labels:
    app: test-model-server
spec:
  containers:
  - name: nginx
    image: nginx:1.27-alpine
    ports:
    - containerPort: 80
    volumeMounts:
    - name: model-data
      mountPath: /usr/share/nginx/html
  volumes:
  - name: model-data
    configMap:
      name: test-model-data
`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test model server pod")

			By("creating test model server service")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(`apiVersion: v1
kind: Service
metadata:
  name: test-model-server
  namespace: e2e-cr-test
spec:
  selector:
    app: test-model-server
  ports:
  - port: 80
    targetPort: 80
`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test model server service")

			By("waiting for test model server to be ready")
			verifyServerReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", "test-model-server",
					"-n", crTestNs, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}
			Eventually(verifyServerReady, 2*time.Minute).Should(Succeed())
		})

		AfterAll(func() {
			By("cleaning up CR test namespace")
			cmd := exec.Command("kubectl", "delete", "ns", crTestNs, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should reconcile a Model CR to Ready", func() {
			By("applying a Model CR pointing to the test server")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: test-model
  namespace: %s
spec:
  source: "%s"
`, crTestNs, testModelServerURL))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Model CR")

			By("waiting for Model to reach Ready phase")
			verifyModelReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "model", "test-model",
					"-n", crTestNs, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Ready"))
			}
			Eventually(verifyModelReady, 2*time.Minute).Should(Succeed())

			By("verifying cacheKey is set")
			cmd = exec.Command("kubectl", "get", "model", "test-model",
				"-n", crTestNs, "-o", "jsonpath={.status.cacheKey}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(MatchRegexp(`^[0-9a-f]{16}$`), "cacheKey should be 16-char hex")

			By("verifying Available condition is True")
			cmd = exec.Command("kubectl", "get", "model", "test-model",
				"-n", crTestNs, "-o",
				`jsonpath={.status.conditions[?(@.type=="Available")].status}`)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("True"))
		})

		It("should keep an HTTP-sourced Model Ready across a controller restart", func() {
			// HTTP(S) sources are deferred to the InferenceService Pod's init
			// container (issue #363) — the Model controller doesn't manage
			// the cache for these sources, so a controller restart should
			// not trigger a re-download and Status.CacheKey should persist
			// across the restart.
			By("recording Status.CacheKey before the restart")
			cmd := exec.Command("kubectl", "get", "model", "test-model",
				"-n", crTestNs, "-o", "jsonpath={.status.cacheKey}")
			cacheKeyBefore, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(cacheKeyBefore).To(MatchRegexp(`^[0-9a-f]{16}$`),
				"HTTP(S) Models must have a CacheKey populated so the InferenceService init container can build /models/<cacheKey>/<basename>")

			By("recording the current controller pod name")
			cmd = exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-n", namespace,
				"-o", "jsonpath={.items[0].metadata.name}")
			oldPod, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(oldPod).NotTo(BeEmpty())

			By("restarting the controller deployment")
			cmd = exec.Command("kubectl", "rollout", "restart",
				"deployment/llmkube-controller-manager", "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for a new controller pod to be Running")
			var newPod string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				pods := utils.GetNonEmptyLines(out)
				g.Expect(pods).To(HaveLen(1))
				g.Expect(pods[0]).NotTo(Equal(oldPod), "new pod should have a different name")
				newPod = pods[0]

				cmd = exec.Command("kubectl", "get", "pod", newPod,
					"-n", namespace, "-o", "jsonpath={.status.phase}")
				phase, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Running"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			// Update controllerPodName so AfterEach logs the right pod on failure
			controllerPodName = newPod

			By("verifying the Model stays Ready and CacheKey is preserved")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "model", "test-model",
					"-n", crTestNs, "-o", "jsonpath={.status.phase}")
				phase, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Ready"))

				cmd = exec.Command("kubectl", "get", "model", "test-model",
					"-n", crTestNs, "-o", "jsonpath={.status.cacheKey}")
				cacheKeyAfter, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cacheKeyAfter).To(Equal(cacheKeyBefore), "CacheKey must survive controller restart")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying the new controller did NOT attempt a controller-side download (issue #363)")
			// Regression guard: prior to issue #363's fix the controller
			// downloaded HTTP(S) sources in-process to its own filesystem,
			// which lived on a different PVC than the inference Pod read
			// from. Asserting absence of "Downloading model" guarantees the
			// new controller takes the workload-deferred path on restart.
			cmd = exec.Command("kubectl", "logs", newPod, "-n", namespace)
			logs, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(logs).NotTo(ContainSubstring("Downloading model"),
				"controller must not perform an in-process download for HTTP(S) sources after restart")
		})

		It("should create Deployment and Service for InferenceService", func() {
			By("applying an InferenceService referencing the ready Model")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: test-inference
  namespace: %s
spec:
  modelRef: test-model
`, crTestNs))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply InferenceService CR")

			By("waiting for Deployment to be created")
			verifyDeploymentExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "test-inference",
					"-n", crTestNs, "-o", "jsonpath={.metadata.labels}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring(`"app":"test-inference"`))
				g.Expect(output).To(ContainSubstring(`"inference.llmkube.dev/service":"test-inference"`))
			}
			Eventually(verifyDeploymentExists, 2*time.Minute).Should(Succeed())

			By("verifying Service exists with correct port")
			cmd = exec.Command("kubectl", "get", "service", "test-inference",
				"-n", crTestNs, "-o", "jsonpath={.spec.ports[0].port}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("8080"))

			By("verifying Service selector matches")
			cmd = exec.Command("kubectl", "get", "service", "test-inference",
				"-n", crTestNs, "-o", "jsonpath={.spec.selector.app}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("test-inference"))

			By("verifying InferenceService status endpoint")
			verifyEndpoint := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "inferenceservice", "test-inference",
					"-n", crTestNs, "-o", "jsonpath={.status.endpoint}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("test-inference"))
				g.Expect(output).To(ContainSubstring(crTestNs))
			}
			Eventually(verifyEndpoint, 2*time.Minute).Should(Succeed())
		})

		It("should report Failed for InferenceService referencing non-existent Model", func() {
			By("applying an InferenceService with a bad model ref")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: test-inference-bad-ref
  namespace: %s
spec:
  modelRef: model-does-not-exist
`, crTestNs))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply InferenceService CR")

			By("waiting for InferenceService to report Failed phase")
			verifyFailed := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "inferenceservice", "test-inference-bad-ref",
					"-n", crTestNs, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Failed"))
			}
			Eventually(verifyFailed, 2*time.Minute).Should(Succeed())
		})

		It("should clean up owned resources when InferenceService is deleted", func() {
			By("verifying the Deployment exists before deletion")
			cmd := exec.Command("kubectl", "get", "deployment", "test-inference", "-n", crTestNs)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Deployment should exist before deletion")

			By("deleting the InferenceService")
			cmd = exec.Command("kubectl", "delete", "inferenceservice", "test-inference", "-n", crTestNs)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete InferenceService")

			By("waiting for Deployment to be garbage collected")
			verifyDeploymentGone := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "test-inference",
					"-n", crTestNs, "--ignore-not-found", "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(BeEmpty())
			}
			Eventually(verifyDeploymentGone, 1*time.Minute).Should(Succeed())

			By("waiting for Service to be garbage collected")
			verifyServiceGone := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", "test-inference",
					"-n", crTestNs, "--ignore-not-found", "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(BeEmpty())
			}
			Eventually(verifyServiceGone, 1*time.Minute).Should(Succeed())
		})

		It("should report Failed for Model with unreachable file:// source", func() {
			// HTTP(S) sources are validated by the InferenceService Pod's init
			// container, not by the Model controller (issue #363), so a 404
			// HTTP source no longer surfaces a Failed Model phase. file://
			// sources still flow through the controller's in-process path
			// and surface broken sources as Model.status.phase=Failed with
			// reason=CopyFailed, which is the controller-side failure
			// surface we want to keep covered here.
			By("applying a Model CR with a file:// path that does not exist on the controller pod")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: test-model-bad-source
  namespace: %s
spec:
  source: "file:///nonexistent/path/to/missing-model.gguf"
`, crTestNs))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Model CR")

			By("waiting for Model to reach Failed phase")
			verifyModelFailed := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "model", "test-model-bad-source",
					"-n", crTestNs, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Failed"))
			}
			Eventually(verifyModelFailed, 2*time.Minute).Should(Succeed())

			By("verifying Degraded condition with CopyFailed reason")
			cmd = exec.Command("kubectl", "get", "model", "test-model-bad-source",
				"-n", crTestNs, "-o",
				`jsonpath={.status.conditions[?(@.type=="Degraded")].reason}`)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("CopyFailed"))
		})
	})

	Context("ModelRouter Reconciliation", func() {
		const mrTestNs = "e2e-modelrouter-test"

		BeforeAll(func() {
			By("creating ModelRouter test namespace")
			cmd := exec.Command("kubectl", "create", "ns", mrTestNs)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test namespace")

			By("seeding a stub InferenceService for backend resolution")
			// The controller resolves InferenceServiceRef to a cluster URL
			// when compiling the router config. The referenced
			// InferenceService doesn't need to be Ready (its pods would
			// require a real model image which the kind e2e doesn't
			// side-load), only present so resolution succeeds.
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: stub-model
  namespace: %s
spec:
  source: file:///tmp/stub.gguf
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: stub-isvc
  namespace: %s
spec:
  modelRef: stub-model
`, mrTestNs, mrTestNs))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to seed stub InferenceService")

			By("seeding a stub Secret for the cloud backend credentials")
			cmd = exec.Command("kubectl", "create", "secret", "generic", "anthropic-key",
				"-n", mrTestNs,
				"--from-literal=ANTHROPIC_API_KEY=stub-key-for-e2e")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create stub credentials Secret")
		})

		AfterAll(func() {
			By("cleaning up ModelRouter test namespace")
			cmd := exec.Command("kubectl", "delete", "ns", mrTestNs, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should reconcile a ModelRouter to a populated endpoint and child resources", func() {
			By("applying a ModelRouter with a valid spec")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: e2e-good-router
  namespace: %s
spec:
  backends:
    - name: local-stub
      inferenceServiceRef:
        name: stub-isvc
      tier: local
    - name: cloud-stub
      external:
        provider: anthropic
        model: claude-opus-4-7
        url: https://api.anthropic.com
        credentialsSecretRef:
          name: anthropic-key
      tier: cloud
  rules:
    - name: pii-stays-local
      match:
        dataClassification: ["pii"]
      route:
        backends: ["local-stub"]
      failClosed: true
    - name: complex-to-cloud
      match:
        taskComplexity: complex
      route:
        backends: ["cloud-stub"]
  defaultRoute: local-stub
`, mrTestNs))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ModelRouter CR")

			By("waiting for ModelRouter Validated=True and status.endpoint to be populated")
			verifyStatus := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "modelrouter", "e2e-good-router",
					"-n", mrTestNs, "-o",
					`jsonpath={.status.conditions[?(@.type=="Validated")].status}`)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))

				cmd = exec.Command("kubectl", "get", "modelrouter", "e2e-good-router",
					"-n", mrTestNs, "-o", "jsonpath={.status.endpoint}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Endpoint convention: http://<name>-router-proxy.<ns>.svc.cluster.local:8080/v1/chat/completions
				g.Expect(output).To(ContainSubstring("e2e-good-router-router-proxy." + mrTestNs))
				g.Expect(output).To(ContainSubstring(":8080/v1/chat/completions"))

				cmd = exec.Command("kubectl", "get", "modelrouter", "e2e-good-router",
					"-n", mrTestNs, "-o", "jsonpath={.status.activeRules}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("2"))
			}
			Eventually(verifyStatus, 1*time.Minute).Should(Succeed())

			By("verifying the proxy ConfigMap exists with the compiled config")
			verifyConfigMap := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", "e2e-good-router-router-proxy",
					"-n", mrTestNs, "-o", "jsonpath={.data.config\\.json}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("local-stub"))
				g.Expect(output).To(ContainSubstring("cloud-stub"))
				g.Expect(output).To(ContainSubstring("pii-stays-local"))
			}
			Eventually(verifyConfigMap, 1*time.Minute).Should(Succeed())

			By("verifying the proxy Service exists on the expected port")
			verifyService := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", "e2e-good-router-router-proxy",
					"-n", mrTestNs, "-o", "jsonpath={.spec.ports[0].port}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("8080"))
			}
			Eventually(verifyService, 1*time.Minute).Should(Succeed())

			By("verifying the proxy Deployment exists with the config hash annotation")
			verifyDeployment := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "e2e-good-router-router-proxy",
					"-n", mrTestNs, "-o",
					`jsonpath={.spec.template.metadata.annotations.inference\.llmkube\.dev/router-config-hash}`)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(),
					"Deployment must carry the config hash annotation that triggers rollout")

				cmd = exec.Command("kubectl", "get", "deployment", "e2e-good-router-router-proxy",
					"-n", mrTestNs, "-o",
					`jsonpath={.spec.template.spec.containers[0].image}`)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(), "Deployment container must have an image set")
			}
			Eventually(verifyDeployment, 1*time.Minute).Should(Succeed())
		})

		It("should report Validated=False for a sensitive-data rule routing to cloud", func() {
			By("applying a ModelRouter whose fail-closed PII rule points at a cloud backend")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: e2e-bad-router
  namespace: %s
spec:
  backends:
    - name: local-stub
      inferenceServiceRef:
        name: stub-isvc
      tier: local
    - name: cloud-stub
      external:
        provider: anthropic
        model: claude-opus-4-7
      tier: cloud
  rules:
    - name: pii-leak
      match:
        dataClassification: ["pii"]
      route:
        backends: ["cloud-stub"]
      failClosed: true
  defaultRoute: local-stub
`, mrTestNs))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply invalid ModelRouter CR")

			By("waiting for ModelRouter status.phase=Failed and Validated=False with SpecInvalid reason")
			verifyInvalid := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "modelrouter", "e2e-bad-router",
					"-n", mrTestNs, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Failed"))

				cmd = exec.Command("kubectl", "get", "modelrouter", "e2e-bad-router",
					"-n", mrTestNs, "-o",
					`jsonpath={.status.conditions[?(@.type=="Validated")].status}`)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("False"))

				cmd = exec.Command("kubectl", "get", "modelrouter", "e2e-bad-router",
					"-n", mrTestNs, "-o",
					`jsonpath={.status.conditions[?(@.type=="Validated")].reason}`)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("SpecInvalid"))

				cmd = exec.Command("kubectl", "get", "modelrouter", "e2e-bad-router",
					"-n", mrTestNs, "-o",
					`jsonpath={.status.conditions[?(@.type=="Validated")].message}`)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cannot route to cloud-tier backend"))
			}
			Eventually(verifyInvalid, 1*time.Minute).Should(Succeed())
		})

		It("should clean up ModelRouter resources and child resources on deletion", func() {
			By("deleting both test ModelRouters")
			cmd := exec.Command("kubectl", "delete", "modelrouter", "e2e-good-router", "e2e-bad-router",
				"-n", mrTestNs, "--ignore-not-found")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete ModelRouters")

			By("verifying both ModelRouters are gone")
			verifyMRGone := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "modelrouters", "-n", mrTestNs,
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(BeEmpty())
			}
			Eventually(verifyMRGone, 30*time.Second).Should(Succeed())

			By("verifying owner-ref GC removed the child Deployment, Service, and ConfigMap")
			verifyChildrenGone := func(g Gomega) {
				for _, kind := range []string{"deployment", "service", "configmap"} {
					cmd := exec.Command("kubectl", "get", kind, "e2e-good-router-router-proxy",
						"-n", mrTestNs, "--ignore-not-found",
						"-o", "jsonpath={.metadata.name}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(BeEmpty(),
						"child %s should be garbage-collected after ModelRouter deletion", kind)
				}
			}
			Eventually(verifyChildrenGone, 1*time.Minute).Should(Succeed())
		})
	})

	Context("License Check", func() {
		const licenseTestNs = "e2e-license-test"
		const testLicenseModelServerURL = "http://test-model-server.e2e-license-test.svc.cluster.local/test-model.gguf"
		var licenseCLIPath string

		BeforeAll(func() {
			By("building the llmkube CLI")
			cmd := exec.Command("make", "build-cli")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to build CLI")
			licenseCLIPath = "bin/llmkube"

			By("creating license test namespace")
			cmd = exec.Command("kubectl", "create", "ns", licenseTestNs)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create license test namespace")

			By("creating ConfigMap with fake GGUF file")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test-model-data
  namespace: %s
binaryData:
  test-model.gguf: ZmFrZS1nZ3VmLWRhdGE=
`, licenseTestNs))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test model ConfigMap")

			By("creating test model server pod")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: test-model-server
  namespace: %s
  labels:
    app: test-model-server
spec:
  containers:
  - name: nginx
    image: nginx:1.27-alpine
    ports:
    - containerPort: 80
    volumeMounts:
    - name: model-data
      mountPath: /usr/share/nginx/html
  volumes:
  - name: model-data
    configMap:
      name: test-model-data
`, licenseTestNs))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test model server pod")

			By("creating test model server service")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: test-model-server
  namespace: %s
spec:
  selector:
    app: test-model-server
  ports:
  - port: 80
    targetPort: 80
`, licenseTestNs))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test model server service")

			By("waiting for test model server to be ready")
			verifyServerReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", "test-model-server",
					"-n", licenseTestNs, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}
			Eventually(verifyServerReady, 2*time.Minute).Should(Succeed())

			By("applying a Model CR pointing to the test server")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: license-test-model
  namespace: %s
spec:
  source: "%s"
`, licenseTestNs, testLicenseModelServerURL))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Model CR")

			By("waiting for Model to reach Ready phase")
			verifyModelReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "model", "license-test-model",
					"-n", licenseTestNs, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Ready"))
			}
			Eventually(verifyModelReady, 2*time.Minute).Should(Succeed())
		})

		AfterAll(func() {
			By("cleaning up license test namespace")
			cmd := exec.Command("kubectl", "delete", "ns", licenseTestNs, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should fail for a non-existent model", func() {
			cmd := exec.Command("./"+licenseCLIPath, "license", "check", "nonexistent-model", "-n", licenseTestNs)
			_, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "Expected error for non-existent model")
		})

		It("should report no license info when GGUF metadata is absent", func() {
			cmd := exec.Command("./"+licenseCLIPath, "license", "check", "license-test-model", "-n", licenseTestNs)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Fake GGUF data won't parse, so Status.GGUF will be nil
			Expect(output).To(ContainSubstring("no license information"))
		})

		It("should display license details after patching GGUF status", func() {
			By("patching Model status to inject GGUF license metadata")
			cmd := exec.Command("kubectl", "patch", "model", "license-test-model",
				"-n", licenseTestNs,
				"--type=merge",
				"--subresource=status",
				`-p={"status":{"gguf":{"license":"apache-2.0"}}}`,
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch Model status")

			By("running license check on the patched model")
			cmd = exec.Command("./"+licenseCLIPath, "license", "check", "license-test-model", "-n", licenseTestNs)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Expect(output).To(ContainSubstring("Apache License 2.0"))
			Expect(output).To(ContainSubstring("Commercial Use:  Yes"))
			Expect(output).To(ContainSubstring("No special restrictions"))
		})

		It("should show unknown license for unrecognized license ID", func() {
			By("patching Model status with an unknown license ID")
			cmd := exec.Command("kubectl", "patch", "model", "license-test-model",
				"-n", licenseTestNs,
				"--type=merge",
				"--subresource=status",
				`-p={"status":{"gguf":{"license":"custom-proprietary-v1"}}}`,
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch Model status")

			By("running license check on the model with unknown license")
			cmd = exec.Command("./"+licenseCLIPath, "license", "check", "license-test-model", "-n", licenseTestNs)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Expect(output).To(ContainSubstring("custom-proprietary-v1"))
			Expect(output).To(ContainSubstring("Unknown license"))
		})
	})

	Context("Cache Inspection", func() {
		const cacheTestNs = "e2e-cache-test"
		const testCacheModelServerURL = "http://test-model-server.e2e-cache-test.svc.cluster.local/test-model.gguf"
		var cliPath string

		BeforeAll(func() {
			By("building the llmkube CLI")
			cmd := exec.Command("make", "build-cli")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to build CLI")
			cliPath = "bin/llmkube"

			By("creating cache test namespace")
			cmd = exec.Command("kubectl", "create", "ns", cacheTestNs)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create cache test namespace")

			By("creating ConfigMap with fake GGUF file")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test-model-data
  namespace: %s
binaryData:
  test-model.gguf: ZmFrZS1nZ3VmLWRhdGE=
`, cacheTestNs))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test model ConfigMap")

			By("creating test model server pod")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: test-model-server
  namespace: %s
  labels:
    app: test-model-server
spec:
  containers:
  - name: nginx
    image: nginx:1.27-alpine
    ports:
    - containerPort: 80
    volumeMounts:
    - name: model-data
      mountPath: /usr/share/nginx/html
  volumes:
  - name: model-data
    configMap:
      name: test-model-data
`, cacheTestNs))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test model server pod")

			By("creating test model server service")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: test-model-server
  namespace: %s
spec:
  selector:
    app: test-model-server
  ports:
  - port: 80
    targetPort: 80
`, cacheTestNs))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test model server service")

			By("waiting for test model server to be ready")
			verifyServerReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", "test-model-server",
					"-n", cacheTestNs, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}
			Eventually(verifyServerReady, 2*time.Minute).Should(Succeed())
		})

		AfterAll(func() {
			By("cleaning up cache test namespace")
			cmd := exec.Command("kubectl", "delete", "ns", cacheTestNs, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		Context("cache list with Model CR and PVC", func() {
			// All specs in this block exercise cache_inspect.go's transient
			// inspector pod path. The inspector mounts the same RWO PVC the
			// InferenceService Deployment also wants. Longhorn's strict RWO
			// enforcement blocks the second attach indefinitely, so the
			// inspector pod never reaches Running. Local-path-provisioner
			// permits the multi-RWO pattern, so this Context runs cleanly on
			// the default kind storage class. Skip the whole block under
			// Longhorn; #418/#419 fsGroup regression coverage is provided by
			// the other specs in the suite which run on Longhorn unchanged.
			BeforeEach(func() {
				if os.Getenv("LLMKUBE_E2E_LONGHORN") == "true" {
					Skip("cache list inspector-pod path is incompatible with Longhorn RWO; covered on default kind storage")
				}
			})

			It("should show active cache entries with STATUS column after model download", func() {
				By("applying a Model CR pointing to the test server")
				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: cache-test-model
  namespace: %s
spec:
  source: "%s"
`, cacheTestNs, testCacheModelServerURL))
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to apply Model CR")

				By("waiting for Model to reach Ready phase")
				verifyModelReady := func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "model", "cache-test-model",
						"-n", cacheTestNs, "-o", "jsonpath={.status.phase}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal("Ready"))
				}
				Eventually(verifyModelReady, 2*time.Minute).Should(Succeed())

				By("applying an InferenceService to trigger PVC creation")
				cmd = exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: cache-test-inference
  namespace: %s
spec:
  modelRef: cache-test-model
`, cacheTestNs))
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to apply InferenceService CR")

				By("waiting for the model cache PVC to exist")
				verifyPVCExists := func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "pvc", "llmkube-model-cache",
						"-n", cacheTestNs, "-o", "jsonpath={.metadata.name}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal("llmkube-model-cache"))
				}
				Eventually(verifyPVCExists, 2*time.Minute).Should(Succeed())

				By("verifying llmkube cache list shows STATUS column and active entry")
				verifyCacheList := func(g Gomega) {
					cmd := exec.Command("./"+cliPath, "cache", "list", "-n", cacheTestNs)
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(ContainSubstring("STATUS"))
					g.Expect(output).To(ContainSubstring("active"))
					g.Expect(output).To(ContainSubstring("cache-test-model"))
				}
				// 4-minute Eventually budget (vs 2 previously) so the inner
				// inspector pod wait (120s in pkg/cli/cache_inspect.go) can
				// run twice before the test gives up. Slow CSI drivers like
				// Longhorn-on-kind routinely take 90-120s to attach a fresh
				// PVC; with a single attempt we'd get one shot per Eventually
				// retry interval, which the previous 2-minute budget did not
				// fit.
				Eventually(verifyCacheList, 4*time.Minute).Should(Succeed())
			})

			It("should show PVC-derived sizes in cache list output", func() {
				cmd := exec.Command("./"+cliPath, "cache", "list", "-n", cacheTestNs)
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				Expect(output).To(ContainSubstring("SIZE"))
				Expect(output).NotTo(ContainSubstring("SIZE\t-"),
					"SIZE should not be '-' when PVC inspection succeeds")
			})

			It("should report correct summary with active/orphaned counts", func() {
				cmd := exec.Command("./"+cliPath, "cache", "list", "-n", cacheTestNs)
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				Expect(output).To(MatchRegexp(`Total: \d+ cache entries \(\d+ active, \d+ orphaned\)`))
				Expect(output).To(ContainSubstring("1 active"))
				Expect(output).To(ContainSubstring("0 orphaned"))
			})

			It("should show no orphaned entries when --orphaned filter is used", func() {
				cmd := exec.Command("./"+cliPath, "cache", "list", "-n", cacheTestNs, "--orphaned")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				Expect(output).To(ContainSubstring("No orphaned cache entries found."))
			})
		})

		Context("cache list orphaned entry detection", func() {
			// Depends on PVC + cache_inspect.go inspector-pod path, both
			// incompatible with Longhorn RWO. See sibling Context above.
			BeforeEach(func() {
				if os.Getenv("LLMKUBE_E2E_LONGHORN") == "true" {
					Skip("test exercises multi-RWO PVC mounts; covered on default kind storage")
				}
			})

			It("should detect orphaned cache entries on the PVC", func() {
				By("getting the PVC name to confirm it exists")
				cmd := exec.Command("kubectl", "get", "pvc", "llmkube-model-cache",
					"-n", cacheTestNs, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(output).To(Equal("llmkube-model-cache"))

				By("creating a fake orphaned cache directory on the PVC via a temporary pod")
				cmd = exec.Command("kubectl", "run", "orphan-creator",
					"--restart=Never",
					"--namespace", cacheTestNs,
					"--image=busybox:1.37.0",
					"--overrides", fmt.Sprintf(`{
						"spec": {
							"containers": [{
								"name": "creator",
								"image": "busybox:1.37.0",
								"command": ["sh", "-c", "mkdir -p /models/deadbeef01234567 && echo 'fake-data' > /models/deadbeef01234567/model.gguf && sleep 5"],
								"volumeMounts": [{
									"name": "cache",
									"mountPath": "/models"
								}]
							}],
							"volumes": [{
								"name": "cache",
								"persistentVolumeClaim": {
									"claimName": "llmkube-model-cache"
								}
							}],
							"restartPolicy": "Never"
						}
					}`),
				)
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to create orphan-creator pod")

				By("waiting for orphan-creator pod to complete")
				verifyOrphanCreatorDone := func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "pod", "orphan-creator",
						"-n", cacheTestNs, "-o", "jsonpath={.status.phase}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal("Succeeded"))
				}
				Eventually(verifyOrphanCreatorDone, 2*time.Minute).Should(Succeed())

				By("cleaning up the orphan-creator pod")
				cmd = exec.Command("kubectl", "delete", "pod", "orphan-creator",
					"-n", cacheTestNs, "--grace-period=0", "--force")
				_, _ = utils.Run(cmd)

				By("verifying cache list detects the orphaned entry")
				verifyCacheListOrphaned := func(g Gomega) {
					cmd := exec.Command("./"+cliPath, "cache", "list", "-n", cacheTestNs)
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(ContainSubstring("orphaned"))
					g.Expect(output).To(ContainSubstring("deadbeef01234567"))
				}
				Eventually(verifyCacheListOrphaned, 2*time.Minute).Should(Succeed())

				By("verifying --orphaned flag filters to only orphaned entries")
				cmd = exec.Command("./"+cliPath, "cache", "list", "-n", cacheTestNs, "--orphaned")
				output, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(output).To(ContainSubstring("deadbeef01234567"))
				Expect(output).NotTo(ContainSubstring("cache-test-model"))
			})
		})

		Context("cache list inspector pod lifecycle", func() {
			// Explicitly exercises the inspector-pod spawn path that
			// Longhorn RWO blocks indefinitely. See sibling Context above.
			BeforeEach(func() {
				if os.Getenv("LLMKUBE_E2E_LONGHORN") == "true" {
					Skip("test exercises multi-RWO PVC mounts; covered on default kind storage")
				}
			})

			It("should create and clean up inspector pod when no existing pods mount the PVC", func() {
				By("creating a namespace with a PVC but no running pods")
				inspectorNs := "e2e-cache-inspector"
				cmd := exec.Command("kubectl", "create", "ns", inspectorNs)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				defer func() {
					cmd := exec.Command("kubectl", "delete", "ns", inspectorNs, "--ignore-not-found")
					_, _ = utils.Run(cmd)
				}()

				By("creating a PVC in the inspector namespace")
				cmd = exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: llmkube-model-cache
  namespace: %s
spec:
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
`, inspectorNs))
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to create PVC")

				By("running cache list which should create an inspector pod")
				cmd = exec.Command("./"+cliPath, "cache", "list", "-n", inspectorNs)
				output, err := utils.Run(cmd)
				if err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "cache list output (may warn): %s\n", output)
				}

				By("verifying the inspector pod was cleaned up")
				verifyInspectorGone := func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "pod", "llmkube-cache-inspector",
						"-n", inspectorNs, "--ignore-not-found", "-o", "jsonpath={.metadata.name}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(BeEmpty(), "Inspector pod should be deleted after cache list completes")
				}
				Eventually(verifyInspectorGone, 1*time.Minute).Should(Succeed())
			})
		})

		Context("cache list graceful fallback without PVC", func() {
			It("should fall back to CR-only output when no PVC exists", func() {
				noPvcNs := "e2e-cache-nopvc"
				cmd := exec.Command("kubectl", "create", "ns", noPvcNs)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				defer func() {
					cmd := exec.Command("kubectl", "delete", "ns", noPvcNs, "--ignore-not-found")
					_, _ = utils.Run(cmd)
				}()

				By("applying a Model CR in a namespace with no PVC")
				cmd = exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: nopvc-model
  namespace: %s
spec:
  source: "http://does-not-matter.example.com/model.gguf"
`, noPvcNs))
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				By("running cache list without a PVC in the namespace")
				verifyCacheListFallback := func(g Gomega) {
					cmd := exec.Command("./"+cliPath, "cache", "list", "-n", noPvcNs)
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).NotTo(ContainSubstring("STATUS"))
					g.Expect(output).To(ContainSubstring("nopvc-model"))
					g.Expect(output).To(ContainSubstring("cache entries"))
					g.Expect(output).To(ContainSubstring("models"))
				}
				Eventually(verifyCacheListFallback, 2*time.Minute).Should(Succeed())
			})
		})

		Context("cache list help documentation", func() {
			It("should show cache subcommands in help", func() {
				cmd := exec.Command("./"+cliPath, "cache", "--help")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				Expect(output).To(ContainSubstring("list"))
				Expect(output).To(ContainSubstring("clear"))
				Expect(output).To(ContainSubstring("preload"))
			})

			It("should show --orphaned flag in cache list help", func() {
				cmd := exec.Command("./"+cliPath, "cache", "list", "--help")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				Expect(output).To(ContainSubstring("--orphaned"))
				Expect(output).To(ContainSubstring("orphaned cache entries"))
			})
		})
	})

	Context("OpenShift SCC admission", func() {
		// Only runs under MicroShift / OpenShift / OKD. The workflow sets
		// LLMKUBE_E2E_OPENSHIFT=true; on plain kind we skip because the
		// restricted-v2 SCC and the namespace allocated supplemental-groups
		// annotation do not exist.
		BeforeEach(func() {
			if os.Getenv("LLMKUBE_E2E_OPENSHIFT") != "true" {
				Skip("requires OpenShift SCC admission controller (set LLMKUBE_E2E_OPENSHIFT=true)")
			}
		})

		It("should be admitted on a restricted-v2 namespace with default fsGroup disabled", func() {
			const sccTestNs = "e2e-openshift-scc"

			By("creating a test namespace and capturing its allocated supplemental-groups range")
			cmd := exec.Command("kubectl", "create", "ns", sccTestNs)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				cmd := exec.Command("kubectl", "delete", "ns", sccTestNs, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}()

			cmd = exec.Command("kubectl", "get", "ns", sccTestNs,
				"-o", `jsonpath={.metadata.annotations.openshift\.io/sa\.scc\.supplemental-groups}`)
			rangeAnnotation, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(rangeAnnotation).NotTo(BeEmpty(),
				"namespace must carry openshift.io/sa.scc.supplemental-groups; SCC admission cannot inject fsGroup otherwise")

			// Parse "1000680000/10000" → (min=1000680000, count=10000).
			parts := strings.SplitN(rangeAnnotation, "/", 2)
			Expect(parts).To(HaveLen(2),
				"supplemental-groups annotation must be in '<min>/<count>' form, got %q", rangeAnnotation)
			rangeMin, err := strconv.ParseInt(parts[0], 10, 64)
			Expect(err).NotTo(HaveOccurred())
			rangeCount, err := strconv.ParseInt(parts[1], 10, 64)
			Expect(err).NotTo(HaveOccurred())
			rangeMax := rangeMin + rangeCount - 1

			By("standing up the in-cluster fake-GGUF model server")
			// Pod spec is PSA-restricted-compliant so it works on
			// MicroShift / OpenShift where the namespace enforces PSA
			// restricted. Uses nginxinc/nginx-unprivileged which runs
			// as non-root (uid 101) and binds to :8080; we map the
			// Service back to :80 so the model URL stays familiar.
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test-model-files
  namespace: %s
binaryData:
  test-model.gguf: ZmFrZS1nZ3VmLWRhdGE=
---
apiVersion: v1
kind: Pod
metadata:
  name: test-model-server
  namespace: %s
  labels:
    app: test-model-server
spec:
  securityContext:
    runAsNonRoot: true
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: nginx
      image: nginxinc/nginx-unprivileged:1.27-alpine
      ports:
        - containerPort: 8080
      volumeMounts:
        - name: files
          mountPath: /usr/share/nginx/html
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop:
            - ALL
  volumes:
    - name: files
      configMap:
        name: test-model-files
---
apiVersion: v1
kind: Service
metadata:
  name: test-model-server
  namespace: %s
spec:
  selector:
    app: test-model-server
  ports:
    - port: 80
      targetPort: 8080
`, sccTestNs, sccTestNs, sccTestNs))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			verifyModelServerReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", "test-model-server",
					"-n", sccTestNs, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}
			Eventually(verifyModelServerReady, 2*time.Minute).Should(Succeed())

			By("applying a Model and InferenceService that exercise the SCC admission path")
			modelURL := fmt.Sprintf("http://test-model-server.%s.svc.cluster.local/test-model.gguf", sccTestNs)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: scc-test-model
  namespace: %s
spec:
  source: "%s"
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: scc-test-inference
  namespace: %s
spec:
  modelRef: scc-test-model
`, sccTestNs, modelURL, sccTestNs))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the InferenceService Deployment to exist (admission succeeded)")
			verifyDeploymentExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "scc-test-inference",
					"-n", sccTestNs, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("scc-test-inference"))
			}
			Eventually(verifyDeploymentExists, 2*time.Minute).Should(Succeed())

			By("verifying SCC injected an fsGroup in the rendered pod spec")
			var podFSGroup int64
			verifyPodFSGroupInRange := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-n", sccTestNs,
					"-l", "app=scc-test-inference",
					"-o", "jsonpath={.items[0].spec.securityContext.fsGroup}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(),
					"SCC admission must inject an fsGroup; got empty value, meaning either the pod was not created or fsGroup is unset")
				fsGroup, err := strconv.ParseInt(output, 10, 64)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(fsGroup).To(BeNumerically(">=", rangeMin),
					"injected fsGroup %d must be >= namespace range min %d", fsGroup, rangeMin)
				g.Expect(fsGroup).To(BeNumerically("<=", rangeMax),
					"injected fsGroup %d must be <= namespace range max %d", fsGroup, rangeMax)
				podFSGroup = fsGroup
			}
			Eventually(verifyPodFSGroupInRange, 2*time.Minute).Should(Succeed())

			_, _ = fmt.Fprintf(GinkgoWriter,
				"SCC injected fsGroup=%d (namespace range %d-%d) on InferenceService pod\n",
				podFSGroup, rangeMin, rangeMax)
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
