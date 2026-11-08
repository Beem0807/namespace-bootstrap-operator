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

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
	"github.com/Beem0807/namespace-bootstrap-operator/internal/metrics"
)

// maxStatusMessage bounds how much of an error ends up on the object.
//
// Status is replicated to every watcher of the resource and stored in etcd, so
// a long Go error chain pasted in verbatim is a cost paid by the whole cluster
// for text nobody reads past the first line. The full error is in the log.
const maxStatusMessage = 400

// observation is everything one reconciliation learned, gathered so that
// status is computed in a single place from facts rather than re-derived from
// the cluster.
type observation struct {
	// resources is what was found for each resource the profile governs.
	resources []platformv1alpha1.ManagedResourceStatus

	// drifted records that at least one resource had to be rewritten because
	// it had diverged from the profile.
	drifted bool

	// progressing records that the reconciliation stopped in a state it
	// expects to leave on its own — a terminating namespace, say.
	progressing bool

	// blocked records that the reconciliation stopped because something is not
	// the profile's to manage. Unlike progressing, this will not resolve
	// without a person intervening.
	blocked       bool
	blockedReason string

	// competitors are the other profiles targeting the same namespace, recorded
	// only on a profile that lost the arbitration. The winner is healthy and says
	// nothing; see the note in provision() for why.
	competitors []string
}

// record adds one resource's outcome.
func (o *observation) record(status platformv1alpha1.ManagedResourceStatus, drifted bool) {
	o.resources = append(o.resources, status)
	o.drifted = o.drifted || drifted
}

// allInSync reports whether every resource is either matching the profile or
// deliberately unmanaged.
//
// NotManaged counts as in sync: a profile that declares no quota is correctly
// configured when no quota exists, so treating its absence as a shortfall
// would leave such a profile permanently not-Ready.
func (o *observation) allInSync() bool {
	for _, resource := range o.resources {
		switch resource.State {
		case platformv1alpha1.ManagedResourceInSync, platformv1alpha1.ManagedResourceNotManaged:
			continue
		default:
			return false
		}
	}
	return true
}

// blockingMessage is the explanation from the first resource that is blocked,
// which is what a user needs to read to unblock the profile.
func (o *observation) blockingMessage() string {
	for _, resource := range o.resources {
		if resource.State == platformv1alpha1.ManagedResourceConflict && resource.Message != "" {
			return resource.Message
		}
	}
	return "a resource this profile governs is not owned by it"
}

