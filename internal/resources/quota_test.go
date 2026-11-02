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

package resources

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

// qty is a terser resource.MustParse for building fixtures.
func qty(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// profileWith returns a profile targeting "tenant" with the given spec tweaks
// applied, so each test states only what it cares about.
func profileWith(mutate func(*platformv1alpha1.NamespaceProfileSpec)) *platformv1alpha1.NamespaceProfile {
	profile := &platformv1alpha1.NamespaceProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-profile", Generation: 1},
		Spec:       platformv1alpha1.NamespaceProfileSpec{Namespace: "tenant"},
	}
	if mutate != nil {
		mutate(&profile.Spec)
	}
	return profile
}

func TestQuotaHardLimits(t *testing.T) {
	tests := []struct {
		name string
		spec *platformv1alpha1.QuotaSpec
		want corev1.ResourceList
	}{
		{
			name: "a nil quota section manages no quota at all",
			spec: nil,
			want: nil,
		},
		{
			// Reachable only from hand-built specs; the API's MinProperties
			// guard rejects "quota: {}" at admission.
			name: "an empty quota section produces no hard limits",
			spec: &platformv1alpha1.QuotaSpec{},
			want: nil,
		},
		{
			name: "compute requests map to the requests.* hard keys",
			spec: &platformv1alpha1.QuotaSpec{
				Requests: &platformv1alpha1.ComputeResources{CPU: qty("8"), Memory: qty("16Gi")},
			},
			want: corev1.ResourceList{
				corev1.ResourceRequestsCPU:    resource.MustParse("8"),
				corev1.ResourceRequestsMemory: resource.MustParse("16Gi"),
			},
		},
		{
			name: "compute limits map to the limits.* hard keys",
			spec: &platformv1alpha1.QuotaSpec{
				Limits: &platformv1alpha1.ComputeResources{CPU: qty("16"), Memory: qty("32Gi")},
			},
			want: corev1.ResourceList{
				corev1.ResourceLimitsCPU:    resource.MustParse("16"),
				corev1.ResourceLimitsMemory: resource.MustParse("32Gi"),
			},
		},
		{
			// A half-specified pair must not invent the other half. Defaulting
			// memory to anything here would silently cap a namespace the
			// profile never intended to constrain.
			name: "setting only cpu leaves memory unconstrained",
			spec: &platformv1alpha1.QuotaSpec{
				Requests: &platformv1alpha1.ComputeResources{CPU: qty("4")},
			},
			want: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("4"),
			},
		},
		{
			name: "the pod count becomes a decimal quantity",
			spec: &platformv1alpha1.QuotaSpec{Pods: ptr.To(int32(50))},
			want: corev1.ResourceList{
				corev1.ResourcePods: resource.MustParse("50"),
			},
		},
		{
			// Zero pods is a legitimate, if drastic, declaration: it quarantines
			// a namespace. It must not be confused with "unset".
			name: "a pod count of zero is honoured rather than treated as unset",
			spec: &platformv1alpha1.QuotaSpec{Pods: ptr.To(int32(0))},
			want: corev1.ResourceList{
				corev1.ResourcePods: resource.MustParse("0"),
			},
		},
		{
			name: "cluster-wide storage maps to requests.storage and the pvc count",
			spec: &platformv1alpha1.QuotaSpec{
				Storage: &platformv1alpha1.StorageQuotaSpec{
					Requests:               qty("500Gi"),
					PersistentVolumeClaims: ptr.To(int32(20)),
				},
			},
			want: corev1.ResourceList{
				corev1.ResourceRequestsStorage:        resource.MustParse("500Gi"),
				corev1.ResourcePersistentVolumeClaims: resource.MustParse("20"),
			},
		},
		{
			// The literal key spellings are the contract with the API server.
			// They are asserted as strings rather than built from the suffix
			// constants, so a typo in those constants cannot pass by agreeing
			// with itself.
			name: "per-StorageClass entries produce the suffixed hard keys",
			spec: &platformv1alpha1.QuotaSpec{
				Storage: &platformv1alpha1.StorageQuotaSpec{
					Classes: []platformv1alpha1.StorageClassQuota{{
						Name:                   "gp3",
						Requests:               qty("200Gi"),
						PersistentVolumeClaims: ptr.To(int32(10)),
					}},
				},
			},
			want: corev1.ResourceList{
				"gp3.storageclass.storage.k8s.io/requests.storage":       resource.MustParse("200Gi"),
				"gp3.storageclass.storage.k8s.io/persistentvolumeclaims": resource.MustParse("10"),
			},
		},
		{
			name: "several StorageClasses each get their own keys",
			spec: &platformv1alpha1.QuotaSpec{
				Storage: &platformv1alpha1.StorageQuotaSpec{
					Requests: qty("1Ti"),
					Classes: []platformv1alpha1.StorageClassQuota{
						{Name: "gp3", Requests: qty("600Gi")},
						{Name: "io2", Requests: qty("100Gi"), PersistentVolumeClaims: ptr.To(int32(4))},
					},
				},
			},
			want: corev1.ResourceList{
				corev1.ResourceRequestsStorage:                           resource.MustParse("1Ti"),
				"gp3.storageclass.storage.k8s.io/requests.storage":       resource.MustParse("600Gi"),
				"io2.storageclass.storage.k8s.io/requests.storage":       resource.MustParse("100Gi"),
				"io2.storageclass.storage.k8s.io/persistentvolumeclaims": resource.MustParse("4"),
			},
		},
		{
			// Node disk, the one compute resource whose exhaustion harms other
			// namespaces: a pod filling its node's disk gets pods evicted on
			// that node regardless of which namespace they belong to.
			name: "ephemeral storage maps to the requests and limits keys",
			spec: &platformv1alpha1.QuotaSpec{
				Requests: &platformv1alpha1.ComputeResources{EphemeralStorage: qty("20Gi")},
				Limits:   &platformv1alpha1.ComputeResources{EphemeralStorage: qty("40Gi")},
			},
			want: corev1.ResourceList{
				corev1.ResourceRequestsEphemeralStorage: resource.MustParse("20Gi"),
				corev1.ResourceLimitsEphemeralStorage:   resource.MustParse("40Gi"),
			},
		},
		{
			// Extended resources are quoted on the requests side only, because
			// Kubernetes requires them to be requested and limited at the same
			// value — so a request ceiling is also a limit ceiling.
			name: "extended resources gain the requests prefix",
			spec: &platformv1alpha1.QuotaSpec{
				ExtendedResources: map[string]resource.Quantity{
					"nvidia.com/gpu": resource.MustParse("4"),
					"amd.com/gpu":    resource.MustParse("2"),
					"hugepages-2Mi":  resource.MustParse("512Mi"),
				},
			},
			want: corev1.ResourceList{
				"requests.nvidia.com/gpu": resource.MustParse("4"),
				"requests.amd.com/gpu":    resource.MustParse("2"),
				"requests.hugepages-2Mi":  resource.MustParse("512Mi"),
			},
		},

		{
			// Object counts pass through under the key they are named for.
			// This caps how many objects may exist; it never touches the
			// objects themselves.
			name: "object counts pass through as named",
			spec: &platformv1alpha1.QuotaSpec{
				ObjectCounts: map[string]int32{
					"services":               10,
					"secrets":                50,
					"count/deployments.apps": 20,
					"count/serviceaccounts":  5,
				},
			},
			want: corev1.ResourceList{
				"services":               resource.MustParse("10"),
				"secrets":                resource.MustParse("50"),
				"count/deployments.apps": resource.MustParse("20"),
				"count/serviceaccounts":  resource.MustParse("5"),
			},
		},
		{
			// Zero forbids the kind outright, and must not be mistaken for
			// "unset", which would leave it unlimited.
			name: "a zero object count is honoured, forbidding the kind outright",
			spec: &platformv1alpha1.QuotaSpec{
				ObjectCounts: map[string]int32{"services.loadbalancers": 0},
			},
			want: corev1.ResourceList{
				"services.loadbalancers": resource.MustParse("0"),
			},
		},
		{
			name: "a fully populated quota produces every key together",
			spec: &platformv1alpha1.QuotaSpec{
				Requests: &platformv1alpha1.ComputeResources{
					CPU: qty("8"), Memory: qty("16Gi"), EphemeralStorage: qty("20Gi"),
				},
				Limits: &platformv1alpha1.ComputeResources{
					CPU: qty("16"), Memory: qty("32Gi"), EphemeralStorage: qty("40Gi"),
				},
				Pods: ptr.To(int32(50)),
				Storage: &platformv1alpha1.StorageQuotaSpec{
					Requests:               qty("500Gi"),
					PersistentVolumeClaims: ptr.To(int32(20)),
					Classes: []platformv1alpha1.StorageClassQuota{
						{Name: "fast-ssd", Requests: qty("200Gi"), PersistentVolumeClaims: ptr.To(int32(10))},
					},
				},
				ObjectCounts:      map[string]int32{"services": 10, "count/cronjobs.batch": 3},
				ExtendedResources: map[string]resource.Quantity{"nvidia.com/gpu": resource.MustParse("4")},
			},
			want: corev1.ResourceList{
				corev1.ResourceRequestsCPU:                                    resource.MustParse("8"),
				corev1.ResourceRequestsMemory:                                 resource.MustParse("16Gi"),
				corev1.ResourceLimitsCPU:                                      resource.MustParse("16"),
				corev1.ResourceLimitsMemory:                                   resource.MustParse("32Gi"),
				corev1.ResourcePods:                                           resource.MustParse("50"),
				corev1.ResourceRequestsStorage:                                resource.MustParse("500Gi"),
				corev1.ResourcePersistentVolumeClaims:                         resource.MustParse("20"),
				"fast-ssd.storageclass.storage.k8s.io/requests.storage":       resource.MustParse("200Gi"),
				"fast-ssd.storageclass.storage.k8s.io/persistentvolumeclaims": resource.MustParse("10"),
				"services":                              resource.MustParse("10"),
				"count/cronjobs.batch":                  resource.MustParse("3"),
				corev1.ResourceRequestsEphemeralStorage: resource.MustParse("20Gi"),
				corev1.ResourceLimitsEphemeralStorage:   resource.MustParse("40Gi"),
				"requests.nvidia.com/gpu":               resource.MustParse("4"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := QuotaHardLimits(tc.spec)
			if !apiequality.Semantic.DeepEqual(tc.want, got) {
				t.Errorf("QuotaHardLimits() mismatch\n  want: %v\n  got:  %v", tc.want, got)
			}
		})
	}
}

