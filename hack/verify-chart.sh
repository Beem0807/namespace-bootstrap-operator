#!/usr/bin/env bash
# Checks that dist/chart has not drifted from config/.
#
# Deliberately assertion-based rather than regenerate-and-diff: the chart carries
# hand-maintained content (templates/admission-policy and its values block) that
# `kubebuilder edit` drops, so a diff against freshly generated output could never
# pass. These are the invariants that actually matter.
#
# Called by `make helm-verify` and by the release workflow, so there is one
# implementation rather than two that can disagree about how strict they are.
set -euo pipefail

cd "$(dirname "$0")/.."

CHART_ROLE=dist/chart/templates/rbac/manager-role.yaml
REAL_ROLE=config/rbac/role.yaml
CHART_CRD=dist/chart/templates/crd/namespaceprofiles.platform.nabeem.com.yaml
REAL_CRD=config/crd/bases/platform.nabeem.com_namespaceprofiles.yaml

fail() { echo "Error: $1" >&2; echo "$2" >&2; exit 1; }

# The chart's ClusterRole differs from the generated one only above `rules:`
# (its kind and name are templated). Everything from `rules:` down is plain
# YAML, so it must match the generated role exactly.
#
# Compared rule by rule rather than just checking that resource names appear:
# a verb-level difference is exactly the drift that a name-presence check misses,
# and an over-granted verb is a privilege the operator should not have.
python3 - "$CHART_ROLE" "$REAL_ROLE" <<'PY' || exit 1
import sys

def rules(path):
    with open(path) as fh:
        lines = fh.read().splitlines()
    for i, line in enumerate(lines):
        if line.startswith("rules:"):
            return [l.rstrip() for l in lines[i:] if l.strip()]
    sys.exit(f"Error: {path} has no 'rules:' block.")

chart_path, real_path = sys.argv[1], sys.argv[2]
chart, real = rules(chart_path), rules(real_path)
if chart == real:
    sys.exit(0)

print(f"Error: {chart_path} RBAC has drifted from {real_path}.", file=sys.stderr)
import difflib
diff = difflib.unified_diff(real, chart, fromfile=real_path, tofile=chart_path, lineterm="")
for line in list(diff)[:40]:
    print("  " + line, file=sys.stderr)
print("", file=sys.stderr)
print("Copy the 'rules:' block from config/rbac/role.yaml into the chart template,", file=sys.stderr)
print("keeping the templated header above it, and commit the result.", file=sys.stderr)
sys.exit(1)
PY

# The chart's CRD is the generated one wrapped in a Helm conditional, so it should
# track the real schema closely. A large divergence means it is stale.
chart_crd_lines=$(grep -c '' "$CHART_CRD")
real_crd_lines=$(grep -c '' "$REAL_CRD")
delta=$(( chart_crd_lines > real_crd_lines ? chart_crd_lines - real_crd_lines : real_crd_lines - chart_crd_lines ))
if [ "$delta" -gt 20 ]; then
    fail "$CHART_CRD ($chart_crd_lines lines) has drifted from $REAL_CRD ($real_crd_lines lines)." \
         "Run 'make helm-regenerate', restore the hand-maintained values, and commit."
fi

# The admission-policy templates survive `kubebuilder edit` but values.yaml does
# not, so the flag can silently disappear on a chart regeneration.
grep -q '^admissionPolicy:' dist/chart/values.yaml || \
    fail "dist/chart/values.yaml has lost the hand-maintained 'admissionPolicy' block." \
         "It is dropped by 'make helm-regenerate'. Restore it (see git history) and commit."

# Also hand-maintained: the version label on the Deployment, which is how a cluster
# reports which build is installed without anyone reading the image tag.
grep -q 'app.kubernetes.io/version' dist/chart/templates/manager/manager.yaml || \
    fail "the manager Deployment has lost its app.kubernetes.io/version label." \
         "It is hand-added and dropped by 'make helm-regenerate'. Restore it and commit."

for t in managed-resources-protected namespace-labels-protected; do
    test -f "dist/chart/templates/admission-policy/$t.yaml" || \
        fail "dist/chart/templates/admission-policy/$t.yaml is missing." \
             "It is hand-maintained; restore it from git history."
done

echo "Helm chart is in sync with config/, and its hand-maintained parts are intact."
