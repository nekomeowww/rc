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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nekomeowww/rc/test/utils"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "example.com/rc:v0.0.1"
	// runnerImage is the minimal Workspace runtime image built from
	// test/e2e/Dockerfile.runner. Its tag is not "latest", so Kind uses the
	// loaded image instead of pulling.
	runnerImage = "example.com/rc-runner:v0.0.1"
	// rcKubeImage is the rc-kube image that the manager injects into the
	// runtime Pods. The runner image has no rc-kube or tini, so every runtime
	// spec also checks the injection.
	rcKubeImage = "example.com/rc-kube:v0.0.1"
	// shouldCleanupCertManager tracks whether CertManager was installed by this suite.
	shouldCleanupCertManager = false
)

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind and CertManager.
//
// To enable kubectl kuberc (use custom kubectl configurations), set: KUBECTL_KUBERC=true
// By default, kuberc is disabled to ensure consistent test behavior across different environments.
// To skip CertManager installation, set: CERT_MANAGER_INSTALL_SKIP=true
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting rc e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the manager and rc-kube images")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage), fmt.Sprintf("RC_KUBE_IMG=%s", rcKubeImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	// TODO(user): If you want to change the e2e test vendor from Kind,
	// ensure the image is built and available, then remove the following block.
	By("loading the manager and rc-kube images on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")
	err = utils.LoadImageToKindClusterWithName(rcKubeImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the rc-kube image into Kind")

	By("building the e2e runner image")
	cmd = exec.Command("make", "docker-build-e2e-runner", fmt.Sprintf("E2E_RUNNER_IMG=%s", runnerImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the e2e runner image")

	By("loading the e2e runner image on Kind")
	err = utils.LoadImageToKindClusterWithName(runnerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the e2e runner image into Kind")

	configureKubectlKubeRC()
	setupCertManager()
	deployManager()
})

var _ = AfterSuite(func() {
	undeployManager()
	teardownCertManager()
})

// deployManager installs the CRDs and deploys the manager with the e2e runner
// image into a namespace that enforces the restricted Pod Security Standard.
// Every spec container shares this deployment, so it lives at suite level.
func deployManager() {
	By("creating manager namespace")
	ExpectWithOffset(1, utils.CreateNamespace(namespace)).To(Succeed(), "Failed to create namespace")

	By("labeling the namespace to enforce the restricted security policy")
	cmd := exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
		"pod-security.kubernetes.io/enforce=restricted")
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

	By("installing CRDs")
	cmd = exec.Command("make", "install")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to install CRDs")

	By("deploying the controller-manager")
	err = utils.DeployManager(managerImage, runnerImage, rcKubeImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

	By("waiting for the controller-manager rollout")
	cmd = exec.Command("kubectl", "rollout", "status", "deployment/rc-controller-manager",
		"-n", namespace,
		"--timeout=3m",
	)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Controller-manager did not become available")
}

// undeployManager removes what deployManager created. Specs delete their own
// Workspaces first, because the manager must run to release their finalizers.
func undeployManager() {
	By("undeploying the controller-manager")
	cmd := exec.Command("make", "undeploy", "ignore-not-found=true")
	_, _ = utils.Run(cmd)

	By("uninstalling CRDs")
	cmd = exec.Command("make", "uninstall", "ignore-not-found=true")
	_, _ = utils.Run(cmd)

	By("removing manager namespace")
	cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)
}

// Disable kubectl kuberc by default for test isolation.
// This prevents local kubectl configurations from affecting test behavior.
// To enable kuberc, set: KUBECTL_KUBERC=true
func configureKubectlKubeRC() {
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		By("disabling kubectl kuberc for test isolation")
		err := os.Setenv("KUBECTL_KUBERC", "false")
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to disable kubectl kuberc")
		_, _ = fmt.Fprintf(GinkgoWriter,
			"kubectl kuberc disabled for consistent test behavior (override with KUBECTL_KUBERC=true)\n")
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl kuberc enabled (KUBECTL_KUBERC=true)\n")
	}
}

// setupCertManager installs CertManager if needed for webhook tests.
// Skips installation if CERT_MANAGER_INSTALL_SKIP=true or if already present.
func setupCertManager() {
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager installation (CERT_MANAGER_INSTALL_SKIP=true)\n")
		return
	}

	By("checking if CertManager is already installed")
	if utils.IsCertManagerCRDsInstalled() {
		_, _ = fmt.Fprintf(GinkgoWriter, "CertManager is already installed. Skipping installation.\n")
		return
	}

	// Mark for cleanup before installation to handle interruptions and partial installs.
	shouldCleanupCertManager = true

	By("installing CertManager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
}

// teardownCertManager uninstalls CertManager if it was installed by setupCertManager.
// This ensures we only remove what we installed.
func teardownCertManager() {
	if !shouldCleanupCertManager {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager cleanup (not installed by this suite)\n")
		return
	}

	By("uninstalling CertManager")
	utils.UninstallCertManager()
}
