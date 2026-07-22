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
  "foreman-operator|ghcr.io/defilantech/llmkube-foreman-operator|Dockerfile.foreman-operator.goreleaser|foreman-operator|./cmd/foreman-operator"
  "foreman-agent|ghcr.io/defilantech/llmkube-foreman-agent|Dockerfile.foreman-agent.goreleaser|foreman-agent|./cmd/foreman-agent"
  "router-proxy|ghcr.io/defilantech/llmkube-router-proxy|Dockerfile.router-proxy.goreleaser|router-proxy|./cmd/router-proxy"
)

want="${IMAGES:-controller foreman-operator foreman-agent router-proxy}"

for row in "${ALL[@]}"; do
  IFS='|' read -r id image dockerfile binary main <<<"$row"
  case " $want " in *" $id "*) ;; *) continue ;; esac

  ctx="$SCAN_ROOT/$id"
  mkdir -p "$ctx/linux/amd64"
  echo "==> building $binary (linux/amd64) for scan"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -o "$ctx/linux/amd64/$binary" "$main"

  tag="${image}:${VERSION}-scan"
  echo "==> buildx --load $tag from $dockerfile"
  docker buildx build --platform linux/amd64 --load \
    -f "$dockerfile" -t "$tag" "$ctx"

  echo "==> trivy scan $tag"
  trivy image \
    --severity CRITICAL,HIGH --ignore-unfixed --exit-code 1 \
    --format table "$tag"
done
echo "all image scans clean"
