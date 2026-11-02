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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// Defaulting in this API happens in the API server, declared by
// +kubebuilder:default markers on the fields themselves. An object read back
// from the cluster therefore arrives fully defaulted, and `kubectl explain`
// reports the defaults to users.
//
// Optional fields carrying a default are pointers rather than plain values.
// A non-pointer optional field cannot distinguish "the user said nothing" from
// "the user said the zero value": a Go client building a partial struct would
// serialise an explicit false or "", the API server would see a value and skip
// defaulting, and a field documented as defaulting to true would silently
// arrive as false.
//
// The accessors below read those pointers. They repeat each field's default so
// that a hand-constructed spec — in a unit test, say — behaves the same as one
// that has been through the API server. A spec test asserts the two agree, so
// a marker changed without its accessor fails the build rather than producing
// two disagreeing notions of the default.

// AdoptionAllowed reports whether the operator may take over a pre-existing
// namespace or managed resource. Defaults to false: refuse rather than seize.
func (s *NamespaceProfileSpec) AdoptionAllowed() bool {
	return ptr.Deref(s.AdoptExisting, false)
}

// DenyIngressEnabled reports whether a default-deny ingress policy should be
// generated. Defaults to true, since fencing inbound traffic is the point of
// asking for a network section at all.
func (n *NetworkSpec) DenyIngressEnabled() bool {
	return ptr.Deref(n.DefaultDenyIngress, true)
}

// DenyEgressEnabled reports whether a default-deny egress policy should be
// generated. Defaults to false: it is the most disruptive setting in the API
// and must be asked for explicitly.
func (n *NetworkSpec) DenyEgressEnabled() bool {
	return ptr.Deref(n.DefaultDenyEgress, false)
}

// DNSAllowed reports whether egress to cluster DNS should be permitted.
// Defaults to true, so enabling a default-deny egress does not take DNS down
// with it.
func (n *NetworkSpec) DNSAllowed() bool {
	return ptr.Deref(n.AllowDNS, true)
}

// SameNamespaceAllowed reports whether pods in the namespace may talk to each
// other. Defaults to true, because a namespace whose own pods cannot reach
// each other is almost never what was intended.
func (n *NetworkSpec) SameNamespaceAllowed() bool {
	return ptr.Deref(n.AllowSameNamespace, true)
}

// NamespaceDeletionRequested reports whether the profile asks for the target
// namespace to be deleted on removal. Defaults to false.
//
// This is only the request. The operator applies further checks — the
// confirmation annotation, ownership labels, and whether the namespace was
// adopted — before acting on it.
func (l *LifecycleSpec) NamespaceDeletionRequested() bool {
	return ptr.Deref(l.DeleteNamespaceOnRemoval, false)
}

// ManagedResourcesRetained reports whether the generated quota, limit range
// and network policies survive the profile's deletion. Defaults to false, so
// cleanup leaves nothing behind that no profile explains.
func (l *LifecycleSpec) ManagedResourcesRetained() bool {
	return ptr.Deref(l.RetainManagedResources, false)
}

// AuditLevel is the Pod Security Standards level to audit against, defaulting
// to the enforced level.
//
// This cannot be a schema default: the fallback is the value of a sibling
// field, spec.security.podSecurityStandard, rather than a constant.
func (s *SecuritySpec) AuditLevel() PodSecurityLevel {
	if s.Audit != nil {
		return *s.Audit
	}
	return s.PodSecurityStandard
}

// WarnLevel is the Pod Security Standards level to warn against, defaulting to
// the enforced level.
func (s *SecuritySpec) WarnLevel() PodSecurityLevel {
	if s.Warn != nil {
		return *s.Warn
	}
	return s.PodSecurityStandard
}

// StandardsVersion is the Pod Security Standards version to pin the namespace
// labels to, defaulting to "latest".
//
// "latest" tracks whatever the API server currently defines, so a cluster
// upgrade can tighten the rules and begin rejecting workloads that previously
// passed. Pinning to a minor such as "v1.34" freezes the definition so an
// upgrade cannot change admission behaviour, at the cost of having to raise it
// deliberately.
func (s *SecuritySpec) StandardsVersion() string {
	if s.EnforceVersion == "" {
		return "latest"
	}
	return s.EnforceVersion
}

// DNSConfig resolves how the cluster DNS service is identified for the
// generated DNS egress allowance.
//
// The API server materialises spec.network.dns and defaults its fields, so a
// profile read from the cluster already carries them. This restates the same
// values for a spec built in Go, and a spec test asserts the two agree — the
// defaults a generated NetworkPolicy depends on are not something to let drift
// silently, because the failure mode is a policy that matches no DNS pods and
// takes name resolution down with it.
func (n *NetworkSpec) DNSConfig() DNSEgressSpec {
	config := DNSEgressSpec{}
	if n.DNS != nil {
		config = *n.DNS
	}

	if config.Namespace == "" {
		config.Namespace = "kube-system"
	}
	if config.PodSelector == nil {
		config.PodSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"k8s-app": "kube-dns"},
		}
	}
	if len(config.Ports) == 0 {
		// Both protocols: resolvers fall back to TCP for responses too large
		// for UDP, so permitting only UDP breaks large responses
		// intermittently and very confusingly.
		config.Ports = []NetworkPort{
			{Protocol: corev1.ProtocolUDP, Port: 53},
			{Protocol: corev1.ProtocolTCP, Port: 53},
		}
	}
	return config
}

// KubeAPIPorts resolves which API server ports to permit, defaulting to TCP
// port 443.
func (k *KubeAPIEgressSpec) KubeAPIPorts() []NetworkPort {
	if len(k.Ports) > 0 {
		return k.Ports
	}
	return []NetworkPort{{Protocol: corev1.ProtocolTCP, Port: 443}}
}
