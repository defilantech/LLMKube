#!/usr/bin/env bash
# sync-crds.sh — Generate CRDs from kubebuilder markers and wrap them for Helm.
# Usage: make chart-crds  (which runs this script after make manifests)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../" && pwd)"

CRD_SOURCE_DIR="$REPO_ROOT/config/crd/bases"
CRD_TARGET_DIR="$REPO_ROOT/charts/llmkube/templates/crds"

# Validate source CRDs exist
if ! compgen -G "$CRD_SOURCE_DIR/*.yaml" > /dev/null; then
  echo "❌ No CRD files found in $CRD_SOURCE_DIR"
  exit 1
fi

# Ensure target directory exists
mkdir -p "$CRD_TARGET_DIR"

synced=0
for src in "$CRD_SOURCE_DIR"/*.yaml; do
  # Strip kubebuilder group prefix: inference.llmkube.dev_inferenceservices.yaml → inferenceservices.yaml
  base="$(basename "$src")"
  short="${base%.*}"        # strip .yaml extension
  short="${short##*_}"      # strip group.version_ prefix
  short="${short}.yaml"     # restore extension
  dst="$CRD_TARGET_DIR/$short"

  echo "Syncing $base → $short"

  # Wrap with Helm conditionals and inject resource-policy keep block
  {
    echo '{{- if .Values.crds.install }}'
    awk '/controller-gen.kubebuilder.io\/version:/{print; print "    {{- if .Values.crds.keep }}"; print "    helm.sh/resource-policy: keep"; print "    {{- end }}"; next}1' "$src"
    echo '{{- end }}'
  } > "$dst"

  synced=$((synced + 1))
done

echo "✅ Synced $synced CRD(s)"
