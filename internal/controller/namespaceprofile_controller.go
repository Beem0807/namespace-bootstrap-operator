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

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
	"github.com/Beem0807/namespace-bootstrap-operator/internal/metrics"
	"github.com/Beem0807/namespace-bootstrap-operator/internal/resources"
)

const (
	// namespaceTerminatingRecheck is how long to wait before looking again at
	// a namespace that is still terminating.
	//
	// A terminating namespace rejects writes, so there is nothing useful to do
	// until it is gone, and how long that takes depends on the finalizers of
	// whatever is inside it. This is the only place the controller polls, and
	// it is a safety net rather than the mechanism: the namespace watch below
	// delivers the deletion event, and this covers the case where that event
	// is missed.
	namespaceTerminatingRecheck = 15 * time.Second

	// specNamespaceIndex indexes NamespaceProfiles by the namespace they
	// target.
	//
	// This is what makes the watches below possible. The operator sets no
	// owner references, so an event on a generated resource carries no pointer
	// back to its profile — all it has is the namespace it lives in. The index
	// turns that namespace into the profiles that claim it, without listing
	// and filtering every profile in the cluster on every event.
	specNamespaceIndex = "spec.namespace"
)

// NamespaceProfileReconciler reconciles a NamespaceProfile object.
type NamespaceProfileReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder publishes Kubernetes Events against the profile.
	//
	// Events carry history, which conditions cannot: a condition says the
	// quota is wrong now, while events say it has been corrected eleven times
	// in the last hour. Those call for completely different responses.
	//
	// This is the events.k8s.io/v1 recorder rather than the legacy core/v1
	// one, which is deprecated in controller-runtime. The newer API separates
	// the reason an event was emitted from the action taken, and is what the
	// cluster aggregates on.
	Recorder recorder.EventRecorder
}

