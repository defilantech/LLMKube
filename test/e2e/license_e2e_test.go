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

})