func TestQuotaHardLimitsIsDeterministic(t *testing.T) {
	// Generated resources must not vary between reconciliations, or the
	// operator would rewrite them forever. Map iteration order is random in
	// Go, so a builder that accumulated into a slice could silently produce a
	// different object each pass.
	spec := &platformv1alpha1.QuotaSpec{
		Requests: &platformv1alpha1.ComputeResources{CPU: qty("8"), Memory: qty("16Gi")},
		Pods:     ptr.To(int32(50)),
		Storage: &platformv1alpha1.StorageQuotaSpec{
			Classes: []platformv1alpha1.StorageClassQuota{
				{Name: "a", Requests: qty("1Gi")},
				{Name: "b", Requests: qty("2Gi")},
				{Name: "c", Requests: qty("3Gi")},
			},
		},
	}

	first := QuotaHardLimits(spec)
	for range 50 {
		if !apiequality.Semantic.DeepEqual(first, QuotaHardLimits(spec)) {
			t.Fatalf("QuotaHardLimits() is not deterministic across calls")
		}
	}
}

func TestResourceQuotaApplyConfiguration(t *testing.T) {
	t.Run("nil when the profile declares no quota", func(t *testing.T) {
		if got := ResourceQuota(profileWith(nil)); got != nil {
			t.Errorf("ResourceQuota() = %v, want nil", got)
		}
	})

	t.Run("carries deterministic name, namespace and ownership labels", func(t *testing.T) {
		profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
			s.Quota = &platformv1alpha1.QuotaSpec{Pods: ptr.To(int32(10))}
		})

		got := ResourceQuota(profile)
		if got == nil {
			t.Fatal("ResourceQuota() = nil, want an apply configuration")
		}
		if *got.Name != platformv1alpha1.ResourceQuotaName {
			t.Errorf("name = %q, want %q", *got.Name, platformv1alpha1.ResourceQuotaName)
		}
		if *got.Namespace != "tenant" {
			t.Errorf("namespace = %q, want %q", *got.Namespace, "tenant")
		}
		if got.Labels[platformv1alpha1.ManagedByLabel] != platformv1alpha1.OperatorName {
			t.Errorf("missing managed-by label, got %v", got.Labels)
		}
		if got.Labels[platformv1alpha1.ProfileLabel] != "tenant-profile" {
			t.Errorf("missing profile label, got %v", got.Labels)
		}
		if got.Annotations[platformv1alpha1.AppliedGenerationAnnotation] != "1" {
			t.Errorf("applied-generation = %q, want %q",
				got.Annotations[platformv1alpha1.AppliedGenerationAnnotation], "1")
		}

		// The apply configuration must declare spec.hard and nothing else, so
		// that server-side apply claims ownership of no other field.
		if got.Spec == nil || got.Spec.Hard == nil {
			t.Fatal("apply configuration does not declare spec.hard")
		}
		if got.Spec.Scopes != nil || got.Spec.ScopeSelector != nil {
			t.Error("apply configuration declares scopes it does not own")
		}
		if got.Status != nil {
			t.Error("apply configuration declares status, which belongs to the quota controller")
		}
	})
}

