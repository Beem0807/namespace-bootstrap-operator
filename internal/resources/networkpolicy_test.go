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
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

// networkProfile builds a profile whose network section is exactly the four
// toggles given, so each test states only the configuration it is about.
func networkProfile(denyIngress, denyEgress, allowDNS, allowSameNamespace bool) *platformv1alpha1.NamespaceProfile {
	return profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Network = &platformv1alpha1.NetworkSpec{
			DefaultDenyIngress: ptr.To(denyIngress),
			DefaultDenyEgress:  ptr.To(denyEgress),
			AllowDNS:           ptr.To(allowDNS),
			AllowSameNamespace: ptr.To(allowSameNamespace),
		}
	})
}

func policyNames(policies []NetworkPolicy) []string {
	names := make([]string, 0, len(policies))
	for _, policy := range policies {
		names = append(names, policy.Name)
	}
	return names
}

func findPolicy(t *testing.T, policies []NetworkPolicy, name string) NetworkPolicy {
	t.Helper()
	for _, policy := range policies {
		if policy.Name == name {
			return policy
		}
	}
	t.Fatalf("policy %q was not generated; got %v", name, policyNames(policies))
	return NetworkPolicy{}
}

func TestNetworkPoliciesGeneratedSet(t *testing.T) {
	tests := []struct {
		name               string
		denyIngress        bool
		denyEgress         bool
		allowDNS           bool
		allowSameNamespace bool
		want               []string
	}{
		{
			name: "all toggles off produces no policies at all",
			want: nil,
		},
		{
			name:        "the default shape: deny ingress, allow intra-namespace traffic",
			denyIngress: true, allowSameNamespace: true, allowDNS: true,
			want: []string{
				platformv1alpha1.DenyIngressPolicyName,
				platformv1alpha1.AllowSameNamespacePolicyName,
			},
		},
		{
			// allowDNS grants nothing without a default deny on egress, since
			// egress is already unrestricted. Generating the policy anyway
			// would imply the namespace is fenced when it is not.
			name:        "allowDNS alone generates no DNS policy",
			denyIngress: true, allowDNS: true,
			want: []string{platformv1alpha1.DenyIngressPolicyName},
		},
		{
			name:       "denying egress brings the DNS allowance with it",
			denyEgress: true, allowDNS: true,
			want: []string{
				platformv1alpha1.DenyEgressPolicyName,
				platformv1alpha1.AllowDNSPolicyName,
			},
		},
		{
			name:        "the fully fenced shape",
			denyIngress: true, denyEgress: true, allowDNS: true, allowSameNamespace: true,
			want: []string{
				platformv1alpha1.DenyIngressPolicyName,
				platformv1alpha1.DenyEgressPolicyName,
				platformv1alpha1.AllowDNSPolicyName,
				platformv1alpha1.AllowSameNamespacePolicyName,
			},
		},
		{
			// Nothing is denied, so there is nothing to make an exception for.
			// A same-namespace policy here would be worse than useless: see
			// TestSameNamespacePolicyNeverRestrictsAnUnaskedDirection.
			name:               "same-namespace traffic with nothing denied generates nothing",
			allowSameNamespace: true,
			want:               nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			profile := networkProfile(tc.denyIngress, tc.denyEgress, tc.allowDNS, tc.allowSameNamespace)
			got := policyNames(NetworkPolicies(profile))
			if !slices.Equal(tc.want, got) {
				t.Errorf("generated policies mismatch\n  want: %v\n  got:  %v", tc.want, got)
			}
		})
	}
}

func TestNetworkPoliciesAreNotGeneratedWithoutANetworkSection(t *testing.T) {
	// Omitting the section means the operator manages no policies, which is
	// different from managing an empty set — it must not touch policies
	// somebody else wrote.
	if got := NetworkPolicies(profileWith(nil)); got != nil {
		t.Errorf("NetworkPolicies() = %v, want nil when spec.network is absent", policyNames(got))
	}
}

