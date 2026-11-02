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

// These specs drive the real reconciler against a real API server.
//
// Reconcile is invoked directly rather than by starting the manager, which
// keeps each spec deterministic: the sequence of reconciliations is explicit
// instead of depending on watch delivery timing. The watches that make drift
// correction automatic are a later phase; what is asserted here is that a
// reconciliation converges, and that a second one does nothing.
//
// Two envtest properties shape these specs. There is no kubelet or scheduler,
// so no pod ever runs; and there is no namespace controller, so a deleted
// namespace stays in Terminating forever rather than disappearing. Namespaces
// are therefore never reused between specs.
var _ = Describe("NamespaceProfile provisioning", func() {
	var (
		reconciler *NamespaceProfileReconciler
		recorder   *events.FakeRecorder
		counter    int
	)

	BeforeEach(func() {
		recorder = events.NewFakeRecorder(100)
		reconciler = &NamespaceProfileReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: recorder,
		}
		counter++
	})

	// drainEvents empties the recorder, returning everything published since
	// the last call.
	drainEvents := func() []string {
		var events []string
		for {
			select {
			case event := <-recorder.Events:
				events = append(events, event)
			default:
				return events
			}
		}
	}

	// unique returns a per-spec suffix so namespaces are never shared, since
	// envtest cannot actually delete one.
	unique := func(prefix string) string {
		return fmt.Sprintf("%s-%d", prefix, counter)
	}

	// reconcileProfile runs one reconciliation and asserts it did not error.
	reconcileProfile := func(name string) reconcile.Result {
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name},
		})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	// createProfile persists a profile and schedules its removal. The profile
	// is cluster-scoped, so only a name is needed.
	createProfile := func(name string, spec platformv1alpha1.NamespaceProfileSpec) *platformv1alpha1.NamespaceProfile {
		profile := &platformv1alpha1.NamespaceProfile{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, profile)).To(Succeed())
		DeferCleanup(func() {
			// The reconciler adds a cleanup finalizer, so a plain Delete would
			// leave the profile in Terminating and its name taken for the rest
			// of the suite. Strip the finalizer first: these specs are about
			// provisioning, and cleanup behaviour has its own suite.
			var leftover platformv1alpha1.NamespaceProfile
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &leftover); err == nil {
				leftover.Finalizers = nil
				_ = k8sClient.Update(ctx, &leftover)
				_ = k8sClient.Delete(ctx, &leftover)
			}
		})
		return profile
	}

	// updateProfile re-reads the profile before mutating it.
	//
	// Reconcile writes status, which bumps the profile's resourceVersion, so a
	// copy held from before a reconciliation cannot be used for an Update — the
	// API server rejects it as a conflict. Re-reading inside a retry is how
	// every real client handles this, and doing it here keeps the specs honest
	// about it rather than papering over it.
	updateProfile := func(name string, mutate func(*platformv1alpha1.NamespaceProfile)) {
		Eventually(func() error {
			var profile platformv1alpha1.NamespaceProfile
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &profile); err != nil {
				return err
			}
			mutate(&profile)
			return k8sClient.Update(ctx, &profile)
		}).Should(Succeed())
	}

	fullSpec := func(namespace string) platformv1alpha1.NamespaceProfileSpec {
		return platformv1alpha1.NamespaceProfileSpec{
			Namespace: namespace,
			Quota: &platformv1alpha1.QuotaSpec{
				Requests: &platformv1alpha1.ComputeResources{
					CPU:    quantity("8"),
					Memory: quantity("16Gi"),
				},
				Pods: ptr.To(int32(50)),
			},
			LimitRange: &platformv1alpha1.LimitRangeSpec{
				DefaultRequest: &platformv1alpha1.ComputeResources{CPU: quantity("100m")},
				Max:            &platformv1alpha1.ComputeResources{CPU: quantity("4")},
			},
			Network: &platformv1alpha1.NetworkSpec{
				DefaultDenyIngress: ptr.To(true),
				DefaultDenyEgress:  ptr.To(true),
				AllowDNS:           ptr.To(true),
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

	conditionOf := func(profile *platformv1alpha1.NamespaceProfile, conditionType string) *metav1.Condition {
		return meta.FindStatusCondition(profile.Status.Conditions, conditionType)
	}

	getNamespace := func(name string) *corev1.Namespace {
		var ns corev1.Namespace
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &ns)).To(Succeed())
		return &ns
	}

	getQuota := func(namespace string) *corev1.ResourceQuota {
		var quota corev1.ResourceQuota
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Namespace: namespace, Name: platformv1alpha1.ResourceQuotaName,
		}, &quota)).To(Succeed())
		return &quota
	}

	getPolicy := func(namespace, name string) *networkingv1.NetworkPolicy {
		var policy networkingv1.NetworkPolicy
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &policy)).To(Succeed())
		return &policy
	}

	listPolicyNames := func(namespace string) []string {
		var policies networkingv1.NetworkPolicyList
		Expect(k8sClient.List(ctx, &policies, client.InNamespace(namespace))).To(Succeed())
		names := make([]string, 0, len(policies.Items))
		for i := range policies.Items {
			names = append(names, policies.Items[i].Name)
		}
		return names
	}

	getLimitRange := func(namespace string) *corev1.LimitRange {
		var limits corev1.LimitRange
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Namespace: namespace, Name: platformv1alpha1.LimitRangeName,
		}, &limits)).To(Succeed())
		return &limits
	}

	Describe("initial provisioning", func() {
		It("creates the namespace, quota and limit range from one reconciliation", func() {
			ns := unique("provision")
			createProfile("provision-profile", fullSpec(ns))

			reconcileProfile("provision-profile")

			By("creating the namespace with ownership and Pod Security labels")
			namespace := getNamespace(ns)
			Expect(namespace.Labels).To(HaveKeyWithValue(
				platformv1alpha1.ManagedByLabel, platformv1alpha1.OperatorName))
			Expect(namespace.Labels).To(HaveKeyWithValue(
				platformv1alpha1.ProfileLabel, "provision-profile"))
			Expect(namespace.Labels).To(HaveKeyWithValue(
				platformv1alpha1.PSAEnforceLabel, "restricted"))
			Expect(namespace.Labels).To(HaveKeyWithValue(
				platformv1alpha1.PSAEnforceVersionLabel, "latest"))
			// audit and warn follow the enforced level when the profile leaves
			// them unset.
			Expect(namespace.Labels).To(HaveKeyWithValue(platformv1alpha1.PSAAuditLabel, "restricted"))
			Expect(namespace.Labels).To(HaveKeyWithValue(platformv1alpha1.PSAWarnLabel, "restricted"))

			By("creating the ResourceQuota with the declared hard limits")
			quota := getQuota(ns)
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceRequestsCPU, resource.MustParse("8")))
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceRequestsMemory, resource.MustParse("16Gi")))
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourcePods, resource.MustParse("50")))
			Expect(quota.Labels).To(HaveKeyWithValue(
				platformv1alpha1.ManagedByLabel, platformv1alpha1.OperatorName))

			By("creating the LimitRange with a single Container item")
			limits := getLimitRange(ns)
			Expect(limits.Spec.Limits).To(HaveLen(1))
			Expect(limits.Spec.Limits[0].Type).To(Equal(corev1.LimitTypeContainer))
			Expect(limits.Spec.Limits[0].DefaultRequest).To(HaveKeyWithValue(
				corev1.ResourceCPU, resource.MustParse("100m")))
			Expect(limits.Spec.Limits[0].Max).To(HaveKeyWithValue(
				corev1.ResourceCPU, resource.MustParse("4")))

			By("recording which generation of the profile produced each object")
			Expect(quota.Annotations).To(HaveKeyWithValue(
				platformv1alpha1.AppliedGenerationAnnotation, "1"))
		})

		It("manages only what the profile declares", func() {
			// A profile with no quota and no limit range still creates and
			// labels its namespace. Omitting a section means the operator
			// manages nothing of that kind, not that it applies an empty one.
			ns := unique("bare")
			createProfile("bare-managed-profile", platformv1alpha1.NamespaceProfileSpec{Namespace: ns})

			reconcileProfile("bare-managed-profile")

			Expect(getNamespace(ns).Labels).To(HaveKeyWithValue(
				platformv1alpha1.ProfileLabel, "bare-managed-profile"))

			By("creating no ResourceQuota")
			err := k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns, Name: platformv1alpha1.ResourceQuotaName,
			}, &corev1.ResourceQuota{})
			Expect(err).To(MatchError(ContainSubstring("not found")))

			By("creating no LimitRange")
			err = k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns, Name: platformv1alpha1.LimitRangeName,
			}, &corev1.LimitRange{})
			Expect(err).To(MatchError(ContainSubstring("not found")))

			By("setting no Pod Security labels, leaving any set by hand alone")
			Expect(getNamespace(ns).Labels).NotTo(HaveKey(platformv1alpha1.PSAEnforceLabel))
		})
	})

	Describe("idempotency", func() {
		It("performs no writes when everything already matches", func() {
			// The property that keeps a settled cluster quiet. If a second
			// reconciliation rewrote these objects, every write would produce a
			// watch event, which would trigger another reconciliation — a loop
			// that looks like a busy operator and never settles.
			ns := unique("idempotent")
			createProfile("idempotent-profile", fullSpec(ns))

			reconcileProfile("idempotent-profile")

			namespaceVersion := getNamespace(ns).ResourceVersion
			quotaVersion := getQuota(ns).ResourceVersion
			limitsVersion := getLimitRange(ns).ResourceVersion

			By("reconciling four more times")
			for range 4 {
				reconcileProfile("idempotent-profile")
			}

			By("leaving every resourceVersion untouched")
			Expect(getNamespace(ns).ResourceVersion).To(Equal(namespaceVersion))
			Expect(getQuota(ns).ResourceVersion).To(Equal(quotaVersion))
			Expect(getLimitRange(ns).ResourceVersion).To(Equal(limitsVersion))
		})
	})

	Describe("drift correction", func() {
		It("restores a hand-edited quota value", func() {
			// The case named in the specification: the profile declares 8 CPU
			// and somebody raises it to 20.
			ns := unique("drift-value")
			createProfile("drift-value-profile", fullSpec(ns))
			reconcileProfile("drift-value-profile")

			quota := getQuota(ns)
			quota.Spec.Hard[corev1.ResourceRequestsCPU] = resource.MustParse("20")
			Expect(k8sClient.Update(ctx, quota)).To(Succeed())
			Expect(getQuota(ns).Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceRequestsCPU, resource.MustParse("20")))

			reconcileProfile("drift-value-profile")

			Expect(getQuota(ns).Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceRequestsCPU, resource.MustParse("8")))
		})

		It("recreates a deleted quota and limit range", func() {
			ns := unique("drift-deleted")
			createProfile("drift-deleted-profile", fullSpec(ns))
			reconcileProfile("drift-deleted-profile")

			Expect(k8sClient.Delete(ctx, getQuota(ns))).To(Succeed())
			Expect(k8sClient.Delete(ctx, getLimitRange(ns))).To(Succeed())

			reconcileProfile("drift-deleted-profile")

			Expect(getQuota(ns).Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourcePods, resource.MustParse("50")))
			Expect(getLimitRange(ns).Spec.Limits).To(HaveLen(1))
		})

		It("restores a removed Pod Security label without disturbing other labels", func() {
			ns := unique("drift-labels")
			createProfile("drift-labels-profile", fullSpec(ns))
			reconcileProfile("drift-labels-profile")

			By("stripping the enforcement label and adding unrelated ones by hand")
			namespace := getNamespace(ns)
			delete(namespace.Labels, platformv1alpha1.PSAEnforceLabel)
			namespace.Labels["istio-injection"] = "enabled"
			namespace.Labels["cost-center"] = "platform-eng"
			Expect(k8sClient.Update(ctx, namespace)).To(Succeed())

			reconcileProfile("drift-labels-profile")

			By("restoring the label the operator owns")
			restored := getNamespace(ns)
			Expect(restored.Labels).To(HaveKeyWithValue(platformv1alpha1.PSAEnforceLabel, "restricted"))

			By("leaving labels it does not own completely alone")
			// This is what server-side apply buys: the operator declares only
			// its own label keys, so it has no opinion about anyone else's.
			Expect(restored.Labels).To(HaveKeyWithValue("istio-injection", "enabled"))
			Expect(restored.Labels).To(HaveKeyWithValue("cost-center", "platform-eng"))
		})

		It("ignores a quota key the operator never produces", func() {
			// spec.hard is a map, so apply tracks ownership key by key. A
			// "services" limit added by an administrator is not the operator's
			// to police, and flagging it would mean reporting drift forever
			// that the operator could never fix.
			ns := unique("drift-foreign-key")
			createProfile("drift-foreign-key-profile", fullSpec(ns))
			reconcileProfile("drift-foreign-key-profile")

			quota := getQuota(ns)
			quota.Spec.Hard["services"] = resource.MustParse("10")
			Expect(k8sClient.Update(ctx, quota)).To(Succeed())
			versionAfterEdit := getQuota(ns).ResourceVersion

			reconcileProfile("drift-foreign-key-profile")

			after := getQuota(ns)
			Expect(after.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceName("services"), resource.MustParse("10")))
			Expect(after.ResourceVersion).To(Equal(versionAfterEdit),
				"a foreign key must not provoke a write")
		})
	})

	Describe("responding to profile changes", func() {
		It("updates the quota when the profile changes", func() {
			ns := unique("update")
			createProfile("update-profile", fullSpec(ns))
			reconcileProfile("update-profile")

			updateProfile("update-profile", func(p *platformv1alpha1.NamespaceProfile) {
				p.Spec.Quota.Requests.CPU = quantity("32")
				p.Spec.Quota.Pods = ptr.To(int32(200))
			})

			reconcileProfile("update-profile")

			quota := getQuota(ns)
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceRequestsCPU, resource.MustParse("32")))
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourcePods, resource.MustParse("200")))
		})

		It("releases a hard limit the profile stops declaring", func() {
			// Withdrawing spec.quota.pods must actually remove the key. This
			// works because the operator stops declaring it and server-side
			// apply prunes fields its field manager no longer asserts — but
			// only if the apply happens at all, which is why the in-sync check
			// treats a withdrawn owned key as drift.
			ns := unique("withdraw-key")
			createProfile("withdraw-key-profile", fullSpec(ns))
			reconcileProfile("withdraw-key-profile")
			Expect(getQuota(ns).Spec.Hard).To(HaveKey(corev1.ResourcePods))

			updateProfile("withdraw-key-profile", func(p *platformv1alpha1.NamespaceProfile) {
				p.Spec.Quota.Pods = nil
			})

			reconcileProfile("withdraw-key-profile")

			quota := getQuota(ns)
			Expect(quota.Spec.Hard).NotTo(HaveKey(corev1.ResourcePods))
			Expect(quota.Spec.Hard).To(HaveKey(corev1.ResourceRequestsCPU),
				"the limits still declared must survive")
		})

		It("withdraws the Pod Security labels when the security section is removed", func() {
			ns := unique("withdraw-psa")
			createProfile("withdraw-psa-profile", fullSpec(ns))
			reconcileProfile("withdraw-psa-profile")
			Expect(getNamespace(ns).Labels).To(HaveKey(platformv1alpha1.PSAEnforceLabel))

			updateProfile("withdraw-psa-profile", func(p *platformv1alpha1.NamespaceProfile) {
				p.Spec.Security = nil
			})

			reconcileProfile("withdraw-psa-profile")

			namespace := getNamespace(ns)
			Expect(namespace.Labels).NotTo(HaveKey(platformv1alpha1.PSAEnforceLabel))
			Expect(namespace.Labels).NotTo(HaveKey(platformv1alpha1.PSAAuditLabel))
			Expect(namespace.Labels).To(HaveKeyWithValue(
				platformv1alpha1.ProfileLabel, "withdraw-psa-profile"),
				"ownership labels must remain")
		})

		It("deletes its quota when the profile stops declaring one", func() {
			ns := unique("withdraw-quota")
			createProfile("withdraw-quota-profile", fullSpec(ns))
			reconcileProfile("withdraw-quota-profile")
			Expect(getQuota(ns)).NotTo(BeNil())

			updateProfile("withdraw-quota-profile", func(p *platformv1alpha1.NamespaceProfile) {
				p.Spec.Quota = nil
			})

			reconcileProfile("withdraw-quota-profile")

			err := k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns, Name: platformv1alpha1.ResourceQuotaName,
			}, &corev1.ResourceQuota{})
			Expect(err).To(MatchError(ContainSubstring("not found")))

			By("leaving the limit range, which is still declared, in place")
			Expect(getLimitRange(ns).Spec.Limits).To(HaveLen(1))
		})
	})

	Describe("object-count quotas", func() {
		It("caps object counts without the operator ever touching those objects", func() {
			// The operator writes numbers into the ResourceQuota and stops
			// there. Enforcement is the API server's ResourceQuota admission
			// plugin rejecting creations beyond the cap — the operator never
			// lists, inspects or deletes a Service, Secret or Deployment, and
			// has no RBAC permission to do so.
			ns := unique("counts")
			createProfile("counts-profile", platformv1alpha1.NamespaceProfileSpec{
				Namespace: ns,
				Quota: &platformv1alpha1.QuotaSpec{
					Pods: ptr.To(int32(50)),
					ObjectCounts: map[string]int32{
						"services":               10,
						"secrets":                50,
						"services.loadbalancers": 0,
						"count/deployments.apps": 20,
						"count/serviceaccounts":  10,
					},
				},
			})

			reconcileProfile("counts-profile")

			quota := getQuota(ns)
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceName("services"), resource.MustParse("10")))
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceName("secrets"), resource.MustParse("50")))
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceName("count/deployments.apps"), resource.MustParse("20")))
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceName("count/serviceaccounts"), resource.MustParse("10")))

			By("honouring a cap of zero rather than treating it as unset")
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceName("services.loadbalancers"), resource.MustParse("0")))

			By("recording the key set it owns, sorted")
			Expect(quota.Annotations).To(HaveKeyWithValue(
				platformv1alpha1.ManagedQuotaKeysAnnotation,
				"count/deployments.apps,count/serviceaccounts,pods,"+
					"secrets,services,services.loadbalancers"))
		})

		It("releases an object-count key the profile stops declaring", func() {
			// This is what the recorded key set exists for. A user-supplied key
			// space means the operator cannot derive from the profile alone
			// whether it once owned "services"; the annotation is the record
			// that lets it release the limit instead of leaving it forever.
			ns := unique("counts-withdraw")
			createProfile("counts-withdraw-profile", platformv1alpha1.NamespaceProfileSpec{
				Namespace: ns,
				Quota: &platformv1alpha1.QuotaSpec{
					Pods:         ptr.To(int32(50)),
					ObjectCounts: map[string]int32{"services": 10, "secrets": 50},
				},
			})
			reconcileProfile("counts-withdraw-profile")
			Expect(getQuota(ns).Spec.Hard).To(HaveKey(corev1.ResourceName("services")))

			updateProfile("counts-withdraw-profile", func(p *platformv1alpha1.NamespaceProfile) {
				p.Spec.Quota.ObjectCounts = map[string]int32{"secrets": 50}
			})

			reconcileProfile("counts-withdraw-profile")

			quota := getQuota(ns)
			Expect(quota.Spec.Hard).NotTo(HaveKey(corev1.ResourceName("services")))
			Expect(quota.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourceName("secrets"), resource.MustParse("50")))
			Expect(quota.Spec.Hard).To(HaveKey(corev1.ResourcePods))
			Expect(quota.Annotations).To(HaveKeyWithValue(
				platformv1alpha1.ManagedQuotaKeysAnnotation, "pods,secrets"))

			By("settling, so the release is not re-applied forever")
			version := quota.ResourceVersion
			reconcileProfile("counts-withdraw-profile")
			Expect(getQuota(ns).ResourceVersion).To(Equal(version))
		})
	})

	Describe("refusing to touch resources it does not own", func() {
		It("refuses a pre-existing namespace and provisions nothing into it", func() {
			// The safety property that matters most. A namespace somebody else
			// created must not be seized, and nothing may be written into it.
			ns := unique("foreign-ns")
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   ns,
					Labels: map[string]string{"owner": "another-team"},
				},
			})).To(Succeed())

			createProfile("foreign-ns-profile", fullSpec(ns))
			reconcileProfile("foreign-ns-profile")

			By("leaving the namespace unlabelled by this operator")
			namespace := getNamespace(ns)
			Expect(namespace.Labels).NotTo(HaveKey(platformv1alpha1.ManagedByLabel))
			Expect(namespace.Labels).NotTo(HaveKey(platformv1alpha1.PSAEnforceLabel))
			Expect(namespace.Labels).To(HaveKeyWithValue("owner", "another-team"))

			By("creating no quota or limit range inside it")
			Expect(k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns, Name: platformv1alpha1.ResourceQuotaName,
			}, &corev1.ResourceQuota{})).To(MatchError(ContainSubstring("not found")))
			Expect(k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns, Name: platformv1alpha1.LimitRangeName,
			}, &corev1.LimitRange{})).To(MatchError(ContainSubstring("not found")))
		})

		It("refuses to modify a foreign object sitting at its deterministic name", func() {
			ns := unique("foreign-quota")
			createProfile("foreign-quota-profile", platformv1alpha1.NamespaceProfileSpec{
				Namespace: ns,
			})
			reconcileProfile("foreign-quota-profile")

			By("planting an unlabelled quota at the operator's chosen name")
			foreign := &corev1.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:      platformv1alpha1.ResourceQuotaName,
					Namespace: ns,
					Labels:    map[string]string{"app.kubernetes.io/managed-by": "Helm"},
				},
				Spec: corev1.ResourceQuotaSpec{
					Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("999")},
				},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			plantedVersion := getQuota(ns).ResourceVersion

			By("then asking the profile to manage a quota")
			var profile platformv1alpha1.NamespaceProfile
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foreign-quota-profile"}, &profile)).To(Succeed())
			profile.Spec.Quota = &platformv1alpha1.QuotaSpec{Pods: ptr.To(int32(10))}
			Expect(k8sClient.Update(ctx, &profile)).To(Succeed())

			reconcileProfile("foreign-quota-profile")

			By("leaving the foreign object exactly as it was")
			after := getQuota(ns)
			Expect(after.ResourceVersion).To(Equal(plantedVersion))
			Expect(after.Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourcePods, resource.MustParse("999")))
		})

		It("does not delete a foreign object when the profile withdraws its own", func() {
			// Withdrawing spec.quota must never delete a quota the operator did
			// not create, even though it occupies the name the operator would
			// have used.
			ns := unique("foreign-withdraw")
			createProfile("foreign-withdraw-profile", platformv1alpha1.NamespaceProfileSpec{
				Namespace: ns,
			})
			reconcileProfile("foreign-withdraw-profile")

			Expect(k8sClient.Create(ctx, &corev1.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name:      platformv1alpha1.ResourceQuotaName,
					Namespace: ns,
				},
				Spec: corev1.ResourceQuotaSpec{
					Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("7")},
				},
			})).To(Succeed())

			reconcileProfile("foreign-withdraw-profile")

			Expect(getQuota(ns).Spec.Hard).To(HaveKeyWithValue(
				corev1.ResourcePods, resource.MustParse("7")))
		})
	})

	Describe("network policies", func() {
		It("generates one policy per concern the profile declares", func() {
			ns := unique("netpol")
			createProfile("netpol-profile", fullSpec(ns))

			reconcileProfile("netpol-profile")

			Expect(listPolicyNames(ns)).To(ConsistOf(
				platformv1alpha1.DenyIngressPolicyName,
				platformv1alpha1.DenyEgressPolicyName,
				platformv1alpha1.AllowDNSPolicyName,
				platformv1alpha1.AllowSameNamespacePolicyName,
			))

			By("restricting only the directions the profile denies")
			denyIngress := getPolicy(ns, platformv1alpha1.DenyIngressPolicyName)
			Expect(denyIngress.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress))
			Expect(denyIngress.Spec.Ingress).To(BeEmpty())

			By("permitting DNS out of the namespace")
			dns := getPolicy(ns, platformv1alpha1.AllowDNSPolicyName)
			Expect(dns.Spec.Egress).To(HaveLen(1))
			Expect(dns.Spec.Egress[0].Ports).To(HaveLen(2))

			By("labelling every policy as the operator's")
			for _, name := range listPolicyNames(ns) {
				Expect(getPolicy(ns, name).Labels).To(HaveKeyWithValue(
					platformv1alpha1.ManagedByLabel, platformv1alpha1.OperatorName))
			}
		})

		It("manages no policies when the profile omits the network section", func() {
			// Omitting the section means the operator manages nothing of that
			// kind, so a policy written by somebody else must survive.
			ns := unique("netpol-none")
			createProfile("netpol-none-profile", platformv1alpha1.NamespaceProfileSpec{Namespace: ns})
			reconcileProfile("netpol-none-profile")

			Expect(k8sClient.Create(ctx, &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "team-written", Namespace: ns},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				},
			})).To(Succeed())

			reconcileProfile("netpol-none-profile")

			Expect(listPolicyNames(ns)).To(ConsistOf("team-written"))
		})

		It("prunes a policy the profile stops declaring", func() {
			// The set of policies is derived from the spec, so turning off a
			// toggle must withdraw the policies it produced. This is why
			// policies need pruning where the quota and limit range do not.
			ns := unique("netpol-prune")
			createProfile("netpol-prune-profile", fullSpec(ns))
			reconcileProfile("netpol-prune-profile")
			Expect(listPolicyNames(ns)).To(HaveLen(4))

			updateProfile("netpol-prune-profile", func(p *platformv1alpha1.NamespaceProfile) {
				p.Spec.Network.DefaultDenyEgress = ptr.To(false)
			})
			reconcileProfile("netpol-prune-profile")

			By("removing the egress deny and the DNS allowance that depended on it")
			Expect(listPolicyNames(ns)).To(ConsistOf(
				platformv1alpha1.DenyIngressPolicyName,
				platformv1alpha1.AllowSameNamespacePolicyName,
			))

			By("narrowing the same-namespace allowance to the one direction still denied")
			sameNamespace := getPolicy(ns, platformv1alpha1.AllowSameNamespacePolicyName)
			Expect(sameNamespace.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress))
			Expect(sameNamespace.Spec.Egress).To(BeEmpty(),
				"an egress rule would restrict egress the profile no longer denies")
		})

		It("never prunes a policy it does not own", func() {
			ns := unique("netpol-foreign")
			createProfile("netpol-foreign-profile", fullSpec(ns))
			reconcileProfile("netpol-foreign-profile")

			Expect(k8sClient.Create(ctx, &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: ns},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				},
			})).To(Succeed())

			reconcileProfile("netpol-foreign-profile")

			Expect(listPolicyNames(ns)).To(ContainElement("someone-elses"))
		})

		It("restores a widened policy", func() {
			// The drift that actually matters here: somebody opens a
			// default-deny namespace to the whole cluster.
			ns := unique("netpol-drift")
			createProfile("netpol-drift-profile", fullSpec(ns))
			reconcileProfile("netpol-drift-profile")

			policy := getPolicy(ns, platformv1alpha1.DenyIngressPolicyName)
			policy.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{},
				}},
			}}
			Expect(k8sClient.Update(ctx, policy)).To(Succeed())
			Expect(getPolicy(ns, platformv1alpha1.DenyIngressPolicyName).Spec.Ingress).To(HaveLen(1))

			reconcileProfile("netpol-drift-profile")

			Expect(getPolicy(ns, platformv1alpha1.DenyIngressPolicyName).Spec.Ingress).To(BeEmpty())
		})

		It("recreates a deleted policy", func() {
			ns := unique("netpol-deleted")
			createProfile("netpol-deleted-profile", fullSpec(ns))
			reconcileProfile("netpol-deleted-profile")

			Expect(k8sClient.Delete(ctx, getPolicy(ns, platformv1alpha1.AllowDNSPolicyName))).To(Succeed())

			reconcileProfile("netpol-deleted-profile")

			Expect(getPolicy(ns, platformv1alpha1.AllowDNSPolicyName).Spec.Egress).To(HaveLen(1))
		})
	})

	Describe("status reporting", func() {
		It("reports a fully provisioned profile as Ready and compliant", func() {
			ns := unique("status-ready")
			createProfile("status-ready-profile", fullSpec(ns))

			reconcileProfile("status-ready-profile")

			profile := getProfile("status-ready-profile")
			Expect(profile.Status.ObservedGeneration).To(Equal(profile.Generation))
			Expect(profile.Status.TargetNamespace).To(Equal(ns))
			Expect(profile.Status.Phase).To(Equal(platformv1alpha1.ProfilePhaseReady))

			Expect(conditionOf(profile, platformv1alpha1.ConditionReady)).
				To(HaveField("Status", metav1.ConditionTrue))
			Expect(conditionOf(profile, platformv1alpha1.ConditionPolicyCompliant)).
				To(HaveField("Status", metav1.ConditionTrue))
			Expect(conditionOf(profile, platformv1alpha1.ConditionProgressing)).
				To(HaveField("Status", metav1.ConditionFalse))
			Expect(conditionOf(profile, platformv1alpha1.ConditionDegraded)).
				To(HaveField("Status", metav1.ConditionFalse))

			By("listing every resource it governs")
			Expect(profile.Status.ManagedResources).To(ConsistOf(
				HaveField("Kind", "Namespace"),
				HaveField("Kind", "ResourceQuota"),
				HaveField("Kind", "LimitRange"),
				HaveField("Name", platformv1alpha1.DenyIngressPolicyName),
				HaveField("Name", platformv1alpha1.DenyEgressPolicyName),
				HaveField("Name", platformv1alpha1.AllowDNSPolicyName),
				HaveField("Name", platformv1alpha1.AllowSameNamespacePolicyName),
			))
			for _, resource := range profile.Status.ManagedResources {
				Expect(resource.State).To(Equal(platformv1alpha1.ManagedResourceInSync),
					"expected %s %s to be in sync", resource.Kind, resource.Name)
			}

			By("stamping the conditions with the generation they were computed from")
			Expect(conditionOf(profile, platformv1alpha1.ConditionReady).ObservedGeneration).
				To(Equal(profile.Generation))

			By("announcing readiness as an Event")
			Expect(drainEvents()).To(ContainElement(ContainSubstring(
				platformv1alpha1.ReasonProvisioned)))
		})

		It("reports a resource the profile does not declare as NotManaged, and stays Ready", func() {
			// A profile that asks for no quota is correctly configured when no
			// quota exists. Treating that absence as a shortfall would leave
			// such a profile permanently not-Ready.
			ns := unique("status-partial")
			createProfile("status-partial-profile", platformv1alpha1.NamespaceProfileSpec{
				Namespace: ns,
				LimitRange: &platformv1alpha1.LimitRangeSpec{
					DefaultRequest: &platformv1alpha1.ComputeResources{CPU: quantity("50m")},
				},
			})

			reconcileProfile("status-partial-profile")

			profile := getProfile("status-partial-profile")
			Expect(profile.Status.Phase).To(Equal(platformv1alpha1.ProfilePhaseReady))
			Expect(conditionOf(profile, platformv1alpha1.ConditionReady)).
				To(HaveField("Status", metav1.ConditionTrue))
			Expect(profile.Status.ManagedResources).To(ContainElement(SatisfyAll(
				HaveField("Kind", "ResourceQuota"),
				HaveField("State", platformv1alpha1.ManagedResourceNotManaged),
			)))
		})

		It("reports a namespace conflict as Conflict but not Degraded", func() {
			// The distinction matters operationally: declining to seize a
			// namespace is the operator working as designed, so it must not
			// look like a failure. Conflating the two would page somebody for
			// correct behaviour.
			ns := unique("status-conflict")
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})).To(Succeed())

			createProfile("status-conflict-profile", fullSpec(ns))
			reconcileProfile("status-conflict-profile")

			profile := getProfile("status-conflict-profile")
			Expect(profile.Status.Phase).To(Equal(platformv1alpha1.ProfilePhaseConflict))

			ready := conditionOf(profile, platformv1alpha1.ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(platformv1alpha1.ReasonNamespaceNotAdopted))
			Expect(ready.Message).To(ContainSubstring("spec.adoptExisting"))

			Expect(conditionOf(profile, platformv1alpha1.ConditionDegraded)).
				To(HaveField("Status", metav1.ConditionFalse))
			Expect(conditionOf(profile, platformv1alpha1.ConditionPolicyCompliant)).
				To(HaveField("Status", metav1.ConditionFalse))
			Expect(conditionOf(profile, platformv1alpha1.ConditionProgressing)).
				To(HaveField("Status", metav1.ConditionFalse))

			By("not provisioning anything into the namespace")
			Expect(profile.Status.ManagedResources).To(HaveLen(1))
			Expect(profile.Status.ManagedResources[0].State).
				To(Equal(platformv1alpha1.ManagedResourceConflict))

			By("warning about it once")
			Expect(drainEvents()).To(ContainElement(SatisfyAll(
				ContainSubstring("Warning"),
				ContainSubstring(platformv1alpha1.ReasonNamespaceNotAdopted),
			)))
		})

		It("writes status once and then leaves it alone", func() {
			// The loop this prevents: an unconditional status write produces a
			// watch event, which triggers a reconciliation, which writes status
			// again. Both halves of the defence are needed — this check, and
			// the generation predicate in SetupWithManager.
			ns := unique("status-idempotent")
			createProfile("status-idempotent-profile", fullSpec(ns))

			reconcileProfile("status-idempotent-profile")
			version := getProfile("status-idempotent-profile").ResourceVersion

			for range 5 {
				reconcileProfile("status-idempotent-profile")
			}

			Expect(getProfile("status-idempotent-profile").ResourceVersion).To(Equal(version))
		})

		It("records when it last had to correct drift", func() {
			// After a correction every other status field is identical, because
			// the resource now matches again. This timestamp is the only trace
			// that something is fighting the operator.
			ns := unique("status-drift")
			createProfile("status-drift-profile", fullSpec(ns))
			reconcileProfile("status-drift-profile")
			Expect(getProfile("status-drift-profile").Status.LastDriftCorrectionTime).To(BeNil())

			quota := getQuota(ns)
			quota.Spec.Hard[corev1.ResourceRequestsCPU] = resource.MustParse("20")
			Expect(k8sClient.Update(ctx, quota)).To(Succeed())
			drainEvents()

			reconcileProfile("status-drift-profile")

			profile := getProfile("status-drift-profile")
			Expect(profile.Status.LastDriftCorrectionTime).NotTo(BeNil())
			Expect(conditionOf(profile, platformv1alpha1.ConditionReady)).
				To(HaveField("Status", metav1.ConditionTrue),
					"the resource matches again once corrected")

			By("announcing the correction, which conditions alone cannot convey")
			Expect(drainEvents()).To(ContainElement(SatisfyAll(
				ContainSubstring(platformv1alpha1.ReasonDriftCorrected),
				ContainSubstring("ResourceQuota"),
			)))
		})
	})

	Describe("recovering without state", func() {
		It("converges from a half-provisioned namespace", func() {
			// Reconcile may be interrupted at any point and restarted from the
			// top, so it must never assume a previous pass finished. Simulated
			// here by provisioning, deleting one resource, and reconciling
			// again — the same situation a controller restart produces.
			ns := unique("partial")
			createProfile("partial-profile", fullSpec(ns))
			reconcileProfile("partial-profile")

			Expect(k8sClient.Delete(ctx, getLimitRange(ns))).To(Succeed())

			By("reconciling with a fresh reconciler, as a restarted process would")
			restarted := &NamespaceProfileReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := restarted.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "partial-profile"},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(getLimitRange(ns).Spec.Limits).To(HaveLen(1))
			Expect(getQuota(ns).Spec.Hard).To(HaveKey(corev1.ResourcePods))
		})

		It("does nothing for a profile that no longer exists", func() {
			// The request carries only a name, and the object may already be
			// gone by the time the worker gets to it. That is ordinary, not an
			// error.
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "never-existed"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
		})
	})
})
