# Continuous Integration & Release

How this repository is tested, built, signed and published. One workflow does all of it:
[`.github/workflows/release.yml`](.github/workflows/release.yml).

## Overview

```
PR into main      checks ─┐
                  test  ──┴─> build          (compiles, scans, publishes NOTHING)

push to main      checks ─┐
                  test  ──┴─> release        (publishes, signs, attests, tags)
```

Two triggers, four jobs. `build` and `release` are mutually exclusive — `build` runs only for
`pull_request`, `release` only for `push` — so the same commit is validated on the way in and
published on the way through, by different code paths that cannot be confused for each other.

Nothing is published from a pull request. A PR builds the multi-arch image and throws it away,
which is enough to prove it compiles and passes a vulnerability scan without putting an
unreviewed artifact anywhere a user could reach it.

## Anatomy of a pipeline

### 1. `checks` — secret + version-bump guard (PR **and** push)

Credential-free: it only reads files and talks to git, so it runs on every PR and catches a
forgotten version bump at review time rather than halfway through a release.

- **Validate required secrets** — fails early and by name if a secret is missing, instead of
  failing opaquely at the first AWS or Docker Hub call.
- **Detect what changed** — classifies the diff into `code` (`api/`, `cmd/`, `internal/`,
  `Dockerfile`, `go.mod`, `go.sum`) and `chart` (`dist/chart/`). The rest of the pipeline keys
  off these two booleans, which is what makes a docs-only commit cost nothing.
- **Read versions** — the operator version from `internal/version/version.go`, and the chart's
  `version:` and `appVersion:` from `dist/chart/Chart.yaml`.
- **Version-bump + tag existence check** — the gate that makes releases immutable:

  | change        | requires                                                           |
  | ------------- | ------------------------------------------------------------------ |
  | code          | `appVersion` == operator version, **and** neither `v<ver>` nor `helm-<chartVer>` already tagged |
  | chart only    | `helm-<chartVer>` not already tagged                               |
  | neither       | nothing                                                            |

  `appVersion` equalling the operator version is asserted twice — here, and by
  [`internal/version/version_test.go`](internal/version/version_test.go), so `make test` catches
  it locally before CI does.

### 2. `test` — the full matrix (PR **and** push)

Exactly what `make test` and `make test-e2e` run locally, in this order:

1. **Generated files are up to date** — `make manifests generate` then `git diff`. A stale CRD
   or stale deepcopy fails here.
2. **Helm chart matches the manifests** — `make helm-verify`, which runs
   [`hack/verify-chart.sh`](hack/verify-chart.sh). Compares the chart's `rules:` block against
   `config/rbac/role.yaml` rule by rule, so a verb-level difference fails and not just a missing
   resource. Also checks the CRD hasn't drifted and that the hand-maintained
   `admissionPolicy` values block and admission-policy templates still exist — those are dropped
   by `make helm-regenerate` and have silently disappeared before.
3. **Unit + envtest suites** — `make test`.
4. **Lint** — `make lint` (golangci-lint, `.golangci.yml`).
5. **Helm lint** — both modes, with and without admission policies.
6. **End-to-end tests on Kind** — a real cluster, a real chart install, a real operator.

### 3. `build` — build + existence check (PRs only)

Builds `linux/amd64,linux/arm64` with **no push**, loads the amd64 image locally, and runs Trivy,
uploading SARIF to code scanning. It also re-runs the artifact existence check, so a PR that
would collide with an already-published version fails before anyone merges it.

### 4. `release` — publish + sign + tag (merge to `main` only)

Needs `checks` and `test`. Every image step is gated on `code == true`; the chart steps on
`code == true || chart == true`. A chart-only change therefore republishes the chart against the
**existing** image rather than rebuilding one.

The ordering is the point:

1. **Build + push by digest, untagged.** The image lands in the registry addressable only by
   `sha256:…`.
2. **Trivy scan the pushed digest** — the exact bytes that were pushed, not a local rebuild.
3. **Sign** — keyless cosign, via the workflow's OIDC identity. No long-lived signing key.
4. **SBOM** — SPDX, attached with `cosign attest`.
5. **SLSA provenance attestation.**
6. **Only now, tag it** `:<version>` and `:latest`.

So a tag never points at an image that hasn't been scanned, signed and attested. If any step
between push and tag fails, the digest is orphaned in the registry and no tag advertises it.

Then: package and publish the chart, invalidate the CDN index, tag the repo, and cut the GitHub
Releases.

## Versioning & tags

| Source of truth                    | Governs                          |
| ---------------------------------- | -------------------------------- |
| `internal/version/version.go`      | the operator version, image tags |
| `dist/chart/Chart.yaml` `version:` | the chart version                |
| `dist/chart/Chart.yaml` `appVersion:` | must equal the operator version |

Two independent tag series, because the chart and the operator change at different rates:

- **`v<version>`** — an operator release. Cut only when code changed.
- **`helm-<chartVersion>`** — a chart release. Cut when the chart changed, which includes every
  code release (since `appVersion` has to move with it).

Both are published artifacts and both are immutable. Re-releasing a version means bumping it.

## Supply-chain integrity

