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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NamespaceProfileSpec is the declared configuration of a single Kubernetes
// namespace: which namespace it is, how much of the cluster it may consume,
// what per-container defaults apply inside it, how its network is fenced, and
// which Pod Security Admission level governs it.
//
// Every sub-section is optional and independently managed. Omitting a section
// means "the operator manages nothing of that kind for this namespace" rather
// than "apply an empty configuration" — so adding a section provisions its
// resources and removing it again withdraws them.
type NamespaceProfileSpec struct {
	// namespace is the name of the namespace this profile governs.
	//
	// It is immutable: re-pointing a live profile at a different namespace
	// would silently abandon the first namespace's quota, limits and network
	// policies with no record that they were ever managed. Delete and recreate
	// the profile instead.
	//
	// Namespaces beginning with "kube-" and the "default" namespace are
	// rejected. Applying a default-deny network policy or a restricted Pod
	// Security level to kube-system can disable cluster-critical components,
	// and this operator offers no way to make that reversible.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.namespace is immutable; delete and recreate the NamespaceProfile to target a different namespace"
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('kube-') && self != 'default'",message="spec.namespace must not target a reserved namespace (kube-* or default)"
	// +required
	Namespace string `json:"namespace"`

	// adoptExisting controls what happens when the target namespace, or a
	// managed resource at one of this operator's deterministic names, already
	// exists without this operator's ownership labels.
	//
	// When false (the default) the operator refuses to touch pre-existing
	// objects and reports the situation through status conditions, so it can
	// never silently seize a namespace that another system created.
	//
	// When true the operator stamps its ownership labels onto those objects
	// and begins managing them. An adopted namespace is additionally marked
	// with the "platform.nabeem.com/adopted" annotation, which permanently
	// disqualifies it from operator-driven deletion: the operator did not
	// create it, so it will not destroy it.
	// +kubebuilder:default=false
	// +optional
	AdoptExisting *bool `json:"adoptExisting,omitempty"`

	// quota declares the namespace's aggregate share of the cluster and is
	// rendered into a single ResourceQuota.
	//
	// Omit to manage no ResourceQuota. Note that a ResourceQuota constraining
	// requests.cpu or requests.memory makes those requests *mandatory* for
	// every pod in the namespace; pair it with limitRange.defaultRequest so
	// that pods which do not set requests explicitly remain admissible.
	// +optional
	Quota *QuotaSpec `json:"quota,omitempty"`

	// limitRange declares per-container resource defaults and bounds, rendered
	// into a single LimitRange with one Container-scoped item.
	//
	// Omit to manage no LimitRange.
	// +optional
	LimitRange *LimitRangeSpec `json:"limitRange,omitempty"`

	// network declares how the namespace is fenced, rendered into a set of
	// NetworkPolicies.
	//
	// Omit to manage no NetworkPolicies. Enforcement is the responsibility of
	// the cluster's CNI plugin: creating these objects on a cluster whose CNI
	// does not implement NetworkPolicy has no effect on traffic whatsoever.
	// +optional
	Network *NetworkSpec `json:"network,omitempty"`

	// security declares the Pod Security Admission level for the namespace,
	// applied as labels on the namespace itself.
	//
	// Omit to leave Pod Security Admission labels untouched, including labels
	// a cluster administrator set by hand.
	// +optional
	Security *SecuritySpec `json:"security,omitempty"`

	// lifecycle declares what the operator does when this profile is deleted.
	//
	// Omit for the safe defaults: retain the namespace, remove the generated
	// quota, limit range and network policies.
	// +optional
	Lifecycle *LifecycleSpec `json:"lifecycle,omitempty"`
}

