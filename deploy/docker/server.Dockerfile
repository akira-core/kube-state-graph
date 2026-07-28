# syntax=docker/dockerfile:1.7

# Envoy tools image supplying router_check_tool. SINGLE SOURCE of that
# reference: `make router-check-tool` greps this line so CI installs the exact
# matcher the image ships (see the Makefile). Pinned by DIGEST, not by the
# floating `tools-v1.34-latest` tag: matchcheck's correctness argument is that
# the matcher is Envoy's own, and parseActuals scrapes the tool's `--details`
# text output — a silent image roll could change either with no code change and
# no CI signal. The digest is the multi-arch index, so multi-platform builds
# still resolve per-platform. Resolved from tools-v1.34-latest (image created
# 2026-04-10); re-pin with
# `docker buildx imagetools inspect envoyproxy/envoy:tools-v1.34-latest`.
ARG ENVOY_TOOLS_IMAGE=envoyproxy/envoy:tools-v1.34-latest@sha256:74a4aebd5cc9ca03889189f96f0164015bcd1e087953528ad557cb389816d0d6

# ---- build stage ---------------------------------------------------------
# Base image Go must match go.mod's `toolchain` directive (go1.26.4); a lower
# base would trigger a silent mid-build toolchain download from dl.google.com
# (GOTOOLCHAIN is not pinned to local), breaking reproducibility.
FROM --platform=$BUILDPLATFORM golang:1.26.4-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

# Cache go mod separately for layer reuse.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/kube-state-graph ./cmd/kube-state-graph

# ---- router_check_tool stage ---------------------------------------------
# Envoy's router_check_tool ships prebuilt in the official tools image; route
# resolution (--route-store-dsn) execs it as a native binary — no docker, no
# Envoy process at runtime. The pin (declared at the top of this file) fixes the
# Envoy version the linked istio.io/istio release generates RouteConfigurations
# for.
FROM ${ENVOY_TOOLS_IMAGE} AS envoy-tools

# ---- runtime stage -------------------------------------------------------
# distroless/cc (not /static): router_check_tool is a dynamically linked C++
# binary needing glibc + libstdc++. The Go server itself stays CGO_ENABLED=0.
FROM gcr.io/distroless/cc:nonroot

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="kube-state-graph" \
      org.opencontainers.image.description="Multi-cluster pod / node / PVC graph API for Kubernetes." \
      org.opencontainers.image.source="https://github.com/akira-core/kube-state-graph" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=build /out/kube-state-graph /usr/local/bin/kube-state-graph
COPY --from=envoy-tools /usr/local/bin/router_check_tool /usr/local/bin/router_check_tool
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kube-state-graph"]
