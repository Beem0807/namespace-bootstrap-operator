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
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

// computeResourceList converts the API's CPU/memory pair into a Kubernetes
// ResourceList, returning nil when neither is set so that no empty map leaks
// into a generated object.
func computeResourceList(r *platformv1alpha1.ComputeResources) corev1.ResourceList {
	if r == nil {
		return nil
	}

	list := corev1.ResourceList{}
	if r.CPU != nil {
		list[corev1.ResourceCPU] = *r.CPU
	}
	if r.Memory != nil {
		list[corev1.ResourceMemory] = *r.Memory
	}
	if r.EphemeralStorage != nil {
		list[corev1.ResourceEphemeralStorage] = *r.EphemeralStorage
	}

	if len(list) == 0 {
		return nil
	}
	return list
}

// ContainerLimitItem translates a profile's limitRange section into a single
// Container-scoped LimitRange item, or nil when the profile asks for none.
//
// Scoping to Container rather than Pod is deliberate. Container limits are
// what admission needs in order to fill in defaults for an individual
// container, and they compose predictably with a ResourceQuota: the LimitRange
// is evaluated first, filling in anything the pod did not declare, and the
// quota is then checked against the resulting totals.
//
// A Pod-scoped item would cap a pod's combined resources instead, which is a
// different and more surprising constraint — a two-container pod would be
// rejected for reasons neither container could explain. The API keeps room to
// add it later rather than conflating the two now.
func ContainerLimitItem(spec *platformv1alpha1.LimitRangeSpec) *corev1.LimitRangeItem {
	if spec == nil {
		return nil
	}

	item := corev1.LimitRangeItem{
		Type:           corev1.LimitTypeContainer,
		Default:        computeResourceList(spec.Default),
		DefaultRequest: computeResourceList(spec.DefaultRequest),
		Min:            computeResourceList(spec.Min),
		Max:            computeResourceList(spec.Max),
	}

	// Every sub-field being empty means the section said nothing usable. The
	// API's MinProperties guard makes this unreachable through the API server,
	// but a hand-built spec in a test could still produce it, and an empty
	// LimitRange item is not something to apply to a cluster.
	if item.Default == nil && item.DefaultRequest == nil && item.Min == nil && item.Max == nil {
		return nil
	}

	applyKubernetesItemDefaults(&item)
	return &item
}

// applyKubernetesItemDefaults fills in the values the API server would add
// anyway, so that the generated object matches what the cluster will store.
//
// Kubernetes defaults Container-scoped LimitRange items on every write: for
// each resource, an unset default takes the value of max, and an unset
// defaultRequest then takes the value of default. Those are plain v1 API
// defaults applied while the request is being decoded, which means they land
// on the object no matter how it was submitted.
//
// Not replicating them is an infinite write loop, and a subtle one. The
// operator would compute a desired item without them, apply it, and the API
// server would store the item *with* them. The next reconciliation would
// compare its desired item against the stored one, find a difference it did
// not cause, declare drift, and re-apply — producing the same divergence
// again. Nothing would ever actually change on the object, so there is no
// runaway write storm to notice; the only symptoms are a drift counter that
// climbs forever, a stream of pointless Events, and a lastDriftCorrectionTime
// that is always a few seconds old.
//
// Mirroring another project's defaulting logic is normally a smell. It is
// acceptable here because this behaviour is part of the long-stable v1 API, and
// because an idempotency test asserts no resourceVersion changes across
// repeated reconciliations — so if Kubernetes ever changed it, that test would
// fail immediately rather than the operator quietly fighting the API server.
func applyKubernetesItemDefaults(item *corev1.LimitRangeItem) {
	if item.Type != corev1.LimitTypeContainer {
		return
	}

	if item.Default == nil && len(item.Max) > 0 {
		item.Default = corev1.ResourceList{}
	}
	for name, quantity := range item.Max {
		if _, set := item.Default[name]; !set {
			item.Default[name] = quantity.DeepCopy()
		}
	}

	if item.DefaultRequest == nil && len(item.Default) > 0 {
		item.DefaultRequest = corev1.ResourceList{}
	}
	for name, quantity := range item.Default {
		if _, set := item.DefaultRequest[name]; !set {
			item.DefaultRequest[name] = quantity.DeepCopy()
		}
	}
}

// LimitRange builds the apply configuration for a profile's LimitRange, or nil
// when the profile asks for none.
func LimitRange(profile *platformv1alpha1.NamespaceProfile) *corev1ac.LimitRangeApplyConfiguration {
	item := ContainerLimitItem(profile.Spec.LimitRange)
	if item == nil {
		return nil
	}

	itemAC := corev1ac.LimitRangeItem().WithType(item.Type)
	if item.Default != nil {
		itemAC = itemAC.WithDefault(item.Default)
	}
	if item.DefaultRequest != nil {
		itemAC = itemAC.WithDefaultRequest(item.DefaultRequest)
	}
	if item.Min != nil {
		itemAC = itemAC.WithMin(item.Min)
	}
	if item.Max != nil {
		itemAC = itemAC.WithMax(item.Max)
	}

	return corev1ac.LimitRange(platformv1alpha1.LimitRangeName, profile.Spec.Namespace).
		WithLabels(OwnershipLabels(profile)).
		WithAnnotations(appliedGenerationAnnotation(profile)).
		WithSpec(corev1ac.LimitRangeSpec().WithLimits(itemAC))
}

// LimitRangeInSync reports whether a live LimitRange already matches the
// profile.
//
// Unlike a ResourceQuota's hard limits, spec.limits is declared +listType=atomic
// in the Kubernetes schema, which means server-side apply treats the list as an
// indivisible unit: whoever applies it owns all of it. So the operator owns the
// entire list, plain equality is the correct comparison, and applying removes
// any item someone else added — which is both the documented behaviour of an
// atomic list and the reason the operator cannot offer to merge with
// hand-written items here.
func LimitRangeInSync(profile *platformv1alpha1.NamespaceProfile, actual *corev1.LimitRange) bool {
	item := ContainerLimitItem(profile.Spec.LimitRange)
	if item == nil {
		return len(actual.Spec.Limits) == 0
	}
	return apiequality.Semantic.DeepEqual([]corev1.LimitRangeItem{*item}, actual.Spec.Limits)
}
