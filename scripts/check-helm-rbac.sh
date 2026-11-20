#!/usr/bin/env bash
# check-helm-rbac.sh — guard against a Helm chart's RBAC drifting behind the
# kubebuilder markers of the operator that chart ships (#379, the RBAC analogue
# of the #367 CRD sync guard).
#
# The operator's ClusterRole is hand-maintained in its chart:
#
#   internal/controller/**  -> charts/llmkube  (inference operator)
#
# A marker that lands in the source but not in that operator's chart silently
# breaks the in-cluster binary: controller-runtime's cache cannot list the type,
# the reflector retries forever, and the feature is dead with only a "Failed to
# watch" line to show for it (#376 Endpoints, #1593 batch/jobs).
#
# WHY THIS READS MARKERS FROM SOURCE RATHER THAN config/rbac/role.yaml
#
# role.yaml is generated with paths="./...", so it is the UNION of every
# marker in the module with no record of which operator needs which rule. This
# repo used to ship a second operator and chart, and an earlier version of this
# check compared role.yaml against the union of BOTH charts, which made the
# two operators cover for each other: the other chart granted batch/jobs and
# pods/log to its agent ServiceAccount, and that satisfied the inference
# operator's identically-shaped markers. The check reported "covers all 264
# rules" while the llmkube controller-manager had neither grant and #1593 was
# live in a release.
#
# Attributing each marker to the tree it came from is what closes that hole.
# Extra chart rules are fine: each chart must be a SUPERSET of its operator's
# markers, not an exact match.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

command -v helm >/dev/null 2>&1 || { echo "❌ helm is required"; exit 2; }
python3 -c 'import yaml' >/dev/null 2>&1 || { echo "❌ python3 with PyYAML is required (pip install pyyaml)"; exit 2; }

render() {
  helm template "$1" --set rbac.create=true --set crds.install=false 2>/dev/null || {
    echo "❌ helm template failed for $1" >&2
    exit 2
  }
}

# Render each chart to its own temp file. A temp FILE, not an env var: the full
# render can exceed the OS env+argv limit (E2BIG) once a chart emits base64
# cert blobs.
LLMKUBE_YAML="$(mktemp)"
trap 'rm -f "$LLMKUBE_YAML"' EXIT
render "$REPO_ROOT/charts/llmkube" > "$LLMKUBE_YAML"

REPO_ROOT="$REPO_ROOT" LLMKUBE_YAML="$LLMKUBE_YAML" python3 - <<'PY'
import os, re, sys, yaml
from pathlib import Path

REPO = Path(os.environ["REPO_ROOT"])

OPERATORS = [
    {
        "label": "inference operator",
        "src": "internal/controller",
        "chart": "charts/llmkube",
        "yaml": os.environ["LLMKUBE_YAML"],
        "fix": "charts/llmkube/templates/clusterrole.yaml",
    },
]

MARKER = re.compile(r'^\s*//\s*\+kubebuilder:rbac:(.+?)\s*$')


def marker_triples(src_dir):
    """(group, resource, verb) required by the operator rooted at src_dir.

    Only comment-form markers in non-test files. _test.go is excluded because a
    test may carry real marker syntax as fixture data, and those are not grants
    anyone needs.
    """
    out = set()
    for path in sorted((REPO / src_dir).rglob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        for line in path.read_text().splitlines():
            m = MARKER.match(line)
            if not m:
                continue
            fields = {}
            for part in m.group(1).split(","):
                if "=" not in part:
                    continue
                k, v = part.split("=", 1)
                fields[k.strip()] = v.strip()
            # controller-gen normalises the core group: both groups=core and
            # groups="" render as apiGroups: [""].
            groups = [("" if g.strip('"') == "core" else g.strip('"'))
                      for g in fields.get("groups", "").split(";")]
            for g in groups:
                for r in fields.get("resources", "").split(";"):
                    for v in fields.get("verbs", "").split(";"):
                        if r and v:
                            out.add((g, r, v))
    return out


def chart_triples(path):
    """(group, resource, verb) the chart's ClusterRoles grant.

    ClusterRole only, deliberately. A namespaced Role is a grant in one
    namespace, so it cannot cover a marker for a cluster-wide watch: a
    ControllerManager whose cache cannot list a type cluster-wide is the
    failure this check exists to catch (#376, #1593). The llmkube chart's
    leader-election Role grants configmaps, events and coordination leases in
    the release namespace. Accepting it here let a lease marker read as covered
    while the operator's ClusterRole had none, so every pooled ModelRouter got
    a 403 on its Lease (found on the #1477 branch, never on main).
    """
    out = set()
    for doc in yaml.safe_load_all(Path(path).read_text()):
        if not doc or doc.get("kind") != "ClusterRole":
            continue
        for rule in (doc.get("rules") or []):
            for g in (rule.get("apiGroups") or []):
                for r in (rule.get("resources") or []):
                    for v in (rule.get("verbs") or []):
                        out.add((g, r, v))
    return out


def covered(triple, granted):
    g, r, v = triple
    if triple in granted:
        return True
    # Honour wildcards on the chart side so a legitimate broad grant does not
    # read as a gap. None exist today; this keeps the check from false-failing
    # if one is ever added.
    for cg, cr, cv in granted:
        if cg in (g, "*") and cr in (r, "*") and cv in (v, "*"):
            return True
    return False


failed = False
total_required = 0
for op in OPERATORS:
    required = marker_triples(op["src"])
    granted = chart_triples(op["yaml"])
    total_required += len(required)
    missing = sorted(t for t in required if not covered(t, granted))
    if missing:
        failed = True
        print("❌ %s: %s/ has markers that %s does not grant:"
              % (op["label"], op["src"], op["chart"]))
        for g, r, v in missing:
            print("     apiGroup=%-24s resource=%-28s verb=%s"
                  % (repr(g), r, v))
        print("   Add them to %s" % op["fix"])
        print()

if failed:
    print("Each chart must be a superset of ITS OWN operator's markers.")
    sys.exit(1)

print("✅ Each chart covers its own operator's markers (%d rules across %d operators)."
      % (total_required, len(OPERATORS)))
PY
