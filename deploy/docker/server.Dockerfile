# syntax=docker/dockerfile:1.7

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

# ---- runtime stage -------------------------------------------------------
FROM gcr.io/distroless/static:nonroot

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
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kube-state-graph"]