func TestDefaultDenyPolicies(t *testing.T) {
	policies := NetworkPolicies(networkProfile(true, true, false, false))

	ingress := findPolicy(t, policies, platformv1alpha1.DenyIngressPolicyName)
	if !apiequality.Semantic.DeepEqual(metav1.LabelSelector{}, ingress.Spec.PodSelector) {
		t.Errorf("deny-ingress must select every pod, got %+v", ingress.Spec.PodSelector)
	}
	if !slices.Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, ingress.Spec.PolicyTypes) {
		t.Errorf("deny-ingress policyTypes = %v, want [Ingress]", ingress.Spec.PolicyTypes)
	}
	// No rules is what makes it a deny: the direction is restricted and
	// nothing is permitted.
	if len(ingress.Spec.Ingress) != 0 {
		t.Errorf("deny-ingress must permit nothing, got %+v", ingress.Spec.Ingress)
	}
	if len(ingress.Spec.Egress) != 0 {
		t.Error("deny-ingress must not mention egress at all")
	}

	egress := findPolicy(t, policies, platformv1alpha1.DenyEgressPolicyName)
	if !slices.Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, egress.Spec.PolicyTypes) {
		t.Errorf("deny-egress policyTypes = %v, want [Egress]", egress.Spec.PolicyTypes)
	}
	if len(egress.Spec.Egress) != 0 {
		t.Errorf("deny-egress must permit nothing, got %+v", egress.Spec.Egress)
	}
}

func TestSameNamespacePolicyNeverRestrictsAnUnaskedDirection(t *testing.T) {
	// The sharpest edge in NetworkPolicy: a pod is restricted for a direction
	// as soon as some policy selects it and names that direction in
	// policyTypes. So if the same-namespace *allowance* named Egress while the
	// profile had not asked to deny egress, it would restrict egress to this
	// namespace only — silently cutting off DNS, the API server and the
	// internet. The allowance must only ever name directions already denied.
	t.Run("ingress denied only", func(t *testing.T) {
		policies := NetworkPolicies(networkProfile(true, false, false, true))
		policy := findPolicy(t, policies, platformv1alpha1.AllowSameNamespacePolicyName)

		if !slices.Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, policy.Spec.PolicyTypes) {
			t.Fatalf("policyTypes = %v, want [Ingress] only", policy.Spec.PolicyTypes)
		}
		if len(policy.Spec.Egress) != 0 {
			t.Error("an egress rule here would restrict egress the profile never denied")
		}
		if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 1 {
			t.Fatalf("want one ingress rule with one peer, got %+v", policy.Spec.Ingress)
		}
		peer := policy.Spec.Ingress[0].From[0]
		if peer.PodSelector == nil {
			t.Fatal("the peer must select pods")
		}
		// An empty pod selector with no namespace selector means "every pod in
		// this policy's own namespace", which is exactly the intent.
		if !apiequality.Semantic.DeepEqual(metav1.LabelSelector{}, *peer.PodSelector) {
			t.Errorf("peer pod selector = %+v, want empty", *peer.PodSelector)
		}
		if peer.NamespaceSelector != nil {
			t.Error("a namespace selector would widen this beyond the namespace")
		}
	})

	t.Run("egress denied only", func(t *testing.T) {
		policies := NetworkPolicies(networkProfile(false, true, false, true))
		policy := findPolicy(t, policies, platformv1alpha1.AllowSameNamespacePolicyName)

		if !slices.Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, policy.Spec.PolicyTypes) {
			t.Fatalf("policyTypes = %v, want [Egress] only", policy.Spec.PolicyTypes)
		}
		if len(policy.Spec.Ingress) != 0 {
			t.Error("an ingress rule here would restrict ingress the profile never denied")
		}
		if len(policy.Spec.Egress) != 1 {
			t.Fatalf("want one egress rule, got %+v", policy.Spec.Egress)
		}
	})

	t.Run("both denied", func(t *testing.T) {
		policies := NetworkPolicies(networkProfile(true, true, false, true))
		policy := findPolicy(t, policies, platformv1alpha1.AllowSameNamespacePolicyName)

		if !slices.Equal([]networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress,
		}, policy.Spec.PolicyTypes) {
			t.Errorf("policyTypes = %v, want [Ingress Egress]", policy.Spec.PolicyTypes)
		}
		if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Egress) != 1 {
			t.Error("both directions should be permitted within the namespace")
		}
	})
}

