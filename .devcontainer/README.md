# Devcontainer

A VS Code / Dev Containers setup that gives you the full toolchain for
`kube-state-graph` — including the ability to run the **testcontainers-go
integration suites** (ClickHouse + VictoriaMetrics) and the Istio route-engine
end-to-end tests, which need the `router_check_tool` binary.

Works on **macOS** (Docker Desktop) and **Linux** (Docker Engine).

## What you get

- Go pinned to **exactly** the `toolchain` version in `go.mod` (`go1.26.6`), so
  nothing is downloaded at run time — see [Go version pinning](#go-version-pinning).
- `router_check_tool` baked into the image at `/usr/local/bin/router_check_tool`
  (copied from `envoyproxy/envoy:tools-v1.34-latest`, the same source the
  production image uses), with `KSG_ROUTER_CHECK_BIN` pre-set — so
  `internal/integration`'s `TestRouteSuite` runs instead of self-skipping.
- golangci-lint (`v2.11.4`) + govulncheck, installed by `make init` (the repo's
  own bootstrap — single source of truth for versions). mockery/swag run via
  `go tool` (go.mod `tool` directives), no install needed.
- The Go and Docker VS Code extensions.

## Go version pinning

The image is built `FROM golang:<GO_VERSION>-bookworm`, where `GO_VERSION`
(in `devcontainer.json` → `build.args`) must equal the `toolchain` directive in
`go.mod`. **Bump both together.**

Why not the `mcr.microsoft.com/devcontainers/go` image: it only publishes minor
tags (`1.26-bookworm`) that float within the patch series, so it can land on a
different patch than `go.mod` pins — which makes Go download the pinned
toolchain on first use (`go: downloading go1.26.6`). Pinning the base to the
exact patch avoids that entirely.

`GOTOOLCHAIN` is left at `auto` purely as a safety net: if someone bumps
`go.mod` before rebuilding the image, Go fetches the correct toolchain instead
of failing. If you ever see a toolchain download, it means the image is stale —
rebuild the container.

## Host prerequisites

- **Docker must be running** on the host:
  - macOS: Docker Desktop.
  - Linux: Docker Engine (the daemon socket at `/var/run/docker.sock`).
- Give Docker **≥ 4 GB RAM** — the ClickHouse container in the route-store tests
  wants it.
- **Rootless Docker / non-default socket:** if your socket is not at
  `/var/run/docker.sock`, set `DOCKER_HOST` (e.g.
  `unix:///run/user/1000/docker.sock`) in your host environment before opening
  the container so the `docker-outside-of-docker` feature mounts the right one.

## How container-in-container tests work here

This uses **Docker-outside-of-Docker (DooD)**: the host Docker socket is mounted
into the devcontainer, and testcontainers-go starts its ClickHouse /
VictoriaMetrics containers as **siblings on the host daemon** (so they reuse the
host image cache — the envoy tools, ClickHouse, and VM images are pulled once).

Two settings make the tests reach those sibling containers on both OSes:

- `runArgs: --add-host=host.docker.internal:host-gateway` — makes
  `host.docker.internal` resolve on native Linux (Docker Desktop already
  provides it on Mac).
- `TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal` — the sibling containers
  publish their ports on the host, not on this container's `localhost`, so
  testcontainers is pointed at the host.

The repo's suites ingest over HTTP / the ClickHouse native protocol and never
bind-mount workspace files into the test containers, so the usual DooD
bind-mount caveat does not apply.

## Troubleshooting

### `mkdir /go/pkg/mod/cache: permission denied`

Your `ksg-gomod` / `ksg-gobuild` named volumes were created by an older version
of this image and are **root-owned**. Docker seeds a fresh named volume from the
image path's ownership, and it never re-seeds an existing volume — so the fix in
the image only applies to newly created volumes. Delete the stale ones and
rebuild:

```bash
# on the HOST, with the devcontainer stopped
docker volume rm ksg-gomod ksg-gobuild
```

Then *Dev Containers: Rebuild Container* in VS Code.

## Verify the setup

```bash
go version                                   # go1.26.6
router_check_tool --help                     # binary + glibc/libstdc++ resolve
docker info                                  # DooD socket works
make lint                                    # golangci-lint v2.11.4

# ClickHouse-only route store suite (no router_check_tool needed):
go test ./internal/integration/ -run TestRouteStoreSuite -v -count=1
# Full route e2e (needs router_check_tool, VM + ClickHouse):
go test ./internal/integration/ -run TestRouteSuite -v -count=1
# Everything, as CI runs it:
make test
```

## Fallback: Docker-in-Docker

If your host forbids mounting the Docker socket, switch to a nested daemon.
In `devcontainer.json`, replace the `docker-outside-of-docker` feature with:

```jsonc
"features": {
  "ghcr.io/devcontainers/features/docker-in-docker:2": {}
},
```

then **remove** `TESTCONTAINERS_HOST_OVERRIDE` from `containerEnv` and the
`--add-host` `runArg` (with DinD the containers publish on this container's own
`localhost`). Trade-offs: requires `--privileged`, re-pulls every image inside
the container, and does not share the host image cache. `router_check_tool` is
baked into the image, so it is unaffected either way.