func TestQuotaKeyEncoding(t *testing.T) {
	hard := corev1.ResourceList{
		corev1.ResourcePods:        resource.MustParse("50"),
		corev1.ResourceRequestsCPU: resource.MustParse("8"),
		"services":                 resource.MustParse("10"),
		"count/deployments.apps":   resource.MustParse("20"),
	}

	// Sorted, because the encoding is written into an annotation and compared
	// against it next pass. Go map iteration order is random, so an unsorted
	// encoding would differ every time and the operator would rewrite the
	// object forever.
	const want = "count/deployments.apps,pods,requests.cpu,services"
	if got := EncodeQuotaKeys(hard); got != want {
		t.Errorf("EncodeQuotaKeys() = %q, want %q", got, want)
	}

	for range 50 {
		if got := EncodeQuotaKeys(hard); got != want {
			t.Fatalf("EncodeQuotaKeys() is not deterministic: got %q", got)
		}
	}

	if got := DecodeQuotaKeys(want); !slices.Equal(got, QuotaKeys(hard)) {
		t.Errorf("DecodeQuotaKeys(EncodeQuotaKeys(x)) = %v, want %v", got, QuotaKeys(hard))
	}

	if got := DecodeQuotaKeys(""); got != nil {
		t.Errorf("DecodeQuotaKeys(\"\") = %v, want nil", got)
	}
}