// The operator's own permissions, from which config/rbac/role.yaml is
// generated. They are deliberately narrow:
//
//   - list and watch appear even for resources the controller only ever reads
//     by name, because the manager's client is cache-backed: a single Get
//     causes an informer to LIST and WATCH that type cluster-wide.
//   - patch is what server-side apply uses, and create covers applying an
//     object that does not exist yet. update is granted only for
//     networkpolicies, which are the one kind written with create and update
//     instead of apply; see resources.NetworkPolicyObject for why.
//   - namespaces do carry delete, which is the single most dangerous
//     permission here. It is reachable only through the gated path in
//     maybeDeleteNamespace, which requires an explicit lifecycle setting, a
//     confirmation annotation naming the namespace, this profile's ownership
//     labels, and the namespace not having been adopted.
//   - the profile itself is never created, updated or deleted by the operator.
//     It is read, its finalizer is patched, and its status subresource is
//     patched - so those are the only verbs granted on it. A controller that
//     could delete the very objects describing its intent would be able to
//     erase its own instructions.
//   - nothing here grants access to pods, workloads, secrets, configmaps, or
//     any RBAC object. Enforcement is left to the Kubernetes mechanisms the
//     profile configures.
//
// +kubebuilder:rbac:groups=platform.nabeem.com,resources=namespaceprofiles,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=platform.nabeem.com,resources=namespaceprofiles/status,verbs=get;patch
// +kubebuilder:rbac:groups=platform.nabeem.com,resources=namespaceprofiles/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups="",resources=limitranges,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives the target namespace toward the profile's declared
// configuration and records the outcome in status.
//
// The body is a flat sequence on purpose. Each step is independently
// idempotent and makes no assumption that the previous reconciliation ran to
// completion: the request carries only a name, so the controller re-reads the
// world every time rather than reacting to a particular change. A fresh
// profile, a profile whose spec just changed, a profile whose quota somebody
// edited by hand, and a profile the operator is seeing for the first time
// after a restart all take the same path.
//
// Returning an error is how the controller retries. controller-runtime applies
// per-key exponential backoff, so a transient API failure needs no retry loop
// here, and an optimistic-concurrency conflict is handled the same way: the
// local copy is stale, so the correct response is to give up and be called
// again with a fresh one.
func (r *NamespaceProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	profile := &platformv1alpha1.NamespaceProfile{}
	if err := r.Get(ctx, req.NamespacedName, profile); err != nil {
		if apierrors.IsNotFound(err) {
			// Stop publishing metrics for a profile that is gone, so an alert
			// on nsprofile_ready == 0 does not fire forever for a namespace
			// nobody manages any more.
			metrics.Forget(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log = log.WithValues("targetNamespace", profile.Spec.Namespace)
	ctx = logf.IntoContext(ctx, log)

	if !profile.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, profile)
	}

	// Registered before anything is created, so there is never a window in
	// which generated resources exist with nothing committed to removing them.
	if err := r.ensureFinalizer(ctx, profile); err != nil {
		return ctrl.Result{}, err
	}

	owners, err := r.resolveOwnership(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}

	obs, result, provisionErr := r.provision(ctx, profile, owners)

	// Status is recorded whether or not provisioning succeeded, so a failure
	// is visible on the object itself rather than only in the operator's logs.
	if err := r.patchStatus(ctx, profile, obs, provisionErr); err != nil {
		if provisionErr == nil {
			return ctrl.Result{}, err
		}
		// The provisioning error is the more useful one to surface and retry
		// on; losing the status write is worth a log line, not a lost cause.
		log.Error(err, "could not record status")
	}

	return result, provisionErr
}

// provision brings every resource the profile declares into line and reports
// what it found.
func (r *NamespaceProfileReconciler) provision(
	ctx context.Context, profile *platformv1alpha1.NamespaceProfile, owners ownership,
) (observation, ctrl.Result, error) {
	obs := observation{}

	// Competitors are recorded only on a profile that lost. A profile that owns
	// the namespace is healthy, and "conflictingProfiles" on a Ready object reads
	// as a problem where there is none — nothing conflicts from the winner's point
	// of view. The losing profile names the owner, which is the half somebody has
	// to act on, and the conflict is independently visible through the Warning
	// event and nsprofile_conflicts_total.
	//
	// This also makes the field deterministic. Recording it on the winner too
	// would mean populating it only when the winner happens to reconcile while a
	// rival exists: nothing wakes an established profile when a newcomer stands
	// down, because standing down touches nothing and so emits no event.
	if owners.owner != profile.Name {
		obs.competitors = owners.competitors
	}

	// Another profile is entitled to this namespace. Stop before touching
	// anything: two profiles writing the same namespace would each undo the
	// other forever, and both would report success while doing it.
	if owners.owner != profile.Name {
		logf.FromContext(ctx).Info("another profile owns this namespace; standing down",
			"owner", owners.owner)
		obs.blocked = true
		obs.blockedReason = platformv1alpha1.ReasonNamespaceConflict
		obs.record(platformv1alpha1.ManagedResourceStatus{
			Kind:  "Namespace",
			Name:  profile.Spec.Namespace,
			State: platformv1alpha1.ManagedResourceConflict,
			Message: fmt.Sprintf(
				"namespace %q is managed by NamespaceProfile %q, so this profile is idle; "+
					"ownership stays with the profile the namespace is already labelled for, "+
					"and otherwise goes to the oldest profile targeting it",
				profile.Spec.Namespace, owners.owner),
		}, false)
		metrics.RecordConflict(profile.Name, platformv1alpha1.ReasonNamespaceConflict)
		return obs, ctrl.Result{}, nil
	}

	namespace, err := r.ensureNamespace(ctx, profile)
	obs.record(namespace.status, namespace.drifted)
	if err != nil {
		return obs, ctrl.Result{}, err
	}

	switch namespace.state {
	case namespaceTerminating:
		obs.progressing = true
		logf.FromContext(ctx).Info("target namespace is terminating; deferring provisioning")
		return obs, ctrl.Result{RequeueAfter: namespaceTerminatingRecheck}, nil

	case namespaceNotManageable:
		// Deliberately idle. Retrying cannot help, because a conflict is
		// resolved by a person — either by removing the competing claim or by
		// opting into adoption — so backing off would only burn API calls.
		// Writing policy into a namespace the operator does not own is the one
		// outcome worth avoiding at any cost.
		obs.blocked = true
		obs.blockedReason = platformv1alpha1.ReasonNamespaceNotAdopted
		metrics.RecordConflict(profile.Name, obs.blockedReason)
		return obs, ctrl.Result{}, nil

	case namespaceUsable:
		// Fall through to the namespaced resources.
	}

	quota, err := r.ensureResourceQuota(ctx, profile)
	obs.record(quota.status, quota.drifted)
	if err != nil {
		return obs, ctrl.Result{}, err
	}

	limits, err := r.ensureLimitRange(ctx, profile)
	obs.record(limits.status, limits.drifted)
	if err != nil {
		return obs, ctrl.Result{}, err
	}

	policies, err := r.ensureNetworkPolicies(ctx, profile)
	for _, policy := range policies {
		obs.record(policy.status, policy.drifted)
	}
	if err != nil {
		return obs, ctrl.Result{}, err
	}

	// A foreign object occupying one of the operator's names leaves the
	// profile only partly in force, which is a conflict a person has to
	// resolve rather than something to retry.
	for _, resource := range obs.resources {
		if resource.State == platformv1alpha1.ManagedResourceConflict {
			obs.blocked = true
			obs.blockedReason = platformv1alpha1.ReasonForeignResource
			metrics.RecordConflict(profile.Name, platformv1alpha1.ReasonForeignResource)
			break
		}
	}

	return obs, ctrl.Result{}, nil
}

// ensureNetworkPolicies brings the namespace's generated NetworkPolicies in
// line with the profile, and removes the ones it no longer declares.
//
// Pruning is necessary here in a way it is not for the quota or limit range,
// because the *set* of policies is derived from the spec: turning off
// defaultDenyEgress means two policies the operator previously created should
// no longer exist. Only policies carrying this profile's ownership labels are
// ever deleted.
func (r *NamespaceProfileReconciler) ensureNetworkPolicies(
	ctx context.Context, profile *platformv1alpha1.NamespaceProfile,
) ([]resourceOutcome, error) {
	desired := resources.NetworkPolicies(profile)

	if err := r.pruneNetworkPolicies(ctx, profile, resources.NetworkPolicyNames(profile)); err != nil {
		return nil, err
	}

	outcomes := make([]resourceOutcome, 0, len(desired))
	for _, policy := range desired {
		outcome, err := r.ensureNetworkPolicy(ctx, profile, policy)
		outcomes = append(outcomes, outcome)
		if err != nil {
			return outcomes, err
		}
	}
	return outcomes, nil
}

// pruneNetworkPolicies deletes policies this profile created that it no longer
// declares.
func (r *NamespaceProfileReconciler) pruneNetworkPolicies(
	ctx context.Context, profile *platformv1alpha1.NamespaceProfile, declared map[string]bool,
) error {
	log := logf.FromContext(ctx)

	var existing networkingv1.NetworkPolicyList
	if err := r.List(ctx, &existing,
		client.InNamespace(profile.Spec.Namespace),
		client.MatchingLabels(resources.OwnershipLabels(profile)),
	); err != nil {
		return fmt.Errorf("listing NetworkPolicies in %q: %w", profile.Spec.Namespace, err)
	}

	for i := range existing.Items {
		policy := &existing.Items[i]
		if declared[policy.Name] {
			continue
		}
		// The label selector above already restricts this to objects carrying
		// both ownership labels, so nothing the operator did not create can
		// reach here. Checking again is cheap and makes the guarantee local.
		if !resources.IsOwnedBy(policy, profile) {
			continue
		}
		if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting NetworkPolicy %s/%s: %w",
				policy.Namespace, policy.Name, err)
		}
		log.Info("removed NetworkPolicy that the profile no longer declares",
			"networkPolicy", policy.Name)
	}
	return nil
}

