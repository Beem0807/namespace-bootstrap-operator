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
	"slices"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
	"github.com/Beem0807/namespace-bootstrap-operator/internal/metrics"
	"github.com/Beem0807/namespace-bootstrap-operator/internal/resources"
)

// ownership is which profile governs a namespace, and who else wants it.
type ownership struct {
	// owner is the name of the profile entitled to manage the namespace.
	owner string

	// competitors are the other profiles targeting the same namespace, named
	// so that the conflict is visible from every side rather than only from
	// whichever profile happened to reconcile last.
	competitors []string
}

// resolveOwnership decides which of the profiles targeting a namespace is
// entitled to manage it.
//
// The rule has to be deterministic, because the alternative is two controllers
// taking turns: each reconciliation would hand the namespace to whichever
// profile ran most recently, rewriting labels and policies forever while both
// profiles reported success. Nothing would be broken, exactly, and nothing
// would ever be stable either.
//
// So ownership is decided in a fixed order:
//
//  1. An existing claim on the namespace wins. If the namespace already
//     carries this operator's labels naming a live profile, that profile keeps
//     it. Ownership is therefore stable over time: it does not change because
//     somebody later created a profile that happens to be older.
//  2. Otherwise the oldest profile wins, with the name breaking ties. Creation
//     timestamps have one-second granularity, so ties are entirely possible and
//     a tie-break is not a formality.
//
// Profiles being deleted are excluded, which is what lets a namespace be handed
// from a retiring profile to its replacement without manual intervention.
func (r *NamespaceProfileReconciler) resolveOwnership(
	ctx context.Context, profile *platformv1alpha1.NamespaceProfile,
) (ownership, error) {
	// Filtered in Go rather than through the spec.namespace field index, so the
	// reconciler works with any client — including an uncached one, which is
	// what makes it testable without starting a manager. The cost is trivial:
	// NamespaceProfile is cluster-scoped with roughly one object per governed
	// namespace, and this runs once per reconciliation. The index stays where
	// it earns its keep, in the event mappers, which run per watch event.
	var profiles platformv1alpha1.NamespaceProfileList
	if err := r.List(ctx, &profiles); err != nil {
		return ownership{}, fmt.Errorf("listing profiles targeting %q: %w", profile.Spec.Namespace, err)
	}

	candidates := make([]platformv1alpha1.NamespaceProfile, 0, 2)
	for i := range profiles.Items {
		candidate := profiles.Items[i]
		if candidate.Spec.Namespace != profile.Spec.Namespace {
			continue
		}
		// A profile being deleted is not a competitor, which is what lets a
		// namespace pass from a retiring profile to its replacement without
		// anyone intervening.
		if candidate.DeletionTimestamp.IsZero() {
			candidates = append(candidates, candidate)
		}
	}

	// The index is populated from the cache, which can briefly lag behind a
	// profile this reconciliation already holds. Treat the profile in hand as
	// authoritative about its own existence.
	if !slices.ContainsFunc(candidates, func(c platformv1alpha1.NamespaceProfile) bool {
		return c.Name == profile.Name
	}) {
		candidates = append(candidates, *profile)
	}

	if len(candidates) == 1 {
		return ownership{owner: profile.Name}, nil
	}

	names := make([]string, 0, len(candidates))
	for i := range candidates {
		names = append(names, candidates[i].Name)
	}

	owner := ""

	var namespace corev1.Namespace
	err := r.Get(ctx, client.ObjectKey{Name: profile.Spec.Namespace}, &namespace)
	switch {
	case err == nil && resources.IsManagedByOperator(&namespace):
		if claimed := resources.ManagingProfile(&namespace); slices.Contains(names, claimed) {
			owner = claimed
		}
	case err != nil && !apierrors.IsNotFound(err):
		return ownership{}, fmt.Errorf("reading namespace %q: %w", profile.Spec.Namespace, err)
	}

	if owner == "" {
		oldest := candidates[0]
		for i := range candidates[1:] {
			candidate := candidates[i+1]
			switch {
			case candidate.CreationTimestamp.Before(&oldest.CreationTimestamp):
				oldest = candidate
			case oldest.CreationTimestamp.Equal(&candidate.CreationTimestamp) &&
				candidate.Name < oldest.Name:
				oldest = candidate
			}
		}
		owner = oldest.Name
	}

	competitors := make([]string, 0, len(names)-1)
	for _, name := range names {
		if name != profile.Name {
			competitors = append(competitors, name)
		}
	}
	slices.Sort(competitors)

	return ownership{owner: owner, competitors: competitors}, nil
}

