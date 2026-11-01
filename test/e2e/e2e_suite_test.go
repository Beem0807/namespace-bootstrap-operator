//go:build e2e

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

// Package e2e runs the operator on a real Kubernetes cluster, installed the way
// a user would install it.
//
// It exists to test the things envtest structurally cannot:
//
//   - RBAC. The envtest suites drive the reconciler with a privileged client,
//     so they prove the logic and say nothing about whether the operator's own
//     ServiceAccount is allowed to do any of it. Here the operator runs
//     in-cluster under exactly the Role the markers generated, so a missing
//     verb is a failure rather than an oversight.
//   - The Helm chart. Running `helm install` is the only way to find out that
//     the chart's CRD and RBAC actually match the code — a mismatch there is
//     invisible to every other test in this repo.
//   - Real namespace deletion. envtest runs no namespace controller, so a
//     deleted namespace stays Terminating forever; only here does it finish.
//   - Real pods. Admission plugins can be exercised in envtest, but a pod that
//     is actually scheduled and runs is what proves a LimitRange's injected
//     defaults and a ResourceQuota's usage accounting.
//
// NetworkPolicy is the deliberate exception. These specs assert the objects'
// *shape* and explicitly do not claim enforcement: whether traffic is blocked
// depends entirely on the cluster's CNI, and kind's default CNI historically
// does not implement NetworkPolicy. A spec that passed on an unenforcing
// cluster while implying traffic was blocked would be worse than no spec.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Beem0807/namespace-bootstrap-operator/test/utils"
)

const (
	// releaseName and releaseNamespace are where the chart installs the
	// operator.
	releaseName      = "nbo"
	releaseNamespace = "nbo-system"

	// managerImage is built locally and side-loaded into the kind node, so the
	// suite never depends on a registry.
	managerImage = "namespace-bootstrap-operator:e2e"

	// chartDir is the chart generated from config/, which is what users install.
	chartDir = "dist/chart"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)
	RunSpecs(t, "End-to-end Suite")
}

var _ = BeforeSuite(func() {
	By("building the manager image")
	_, err := utils.Run(exec.Command("make", "docker-build", "IMG="+managerImage))
	Expect(err).NotTo(HaveOccurred(), "could not build the manager image")

	By("loading the image into the kind node")
	Expect(utils.LoadImageToKindClusterWithName(managerImage)).To(Succeed())

	By("installing the operator with Helm, exactly as a user would")
	// Installing the chart rather than applying kustomize output on purpose:
	// Helm is the documented deploy path, so this is the only test that can
	// catch a chart whose CRD or RBAC has drifted from the code.
	_, err = utils.Run(exec.Command("helm", "upgrade", "--install", releaseName, chartDir,
		"--namespace", releaseNamespace,
		"--create-namespace",
		// These keys must match dist/chart/values.yaml exactly. A typo here is
		// silent: Helm accepts unknown --set paths, the chart falls back to its
		// default image, and the install fails on an ImagePullBackOff that
		// looks nothing like a wrong value path.
		"--set", "manager.image.repository=namespace-bootstrap-operator",
		"--set", "manager.image.tag=e2e",
		"--set", "manager.image.pullPolicy=Never",
		"--wait", "--timeout", "3m",
	))
	Expect(err).NotTo(HaveOccurred(), "helm install failed")

	By("waiting for the manager to report ready")
	Eventually(func() (string, error) {
		return kubectl("get", "deploy", "-n", releaseNamespace,
			"-l", "control-plane=controller-manager",
			"-o", "jsonpath={.items[0].status.readyReplicas}")
	}).Should(Equal("1"), "the manager never became ready")
})

var _ = AfterSuite(func() {
	By("collecting manager logs, which are the only record of an RBAC failure")
	if logs, err := kubectl("logs", "-n", releaseNamespace,
		"-l", "control-plane=controller-manager", "--tail=200"); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "\n--- manager logs ---\n%s\n", logs)
	}

	By("uninstalling the release")
	_, _ = utils.Run(exec.Command("helm", "uninstall", releaseName, "--namespace", releaseNamespace))
})

// kubectl runs kubectl and returns its trimmed output.
func kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	out, err := utils.Run(cmd)
	return out, err
}

// applyStdin pipes a manifest into kubectl apply, so specs can declare objects
// inline rather than through temporary files.
func applyStdin(manifest string) (string, error) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = stringReader(manifest)
	cmd.Dir = projectDir()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// createStdin is applyStdin for objects that must be *rejected*, where apply's
// server-side merge would mask an admission error.
func createStdin(manifest string) (string, error) {
	cmd := exec.Command("kubectl", "create", "-f", "-")
	cmd.Stdin = stringReader(manifest)
	cmd.Dir = projectDir()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func stringReader(s string) *os.File {
	read, write, err := os.Pipe()
	Expect(err).NotTo(HaveOccurred())
	go func() {
		defer func() { _ = write.Close() }()
		_, _ = write.WriteString(s)
	}()
	return read
}

func projectDir() string {
	dir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())
	return dir
}