// ensureNetworkPolicy reconciles a single generated policy.
func (r *NamespaceProfileReconciler) ensureNetworkPolicy(
	ctx context.Context, profile *platformv1alpha1.NamespaceProfile, policy resources.NetworkPolicy,
) (resourceOutcome, error) {
	log := logf.FromContext(ctx).WithValues("networkPolicy", policy.Name)
	report := func(state platformv1alpha1.ManagedResourceState, message string) platformv1alpha1.ManagedResourceStatus {
		return platformv1alpha1.ManagedResourceStatus{
			Kind:    "NetworkPolicy",
			Name:    policy.Name,
			State:   state,
			Message: message,
		}
	}

	key := client.ObjectKey{Namespace: profile.Spec.Namespace, Name: policy.Name}

	var actual networkingv1.NetworkPolicy
	err := r.Get(ctx, key, &actual)
	if err != nil && !apierrors.IsNotFound(err) {
		return resourceOutcome{status: report(platformv1alpha1.ManagedResourceMissing, "could not be read")},
			fmt.Errorf("reading NetworkPolicy %s: %w", key, err)
	}
	exists := err == nil

	desired := resources.NetworkPolicyObject(profile, policy)

	// NetworkPolicies are created and updated rather than server-side applied.
	// A default deny is a policy whose desired state is "this rule list is
	// empty", which an apply configuration cannot express — the field is
	// omitempty, so it is dropped from the request, and apply can only prune
	// fields the manager already owned. See resources.NetworkPolicyObject.
	if !exists {
		if err := r.Create(ctx, desired); err != nil {
			return resourceOutcome{status: report(platformv1alpha1.ManagedResourceMissing, "could not be created")},
				fmt.Errorf("creating NetworkPolicy %s: %w", key, err)
		}
		log.Info("created NetworkPolicy")
		r.event(profile, corev1.EventTypeNormal, platformv1alpha1.ReasonProvisioned,
			platformv1alpha1.ActionProvisioning, "created NetworkPolicy %s/%s",
			profile.Spec.Namespace, policy.Name)
		return resourceOutcome{status: report(platformv1alpha1.ManagedResourceInSync, "")}, nil
	}

	if decision := resources.CanManage(&actual, profile); !mayWrite(decision) {
		log.Info("refusing to modify a NetworkPolicy this operator does not own",
			"managedBy", actual.Labels[platformv1alpha1.ManagedByLabel],
			"existingProfile", resources.ManagingProfile(&actual))
		return resourceOutcome{status: report(platformv1alpha1.ManagedResourceConflict,
			conflictMessage("NetworkPolicy", policy.Name, &actual))}, nil
	}

	if resources.NetworkPolicyInSync(policy.Spec, &actual) {
		return resourceOutcome{status: report(platformv1alpha1.ManagedResourceInSync, "")}, nil
	}

	log.Info("correcting NetworkPolicy drift")

	// Update the object as fetched, replacing only what the operator owns, so
	// labels and annotations written by anything else survive. The fetched
	// resourceVersion comes with it, which is what makes this a compare-and-swap:
	// a concurrent change causes a conflict rather than a silent overwrite, and
	// the conflict is retried with a fresh copy.
	updated := actual.DeepCopy()
	updated.Spec = *policy.Spec.DeepCopy()
	for key, value := range resources.OwnershipLabels(profile) {
		if updated.Labels == nil {
			updated.Labels = map[string]string{}
		}
		updated.Labels[key] = value
	}
	if err := r.Update(ctx, updated); err != nil {
		return resourceOutcome{status: report(platformv1alpha1.ManagedResourceDrifted, "could not be corrected")},
			fmt.Errorf("updating NetworkPolicy %s: %w", key, err)
	}

	r.recordDrift(profile, "NetworkPolicy", policy.Name)
	return resourceOutcome{status: report(platformv1alpha1.ManagedResourceInSync, ""), drifted: true}, nil
}

