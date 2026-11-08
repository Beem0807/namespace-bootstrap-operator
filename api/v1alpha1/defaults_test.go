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

package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// These accessors are the single place the API's defaults live. Every caller
// reads optional fields through them, so a wrong default here is a wrong
// default everywhere — and because they are nil-safe by design, the nil case
// matters as much as the set one.

func TestBoolAccessorsDefaultWhenUnset(t *testing.T) {
	tests := []struct {
		name string
		got  func() bool
		want bool
	}{
		// Adoption is off unless asked for: taking over a namespace the
		// operator did not create is never the safe assumption.
		{"AdoptionAllowed/nil spec field", func() bool { return (&NamespaceProfileSpec{}).AdoptionAllowed() }, false},
		{"AdoptionAllowed/true", func() bool {
			return (&NamespaceProfileSpec{AdoptExisting: ptr.To(true)}).AdoptionAllowed()
		}, true},
		{"AdoptionAllowed/false", func() bool {
			return (&NamespaceProfileSpec{AdoptExisting: ptr.To(false)}).AdoptionAllowed()
		}, false},

		// The two directions deliberately default differently: fencing inbound
		// traffic is the point of asking for a network section, while denying
		// egress is the most disruptive setting here and must be asked for.
		{"DenyIngressEnabled/unset", func() bool { return (&NetworkSpec{}).DenyIngressEnabled() }, true},
		{"DenyIngressEnabled/explicit false", func() bool {
			return (&NetworkSpec{DefaultDenyIngress: ptr.To(false)}).DenyIngressEnabled()
		}, false},
		{"DenyEgressEnabled/unset", func() bool { return (&NetworkSpec{}).DenyEgressEnabled() }, false},
		{"DenyEgressEnabled/explicit true", func() bool {
			return (&NetworkSpec{DefaultDenyEgress: ptr.To(true)}).DenyEgressEnabled()
		}, true},

		// DNS defaults ON. A default-deny egress policy without a DNS
		// exception takes name resolution down with it, which is the single
		// most destructive misconfiguration this API can express.
		{"DNSAllowed/unset", func() bool { return (&NetworkSpec{}).DNSAllowed() }, true},
		{"DNSAllowed/explicit false", func() bool {
			return (&NetworkSpec{AllowDNS: ptr.To(false)}).DNSAllowed()
		}, false},

		{"SameNamespaceAllowed/unset", func() bool { return (&NetworkSpec{}).SameNamespaceAllowed() }, true},
		{"SameNamespaceAllowed/explicit false", func() bool {
			return (&NetworkSpec{AllowSameNamespace: ptr.To(false)}).SameNamespaceAllowed()
		}, false},

		// Both lifecycle flags default off: deleting a namespace and
		// abandoning generated policy are each opt-in.
		{"NamespaceDeletionRequested/unset", func() bool { return (&LifecycleSpec{}).NamespaceDeletionRequested() }, false},
		{"NamespaceDeletionRequested/true", func() bool {
			return (&LifecycleSpec{DeleteNamespaceOnRemoval: ptr.To(true)}).NamespaceDeletionRequested()
		}, true},
		{"ManagedResourcesRetained/unset", func() bool { return (&LifecycleSpec{}).ManagedResourcesRetained() }, false},
		{"ManagedResourcesRetained/true", func() bool {
			return (&LifecycleSpec{RetainManagedResources: ptr.To(true)}).ManagedResourcesRetained()
		}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.got(); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSecurityLevelsFallBackToTheEnforcedLevel(t *testing.T) {
	// audit and warn default to whatever is enforced, so that stating one
	// level is enough for the common case and a staged rollout is the
	// deliberate exception rather than the thing you must remember.
	enforced := &SecuritySpec{PodSecurityStandard: PodSecurityRestricted}
	if got := enforced.AuditLevel(); got != PodSecurityRestricted {
		t.Errorf("AuditLevel() = %q, want it to follow podSecurityStandard", got)
	}
	if got := enforced.WarnLevel(); got != PodSecurityRestricted {
		t.Errorf("WarnLevel() = %q, want it to follow podSecurityStandard", got)
	}

	staged := &SecuritySpec{
		PodSecurityStandard: PodSecurityBaseline,
		Audit:               ptr.To(PodSecurityRestricted),
		Warn:                ptr.To(PodSecurityRestricted),
	}
	if got := staged.AuditLevel(); got != PodSecurityRestricted {
		t.Errorf("AuditLevel() = %q, want the explicit value", got)
	}
	if got := staged.WarnLevel(); got != PodSecurityRestricted {
		t.Errorf("WarnLevel() = %q, want the explicit value", got)
	}
}

func TestStandardsVersionDefaultsToLatest(t *testing.T) {
	if got := (&SecuritySpec{}).StandardsVersion(); got != "latest" {
		t.Errorf("StandardsVersion() = %q, want \"latest\"", got)
	}
	if got := (&SecuritySpec{EnforceVersion: "v1.34"}).StandardsVersion(); got != "v1.34" {
		t.Errorf("StandardsVersion() = %q, want the pinned version", got)
	}
}

// DNSConfig's defaults are what a generated DNS egress policy is built from.
// Getting one wrong produces a policy that matches no DNS pods, which combined
// with defaultDenyEgress takes name resolution down for the whole namespace.
func TestDNSConfigDefaults(t *testing.T) {
	got := (&NetworkSpec{}).DNSConfig()

	if got.Namespace != "kube-system" {
		t.Errorf("namespace = %q, want kube-system", got.Namespace)
	}
	if got.PodSelector == nil || got.PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Errorf("podSelector = %+v, want k8s-app=kube-dns", got.PodSelector)
	}

	// Both protocols, deliberately: resolvers fall back to TCP for responses
	// too large for UDP, so a UDP-only allowance fails intermittently and very
	// confusingly.
	if len(got.Ports) != 2 {
		t.Fatalf("got %d ports, want both UDP and TCP", len(got.Ports))
	}
	protocols := map[corev1.Protocol]int32{}
	for _, p := range got.Ports {
		protocols[p.Protocol] = p.Port
	}
	if protocols[corev1.ProtocolUDP] != 53 || protocols[corev1.ProtocolTCP] != 53 {
		t.Errorf("ports = %+v, want UDP/53 and TCP/53", got.Ports)
	}
}

func TestDNSConfigKeepsWhatWasSet(t *testing.T) {
	custom := &NetworkSpec{DNS: &DNSEgressSpec{
		Namespace:   "dns-system",
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "coredns"}},
		Ports:       []NetworkPort{{Protocol: corev1.ProtocolUDP, Port: 5353}},
	}}

	got := custom.DNSConfig()
	if got.Namespace != "dns-system" {
		t.Errorf("namespace = %q, want the configured one", got.Namespace)
	}
	if got.PodSelector.MatchLabels["app"] != "coredns" {
		t.Errorf("podSelector = %+v, want the configured one", got.PodSelector)
	}
	if len(got.Ports) != 1 || got.Ports[0].Port != 5353 {
		t.Errorf("ports = %+v, want only the configured port", got.Ports)
	}
}

func TestKubeAPIPortsDefaultsToHTTPS(t *testing.T) {
	got := (&KubeAPIEgressSpec{}).KubeAPIPorts()
	if len(got) != 1 || got[0].Protocol != corev1.ProtocolTCP || got[0].Port != 443 {
		t.Errorf("got %+v, want TCP/443", got)
	}

	custom := (&KubeAPIEgressSpec{Ports: []NetworkPort{{Protocol: corev1.ProtocolTCP, Port: 6443}}}).KubeAPIPorts()
	if len(custom) != 1 || custom[0].Port != 6443 {
		t.Errorf("got %+v, want the configured port", custom)
	}
}
