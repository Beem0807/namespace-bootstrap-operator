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

package version

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestVersionIsSemver guards the format the release pipeline depends on when
// it builds image tags and git tags.
func TestVersionIsSemver(t *testing.T) {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(Version) {
		t.Errorf("Version = %q, want a bare semver such as 0.1.0 (no leading v)", Version)
	}
}

// TestChartAppVersionMatches keeps the chart honest about what it deploys.
//
// A chart whose appVersion disagrees with the code would install an image tag
// that does not correspond to the chart's contents — the kind of mismatch
// nobody notices until a rollback behaves strangely. Checking it here means a
// forgotten bump fails `make test`, long before the release pipeline runs the
// same comparison.
func TestChartAppVersionMatches(t *testing.T) {
	path := filepath.Join("..", "..", "dist", "chart", "Chart.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	appVersion := findYAMLScalar(t, string(raw), "appVersion")
	if appVersion != Version {
		t.Errorf("chart appVersion = %q, want %q (version.Version); "+
			"bump appVersion in dist/chart/Chart.yaml", appVersion, Version)
	}

	// The chart's own version is allowed to move independently — a
	// documentation-only chart fix does not need a code release — but it must
	// be present and well formed, because the pipeline tags releases with it.
	chartVersion := findYAMLScalar(t, string(raw), "version")
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(chartVersion) {
		t.Errorf("chart version = %q, want a bare semver", chartVersion)
	}
}

// findYAMLScalar pulls a top-level scalar out of Chart.yaml without taking a
// YAML dependency for two fields.
func findYAMLScalar(t *testing.T, doc, key string) string {
	t.Helper()
	matches := regexp.MustCompile(`(?m)^` + key + `:\s*"?([^"\s]+)"?\s*$`).FindStringSubmatch(doc)
	if matches == nil {
		t.Fatalf("no top-level %q key in Chart.yaml", key)
	}
	return matches[1]
}

// TestPipelineCanParseVersion guards a coupling that is invisible from the Go
// side: the release workflow does not import this package, it greps this
// file's source text for the version.
//
//	grep -oE 'Version\s*=\s*"[0-9]+\.[0-9]+\.[0-9]+"' internal/version/version.go
//
// Moving Version into a var block would make gofmt align the "=", and a
// stricter form of that pattern would then match nothing — the pipeline would
// read an empty version and tag an image ":" with no version at all. This test
// applies the same expression the workflow does, so that failure shows up here
// instead of mid-release.
func TestPipelineCanParseVersion(t *testing.T) {
	raw, err := os.ReadFile("version.go")
	if err != nil {
		t.Fatalf("reading version.go: %v", err)
	}

	matches := regexp.MustCompile(`Version\s*=\s*"([0-9]+\.[0-9]+\.[0-9]+)"`).FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatal("the release workflow's regex finds no version in version.go; " +
			"it would tag an image with an empty version")
	}
	if len(matches) > 1 {
		t.Fatalf("the workflow's regex matches %d times in version.go; it takes the first, "+
			"so an unintended match could change what gets published", len(matches))
	}
	if got := matches[0][1]; got != Version {
		t.Errorf("workflow would parse %q, but Version is %q", got, Version)
	}
}
