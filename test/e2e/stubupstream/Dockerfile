# Container image for the e2e stub upstream. Only built during the
# ModelRouter cluster e2e job; not shipped to users. Mirrors the
# distroless-static + non-root layout of the router-proxy image so the
# proxy and the stub run under the same PodSecurityStandard restricted
# constraints that we enforce in the test namespace.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY test/e2e/stubupstream/ test/e2e/stubupstream/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o stub-upstream ./test/e2e/stubupstream

FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=builder /workspace/stub-upstream /usr/local/bin/stub-upstream

EXPOSE 8080

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/stub-upstream"]
