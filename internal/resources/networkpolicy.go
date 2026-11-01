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

	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

// namespaceNameLabel is the label Kubernetes sets on every namespace,
// containing the namespace's own name.
//
// It is how a NetworkPolicy peer names a specific namespace, since a peer can
// only select namespaces by label. The API server has maintained it
// automatically since Kubernetes 1.22, so it needs no cooperation from whoever
// created the namespace — which matters here, because the DNS namespace
// belongs to the cluster, not to this operator.
const namespaceNameLabel = "kubernetes.io/metadata.name"

// NetworkPolicy is one generated policy: a deterministic name and the spec the
// operator intends the cluster to hold.
type NetworkPolicy struct {
	Name string
	Spec networkingv1.NetworkPolicySpec
}

// NetworkPolicies returns the complete set of policies a profile declares, in
// a stable order.
//
// One policy per concern, rather than a single combined object, so that
// `kubectl get netpol` in the namespace reads as a list of the decisions the
// profile made. The order is fixed so logs and tests do not depend on map
// iteration.
//
// The subtlety that governs this whole file is how NetworkPolicy decides what
// is restricted. A pod is restricted for a direction only if some policy
// selects it *and* names that direction in policyTypes. Everything not then
// permitted by a rule is denied. So listing a direction in policyTypes is
// itself the act of denying it — which means an "allow" policy that carelessly
// names a direction the profile never asked to restrict would silently cut off
// all other traffic in that direction.
//
// Every policy below therefore names a direction only when the profile has
// asked for that direction to be denied by default.
func NetworkPolicies(profile *platformv1alpha1.NamespaceProfile) []NetworkPolicy {
	network := profile.Spec.Network
	if network == nil {
		return nil
	}

	denyIngress := network.DenyIngressEnabled()
	denyEgress := network.DenyEgressEnabled()

	// An empty selector selects every pod in the policy's own namespace.
	allPods := metav1.LabelSelector{}

	var policies []NetworkPolicy

	if denyIngress {
		// No ingress rules at all: the policy restricts ingress and permits
		// nothing, which is what makes it a default deny.
		policies = append(policies, NetworkPolicy{
			Name: platformv1alpha1.DenyIngressPolicyName,
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: allPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			},
		})
	}

	if denyEgress {
		policies = append(policies, NetworkPolicy{
			Name: platformv1alpha1.DenyEgressPolicyName,
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: allPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			},
		})
	}

	// DNS is only granted alongside a default deny on egress. Without one,
	// egress is already unrestricted and this policy would grant nothing while
	// implying the namespace is fenced when it is not.
	if denyEgress && network.DNSAllowed() {
		policies = append(policies, NetworkPolicy{
			Name: platformv1alpha1.AllowDNSPolicyName,
			Spec: dnsEgressSpec(allPods, network.DNSConfig()),
		})
	}

	// The API server is almost always needed once egress is denied, and is the
	// allowance most likely to be forgotten: nothing in the namespace fails to
	// start, it just cannot talk to the control plane.
	if denyEgress && network.AllowKubeAPI != nil {
		policies = append(policies, NetworkPolicy{
			Name: platformv1alpha1.AllowKubeAPIPolicyName,
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: allPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To:    kubeAPIPeers(network.AllowKubeAPI.Endpoints),
					Ports: networkPorts(network.AllowKubeAPI.KubeAPIPorts()),
				}},
			},
		})
	}

	if denyEgress && len(network.AllowEgressToNamespaces) > 0 {
		policies = append(policies, NetworkPolicy{
			Name: platformv1alpha1.AllowEgressToNamespacesPolicyName,
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: allPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{
						namespacesPeer(network.AllowEgressToNamespaces),
					},
				}},
			},
		})
	}

	if denyIngress && len(network.AllowIngressFromNamespaces) > 0 {
		policies = append(policies, NetworkPolicy{
			Name: platformv1alpha1.AllowIngressFromNamespacesPolicyName,
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: allPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{
						namespacesPeer(network.AllowIngressFromNamespaces),
					},
				}},
			},
		})
	}

	// Same-namespace traffic is only granted in the directions that are denied.
	// Naming a direction here that the profile does not deny would restrict it,
	// turning an allowance into a near-total block.
	if network.SameNamespaceAllowed() && (denyIngress || denyEgress) {
		spec := networkingv1.NetworkPolicySpec{PodSelector: allPods}
		if denyIngress {
			spec.PolicyTypes = append(spec.PolicyTypes, networkingv1.PolicyTypeIngress)
			spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{PodSelector: &allPods}},
			}}
		}
		if denyEgress {
			spec.PolicyTypes = append(spec.PolicyTypes, networkingv1.PolicyTypeEgress)
			spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{PodSelector: &allPods}},
			}}
		}
		policies = append(policies, NetworkPolicy{
			Name: platformv1alpha1.AllowSameNamespacePolicyName,
			Spec: spec,
		})
	}

	return policies
}