func TestDNSPolicyUsesTheDefaultedConfiguration(t *testing.T) {
	policies := NetworkPolicies(networkProfile(false, true, true, false))
	policy := findPolicy(t, policies, platformv1alpha1.AllowDNSPolicyName)

	if !slices.Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, policy.Spec.PolicyTypes) {
		t.Errorf("policyTypes = %v, want [Egress]", policy.Spec.PolicyTypes)
	}
	if len(policy.Spec.Egress) != 1 || len(policy.Spec.Egress[0].To) != 1 {
		t.Fatalf("want one egress rule with one peer, got %+v", policy.Spec.Egress)
	}

	peer := policy.Spec.Egress[0].To[0]

	// A peer can only name a namespace by label, and this is the label the API
	// server maintains on every namespace — so it works without cooperation
	// from whoever owns the DNS namespace.
	if peer.NamespaceSelector == nil {
		t.Fatal("the DNS peer must select the DNS namespace")
	}
	if got := peer.NamespaceSelector.MatchLabels[namespaceNameLabel]; got != "kube-system" {
		t.Errorf("DNS namespace selector = %q, want kube-system", got)
	}
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Errorf("DNS pod selector = %+v, want k8s-app=kube-dns", peer.PodSelector)
	}

	// Both protocols: resolvers fall back to TCP for responses too large for
	// UDP, so UDP alone breaks large responses intermittently.
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	port53 := intstr.FromInt32(53)
	want := []networkingv1.NetworkPolicyPort{
		{Protocol: &udp, Port: &port53},
		{Protocol: &tcp, Port: &port53},
	}
	if !apiequality.Semantic.DeepEqual(want, policy.Spec.Egress[0].Ports) {
		t.Errorf("DNS ports mismatch\n  want: %+v\n  got:  %+v", want, policy.Spec.Egress[0].Ports)
	}
}

func TestDNSPolicyHonoursAnOverriddenConfiguration(t *testing.T) {
	// Clusters that deploy DNS elsewhere, label it differently, or run it on
	// another port must be able to say so — otherwise the generated policy
	// matches no DNS pods and takes name resolution down with it.
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Network = &platformv1alpha1.NetworkSpec{
			DefaultDenyEgress: ptr.To(true),
			AllowDNS:          ptr.To(true),
			DNS: &platformv1alpha1.DNSEgressSpec{
				Namespace: "openshift-dns",
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"dns.operator.openshift.io/daemonset-dns": "default"},
				},
				Ports: []platformv1alpha1.NetworkPort{{Protocol: corev1.ProtocolUDP, Port: 5353}},
			},
		}
	})

	policy := findPolicy(t, NetworkPolicies(profile), platformv1alpha1.AllowDNSPolicyName)
	peer := policy.Spec.Egress[0].To[0]

	if got := peer.NamespaceSelector.MatchLabels[namespaceNameLabel]; got != "openshift-dns" {
		t.Errorf("DNS namespace = %q, want openshift-dns", got)
	}
	if peer.PodSelector.MatchLabels["dns.operator.openshift.io/daemonset-dns"] != "default" {
		t.Errorf("DNS pod selector not carried through: %+v", peer.PodSelector)
	}
	if len(policy.Spec.Egress[0].Ports) != 1 {
		t.Fatalf("want one port, got %+v", policy.Spec.Egress[0].Ports)
	}
	if got := policy.Spec.Egress[0].Ports[0].Port.IntValue(); got != 5353 {
		t.Errorf("DNS port = %d, want 5353", got)
	}
}

func TestNetworkPoliciesAreDeterministic(t *testing.T) {
	// Generated resources must not vary between reconciliations, or the
	// operator would rewrite them forever.
	profile := networkProfile(true, true, true, true)
	first := NetworkPolicies(profile)

	for range 50 {
		if !apiequality.Semantic.DeepEqual(first, NetworkPolicies(profile)) {
			t.Fatal("NetworkPolicies() is not deterministic across calls")
		}
	}
}

func TestNetworkPolicyNames(t *testing.T) {
	names := NetworkPolicyNames(networkProfile(true, false, true, true))
	if !names[platformv1alpha1.DenyIngressPolicyName] {
		t.Error("the declared set should contain the deny-ingress policy")
	}
	if names[platformv1alpha1.DenyEgressPolicyName] {
		t.Error("the declared set must not contain a policy the profile does not generate")
	}
	if len(names) != 2 {
		t.Errorf("declared set = %v, want 2 entries", names)
	}
}

