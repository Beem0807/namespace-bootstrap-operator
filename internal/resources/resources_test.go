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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

// The ownership check is the single most safety-critical function in the
// package: it is the difference between correcting drift on an object the
// operator created and destroying an identically named object belonging to
// someone else. Both labels must match, and neither alone is sufficient.
func TestIsOwnedBy(t *testing.T) {
	profile := profileWith(nil)

	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name: "both ownership labels present and matching",
			labels: map[string]string{
				platformv1alpha1.ManagedByLabel: platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:   "tenant-profile",
			},
			want: true,
		},
		{
			name:   "no labels at all",
			labels: nil,
			want:   false,
		},
		{
			// The dangerous case: another profile's object, at the same
			// deterministic name, in the same namespace.
			name: "managed by this operator but for a different profile",
			labels: map[string]string{
				platformv1alpha1.ManagedByLabel: platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:   "other-profile",
			},
			want: false,
		},
		{
			name: "named for this profile but managed by something else",
			labels: map[string]string{
				platformv1alpha1.ManagedByLabel: "helm",
				platformv1alpha1.ProfileLabel:   "tenant-profile",
			},
			want: false,
		},
		{
			name: "the profile label alone is not ownership",
			labels: map[string]string{
				platformv1alpha1.ProfileLabel: "tenant-profile",
			},
			want: false,
		},
		{
			name: "the managed-by label alone is not ownership",
			labels: map[string]string{
				platformv1alpha1.ManagedByLabel: platformv1alpha1.OperatorName,
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obj := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Labels: tc.labels}}
			if got := IsOwnedBy(obj, profile); got != tc.want {
				t.Errorf("IsOwnedBy() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsManagedByOperator(t *testing.T) {
	// The weaker question, and the right one when deciding whether an event is
	// worth reconciling: an object belonging to a *different* profile still
	// concerns this operator, whereas one belonging to nobody does not.
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name: "generated for some other profile",
			labels: map[string]string{
				platformv1alpha1.ManagedByLabel: platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:   "other-profile",
			},
			want: true,
		},
		{
			name:   "managed by a different tool",
			labels: map[string]string{platformv1alpha1.ManagedByLabel: "kustomize"},
			want:   false,
		},
		{
			name:   "unlabelled",
			labels: nil,
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obj := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Labels: tc.labels}}
			if got := IsManagedByOperator(obj); got != tc.want {
				t.Errorf("IsManagedByOperator() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManagingProfile(t *testing.T) {
	obj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{platformv1alpha1.ProfileLabel: "payments-profile"},
	}}
	if got := ManagingProfile(obj); got != "payments-profile" {
		t.Errorf("ManagingProfile() = %q, want %q", got, "payments-profile")
	}

	if got := ManagingProfile(&corev1.Namespace{}); got != "" {
		t.Errorf("ManagingProfile() on an unlabelled object = %q, want empty", got)
	}
}

func TestOwnershipLabelsAreNotAliased(t *testing.T) {
	// NamespaceLabels builds on top of OwnershipLabels and adds to the map it
	// receives. If OwnershipLabels ever returned a shared map, one profile's
	// Pod Security labels would leak into another's ownership labels.
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Security = &platformv1alpha1.SecuritySpec{
			PodSecurityStandard: platformv1alpha1.PodSecurityRestricted,
		}
	})

	_ = NamespaceLabels(profile)

	fresh := OwnershipLabels(profile)
	if len(fresh) != 2 {
		t.Errorf("OwnershipLabels() returned %d labels, want 2: %v", len(fresh), fresh)
	}
}
