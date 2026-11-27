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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ushitora-anqou/koujouo/test/utils"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "example.com/koujouo:v0.0.1"
	// oldManagerImage is the image of a previous build: both binaries carry
	// the version "e2e-old" (dockerImageE2EOld of flake.nix) instead of the
	// git revision. The suite installs it first, so that the manager
	// upgraded to managerImage finds an agent of another build and replaces
	// its binary in place, with the game running.
	oldManagerImage = "example.com/koujouo:e2e-old"
	// shouldCleanupCertManager tracks whether CertManager was installed by this suite.
	shouldCleanupCertManager = false
)

// The suite deploys the operator with the chart in charts/koujouo, the only
// supported way of deploying it: `make install` and `make deploy` no longer
// exist.
const (
	// helmReleaseName is the name of the Helm release the suite installs.
	helmReleaseName = "koujouo"
	// chartPath is the chart holding the CRD, the RBAC and the manager Deployment.
	chartPath = "charts/koujouo"
	// crdName is the CRD of the chart. It carries `helm.sh/resource-policy: keep`,
	// so `helm uninstall` leaves it behind and the suite deletes it itself.
	crdName = "factorioservers.koujouo.anqou.net"
)

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind and CertManager.
//
// To enable kubectl kuberc (use custom kubectl configurations), set: KUBECTL_KUBERC=true
// By default, kuberc is disabled to ensure consistent test behavior across different environments.
// To skip CertManager installation, set: CERT_MANAGER_INSTALL_SKIP=true
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting koujouo e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

// The suite deploys the controller once, so that every container of the
// suite (the manager checks and the FactorioServer scenarios) runs
// against the same operator.
var _ = BeforeSuite(func() {
	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	// The upgrade scenario needs a manager and an agent of another build
	// than the one under test: the same source with the version "e2e-old"
	// baked in. The suite installs that image first and upgrades the
	// release to managerImage later, which makes the new manager replace
	// the binary of the running agent.
	By("building the old manager image")
	cmd = exec.Command("make", "docker-build-e2e-old", fmt.Sprintf("IMG=%s", oldManagerImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the old manager image")

	// TODO(user): If you want to change the e2e test vendor from Kind,
	// ensure the image is built and available, then remove the following block.
	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	By("loading the old manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(oldManagerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the old manager image into Kind")

	configureKubectlKubeRC()
	setupCertManager()

	// The chart creates no Namespace, so the suite creates the one of the
	// release and enforces the restricted Pod Security Standards in it. Both
	// steps tolerate a namespace a failed run left behind.
	By("creating manager namespace")
	ExpectWithOffset(1, createNamespace(namespace)).To(Succeed(), "Failed to create namespace")

	By("labeling the namespace to enforce the restricted security policy")
	cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
		"pod-security.kubernetes.io/enforce=restricted")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

	// The CRD carries `helm.sh/resource-policy: keep`, so the uninstall of a
	// failed run leaves it behind. Helm refuses to install the chart over a
	// CRD it does not own, so delete the leftover before the install.
	By("deleting the CRD a previous run left behind")
	cmd = exec.Command("kubectl", "delete", "crd", crdName, "--ignore-not-found")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to delete the leftover CRD")

	// The release starts with the image of the previous build: the agent
	// of the first FactorioServer then runs the old binary, and the
	// upgrade scenario upgrades the release to managerImage.
	By("deploying the controller-manager with Helm")
	repository, tag, err := splitManagerImage(oldManagerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to split the old manager image")

	cmd = exec.Command("helm", "upgrade", "--install", helmReleaseName, chartPath,
		"--namespace", namespace,
		"--set", "image.repository="+repository,
		"--set", "image.tag="+tag,
	)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager with Helm")

	By("waiting for the controller-manager deployment to be available")
	cmd = exec.Command("kubectl", "wait", "deployment/"+managerDeploymentName,
		"-n", namespace, "--for", "condition=Available", "--timeout=5m")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to wait for the controller-manager deployment")
})

var _ = AfterSuite(func() {
	By("uninstalling the Helm release")
	cmd := exec.Command("helm", "uninstall", helmReleaseName, "--namespace", namespace)
	output, err := utils.Run(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to uninstall the Helm release: %v: %s\n", err, output)
	}

	// `helm uninstall` keeps the CRD: it carries helm.sh/resource-policy: keep,
	// so that a user does not lose the FactorioServers with the release.
	By("deleting the CRD")
	cmd = exec.Command("kubectl", "delete", "crd", crdName, "--ignore-not-found")
	_, _ = utils.Run(cmd)

	By("removing manager namespace")
	cmd = exec.Command("kubectl", "delete", "ns", namespace, "--wait=true", "--timeout=5m")
	_, _ = utils.Run(cmd)

	teardownCertManager()
})

// createNamespace creates the namespace name unless it exists, so that a
// suite that runs again on the cluster of a failed run does not stop here.
func createNamespace(name string) error {
	_, err := utils.Kubectl(context.Background(), "get", "namespace", name)
	if err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Namespace %q already exists, reusing it\n", name)

		return nil
	}

	_, err = utils.Kubectl(context.Background(), "create", "namespace", name)

	return err
}

// errNoImageTag reports an image reference of the form <repository> without
// the tag the chart needs.
var errNoImageTag = errors.New("the image reference has no tag")

// splitManagerImage splits an image reference of the form <repository>:<tag>
// into its parts. The colon of a registry port belongs to the repository, so
// only a colon after the last slash starts the tag.
func splitManagerImage(image string) (string, string, error) {
	colon := strings.LastIndex(image, ":")
	if colon < 0 || colon < strings.LastIndex(image, "/") {
		return "", "", fmt.Errorf("%w: %q", errNoImageTag, image)
	}

	repository, tag := image[:colon], image[colon+1:]
	if repository == "" || tag == "" {
		return "", "", fmt.Errorf("%w: %q", errNoImageTag, image)
	}

	return repository, tag, nil
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