// namespaceState is whether the target namespace can be provisioned into.
type namespaceState int

const (
	// namespaceUsable means the namespace exists, belongs to this profile, and
	// carries the profile's labels.
	namespaceUsable namespaceState = iota

	// namespaceTerminating means the namespace is being deleted. Writes into it
	// would be rejected, so provisioning waits.
	namespaceTerminating

	// namespaceNotManageable means the namespace exists but is not this
	// profile's to manage. Nothing further is attempted.
	namespaceNotManageable
)

// namespaceOutcome is what ensureNamespace found and did.
type namespaceOutcome struct {
	status  platformv1alpha1.ManagedResourceStatus
	state   namespaceState
	drifted bool
}

// resourceOutcome is what one namespaced ensure call found and did.
type resourceOutcome struct {
	status  platformv1alpha1.ManagedResourceStatus
	drifted bool
}

// ensureNamespace creates the target namespace if absent, and otherwise
// reconciles the labels the operator declares on it.
//
// The ownership check is the safety-critical part. A namespace that already
// exists without this profile's labels was created by somebody else, and
// stamping policy onto it would be indistinguishable from seizing it — so the
// operator refuses rather than guessing.
func (r *NamespaceProfileReconciler) ensureNamespace(
	ctx context.Context, profile *platformv1alpha1.NamespaceProfile,
) (namespaceOutcome, error) {
	log := logf.FromContext(ctx)
	report := func(state platformv1alpha1.ManagedResourceState, message string) platformv1alpha1.ManagedResourceStatus {
		return platformv1alpha1.ManagedResourceStatus{
			Kind:    "Namespace",
			Name:    profile.Spec.Namespace,
			State:   state,
			Message: message,
		}
	}

	var namespace corev1.Namespace
	err := r.Get(ctx, client.ObjectKey{Name: profile.Spec.Namespace}, &namespace)

	switch {
	case apierrors.IsNotFound(err):
		// Nothing exists, so no ownership question arises. Applying creates
		// the namespace already carrying this profile's labels, which means
		// there is no window in which it exists unlabelled and a second
		// profile could mistake it for adoptable.
		if err := r.apply(ctx, resources.Namespace(profile, false)); err != nil {
			return namespaceOutcome{
				status: report(platformv1alpha1.ManagedResourceMissing, "could not be created"),
				state:  namespaceNotManageable,
			}, fmt.Errorf("creating namespace %q: %w", profile.Spec.Namespace, err)
		}
		log.Info("created namespace")
		r.event(profile, corev1.EventTypeNormal, platformv1alpha1.ReasonProvisioned,
			platformv1alpha1.ActionProvisioning, "created namespace %s", profile.Spec.Namespace)
		return namespaceOutcome{
			status: report(platformv1alpha1.ManagedResourceInSync, ""),
			state:  namespaceUsable,
		}, nil

	case err != nil:
		return namespaceOutcome{
			status: report(platformv1alpha1.ManagedResourceMissing, "could not be read"),
			state:  namespaceNotManageable,
		}, fmt.Errorf("reading namespace %q: %w", profile.Spec.Namespace, err)
	}

	if namespace.Status.Phase == corev1.NamespaceTerminating || !namespace.DeletionTimestamp.IsZero() {
		return namespaceOutcome{
			status: report(platformv1alpha1.ManagedResourceMissing,
				"the namespace is terminating; provisioning is deferred until it is gone"),
			state: namespaceTerminating,
		}, nil
	}

	decision := resources.CanManage(&namespace, profile)
	if !mayWrite(decision) {
		log.Info("refusing to manage a namespace this profile does not own",
			"managedBy", namespace.Labels[platformv1alpha1.ManagedByLabel],
			"existingProfile", resources.ManagingProfile(&namespace))
		return namespaceOutcome{
			status: report(platformv1alpha1.ManagedResourceConflict,
				conflictMessage("namespace", profile.Spec.Namespace, &namespace)),
			state: namespaceNotManageable,
		}, nil
	}

	// The adoption marker is sticky: once a namespace is recorded as adopted it
	// stays that way, so a later reconciliation cannot quietly re-arm namespace
	// deletion for a namespace the operator never created.
	adopting := decision == resources.ManageAdopt
	reclaiming := decision == resources.ManageRefuseOtherProfile
	adopted := adopting || resources.WasAdopted(&namespace)

	// Skipping the apply when nothing differs is what keeps a settled cluster
	// quiet: no write, no resourceVersion bump, and no watch event to wake
	// this controller again.
	if !adopting && !reclaiming && resources.NamespaceLabelsInSync(profile, &namespace) {
		return namespaceOutcome{
			status: report(platformv1alpha1.ManagedResourceInSync, ""),
			state:  namespaceUsable,
		}, nil
	}

	if err := r.apply(ctx, resources.Namespace(profile, adopted)); err != nil {
		return namespaceOutcome{
			status: report(platformv1alpha1.ManagedResourceDrifted, "could not be corrected"),
			state:  namespaceNotManageable,
		}, fmt.Errorf("applying namespace %q: %w", profile.Spec.Namespace, err)
	}

	if reclaiming {
		// namespace is the copy read before the apply, so this still names the
		// profile that held it rather than the one taking over.
		previous := resources.ManagingProfile(&namespace)
		log.Info("reclaimed a namespace whose previous profile no longer competes for it",
			"previousProfile", previous)
		r.event(profile, corev1.EventTypeWarning, platformv1alpha1.ReasonNamespaceReclaimed,
			platformv1alpha1.ActionProvisioning,
			"took over namespace %s from profile %s, which no longer exists",
			profile.Spec.Namespace, previous)
		return namespaceOutcome{
			status: report(platformv1alpha1.ManagedResourceInSync, ""),
			state:  namespaceUsable,
		}, nil
	}

	if adopting {
		log.Info("adopted a pre-existing namespace; it is now marked as never deletable by the operator")
		r.event(profile, corev1.EventTypeWarning, platformv1alpha1.ReasonProvisioned,
			platformv1alpha1.ActionProvisioning,
			"adopted pre-existing namespace %s; the operator will never delete it",
			profile.Spec.Namespace)
		return namespaceOutcome{
			status: report(platformv1alpha1.ManagedResourceInSync, ""),
			state:  namespaceUsable,
		}, nil
	}

	log.Info("corrected namespace labels")
	r.recordDrift(profile, "Namespace", profile.Spec.Namespace)
	return namespaceOutcome{
		status:  report(platformv1alpha1.ManagedResourceInSync, ""),
		state:   namespaceUsable,
		drifted: true,
	}, nil
}

