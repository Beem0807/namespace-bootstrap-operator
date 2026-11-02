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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

func TestContainerLimitItem(t *testing.T) {
	tests := []struct {
		name string
		spec *platformv1alpha1.LimitRangeSpec
		want *corev1.LimitRangeItem
	}{
		{
			name: "a nil section manages no limit range",
			spec: nil,
			want: nil,
		},
		{
			name: "an empty section produces no item",
			spec: &platformv1alpha1.LimitRangeSpec{},
			want: nil,
		},
		{
			// defaultRequest is the field that keeps a quota-constrained
			// namespace usable: a ResourceQuota on requests.cpu rejects any pod
			// that declares no CPU request, and this is what fills one in.
			name: "defaultRequest alone produces a Container item, with nothing to default",
			spec: &platformv1alpha1.LimitRangeSpec{
				DefaultRequest: &platformv1alpha1.ComputeResources{CPU: qty("100m"), Memory: qty("128Mi")},
			},
			want: &corev1.LimitRangeItem{
				Type: corev1.LimitTypeContainer,
				DefaultRequest: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
		},
		{
			name: "all four bounds are carried through to the right fields",
			spec: &platformv1alpha1.LimitRangeSpec{
				DefaultRequest: &platformv1alpha1.ComputeResources{CPU: qty("100m")},
				Default:        &platformv1alpha1.ComputeResources{CPU: qty("500m")},
				Min:            &platformv1alpha1.ComputeResources{CPU: qty("10m")},
				Max:            &platformv1alpha1.ComputeResources{CPU: qty("4")},
			},
			want: &corev1.LimitRangeItem{
				Type:           corev1.LimitTypeContainer,
				DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				Default:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Min:            corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
				Max:            corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
			},
		},
		{
			// ComputeResources is shared between quota and limit range, so
			// ephemeral storage is bounded per container by the same field
			// change that quotas it in aggregate. LimitRange genuinely
			// supports ephemeral-storage, so this is correct rather than
			// incidental.
			name: "ephemeral storage is bounded per container too",
			spec: &platformv1alpha1.LimitRangeSpec{
				DefaultRequest: &platformv1alpha1.ComputeResources{EphemeralStorage: qty("256Mi")},
				Max:            &platformv1alpha1.ComputeResources{EphemeralStorage: qty("10Gi")},
			},
			want: &corev1.LimitRangeItem{
				Type: corev1.LimitTypeContainer,
				DefaultRequest: corev1.ResourceList{
					corev1.ResourceEphemeralStorage: resource.MustParse("256Mi"),
				},
				// Defaulted from max, exactly as the API server would.
				Default: corev1.ResourceList{
					corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
				},
				Max: corev1.ResourceList{
					corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
				},
			},
		},
		{
			// The defaulting chain in full: max fills in default, and default
			// then fills in defaultRequest. Replicating it here is what keeps
			// the generated object identical to the stored one; without it the
			// operator would see drift it caused itself, on every single pass.
			name: "max alone cascades into default and defaultRequest",
			spec: &platformv1alpha1.LimitRangeSpec{
				Max: &platformv1alpha1.ComputeResources{CPU: qty("4"), Memory: qty("8Gi")},
			},
			want: &corev1.LimitRangeItem{
				Type: corev1.LimitTypeContainer,
				Max: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
				Default: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
				DefaultRequest: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		},
		{
			// An explicit value is never overwritten by the cascade — only
			// gaps are filled.
			name: "an explicit default survives the cascade from max",
			spec: &platformv1alpha1.LimitRangeSpec{
				Default: &platformv1alpha1.ComputeResources{CPU: qty("500m")},
				Max:     &platformv1alpha1.ComputeResources{CPU: qty("4")},
			},
			want: &corev1.LimitRangeItem{
				Type:           corev1.LimitTypeContainer,
				Default:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Max:            corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
			},
		},
		{
			name: "cpu, memory and ephemeral storage are independent, so one may be bounded alone",
			spec: &platformv1alpha1.LimitRangeSpec{
				Max: &platformv1alpha1.ComputeResources{Memory: qty("8Gi")},
			},
			want: &corev1.LimitRangeItem{
				Type:           corev1.LimitTypeContainer,
				Max:            corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
				Default:        corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
				DefaultRequest: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ContainerLimitItem(tc.spec)
			if !apiequality.Semantic.DeepEqual(tc.want, got) {
				t.Errorf("ContainerLimitItem() mismatch\n  want: %+v\n  got:  %+v", tc.want, got)
			}
		})
	}
}

func TestContainerLimitItemIsAlwaysContainerScoped(t *testing.T) {
	// Pod-scoped limits cap a pod's combined resources, which rejects
	// multi-container pods for reasons no single container explains. The
	// operator deliberately only emits Container scope.
	item := ContainerLimitItem(&platformv1alpha1.LimitRangeSpec{
		Max: &platformv1alpha1.ComputeResources{CPU: qty("1")},
	})
	if item.Type != corev1.LimitTypeContainer {
		t.Errorf("item type = %q, want %q", item.Type, corev1.LimitTypeContainer)
	}
}

func TestLimitRangeApplyConfiguration(t *testing.T) {
	t.Run("nil when the profile declares no limit range", func(t *testing.T) {
		if got := LimitRange(profileWith(nil)); got != nil {
			t.Errorf("LimitRange() = %v, want nil", got)
		}
	})

	t.Run("declares exactly one Container item with the set bounds", func(t *testing.T) {
		profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
			s.LimitRange = &platformv1alpha1.LimitRangeSpec{
				DefaultRequest: &platformv1alpha1.ComputeResources{CPU: qty("100m")},
				Max:            &platformv1alpha1.ComputeResources{CPU: qty("4")},
			}
		})

		got := LimitRange(profile)
		if got == nil {
			t.Fatal("LimitRange() = nil, want an apply configuration")
		}
		if *got.Name != platformv1alpha1.LimitRangeName {
			t.Errorf("name = %q, want %q", *got.Name, platformv1alpha1.LimitRangeName)
		}
		if *got.Namespace != "tenant" {
			t.Errorf("namespace = %q, want %q", *got.Namespace, "tenant")
		}
		if got.Labels[platformv1alpha1.ManagedByLabel] != platformv1alpha1.OperatorName {
			t.Errorf("missing managed-by label, got %v", got.Labels)
		}
		if got.Spec == nil || len(got.Spec.Limits) != 1 {
			t.Fatalf("want exactly one limit item, got %+v", got.Spec)
		}

		item := got.Spec.Limits[0]
		if *item.Type != corev1.LimitTypeContainer {
			t.Errorf("item type = %q, want Container", *item.Type)
		}
		// min is genuinely absent from the profile and nothing defaults it, so
		// it must stay undeclared rather than being sent as an empty map that
		// asserts ownership of a field the profile is silent about.
		if item.Min != nil {
			t.Error("min was declared though the profile does not set it")
		}
		if item.DefaultRequest == nil || item.Max == nil {
			t.Error("defaultRequest and max should both be declared")
		}
		// default IS declared, even though the profile omits it, because the
		// API server would default it from max regardless. Declaring it is
		// what makes the generated object match the stored one.
		if item.Default == nil {
			t.Error("default should be declared, cascaded from max")
		} else if got := (*item.Default)[corev1.ResourceCPU]; got.String() != "4" {
			t.Errorf("default cpu = %s, want 4 (cascaded from max)", got.String())
		}
	})
}

