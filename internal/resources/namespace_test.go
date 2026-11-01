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
	"maps"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

func TestNamespaceLabels(t *testing.T) {
	tests := []struct {
		name     string
		security *platformv1alpha1.SecuritySpec
		want     map[string]string
	}{
		{
			// Declaring no Pod Security labels is not the same as declaring
			// "privileged". Declaring nothing defers to whoever set the labels;
			// writing "privileged" would actively strip a namespace's
			// protection.
			name:     "no security section declares no Pod Security labels",
			security: nil,
			want: map[string]string{
				platformv1alpha1.ManagedByLabel: platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:   "tenant-profile",
			},
		},
		{
			name: "audit and warn follow the enforced level when unset",
			security: &platformv1alpha1.SecuritySpec{
				PodSecurityStandard: platformv1alpha1.PodSecurityRestricted,
			},
			want: map[string]string{
				platformv1alpha1.ManagedByLabel:         platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:           "tenant-profile",
				platformv1alpha1.PSAEnforceLabel:        "restricted",
				platformv1alpha1.PSAEnforceVersionLabel: "latest",
				platformv1alpha1.PSAAuditLabel:          "restricted",
				platformv1alpha1.PSAAuditVersionLabel:   "latest",
				platformv1alpha1.PSAWarnLabel:           "restricted",
				platformv1alpha1.PSAWarnVersionLabel:    "latest",
			},
		},
		{
			// The staged-rollout shape: measure what restricted would reject
			// while only enforcing baseline.
			name: "audit and warn can exceed the enforced level",
			security: &platformv1alpha1.SecuritySpec{
				PodSecurityStandard: platformv1alpha1.PodSecurityBaseline,
				Audit:               ptr.To(platformv1alpha1.PodSecurityRestricted),
				Warn:                ptr.To(platformv1alpha1.PodSecurityRestricted),
			},
			want: map[string]string{
				platformv1alpha1.ManagedByLabel:         platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:           "tenant-profile",
				platformv1alpha1.PSAEnforceLabel:        "baseline",
				platformv1alpha1.PSAEnforceVersionLabel: "latest",
				platformv1alpha1.PSAAuditLabel:          "restricted",
				platformv1alpha1.PSAAuditVersionLabel:   "latest",
				platformv1alpha1.PSAWarnLabel:           "restricted",
				platformv1alpha1.PSAWarnVersionLabel:    "latest",
			},
		},
		{
			// All three modes share one version, so audit and enforce cannot
			// disagree about what a level means — which is the entire point of
			// auditing at restricted while enforcing baseline.
			name: "a pinned version applies to all three modes",
			security: &platformv1alpha1.SecuritySpec{
				PodSecurityStandard: platformv1alpha1.PodSecurityRestricted,
				EnforceVersion:      "v1.34",
			},
			want: map[string]string{
				platformv1alpha1.ManagedByLabel:         platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:           "tenant-profile",
				platformv1alpha1.PSAEnforceLabel:        "restricted",
				platformv1alpha1.PSAEnforceVersionLabel: "v1.34",
				platformv1alpha1.PSAAuditLabel:          "restricted",
				platformv1alpha1.PSAAuditVersionLabel:   "v1.34",
				platformv1alpha1.PSAWarnLabel:           "restricted",
				platformv1alpha1.PSAWarnVersionLabel:    "v1.34",
			},
		},
		{
			name: "privileged is written out explicitly when asked for",
			security: &platformv1alpha1.SecuritySpec{
				PodSecurityStandard: platformv1alpha1.PodSecurityPrivileged,
			},
			want: map[string]string{
				platformv1alpha1.ManagedByLabel:         platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:           "tenant-profile",
				platformv1alpha1.PSAEnforceLabel:        "privileged",
				platformv1alpha1.PSAEnforceVersionLabel: "latest",
				platformv1alpha1.PSAAuditLabel:          "privileged",
				platformv1alpha1.PSAAuditVersionLabel:   "latest",
				platformv1alpha1.PSAWarnLabel:           "privileged",
				platformv1alpha1.PSAWarnVersionLabel:    "latest",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
				s.Security = tc.security
			})
			got := NamespaceLabels(profile)
			if !maps.Equal(tc.want, got) {
				t.Errorf("NamespaceLabels() mismatch\n  want: %v\n  got:  %v", tc.want, got)
			}
		})
	}
}

