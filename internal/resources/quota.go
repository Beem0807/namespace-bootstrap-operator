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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

// Per-StorageClass quota keys take the form "<class><suffix>", for example
// "gp3.storageclass.storage.k8s.io/requests.storage".
//
// Kubernetes does not export these suffixes from k8s.io/api — they live in
// k8s.io/kubernetes, which is not a dependency and should not become one — so
// they are restated here. They are part of the stable ResourceQuota contract,
// and the unit tests assert against literal expected key names so a typo
// cannot pass silently.
const (
	storageClassRequestsStorageSuffix = ".storageclass.storage.k8s.io/requests.storage"
	storageClassPVCSuffix             = ".storageclass.storage.k8s.io/persistentvolumeclaims"
)

// QuotaHardLimits translates a profile's quota section into ResourceQuota hard
// limits.
//
// This is the whole of the quota logic, and it is deliberately a plain map
// transformation. An unset field produces no key, and a key absent from a
// ResourceQuota means that resource is unconstrained — so omitting
// spec.quota.pods leaves the pod count unlimited rather than capping it at
// zero. Getting that backwards would quietly make every pod in the namespace
// unschedulable.
//
// Returns nil when there is nothing to constrain, which the caller reads as
// "manage no ResourceQuota" rather than "apply an empty one".
func QuotaHardLimits(spec *platformv1alpha1.QuotaSpec) corev1.ResourceList {
	if spec == nil {
		return nil
	}

	hard := corev1.ResourceList{}

	// Aggregate compute. Requests govern how much cluster capacity the
	// namespace can reserve, since the scheduler places pods by request;
	// limits govern its burst ceiling.
	if spec.Requests != nil {
		if spec.Requests.CPU != nil {
			hard[corev1.ResourceRequestsCPU] = *spec.Requests.CPU
		}
		if spec.Requests.Memory != nil {
			hard[corev1.ResourceRequestsMemory] = *spec.Requests.Memory
		}
		if spec.Requests.EphemeralStorage != nil {
			hard[corev1.ResourceRequestsEphemeralStorage] = *spec.Requests.EphemeralStorage
		}
	}
	if spec.Limits != nil {
		if spec.Limits.CPU != nil {
			hard[corev1.ResourceLimitsCPU] = *spec.Limits.CPU
		}
		if spec.Limits.Memory != nil {
			hard[corev1.ResourceLimitsMemory] = *spec.Limits.Memory
		}
		if spec.Limits.EphemeralStorage != nil {
			hard[corev1.ResourceLimitsEphemeralStorage] = *spec.Limits.EphemeralStorage
		}
	}

	// Pod count. The API models this as an integer because a count of "50m"
	// is meaningless, but ResourceQuota speaks only in quantities, so it is
	// converted here. DecimalSI keeps the serialised form a plain "50" rather
	// than something like "50k".
	if spec.Pods != nil {
		hard[corev1.ResourcePods] = countQuantity(*spec.Pods)
	}

	if spec.Storage != nil {
		if spec.Storage.Requests != nil {
			hard[corev1.ResourceRequestsStorage] = *spec.Storage.Requests
		}
		if spec.Storage.PersistentVolumeClaims != nil {
			hard[corev1.ResourcePersistentVolumeClaims] = countQuantity(*spec.Storage.PersistentVolumeClaims)
		}

		// Per-StorageClass ceilings sit alongside the cluster-wide ones rather
		// than replacing them: a claim against "gp3" is charged both to
		// requests.storage and to gp3's own key, so whichever is tighter wins.
		// That is what lets a platform ration an expensive tier without
		// raising the namespace's overall storage budget.
		for _, class := range spec.Storage.Classes {
			if class.Requests != nil {
				hard[corev1.ResourceName(class.Name+storageClassRequestsStorageSuffix)] = *class.Requests
			}
			if class.PersistentVolumeClaims != nil {
				hard[corev1.ResourceName(class.Name+storageClassPVCSuffix)] =
					countQuantity(*class.PersistentVolumeClaims)
			}
		}
	}

	// Extended resources — GPUs and other device-plugin resources, and
	// hugepages — are quoted on the requests side only. Kubernetes requires
	// them to be requested and limited at the same value, so a request ceiling
	// is also a limit ceiling.
	for name, quota := range spec.ExtendedResources {
		hard[corev1.ResourceName(corev1.DefaultResourceRequestsPrefix+name)] = quota
	}

	// Object counts are passed through as named. Their keys are validated by
	// the API server's schema, so anything reaching here is already a
	// well-formed ResourceQuota object-count name.
	for name, count := range spec.ObjectCounts {
		hard[corev1.ResourceName(name)] = countQuantity(count)
	}

	if len(hard) == 0 {
		return nil
	}
	return hard
}