// QuotaSpec maps onto the "hard" limits of a single ResourceQuota. Each field
// documents the ResourceQuota key it produces. Fields left unset produce no
// corresponding hard limit, which means that resource is unconstrained.
// +kubebuilder:validation:MinProperties=1
type QuotaSpec struct {
	// requests caps the sum of container resource *requests* across the
	// namespace. Renders the ResourceQuota hard limits "requests.cpu" and
	// "requests.memory".
	//
	// This is the field that governs how much cluster capacity the namespace
	// can reserve, because the scheduler places pods by their requests.
	// +optional
	Requests *ComputeResources `json:"requests,omitempty"`

	// limits caps the sum of container resource *limits* across the namespace.
	// Renders the ResourceQuota hard limits "limits.cpu" and "limits.memory".
	//
	// This bounds the namespace's burst ceiling rather than its reservation,
	// and is normally set higher than requests to permit overcommit.
	// +optional
	Limits *ComputeResources `json:"limits,omitempty"`

	// pods caps the number of non-terminal pods in the namespace. Renders the
	// ResourceQuota hard limit "pods".
	//
	// Modelled as an integer rather than a resource.Quantity because a pod
	// count of "50m" is meaningless; the integer form is validated by the API
	// server where a quantity string would not be.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Pods *int32 `json:"pods,omitempty"`

	// storage caps persistent storage consumption, optionally per StorageClass.
	// +optional
	Storage *StorageQuotaSpec `json:"storage,omitempty"`

	// objectCounts caps how many objects of a given kind may exist in the
	// namespace. Each entry renders one ResourceQuota hard limit under the key
	// it is named for.
	//
	// Two key forms are accepted. Kubernetes defines nine shorthand names —
	// "services", "services.nodeports", "services.loadbalancers", "secrets",
	// "configmaps", "replicationcontrollers" and "resourcequotas" — and the
	// generic form "count/<resource>.<group>" works for anything else,
	// including custom resources:
	//
	//	objectCounts:
	//	  services: 10
	//	  secrets: 50
	//	  count/deployments.apps: 20
	//	  count/cronjobs.batch: 5
	//	  count/serviceaccounts: 10
	//
	// Note that ServiceAccounts have no shorthand, so they are capped as
	// "count/serviceaccounts" — the core API group takes no suffix.
	//
	// This caps object *counts* only. It is not workload management: the
	// operator never reads, creates or deletes any of the objects named here.
	// It writes one number into the ResourceQuota and the API server refuses
	// creations beyond it, which is the same division of labour as every other
	// field in this profile.
	//
	// A key is deliberately a free-form string rather than an enumeration,
	// because every CustomResourceDefinition installed in a cluster adds
	// another possible key. Enumerating them would mean a release of this
	// operator every time a team wanted to cap a new kind.
	//
	// "pods" and "persistentvolumeclaims" are rejected here; use spec.quota.pods
	// and spec.quota.storage.persistentVolumeClaims, so that each limit has
	// exactly one place it can be expressed.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^(pods|services|services[.]nodeports|services[.]loadbalancers|secrets|configmaps|persistentvolumeclaims|replicationcontrollers|resourcequotas|count/[a-z0-9][a-z0-9-]*([.][a-z0-9][a-z0-9-]*)*)$'))",message="objectCounts keys must be a Kubernetes object-count quota name (services, secrets, configmaps, services.nodeports, services.loadbalancers, replicationcontrollers, resourcequotas) or of the form count/<resource>.<group>"
	// +kubebuilder:validation:XValidation:rule="!self.exists(k, k == 'pods' || k == 'persistentvolumeclaims')",message="use spec.quota.pods and spec.quota.storage.persistentVolumeClaims rather than objectCounts, so each limit has one canonical place"
	// +kubebuilder:validation:XValidation:rule="self.all(k, self[k] >= 0)",message="objectCounts values must not be negative"
	// +optional
	ObjectCounts map[string]int32 `json:"objectCounts,omitempty"`

	// extendedResources caps aggregate consumption of resources that are not
	// built into Kubernetes: device-plugin resources such as GPUs, and
	// hugepages. Each entry renders the ResourceQuota hard limit
	// "requests.<key>".
	//
	//	extendedResources:
	//	  nvidia.com/gpu: "4"
	//	  amd.com/gpu: "2"
	//	  hugepages-2Mi: 512Mi
	//
	// Keys must be either a vendor-qualified name ("nvidia.com/gpu") or a
	// hugepages size ("hugepages-2Mi"). Do not include a "requests." prefix;
	// the operator adds it.
	//
	// Only the requests side is expressible, and that is sufficient:
	// Kubernetes requires extended resources to be requested and limited at
	// the same value, so a request ceiling is also a limit ceiling. cpu,
	// memory and ephemeral storage are rejected here — they have typed fields
	// under requests and limits, so each limit has one canonical place.
	//
	// A value of zero is meaningful and allowed: it forbids the resource in
	// this namespace outright, which is how a platform keeps accelerators out
	// of namespaces that have no business requesting them.
	//
	// As with objectCounts, keys are free-form because the set of device
	// plugins a cluster might install is open-ended. The cap of eight entries
	// is partly a cost control: the API server budgets the CEL rules below
	// against the map's declared maximum size, and map keys have no length
	// bound it can assume.
	// +kubebuilder:validation:MaxProperties=8
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)+/[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?|hugepages-[0-9]+(Ki|Mi|Gi|Ti|Pi|Ei))$'))",message="extendedResources keys must be a vendor-qualified resource name such as nvidia.com/gpu, or a hugepages size such as hugepages-2Mi"
	// +kubebuilder:validation:XValidation:rule="!self.exists(k, k.startsWith('requests.') || k.startsWith('limits.'))",message="extendedResources keys must not carry a requests. or limits. prefix; the operator adds it"
	// +kubebuilder:validation:XValidation:rule="!self.exists(k, k.startsWith('kubernetes.io/'))",message="the kubernetes.io/ prefix is reserved and cannot be used for an extended resource"
	// +kubebuilder:validation:XValidation:rule="!self.exists(k, quantity(string(self[k])).isLessThan(quantity('0')))",message="extendedResources values must not be negative"
	// +optional
	ExtendedResources map[string]resource.Quantity `json:"extendedResources,omitempty"`
}