func TestNamespaceLabelsOnlyUsesKeysItDeclaresItManages(t *testing.T) {
	// The in-sync comparison examines only operatorNamespaceLabelKeys. If a
	// builder ever emitted a key outside that set, the operator would write a
	// label it then never checked — so it could be removed by anyone and never
	// restored.
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Security = &platformv1alpha1.SecuritySpec{
			PodSecurityStandard: platformv1alpha1.PodSecurityRestricted,
			EnforceVersion:      "v1.34",
		}
	})

	managed := make(map[string]bool, len(OperatorNamespaceLabelKeys()))
	for _, key := range OperatorNamespaceLabelKeys() {
		managed[key] = true
	}

	for key := range NamespaceLabels(profile) {
		if !managed[key] {
			t.Errorf("NamespaceLabels() emits %q, which is not in OperatorNamespaceLabelKeys()", key)
		}
	}
}

func TestNamespaceApplyConfiguration(t *testing.T) {
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Security = &platformv1alpha1.SecuritySpec{
			PodSecurityStandard: platformv1alpha1.PodSecurityBaseline,
		}
	})

	got := Namespace(profile, false)
	if *got.Name != "tenant" {
		t.Errorf("name = %q, want %q", *got.Name, "tenant")
	}
	if got.Labels[platformv1alpha1.PSAEnforceLabel] != "baseline" {
		t.Errorf("enforce label = %q, want baseline", got.Labels[platformv1alpha1.PSAEnforceLabel])
	}

	// A namespace's spec holds its finalizers. Claiming ownership of that field
	// could interfere with namespace teardown, so the operator declares
	// metadata and nothing else.
	if got.Spec != nil {
		t.Error("apply configuration declares namespace spec, which it must not own")
	}
	if got.Status != nil {
		t.Error("apply configuration declares namespace status, which it must not own")
	}
}