// mayWrite decides whether the operator may write to an existing object.
//
// By the time any ensure function runs, provision has already established that
// this profile is the namespace's entitled owner — a profile that is not stands
// down before touching anything. So an object labelled for a *different*
// profile, reached from here, is a stale claim left behind by a profile that no
// longer competes for the namespace: most often the previous owner, now
// deleted. Reclaiming it is correct, and refusing would instead strand the
// namespace, manageable by nobody, until someone edited labels by hand.
//
// The one genuine refusal left is an object this operator never created at all.
// Writing to that would be indistinguishable from seizing somebody else's
// resource, so it requires spec.adoptExisting.
func mayWrite(decision resources.ManageDecision) bool {
	return decision != resources.ManageRefuseForeign
}

// conflictMessage explains, in terms a user can act on, why the operator will
// not write to an object.
func conflictMessage(kind, name string, obj metav1.Object) string {
	return fmt.Sprintf(
		"the %s %q already exists without this operator's ownership labels "+
			"(managed-by=%q); set spec.adoptExisting to take it over deliberately",
		kind, name, obj.GetLabels()[platformv1alpha1.ManagedByLabel])
}

// ensureResourceQuota brings the namespace's ResourceQuota in line with the
// profile, creating, correcting, or withdrawing it as the profile dictates.
func (r *NamespaceProfileReconciler) ensureResourceQuota(
	ctx context.Context, profile *platformv1alpha1.NamespaceProfile,
) (resourceOutcome, error) {
	log := logf.FromContext(ctx).WithValues("resourceQuota", platformv1alpha1.ResourceQuotaName)
	report := func(state platformv1alpha1.ManagedResourceState, message string) platformv1alpha1.ManagedResourceStatus {
		return platformv1alpha1.ManagedResourceStatus{
			Kind:    "ResourceQuota",
			Name:    platformv1alpha1.ResourceQuotaName,
			State:   state,
			Message: message,
		}
	}

	key := client.ObjectKey{
		Namespace: profile.Spec.Namespace,
		Name:      platformv1alpha1.ResourceQuotaName,
	}

	var actual corev1.ResourceQuota
	err := r.Get(ctx, key, &actual)
	if err != nil && !apierrors.IsNotFound(err) {
		return resourceOutcome{status: report(platformv1alpha1.ManagedResourceMissing, "could not be read")},
			fmt.Errorf("reading ResourceQuota %s: %w", key, err)
	}
	exists := err == nil

	desired := resources.ResourceQuota(profile)

	// The profile declares no quota. Withdraw one the operator previously
	// created, and never touch one it did not.
	if desired == nil {
		if !exists || !resources.IsOwnedBy(&actual, profile) {
			return resourceOutcome{status: report(platformv1alpha1.ManagedResourceNotManaged, "")}, nil
		}
		if err := r.Delete(ctx, &actual); err != nil && !apierrors.IsNotFound(err) {
			return resourceOutcome{status: report(platformv1alpha1.ManagedResourceNotManaged, "could not be removed")},
				fmt.Errorf("deleting ResourceQuota %s: %w", key, err)
		}
		log.Info("removed ResourceQuota that the profile no longer declares")
		return resourceOutcome{status: report(platformv1alpha1.ManagedResourceNotManaged, "")}, nil
	}

	drifted := false
	if exists {
		if decision := resources.CanManage(&actual, profile); !mayWrite(decision) {
			log.Info("refusing to modify a ResourceQuota this operator does not own",
				"managedBy", actual.Labels[platformv1alpha1.ManagedByLabel],
				"existingProfile", resources.ManagingProfile(&actual))
			return resourceOutcome{status: report(platformv1alpha1.ManagedResourceConflict,
				conflictMessage("ResourceQuota", platformv1alpha1.ResourceQuotaName, &actual))}, nil
		}
		if resources.QuotaInSync(profile, &actual) {
			return resourceOutcome{status: report(platformv1alpha1.ManagedResourceInSync, "")}, nil
		}
		drifted = true
		log.Info("correcting ResourceQuota drift")
	}

	if err := r.apply(ctx, desired); err != nil {
		state := platformv1alpha1.ManagedResourceMissing
		if exists {
			state = platformv1alpha1.ManagedResourceDrifted
		}
		return resourceOutcome{status: report(state, "could not be applied")},
			fmt.Errorf("applying ResourceQuota %s: %w", key, err)
	}

	if drifted {
		r.recordDrift(profile, "ResourceQuota", platformv1alpha1.ResourceQuotaName)
	} else {
		log.Info("created ResourceQuota")
		r.event(profile, corev1.EventTypeNormal, platformv1alpha1.ReasonProvisioned,
			platformv1alpha1.ActionProvisioning, "created ResourceQuota %s/%s",
			profile.Spec.Namespace, platformv1alpha1.ResourceQuotaName)
	}
	return resourceOutcome{status: report(platformv1alpha1.ManagedResourceInSync, ""), drifted: drifted}, nil
}