// ensureFinalizer registers the operator's interest in running cleanup before
// the profile disappears.
//
// A finalizer is needed because cleanup is a decision rather than a fixed
// object graph: whether to delete the namespace depends on the lifecycle
// settings, a confirmation annotation and whether the namespace was adopted,
// and whether to remove the generated policy objects depends on a flag that can
// be changed after those objects were created. Owner references are fixed when
// an object is created and can express none of that — and wiring the namespace
// into garbage collection would make deleting a profile capable of deleting
// live workloads.
func (r *NamespaceProfileReconciler) ensureFinalizer(
	ctx context.Context, profile *platformv1alpha1.NamespaceProfile,
) error {
	if controllerutil.ContainsFinalizer(profile, platformv1alpha1.CleanupFinalizer) {
		return nil
	}

	patch := client.MergeFrom(profile.DeepCopy())
	controllerutil.AddFinalizer(profile, platformv1alpha1.CleanupFinalizer)
	if err := r.Patch(ctx, profile, patch); err != nil {
		return fmt.Errorf("adding finalizer: %w", err)
	}
	// Patch writes the server's response back into profile, so the caller
	// continues with a current resourceVersion and the status write later in
	// this reconciliation does not conflict with this one.
	return nil
}

// finalize runs cleanup and then releases the profile.
//
// Cleanup is idempotent: every deletion treats NotFound as success, so a
// partially completed pass simply finishes on the next one. If it cannot
// complete, the error is returned and the finalizer stays — the profile remains
// in Terminating, which is the correct failure mode. It is visible, it is
// retried with backoff, and it never leaves policy behind that nothing
// explains.
func (r *NamespaceProfileReconciler) finalize(
	ctx context.Context, profile *platformv1alpha1.NamespaceProfile,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(profile, platformv1alpha1.CleanupFinalizer) {
		return ctrl.Result{}, nil
	}

	lifecycle := profile.Spec.Lifecycle
	retain := lifecycle != nil && lifecycle.ManagedResourcesRetained()

	if retain {
		// Deliberately leaving policy in place for another system to take
		// over. Worth saying out loud, because the namespace is now carrying
		// a quota and network policies that no profile accounts for.
		log.Info("retaining generated resources as the profile requests; " +
			"the namespace keeps policy that no NamespaceProfile explains")
		r.event(profile, corev1.EventTypeWarning, platformv1alpha1.ReasonTerminating,
			platformv1alpha1.ActionReconciling,
			"retaining generated resources in namespace %s; they are now unmanaged",
			profile.Spec.Namespace)
	} else if err := r.deleteManagedResources(ctx, profile); err != nil {
		return ctrl.Result{}, err
	}

	// The operator's labels are deliberately left on a retained namespace.
	// They remain true — this operator did create it — and they are what lets a
	// replacement profile reclaim the namespace rather than being refused as
	// though it belonged to a stranger. Stripping them would strand the
	// namespace, manageable by nobody, until somebody edited labels by hand.
	// The Pod Security labels stay for the same reason plus a better one:
	// removing a namespace's enforcement level because a profile was deleted
	// would quietly weaken it.

	if err := r.maybeDeleteNamespace(ctx, profile); err != nil {
		return ctrl.Result{}, err
	}

	patch := client.MergeFrom(profile.DeepCopy())
	controllerutil.RemoveFinalizer(profile, platformv1alpha1.CleanupFinalizer)
	if err := r.Patch(ctx, profile, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	metrics.Forget(profile.Name)
	log.Info("cleanup complete")
	return ctrl.Result{}, nil
}

// deleteManagedResources removes the quota, limit range and network policies
// this profile generated, leaving the namespace and everything in it alone.
//
// Only objects carrying this profile's ownership labels are touched. An object
// the operator refused to manage during provisioning is equally untouchable
// here: cleanup must not become a way to delete something the operator was
// never allowed to write to.
func (r *NamespaceProfileReconciler) deleteManagedResources(
	ctx context.Context, profile *platformv1alpha1.NamespaceProfile,
) error {
	log := logf.FromContext(ctx)

	var quota corev1.ResourceQuota
	quotaKey := client.ObjectKey{Namespace: profile.Spec.Namespace, Name: platformv1alpha1.ResourceQuotaName}
	switch err := r.Get(ctx, quotaKey, &quota); {
	case err == nil:
		if resources.IsOwnedBy(&quota, profile) {
			if err := r.Delete(ctx, &quota); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting ResourceQuota %s: %w", quotaKey, err)
			}
			log.Info("removed ResourceQuota during cleanup")
		}
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("reading ResourceQuota %s: %w", quotaKey, err)
	}

	var limits corev1.LimitRange
	limitsKey := client.ObjectKey{Namespace: profile.Spec.Namespace, Name: platformv1alpha1.LimitRangeName}
	switch err := r.Get(ctx, limitsKey, &limits); {
	case err == nil:
		if resources.IsOwnedBy(&limits, profile) {
			if err := r.Delete(ctx, &limits); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting LimitRange %s: %w", limitsKey, err)
			}
			log.Info("removed LimitRange during cleanup")
		}
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("reading LimitRange %s: %w", limitsKey, err)
	}

	// Policies are found by label rather than by name, so a policy left behind
	// by an older version of this operator — under a name the current version
	// no longer generates — is still cleaned up.
	var policies networkingv1.NetworkPolicyList
	if err := r.List(ctx, &policies,
		client.InNamespace(profile.Spec.Namespace),
		client.MatchingLabels(resources.OwnershipLabels(profile)),
	); err != nil {
		return fmt.Errorf("listing NetworkPolicies in %q: %w", profile.Spec.Namespace, err)
	}
	for i := range policies.Items {
		policy := &policies.Items[i]
		if !resources.IsOwnedBy(policy, profile) {
			continue
		}
		if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting NetworkPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
		}
		log.Info("removed NetworkPolicy during cleanup", "networkPolicy", policy.Name)
	}

	return nil
}

