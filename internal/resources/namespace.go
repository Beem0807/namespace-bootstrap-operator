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
	corev1 "k8s.io/api/core/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

// operatorNamespaceLabelKeys is every namespace label key this operator can
// write: the two ownership labels, plus the six Pod Security Admission labels.
//
// The set is fixed and fully enumerable, which is what makes precise label
// management possible without storing any extra bookkeeping. To decide whether
// the namespace matches the profile, the operator compares exactly these keys
// and ignores every other label on the namespace. A label applied by a cluster
// administrator, a service mesh, a cost-allocation tool or a GitOps controller
// is therefore invisible to this operator and survives untouched.
var operatorNamespaceLabelKeys = []string{
	platformv1alpha1.ManagedByLabel,
	platformv1alpha1.ProfileLabel,
	platformv1alpha1.PSAEnforceLabel,
	platformv1alpha1.PSAEnforceVersionLabel,
	platformv1alpha1.PSAAuditLabel,
	platformv1alpha1.PSAAuditVersionLabel,
	platformv1alpha1.PSAWarnLabel,
	platformv1alpha1.PSAWarnVersionLabel,
}

// OperatorNamespaceLabelKeys returns the namespace label keys this operator
// manages. Exported so that tests and documentation can assert the set rather
// than restating it.
func OperatorNamespaceLabelKeys() []string {
	keys := make([]string, len(operatorNamespaceLabelKeys))
	copy(keys, operatorNamespaceLabelKeys)
	return keys
}

// NamespaceLabels is the complete set of labels this operator declares on the
// target namespace.
//
// Ownership labels are always present. Pod Security Admission labels appear
// only when the profile has a security section — if it does not, the operator
// declares none of them, which leaves any PSA labels an administrator set by
// hand completely alone. That is a meaningful difference from writing
// "privileged": declaring nothing defers to whoever did, whereas writing
// "privileged" would actively strip a namespace's protection.
func NamespaceLabels(profile *platformv1alpha1.NamespaceProfile) map[string]string {
	labels := OwnershipLabels(profile)

	security := profile.Spec.Security
	if security == nil {
		return labels
	}

	// All three modes are pinned to the same Standards version. Pinning them
	// independently is possible in Kubernetes but means audit and enforce can
	// disagree about what a level even means, which makes a staged rollout
	// measure the wrong thing — the whole point of auditing at restricted
	// while enforcing baseline is that both read the same rulebook.
	version := security.StandardsVersion()

	labels[platformv1alpha1.PSAEnforceLabel] = string(security.PodSecurityStandard)
	labels[platformv1alpha1.PSAEnforceVersionLabel] = version
	labels[platformv1alpha1.PSAAuditLabel] = string(security.AuditLevel())
	labels[platformv1alpha1.PSAAuditVersionLabel] = version
	labels[platformv1alpha1.PSAWarnLabel] = string(security.WarnLevel())
	labels[platformv1alpha1.PSAWarnVersionLabel] = version

	return labels
}

// Namespace builds the apply configuration for the target namespace.
//
// Only metadata is declared — never spec, and never status. A namespace's spec
// holds its finalizers, and claiming ownership of that field could interfere
// with namespace teardown. The operator's entire interest in the namespace is
// its labels.
//
// adopted records that the operator took over a namespace it did not create.
// It must be sticky: the caller passes true if the live namespace already
// carries the marker, so that a later reconciliation does not drop it. Losing
// it would silently re-arm namespace deletion for a namespace the operator has
// no business deleting.
func Namespace(
	profile *platformv1alpha1.NamespaceProfile, adopted bool,
) *corev1ac.NamespaceApplyConfiguration {
	annotations := appliedGenerationAnnotation(profile)
	if adopted {
		annotations[platformv1alpha1.AdoptedAnnotation] = "true"
	}

	return corev1ac.Namespace(profile.Spec.Namespace).
		WithLabels(NamespaceLabels(profile)).
		WithAnnotations(annotations)
}

// WasAdopted reports whether a namespace carries the adoption marker.
//
// A namespace that was adopted rather than created is permanently
// disqualified from operator-driven deletion: something else created it, and
// the operator cannot know what else depends on it.
func WasAdopted(namespace *corev1.Namespace) bool {
	return namespace.Annotations[platformv1alpha1.AdoptedAnnotation] == "true"
}

// NamespaceLabelsInSync reports whether the namespace's labels already match
// the profile.
//
// Only the keys from operatorNamespaceLabelKeys are examined, and they are
// checked in both directions: a key the profile declares must be present with
// the right value, and a key the operator manages must be absent if the profile
// no longer declares it. The second direction is what makes removing
// spec.security actually withdraw the Pod Security labels — skipping the apply
// is precisely what would stop server-side apply from pruning them.
//
// Labels outside that set are not consulted at all, so unrelated namespace
// metadata can never make a profile look out of sync.
func NamespaceLabelsInSync(profile *platformv1alpha1.NamespaceProfile, actual *corev1.Namespace) bool {
	desired := NamespaceLabels(profile)
	live := actual.GetLabels()

	for _, key := range operatorNamespaceLabelKeys {
		want, declared := desired[key]
		got, present := live[key]

		if declared != present || (declared && want != got) {
			return false
		}
	}
	return true
}
