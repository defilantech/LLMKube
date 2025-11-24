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
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/defilantech/llmkube/test/utils"
)

var _ = Describe("Catalog E2E Tests", Ordered, func() {
	var cliPath string

	BeforeAll(func() {
		// Build the CLI binary
		By("building the llmkube CLI")
		cmd := exec.Command("make", "build-cli")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to build CLI")

		// Assuming the binary is output to bin/llmkube
		cliPath = "../../bin/llmkube"
	})

	Context("Catalog Commands", func() {
		It("should list all models in the catalog", func() {
			By("running llmkube catalog list")
			cmd := exec.Command(cliPath, "catalog", "list")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to run catalog list")

			// Verify output contains expected models
			Expect(output).To(ContainSubstring("llama-3.1-8b"))
			Expect(output).To(ContainSubstring("mistral-7b"))
			Expect(output).To(ContainSubstring("qwen-2.5-coder-7b"))
			Expect(output).To(ContainSubstring("deepseek-coder-6.7b"))

			// Verify table headers
			Expect(output).To(ContainSubstring("ID"))
			Expect(output).To(ContainSubstring("NAME"))
			Expect(output).To(ContainSubstring("SIZE"))
			Expect(output).To(ContainSubstring("VRAM"))
		})

		It("should filter models by tag", func() {
			By("running llmkube catalog list --tag code")
			cmd := exec.Command(cliPath, "catalog", "list", "--tag", "code")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to run catalog list with tag filter")

			// Should contain code-related models
			Expect(output).To(ContainSubstring("qwen-2.5-coder-7b"))
			Expect(output).To(ContainSubstring("deepseek-coder-6.7b"))

			// Should show filter in output
			Expect(output).To(ContainSubstring("Filter: tag=code"))
		})

		It("should show detailed info for a specific model", func() {
			By("running llmkube catalog info llama-3.1-8b")
			cmd := exec.Command(cliPath, "catalog", "info", "llama-3.1-8b")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to run catalog info")

			// Verify detailed information is present
			Expect(output).To(ContainSubstring("Llama 3.1 8B Instruct"))
			Expect(output).To(ContainSubstring("8B parameters"))
			Expect(output).To(ContainSubstring("Q5_K_M"))
			Expect(output).To(ContainSubstring("Quick Deploy:"))
			Expect(output).To(ContainSubstring("llmkube deploy llama-3.1-8b --gpu"))

			// Verify resource requirements are shown
			Expect(output).To(ContainSubstring("Resource Requirements:"))
			Expect(output).To(ContainSubstring("CPU:"))
			Expect(output).To(ContainSubstring("Memory:"))
		})

		It("should fail gracefully for non-existent model", func() {
			By("running llmkube catalog info non-existent-model")
			cmd := exec.Command(cliPath, "catalog", "info", "non-existent-model")
			_, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "Expected error for non-existent model")
		})
	})

	Context("Deploy with Catalog", func() {
		It("should deploy a catalog model without --source flag", func() {
			Skip("This requires a running Kubernetes cluster with llmkube operator installed")

			By("deploying llama-3.1-8b from catalog")
			cmd := exec.Command(cliPath, "deploy", "llama-3.1-8b",
				"--cpu", "500m",
				"--memory", "1Gi",
				"--wait=false", // Don't wait for readiness in test
			)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy catalog model")

			// Verify catalog model was used
			Expect(output).To(ContainSubstring("Using catalog model"))
			Expect(output).To(ContainSubstring("Llama 3.1 8B Instruct"))
		})

		It("should show helpful error when deploying non-catalog model without --source", func() {
			Skip("This requires a running Kubernetes cluster with llmkube operator installed")

			By("attempting to deploy non-catalog model without source")
			cmd := exec.Command(cliPath, "deploy", "my-custom-model")
			_, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "Expected error for missing source")

			// The error should mention the catalog
			output, _ := cmd.CombinedOutput()
			Expect(string(output)).To(ContainSubstring("not found in catalog"))
			Expect(string(output)).To(ContainSubstring("llmkube catalog list"))
		})
	})

	Context("Catalog Help Documentation", func() {
		It("should show catalog in main help", func() {
			By("running llmkube --help")
			cmd := exec.Command(cliPath, "--help")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Expect(output).To(ContainSubstring("catalog"))
			Expect(output).To(ContainSubstring("Manage and browse the model catalog"))
		})

		It("should show catalog subcommands in help", func() {
			By("running llmkube catalog --help")
			cmd := exec.Command(cliPath, "catalog", "--help")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Expect(output).To(ContainSubstring("list"))
			Expect(output).To(ContainSubstring("info"))
			Expect(output).To(ContainSubstring("battle-tested models"))
		})

		It("should mention catalog in deploy help", func() {
			By("running llmkube deploy --help")
			cmd := exec.Command(cliPath, "deploy", "--help")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Deploy help should mention catalog
			Expect(output).To(ContainSubstring("catalog"))
			// Should show catalog deployment examples
			lowerOutput := strings.ToLower(output)
			Expect(lowerOutput).To(Or(
				ContainSubstring("llmkube catalog list"),
				ContainSubstring("from the catalog"),
			))
		})
	})
})