// StorageQuotaSpec bounds persistent storage for the namespace. The
// cluster-wide fields apply across all StorageClasses; entries in classes add
// independent per-StorageClass ceilings on top, which is how platforms ration
// expensive tiers separately from cheap ones.
// +kubebuilder:validation:MinProperties=1
type StorageQuotaSpec struct {
	// requests caps the sum of PersistentVolumeClaim storage requests across
	// all StorageClasses. Renders the ResourceQuota hard limit
	// "requests.storage".
	// +optional
	Requests *resource.Quantity `json:"requests,omitempty"`

	// persistentVolumeClaims caps the number of PersistentVolumeClaims in the
	// namespace across all StorageClasses. Renders the ResourceQuota hard
	// limit "persistentvolumeclaims".
	// +kubebuilder:validation:Minimum=0
	// +optional
	PersistentVolumeClaims *int32 `json:"persistentVolumeClaims,omitempty"`

	// classes adds per-StorageClass ceilings. Each entry renders the
	// ResourceQuota hard limits
	// "<class>.storageclass.storage.k8s.io/requests.storage" and
	// "<class>.storageclass.storage.k8s.io/persistentvolumeclaims".
	//
	// Entries are keyed by StorageClass name, so a name may appear only once.
	// The operator does not verify that the StorageClass exists: a quota
	// naming an absent class is inert rather than an error, which keeps
	// profiles portable across clusters with different storage offerings.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Classes []StorageClassQuota `json:"classes,omitempty"`
}

// StorageClassQuota is a storage ceiling scoped to one StorageClass.
type StorageClassQuota struct {
	// name is the StorageClass this entry constrains.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	// +required
	Name string `json:"name"`

	// requests caps the sum of PersistentVolumeClaim storage requests for this
	// StorageClass.
	// +optional
	Requests *resource.Quantity `json:"requests,omitempty"`

	// persistentVolumeClaims caps the number of PersistentVolumeClaims using
	// this StorageClass.
	// +kubebuilder:validation:Minimum=0
	// +optional
	PersistentVolumeClaims *int32 `json:"persistentVolumeClaims,omitempty"`
}

// ComputeResources is a CPU and memory pair. It is reused wherever the API
// needs one, which keeps the positivity validation below in a single place.
//
// Quantities use the standard Kubernetes format: CPU in cores or millicores
// ("8", "500m"), and memory and ephemeral storage in bytes with an optional
// suffix ("16Gi", "512Mi").
// +kubebuilder:validation:XValidation:rule="!has(self.cpu) || quantity(string(self.cpu)).isGreaterThan(quantity('0'))",message="cpu must be greater than zero"
// +kubebuilder:validation:XValidation:rule="!has(self.memory) || quantity(string(self.memory)).isGreaterThan(quantity('0'))",message="memory must be greater than zero"
// +kubebuilder:validation:XValidation:rule="!has(self.ephemeralStorage) || quantity(string(self.ephemeralStorage)).isGreaterThan(quantity('0'))",message="ephemeralStorage must be greater than zero"
// +kubebuilder:validation:MinProperties=1
type ComputeResources struct {
	// cpu is a CPU quantity, for example "8" or "500m".
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`

	// memory is a memory quantity, for example "16Gi" or "512Mi".
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`

	// ephemeralStorage is a local ephemeral storage quantity, for example
	// "20Gi".
	//
	// This is node disk: container writable layers, emptyDir volumes, and
	// container logs. It is worth constraining even when it feels obscure,
	// because it is the one compute resource whose exhaustion harms other
	// namespaces. A pod writing without limit to an emptyDir can fill its
	// node's disk, at which point the kubelet starts evicting pods on that
	// node — including pods belonging to namespaces that did nothing wrong.
	// +optional
	EphemeralStorage *resource.Quantity `json:"ephemeralStorage,omitempty"`
}

