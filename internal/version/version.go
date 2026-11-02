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

// Package version is the single source of truth for the operator's released
// version.
//
// The release pipeline reads Version out of this file's source text to decide
// what to tag the container image and the git tag, and refuses to publish a
// version that already exists — so shipping a change means bumping this. The
// chart's appVersion must match it, which version_test.go enforces, so a
// forgotten bump fails the build rather than silently republishing.
package version

import (
	"fmt"
	"runtime"
)

// Version is the operator's semantic version.
//
// Bump this in the same commit as the change it ships, and bump the chart's
// version and appVersion in dist/chart/Chart.yaml to match.
//
// Declared on its own rather than inside a var block on purpose. The release
// pipeline parses this line out of the source, and gofmt aligns the "=" in a
// block — which would silently stop the pipeline from finding the version.
// TestPipelineCanParseVersion guards that coupling.
var Version = "0.1.0"

// Commit and BuildDate are stamped in at link time by the release build:
//
//	-ldflags "-X .../internal/version.Commit=$(git rev-parse HEAD)"
//
// They stay at their defaults for a plain `go build`, which is how a
// development binary identifies itself as one.
var (
	Commit    = "unknown"
	BuildDate = "unknown"
)

// String renders the full build identity, for a startup log line and the
// build_info metric.
func String() string {
	return fmt.Sprintf("v%s (commit %s, built %s, %s)", Version, Commit, BuildDate, runtime.Version())
}

// GoVersion reports the toolchain the binary was built with.
func GoVersion() string { return runtime.Version() }
