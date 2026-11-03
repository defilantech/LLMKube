# Container image for the e2e stub upstream. Only built during the
# ModelRouter cluster e2e job; not shipped to users. Mirrors the
# distroless-static + non-root layout of the router-proxy image so the
# proxy and the stub run under the same PodSecurityStandard restricted
# constraints that we enforce in the test namespace.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# The script retries a network error from the module proxy (#2380).
COPY go.mod go.sum ./
COPY hack/go-mod-download.sh hack/go-mod-download.sh
RUN sh hack/go-mod-download.sh

COPY test/e2e/stubupstream/ test/e2e/stubupstream/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o stub-upstream ./test/e2e/stubupstream

FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=builder /workspace/stub-upstream /usr/local/bin/stub-upstream

EXPOSE 8080

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/stub-upstream"]
