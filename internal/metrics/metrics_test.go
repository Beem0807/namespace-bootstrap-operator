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

package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Forget is the one here with real consequences: without it, a deleted profile
// leaves nsprofile_ready at 0 forever and any alert on it fires indefinitely
// for a namespace nobody manages.
func TestForgetDropsEverySeriesForAProfile(t *testing.T) {
	const profile = "forget-me"

	SetReady(profile, true)
	RecordDrift(profile, "ResourceQuota")
	RecordConflict(profile, "NamespaceConflict")

	if got := testutil.CollectAndCount(Ready); got == 0 {
		t.Fatal("expected a readiness series before forgetting")
	}

	Forget(profile)

	if got := testutil.CollectAndCount(Ready); got != 0 {
		t.Errorf("nsprofile_ready still has %d series after Forget", got)
	}
	// Counters are deliberately dropped too: a profile that is gone should not
	// keep contributing to a rate() long after it stopped existing.
	if got := testutil.CollectAndCount(DriftCorrectionsTotal); got != 0 {
		t.Errorf("nsprofile_drift_corrections_total still has %d series", got)
	}
	if got := testutil.CollectAndCount(ConflictsTotal); got != 0 {
		t.Errorf("nsprofile_conflicts_total still has %d series", got)
	}
}

func TestForgetLeavesOtherProfilesAlone(t *testing.T) {
	SetReady("keep-me", true)
	SetReady("drop-me", false)

	Forget("drop-me")

	if got := testutil.CollectAndCount(Ready); got != 1 {
		t.Errorf("expected exactly the surviving profile's series, got %d", got)
	}
	Forget("keep-me")
}

func TestSetReadyMapsBoolToGaugeValue(t *testing.T) {
	SetReady("ready-probe", true)
	if got := testutil.ToFloat64(Ready.WithLabelValues("ready-probe")); got != 1 {
		t.Errorf("ready gauge = %v, want 1", got)
	}
	SetReady("ready-probe", false)
	if got := testutil.ToFloat64(Ready.WithLabelValues("ready-probe")); got != 0 {
		t.Errorf("ready gauge = %v, want 0", got)
	}
	Forget("ready-probe")
}

// build_info is always 1; the labels carry the information. The point of the
// test is that the label names stay stable, because a dashboard or an alert
// grouping by "version" breaks silently if one is renamed.
func TestBuildInfoCarriesItsLabels(t *testing.T) {
	RecordBuildInfo("1.2.3", "abc1234", "2026-01-01T00:00:00Z", "go1.26.3")

	expected := `
# HELP nsprofile_build_info Build identity of the running operator. Always 1; the labels carry the information.
# TYPE nsprofile_build_info gauge
nsprofile_build_info{build_date="2026-01-01T00:00:00Z",commit="abc1234",go_version="go1.26.3",version="1.2.3"} 1
`
	if err := testutil.CollectAndCompare(BuildInfo, strings.NewReader(expected)); err != nil {
		t.Error(err)
	}
	BuildInfo.Reset()
}