// ensureLimitRange brings the namespace's LimitRange in line with the profile.
//
// It mirrors ensureResourceQuota rather than sharing a generic helper with it.
// The two differ in how ownership of their fields works — a quota's hard limits
// are a map with per-key ownership, while a limit range's items are an atomic
// list — and collapsing them behind one abstraction would hide exactly the
// distinction that makes each one correct.
func (r *NamespaceProfileReconciler) ensureLimitRange(
	ctx context.Context, profile *platformv1alpha1.NamespaceProfile,
) (resourceOutcome, error) {
	log := logf.FromContext(ctx).WithValues("limitRange", platformv1alpha1.LimitRangeName)
	report := func(state platformv1alpha1.ManagedResourceState, message string) platformv1alpha1.ManagedResourceStatus {
		return platformv1alpha1.ManagedResourceStatus{
			Kind:    "LimitRange",
			Name:    platformv1alpha1.LimitRangeName,
			State:   state,
			Message: message,
		}
	}

	key := client.ObjectKey{
		Namespace: profile.Spec.Namespace,
		Name:      platformv1alpha1.LimitRangeName,
	}

	var actual corev1.LimitRange
	err := r.Get(ctx, key, &actual)
	if err != nil && !apierrors.IsNotFound(err) {
		return resourceOutcome{status: report(platformv1alpha1.ManagedResourceMissing, "could not be read")},
			fmt.Errorf("reading LimitRange %s: %w", key, err)
	}
	exists := err == nil

	desired := resources.LimitRange(profile)

	if desired == nil {
		if !exists || !resources.IsOwnedBy(&actual, profile) {
			return resourceOutcome{status: report(platformv1alpha1.ManagedResourceNotManaged, "")}, nil
		}
		if err := r.Delete(ctx, &actual); err != nil && !apierrors.IsNotFound(err) {
			return resourceOutcome{status: report(platformv1alpha1.ManagedResourceNotManaged, "could not be removed")},
				fmt.Errorf("deleting LimitRange %s: %w", key, err)
		}
		log.Info("removed LimitRange that the profile no longer declares")
		return resourceOutcome{status: report(platformv1alpha1.ManagedResourceNotManaged, "")}, nil
	}

	drifted := false
	if exists {
		if decision := resources.CanManage(&actual, profile); !mayWrite(decision) {
			log.Info("refusing to modify a LimitRange this operator does not own",
				"managedBy", actual.Labels[platformv1alpha1.ManagedByLabel],
				"existingProfile", resources.ManagingProfile(&actual))
			return resourceOutcome{status: report(platformv1alpha1.ManagedResourceConflict,
				conflictMessage("LimitRange", platformv1alpha1.LimitRangeName, &actual))}, nil
		}
		if resources.LimitRangeInSync(profile, &actual) {
			return resourceOutcome{status: report(platformv1alpha1.ManagedResourceInSync, "")}, nil
		}
		drifted = true
		log.Info("correcting LimitRange drift")
	}

	if err := r.apply(ctx, desired); err != nil {
		state := platformv1alpha1.ManagedResourceMissing
		if exists {
			state = platformv1alpha1.ManagedResourceDrifted
		}
		return resourceOutcome{status: report(state, "could not be applied")},
			fmt.Errorf("applying LimitRange %s: %w", key, err)
	}

	if drifted {
		r.recordDrift(profile, "LimitRange", platformv1alpha1.LimitRangeName)
	} else {
		log.Info("created LimitRange")
		r.event(profile, corev1.EventTypeNormal, platformv1alpha1.ReasonProvisioned,
			platformv1alpha1.ActionProvisioning, "created LimitRange %s/%s",
			profile.Spec.Namespace, platformv1alpha1.LimitRangeName)
	}
	return resourceOutcome{status: report(platformv1alpha1.ManagedResourceInSync, ""), drifted: drifted}, nil
}