// LimitRangeSpec renders a LimitRange containing a single Container-scoped
// item. It controls what individual containers get and what they are allowed
// to ask for, which is a different concern from QuotaSpec: the quota bounds the
// namespace in aggregate, while this bounds each container.
//
// The two interact. A ResourceQuota on requests.cpu rejects any pod that does
// not declare a CPU request, so defaultRequest is what keeps ordinary
// workloads — which frequently declare nothing — admissible. Conversely, max
// is what stops one container from consuming the namespace's entire quota.
//
// Admission evaluates the LimitRange first, filling in defaults, and then the
// ResourceQuota against the resulting values.
//
// The validation rules below enforce the ordering
// min <= defaultRequest <= default <= max, independently for cpu, memory and
// ephemeral storage, so a LimitRange that could never admit any pod is
// rejected on write rather than silently blocking every deployment later.
// +kubebuilder:validation:MinProperties=1
// +kubebuilder:validation:XValidation:rule="!has(self.min) || !has(self.max) || ((!has(self.min.cpu) || !has(self.max.cpu) || quantity(string(self.min.cpu)).compareTo(quantity(string(self.max.cpu))) <= 0) && (!has(self.min.memory) || !has(self.max.memory) || quantity(string(self.min.memory)).compareTo(quantity(string(self.max.memory))) <= 0) && (!has(self.min.ephemeralStorage) || !has(self.max.ephemeralStorage) || quantity(string(self.min.ephemeralStorage)).compareTo(quantity(string(self.max.ephemeralStorage))) <= 0))",message="limitRange.min must not exceed limitRange.max"
// +kubebuilder:validation:XValidation:rule="!has(self.min) || !has(self.defaultRequest) || ((!has(self.min.cpu) || !has(self.defaultRequest.cpu) || quantity(string(self.min.cpu)).compareTo(quantity(string(self.defaultRequest.cpu))) <= 0) && (!has(self.min.memory) || !has(self.defaultRequest.memory) || quantity(string(self.min.memory)).compareTo(quantity(string(self.defaultRequest.memory))) <= 0) && (!has(self.min.ephemeralStorage) || !has(self.defaultRequest.ephemeralStorage) || quantity(string(self.min.ephemeralStorage)).compareTo(quantity(string(self.defaultRequest.ephemeralStorage))) <= 0))",message="limitRange.min must not exceed limitRange.defaultRequest"
// +kubebuilder:validation:XValidation:rule="!has(self.min) || !has(self.default) || ((!has(self.min.cpu) || !has(self.default.cpu) || quantity(string(self.min.cpu)).compareTo(quantity(string(self.default.cpu))) <= 0) && (!has(self.min.memory) || !has(self.default.memory) || quantity(string(self.min.memory)).compareTo(quantity(string(self.default.memory))) <= 0) && (!has(self.min.ephemeralStorage) || !has(self.default.ephemeralStorage) || quantity(string(self.min.ephemeralStorage)).compareTo(quantity(string(self.default.ephemeralStorage))) <= 0))",message="limitRange.min must not exceed limitRange.default"
// +kubebuilder:validation:XValidation:rule="!has(self.defaultRequest) || !has(self.default) || ((!has(self.defaultRequest.cpu) || !has(self.default.cpu) || quantity(string(self.defaultRequest.cpu)).compareTo(quantity(string(self.default.cpu))) <= 0) && (!has(self.defaultRequest.memory) || !has(self.default.memory) || quantity(string(self.defaultRequest.memory)).compareTo(quantity(string(self.default.memory))) <= 0) && (!has(self.defaultRequest.ephemeralStorage) || !has(self.default.ephemeralStorage) || quantity(string(self.defaultRequest.ephemeralStorage)).compareTo(quantity(string(self.default.ephemeralStorage))) <= 0))",message="limitRange.defaultRequest must not exceed limitRange.default"
// +kubebuilder:validation:XValidation:rule="!has(self.defaultRequest) || !has(self.max) || ((!has(self.defaultRequest.cpu) || !has(self.max.cpu) || quantity(string(self.defaultRequest.cpu)).compareTo(quantity(string(self.max.cpu))) <= 0) && (!has(self.defaultRequest.memory) || !has(self.max.memory) || quantity(string(self.defaultRequest.memory)).compareTo(quantity(string(self.max.memory))) <= 0) && (!has(self.defaultRequest.ephemeralStorage) || !has(self.max.ephemeralStorage) || quantity(string(self.defaultRequest.ephemeralStorage)).compareTo(quantity(string(self.max.ephemeralStorage))) <= 0))",message="limitRange.defaultRequest must not exceed limitRange.max"
// +kubebuilder:validation:XValidation:rule="!has(self.default) || !has(self.max) || ((!has(self.default.cpu) || !has(self.max.cpu) || quantity(string(self.default.cpu)).compareTo(quantity(string(self.max.cpu))) <= 0) && (!has(self.default.memory) || !has(self.max.memory) || quantity(string(self.default.memory)).compareTo(quantity(string(self.max.memory))) <= 0) && (!has(self.default.ephemeralStorage) || !has(self.max.ephemeralStorage) || quantity(string(self.default.ephemeralStorage)).compareTo(quantity(string(self.max.ephemeralStorage))) <= 0))",message="limitRange.default must not exceed limitRange.max"
type LimitRangeSpec struct {
	// defaultRequest is the resource request applied to a container that
	// declares none. Renders the LimitRange "defaultRequest" for type
	// Container.
	//
	// Set this whenever quota.requests is set, otherwise pods without explicit
	// requests are rejected by the ResourceQuota.
	// +optional
	DefaultRequest *ComputeResources `json:"defaultRequest,omitempty"`

	// default is the resource limit applied to a container that declares none.
	// Renders the LimitRange "default" for type Container.
	// +optional
	Default *ComputeResources `json:"default,omitempty"`

	// min is the smallest request a container may declare. Renders the
	// LimitRange "min" for type Container. Containers below it are rejected.
	// +optional
	Min *ComputeResources `json:"min,omitempty"`

	// max is the largest limit a container may declare. Renders the LimitRange
	// "max" for type Container. Containers above it are rejected.
	// +optional
	Max *ComputeResources `json:"max,omitempty"`
}