// dnsEgressSpec builds the egress allowance for the cluster DNS service.
//
// The peer names both a namespace and a pod selector, which NetworkPolicy
// reads as a conjunction: pods matching the selector *within* those namespaces.
// Omitting the namespace selector would instead mean DNS pods in this
// namespace, which do not exist.
func dnsEgressSpec(allPods metav1.LabelSelector, dns platformv1alpha1.DNSEgressSpec) networkingv1.NetworkPolicySpec {

	return networkingv1.NetworkPolicySpec{
		PodSelector: allPods,
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		Egress: []networkingv1.NetworkPolicyEgressRule{{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{namespaceNameLabel: dns.Namespace},
				},
				PodSelector: dns.PodSelector,
			}},
			Ports: networkPorts(dns.Ports),
		}},
	}
}

// networkPorts converts the API's port list into NetworkPolicy ports.
func networkPorts(declared []platformv1alpha1.NetworkPort) []networkingv1.NetworkPolicyPort {
	ports := make([]networkingv1.NetworkPolicyPort, 0, len(declared))
	for _, port := range declared {
		protocol := port.Protocol
		value := intstr.FromInt32(port.Port)
		ports = append(ports, networkingv1.NetworkPolicyPort{
			Protocol: &protocol,
			Port:     &value,
		})
	}
	return ports
}

// namespacesPeer builds a single peer selecting every pod in the named
// namespaces.
//
// One peer with a set-based requirement rather than one peer per namespace:
// the generated object stays compact however long the list, and sorting the
// values makes it byte-identical on every reconciliation. An unsorted list
// would differ each pass and the operator would rewrite the policy forever.
func namespacesPeer(namespaces []string) networkingv1.NetworkPolicyPeer {
	values := slices.Clone(namespaces)
	slices.Sort(values)

	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      namespaceNameLabel,
				Operator: metav1.LabelSelectorOpIn,
				Values:   values,
			}},
		},
	}
}

// kubeAPIPeers builds the ipBlock peers for the API server.
//
// An ipBlock rather than a selector, because the API server is generally not a
// pod: on managed clusters it sits outside the cluster, and with kubeadm it is
// a host process. A NetworkPolicy peer can only name pods, namespaces or CIDRs,
// so a CIDR is the only form that can reach it.
func kubeAPIPeers(endpoints []string) []networkingv1.NetworkPolicyPeer {
	values := slices.Clone(endpoints)
	slices.Sort(values)

	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(values))
	for _, cidr := range values {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: cidr},
		})
	}
	return peers
}

// NetworkPolicyObject renders one generated policy as a complete typed object.
//
// Unlike the quota, limit range and namespace, NetworkPolicies are written with
// create and update rather than server-side apply. That is a deliberate
// exception, forced by what a default-deny policy actually is.
//
// A default deny is a policy that names a direction in policyTypes and lists no
// rules for it — its desired state is literally "spec.ingress is empty". An
// apply configuration cannot express that: the field is tagged omitempty, so an
// empty list is dropped from the request and the operator asserts nothing about
// it. And server-side apply can only prune fields a manager previously owned —
// so once somebody adds an ingress rule by hand, taking ownership of
// spec.ingress away from nobody, an apply that never mentions the field leaves
// the rule in place forever. A namespace believed to be fenced would quietly
// stay open.
//
// Replacing the whole spec is safe here in a way it would not be for a quota,
// because a NetworkPolicySpec has exactly four fields and the operator declares
// all of them. There is no foreign field to clobber: whoever writes one of these
// objects owns all of it, which is also why every list in the spec is declared
// atomic in the Kubernetes schema.
func NetworkPolicyObject(
	profile *platformv1alpha1.NamespaceProfile, policy NetworkPolicy,
) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:        policy.Name,
			Namespace:   profile.Spec.Namespace,
			Labels:      OwnershipLabels(profile),
			Annotations: appliedGenerationAnnotation(profile),
		},
		Spec: *policy.Spec.DeepCopy(),
	}
}

// NetworkPolicyNames returns the names a profile declares, for pruning
// policies it previously created and no longer wants.
func NetworkPolicyNames(profile *platformv1alpha1.NamespaceProfile) map[string]bool {
	policies := NetworkPolicies(profile)
	names := make(map[string]bool, len(policies))
	for _, policy := range policies {
		names[policy.Name] = true
	}
	return names
}

// NetworkPolicyInSync reports whether a live policy matches what the profile
// declares.
//
// The whole spec is compared. Every list in a NetworkPolicySpec — ingress,
// egress and policyTypes — is declared +listType=atomic in the Kubernetes
// schema, so server-side apply treats each as an indivisible unit owned by
// whoever applies it. The operator therefore owns the entire spec, and plain
// equality is the correct comparison.
func NetworkPolicyInSync(desired networkingv1.NetworkPolicySpec, actual *networkingv1.NetworkPolicy) bool {
	return apiequality.Semantic.DeepEqual(desired, actual.Spec)
}