func TestLimitRangeInSync(t *testing.T) {
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.LimitRange = &platformv1alpha1.LimitRangeSpec{
			DefaultRequest: &platformv1alpha1.ComputeResources{CPU: qty("100m")},
			Max:            &platformv1alpha1.ComputeResources{CPU: qty("4")},
		}
	})

	// Including the value the API server cascades from max into default, since
	// that is what the cluster actually stores.
	desired := corev1.LimitRangeItem{
		Type:           corev1.LimitTypeContainer,
		DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
		Default:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
		Max:            corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
	}

	tests := []struct {
		name   string
		limits []corev1.LimitRangeItem
		want   bool
	}{
		{
			name:   "an exact match needs no write",
			limits: []corev1.LimitRangeItem{desired},
			want:   true,
		},
		{
			name:   "an empty list is drift",
			limits: nil,
			want:   false,
		},
		{
			name: "a changed bound is drift",
			limits: []corev1.LimitRangeItem{{
				Type:           corev1.LimitTypeContainer,
				DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				Max:            corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("16")},
			}},
			want: false,
		},
		{
			name: "an equivalent spelling of the same quantity is not drift",
			limits: []corev1.LimitRangeItem{{
				Type:           corev1.LimitTypeContainer,
				DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0.1")},
				Default:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4000m")},
				Max:            corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4000m")},
			}},
			want: true,
		},
		{
			// spec.limits is +listType=atomic, so the operator owns the whole
			// list rather than individual items. An extra item is drift and
			// applying removes it — which is the documented behaviour of an
			// atomic list, and the reason this builder cannot offer to merge
			// with hand-written items.
			name: "an extra hand-added item is drift, because the list is atomic",
			limits: []corev1.LimitRangeItem{desired, {
				Type: corev1.LimitTypePod,
				Max:  corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")},
			}},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := &corev1.LimitRange{Spec: corev1.LimitRangeSpec{Limits: tc.limits}}
			if got := LimitRangeInSync(profile, actual); got != tc.want {
				t.Errorf("LimitRangeInSync() = %v, want %v", got, tc.want)
			}
		})
	}
}