// NetworkSpec declares the namespace's network fence. It renders a
// deterministic set of NetworkPolicies, one per enabled concern, so that
// reading `kubectl get netpol` in the namespace shows directly which parts of
// the profile are in force.
//
// Enforcement depends entirely on the CNI plugin. Clusters whose CNI does not
// implement NetworkPolicy will accept these objects and ignore them.
// +kubebuilder:validation:XValidation:rule="!(self.defaultDenyEgress && !self.allowDNS)",message="network.defaultDenyEgress with allowDNS=false blocks all DNS resolution in the namespace; set allowDNS=true"
type NetworkSpec struct {
	// defaultDenyIngress denies all inbound pod traffic by default, so only
	// traffic explicitly permitted by another policy is allowed.
	//
	// This is safe to enable on an existing namespace only if you also permit
	// the traffic its workloads actually receive — most obviously via
	// allowSameNamespace, and via ingress-controller policies that live
	// outside this profile.
	// +kubebuilder:default=true
	// +optional
	DefaultDenyIngress *bool `json:"defaultDenyIngress,omitempty"`

	// defaultDenyEgress denies all outbound pod traffic by default.
	//
	// This is the most disruptive field in the API and defaults to false. With
	// it enabled, pods cannot reach DNS, the Kubernetes API server, cluster
	// services in other namespaces, or anything on the internet unless a
	// policy permits it. allowDNS covers DNS; everything else is out of this
	// profile's scope and must be granted by policies you manage yourself.
	//
	// Image pulls are unaffected, because the kubelet — not the pod — fetches
	// images.
	// +kubebuilder:default=false
	// +optional
	DefaultDenyEgress *bool `json:"defaultDenyEgress,omitempty"`

	// allowDNS permits egress to the cluster DNS service. Only meaningful when
	// defaultDenyEgress is true, since without a default deny egress is
	// already unrestricted.
	//
	// The DNS service's location and labels are cluster-specific; see the dns
	// field.
	// +kubebuilder:default=true
	// +optional
	AllowDNS *bool `json:"allowDNS,omitempty"`

	// allowSameNamespace permits pods in this namespace to talk to each other.
	//
	// It adds an ingress allowance from pods in the same namespace, and when
	// defaultDenyEgress is true a matching egress allowance, so intra-namespace
	// traffic works in both directions.
	// +kubebuilder:default=true
	// +optional
	AllowSameNamespace *bool `json:"allowSameNamespace,omitempty"`

	// dns overrides how the cluster DNS service is identified when allowDNS is
	// in effect.
	//
	// Defaults suit kube-dns and CoreDNS as deployed by kubeadm, kind and most
	// managed providers: the "kube-system" namespace, pods labelled
	// "k8s-app: kube-dns", UDP and TCP port 53. Clusters that deploy DNS
	// elsewhere, label it differently, or run it on a non-standard port must
	// set this, or the generated allowance will not match the real DNS pods
	// and resolution will fail.
	//
	// Defaulted to an empty object so the API server fills in the fields below
	// and stores them explicitly. The DNS assumptions are therefore visible in
	// the stored resource and defined in exactly one place — the schema —
	// rather than being duplicated as fallbacks in controller code.
	// +kubebuilder:default={}
	// +optional
	DNS *DNSEgressSpec `json:"dns,omitempty"`

	// allowKubeAPI permits egress to the Kubernetes API server. Only
	// meaningful when defaultDenyEgress is true.
	//
	// Almost every namespace needs this: controllers, operators and
	// service-mesh sidecars all talk to the API server, and a default-deny
	// egress silently breaks them.
	//
	// The endpoints must be given explicitly rather than inferred, because the
	// API server is usually not a pod. On managed clusters it is outside the
	// cluster entirely; with kubeadm it is a host process. A NetworkPolicy peer
	// can only select pods, namespaces or CIDRs, so reaching it requires an
	// ipBlock — and the correct CIDR is specific to the cluster. Guessing would
	// produce a policy that permits nothing while appearing to permit
	// something, which is the worst available outcome.
	//
	// Find the value with: kubectl get endpoints kubernetes -n default
	// +optional
	AllowKubeAPI *KubeAPIEgressSpec `json:"allowKubeAPI,omitempty"`

	// allowEgressToNamespaces permits egress to every pod in the named
	// namespaces. Only meaningful when defaultDenyEgress is true.
	//
	// This is how a namespace reaches shared platform services — a database,
	// an internal registry, an observability stack — without opening egress
	// generally.
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	AllowEgressToNamespaces []string `json:"allowEgressToNamespaces,omitempty"`

	// allowIngressFromNamespaces permits ingress from every pod in the named
	// namespaces. Only meaningful when defaultDenyIngress is true.
	//
	// Typically the ingress controller's namespace, and whichever namespace
	// runs metrics scraping — both of which need to reach pods here and
	// neither of which a default-deny ingress permits.
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	AllowIngressFromNamespaces []string `json:"allowIngressFromNamespaces,omitempty"`
}