func TestNamespaceLabelsInSync(t *testing.T) {
	restricted := &platformv1alpha1.SecuritySpec{
		PodSecurityStandard: platformv1alpha1.PodSecurityRestricted,
	}

	tests := []struct {
		name     string
		security *platformv1alpha1.SecuritySpec
		live     map[string]string
		want     bool
	}{
		{
			name:     "an exact match needs no write",
			security: restricted,
			live: map[string]string{
				platformv1alpha1.ManagedByLabel:         platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:           "tenant-profile",
				platformv1alpha1.PSAEnforceLabel:        "restricted",
				platformv1alpha1.PSAEnforceVersionLabel: "latest",
				platformv1alpha1.PSAAuditLabel:          "restricted",
				platformv1alpha1.PSAAuditVersionLabel:   "latest",
				platformv1alpha1.PSAWarnLabel:           "restricted",
				platformv1alpha1.PSAWarnVersionLabel:    "latest",
			},
			want: true,
		},
		{
			// The central requirement for namespace metadata: unrelated labels
			// must be invisible to this operator, so a cost-allocation tag or a
			// service mesh label can never make a profile look out of sync, and
			// can never be stripped by it.
			name:     "unrelated labels are ignored entirely",
			security: restricted,
			live: map[string]string{
				platformv1alpha1.ManagedByLabel:         platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:           "tenant-profile",
				platformv1alpha1.PSAEnforceLabel:        "restricted",
				platformv1alpha1.PSAEnforceVersionLabel: "latest",
				platformv1alpha1.PSAAuditLabel:          "restricted",
				platformv1alpha1.PSAAuditVersionLabel:   "latest",
				platformv1alpha1.PSAWarnLabel:           "restricted",
				platformv1alpha1.PSAWarnVersionLabel:    "latest",
				"kubernetes.io/metadata.name":           "tenant",
				"istio-injection":                       "enabled",
				"cost-center":                           "platform-eng",
				"argocd.argoproj.io/instance":           "tenant-app",
			},
			want: true,
		},
		{
			name:     "a downgraded enforcement level is drift",
			security: restricted,
			live: map[string]string{
				platformv1alpha1.ManagedByLabel:         platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:           "tenant-profile",
				platformv1alpha1.PSAEnforceLabel:        "privileged",
				platformv1alpha1.PSAEnforceVersionLabel: "latest",
				platformv1alpha1.PSAAuditLabel:          "restricted",
				platformv1alpha1.PSAAuditVersionLabel:   "latest",
				platformv1alpha1.PSAWarnLabel:           "restricted",
				platformv1alpha1.PSAWarnVersionLabel:    "latest",
			},
			want: false,
		},
		{
			name:     "a deleted Pod Security label is drift",
			security: restricted,
			live: map[string]string{
				platformv1alpha1.ManagedByLabel: platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:   "tenant-profile",
			},
			want: false,
		},
		{
			// Removing spec.security must actually withdraw the labels. Were
			// this reported as in sync, the apply would be skipped and
			// server-side apply would never prune them.
			name:     "labels left over from a removed security section are drift",
			security: nil,
			live: map[string]string{
				platformv1alpha1.ManagedByLabel:         platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:           "tenant-profile",
				platformv1alpha1.PSAEnforceLabel:        "restricted",
				platformv1alpha1.PSAEnforceVersionLabel: "latest",
			},
			want: false,
		},
		{
			name:     "an unlabelled namespace is drift",
			security: nil,
			live:     nil,
			want:     false,
		},
		{
			name:     "a namespace labelled for a different profile is drift",
			security: nil,
			live: map[string]string{
				platformv1alpha1.ManagedByLabel: platformv1alpha1.OperatorName,
				platformv1alpha1.ProfileLabel:   "some-other-profile",
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
				s.Security = tc.security
			})
			actual := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "tenant", Labels: tc.live},
			}
			if got := NamespaceLabelsInSync(profile, actual); got != tc.want {
				t.Errorf("NamespaceLabelsInSync() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNamespaceAdoptionMarker(t *testing.T) {
	// The marker is what permanently bars the operator from deleting a
	// namespace it did not create, so it must appear only when asked for and
	// must survive once set.
	profile := profileWith(nil)

	if got := Namespace(profile, false); got.Annotations[platformv1alpha1.AdoptedAnnotation] != "" {
		t.Errorf("a created namespace must not be marked adopted, got %v", got.Annotations)
	}

	adopted := Namespace(profile, true)
	if adopted.Annotations[platformv1alpha1.AdoptedAnnotation] != "true" {
		t.Errorf("an adopted namespace must carry the marker, got %v", adopted.Annotations)
	}

	marked := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{platformv1alpha1.AdoptedAnnotation: "true"},
	}}
	if !WasAdopted(marked) {
		t.Error("WasAdopted() = false for a namespace carrying the marker")
	}
	if WasAdopted(&corev1.Namespace{}) {
		t.Error("WasAdopted() = true for a namespace with no annotations")
	}
}

func TestCanManage(t *testing.T) {
	profile := profileWith(nil)
	adopting := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.AdoptExisting = ptr.To(true)
	})

	ours := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Labels: OwnershipLabels(profile)}}
	if got := CanManage(ours, profile); got != ManageOwned {
		t.Errorf("CanManage(own object) = %v, want ManageOwned", got)
	}

	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"owner": "another-team"},
	}}
	if got := CanManage(foreign, profile); got != ManageRefuseForeign {
		t.Errorf("CanManage(foreign, no adoption) = %v, want ManageRefuseForeign", got)
	}
	if got := CanManage(foreign, adopting); got != ManageAdopt {
		t.Errorf("CanManage(foreign, adopting) = %v, want ManageAdopt", got)
	}

	// The check that stops adoption being a way to steal from a peer profile.
	otherProfile := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		platformv1alpha1.ManagedByLabel: platformv1alpha1.OperatorName,
		platformv1alpha1.ProfileLabel:   "someone-elses-profile",
	}}}
	if got := CanManage(otherProfile, profile); got != ManageRefuseOtherProfile {
		t.Errorf("CanManage(other profile) = %v, want ManageRefuseOtherProfile", got)
	}
	if got := CanManage(otherProfile, adopting); got != ManageRefuseOtherProfile {
		t.Errorf("CanManage(other profile, adopting) = %v, want ManageRefuseOtherProfile; "+
			"adoption must never override another profile's ownership", got)
	}

}