func TestNetworkPolicyObjectShape(t *testing.T) {
	profile := networkProfile(true, false, false, false)
	policy := findPolicy(t, NetworkPolicies(profile), platformv1alpha1.DenyIngressPolicyName)

	object := NetworkPolicyObject(profile, policy)

	if object.Name != platformv1alpha1.DenyIngressPolicyName {
		t.Errorf("name = %q, want %q", object.Name, platformv1alpha1.DenyIngressPolicyName)
	}
	if object.Namespace != "tenant" {
		t.Errorf("namespace = %q, want tenant", object.Namespace)
	}
	if object.Labels[platformv1alpha1.ManagedByLabel] != platformv1alpha1.OperatorName {
		t.Errorf("missing ownership labels, got %v", object.Labels)
	}
	if object.Labels[platformv1alpha1.ProfileLabel] != "tenant-profile" {
		t.Errorf("missing profile label, got %v", object.Labels)
	}
	if object.Annotations[platformv1alpha1.AppliedGenerationAnnotation] != "1" {
		t.Errorf("applied-generation = %q, want 1",
			object.Annotations[platformv1alpha1.AppliedGenerationAnnotation])
	}
	if len(object.Spec.PolicyTypes) != 1 {
		t.Errorf("policyTypes = %v, want exactly [Ingress]", object.Spec.PolicyTypes)
	}
	// A default deny is precisely a policy with no rules. This is the state an
	// apply configuration could not express, and the reason these objects are
	// written with create and update instead.
	if len(object.Spec.Ingress) != 0 || len(object.Spec.Egress) != 0 {
		t.Errorf("a default-deny policy must carry no rules, got ingress=%v egress=%v",
			object.Spec.Ingress, object.Spec.Egress)
	}
}

func TestNetworkPolicyObjectDoesNotAliasTheProfile(t *testing.T) {
	// The spec is deep-copied, so mutating a rendered object cannot corrupt the
	// value a later reconciliation computes from the same profile.
	profile := networkProfile(true, true, true, true)
	policy := findPolicy(t, NetworkPolicies(profile), platformv1alpha1.AllowSameNamespacePolicyName)

	object := NetworkPolicyObject(profile, policy)
	object.Spec.PolicyTypes = nil
	object.Spec.Ingress = nil

	if len(policy.Spec.PolicyTypes) == 0 || len(policy.Spec.Ingress) == 0 {
		t.Error("NetworkPolicyObject aliased the desired spec instead of copying it")
	}
}

func TestNetworkPolicyInSync(t *testing.T) {
	profile := networkProfile(true, true, true, true)
	policy := findPolicy(t, NetworkPolicies(profile), platformv1alpha1.AllowSameNamespacePolicyName)

	t.Run("an identical spec needs no write", func(t *testing.T) {
		actual := &networkingv1.NetworkPolicy{Spec: policy.Spec}
		if !NetworkPolicyInSync(policy.Spec, actual) {
			t.Error("NetworkPolicyInSync() = false, want true")
		}
	})

	t.Run("a removed rule is drift", func(t *testing.T) {
		stripped := *policy.Spec.DeepCopy()
		stripped.Egress = nil
		actual := &networkingv1.NetworkPolicy{Spec: stripped}
		if NetworkPolicyInSync(policy.Spec, actual) {
			t.Error("NetworkPolicyInSync() = true, want false when a rule was removed")
		}
	})

	t.Run("a widened peer is drift", func(t *testing.T) {
		// The case that matters: somebody opens the namespace to everything by
		// adding an empty namespace selector.
		widened := *policy.Spec.DeepCopy()
		widened.Ingress[0].From[0].NamespaceSelector = &metav1.LabelSelector{}
		actual := &networkingv1.NetworkPolicy{Spec: widened}
		if NetworkPolicyInSync(policy.Spec, actual) {
			t.Error("NetworkPolicyInSync() = true, want false when a peer was widened")
		}
	})

	t.Run("a dropped policy type is drift", func(t *testing.T) {
		weakened := *policy.Spec.DeepCopy()
		weakened.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		actual := &networkingv1.NetworkPolicy{Spec: weakened}
		if NetworkPolicyInSync(policy.Spec, actual) {
			t.Error("NetworkPolicyInSync() = true, want false when a policy type was dropped")
		}
	})
}