// apply performs a server-side apply of one generated object.
//
// Every write the operator makes goes through here, under a single field
// manager. The API server records per-field ownership against that manager, so
// the operator asserts exactly the fields its apply configuration declares and
// nothing else: unrelated labels, annotations and fields set by other actors
// survive untouched, and a field the operator stops declaring is released
// rather than left behind.
//
// ForceOwnership is what makes drift correction work. Editing an object by hand
// transfers ownership of the edited field to kubectl, and a non-forcing apply
// would then fail with a conflict and leave the drift in place — so a quota
// raised from 8 to 20 would stay at 20, which is precisely the behaviour this
// operator exists to prevent. Forcing is safe here only because every caller
// has already established that the object carries this profile's ownership
// labels; an object that fails that check is never passed to this function at
// all.
func (r *NamespaceProfileReconciler) apply(ctx context.Context, obj runtime.ApplyConfiguration) error {
	return r.Apply(ctx, obj,
		client.FieldOwner(platformv1alpha1.OperatorName),
		client.ForceOwnership,
	)
}

// recordDrift logs, counts and announces a correction the operator had to make.
//
// All three together, because each answers a different question: the log says
// what happened, the metric says how often, and the Event puts it on the
// profile where whoever is fighting the operator will see it.
func (r *NamespaceProfileReconciler) recordDrift(
	profile *platformv1alpha1.NamespaceProfile, kind, name string,
) {
	metrics.RecordDrift(profile.Name, kind)
	r.event(profile, corev1.EventTypeNormal, platformv1alpha1.ReasonDriftCorrected,
		platformv1alpha1.ActionCorrecting,
		"%s %s diverged from the profile and was corrected", kind, name)
}

// event publishes a Kubernetes Event against the profile.
//
// Tolerating a nil Recorder keeps the reconciler usable when constructed
// directly — in a test, say — without making a missing recorder able to crash
// the manager.
func (r *NamespaceProfileReconciler) event(
	profile *platformv1alpha1.NamespaceProfile, eventType, reason, action, note string, args ...any,
) {
	if r.Recorder == nil {
		return
	}
	// The nil second argument is the "related" object, which this operator
	// never has: every event it emits is about the profile itself.
	r.Recorder.Eventf(profile, nil, eventType, reason, action, note, args...)
}