// maybeDeleteNamespace deletes the target namespace, but only if every safety
// check passes.
//
// Deleting a namespace cascades to everything inside it — Deployments,
// StatefulSets, PersistentVolumeClaims and their data, Secrets — none of which
// this operator manages or could restore. So the bar is four independent
// conditions, and a single failure means the namespace is retained and the
// reason is said out loud.
//
// Notably, none of the checks involve listing Pods. An "is the namespace empty?"
// test would need Pod read permission across the cluster, for a check the
// confirmation annotation already covers more reliably: an empty namespace
// today may be a busy one tomorrow, whereas a human typing the namespace name
// is a statement of intent.
func (r *NamespaceProfileReconciler) maybeDeleteNamespace(
	ctx context.Context, profile *platformv1alpha1.NamespaceProfile,
) error {
	log := logf.FromContext(ctx)

	lifecycle := profile.Spec.Lifecycle
	if lifecycle == nil || !lifecycle.NamespaceDeletionRequested() {
		return nil
	}

	refuse := func(reason string) {
		log.Info("retaining the namespace despite deleteNamespaceOnRemoval", "reason", reason)
		r.event(profile, corev1.EventTypeWarning, platformv1alpha1.ReasonDeletionNotConfirmed,
			platformv1alpha1.ActionReconciling,
			"namespace %s was retained: %s", profile.Spec.Namespace, reason)
	}

	// The "type the name to confirm" gate. A bare boolean is too easy to copy
	// between profiles or set by a templating mistake; requiring the namespace
	// name to be repeated means a confirmation cannot be accidentally correct
	// for the wrong namespace.
	confirmation := profile.Annotations[platformv1alpha1.ConfirmNamespaceDeletionAnnotation]
	if confirmation != profile.Spec.Namespace {
		refuse(fmt.Sprintf(
			"annotation %s must be set to %q to confirm deletion",
			platformv1alpha1.ConfirmNamespaceDeletionAnnotation, profile.Spec.Namespace))
		return nil
	}

	var namespace corev1.Namespace
	switch err := r.Get(ctx, client.ObjectKey{Name: profile.Spec.Namespace}, &namespace); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("reading namespace %q: %w", profile.Spec.Namespace, err)
	}

	if !namespace.DeletionTimestamp.IsZero() {
		return nil
	}

	if !resources.IsOwnedBy(&namespace, profile) {
		refuse("the namespace does not carry this profile's ownership labels")
		return nil
	}

	// An adopted namespace is permanently disqualified. The operator did not
	// create it and cannot know what else depends on it.
	if resources.WasAdopted(&namespace) {
		refuse("the namespace was adopted rather than created by this profile, " +
			"so the operator will not delete it")
		return nil
	}

	if err := r.Delete(ctx, &namespace); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting namespace %q: %w", profile.Spec.Namespace, err)
	}

	log.Info("deleted the namespace, as explicitly confirmed by the profile")
	r.event(profile, corev1.EventTypeWarning, platformv1alpha1.ReasonTerminating,
		platformv1alpha1.ActionReconciling,
		"deleted namespace %s and everything in it, as confirmed by annotation %s",
		profile.Spec.Namespace, platformv1alpha1.ConfirmNamespaceDeletionAnnotation)
	return nil
}
