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

// Package metrics holds the operator's domain metrics.
//
// controller-runtime already exports the mechanical ones —
// controller_runtime_reconcile_total, _reconcile_errors_total and
// _reconcile_time_seconds, all labelled by controller — so nothing here
// duplicates reconciliation counts, durations or error rates. What it adds is
// the handful of facts those cannot express: whether each profile is actually
// in force, how often something is fighting the operator, and how often a
// profile is blocked on a conflict it cannot resolve by itself.
//
// Every series is labelled by profile name. NamespaceProfile is cluster-scoped
// and created by platform engineers, so that label is bounded by how many
// namespaces the platform governs — it cannot be driven up by workload
// activity.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// profileLabel is the label every series carries, naming the NamespaceProfile
// the measurement belongs to.
const profileLabel = "profile"

var (
	// DriftCorrectionsTotal counts how often the operator had to rewrite a
	// managed resource that had been changed outside its profile.
	//
	// This is the metric to alert on. A steadily climbing count means
	// something — a person, a GitOps controller, another operator — is
	// repeatedly setting a field this operator owns, and the two will keep
	// overwriting each other indefinitely. The operator always wins, so
	// nothing breaks; it simply never settles, and the only visible symptom is
	// this counter.
	DriftCorrectionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nsprofile_drift_corrections_total",
			Help: "Number of times a managed resource was rewritten after diverging from its NamespaceProfile.",
		},
		[]string{profileLabel, "kind"},
	)

	// ConflictsTotal counts reconciliations that stopped because a resource
	// belonged to somebody else.
	//
	// Separate from reconcile errors on purpose: a conflict is the operator
	// working exactly as designed, declining to seize something it does not
	// own. It is not a failure, but it does mean a profile is inert and needs
	// a human.
	ConflictsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nsprofile_conflicts_total",
			Help: "Number of reconciliations halted because a resource is not owned by the NamespaceProfile.",
		},
		[]string{profileLabel, "reason"},
	)

	// Ready reports whether each profile's declared configuration is fully in
	// force: 1 when ready, 0 otherwise.
	//
	// A gauge rather than a counter, because the question it answers is "is
	// this namespace governed right now?" — which is what a dashboard and an
	// alert both want. Series are removed by Forget when a profile is deleted,
	// so a vanished profile stops reporting instead of being stuck at 0
	// forever.
	Ready = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nsprofile_ready",
			Help: "Whether a NamespaceProfile's declared configuration is fully provisioned (1) or not (0).",
		},
		[]string{profileLabel},
	)
)

// BuildInfo publishes which build is running, as labels on a gauge that is
// always 1.
//
// This is the standard Prometheus way to expose build identity, and it exists
// because the image tag is not a reliable answer: anything deployed from
// :latest, or with the tag overridden, leaves no way to tell what is actually
// running. Having it as a series means the question is answerable from a
// scrape that already exists, and that `count by (version) (nsprofile_build_info)`
// will show a straggler left behind by a partial rollout.
//
// The label set is bounded by the number of distinct builds deployed, not by
// anything a workload can influence.
var BuildInfo = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "nsprofile_build_info",
		Help: "Build identity of the running operator. Always 1; the labels carry the information.",
	},
	[]string{"version", "commit", "build_date", "go_version"},
)

func init() {
	// Registering with controller-runtime's registry rather than Prometheus's
	// default is what puts these on the manager's existing metrics endpoint,
	// alongside the built-in controller and Go runtime metrics. No separate
	// server, port or scrape target is involved.
	ctrlmetrics.Registry.MustRegister(DriftCorrectionsTotal, ConflictsTotal, Ready, BuildInfo)
}

// RecordBuildInfo publishes the running build's identity. Called once at
// startup.
func RecordBuildInfo(version, commit, buildDate, goVersion string) {
	BuildInfo.WithLabelValues(version, commit, buildDate, goVersion).Set(1)
}

// RecordDrift notes that a managed resource of the given kind had to be
// corrected for a profile.
func RecordDrift(profile, kind string) {
	DriftCorrectionsTotal.WithLabelValues(profile, kind).Inc()
}

// RecordConflict notes that a profile could not proceed because something was
// not its to manage.
func RecordConflict(profile, reason string) {
	ConflictsTotal.WithLabelValues(profile, reason).Inc()
}

// SetReady publishes whether a profile is fully in force.
func SetReady(profile string, ready bool) {
	value := 0.0
	if ready {
		value = 1.0
	}
	Ready.WithLabelValues(profile).Set(value)
}

// Forget drops every series for a profile.
//
// Called when a profile is gone. Without it, a deleted profile's gauge would
// sit at its last value forever, and an alert on nsprofile_ready == 0 would
// fire permanently for a namespace nobody is managing any more.
func Forget(profile string) {
	Ready.DeleteLabelValues(profile)
	DriftCorrectionsTotal.DeletePartialMatch(prometheus.Labels{profileLabel: profile})
	ConflictsTotal.DeletePartialMatch(prometheus.Labels{profileLabel: profile})
}
