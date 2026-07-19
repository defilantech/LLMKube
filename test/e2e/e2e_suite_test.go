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
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/defilantech/llmkube/test/utils"
)

var (
	// Optional Environment Variables:
	// - CERT_MANAGER_INSTALL_SKIP=true: Skips CertManager installation during test setup.
	// - LLMKUBE_E2E_ROUTER_CLUSTER=true: Runs the "ModelRouter Cluster" Context
	//   (test/e2e/e2e_test.go), which side-loads the router-proxy and
	//   stub-upstream images built below.
	// - LLMKUBE_E2E_PYRRA=true: Runs the "Pyrra SLO Integration" Context
	//   (test/e2e/slo_e2e_test.go), which side-loads the Pyrra CRD/operator
	//   plus the prometheus-operator PrometheusRule CRD and does a second
	//   controller-manager deploy with --enable-pyrra-slo. Needs no image
	//   build/load here (unlike LLMKUBE_E2E_ROUTER_CLUSTER): Pyrra's
	//   kubernetes operator image is pulled directly from ghcr.io.
	// These variables are useful if CertManager is already installed, avoiding
	// re-installation and conflicts.
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true"
	// isCertManagerAlreadyInstalled will be set true when CertManager CRDs be found on the cluster
	isCertManagerAlreadyInstalled = false

	// projectImage is the name of the image which will be build and loaded
	// with the code source changes to be tested.
	projectImage = "example.com/llmkube:v0.0.1"

	// routerProxyImage matches the default image the operator's
	// ModelRouterReconciler hard-codes (internal/controller/router_deployment_builder.go).
	// Side-loading exactly this tag means the controller can dispatch
	// new ModelRouter resources without a per-deploy --router-proxy-image
	// flag rewrite. Override with the LLMKUBE_E2E_ROUTER_PROXY_IMG env
	// var when running locally against a different registry/tag.
	routerProxyImage = "ghcr.io/defilantech/llmkube-router-proxy:dev"

	// stubUpstreamImage is the fake llama.cpp / cloud-provider used in
	// the ModelRouter cluster Context. Only side-loaded when
	// LLMKUBE_E2E_ROUTER_CLUSTER=true so unrelated jobs stay fast.
	stubUpstreamImage = "localhost/llmkube-stub-upstream:e2e"
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purpose of being used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting llmkube integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	// The MicroShift / OpenShift CI path builds and pushes the controller
	// image to a runner-local registry, then installs LLMKube via Helm
	// before this suite runs. Skip the kind-specific docker-build and
	// LoadImageToKindClusterWithName steps in that case; the workflow
	// already has the cluster in the desired state.
	onOpenShift := os.Getenv("LLMKUBE_E2E_OPENSHIFT") == "true"

	if !onOpenShift {
		By("building the manager(Operator) image")
		cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
		_, err := utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

		// TODO(user): If you want to change the e2e test vendor from Kind, ensure the image is
		// built and available before running the tests. Also, remove the following block.
		By("loading the manager(Operator) image on Kind")
		err = utils.LoadImageToKindClusterWithName(projectImage)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into Kind")

		if os.Getenv("LLMKUBE_E2E_ROUTER_CLUSTER") == "true" {
			By("building the router-proxy image")
			cmd = exec.Command("make", "docker-build-router-proxy",
				fmt.Sprintf("ROUTER_PROXY_IMG=%s", routerProxyImage))
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(),
				"Failed to build the router-proxy image")

			By("loading the router-proxy image on Kind")
			err = utils.LoadImageToKindClusterWithName(routerProxyImage)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(),
				"Failed to load the router-proxy image into Kind")

			By("building the stub-upstream image")
			cmd = exec.Command("make", "docker-build-stub-upstream",
				fmt.Sprintf("STUB_UPSTREAM_IMG=%s", stubUpstreamImage))
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(),
				"Failed to build the stub-upstream image")

			By("loading the stub-upstream image on Kind")
			err = utils.LoadImageToKindClusterWithName(stubUpstreamImage)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(),
				"Failed to load the stub-upstream image into Kind")
		}
	}

	// The tests-e2e are intended to run on a temporary cluster that is created and destroyed for testing.
	// To prevent errors when tests run in environments with CertManager already installed,
	// we check for its presence before execution.
	// Setup CertManager before the suite if not skipped and if not already installed
	if !skipCertManagerInstall {
		By("checking if cert manager is installed already")
		isCertManagerAlreadyInstalled = utils.IsCertManagerCRDsInstalled()
		if !isCertManagerAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing CertManager...\n")
			Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CertManager is already installed. Skipping installation...\n")
		}
	}
})

var _ = AfterSuite(func() {
	// Teardown CertManager after the suite if not skipped and if it was not already installed
	if !skipCertManagerInstall && !isCertManagerAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling CertManager...\n")
		utils.UninstallCertManager()
	}
})
