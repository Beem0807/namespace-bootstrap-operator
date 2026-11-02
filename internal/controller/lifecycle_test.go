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

package controller

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

// These specs cover the parts of the operator that decide what *not* to do:
// which profile is entitled to a namespace, when a pre-existing namespace may
// be taken over, and what deleting a profile is allowed to destroy.
//
// They matter more than their line count suggests. Every other behaviour here
// is recoverable — a wrong quota is corrected on the next pass. Deleting a
// namespace is not.
var _ = Describe("Ownership, adoption and deletion", func() {
	var (
		reconciler *NamespaceProfileReconciler
		counter    int
	)

	BeforeEach(func() {
		reconciler = &NamespaceProfileReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: events.NewFakeRecorder(100),
		}
		counter++
	})

	unique := func(prefix string) string {
		return fmt.Sprintf("%s-%d", prefix, counter)
	}

	reconcileProfile := func(name string) {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	createProfile := func(name string, spec platformv1alpha1.NamespaceProfileSpec) *platformv1alpha1.NamespaceProfile {
		profile := &platformv1alpha1.NamespaceProfile{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, profile)).To(Succeed())
		DeferCleanup(func() {
			// Remove the finalizer directly, so a spec that leaves a profile
			// mid-deletion cannot wedge the suite.
			var leftover platformv1alpha1.NamespaceProfile
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &leftover); err == nil {
				leftover.Finalizers = nil
				_ = k8sClient.Update(ctx, &leftover)
				_ = k8sClient.Delete(ctx, &leftover)
			}
		})
		return profile
	}

	managedSpec := func(namespace string) platformv1alpha1.NamespaceProfileSpec {
		return platformv1alpha1.NamespaceProfileSpec{
			Namespace: namespace,
			Quota: &platformv1alpha1.QuotaSpec{
				Requests: &platformv1alpha1.ComputeResources{CPU: quantity("8")},
			},
			LimitRange: &platformv1alpha1.LimitRangeSpec{
				DefaultRequest: &platformv1alpha1.ComputeResources{CPU: quantity("100m")},
			},
			// Two policies: the default deny, plus the same-namespace
			// allowance that keeps the namespace's own pods able to talk to
			// each other. Spelled out rather than left to the accessor
			// defaults, so the counts asserted below are obvious.
			Network: &platformv1alpha1.NetworkSpec{
				DefaultDenyIngress: ptr.To(true),
				AllowSameNamespace: ptr.To(true),
			},
			Security: &platformv1alpha1.SecuritySpec{
				PodSecurityStandard: platformv1alpha1.PodSecurityRestricted,
			},
		}
	}

	getProfile := func(name string) *platformv1alpha1.NamespaceProfile {
		var profile platformv1alpha1.NamespaceProfile
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &profile)).To(Succeed())
		return &profile
	}

	getNamespace := func(name string) *corev1.Namespace {
		var namespace corev1.Namespace
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &namespace)).To(Succeed())
		return &namespace
	}

	quotaExists := func(namespace string) bool {
		err := k8sClient.Get(ctx, client.ObjectKey{
			Namespace: namespace, Name: platformv1alpha1.ResourceQuotaName,
		}, &corev1.ResourceQuota{})
		return err == nil
	}

	limitRangeExists := func(namespace string) bool {
		err := k8sClient.Get(ctx, client.ObjectKey{
			Namespace: namespace, Name: platformv1alpha1.LimitRangeName,
		}, &corev1.LimitRange{})
		return err == nil
	}

	policyCount := func(namespace string) int {
		var policies networkingv1.NetworkPolicyList
		Expect(k8sClient.List(ctx, &policies, client.InNamespace(namespace))).To(Succeed())
		return len(policies.Items)
	}

	createNamespace := func(name string, labels map[string]string) {
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		})).To(Succeed())
	}

	Describe("the finalizer", func() {
		It("is registered before anything is provisioned", func() {
			// Registered first so there is never a window in which generated
			// resources exist with nothing committed to removing them.
			ns := unique("finalizer")
			createProfile("finalizer-profile", managedSpec(ns))

			reconcileProfile("finalizer-profile")

			Expect(getProfile("finalizer-profile").Finalizers).
				To(ContainElement(platformv1alpha1.CleanupFinalizer))
		})

		It("is added only once", func() {
			ns := unique("finalizer-once")
			createProfile("finalizer-once-profile", managedSpec(ns))

			reconcileProfile("finalizer-once-profile")
			version := getProfile("finalizer-once-profile").ResourceVersion

			reconcileProfile("finalizer-once-profile")

			Expect(getProfile("finalizer-once-profile").ResourceVersion).To(Equal(version),
				"a second reconciliation must not re-patch the finalizer")
		})
	})

	Describe("deleting a profile", func() {
		It("removes the generated resources and keeps the namespace", func() {
			// The default and the important case: deleting a profile must never
			// take workloads with it.
			ns := unique("cleanup")
			profile := createProfile("cleanup-profile", managedSpec(ns))
			reconcileProfile("cleanup-profile")
			Expect(quotaExists(ns)).To(BeTrue())
			Expect(policyCount(ns)).To(Equal(2))

			Expect(k8sClient.Delete(ctx, profile)).To(Succeed())
			reconcileProfile("cleanup-profile")

			By("letting the profile actually go away")
			err := k8sClient.Get(ctx, client.ObjectKey{Name: "cleanup-profile"},
				&platformv1alpha1.NamespaceProfile{})
			Expect(err).To(MatchError(ContainSubstring("not found")))

			By("removing everything it generated")
			Expect(quotaExists(ns)).To(BeFalse())
			Expect(limitRangeExists(ns)).To(BeFalse())
			Expect(policyCount(ns)).To(BeZero())

			By("leaving the namespace in place")
			namespace := getNamespace(ns)
			Expect(namespace.DeletionTimestamp).To(BeNil())
		})

		It("retains the generated resources when asked to", func() {
			// For handing a namespace to another system. The trade-off is that
			// the namespace keeps policy nothing accounts for, which is why the
			// operator warns about it.
			ns := unique("retain")
			spec := managedSpec(ns)
			spec.Lifecycle = &platformv1alpha1.LifecycleSpec{
				RetainManagedResources: ptr.To(true),
			}
			profile := createProfile("retain-profile", spec)
			reconcileProfile("retain-profile")
			Expect(quotaExists(ns)).To(BeTrue())

			Expect(k8sClient.Delete(ctx, profile)).To(Succeed())
			reconcileProfile("retain-profile")

			Expect(quotaExists(ns)).To(BeTrue())
			Expect(limitRangeExists(ns)).To(BeTrue())
			Expect(policyCount(ns)).To(Equal(2))
		})

		It("never deletes a resource it does not own", func() {
			ns := unique("cleanup-foreign")
			profile := createProfile("cleanup-foreign-profile",
				platformv1alpha1.NamespaceProfileSpec{Namespace: ns})
			reconcileProfile("cleanup-foreign-profile")

			Expect(k8sClient.Create(ctx, &corev1.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:      platformv1alpha1.ResourceQuotaName,
					Namespace: ns,
				},
				Spec: corev1.ResourceQuotaSpec{
					Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")},
				},
			})).To(Succeed())

			Expect(k8sClient.Delete(ctx, profile)).To(Succeed())
			reconcileProfile("cleanup-foreign-profile")

			Expect(quotaExists(ns)).To(BeTrue(),
				"cleanup must not become a way to delete what the operator could never write to")
		})

		It("is idempotent, so a repeated cleanup is harmless", func() {
			ns := unique("cleanup-twice")
			profile := createProfile("cleanup-twice-profile", managedSpec(ns))
			reconcileProfile("cleanup-twice-profile")

			Expect(k8sClient.Delete(ctx, profile)).To(Succeed())
			reconcileProfile("cleanup-twice-profile")

			// The profile is gone, so this exercises the not-found path the
			// work queue can legitimately deliver after cleanup finished.
			reconcileProfile("cleanup-twice-profile")
		})
	})

	Describe("namespace deletion", func() {
		// envtest runs no namespace controller, so a deleted namespace stays
		// Terminating forever rather than disappearing. These specs therefore
		// assert on deletionTimestamp, which is what the API server actually
		// records — asserting the namespace had vanished would be asserting
		// something envtest cannot do.
		It("retains the namespace when deletion is not confirmed", func() {
			ns := unique("delete-unconfirmed")
			spec := managedSpec(ns)
			spec.Lifecycle = &platformv1alpha1.LifecycleSpec{
				DeleteNamespaceOnRemoval: ptr.To(true),
			}
			profile := createProfile("delete-unconfirmed-profile", spec)
			reconcileProfile("delete-unconfirmed-profile")

			Expect(k8sClient.Delete(ctx, profile)).To(Succeed())
			reconcileProfile("delete-unconfirmed-profile")

			Expect(getNamespace(ns).DeletionTimestamp).To(BeNil(),
				"the lifecycle flag alone must not be enough")
		})

		It("retains the namespace when the confirmation names the wrong namespace", func() {
			// Why the annotation holds a name rather than being a boolean: a
			// confirmation copied from another profile cannot be accidentally
			// correct.
			ns := unique("delete-wrong-name")
			spec := managedSpec(ns)
			spec.Lifecycle = &platformv1alpha1.LifecycleSpec{
				DeleteNamespaceOnRemoval: ptr.To(true),
			}
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name: "delete-wrong-name-profile",
					Annotations: map[string]string{
						platformv1alpha1.ConfirmNamespaceDeletionAnnotation: "some-other-namespace",
					},
				},
				Spec: spec,
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			reconcileProfile("delete-wrong-name-profile")

			Expect(k8sClient.Delete(ctx, profile)).To(Succeed())
			reconcileProfile("delete-wrong-name-profile")

			Expect(getNamespace(ns).DeletionTimestamp).To(BeNil())
		})

		It("deletes the namespace when every check passes", func() {
			ns := unique("delete-confirmed")
			spec := managedSpec(ns)
			spec.Lifecycle = &platformv1alpha1.LifecycleSpec{
				DeleteNamespaceOnRemoval: ptr.To(true),
			}
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name: "delete-confirmed-profile",
					Annotations: map[string]string{
						platformv1alpha1.ConfirmNamespaceDeletionAnnotation: ns,
					},
				},
				Spec: spec,
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			reconcileProfile("delete-confirmed-profile")
			Expect(getNamespace(ns).DeletionTimestamp).To(BeNil())

			Expect(k8sClient.Delete(ctx, profile)).To(Succeed())
			reconcileProfile("delete-confirmed-profile")

			Expect(getNamespace(ns).DeletionTimestamp).NotTo(BeNil())
		})

		It("refuses to delete an adopted namespace, however it is confirmed", func() {
			// The strongest of the four checks. The operator did not create
			// this namespace and cannot know what else depends on it, so no
			// combination of settings makes it deletable.
			ns := unique("delete-adopted")
			createNamespace(ns, map[string]string{"owner": "another-team"})

			spec := managedSpec(ns)
			spec.AdoptExisting = ptr.To(true)
			spec.Lifecycle = &platformv1alpha1.LifecycleSpec{
				DeleteNamespaceOnRemoval: ptr.To(true),
			}
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name: "delete-adopted-profile",
					Annotations: map[string]string{
						platformv1alpha1.ConfirmNamespaceDeletionAnnotation: ns,
					},
				},
				Spec: spec,
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			reconcileProfile("delete-adopted-profile")
			Expect(getNamespace(ns).Annotations).
				To(HaveKeyWithValue(platformv1alpha1.AdoptedAnnotation, "true"))

			Expect(k8sClient.Delete(ctx, profile)).To(Succeed())
			reconcileProfile("delete-adopted-profile")

			Expect(getNamespace(ns).DeletionTimestamp).To(BeNil())
			Expect(getNamespace(ns).Labels).To(HaveKeyWithValue("owner", "another-team"))
		})
	})

	Describe("adoption", func() {
		It("takes over a pre-existing namespace when asked", func() {
			ns := unique("adopt")
			createNamespace(ns, map[string]string{"owner": "another-team", "cost-center": "eng"})

			spec := managedSpec(ns)
			spec.AdoptExisting = ptr.To(true)
			createProfile("adopt-profile", spec)

			reconcileProfile("adopt-profile")

			namespace := getNamespace(ns)
			By("stamping its own ownership labels")
			Expect(namespace.Labels).To(HaveKeyWithValue(
				platformv1alpha1.ManagedByLabel, platformv1alpha1.OperatorName))
			Expect(namespace.Labels).To(HaveKeyWithValue(
				platformv1alpha1.ProfileLabel, "adopt-profile"))

			By("applying the policy the profile declares")
			Expect(namespace.Labels).To(HaveKeyWithValue(
				platformv1alpha1.PSAEnforceLabel, "restricted"))
			Expect(quotaExists(ns)).To(BeTrue())
			Expect(policyCount(ns)).To(Equal(2))

			By("marking it as adopted, permanently barring deletion")
			Expect(namespace.Annotations).To(HaveKeyWithValue(
				platformv1alpha1.AdoptedAnnotation, "true"))

			By("preserving labels it has no opinion about")
			Expect(namespace.Labels).To(HaveKeyWithValue("owner", "another-team"))
			Expect(namespace.Labels).To(HaveKeyWithValue("cost-center", "eng"))

			By("reporting itself Ready")
			Expect(getProfile("adopt-profile").Status.Phase).
				To(Equal(platformv1alpha1.ProfilePhaseReady))
		})

		It("keeps the adoption marker across later reconciliations", func() {
			// The marker must be sticky. Dropping it would silently re-arm
			// namespace deletion for a namespace the operator never created.
			ns := unique("adopt-sticky")
			createNamespace(ns, nil)

			spec := managedSpec(ns)
			spec.AdoptExisting = ptr.To(true)
			createProfile("adopt-sticky-profile", spec)

			reconcileProfile("adopt-sticky-profile")
			reconcileProfile("adopt-sticky-profile")
			reconcileProfile("adopt-sticky-profile")

			Expect(getNamespace(ns).Annotations).
				To(HaveKeyWithValue(platformv1alpha1.AdoptedAnnotation, "true"))
		})

		It("settles after adopting, rather than rewriting on every pass", func() {
			ns := unique("adopt-settle")
			createNamespace(ns, nil)

			spec := managedSpec(ns)
			spec.AdoptExisting = ptr.To(true)
			createProfile("adopt-settle-profile", spec)

			reconcileProfile("adopt-settle-profile")
			version := getNamespace(ns).ResourceVersion

			reconcileProfile("adopt-settle-profile")
			reconcileProfile("adopt-settle-profile")

			Expect(getNamespace(ns).ResourceVersion).To(Equal(version))
		})

		It("adopts a foreign resource sitting at one of its names", func() {
			ns := unique("adopt-quota")
			createNamespace(ns, nil)
			Expect(k8sClient.Create(ctx, &corev1.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:      platformv1alpha1.ResourceQuotaName,
					Namespace: ns,
				},
				Spec: corev1.ResourceQuotaSpec{
					Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("999")},
				},
			})).To(Succeed())

			spec := managedSpec(ns)
			spec.AdoptExisting = ptr.To(true)
			createProfile("adopt-quota-profile", spec)

			reconcileProfile("adopt-quota-profile")

			var quota corev1.ResourceQuota
			Expect(k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns, Name: platformv1alpha1.ResourceQuotaName,
			}, &quota)).To(Succeed())
			Expect(quota.Labels).To(HaveKeyWithValue(
				platformv1alpha1.ManagedByLabel, platformv1alpha1.OperatorName))
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceRequestsCPU, resource.MustParse("8")))

			// The pre-existing pod cap survives, and that is correct rather
			// than a shortfall. spec.hard is a map, so server-side apply
			// tracks ownership key by key, and it can only prune keys this
			// operator previously owned — "pods" was written by somebody else.
			// The recorded-keys annotation does not list it either, so it is
			// ignored from here on rather than being reported as drift the
			// operator could never fix.
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourcePods, resource.MustParse("999")))
			Expect(quota.Annotations[platformv1alpha1.ManagedQuotaKeysAnnotation]).
				NotTo(ContainSubstring("pods"))
		})

		It("still refuses a pre-existing namespace without adoption", func() {
			ns := unique("adopt-off")
			createNamespace(ns, map[string]string{"owner": "another-team"})
			createProfile("adopt-off-profile", managedSpec(ns))

			reconcileProfile("adopt-off-profile")

			namespace := getNamespace(ns)
			Expect(namespace.Labels).NotTo(HaveKey(platformv1alpha1.ManagedByLabel))
			Expect(quotaExists(ns)).To(BeFalse())

			profile := getProfile("adopt-off-profile")
			Expect(profile.Status.Phase).To(Equal(platformv1alpha1.ProfilePhaseConflict))
			ready := meta.FindStatusCondition(profile.Status.Conditions, platformv1alpha1.ConditionReady)
			Expect(ready.Reason).To(Equal(platformv1alpha1.ReasonNamespaceNotAdopted))
		})
	})

	Describe("two profiles competing for one namespace", func() {
		It("gives the namespace to one profile deterministically, by name on a tie", func() {
			// Creation timestamps have one-second granularity, so profiles made
			// back to back genuinely tie — the name tie-break is load-bearing,
			// not decoration. Without it, ownership would depend on map
			// iteration or reconcile order and the two profiles would take
			// turns overwriting each other forever.
			ns := unique("conflict-tie")
			createProfile("aaa-conflict-profile", managedSpec(ns))
			createProfile("zzz-conflict-profile", managedSpec(ns))

			By("reconciling the alphabetically later profile first")
			reconcileProfile("zzz-conflict-profile")
			reconcileProfile("aaa-conflict-profile")

			Expect(getNamespace(ns).Labels).To(HaveKeyWithValue(
				platformv1alpha1.ProfileLabel, "aaa-conflict-profile"))

			winner := getProfile("aaa-conflict-profile")
			loser := getProfile("zzz-conflict-profile")
			Expect(winner.Status.Phase).To(Equal(platformv1alpha1.ProfilePhaseReady))
			Expect(loser.Status.Phase).To(Equal(platformv1alpha1.ProfilePhaseConflict))

			By("naming the competitor on both sides, so the conflict is visible from either")
			Expect(winner.Status.ConflictingProfiles).To(ConsistOf("zzz-conflict-profile"))
			Expect(loser.Status.ConflictingProfiles).To(ConsistOf("aaa-conflict-profile"))

			By("reporting the loser as conflicted but not degraded")
			ready := meta.FindStatusCondition(loser.Status.Conditions, platformv1alpha1.ConditionReady)
			Expect(ready.Reason).To(Equal(platformv1alpha1.ReasonNamespaceConflict))
			Expect(ready.Message).To(ContainSubstring("aaa-conflict-profile"))
			Expect(meta.FindStatusCondition(loser.Status.Conditions,
				platformv1alpha1.ConditionDegraded)).To(HaveField("Status", metav1.ConditionFalse))
		})

		It("holds the decision stable no matter how often either side reconciles", func() {
			ns := unique("conflict-stable")
			createProfile("aaa-stable-profile", managedSpec(ns))
			createProfile("zzz-stable-profile", managedSpec(ns))

			for range 5 {
				reconcileProfile("zzz-stable-profile")
				reconcileProfile("aaa-stable-profile")
			}

			Expect(getNamespace(ns).Labels).To(HaveKeyWithValue(
				platformv1alpha1.ProfileLabel, "aaa-stable-profile"))

			By("leaving the namespace untouched once settled")
			version := getNamespace(ns).ResourceVersion
			reconcileProfile("zzz-stable-profile")
			reconcileProfile("aaa-stable-profile")
			Expect(getNamespace(ns).ResourceVersion).To(Equal(version))
		})

		It("prefers the older profile over the alphabetical tie-break", func() {
			// Age is the primary rule; the name only settles ties. Worth a
			// real second of waiting, because getting the precedence backwards
			// would be invisible in every other spec here.
			ns := unique("conflict-age")
			createProfile("zzz-older-profile", managedSpec(ns))
			time.Sleep(1100 * time.Millisecond)
			createProfile("aaa-newer-profile", managedSpec(ns))

			reconcileProfile("aaa-newer-profile")
			reconcileProfile("zzz-older-profile")

			Expect(getNamespace(ns).Labels).To(HaveKeyWithValue(
				platformv1alpha1.ProfileLabel, "zzz-older-profile"))
		})

		It("keeps an existing claim even when an older profile appears later", func() {
			// Ownership must be stable over time, not recomputed from scratch.
			// Re-deciding on every pass would let a namespace change hands
			// because somebody created a profile with an older timestamp.
			ns := unique("conflict-claim")
			createProfile("zzz-claimed-profile", managedSpec(ns))
			reconcileProfile("zzz-claimed-profile")
			Expect(getNamespace(ns).Labels).To(HaveKeyWithValue(
				platformv1alpha1.ProfileLabel, "zzz-claimed-profile"))

			By("introducing a profile that would otherwise win the tie-break")
			createProfile("aaa-latecomer-profile", managedSpec(ns))
			reconcileProfile("aaa-latecomer-profile")
			reconcileProfile("zzz-claimed-profile")

			Expect(getNamespace(ns).Labels).To(HaveKeyWithValue(
				platformv1alpha1.ProfileLabel, "zzz-claimed-profile"))
			Expect(getProfile("aaa-latecomer-profile").Status.Phase).
				To(Equal(platformv1alpha1.ProfilePhaseConflict))
		})

		It("hands the namespace over when the owning profile is deleted", func() {
			// A profile being deleted stops being a competitor, which is what
			// lets a namespace pass to its replacement without intervention.
			ns := unique("conflict-handover")
			retiring := createProfile("aaa-retiring-profile", managedSpec(ns))
			createProfile("zzz-successor-profile", managedSpec(ns))

			reconcileProfile("aaa-retiring-profile")
			reconcileProfile("zzz-successor-profile")
			Expect(getProfile("zzz-successor-profile").Status.Phase).
				To(Equal(platformv1alpha1.ProfilePhaseConflict))

			By("deleting the owner, which also removes what it generated")
			Expect(k8sClient.Delete(ctx, retiring)).To(Succeed())
			reconcileProfile("aaa-retiring-profile")

			By("letting the successor take over")
			reconcileProfile("zzz-successor-profile")

			Expect(getNamespace(ns).Labels).To(HaveKeyWithValue(
				platformv1alpha1.ProfileLabel, "zzz-successor-profile"))
			Expect(getProfile("zzz-successor-profile").Status.Phase).
				To(Equal(platformv1alpha1.ProfilePhaseReady))
			Expect(quotaExists(ns)).To(BeTrue())
		})

		It("never lets adoption override another profile's ownership", func() {
			// Otherwise spec.adoptExisting would be a way to steal a namespace
			// from a peer profile, which is exactly the conflict the ownership
			// rules exist to arbitrate.
			ns := unique("conflict-adopt")
			createProfile("aaa-owner-profile", managedSpec(ns))
			reconcileProfile("aaa-owner-profile")

			spec := managedSpec(ns)
			spec.AdoptExisting = ptr.To(true)
			createProfile("zzz-thief-profile", spec)

			reconcileProfile("zzz-thief-profile")

			Expect(getNamespace(ns).Labels).To(HaveKeyWithValue(
				platformv1alpha1.ProfileLabel, "aaa-owner-profile"))

			thief := getProfile("zzz-thief-profile")
			Expect(thief.Status.Phase).To(Equal(platformv1alpha1.ProfilePhaseConflict))
			ready := meta.FindStatusCondition(thief.Status.Conditions, platformv1alpha1.ConditionReady)
			Expect(ready.Reason).To(Equal(platformv1alpha1.ReasonNamespaceConflict))
		})
	})
})
