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

package e2e

import (
	"fmt"
	"os/exec"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Beem0807/namespace-bootstrap-operator/test/utils"
)

const policyNamespace = "e2e-protected"

// The optional ValidatingAdmissionPolicy turns drift correction into drift
// prevention: an edit to a managed resource is rejected rather than silently
// undone.
//
// The spec that matters most here is the last one. A policy like this has to
// exempt the operator's own ServiceAccount, and getting that name wrong locks the
// operator out of the resources it exists to maintain — an outage traded for
// closing a millisecond window. So it is not enough to prove that a human is
// blocked; the operator must still be able to reconcile with the policy active.
var _ = Describe("The optional admission policy", Ordered, func() {
	// helmUpgrade re-runs the install with the policy toggled, which is how a user
	// would turn it on.
	helmUpgrade := func(enabled string) {
		_, err := utils.Run(exec.Command("helm", "upgrade", "--install", releaseName, chartDir,
			"--namespace", releaseNamespace,
			"--set", "manager.image.repository=namespace-bootstrap-operator",
			"--set", "manager.image.tag=e2e",
			"--set", "manager.image.pullPolicy=Never",
			"--set", "admissionPolicy.enabled="+enabled,
			"--wait", "--timeout", "3m",
		))
		Expect(err).NotTo(HaveOccurred(), "helm upgrade with admissionPolicy.enabled=%s failed", enabled)
	}

	BeforeAll(func() {
		By("provisioning a namespace to protect, with the policy still off")
		out, err := applyStdin(`
apiVersion: platform.nabeem.com/v1alpha1
kind: NamespaceProfile
metadata:
  name: e2e-protected-profile
spec:
  namespace: ` + policyNamespace + `
  quota:
    requests:
      cpu: "2"
    pods: 5
  security:
    podSecurityStandard: baseline
`)
		Expect(err).NotTo(HaveOccurred(), out)

		Eventually(func() (string, error) {
			return kubectl("get", "resourcequota", "nsprofile-quota", "-n", policyNamespace,
				"-o", "jsonpath={.spec.hard.requests\\.cpu}")
		}).Should(Equal("2"))

		By("enabling the policy")
		helmUpgrade("true")

		Eventually(func() (string, error) {
			return kubectl("get", "validatingadmissionpolicy",
				"-o", "jsonpath={.items[*].metadata.name}")
		}).Should(ContainSubstring("managed-resources-protected"))

		Eventually(func() (string, error) {
			return kubectl("get", "ns", policyNamespace,
				"-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`)
		}).Should(Equal("baseline"))

		By("waiting until the policy is actually enforcing, not merely created")
		// The API server compiles and caches admission policies, so enforcement
		// lags the object's creation by a moment. The object existing is not the
		// signal; being refused is. Probing with the value the profile already
		// declares means a probe that slips through changes nothing.
		Eventually(func() error {
			if _, err := kubectl("patch", "resourcequota", "nsprofile-quota",
				"-n", policyNamespace, "--type=merge",
				"-p", `{"spec":{"hard":{"requests.cpu":"2"}}}`); err == nil {
				return fmt.Errorf("the policy is not rejecting writes yet")
			}
			return nil
		}).Should(Succeed(), "the policy never began enforcing")
	})

	AfterAll(func() {
		By("turning the policy back off, so it cannot affect later runs of this suite")
		helmUpgrade("false")
		_, _ = kubectl("delete", "nsp", "e2e-protected-profile", "--wait=true", "--timeout=2m")
	})

	It("rejects an edit to a managed resource, naming the profile to change instead", func() {
		out, err := kubectl("patch", "resourcequota", "nsprofile-quota", "-n", policyNamespace,
			"--type=merge", "-p", `{"spec":{"hard":{"requests.cpu":"64"}}}`)
		Expect(err).To(HaveOccurred(), "the edit should have been rejected outright")

		// The error message is the real payoff: without it, an engineer's edit
		// silently reverts and they simply try again, puzzled.
		Expect(out).To(ContainSubstring("managed by the Namespace Bootstrap Operator"))
		Expect(out).To(ContainSubstring("e2e-protected-profile"))

		By("leaving the value untouched, since the write never happened")
		cpu, err := kubectl("get", "resourcequota", "nsprofile-quota", "-n", policyNamespace,
			"-o", "jsonpath={.spec.hard.requests\\.cpu}")
		Expect(err).NotTo(HaveOccurred())
		Expect(cpu).To(Equal("2"))
	})

	It("rejects deleting a managed resource", func() {
		out, err := kubectl("delete", "resourcequota", "nsprofile-quota", "-n", policyNamespace)
		Expect(err).To(HaveOccurred(), "deleting a managed resource should have been rejected")
		Expect(out).To(ContainSubstring("managed by the Namespace Bootstrap Operator"))
	})

	It("leaves objects it does not manage freely editable", func() {
		// The property RBAC cannot express: protect the operator's objects while
		// a team's own resources in the same namespace stay editable.
		out, err := applyStdin(`
apiVersion: v1
kind: ResourceQuota
metadata:
  name: team-written-quota
  namespace: ` + policyNamespace + `
spec:
  hard:
    configmaps: "10"
`)
		Expect(err).NotTo(HaveOccurred(), out)

		out, err = kubectl("patch", "resourcequota", "team-written-quota", "-n", policyNamespace,
			"--type=merge", "-p", `{"spec":{"hard":{"configmaps":"20"}}}`)
		Expect(err).NotTo(HaveOccurred(), out)

		value, err := kubectl("get", "resourcequota", "team-written-quota", "-n", policyNamespace,
			"-o", "jsonpath={.spec.hard.configmaps}")
		Expect(err).NotTo(HaveOccurred())
		Expect(value).To(Equal("20"))

		_, _ = kubectl("delete", "resourcequota", "team-written-quota", "-n", policyNamespace)
	})

	It("rejects stripping a Pod Security label from a managed namespace", func() {
		out, err := kubectl("label", "ns", policyNamespace,
			"pod-security.kubernetes.io/enforce-", "--overwrite")
		Expect(err).To(HaveOccurred(), "removing the enforcement label should have been rejected")
		Expect(out).To(ContainSubstring("managed by the Namespace Bootstrap Operator"))
		Expect(out).To(ContainSubstring("e2e-protected-profile"))

		By("leaving the label in place, since the write never happened")
		value, err := kubectl("get", "ns", policyNamespace,
			"-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(value).To(Equal("baseline"))
	})

	It("rejects weakening a Pod Security label rather than only its removal", func() {
		out, err := kubectl("label", "ns", policyNamespace,
			"pod-security.kubernetes.io/enforce=privileged", "--overwrite")
		Expect(err).To(HaveOccurred(), "downgrading the enforcement level should have been rejected")
		Expect(out).To(ContainSubstring("managed by the Namespace Bootstrap Operator"))
	})

	It("still allows every other label on a managed namespace", func() {
		// The whole reason this is a field-comparison policy rather than
		// "only the operator may touch this namespace": a service mesh label, a
		// cost-allocation tag and kubectl annotate all have to keep working.
		out, err := kubectl("label", "ns", policyNamespace,
			"istio-injection=enabled", "cost-center=platform-eng", "--overwrite")
		Expect(err).NotTo(HaveOccurred(), out)

		out, err = kubectl("annotate", "ns", policyNamespace,
			"team=payments", "--overwrite")
		Expect(err).NotTo(HaveOccurred(), out)

		labels, err := kubectl("get", "ns", policyNamespace, "-o", "jsonpath={.metadata.labels}")
		Expect(err).NotTo(HaveOccurred())
		Expect(labels).To(ContainSubstring(`"istio-injection":"enabled"`))
		Expect(labels).To(ContainSubstring(`"cost-center":"platform-eng"`))
	})

	It("does not lock the operator out of its own resources", func() {
		// The spec this whole feature lives or dies by. If the policy's exemption
		// named the wrong ServiceAccount, the operator could no longer write the
		// resources it maintains, and every reconciliation would fail — a far worse
		// outcome than the drift window the policy closes.
		By("changing the profile, which the operator must apply through the policy")
		_, err := kubectl("patch", "nsp", "e2e-protected-profile", "--type=merge",
			"-p", `{"spec":{"quota":{"requests":{"cpu":"7"},"pods":5}}}`)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() (string, error) {
			return kubectl("get", "resourcequota", "nsprofile-quota", "-n", policyNamespace,
				"-o", "jsonpath={.spec.hard.requests\\.cpu}")
		}).Should(Equal("7"), "the operator must still be able to update its own resources")

		By("changing the Pod Security level, which only the operator may do")
		_, err = kubectl("patch", "nsp", "e2e-protected-profile", "--type=merge",
			"-p", `{"spec":{"security":{"podSecurityStandard":"restricted"}}}`)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() (string, error) {
			return kubectl("get", "ns", policyNamespace,
				"-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`)
		}).Should(Equal("restricted"),
			"the operator must still be able to write the labels the policy freezes")

		By("staying Ready rather than wedging on denied writes")
		Eventually(func() (string, error) {
			return kubectl("get", "nsp", "e2e-protected-profile",
				"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
		}).Should(Equal("True"))

		By("reporting no admission denials in the manager log")
		logs, err := kubectl("logs", "-n", releaseNamespace,
			"-l", "control-plane=controller-manager", "--tail=300")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).NotTo(ContainSubstring("denied the request"))
	})
})
