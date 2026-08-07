# syntax=docker/dockerfile:1

# Pinned to the EXACT toolchain go.mod declares (`toolchain go1.26.5`), so the
# container's Go IS the module's Go and nothing is downloaded at run time.
# Bump this together with the go.mod toolchain directive.
#
# Note: the mcr.microsoft.com/devcontainers/go images only publish minor tags
# (1.26-bookworm), which float within the 1.26.x patch series and therefore
# cannot guarantee the pinned patch — hence the official image.
ARG GO_VERSION=1.26.5

# Source the router_check_tool binary from the Envoy tools image — the SAME
# image and path the production build uses (deploy/docker/server.Dockerfile).
# It is multi-arch, so an arm64 Mac gets the arm64 binary and an amd64 host the
# amd64 one (COPY --from resolves at the devcontainer image's build platform).
FROM envoyproxy/envoy:tools-v1.34-latest AS envoy-tools

FROM golang:${GO_VERSION}-bookworm

ARG USERNAME=vscode
ARG USER_UID=1000
ARG USER_GID=$USER_UID

# router_check_tool is a dynamically-linked C++ binary (the production runtime is
# distroless/cc for exactly this reason): it needs glibc + libstdc++. The
# buildpack-deps-based golang image already carries those and a C toolchain for
# `go test -race` (cgo); they are installed explicitly so the dependency is
# documented and survives a future base-image change. sudo is for the non-root
# user below.
RUN apt-get update \
    && export DEBIAN_FRONTEND=noninteractive \
    && apt-get install -y --no-install-recommends \
        build-essential \
        libstdc++6 \
        ca-certificates \
        sudo \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Non-root dev user (the official golang image has none; the devcontainers
# images would otherwise provide it). Passwordless sudo for ad-hoc installs.
RUN groupadd --gid $USER_GID $USERNAME \
    && useradd --uid $USER_UID --gid $USER_GID --create-home --shell /bin/bash $USERNAME \
    && echo "$USERNAME ALL=(root) NOPASSWD:ALL" > /etc/sudoers.d/$USERNAME \
    && chmod 0440 /etc/sudoers.d/$USERNAME

# The official image puts Go on PATH via ENV, but /etc/profile RESETS PATH for
# login shells — and the VS Code integrated terminal is a login shell, so `go`
# and the golangci-lint that `make init-tools` installs into /go/bin would both
# be "command not found". Re-export them for login shells.
RUN printf '%s\n' 'export PATH=/go/bin:/usr/local/go/bin:$PATH' \
        > /etc/profile.d/10-go-path.sh \
    && chmod 0644 /etc/profile.d/10-go-path.sh

COPY --from=envoy-tools /usr/local/bin/router_check_tool /usr/local/bin/router_check_tool

# Build-time assertion: fail loudly here rather than mid-test if the binary
# copied from the Ubuntu-based Envoy image cannot resolve a shared library on
# this Debian base.
RUN ! ldd /usr/local/bin/router_check_tool | grep -q "not found"

# Create the Go cache directories UP FRONT and owned by the dev user.
# devcontainer.json mounts named volumes over both: Docker seeds a fresh named
# volume from the image path's content *and ownership*, so if these directories
# do not already exist the volume is created root-owned and the user cannot
# write to it ("mkdir /go/pkg/mod/cache: permission denied"). GOPATH=/go in the
# official image and is root-owned, so it must be handed over too.
RUN mkdir -p /go/pkg/mod /go/bin /home/$USERNAME/.cache/go-build \
    && chown -R $USER_UID:$USER_GID /go /home/$USERNAME

USER $USERNAME