// KubeAPIEgressSpec identifies the Kubernetes API server for the generated
// egress allowance.
type KubeAPIEgressSpec struct {
	// endpoints are the CIDRs the API server is reachable on.
	//
	// A single address is written as a /32 (or /128 for IPv6). More than one
	// entry is normal for a highly available control plane.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=43
	// +kubebuilder:validation:XValidation:rule="self.all(e, isCIDR(e))",message="endpoints must be CIDRs, for example 10.0.0.1/32"
	// +required
	Endpoints []string `json:"endpoints"`

	// ports are the API server ports to permit. Defaults to TCP port 443.
	// +kubebuilder:default={{protocol:TCP,port:443}}
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	// +optional
	Ports []NetworkPort `json:"ports,omitempty"`
}

// DNSEgressSpec identifies the cluster DNS service for the generated DNS
// egress allowance. Every assumption the operator would otherwise hardcode is
// expressed here explicitly.
type DNSEgressSpec struct {
	// namespace is the namespace running the cluster DNS pods.
	// +kubebuilder:default=kube-system
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// podSelector selects the DNS pods within that namespace. CoreDNS and
	// kube-dns both carry "k8s-app: kube-dns" in standard deployments, which
	// is the default.
	// +kubebuilder:default={matchLabels:{"k8s-app":"kube-dns"}}
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// ports are the DNS ports to permit. Both UDP and TCP port 53 are
	// permitted by default: resolvers fall back to TCP for responses too large
	// for UDP, so permitting only UDP breaks large responses intermittently
	// and confusingly.
	// +kubebuilder:default={{protocol:UDP,port:53},{protocol:TCP,port:53}}
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	// +optional
	Ports []NetworkPort `json:"ports,omitempty"`
}