func TestKubeAPIPolicy(t *testing.T) {
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Network = &platformv1alpha1.NetworkSpec{
			DefaultDenyEgress: ptr.To(true),
			AllowDNS:          ptr.To(false),
			AllowKubeAPI: &platformv1alpha1.KubeAPIEgressSpec{
				// Deliberately out of order, to pin down that the output is sorted.
				Endpoints: []string{"10.0.0.2/32", "10.0.0.1/32"},
			},
		}
	})

	policy := findPolicy(t, NetworkPolicies(profile), platformv1alpha1.AllowKubeAPIPolicyName)

	if !slices.Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, policy.Spec.PolicyTypes) {
		t.Errorf("policyTypes = %v, want [Egress]", policy.Spec.PolicyTypes)
	}
	if len(policy.Spec.Egress) != 1 {
		t.Fatalf("want one egress rule, got %+v", policy.Spec.Egress)
	}

	// ipBlock rather than a selector: the API server is generally not a pod, so
	// no pod or namespace selector can reach it.
	peers := policy.Spec.Egress[0].To
	if len(peers) != 2 {
		t.Fatalf("want one peer per endpoint, got %+v", peers)
	}
	for i, want := range []string{"10.0.0.1/32", "10.0.0.2/32"} {
		if peers[i].IPBlock == nil {
			t.Fatalf("peer %d must be an ipBlock, got %+v", i, peers[i])
		}
		if peers[i].IPBlock.CIDR != want {
			t.Errorf("peer %d CIDR = %q, want %q (sorted)", i, peers[i].IPBlock.CIDR, want)
		}
		if peers[i].PodSelector != nil || peers[i].NamespaceSelector != nil {
			t.Errorf("peer %d must not carry selectors alongside an ipBlock", i)
		}
	}

	tcp := corev1.ProtocolTCP
	port443 := intstr.FromInt32(443)
	want := []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port443}}
	if !apiequality.Semantic.DeepEqual(want, policy.Spec.Egress[0].Ports) {
		t.Errorf("ports mismatch\n  want: %+v\n  got:  %+v", want, policy.Spec.Egress[0].Ports)
	}
}

func TestKubeAPIPolicyHonoursExplicitPorts(t *testing.T) {
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Network = &platformv1alpha1.NetworkSpec{
			DefaultDenyEgress: ptr.To(true),
			AllowDNS:          ptr.To(false),
			AllowKubeAPI: &platformv1alpha1.KubeAPIEgressSpec{
				Endpoints: []string{"172.20.0.1/32"},
				Ports:     []platformv1alpha1.NetworkPort{{Protocol: corev1.ProtocolTCP, Port: 6443}},
			},
		}
	})

	policy := findPolicy(t, NetworkPolicies(profile), platformv1alpha1.AllowKubeAPIPolicyName)
	if got := policy.Spec.Egress[0].Ports[0].Port.IntValue(); got != 6443 {
		t.Errorf("port = %d, want 6443", got)
	}
}

func TestKubeAPIPolicyRequiresADefaultDenyEgress(t *testing.T) {
	// Without a default deny, egress is already unrestricted and the policy
	// would grant nothing while implying the namespace is fenced.
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Network = &platformv1alpha1.NetworkSpec{
			DefaultDenyIngress: ptr.To(true),
			AllowSameNamespace: ptr.To(false),
			AllowKubeAPI: &platformv1alpha1.KubeAPIEgressSpec{
				Endpoints: []string{"10.0.0.1/32"},
			},
		}
	})

	if got := policyNames(NetworkPolicies(profile)); slices.Contains(got, platformv1alpha1.AllowKubeAPIPolicyName) {
		t.Errorf("generated an inert kube-api policy: %v", got)
	}
}

