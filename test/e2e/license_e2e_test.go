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

var _ = Describe("License E2E Tests", Ordered, func() {
	var cliPath string

	BeforeAll(func() {
		By("building the llmkube CLI")
		cmd := exec.Command("make", "build-cli")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to build CLI")
		cliPath = "bin/llmkube"
	})

	Context("license list (no cluster needed)", func() {
		It("should list known licenses with table headers", func() {
			cmd := exec.Command("./"+cliPath, "license", "list")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to run license list")

			Expect(output).To(ContainSubstring("ID"))
			Expect(output).To(ContainSubstring("NAME"))
			Expect(output).To(ContainSubstring("COMMERCIAL"))
			Expect(output).To(ContainSubstring("RESTRICTIONS"))
		})

		It("should contain known license IDs", func() {
			cmd := exec.Command("./"+cliPath, "license", "list")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Expect(output).To(ContainSubstring("apache-2.0"))
			Expect(output).To(ContainSubstring("mit"))
			Expect(output).To(ContainSubstring("gemma"))
		})

		It("should show license names", func() {
			cmd := exec.Command("./"+cliPath, "license", "list")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Expect(output).To(ContainSubstring("Apache License 2.0"))
			Expect(output).To(ContainSubstring("MIT License"))
			Expect(output).To(ContainSubstring("Gemma Terms of Use"))
		})

		It("should show license in main help", func() {
			cmd := exec.Command("./"+cliPath, "--help")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Expect(output).To(ContainSubstring("license"))
		})

		It("should show license subcommands in help", func() {
			cmd := exec.Command("./"+cliPath, "license", "--help")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Expect(output).To(ContainSubstring("list"))
			Expect(output).To(ContainSubstring("check"))
		})
	})

	Context("license check (requires cluster)", func() {
		const licenseTestNs = "e2e-license-test"
		const testLicenseModelServerURL = "http://test-model-server.e2e-license-test.svc.cluster.local/test-model.gguf"

		BeforeAll(func() {
			By("creating license test namespace")
			cmd := exec.Command("kubectl", "create", "ns", licenseTestNs)
			_, err := utils.Run(cmd)
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
			cmd := exec.Command("./"+cliPath, "license", "check", "nonexistent-model", "-n", licenseTestNs)
			_, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "Expected error for non-existent model")
		})

		It("should report no license info when GGUF metadata is absent", func() {
			cmd := exec.Command("./"+cliPath, "license", "check", "license-test-model", "-n", licenseTestNs)
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
			cmd = exec.Command("./"+cliPath, "license", "check", "license-test-model", "-n", licenseTestNs)
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
			cmd = exec.Command("./"+cliPath, "license", "check", "license-test-model", "-n", licenseTestNs)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Expect(output).To(ContainSubstring("custom-proprietary-v1"))
			Expect(output).To(ContainSubstring("Unknown license"))
		})
	})
})