// NetworkPort is a protocol and port pair to permit.
//
// Each field that takes a list of these carries its own default — DNS defaults
// to UDP and TCP port 53, the Kubernetes API to TCP port 443 — so the defaults
// live where they make sense rather than on the shared type.
type NetworkPort struct {
	// protocol is the IP protocol.
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +kubebuilder:default=TCP
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`

	// port is the port number.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +required
	Port int32 `json:"port"`
}

// PodSecurityLevel is a Pod Security Standards level, as enforced by the
// built-in Pod Security Admission controller.
// +kubebuilder:validation:Enum=privileged;baseline;restricted
type PodSecurityLevel string

const (
	// PodSecurityPrivileged applies no restrictions. Equivalent to having no
	// policy at all.
	PodSecurityPrivileged PodSecurityLevel = "privileged"

	// PodSecurityBaseline blocks known privilege escalations while remaining
	// permissive enough for most existing workloads.
	PodSecurityBaseline PodSecurityLevel = "baseline"

	// PodSecurityRestricted enforces current pod hardening best practice. It
	// requires workloads to drop capabilities, run as non-root and set a seccomp
	// profile, so it commonly rejects workloads that were not written for it.
	PodSecurityRestricted PodSecurityLevel = "restricted"
)

// SecuritySpec declares the namespace's Pod Security Admission configuration.
// It renders labels on the namespace; the API server's built-in admission
// plugin does the enforcing, so no controller of ours ever inspects a pod.
//
// Pod Security Admission has three independent modes. Enforce rejects
// violating pods; audit records them in the audit log; warn returns a message
// to the client. A staged rollout typically enforces baseline while auditing
// and warning at restricted, which is why audit and warn are separate fields
// rather than being implied by the enforce level.
type SecuritySpec struct {
	// podSecurityStandard is the level to *enforce*, rendering the namespace
	// label "pod-security.kubernetes.io/enforce".
	//
	// Pods violating this level are rejected at admission. Already-running
	// pods are never evicted by a level change, so raising it affects new
	// pods only.
	// +required
	PodSecurityStandard PodSecurityLevel `json:"podSecurityStandard"`

	// audit is the level to audit, rendering the namespace label
	// "pod-security.kubernetes.io/audit". Violations are recorded in the API
	// server audit log and otherwise permitted.
	//
	// Defaults to podSecurityStandard when unset.
	// +optional
	Audit *PodSecurityLevel `json:"audit,omitempty"`

	// warn is the level to warn on, rendering the namespace label
	// "pod-security.kubernetes.io/warn". Violations produce a client-visible
	// warning and are otherwise permitted.
	//
	// Defaults to podSecurityStandard when unset.
	// +optional
	Warn *PodSecurityLevel `json:"warn,omitempty"`

	// enforceVersion pins which version of the Pod Security Standards is
	// applied, rendering the "-version" variants of the labels above.
	//
	// "latest" tracks the API server's current definition, which means a
	// cluster upgrade can tighten the rules and start rejecting workloads that
	// previously passed. Pinning to a specific minor ("v1.34") freezes the
	// definition so upgrades cannot change admission behaviour, at the cost of
	// having to bump it deliberately.
	// +kubebuilder:default=latest
	// +kubebuilder:validation:Pattern=`^(latest|v1\.[0-9]{1,3})$`
	// +optional
	EnforceVersion string `json:"enforceVersion,omitempty"`
}

// LifecycleSpec declares what happens when the NamespaceProfile is deleted.
//
// Deleting a namespace deletes everything inside it, including workloads this
// operator never managed, so the two fields here are deliberately conservative
// and mutually constrained.
// +kubebuilder:validation:XValidation:rule="!(self.deleteNamespaceOnRemoval && self.retainManagedResources)",message="lifecycle.retainManagedResources is meaningless with deleteNamespaceOnRemoval=true, because deleting the namespace deletes the managed resources with it"
type LifecycleSpec struct {
	// deleteNamespaceOnRemoval allows the operator to delete the target
	// namespace when this profile is deleted.
	//
	// Deleting a namespace cascades to every object in it — Deployments,
	// StatefulSets, PersistentVolumeClaims and their data, Secrets — none of
	// which this operator manages or can restore. It therefore defaults to
	// false, and setting it to true is not by itself sufficient: the operator
	// additionally requires the confirmation annotation
	// "platform.nabeem.com/confirm-namespace-deletion", whose value must equal
	// spec.namespace, and it refuses outright for namespaces it adopted rather
	// than created. When any check fails the operator retains the namespace
	// and explains why in status rather than guessing.
	// +kubebuilder:default=false
	// +optional
	DeleteNamespaceOnRemoval *bool `json:"deleteNamespaceOnRemoval,omitempty"`

	// retainManagedResources leaves the generated ResourceQuota, LimitRange
	// and NetworkPolicies in the namespace after this profile is deleted.
	//
	// The default is false — cleanup removes them. An orphaned default-deny
	// NetworkPolicy that no profile explains is a trap: it keeps breaking
	// traffic with nothing left in the cluster to say why. Removing the
	// generated objects is reversible, never touches workloads, and leaves the
	// namespace in a plainly unmanaged state.
	//
	// Set this to true when deliberately handing a namespace over to another
	// system — GitOps, say — that should keep the current policy objects in
	// place. The trade-off is that the namespace is then left with policy
	// nobody owns, including the loss of its quota ceiling being invisible.
	// +kubebuilder:default=false
	// +optional
	RetainManagedResources *bool `json:"retainManagedResources,omitempty"`
}

// ManagedResourceState is the operator's assessment of one managed resource.
// +kubebuilder:validation:Enum=InSync;Drifted;Missing;Conflict;NotManaged
type ManagedResourceState string

const (
	// ManagedResourceInSync means the live resource matches the fields the
	// operator owns.
	ManagedResourceInSync ManagedResourceState = "InSync"

	// ManagedResourceDrifted means the live resource diverged from the profile
	// and the operator is correcting it.
	ManagedResourceDrifted ManagedResourceState = "Drifted"

	// ManagedResourceMissing means the resource should exist but does not, and
	// the operator has not yet succeeded in creating it.
	ManagedResourceMissing ManagedResourceState = "Missing"

	// ManagedResourceConflict means an object already occupies the name without
	// this operator's ownership labels. The operator will not write to it; set
	// spec.adoptExisting to take it over deliberately.
	ManagedResourceConflict ManagedResourceState = "Conflict"

	// ManagedResourceNotManaged means the profile does not ask for this
	// resource, so the operator manages nothing of that kind.
	ManagedResourceNotManaged ManagedResourceState = "NotManaged"
)

// ManagedResourceStatus reports one resource the operator is responsible for.
// Namespace is omitted because every namespaced managed resource lives in
// spec.namespace by construction.
type ManagedResourceStatus struct {
	// kind is the resource kind, for example "ResourceQuota".
	// +kubebuilder:validation:MaxLength=63
	// +required
	Kind string `json:"kind"`

	// name is the resource name. The operator uses deterministic names, so
	// this is predictable from the kind.
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`

	// state is the operator's assessment of this resource.
	// +required
	State ManagedResourceState `json:"state"`

	// message explains a non-InSync state in one short sentence.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Message string `json:"message,omitempty"`
}

