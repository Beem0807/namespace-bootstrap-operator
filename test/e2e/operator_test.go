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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const tenantNamespace = "e2e-payments"

// profileManifest is a realistic production profile: quota, container limits,
// a fenced network, and a restricted Pod Security level.
const profileManifest = `
apiVersion: platform.nabeem.com/v1alpha1
kind: NamespaceProfile
metadata:
  name: e2e-payments-profile
spec:
  namespace: ` + tenantNamespace + `
  quota:
    requests:
      cpu: "2"
      memory: 2Gi
    limits:
      cpu: "4"
      memory: 4Gi
    pods: 3
    objectCounts:
      services: 2
      configmaps: 20
  limitRange:
    defaultRequest:
      cpu: 50m
      memory: 64Mi
    default:
      cpu: 100m
      memory: 128Mi
    min:
      cpu: 10m
      memory: 16Mi
    max:
      cpu: "1"
      memory: 1Gi
  network:
    defaultDenyIngress: true
    defaultDenyEgress: false
    allowDNS: true
    allowSameNamespace: true
  security:
    podSecurityStandard: restricted
  lifecycle:
    deleteNamespaceOnRemoval: false
`

var _ = Describe("The operator on a real cluster", Ordered, func() {
	Describe("provisioning", func() {
		It("provisions the namespace and everything the profile declares", func() {
			// The first and most important assertion of this whole suite: the
			// operator does this under its own ServiceAccount, with only the
			// Role its RBAC markers generated. A missing verb fails here.
			out, err := applyStdin(profileManifest)
			Expect(err).NotTo(HaveOccurred(), out)

			By("creating the namespace")
			Eventually(func() (string, error) {
				return kubectl("get", "ns", tenantNamespace, "-o", "jsonpath={.status.phase}")
			}).Should(Equal("Active"))

			By("reaching Ready, which proves every API call it needed was permitted")
			Eventually(func() (string, error) {
				return kubectl("get", "nsp", "e2e-payments-profile",
					"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
			}).Should(Equal("True"))

			Eventually(func() (string, error) {
				return kubectl("get", "nsp", "e2e-payments-profile", "-o", "jsonpath={.status.phase}")
			}).Should(Equal("Ready"))
		})

		It("has no RBAC failures in the manager log", func() {
			// Belt and braces alongside Ready=True: a Forbidden on a path the
			// specs above do not reach would otherwise go unnoticed.
			logs, err := kubectl("logs", "-n", releaseNamespace,
				"-l", "control-plane=controller-manager", "--tail=500")
			Expect(err).NotTo(HaveOccurred())
			Expect(logs).NotTo(ContainSubstring("is forbidden"))
			Expect(logs).NotTo(ContainSubstring("cannot list resource"))
		})

		It("created the ResourceQuota with the declared hard limits", func() {
			hard, err := kubectl("get", "resourcequota", "nsprofile-quota",
				"-n", tenantNamespace, "-o", "jsonpath={.spec.hard}")
			Expect(err).NotTo(HaveOccurred())
			Expect(hard).To(ContainSubstring(`"requests.cpu":"2"`))
			Expect(hard).To(ContainSubstring(`"pods":"3"`))
			Expect(hard).To(ContainSubstring(`"services":"2"`))
		})

		It("recorded which quota keys it owns", func() {
			keys, err := kubectl("get", "resourcequota", "nsprofile-quota",
				"-n", tenantNamespace,
				"-o", `jsonpath={.metadata.annotations.platform\.nabeem\.com/managed-quota-keys}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(keys).To(ContainSubstring("requests.cpu"))
			Expect(keys).To(ContainSubstring("services"))
		})

		It("applied the Pod Security Admission labels", func() {
			labels, err := kubectl("get", "ns", tenantNamespace, "-o", "jsonpath={.metadata.labels}")
			Expect(err).NotTo(HaveOccurred())
			Expect(labels).To(ContainSubstring(`"pod-security.kubernetes.io/enforce":"restricted"`))
			Expect(labels).To(ContainSubstring(`"app.kubernetes.io/managed-by":"namespace-bootstrap-operator"`))
		})

		It("generated the NetworkPolicies the profile asks for", func() {
			// Shape only. See the package comment: enforcement depends on the
			// CNI, and this suite does not claim it.
			names, err := kubectl("get", "netpol", "-n", tenantNamespace,
				"-o", "jsonpath={.items[*].metadata.name}")
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(ContainSubstring("nsprofile-default-deny-ingress"))
			Expect(names).To(ContainSubstring("nsprofile-allow-same-namespace"))
			Expect(names).NotTo(ContainSubstring("nsprofile-default-deny-egress"),
				"the profile does not deny egress, so no egress policy should exist")
		})
	})

	Describe("the policies actually taking effect", func() {
		It("injects the LimitRange defaults into a pod that declares none", func() {
			// Only a real cluster proves this: the pod has to be admitted,
			// defaulted, scheduled and run.
			out, err := applyStdin(restrictedPod("defaults-probe", ""))
			Expect(err).NotTo(HaveOccurred(), out)

			Eventually(func() (string, error) {
				return kubectl("get", "pod", "defaults-probe", "-n", tenantNamespace,
					"-o", "jsonpath={.status.phase}")
			}).Should(BeElementOf("Running", "Succeeded"))

			requests, err := kubectl("get", "pod", "defaults-probe", "-n", tenantNamespace,
				"-o", "jsonpath={.spec.containers[0].resources.requests}")
			Expect(err).NotTo(HaveOccurred())
			Expect(requests).To(ContainSubstring(`"cpu":"50m"`))
			Expect(requests).To(ContainSubstring(`"memory":"64Mi"`))

			limits, err := kubectl("get", "pod", "defaults-probe", "-n", tenantNamespace,
				"-o", "jsonpath={.spec.containers[0].resources.limits}")
			Expect(err).NotTo(HaveOccurred())
			Expect(limits).To(ContainSubstring(`"cpu":"100m"`))
		})

		It("rejects a pod that violates the restricted Pod Security level", func() {
			// Enforced by the API server's admission plugin, reading the labels
			// the operator wrote. The operator is not in this path at all.
			out, err := createStdin(privilegedPod("psa-violator"))
			Expect(err).To(HaveOccurred(), "a privileged pod should have been rejected")
			Expect(out).To(ContainSubstring("violates PodSecurity"))
			Expect(out).To(ContainSubstring("restricted"))
		})

		It("rejects a container that exceeds the LimitRange maximum", func() {
			out, err := createStdin(restrictedPod("too-big", "4"))
			Expect(err).To(HaveOccurred(), "a container above max should have been rejected")
			Expect(out).To(ContainSubstring("maximum cpu usage per Container"))
		})

		It("counts real usage against the quota and refuses to exceed it", func() {
			// The quota allows 3 pods and one already exists.
			for i := range 2 {
				out, err := applyStdin(restrictedPod(fmt.Sprintf("filler-%d", i), ""))
				Expect(err).NotTo(HaveOccurred(), out)
			}

			Eventually(func() (string, error) {
				return kubectl("get", "resourcequota", "nsprofile-quota", "-n", tenantNamespace,
					"-o", "jsonpath={.status.used.pods}")
			}).Should(Equal("3"))

			out, err := createStdin(restrictedPod("over-quota", ""))
			Expect(err).To(HaveOccurred(), "the fourth pod should have been rejected")
			Expect(out).To(ContainSubstring("exceeded quota"))
		})
	})

	Describe("drift correction", func() {
		It("restores a hand-edited quota within seconds", func() {
			_, err := kubectl("patch", "resourcequota", "nsprofile-quota", "-n", tenantNamespace,
				"--type=merge", "-p", `{"spec":{"hard":{"requests.cpu":"64"}}}`)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() (string, error) {
				return kubectl("get", "resourcequota", "nsprofile-quota", "-n", tenantNamespace,
					"-o", "jsonpath={.spec.hard.requests\\.cpu}")
			}).Should(Equal("2"), "the operator should have corrected the edit")

			By("recording the correction on the profile")
			Eventually(func() (string, error) {
				return kubectl("get", "nsp", "e2e-payments-profile",
					"-o", "jsonpath={.status.lastDriftCorrectionTime}")
			}).ShouldNot(BeEmpty())
		})

		It("recreates a deleted NetworkPolicy", func() {
			_, err := kubectl("delete", "netpol", "nsprofile-default-deny-ingress", "-n", tenantNamespace)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() (string, error) {
				return kubectl("get", "netpol", "nsprofile-default-deny-ingress",
					"-n", tenantNamespace, "-o", "jsonpath={.metadata.name}")
			}).Should(Equal("nsprofile-default-deny-ingress"))
		})

		It("restores a stripped Pod Security label without touching foreign labels", func() {
			_, err := kubectl("label", "ns", tenantNamespace,
				"pod-security.kubernetes.io/enforce-", "e2e-owner=platform", "--overwrite")
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() (string, error) {
				return kubectl("get", "ns", tenantNamespace,
					"-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`)
			}).Should(Equal("restricted"))

			foreign, err := kubectl("get", "ns", tenantNamespace,
				"-o", "jsonpath={.metadata.labels.e2e-owner}")
			Expect(err).NotTo(HaveOccurred())
			Expect(foreign).To(Equal("platform"), "a label the operator does not own must survive")
		})
	})

	Describe("responding to a profile change", func() {
		It("applies an edited profile and prunes what it no longer declares", func() {
			_, err := kubectl("patch", "nsp", "e2e-payments-profile", "--type=merge",
				"-p", `{"spec":{"network":{"defaultDenyIngress":false,"allowSameNamespace":false}}}`)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() (string, error) {
				return kubectl("get", "netpol", "-n", tenantNamespace,
					"-o", "jsonpath={.items[*].metadata.name}")
			}).Should(BeEmpty(), "withdrawing the toggles should remove the policies")

			By("leaving the quota, which is still declared, in place")
			_, err = kubectl("get", "resourcequota", "nsprofile-quota", "-n", tenantNamespace)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("deletion", func() {
		It("removes the generated resources and keeps the namespace and its pods", func() {
			// The property that matters most: deleting a profile must never
			// take workloads with it.
			_, err := kubectl("delete", "nsp", "e2e-payments-profile", "--wait=true", "--timeout=2m")
			Expect(err).NotTo(HaveOccurred(), "the finalizer should have completed and released the profile")

			By("removing the quota and limit range")
			Eventually(func() bool {
				_, err := kubectl("get", "resourcequota", "nsprofile-quota", "-n", tenantNamespace)
				return err != nil
			}).Should(BeTrue())
			Eventually(func() bool {
				_, err := kubectl("get", "limitrange", "nsprofile-limits", "-n", tenantNamespace)
				return err != nil
			}).Should(BeTrue())

			By("retaining the namespace")
			phase, err := kubectl("get", "ns", tenantNamespace, "-o", "jsonpath={.status.phase}")
			Expect(err).NotTo(HaveOccurred())
			Expect(phase).To(Equal("Active"))

			By("retaining the pods inside it")
			pods, err := kubectl("get", "pods", "-n", tenantNamespace, "-o", "jsonpath={.items[*].metadata.name}")
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(ContainSubstring("defaults-probe"))
		})

		It("deletes the namespace only when deletion is confirmed", func() {
			// A real cluster is the only place this completes: envtest runs no
			// namespace controller, so a deleted namespace never goes away.
			const throwaway = "e2e-disposable"
			manifest := fmt.Sprintf(`
apiVersion: platform.nabeem.com/v1alpha1
kind: NamespaceProfile
metadata:
  name: e2e-disposable-profile
  annotations:
    platform.nabeem.com/confirm-namespace-deletion: %s
spec:
  namespace: %s
  lifecycle:
    deleteNamespaceOnRemoval: true
`, throwaway, throwaway)

			out, err := applyStdin(manifest)
			Expect(err).NotTo(HaveOccurred(), out)

			Eventually(func() (string, error) {
				return kubectl("get", "ns", throwaway, "-o", "jsonpath={.status.phase}")
			}).Should(Equal("Active"))

			_, err = kubectl("delete", "nsp", "e2e-disposable-profile", "--wait=true", "--timeout=2m")
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				_, err := kubectl("get", "ns", throwaway)
				return err != nil
			}).Should(BeTrue(), "the confirmed namespace should have been deleted and finished terminating")
		})

		It("retains an unconfirmed namespace", func() {
			const kept = "e2e-kept"
			manifest := fmt.Sprintf(`
apiVersion: platform.nabeem.com/v1alpha1
kind: NamespaceProfile
metadata:
  name: e2e-kept-profile
spec:
  namespace: %s
  lifecycle:
    deleteNamespaceOnRemoval: true
`, kept)

			out, err := applyStdin(manifest)
			Expect(err).NotTo(HaveOccurred(), out)
			Eventually(func() (string, error) {
				return kubectl("get", "ns", kept, "-o", "jsonpath={.status.phase}")
			}).Should(Equal("Active"))

			_, err = kubectl("delete", "nsp", "e2e-kept-profile", "--wait=true", "--timeout=2m")
			Expect(err).NotTo(HaveOccurred())

			By("keeping the namespace, because the confirmation annotation is absent")
			phase, err := kubectl("get", "ns", kept, "-o", "jsonpath={.status.phase}")
			Expect(err).NotTo(HaveOccurred())
			Expect(phase).To(Equal("Active"))

			_, _ = kubectl("delete", "ns", kept, "--wait=false")
		})
	})

	Describe("rejecting invalid profiles at admission", func() {
		It("refuses a profile targeting a reserved namespace", func() {
			out, err := createStdin(`
apiVersion: platform.nabeem.com/v1alpha1
kind: NamespaceProfile
metadata:
  name: e2e-reserved-profile
spec:
  namespace: kube-system
`)
			Expect(err).To(HaveOccurred())
			Expect(out).To(ContainSubstring("must not target a reserved namespace"))
		})

		It("refuses a contradictory limit range", func() {
			out, err := createStdin(`
apiVersion: platform.nabeem.com/v1alpha1
kind: NamespaceProfile
metadata:
  name: e2e-bad-limits-profile
spec:
  namespace: e2e-bad-limits
  limitRange:
    min: {cpu: "2"}
    max: {cpu: "1"}
`)
			Expect(err).To(HaveOccurred())
			Expect(out).To(ContainSubstring("limitRange.min must not exceed limitRange.max"))
		})
	})
})

// restrictedPod renders a pod that satisfies the restricted Pod Security
// Standard. cpu, when non-empty, sets an explicit CPU request so a spec can
// deliberately exceed the LimitRange maximum.
func restrictedPod(name, cpu string) string {
	// Six spaces: a sibling of the container's securityContext, not a child of
	// it. Getting this wrong produces a strict-decoding error about an unknown
	// field rather than the admission rejection the spec is looking for.
	resources := ""
	if cpu != "" {
		resources = fmt.Sprintf(`
      resources:
        requests:
          cpu: "%s"
        limits:
          cpu: "%s"`, cpu, cpu)
	}

	return fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: pause
      image: registry.k8s.io/pause:3.10
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]%s
`, name, tenantNamespace, resources)
}

// privilegedPod renders a pod the restricted level must reject: it runs as
// root, escalates privileges and drops no capabilities.
func privilegedPod(name string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  containers:
    - name: pause
      image: registry.k8s.io/pause:3.10
      securityContext:
        privileged: true
`, name, tenantNamespace)
}