// patchStatus records the outcome of one reconciliation on the profile.
//
// It writes nothing when nothing changed. That restraint is not an
// optimisation: the controller watches NamespaceProfile, so an unconditional
// status write would produce a watch event, which would trigger another
// reconciliation, which would write status again. The generation-based
// predicate in SetupWithManager is the second half of that defence; this is
// the first.
func (r *NamespaceProfileReconciler) patchStatus(
	ctx context.Context,
	profile *platformv1alpha1.NamespaceProfile,
	obs observation,
	provisionErr error,
) error {
	original := profile.DeepCopy()

	profile.Status.ObservedGeneration = profile.Generation
	profile.Status.TargetNamespace = profile.Spec.Namespace
	profile.Status.ManagedResources = obs.resources
	profile.Status.ConflictingProfiles = obs.competitors

	ready := provisionErr == nil && !obs.progressing && !obs.blocked && obs.allInSync()

	switch {
	case provisionErr != nil:
		// Something went wrong talking to the API server. The controller will
		// retry with backoff, so this is both degraded and still progressing.
		message := truncate(provisionErr.Error())
		profile.Status.Phase = platformv1alpha1.ProfilePhaseDegraded
		r.setCondition(profile, platformv1alpha1.ConditionReady, metav1.ConditionFalse,
			platformv1alpha1.ReasonReconcileError, message)
		r.setCondition(profile, platformv1alpha1.ConditionProgressing, metav1.ConditionTrue,
			platformv1alpha1.ReasonReconcileError, "the operator will retry with backoff")
		r.setCondition(profile, platformv1alpha1.ConditionDegraded, metav1.ConditionTrue,
			platformv1alpha1.ReasonReconcileError, message)
		r.setCondition(profile, platformv1alpha1.ConditionPolicyCompliant, metav1.ConditionUnknown,
			platformv1alpha1.ReasonReconcileError, "the live configuration could not be determined")

	case obs.blocked:
		// Not Ready, but emphatically not Degraded: declining to seize a
		// resource the profile does not own is the operator working exactly as
		// designed. Conflating the two would page somebody for correct
		// behaviour.
		message := obs.blockingMessage()
		profile.Status.Phase = platformv1alpha1.ProfilePhaseConflict
		r.setCondition(profile, platformv1alpha1.ConditionReady, metav1.ConditionFalse,
			obs.blockedReason, message)
		r.setCondition(profile, platformv1alpha1.ConditionProgressing, metav1.ConditionFalse,
			obs.blockedReason, "the operator is idle until the conflict is resolved")
		r.setCondition(profile, platformv1alpha1.ConditionDegraded, metav1.ConditionFalse,
			obs.blockedReason, "nothing has failed; the operator is declining to take ownership")
		r.setCondition(profile, platformv1alpha1.ConditionPolicyCompliant, metav1.ConditionFalse,
			obs.blockedReason, message)

	case obs.progressing:
		profile.Status.Phase = platformv1alpha1.ProfilePhaseProvisioning
		r.setCondition(profile, platformv1alpha1.ConditionReady, metav1.ConditionFalse,
			platformv1alpha1.ReasonNamespaceTerminating,
			"the target namespace is terminating; provisioning resumes once it is gone")
		r.setCondition(profile, platformv1alpha1.ConditionProgressing, metav1.ConditionTrue,
			platformv1alpha1.ReasonNamespaceTerminating, "waiting for the namespace to finish terminating")
		r.setCondition(profile, platformv1alpha1.ConditionDegraded, metav1.ConditionFalse,
			platformv1alpha1.ReasonNamespaceTerminating, "nothing has failed")
		r.setCondition(profile, platformv1alpha1.ConditionPolicyCompliant, metav1.ConditionFalse,
			platformv1alpha1.ReasonNamespaceTerminating, "the namespace does not currently exist")

	default:
		profile.Status.Phase = platformv1alpha1.ProfilePhaseReady
		r.setCondition(profile, platformv1alpha1.ConditionReady, metav1.ConditionTrue,
			platformv1alpha1.ReasonProvisioned,
			"every resource this profile declares exists and matches it")
		r.setCondition(profile, platformv1alpha1.ConditionProgressing, metav1.ConditionFalse,
			platformv1alpha1.ReasonProvisioned, "the declared configuration is in force")
		r.setCondition(profile, platformv1alpha1.ConditionDegraded, metav1.ConditionFalse,
			platformv1alpha1.ReasonProvisioned, "nothing has failed")
		r.setCondition(profile, platformv1alpha1.ConditionPolicyCompliant, metav1.ConditionTrue,
			platformv1alpha1.ReasonProvisioned, "the live configuration matches the profile")
	}

	// Set before the comparison below, because a correction that restores a
	// resource leaves every other status field identical — this timestamp is
	// the only trace that something is repeatedly fighting the operator.
	if obs.drifted {
		now := metav1.Now()
		profile.Status.LastDriftCorrectionTime = &now
	}

	metrics.SetReady(profile.Name, ready)

	// Compared before lastSuccessfulReconcileTime is touched, so a
	// reconciliation that found nothing to do does not write status purely to
	// move a clock forward. The consequence, deliberately accepted, is that
	// the timestamp marks the last reconciliation that *changed* something
	// rather than acting as a heartbeat — liveness is a question for the
	// controller-runtime metrics, not for status.
	if apiequality.Semantic.DeepEqual(original.Status, profile.Status) {
		return nil
	}

	r.announceReadyTransition(profile, original)

	if provisionErr == nil {
		now := metav1.Now()
		profile.Status.LastSuccessfulReconcileTime = &now
	}

	return r.Status().Patch(ctx, profile, client.MergeFrom(original))
}

// setCondition updates one condition, stamped with the generation it was
// computed from.
//
// meta.SetStatusCondition preserves LastTransitionTime when the status value is
// unchanged, which is what lets the equality check above treat a recomputed but
// identical condition as no change at all.
func (r *NamespaceProfileReconciler) setCondition(
	profile *platformv1alpha1.NamespaceProfile,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            truncate(message),
		ObservedGeneration: profile.Generation,
	})
}

// announceReadyTransition emits an Event when readiness actually changes.
//
// Only on a transition. A profile blocked by a conflict is reconciled on every
// event in its namespace, and emitting a warning each time would bury the
// cluster's event stream in repetitions of one unchanged fact.
func (r *NamespaceProfileReconciler) announceReadyTransition(
	profile, previous *platformv1alpha1.NamespaceProfile,
) {
	current := meta.FindStatusCondition(profile.Status.Conditions, platformv1alpha1.ConditionReady)
	if current == nil {
		return
	}

	was := meta.FindStatusCondition(previous.Status.Conditions, platformv1alpha1.ConditionReady)
	if was != nil && was.Status == current.Status && was.Reason == current.Reason {
		return
	}

	eventType := corev1.EventTypeWarning
	if current.Status == metav1.ConditionTrue {
		eventType = corev1.EventTypeNormal
	}
	r.event(profile, eventType, current.Reason, platformv1alpha1.ActionReconciling,
		"%s", current.Message)
}

// truncate bounds a status message, marking it so a reader knows the full text
// is in the operator's log rather than wondering why a sentence stops.
func truncate(message string) string {
	if len(message) <= maxStatusMessage {
		return message
	}
	return message[:maxStatusMessage] + "… (truncated; see operator logs)"
}