// ProfilePhase is a single-word summary of the profile, for display in
// `kubectl get`. Conditions carry the authoritative detail; this exists only
// so the common case is legible at a glance.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Degraded;Conflict;Terminating
type ProfilePhase string

const (
	// ProfilePhasePending means the operator has not yet acted on the profile.
	ProfilePhasePending ProfilePhase = "Pending"

	// ProfilePhaseProvisioning means the operator is creating or updating
	// managed resources.
	ProfilePhaseProvisioning ProfilePhase = "Provisioning"

	// ProfilePhaseReady means every requested resource exists and matches the
	// profile.
	ProfilePhaseReady ProfilePhase = "Ready"

	// ProfilePhaseDegraded means the operator could not reach the desired
	// state.
	ProfilePhaseDegraded ProfilePhase = "Degraded"

	// ProfilePhaseConflict means another NamespaceProfile owns the target
	// namespace, or a pre-existing resource blocks provisioning. The operator
	// is deliberately idle.
	ProfilePhaseConflict ProfilePhase = "Conflict"

	// ProfilePhaseTerminating means the profile is being deleted and cleanup is
	// in progress.
	ProfilePhaseTerminating ProfilePhase = "Terminating"
)

// NamespaceProfileStatus is the operator's observed state of the profile.
//
// It deliberately holds no copy of the namespace's contents and no workload
// detail: it records what the operator did, not what lives in the namespace.
type NamespaceProfileStatus struct {
	// observedGeneration is the metadata.generation this status was computed
	// from. When it trails metadata.generation, the reported state predates the
	// most recent spec change.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// targetNamespace echoes spec.namespace, so the namespace a profile governs
	// is visible in status and in `kubectl get` output without resolving the
	// spec.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	TargetNamespace string `json:"targetNamespace,omitempty"`

	// phase is a one-word summary for display. See conditions for detail.
	// +optional
	Phase ProfilePhase `json:"phase,omitempty"`

	// conditions is the authoritative state of the profile.
	//
	// Condition types:
	//   - "Ready":           every requested resource exists and matches.
	//   - "Progressing":     the operator is actively converging.
	//   - "Degraded":        the operator could not reach the desired state.
	//   - "PolicyCompliant": the live quota, limits, network policies and
	//                        namespace labels match the profile — that is,
	//                        no uncorrected drift.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// managedResources reports each resource the operator is responsible for
	// and whether it currently matches the profile.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	// +optional
	ManagedResources []ManagedResourceStatus `json:"managedResources,omitempty"`

	// conflictingProfiles names the other NamespaceProfiles that target the same
	// namespace, including whichever one owns it.
	//
	// Populated only on a profile that does not own the namespace — that profile
	// is the one blocked, and this is where to look for who to talk to. A profile
	// that owns its namespace is working correctly and leaves this empty.
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	// +optional
	ConflictingProfiles []string `json:"conflictingProfiles,omitempty"`

	// lastSuccessfulReconcileTime is when the operator last completed a
	// reconciliation with no errors.
	//
	// It is not refreshed on every successful pass. Doing so would write status
	// on every reconciliation, including the overwhelming majority that find
	// nothing to do, turning a quiet cluster into a steady stream of writes and
	// watch events.
	// +optional
	LastSuccessfulReconcileTime *metav1.Time `json:"lastSuccessfulReconcileTime,omitempty"`

	// lastDriftCorrectionTime is when the operator last had to rewrite a
	// managed resource that had been changed outside the profile. A recent
	// value here alongside a healthy Ready condition is the signature of
	// something repeatedly fighting the operator.
	// +optional
	LastDriftCorrectionTime *metav1.Time `json:"lastDriftCorrectionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nsprofile;nsp
// +kubebuilder:printcolumn:name="Namespace",type="string",JSONPath=".spec.namespace"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Compliant",type="string",JSONPath=".status.conditions[?(@.type==\"PolicyCompliant\")].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NamespaceProfile is the declared configuration of one Kubernetes namespace:
// its resource quota, container limits, network fence and Pod Security
// Admission level.
//
// It is cluster-scoped. Creating a namespace and choosing its Pod Security
// level are cluster-level powers, so the resource that confers them should not
// be grantable inside a single namespace. Cluster scope also makes
// metadata.name unique across the cluster, which is what lets the operator
// record namespace ownership in a single label.
//
// The operator manages only namespace-level policy. It never creates, inspects
// or modifies workloads; enforcement is left to the Kubernetes mechanisms the
// profile configures.
type NamespaceProfile struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of NamespaceProfile
	// +required
	Spec NamespaceProfileSpec `json:"spec"`

	// status defines the observed state of NamespaceProfile
	// +optional
	Status NamespaceProfileStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NamespaceProfileList contains a list of NamespaceProfile
type NamespaceProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NamespaceProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &NamespaceProfile{}, &NamespaceProfileList{})
		return nil
	})
}