- **Keyless cosign signatures** — identity is the workflow itself; there is no key to leak.
- **SPDX SBOM**, attached as an attestation rather than a loose file.
- **SLSA provenance**, so the build is traceable to this repository and commit.
- **Trivy** on every PR and on the pushed digest, with results in GitHub code scanning. Run via
  [`.github/actions/trivy-scan`](.github/actions/trivy-scan/action.yml), which scans **twice**: once
  to report (`exit-code: 0`, SARIF uploaded unconditionally) and once to gate (`exit-code: 1`,
  table output). Reporting and gating are separate calls because a single call that both writes
  SARIF and fails on a finding can abort before the report is written — losing the alert exactly
  when there is something to alert about. Unfixable base-image CVEs are reported but do not block.
- **Actions are pinned by tag, not by digest** — `actions/checkout@v4`,
  `docker/build-push-action@v6` and so on. Tags are mutable, so this trusts the action
  publishers rather than verifying them. Pinning to commit SHAs would close that gap and is the
  one supply-chain control this pipeline does not yet apply to itself.
- **No `s3:DeleteObject`.** The publishing role cannot delete a published artifact even by
  mistake; immutability is enforced by the absence of the permission, not by discipline.

## Distribution & hosting

- **Images** → Docker Hub, `nabeemdev/namespace-bootstrap-operator`, tagged `<version>` and
  `latest`. Docker Hub is reached with a token, not AWS — the OIDC role is S3/CloudFront only.
- **Charts** → the `nabeem-releases` bucket under `charts/namespace-bootstrap-operator/`, served
  over a CDN at **`https://release.nabeem.com/charts/namespace-bootstrap-operator`**.

  Publishing fetches the existing `index.yaml`, merges the new chart into it with
  `helm repo index --merge`, and writes both back. That merge is why the role needs
  `s3:GetObject` as well as `s3:PutObject`: an index written without it would erase every
  previously published version from the repo listing.

  `index.yaml` is the one mutable object, so it is invalidated at the edge on publish. Versioned
  `.tgz` files are immutable and stay cached.

## Prerequisites (one-time setup)

**Repo secrets:**

| Secret                                  | Used for                                              |
| --------------------------------------- | ----------------------------------------------------- |
| `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN` | pushing images to Docker Hub                          |
| `AWS_ROLE_ARN`                          | the IAM role assumed via GitHub OIDC (no static keys) |

**Repo variable:**

| Variable                      | Used for                                                                          |
| ----------------------------- | --------------------------------------------------------------------------------- |
| `RELEASES_CF_DISTRIBUTION_ID` | the CloudFront distribution ID, so the invalidation step fires (no-op until set)   |

**AWS — expected to already exist.** The pipeline consumes this infrastructure; it does not
provision it, and it is not defined in this repository.

| Expected                                                                 | Why                                              |
| ------------------------------------------------------------------------ | ------------------------------------------------ |
| the `nabeem-releases` bucket, CDN-fronted at `release.nabeem.com`        | where published charts are written and read      |
| a GitHub OIDC provider for `token.actions.githubusercontent.com`         | lets Actions assume a role without static keys   |
| an IAM role trusting `repo:<owner>/namespace-bootstrap-operator:ref:refs/heads/main` | its ARN is the `AWS_ROLE_ARN` secret above |

The role needs `s3:ListBucket`, `s3:GetObject` and `s3:PutObject` on the bucket, plus
`cloudfront:CreateInvalidation` on the distribution — and deliberately **not**
`s3:DeleteObject`.

Without `RELEASES_CF_DISTRIBUTION_ID` the invalidation step is skipped and a new chart version
stays invisible to `helm repo update` until the index's edge TTL expires.

## Cutting a release

Do the bump in a PR; merging to `main` releases it.

- **Operator release:** bump `Version` in [`internal/version/version.go`](internal/version/version.go),
  **and** both `version:` and `appVersion:` in `dist/chart/Chart.yaml` — `appVersion` must equal
  the new operator version.
- **Chart-only release:** bump just `version:` in `dist/chart/Chart.yaml`. The existing image is
  reused.

Run `make test lint helm-verify` first; it is the same gate CI applies.

## Troubleshooting

| Symptom                                                   | Cause                                                                        |
| --------------------------------------------------------- | ---------------------------------------------------------------------------- |
| `chart appVersion (x) must equal the operator version (y)` | bumped one but not the other                                                |
| `tag vX already exists`                                    | code changed without a version bump                                          |
| `tag helm-X already exists`                                | chart changed without a chart-version bump                                   |
| `dist/chart RBAC has drifted from config/rbac/role.yaml`   | RBAC markers changed; copy the regenerated `rules:` block into the chart     |
| `Generated files are stale`                                | run `make manifests generate` and commit                                     |
| new chart version not visible to `helm repo update`        | `RELEASES_CF_DISTRIBUTION_ID` unset, so the index is still edge-cached       |
| image pushed but no tag                                    | a signing or attestation step failed; the digest is orphaned, nothing points at it |

## File reference

| Path                                      | What it is                                              |
| ----------------------------------------- | ------------------------------------------------------- |
| `.github/workflows/release.yml`           | the whole pipeline                                      |
| `hack/verify-chart.sh`                    | chart-drift checks, shared by `make helm-verify` and CI |
| `.github/actions/trivy-scan/`             | the shared Trivy scan: report + gate                    |
| `internal/version/version.go`             | the operator version, single source of truth            |
| `internal/version/version_test.go`        | asserts the chart's `appVersion` matches it, and that the workflow's regex can still parse it |
| `Dockerfile`                              | cross-compiles via `--platform=$BUILDPLATFORM`; stamps version/commit/date via `-ldflags` |
| `dist/chart/Chart.yaml`                   | chart version and `appVersion`                          |
| `Makefile`                                | every target CI invokes                                 |
