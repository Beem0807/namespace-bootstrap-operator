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

package integration

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

var _ = Describe("Watches and automatic drift correction", func() {
	var counter int

	BeforeEach(func() { counter++ })

	// Namespaces are never reused, because envtest has no namespace controller
	// and a deleted namespace stays Terminating forever.
	unique := func(prefix string) string {
		return fmt.Sprintf("%s-%d", prefix, counter)
	}

	quantity := func(s string) *resource.Quantity {
		q := resource.MustParse(s)
		return &q
	}

	createProfile := func(name, namespace string) *platformv1alpha1.NamespaceProfile {
		profile := &platformv1alpha1.NamespaceProfile{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: platformv1alpha1.NamespaceProfileSpec{
				Namespace: namespace,
				Quota: &platformv1alpha1.QuotaSpec{
					Requests: &platformv1alpha1.ComputeResources{CPU: quantity("8")},
					Pods:     ptr.To(int32(50)),
				},
				LimitRange: &platformv1alpha1.LimitRangeSpec{
					DefaultRequest: &platformv1alpha1.ComputeResources{CPU: quantity("100m")},
				},
				Security: &platformv1alpha1.SecuritySpec{
					PodSecurityStandard: platformv1alpha1.PodSecurityRestricted,
				},
			},
		}
		Expect(k8sClient.Create(ctx, profile)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, profile) })
		return profile
	}

	// quotaCPU reads the live CPU request ceiling, for use inside Eventually.
	quotaCPU := func(namespace string) func() (string, error) {
		return func() (string, error) {
			var quota corev1.ResourceQuota
			key := client.ObjectKey{Namespace: namespace, Name: platformv1alpha1.ResourceQuotaName}
			if err := k8sClient.Get(ctx, key, &quota); err != nil {
				return "", err
			}
			value := quota.Spec.Hard[corev1.ResourceRequestsCPU]
			return value.String(), nil
		}
	}

	readyCondition := func(name string) func() (metav1.ConditionStatus, error) {
		return func() (metav1.ConditionStatus, error) {
			var profile platformv1alpha1.NamespaceProfile
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &profile); err != nil {
				return "", err
			}
			condition := meta.FindStatusCondition(profile.Status.Conditions, platformv1alpha1.ConditionReady)
			if condition == nil {
				return "", nil
			}
			return condition.Status, nil
		}
	}

	Describe("provisioning on its own", func() {
		It("creates everything in response to the profile being created", func() {
			// No Reconcile call anywhere in this spec. The only trigger is the
			// NamespaceProfile create event reaching the controller.
			ns := unique("auto-provision")
			createProfile("auto-provision-profile", ns)

			Eventually(func() error {
				var namespace corev1.Namespace
				return k8sClient.Get(ctx, client.ObjectKey{Name: ns}, &namespace)
			}).Should(Succeed(), "the namespace should be created without any prompting")

			Eventually(quotaCPU(ns)).Should(Equal("8"))

			Eventually(func() error {
				var limits corev1.LimitRange
				return k8sClient.Get(ctx, client.ObjectKey{
					Namespace: ns, Name: platformv1alpha1.LimitRangeName,
				}, &limits)
			}).Should(Succeed())

			Eventually(readyCondition("auto-provision-profile")).Should(Equal(metav1.ConditionTrue))
		})
	})

	Describe("correcting drift without being asked", func() {
		It("restores a hand-edited quota value", func() {
			// The behaviour the whole operator exists for, end to end. Phase 2
			// proved the repair is correct; this proves something notices.
			ns := unique("auto-drift")
			createProfile("auto-drift-profile", ns)
			Eventually(quotaCPU(ns)).Should(Equal("8"))

			By("raising the CPU ceiling by hand, as an impatient engineer would")
			var quota corev1.ResourceQuota
			key := client.ObjectKey{Namespace: ns, Name: platformv1alpha1.ResourceQuotaName}
			Expect(k8sClient.Get(ctx, key, &quota)).To(Succeed())
			quota.Spec.Hard[corev1.ResourceRequestsCPU] = resource.MustParse("20")
			Expect(k8sClient.Update(ctx, &quota)).To(Succeed())

			By("watching it snap back, with nothing calling the reconciler")
			Eventually(quotaCPU(ns)).Should(Equal("8"))

			By("recording that a correction happened")
			Eventually(func() (*metav1.Time, error) {
				var profile platformv1alpha1.NamespaceProfile
				if err := k8sClient.Get(ctx, client.ObjectKey{Name: "auto-drift-profile"}, &profile); err != nil {
					return nil, err
				}
				return profile.Status.LastDriftCorrectionTime, nil
			}).ShouldNot(BeNil())
		})

		It("recreates a deleted quota", func() {
			ns := unique("auto-deleted")
			createProfile("auto-deleted-profile", ns)
			Eventually(quotaCPU(ns)).Should(Equal("8"))

			var quota corev1.ResourceQuota
			key := client.ObjectKey{Namespace: ns, Name: platformv1alpha1.ResourceQuotaName}
			Expect(k8sClient.Get(ctx, key, &quota)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &quota)).To(Succeed())

			// A delete event, rather than an update — a different path through
			// the predicate, since a deleted object may be the last known copy.
			Eventually(quotaCPU(ns)).Should(Equal("8"))
		})

		It("restores a stripped Pod Security label and leaves foreign labels alone", func() {
			ns := unique("auto-labels")
			createProfile("auto-labels-profile", ns)

			Eventually(func() (string, error) {
				var namespace corev1.Namespace
				if err := k8sClient.Get(ctx, client.ObjectKey{Name: ns}, &namespace); err != nil {
					return "", err
				}
				return namespace.Labels[platformv1alpha1.PSAEnforceLabel], nil
			}).Should(Equal("restricted"))

			By("stripping the enforcement label and adding an unrelated one")
			var namespace corev1.Namespace
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, &namespace)).To(Succeed())
			delete(namespace.Labels, platformv1alpha1.PSAEnforceLabel)
			namespace.Labels["istio-injection"] = "enabled"
			Expect(k8sClient.Update(ctx, &namespace)).To(Succeed())

			By("restoring only the label the operator owns")
			Eventually(func() (map[string]string, error) {
				var updated corev1.Namespace
				if err := k8sClient.Get(ctx, client.ObjectKey{Name: ns}, &updated); err != nil {
					return nil, err
				}
				return updated.Labels, nil
			}).Should(SatisfyAll(
				HaveKeyWithValue(platformv1alpha1.PSAEnforceLabel, "restricted"),
				HaveKeyWithValue("istio-injection", "enabled"),
			))
		})
	})

	Describe("responding to spec changes", func() {
		It("applies an edited profile without being prompted", func() {
			ns := unique("auto-update")
			createProfile("auto-update-profile", ns)
			Eventually(quotaCPU(ns)).Should(Equal("8"))

			Eventually(func() error {
				var profile platformv1alpha1.NamespaceProfile
				if err := k8sClient.Get(ctx, client.ObjectKey{Name: "auto-update-profile"}, &profile); err != nil {
					return err
				}
				profile.Spec.Quota.Requests.CPU = quantity("32")
				return k8sClient.Update(ctx, &profile)
			}).Should(Succeed())

			Eventually(quotaCPU(ns)).Should(Equal("32"))
		})
	})

	Describe("settling", func() {
		It("stops writing once everything matches", func() {
			// The loop this rules out: a status write wakes the controller,
			// which writes status, forever. Running under a real manager is
			// the only way to observe it — a direct Reconcile call cannot,
			// because nothing is delivering the events it would produce.
			ns := unique("auto-settle")
			createProfile("auto-settle-profile", ns)
			Eventually(readyCondition("auto-settle-profile")).Should(Equal(metav1.ConditionTrue))

			settled := func() (string, error) {
				var profile platformv1alpha1.NamespaceProfile
				if err := k8sClient.Get(ctx, client.ObjectKey{Name: "auto-settle-profile"}, &profile); err != nil {
					return "", err
				}
				return profile.ResourceVersion, nil
			}

			version, err := settled()
			Expect(err).NotTo(HaveOccurred())

			By("observing no further writes to the profile")
			Consistently(settled).Should(Equal(version))

			By("observing no further writes to the quota")
			quotaVersion := func() (string, error) {
				var quota corev1.ResourceQuota
				key := client.ObjectKey{Namespace: ns, Name: platformv1alpha1.ResourceQuotaName}
				if err := k8sClient.Get(ctx, key, &quota); err != nil {
					return "", err
				}
				return quota.ResourceVersion, nil
			}
			observed, err := quotaVersion()
			Expect(err).NotTo(HaveOccurred())
			Consistently(quotaVersion).Should(Equal(observed))
		})
	})

	Describe("mapping events to profiles", func() {
		It("wakes every profile that targets the namespace an event came from", func() {
			// The field index and map function in one assertion. Two profiles
			// claim one namespace, so an event in it must fan out to both —
			// otherwise only whichever reconciled last would report anything,
			// and the other would sit with empty status forever.
			//
			// Which one wins is not asserted: the first to reconcile creates
			// the namespace with its own labels and the other is refused.
			// Making that deterministic is the conflict-detection work still
			// to come; what matters here is that both are woken and both
			// report.
			ns := unique("auto-fanout")
			createProfile("auto-fanout-first", ns)
			createProfile("auto-fanout-second", ns)

			phases := func() ([]platformv1alpha1.ProfilePhase, error) {
				var observed []platformv1alpha1.ProfilePhase
				for _, name := range []string{"auto-fanout-first", "auto-fanout-second"} {
					var profile platformv1alpha1.NamespaceProfile
					if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &profile); err != nil {
						return nil, err
					}
					observed = append(observed, profile.Status.Phase)
				}
				return observed, nil
			}

			Eventually(phases).Should(ConsistOf(
				platformv1alpha1.ProfilePhaseReady,
				platformv1alpha1.ProfilePhaseConflict,
			))
		})
	})
})