// countQuantity converts an object count into the quantity form ResourceQuota
// requires. DecimalSI keeps the serialised value a plain integer.
func countQuantity(n int32) resource.Quantity {
	return *resource.NewQuantity(int64(n), resource.DecimalSI)
}

// ResourceQuota builds the apply configuration for a profile's ResourceQuota,
// or nil when the profile asks for no quota.
//
// The object name is fixed rather than derived from the profile name, because
// a namespace is governed by exactly one profile and a fixed name cannot be
// stranded by renaming that profile.
func ResourceQuota(profile *platformv1alpha1.NamespaceProfile) *corev1ac.ResourceQuotaApplyConfiguration {
	hard := QuotaHardLimits(profile.Spec.Quota)
	if hard == nil {
		return nil
	}

	annotations := appliedGenerationAnnotation(profile)
	annotations[platformv1alpha1.ManagedQuotaKeysAnnotation] = EncodeQuotaKeys(hard)

	return corev1ac.ResourceQuota(platformv1alpha1.ResourceQuotaName, profile.Spec.Namespace).
		WithLabels(OwnershipLabels(profile)).
		WithAnnotations(annotations).
		WithSpec(corev1ac.ResourceQuotaSpec().WithHard(hard))
}

// QuotaKeys returns the hard-limit keys of a ResourceList, sorted.
//
// Sorting matters: the result is written into an annotation and compared
// against it on the next pass, and Go map iteration order is random. An
// unsorted encoding would differ on every reconciliation and the operator
// would rewrite the object forever.
func QuotaKeys(hard corev1.ResourceList) []string {
	keys := make([]string, 0, len(hard))
	for name := range hard {
		keys = append(keys, string(name))
	}
	slices.Sort(keys)
	return keys
}

// EncodeQuotaKeys renders a key set for storage in the managed-keys annotation.
func EncodeQuotaKeys(hard corev1.ResourceList) string {
	return strings.Join(QuotaKeys(hard), ",")
}

// DecodeQuotaKeys reads a key set back out of the managed-keys annotation.
func DecodeQuotaKeys(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

// QuotaInSync reports whether a live ResourceQuota already matches the profile.
//
// Two things must hold. Every key the profile declares must be present with a
// semantically equal value — ordinary drift — and the set of keys the operator
// recorded as its own must equal the set it now declares.
//
// The second condition is what makes a withdrawn limit disappear. A hard-limit
// map has per-key ownership under server-side apply, so apply prunes the keys
// the operator stops declaring — but only if an apply happens at all. Comparing
// recorded keys against declared keys is what detects that an apply is needed,
// and it is also why the operator can tell a withdrawn limit from one it never
// set: the latter was never recorded.
//
// A limit the operator never recorded is therefore left strictly alone. An
// administrator adding "services: 10" to a quota whose profile does not mention
// it provokes no write, because it is not the operator's to police.
//
// status is deliberately not compared. The quota controller owns it and
// rewrites it continuously as namespace usage changes, so including it would
// make ordinary activity look like drift.
//
// Comparison of values is semantic, so quantities are compared by value: a
// limit written as "8" and one written as "8000m" are the same CPU allowance
// and produce no write.
func QuotaInSync(profile *platformv1alpha1.NamespaceProfile, actual *corev1.ResourceQuota) bool {
	desired := QuotaHardLimits(profile.Spec.Quota)

	recorded := DecodeQuotaKeys(actual.Annotations[platformv1alpha1.ManagedQuotaKeysAnnotation])
	if !slices.Equal(recorded, QuotaKeys(desired)) {
		return false
	}

	for name, want := range desired {
		got, present := actual.Spec.Hard[name]
		if !present || !apiequality.Semantic.DeepEqual(want, got) {
			return false
		}
	}

	return true
}