// SetupWithManager registers the controller and everything it watches.
//
// The operator sets no owner references — a namespace must never be garbage
// collected because a profile was deleted — so `.Owns()` is unavailable and
// each managed kind needs an explicit watch with a map function. The field
// index registered first is what makes those map functions cheap: an event on
// a ResourceQuota knows only its namespace, and the index turns that namespace
// into the profiles that claim it.
func (r *NamespaceProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("namespaceprofile-controller")
	}

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&platformv1alpha1.NamespaceProfile{},
		specNamespaceIndex,
		func(obj client.Object) []string {
			profile, ok := obj.(*platformv1alpha1.NamespaceProfile)
			if !ok || profile.Spec.Namespace == "" {
				return nil
			}
			return []string{profile.Spec.Namespace}
		},
	); err != nil {
		return fmt.Errorf("indexing NamespaceProfile by %s: %w", specNamespaceIndex, err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		// Status updates must not wake the controller, or writing status would
		// trigger a reconciliation that writes status again. Generation
		// changes cover spec edits; annotation changes are also needed because
		// the namespace-deletion confirmation lives in an annotation, which
		// does not bump generation.
		For(&platformv1alpha1.NamespaceProfile{}, builder.WithPredicates(
			predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.AnnotationChangedPredicate{},
			),
		)).
		// Namespaces are watched unfiltered. The index makes an event for a
		// namespace no profile targets resolve to zero requests, which is
		// cheaper than any predicate could be, and it means a namespace being
		// deleted or re-created is noticed immediately.
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.profilesForNamespace)).
		Watches(&corev1.ResourceQuota{},
			handler.EnqueueRequestsFromMapFunc(r.profilesForManagedObject),
			builder.WithPredicates(managedResourcePredicate())).
		Watches(&corev1.LimitRange{},
			handler.EnqueueRequestsFromMapFunc(r.profilesForManagedObject),
			builder.WithPredicates(managedResourcePredicate())).
		Watches(&networkingv1.NetworkPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.profilesForManagedObject),
			builder.WithPredicates(managedResourcePredicate())).
		Named("namespaceprofile").
		Complete(r)
}

// managedResourcePredicate keeps the controller from waking for namespaced
// objects that have nothing to do with it.
//
// An object qualifies if it carries the operator's managed-by label, or if it
// merely occupies one of the operator's deterministic names. The second clause
// matters in both directions: a foreign object appearing at such a name is a
// conflict worth reporting, and one being deleted frees the name so the
// operator can finally create its own.
func managedResourcePredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		if resources.IsManagedByOperator(obj) {
			return true
		}
		switch obj.GetName() {
		case platformv1alpha1.ResourceQuotaName,
			platformv1alpha1.LimitRangeName,
			platformv1alpha1.DenyIngressPolicyName,
			platformv1alpha1.DenyEgressPolicyName,
			platformv1alpha1.AllowDNSPolicyName,
			platformv1alpha1.AllowSameNamespacePolicyName,
			platformv1alpha1.AllowKubeAPIPolicyName,
			platformv1alpha1.AllowEgressToNamespacesPolicyName,
			platformv1alpha1.AllowIngressFromNamespacesPolicyName:
			return true
		default:
			return false
		}
	})
}

// profilesForNamespace maps a Namespace event to the profiles targeting it.
func (r *NamespaceProfileReconciler) profilesForNamespace(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	return r.profilesTargeting(ctx, obj.GetName())
}

// profilesForManagedObject maps an event on a namespaced resource to the
// profiles targeting the namespace it lives in.
func (r *NamespaceProfileReconciler) profilesForManagedObject(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	return r.profilesTargeting(ctx, obj.GetNamespace())
}

// profilesTargeting returns a reconcile request for every profile that claims
// the given namespace.
//
// It returns more than one when profiles conflict over a namespace. That is
// intentional: both sides need to reconcile so that both report the conflict,
// rather than only whichever happened to be reconciled last.
func (r *NamespaceProfileReconciler) profilesTargeting(
	ctx context.Context, namespace string,
) []reconcile.Request {
	if namespace == "" {
		return nil
	}

	var profiles platformv1alpha1.NamespaceProfileList
	if err := r.List(ctx, &profiles, client.MatchingFields{specNamespaceIndex: namespace}); err != nil {
		// Dropping the event is the only option — there is nothing to return a
		// failure to. The periodic resync is the backstop, so a lost event
		// delays convergence rather than preventing it.
		logf.FromContext(ctx).Error(err, "could not map event to profiles", "namespace", namespace)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(profiles.Items))
	for i := range profiles.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: profiles.Items[i].Name},
		})
	}
	return requests
}
