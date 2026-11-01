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
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

// These specs exercise the CRD's OpenAPI schema and CEL validation rules
// against a real API server, which is the only place they actually run.
// Nothing here involves the controller: the point is that an invalid or
// self-contradictory profile is rejected on write, before any reconciliation
// could act on it.
//
// envtest gives us a real kube-apiserver and etcd with no kubelet, scheduler
// or controller-manager. That is exactly the right scope for schema tests —
// admission and validation are apiserver concerns — and it is also why the CRD
// failing to install at all would surface here as a suite-wide failure: the API
// server compiles every CEL expression when the CRD is created.

// quantity is a terser resource.MustParse for building test fixtures.
func quantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

var _ = Describe("NamespaceProfile CRD schema", func() {
	// deleteProfile removes a profile if the spec under test managed to create
	// one, so entries cannot collide through a shared namespace name.
	deleteProfile := func(p *platformv1alpha1.NamespaceProfile) {
		_ = k8sClient.Delete(ctx, p)
	}

	Describe("the shipped samples", func() {
		// Validating the samples themselves keeps documentation and schema from
		// drifting apart: a sample that stops matching the API fails the build
		// rather than failing a user who copies it.
		sampleFiles := []string{
			"platform_v1alpha1_namespaceprofile.yaml",
			"platform_v1alpha1_namespaceprofile_minimal.yaml",
			"platform_v1alpha1_namespaceprofile_adopt.yaml",
			"platform_v1alpha1_namespaceprofile_gpu.yaml",
		}

		for _, name := range sampleFiles {
			It("accepts config/samples/"+name, func() {
				raw, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", name))
				Expect(err).NotTo(HaveOccurred())

				profile := &platformv1alpha1.NamespaceProfile{}
				Expect(yaml.UnmarshalStrict(raw, profile)).To(Succeed())

				Expect(k8sClient.Create(ctx, profile)).To(Succeed())
				DeferCleanup(deleteProfile, profile)
			})
		}
	})

	Describe("defaulting", func() {
		It("fills in the documented defaults from the schema alone", func() {
			// Only the required field is set. Everything else must come back
			// populated by the API server, which is what makes the schema the
			// single definition of what "unset" means.
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "defaults-profile"},
				Spec: platformv1alpha1.NamespaceProfileSpec{
					Namespace: "defaults-ns",
					Network:   &platformv1alpha1.NetworkSpec{},
					Security: &platformv1alpha1.SecuritySpec{
						PodSecurityStandard: platformv1alpha1.PodSecurityBaseline,
					},
					Lifecycle: &platformv1alpha1.LifecycleSpec{},
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			DeferCleanup(deleteProfile, profile)

			By("defaulting adoption to refusing pre-existing namespaces")
			Expect(profile.Spec.AdoptExisting).To(HaveValue(BeFalse()))

			By("defaulting the network fence to deny-ingress but not deny-egress")
			Expect(profile.Spec.Network.DefaultDenyIngress).To(HaveValue(BeTrue()))
			Expect(profile.Spec.Network.DefaultDenyEgress).To(HaveValue(BeFalse()))
			Expect(profile.Spec.Network.AllowDNS).To(HaveValue(BeTrue()))
			Expect(profile.Spec.Network.AllowSameNamespace).To(HaveValue(BeTrue()))

			By("materialising the dns block and then defaulting its children")
			// The dns field defaults to {} and its children carry their own
			// defaults. This asserts the API server applies both levels in one
			// write, which is what lets the controller read the DNS assumptions
			// off the object instead of re-deriving them in Go.
			Expect(profile.Spec.Network.DNS).NotTo(BeNil())
			Expect(profile.Spec.Network.DNS.Namespace).To(Equal("kube-system"))
			Expect(profile.Spec.Network.DNS.PodSelector).NotTo(BeNil())
			Expect(profile.Spec.Network.DNS.PodSelector.MatchLabels).
				To(HaveKeyWithValue("k8s-app", "kube-dns"))
			Expect(profile.Spec.Network.DNS.Ports).To(ConsistOf(
				platformv1alpha1.NetworkPort{Protocol: corev1.ProtocolUDP, Port: 53},
				platformv1alpha1.NetworkPort{Protocol: corev1.ProtocolTCP, Port: 53},
			))

			By("defaulting the Pod Security Standards version to latest")
			Expect(profile.Spec.Security.EnforceVersion).To(Equal("latest"))

			By("defaulting the lifecycle to retaining the namespace")
			Expect(profile.Spec.Lifecycle.DeleteNamespaceOnRemoval).To(HaveValue(BeFalse()))
			Expect(profile.Spec.Lifecycle.RetainManagedResources).To(HaveValue(BeFalse()))

			By("agreeing with the Go accessors used for hand-built specs")
			// The accessors in api/v1alpha1/defaults.go restate each default so
			// that a spec built in Go, which never passes through the API
			// server, behaves identically. Comparing the server's answer
			// against the accessor's nil fallback is what stops a marker and
			// its accessor from drifting apart unnoticed.
			unset := &platformv1alpha1.NamespaceProfileSpec{}
			Expect(unset.AdoptionAllowed()).To(Equal(*profile.Spec.AdoptExisting))

			unsetNetwork := &platformv1alpha1.NetworkSpec{}
			Expect(unsetNetwork.DenyIngressEnabled()).
				To(Equal(*profile.Spec.Network.DefaultDenyIngress))
			Expect(unsetNetwork.DenyEgressEnabled()).
				To(Equal(*profile.Spec.Network.DefaultDenyEgress))
			Expect(unsetNetwork.DNSAllowed()).To(Equal(*profile.Spec.Network.AllowDNS))
			Expect(unsetNetwork.SameNamespaceAllowed()).
				To(Equal(*profile.Spec.Network.AllowSameNamespace))

			unsetLifecycle := &platformv1alpha1.LifecycleSpec{}
			Expect(unsetLifecycle.NamespaceDeletionRequested()).
				To(Equal(*profile.Spec.Lifecycle.DeleteNamespaceOnRemoval))
			Expect(unsetLifecycle.ManagedResourcesRetained()).
				To(Equal(*profile.Spec.Lifecycle.RetainManagedResources))
		})

		It("leaves audit and warn unset so they follow the enforced level", func() {
			// These two cannot be schema defaults, because their fallback is a
			// sibling field's value rather than a constant. The accessors in
			// api/v1alpha1/defaults.go are the single definition instead.
			security := &platformv1alpha1.SecuritySpec{
				PodSecurityStandard: platformv1alpha1.PodSecurityRestricted,
			}
			Expect(security.Audit).To(BeNil())
			Expect(security.AuditLevel()).To(Equal(platformv1alpha1.PodSecurityRestricted))
			Expect(security.WarnLevel()).To(Equal(platformv1alpha1.PodSecurityRestricted))

			security.Audit = ptr.To(platformv1alpha1.PodSecurityBaseline)
			Expect(security.AuditLevel()).To(Equal(platformv1alpha1.PodSecurityBaseline))
			Expect(security.WarnLevel()).To(Equal(platformv1alpha1.PodSecurityRestricted))
		})
	})

	Describe("immutability of the target namespace", func() {
		It("rejects re-pointing a live profile at a different namespace", func() {
			// Re-pointing would abandon the first namespace's quota, limits and
			// network policies with nothing left to record that they were ever
			// managed, so the schema forbids it outright.
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "immutable-profile"},
				Spec:       platformv1alpha1.NamespaceProfileSpec{Namespace: "immutable-a"},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			DeferCleanup(deleteProfile, profile)

			profile.Spec.Namespace = "immutable-b"
			Expect(k8sClient.Update(ctx, profile)).
				To(MatchError(ContainSubstring("spec.namespace is immutable")))
		})
	})

	Describe("rejecting invalid profiles", func() {
		DescribeTable("the API server refuses the write",
			func(spec platformv1alpha1.NamespaceProfileSpec, wantMessage string) {
				profile := &platformv1alpha1.NamespaceProfile{
					ObjectMeta: metav1.ObjectMeta{GenerateName: "invalid-"},
					Spec:       spec,
				}
				err := k8sClient.Create(ctx, profile)
				DeferCleanup(deleteProfile, profile)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(wantMessage))
			},

			// --- the target namespace ---

			Entry("an empty namespace name",
				platformv1alpha1.NamespaceProfileSpec{Namespace: ""},
				"should be at least 1 chars long"),

			Entry("a namespace name that is not a DNS label",
				platformv1alpha1.NamespaceProfileSpec{Namespace: "Payments"},
				"should match"),

			Entry("a kube-prefixed namespace, where a default-deny policy or a "+
				"restricted level could disable the cluster",
				platformv1alpha1.NamespaceProfileSpec{Namespace: "kube-system"},
				"must not target a reserved namespace"),

			Entry("the default namespace",
				platformv1alpha1.NamespaceProfileSpec{Namespace: "default"},
				"must not target a reserved namespace"),

			// --- contradictory network configuration ---

			Entry("default-deny egress with DNS disabled, which would break all "+
				"name resolution in the namespace",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "netconflict",
					Network: &platformv1alpha1.NetworkSpec{
						DefaultDenyEgress: ptr.To(true),
						AllowDNS:          ptr.To(false),
					},
				},
				"blocks all DNS resolution"),

			Entry("an API server endpoint that is not a CIDR",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "bad-kube-api",
					Network: &platformv1alpha1.NetworkSpec{
						DefaultDenyEgress: ptr.To(true),
						AllowKubeAPI: &platformv1alpha1.KubeAPIEgressSpec{
							Endpoints: []string{"10.0.0.1"},
						},
					},
				},
				"endpoints must be CIDRs"),

			Entry("an empty API server endpoint list, which would permit nothing",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "empty-kube-api",
					Network: &platformv1alpha1.NetworkSpec{
						DefaultDenyEgress: ptr.To(true),
						AllowKubeAPI:      &platformv1alpha1.KubeAPIEgressSpec{Endpoints: []string{}},
					},
				},
				"should have at least 1 items"),

			Entry("a cross-namespace allowance naming something that is not a namespace",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "bad-ns-allowance",
					Network: &platformv1alpha1.NetworkSpec{
						DefaultDenyEgress:       ptr.To(true),
						AllowEgressToNamespaces: []string{"Platform_Shared"},
					},
				},
				"should match"),

			// --- contradictory lifecycle configuration ---

			Entry("retaining managed resources while deleting the namespace that "+
				"contains them",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "lifecycleconflict",
					Lifecycle: &platformv1alpha1.LifecycleSpec{
						DeleteNamespaceOnRemoval: ptr.To(true),
						RetainManagedResources:   ptr.To(true),
					},
				},
				"retainManagedResources is meaningless"),

			// --- limit range ordering ---

			Entry("a minimum above the maximum",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "lr-min-max",
					LimitRange: &platformv1alpha1.LimitRangeSpec{
						Min: &platformv1alpha1.ComputeResources{CPU: quantity("2")},
						Max: &platformv1alpha1.ComputeResources{CPU: quantity("1")},
					},
				},
				"limitRange.min must not exceed limitRange.max"),

			Entry("a memory minimum above the maximum, checked independently of cpu",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "lr-min-max-mem",
					LimitRange: &platformv1alpha1.LimitRangeSpec{
						Min: &platformv1alpha1.ComputeResources{
							CPU:    quantity("1"),
							Memory: quantity("4Gi"),
						},
						Max: &platformv1alpha1.ComputeResources{
							CPU:    quantity("2"),
							Memory: quantity("1Gi"),
						},
					},
				},
				"limitRange.min must not exceed limitRange.max"),

			Entry("a default request above the default limit",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "lr-req-default",
					LimitRange: &platformv1alpha1.LimitRangeSpec{
						DefaultRequest: &platformv1alpha1.ComputeResources{CPU: quantity("2")},
						Default:        &platformv1alpha1.ComputeResources{CPU: quantity("1")},
					},
				},
				"limitRange.defaultRequest must not exceed limitRange.default"),

			Entry("a default limit above the maximum",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "lr-default-max",
					LimitRange: &platformv1alpha1.LimitRangeSpec{
						Default: &platformv1alpha1.ComputeResources{Memory: quantity("16Gi")},
						Max:     &platformv1alpha1.ComputeResources{Memory: quantity("8Gi")},
					},
				},
				"limitRange.default must not exceed limitRange.max"),

			Entry("a minimum above the default request",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "lr-min-req",
					LimitRange: &platformv1alpha1.LimitRangeSpec{
						Min:            &platformv1alpha1.ComputeResources{CPU: quantity("500m")},
						DefaultRequest: &platformv1alpha1.ComputeResources{CPU: quantity("100m")},
					},
				},
				"limitRange.min must not exceed limitRange.defaultRequest"),

			// --- quantities ---

			Entry("a zero cpu quantity, which would quota the namespace to nothing",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "zero-cpu",
					Quota: &platformv1alpha1.QuotaSpec{
						Requests: &platformv1alpha1.ComputeResources{CPU: quantity("0")},
					},
				},
				"cpu must be greater than zero"),

			Entry("a negative memory quantity",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "negative-memory",
					Quota: &platformv1alpha1.QuotaSpec{
						Limits: &platformv1alpha1.ComputeResources{Memory: quantity("-1Gi")},
					},
				},
				"memory must be greater than zero"),

			Entry("an empty compute resources block, which states nothing",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "empty-requests",
					Quota: &platformv1alpha1.QuotaSpec{
						Requests: &platformv1alpha1.ComputeResources{},
					},
				},
				"should have at least 1 properties"),

			Entry("a negative pod count",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "negative-pods",
					Quota:     &platformv1alpha1.QuotaSpec{Pods: ptr.To(int32(-1))},
				},
				"should be greater than or equal to 0"),

			Entry("a negative persistent volume claim count",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "negative-pvcs",
					Quota: &platformv1alpha1.QuotaSpec{
						Storage: &platformv1alpha1.StorageQuotaSpec{
							PersistentVolumeClaims: ptr.To(int32(-5)),
						},
					},
				},
				"should be greater than or equal to 0"),

			// --- enumerations and patterns ---

			Entry("an ephemeral storage minimum above the maximum, checked "+
				"independently of cpu and memory",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "lr-min-max-ephemeral",
					LimitRange: &platformv1alpha1.LimitRangeSpec{
						Min: &platformv1alpha1.ComputeResources{
							CPU:              quantity("1"),
							EphemeralStorage: quantity("20Gi"),
						},
						Max: &platformv1alpha1.ComputeResources{
							CPU:              quantity("2"),
							EphemeralStorage: quantity("1Gi"),
						},
					},
				},
				"limitRange.min must not exceed limitRange.max"),

			Entry("a zero ephemeral storage quota, which would deny all node disk",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "zero-ephemeral",
					Quota: &platformv1alpha1.QuotaSpec{
						Requests: &platformv1alpha1.ComputeResources{
							EphemeralStorage: quantity("0"),
						},
					},
				},
				"ephemeralStorage must be greater than zero"),

			// --- extended resources ---

			Entry("an unqualified extended resource name",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "bad-extended",
					Quota: &platformv1alpha1.QuotaSpec{
						ExtendedResources: map[string]resource.Quantity{
							"gpu": resource.MustParse("4"),
						},
					},
				},
				"must be a vendor-qualified resource name"),

			Entry("an extended resource key carrying the prefix the operator adds",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "extended-prefixed",
					Quota: &platformv1alpha1.QuotaSpec{
						ExtendedResources: map[string]resource.Quantity{
							"requests.nvidia.com/gpu": resource.MustParse("4"),
						},
					},
				},
				"must not carry a requests. or limits. prefix"),

			Entry("an extended resource in the reserved kubernetes.io namespace",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "extended-reserved",
					Quota: &platformv1alpha1.QuotaSpec{
						ExtendedResources: map[string]resource.Quantity{
							"kubernetes.io/gpu": resource.MustParse("4"),
						},
					},
				},
				"reserved"),

			Entry("a negative extended resource quota",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "extended-negative",
					Quota: &platformv1alpha1.QuotaSpec{
						ExtendedResources: map[string]resource.Quantity{
							"nvidia.com/gpu": resource.MustParse("-1"),
						},
					},
				},
				"must not be negative"),

			// --- object counts ---

			Entry("an object-count key that is not a valid quota resource name",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "bad-count-key",
					Quota: &platformv1alpha1.QuotaSpec{
						ObjectCounts: map[string]int32{"Deployments": 5},
					},
				},
				"must be a Kubernetes object-count quota name"),

			Entry("an object-count key missing the count/ prefix for a grouped resource",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "bad-count-prefix",
					Quota: &platformv1alpha1.QuotaSpec{
						ObjectCounts: map[string]int32{"deployments.apps": 5},
					},
				},
				"must be a Kubernetes object-count quota name"),

			Entry("a pod cap expressed through objectCounts rather than its own field",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "count-collision-pods",
					Quota: &platformv1alpha1.QuotaSpec{
						ObjectCounts: map[string]int32{"pods": 50},
					},
				},
				"use spec.quota.pods and spec.quota.storage.persistentVolumeClaims"),

			Entry("a pvc cap expressed through objectCounts rather than its own field",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "count-collision-pvcs",
					Quota: &platformv1alpha1.QuotaSpec{
						ObjectCounts: map[string]int32{"persistentvolumeclaims": 20},
					},
				},
				"use spec.quota.pods and spec.quota.storage.persistentVolumeClaims"),

			Entry("a negative object count",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "negative-count",
					Quota: &platformv1alpha1.QuotaSpec{
						ObjectCounts: map[string]int32{"services": -1},
					},
				},
				"objectCounts values must not be negative"),

			// --- enumerations and patterns ---

			Entry("an unknown Pod Security Standards level",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "bad-psa",
					Security: &platformv1alpha1.SecuritySpec{
						PodSecurityStandard: platformv1alpha1.PodSecurityLevel("paranoid"),
					},
				},
				"Unsupported value"),

			Entry("a Pod Security Standards version that is not latest or a minor",
				platformv1alpha1.NamespaceProfileSpec{
					Namespace: "bad-psa-version",
					Security: &platformv1alpha1.SecuritySpec{
						PodSecurityStandard: platformv1alpha1.PodSecurityBaseline,
						EnforceVersion:      "v2",
					},
				},
				"should match"),
		)

		It("rejects two quota entries for the same StorageClass", func() {
			// classes is a map-typed list keyed on name, so the API server
			// rejects duplicates for us. Without that, two entries for one class
			// would render two conflicting hard limits under the same key and
			// whichever came last would silently win.
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "dup-class-profile"},
				Spec: platformv1alpha1.NamespaceProfileSpec{
					Namespace: "dup-class",
					Quota: &platformv1alpha1.QuotaSpec{
						Storage: &platformv1alpha1.StorageQuotaSpec{
							Classes: []platformv1alpha1.StorageClassQuota{
								{Name: "gp3", Requests: quantity("100Gi")},
								{Name: "gp3", Requests: quantity("200Gi")},
							},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, profile)
			DeferCleanup(deleteProfile, profile)

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Duplicate value"))
		})
	})

	Describe("accepting valid edge cases", func() {
		It("accepts a profile that configures nothing but the namespace", func() {
			// Every section being optional is a deliberate part of the design:
			// this profile creates and labels a namespace and manages no policy
			// at all, which is a legitimate starting point for onboarding.
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "bare-profile"},
				Spec:       platformv1alpha1.NamespaceProfileSpec{Namespace: "bare-ns"},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			DeferCleanup(deleteProfile, profile)

			Expect(profile.Spec.Quota).To(BeNil())
			Expect(profile.Spec.LimitRange).To(BeNil())
			Expect(profile.Spec.Network).To(BeNil())
			Expect(profile.Spec.Security).To(BeNil())
		})

		It("accepts a fully fenced network configuration", func() {
			// The shape a real production tenant needs: denied by default in
			// both directions, with the four allowances that make that
			// survivable.
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "fenced-profile"},
				Spec: platformv1alpha1.NamespaceProfileSpec{
					Namespace: "fenced-ns",
					Network: &platformv1alpha1.NetworkSpec{
						DefaultDenyIngress: ptr.To(true),
						DefaultDenyEgress:  ptr.To(true),
						AllowDNS:           ptr.To(true),
						AllowSameNamespace: ptr.To(true),
						AllowKubeAPI: &platformv1alpha1.KubeAPIEgressSpec{
							Endpoints: []string{"10.0.0.1/32", "fd00::1/128"},
						},
						AllowEgressToNamespaces:    []string{"platform-shared", "observability"},
						AllowIngressFromNamespaces: []string{"ingress-nginx", "monitoring"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			DeferCleanup(deleteProfile, profile)

			By("defaulting the API server ports to TCP 443")
			Expect(profile.Spec.Network.AllowKubeAPI.Ports).To(ConsistOf(
				platformv1alpha1.NetworkPort{Protocol: corev1.ProtocolTCP, Port: 443},
			))
		})

		It("accepts every documented object-count key form", func() {
			// Both families Kubernetes supports: the nine shorthand names, and
			// the generic count/<resource>.<group> form that covers anything
			// else including custom resources.
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "object-counts-profile"},
				Spec: platformv1alpha1.NamespaceProfileSpec{
					Namespace: "object-counts-ns",
					Quota: &platformv1alpha1.QuotaSpec{
						ObjectCounts: map[string]int32{
							"services":                          10,
							"services.nodeports":                2,
							"services.loadbalancers":            0,
							"secrets":                           50,
							"configmaps":                        50,
							"replicationcontrollers":            5,
							"resourcequotas":                    1,
							"count/deployments.apps":            20,
							"count/cronjobs.batch":              5,
							"count/serviceaccounts":             10,
							"count/ingresses.networking.k8s.io": 3,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			DeferCleanup(deleteProfile, profile)
		})

		It("accepts both documented extended resource key forms", func() {
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "extended-profile"},
				Spec: platformv1alpha1.NamespaceProfileSpec{
					Namespace: "extended-ns",
					Quota: &platformv1alpha1.QuotaSpec{
						ExtendedResources: map[string]resource.Quantity{
							"nvidia.com/gpu":               resource.MustParse("8"),
							"amd.com/gpu":                  resource.MustParse("2"),
							"example.com/fpga-accelerator": resource.MustParse("1"),
							"hugepages-2Mi":                resource.MustParse("512Mi"),
							"hugepages-1Gi":                resource.MustParse("4Gi"),
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			DeferCleanup(deleteProfile, profile)
		})

		It("accepts a zero extended resource quota, forbidding the device outright", func() {
			// Unlike cpu or memory, zero is meaningful here: it keeps
			// accelerators out of a namespace that has no business asking for
			// them. The positivity rule used elsewhere would have made this
			// inexpressible.
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "no-gpu-profile"},
				Spec: platformv1alpha1.NamespaceProfileSpec{
					Namespace: "no-gpu-ns",
					Quota: &platformv1alpha1.QuotaSpec{
						ExtendedResources: map[string]resource.Quantity{
							"nvidia.com/gpu": resource.MustParse("0"),
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			DeferCleanup(deleteProfile, profile)
		})

		It("accepts a cap on its own custom resource", func() {
			// Nothing about the generic form is limited to built-in kinds,
			// which is why the key is a free-form string rather than an
			// enumeration: every CRD in a cluster adds another valid key.
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "crd-count-profile"},
				Spec: platformv1alpha1.NamespaceProfileSpec{
					Namespace: "crd-count-ns",
					Quota: &platformv1alpha1.QuotaSpec{
						ObjectCounts: map[string]int32{
							"count/namespaceprofiles.platform.nabeem.com": 1,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			DeferCleanup(deleteProfile, profile)
		})

		It("accepts a limit range whose bounds are exactly equal", func() {
			// The ordering rules use <=, so pinning every container to one exact
			// size is expressible. A strict < would have made that impossible.
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "pinned-profile"},
				Spec: platformv1alpha1.NamespaceProfileSpec{
					Namespace: "pinned-ns",
					LimitRange: &platformv1alpha1.LimitRangeSpec{
						Min:            &platformv1alpha1.ComputeResources{CPU: quantity("1")},
						DefaultRequest: &platformv1alpha1.ComputeResources{CPU: quantity("1")},
						Default:        &platformv1alpha1.ComputeResources{CPU: quantity("1")},
						Max:            &platformv1alpha1.ComputeResources{CPU: quantity("1")},
					},
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			DeferCleanup(deleteProfile, profile)
		})

		It("compares quantities by value, not by string", func() {
			// "1000m" and "1" are the same quantity. The CEL rules use the
			// quantity library rather than string comparison, so equivalent
			// spellings behave identically.
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "equivalent-profile"},
				Spec: platformv1alpha1.NamespaceProfileSpec{
					Namespace: "equivalent-ns",
					LimitRange: &platformv1alpha1.LimitRangeSpec{
						Min: &platformv1alpha1.ComputeResources{CPU: quantity("1000m")},
						Max: &platformv1alpha1.ComputeResources{CPU: quantity("1")},
					},
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			DeferCleanup(deleteProfile, profile)
		})

		It("accepts default-deny egress without restating allowDNS", func() {
			// The DNS contradiction rule reads a sibling boolean that the user
			// did not set. It holds because the API server applies schema
			// defaults while decoding, before CEL validation runs — so by the
			// time the rule is evaluated, allowDNS is present and true. If that
			// ordering ever changed, this would start failing instead of
			// quietly admitting a namespace with no DNS.
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "deny-egress-profile"},
				Spec: platformv1alpha1.NamespaceProfileSpec{
					Namespace: "deny-egress-ns",
					Network: &platformv1alpha1.NetworkSpec{
						DefaultDenyEgress: ptr.To(true),
					},
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			DeferCleanup(deleteProfile, profile)

			Expect(profile.Spec.Network.AllowDNS).To(HaveValue(BeTrue()))
		})

		It("accepts a pinned Pod Security Standards version", func() {
			profile := &platformv1alpha1.NamespaceProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "pinned-psa-profile"},
				Spec: platformv1alpha1.NamespaceProfileSpec{
					Namespace: "pinned-psa-ns",
					Security: &platformv1alpha1.SecuritySpec{
						PodSecurityStandard: platformv1alpha1.PodSecurityRestricted,
						EnforceVersion:      "v1.34",
					},
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			DeferCleanup(deleteProfile, profile)
		})
	})
})
