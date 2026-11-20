#!/usr/bin/env bash
# Build each release image for linux/amd64 and Trivy-scan it BEFORE anything is
# pushed (issue #233, fixes #1213). The scan binary is built the same way
# GoReleaser builds it (CGO_ENABLED=0 static), staged under the $TARGETPLATFORM
# layout the Dockerfiles COPY from, so the assembled image Trivy sees is
# representative of what GoReleaser will push. amd64 is representative: the base
# image and Go module set are identical across arches.
set -euo pipefail

VERSION="${VERSION:-scan}"
SCAN_ROOT="$(mktemp -d)"
trap 'rm -rf "$SCAN_ROOT"' EXIT

# id | image | dockerfile | binary | main
ALL=(
  "controller|ghcr.io/defilantech/llmkube-controller|Dockerfile.goreleaser|manager|./cmd/main.go"
  "router-proxy|ghcr.io/defilantech/llmkube-router-proxy|Dockerfile.router-proxy.goreleaser|router-proxy|./cmd/router-proxy"
)

want="${IMAGES:-controller router-proxy}"

# The release Dockerfiles COPY the license files that dockers_v2 stages into
# the build context (#2564); assemble the same context here.
if [ ! -d third_party_licenses ]; then
  echo "==> generating third_party_licenses/"
  make third-party-licenses
fi

# Findings do not abort the loop: a run reports every image's findings before
# failing, instead of hiding the rest behind the first.
failed=0

for row in "${ALL[@]}"; do
  IFS='|' read -r id image dockerfile binary main <<<"$row"
  case " $want " in *" $id "*) ;; *) continue ;; esac

  ctx="$SCAN_ROOT/$id"
  mkdir -p "$ctx/linux/amd64"
  cp LICENSE "$ctx/LICENSE"
  cp -R third_party_licenses "$ctx/third_party_licenses"
  echo "==> building $binary (linux/amd64) for scan"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -o "$ctx/linux/amd64/$binary" "$main"

  tag="${image}:${VERSION}-scan"
  echo "==> buildx --load $tag from $dockerfile"
  docker buildx build --platform linux/amd64 --load \
    -f "$dockerfile" -t "$tag" "$ctx"

  echo "==> trivy scan $tag"
  if ! trivy image \
    --severity CRITICAL,HIGH --ignore-unfixed --exit-code 1 \
    --format table "$tag"; then
    echo "==> FAIL $tag"
    failed=1
  fi
done

if (( failed )); then
  echo "one or more image scans failed" >&2
  exit 1
fi
echo "all image scans clean"