func TestQuotaInSync(t *testing.T) {
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Quota = &platformv1alpha1.QuotaSpec{
			Requests: &platformv1alpha1.ComputeResources{CPU: qty("8")},
			Pods:     ptr.To(int32(50)),
		}
	})
	// What the operator would have recorded after applying the profile above.
	const recordedKeys = "pods,requests.cpu"

	tests := []struct {
		name     string
		hard     corev1.ResourceList
		recorded string
		want     bool
	}{
		{
			name: "an exact match needs no write",
			hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("8"),
				corev1.ResourcePods:        resource.MustParse("50"),
			},
			recorded: recordedKeys,
			want:     true,
		},
		{
			// The central drift case from the specification: someone raised the
			// CPU allowance by hand.
			name: "a changed value is drift",
			hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("20"),
				corev1.ResourcePods:        resource.MustParse("50"),
			},
			recorded: recordedKeys,
			want:     false,
		},
		{
			name: "a deleted key is drift",
			hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("8"),
			},
			recorded: recordedKeys,
			want:     false,
		},
		{
			// Quantities are compared by value, not by spelling. Without this,
			// the operator would rewrite the object on every single pass.
			name: "an equivalent spelling of the same quantity is not drift",
			hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("8000m"),
				corev1.ResourcePods:        resource.MustParse("50"),
			},
			recorded: recordedKeys,
			want:     true,
		},
		{
			// spec.hard has per-key ownership under server-side apply. A limit
			// the operator never recorded is not its to police, and flagging it
			// would mean reporting drift it could never correct — apply cannot
			// prune a key owned by another field manager.
			name: "a limit the operator never recorded is ignored",
			hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("8"),
				corev1.ResourcePods:        resource.MustParse("50"),
				"services":                 resource.MustParse("10"),
				"configmaps":               resource.MustParse("30"),
			},
			recorded: recordedKeys,
			want:     true,
		},
		{
			// The operator recorded "services" as its own, and the profile no
			// longer declares it. Re-applying releases it. Reporting "in sync"
			// here would leave the stale limit forever, because skipping the
			// apply is exactly what stops apply from pruning it.
			name: "a recorded key the profile no longer declares is drift",
			hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("8"),
				corev1.ResourcePods:        resource.MustParse("50"),
				"services":                 resource.MustParse("10"),
			},
			recorded: "pods,requests.cpu,services",
			want:     false,
		},
		{
			// An object from before the annotation existed, or written by an
			// older build. One apply records the key set and it self-heals.
			name: "a quota with no recorded key set is drift",
			hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("8"),
				corev1.ResourcePods:        resource.MustParse("50"),
			},
			recorded: "",
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := &corev1.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						platformv1alpha1.ManagedQuotaKeysAnnotation: tc.recorded,
					},
				},
				Spec: corev1.ResourceQuotaSpec{Hard: tc.hard},
			}
			if got := QuotaInSync(profile, actual); got != tc.want {
				t.Errorf("QuotaInSync() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQuotaInSyncIgnoresStatus(t *testing.T) {
	// The quota controller rewrites status continuously as namespace usage
	// changes. If the operator compared it, every pod scheduled in the
	// namespace would look like drift and provoke a write.
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Quota = &platformv1alpha1.QuotaSpec{Pods: ptr.To(int32(50))}
	})

	actual := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				platformv1alpha1.ManagedQuotaKeysAnnotation: "pods",
			},
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("50")},
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("50")},
			Used: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("37")},
		},
	}

	if !QuotaInSync(profile, actual) {
		t.Error("QuotaInSync() = false, want true: status must not be compared")
	}
}