func TestCrossNamespaceAllowances(t *testing.T) {
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Network = &platformv1alpha1.NetworkSpec{
			DefaultDenyIngress:         ptr.To(true),
			DefaultDenyEgress:          ptr.To(true),
			AllowDNS:                   ptr.To(false),
			AllowSameNamespace:         ptr.To(false),
			AllowEgressToNamespaces:    []string{"observability", "platform-shared"},
			AllowIngressFromNamespaces: []string{"monitoring", "ingress-nginx"},
		}
	})

	policies := NetworkPolicies(profile)

	t.Run("egress", func(t *testing.T) {
		policy := findPolicy(t, policies, platformv1alpha1.AllowEgressToNamespacesPolicyName)
		if !slices.Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, policy.Spec.PolicyTypes) {
			t.Errorf("policyTypes = %v, want [Egress] only", policy.Spec.PolicyTypes)
		}
		if len(policy.Spec.Ingress) != 0 {
			t.Error("an ingress rule here would restrict ingress this allowance never intended")
		}

		// One peer with a set-based requirement, so the object stays compact
		// however long the list and is byte-identical on every pass.
		peers := policy.Spec.Egress[0].To
		if len(peers) != 1 {
			t.Fatalf("want a single peer, got %+v", peers)
		}
		requirements := peers[0].NamespaceSelector.MatchExpressions
		if len(requirements) != 1 {
			t.Fatalf("want one requirement, got %+v", requirements)
		}
		if requirements[0].Key != namespaceNameLabel {
			t.Errorf("selector key = %q, want %q", requirements[0].Key, namespaceNameLabel)
		}
		if requirements[0].Operator != metav1.LabelSelectorOpIn {
			t.Errorf("operator = %q, want In", requirements[0].Operator)
		}
		if !slices.Equal([]string{"observability", "platform-shared"}, requirements[0].Values) {
			t.Errorf("values = %v, want sorted [observability platform-shared]", requirements[0].Values)
		}
	})

	t.Run("ingress", func(t *testing.T) {
		policy := findPolicy(t, policies, platformv1alpha1.AllowIngressFromNamespacesPolicyName)
		if !slices.Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, policy.Spec.PolicyTypes) {
			t.Errorf("policyTypes = %v, want [Ingress] only", policy.Spec.PolicyTypes)
		}
		if len(policy.Spec.Egress) != 0 {
			t.Error("an egress rule here would restrict egress this allowance never intended")
		}
		values := policy.Spec.Ingress[0].From[0].NamespaceSelector.MatchExpressions[0].Values
		if !slices.Equal([]string{"ingress-nginx", "monitoring"}, values) {
			t.Errorf("values = %v, want sorted [ingress-nginx monitoring]", values)
		}
	})
}

func TestCrossNamespaceAllowancesFollowTheirDirection(t *testing.T) {
	// Each allowance is only generated when its own direction is denied, for
	// the same reason the same-namespace policy is: naming a direction is what
	// restricts it.
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Network = &platformv1alpha1.NetworkSpec{
			DefaultDenyIngress:         ptr.To(true),
			DefaultDenyEgress:          ptr.To(false),
			AllowSameNamespace:         ptr.To(false),
			AllowEgressToNamespaces:    []string{"platform-shared"},
			AllowIngressFromNamespaces: []string{"ingress-nginx"},
		}
	})

	names := policyNames(NetworkPolicies(profile))
	if slices.Contains(names, platformv1alpha1.AllowEgressToNamespacesPolicyName) {
		t.Errorf("egress allowance generated without a default deny on egress: %v", names)
	}
	if !slices.Contains(names, platformv1alpha1.AllowIngressFromNamespacesPolicyName) {
		t.Errorf("ingress allowance should be generated: %v", names)
	}
}

func TestTheFullyFencedSetIsDeterministic(t *testing.T) {
	profile := profileWith(func(s *platformv1alpha1.NamespaceProfileSpec) {
		s.Network = &platformv1alpha1.NetworkSpec{
			DefaultDenyIngress: ptr.To(true),
			DefaultDenyEgress:  ptr.To(true),
			AllowDNS:           ptr.To(true),
			AllowSameNamespace: ptr.To(true),
			AllowKubeAPI: &platformv1alpha1.KubeAPIEgressSpec{
				Endpoints: []string{"10.0.0.3/32", "10.0.0.1/32", "10.0.0.2/32"},
			},
			AllowEgressToNamespaces:    []string{"z-ns", "a-ns", "m-ns"},
			AllowIngressFromNamespaces: []string{"z-ns", "a-ns"},
		}
	})

	want := []string{
		platformv1alpha1.DenyIngressPolicyName,
		platformv1alpha1.DenyEgressPolicyName,
		platformv1alpha1.AllowDNSPolicyName,
		platformv1alpha1.AllowKubeAPIPolicyName,
		platformv1alpha1.AllowEgressToNamespacesPolicyName,
		platformv1alpha1.AllowIngressFromNamespacesPolicyName,
		platformv1alpha1.AllowSameNamespacePolicyName,
	}

	first := NetworkPolicies(profile)
	if !slices.Equal(want, policyNames(first)) {
		t.Errorf("generated set mismatch\n  want: %v\n  got:  %v", want, policyNames(first))
	}

	// Unsorted input must still produce identical output every time, or the
	// operator would rewrite these policies forever.
	for range 50 {
		if !apiequality.Semantic.DeepEqual(first, NetworkPolicies(profile)) {
			t.Fatal("NetworkPolicies() is not deterministic with unsorted input")
		}
	}
}
