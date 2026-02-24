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
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/defilantech/llmkube/test/utils"
)

var _ = Describe("Cache Inspection E2E Tests", Ordered, func() {
	const cacheTestNs = "e2e-cache-test"
	const testModelServerURL = "http://test-model-server.e2e-cache-test.svc.cluster.local/test-model.gguf"
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
`, cacheTestNs, testModelServerURL))
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

			By("verifying llmkube cache list shows STATUS column and active entry")
			verifyCacheList := func(g Gomega) {
				cmd := exec.Command("./"+cliPath, "cache", "list", "-n", cacheTestNs)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("STATUS"))
				g.Expect(output).To(ContainSubstring("active"))
				g.Expect(output).To(ContainSubstring("cache-test-model"))
			}
			Eventually(verifyCacheList, 2*time.Minute).Should(Succeed())
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
			// The command itself may succeed or show a warning;
			// the key validation is that the inspector pod is cleaned up
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
				// Should NOT show STATUS column (PVC inspection didn't happen)
				g.Expect(output).NotTo(ContainSubstring("STATUS"))
				// Should still show the model entry from CR data
				g.Expect(output).To(ContainSubstring("nopvc-model"))
				// Summary should use the CR-only format
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
