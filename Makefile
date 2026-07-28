.PHONY: build test vet lint vuln ci cover docs check-docs check-route-containment clean \
        docker-build docker-push docker-buildx docker-run docker-docs docker-docs-stop \
        init init-go init-tools init-hooks doctor mocks verify-mocks tools-versions \
        router-check-tool

BIN_DIR := bin
BIN     := $(BIN_DIR)/kube-state-graph
PKG     := github.com/akira-core/kube-state-graph
# Recursive (=) so $(VERSION), defined below, resolves at use time. Injects the
# build version into main.version (→ OTel service.version) like the container.
LDFLAGS = -s -w -X main.version=$(VERSION)

# Container image settings. Override on the command line, e.g.:
#   make docker-push REGISTRY=docker.io IMAGE_REPO=akira-core/kube-state-graph VERSION=v0.2.0
REGISTRY      ?= docker.io
IMAGE_REPO    ?= akira-core/kube-state-graph
IMAGE         ?= $(REGISTRY)/$(IMAGE_REPO)
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT        ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
DOCKERFILE    := deploy/docker/server.Dockerfile
# Read the digest-pinned Envoy tools image straight out of the Dockerfile's ARG
# so the matcher CI installs is byte-identical to the one the server image
# ships. One declaration, no drift check needed.
ENVOY_TOOLS_IMAGE := $(shell sed -n 's/^ARG ENVOY_TOOLS_IMAGE=//p' $(DOCKERFILE))
ROUTER_CHECK_BIN  := $(BIN_DIR)/router_check_tool
PLATFORMS     ?= linux/amd64,linux/arm64
LOCAL_TAG     := localhost/kube-state-graph/server:dev
DOCKER_BUILD_ARGS := \
    --build-arg VERSION=$(VERSION) \
    --build-arg COMMIT=$(COMMIT) \
    --build-arg BUILD_DATE=$(BUILD_DATE)

## ---------------------------------------------------------------------------
## Local-dev bootstrap.
##
##   make init          # one-shot: go mod download + dev tools + (optional) hooks
##   make init-go       # only: go mod download / tidy verify
##   make init-tools    # only: install host-level dev binaries (golangci-lint, govulncheck)
##   make init-hooks    # optional: enable .githooks/ (pre-commit gofmt+lint+quick-test, pre-push make ci)
##   make doctor        # report toolchain versions & missing pieces
##
## Go-based tools tracked via go.mod `tool` directive (Go 1.24+) are invoked
## with `go tool <name>` and need no install step. They are excluded from the
## production binary by the Go toolchain.
##
## Host-level tools (golangci-lint, govulncheck) live in $(GOBIN). They are
## NOT pulled into go.mod and NOT required by CI consumers of the module.
GOBIN ?= $(shell go env GOPATH)/bin
GOLANGCI_LINT_VERSION ?= v2.11.4
GOVULNCHECK_VERSION   ?= latest

init: init-go init-tools
	@echo ""
	@echo "Local dev environment ready."
	@echo "  make doctor         # verify toolchain"
	@echo "  make init-hooks     # (optional) enable .githooks (pre-commit + pre-push CI)"

init-go:
	@echo ">> go mod download"
	go mod download
	@echo ">> verifying go.mod is tidy (read-only check)"
	@cp go.mod go.mod.bak && cp go.sum go.sum.bak; \
	    go mod tidy >/dev/null 2>&1; \
	    if ! diff -q go.mod go.mod.bak >/dev/null || ! diff -q go.sum go.sum.bak >/dev/null; then \
	        mv go.mod.bak go.mod && mv go.sum.bak go.sum; \
	        echo "WARN: go.mod / go.sum not tidy. Run 'go mod tidy' and commit."; \
	    else \
	        rm -f go.mod.bak go.sum.bak; \
	        echo "  ok"; \
	    fi

## Install host-level dev binaries into $(GOBIN). Skipped if already present
## at the pinned version. Safe to re-run.
init-tools:
	@echo ">> ensure $(GOBIN) is on PATH"
	@case ":$$PATH:" in *":$(GOBIN):"*) ;; *) echo "  WARN: $(GOBIN) not on PATH. Add: export PATH=\"$(GOBIN):\$$PATH\"";; esac
	@echo ">> golangci-lint $(GOLANGCI_LINT_VERSION)"
	@if ! command -v golangci-lint >/dev/null 2>&1 || ! golangci-lint --version 2>/dev/null | grep -q "$$(echo $(GOLANGCI_LINT_VERSION) | sed 's/^v//')"; then \
	    curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
	        | sh -s -- -b $(GOBIN) $(GOLANGCI_LINT_VERSION); \
	else \
	    echo "  already installed: $$(golangci-lint --version)"; \
	fi
	@echo ">> govulncheck $(GOVULNCHECK_VERSION)"
	@if ! command -v govulncheck >/dev/null 2>&1; then \
	    GOBIN=$(GOBIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION); \
	else \
	    echo "  already installed at $$(command -v govulncheck)"; \
	fi
	@echo ">> mockery (via go tool — no install needed)"
	@go tool mockery --version 2>/dev/null | tail -1 | awk '{print "  " $$0}' || echo "  WARN: 'go tool mockery' failed; check go.mod tool directive."

## Enable the version-controlled git hooks under .githooks/ by pointing
## core.hooksPath at that directory. pre-commit runs gofmt (staged Go files) +
## golangci-lint + a quick unit-test pass (no race/shuffle, no integration);
## pre-push runs the full CI mirror (`make ci`). The hook scripts live
## in git (reviewable, team-consistent) — this target only flips the per-repo
## core.hooksPath setting (which is not itself version-controlled). Idempotent.
## Bypass any hook ad hoc with `git commit/push --no-verify`.
init-hooks:
	@if [ ! -d .git ]; then echo "not a git repo; skipping"; exit 0; fi
	@chmod +x .githooks/pre-commit .githooks/pre-push
	@git config core.hooksPath .githooks
	@echo "configured core.hooksPath -> .githooks"
	@echo "  pre-commit: gofmt (staged) + golangci-lint + quick unit tests"
	@echo "  pre-push  : make ci (lint + vuln + test + docs + mocks)"

doctor:
	@echo "go         : $$(go version 2>/dev/null || echo MISSING)"
	@echo "GOBIN      : $(GOBIN)"
	@echo "golangci   : $$(golangci-lint --version 2>/dev/null | head -1 || echo MISSING)"
	@echo "govulncheck: $$(command -v govulncheck >/dev/null && echo present || echo MISSING)"
	@echo "swag (tool): $$(go tool swag -v 2>/dev/null | head -1 || echo MISSING)"
	@echo "mockery    : $$(go tool mockery --version 2>/dev/null | tail -1 || echo MISSING)"
	@echo "docker     : $$(docker --version 2>/dev/null || echo MISSING)"

tools-versions:
	@echo "Pinned dev tools (override via env):"
	@echo "  GOLANGCI_LINT_VERSION = $(GOLANGCI_LINT_VERSION)"
	@echo "  GOVULNCHECK_VERSION   = $(GOVULNCHECK_VERSION)"

## Generate testify-style mocks. Re-run whenever an interface in a configured
## package changes. Mocks are committed to git so CI does not need mockery.
mocks:
	@if [ ! -f .mockery.yaml ]; then \
	    echo "no .mockery.yaml; nothing to generate"; \
	else \
	    go tool mockery; \
	fi

## Verify committed mocks match the current interfaces. Used by CI.
verify-mocks: mocks
	@if ! git diff --quiet -- '**/mocks/' 2>/dev/null; then \
	    echo "FAIL: mocks out of sync. Run 'make mocks' and commit."; \
	    git --no-pager diff -- '**/mocks/'; \
	    exit 1; \
	fi
	@echo "mocks up-to-date"

## ---------------------------------------------------------------------------

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/kube-state-graph

test:
	go test ./... -count=1 -race -shuffle=on -timeout 30m

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed: https://golangci-lint.run/usage/install/"; exit 1; }
	golangci-lint run --timeout=5m

vuln:
	@if command -v govulncheck >/dev/null 2>&1; then \
	    govulncheck ./...; \
	else \
	    go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...; \
	fi

## Dependency-containment check (translate-global-fqdn-to-k8s-service D1):
## pkg/build declares only the RouteResolver interface; the heavy engine lives
## in pkg/route. If pkg/kubegraph (or pkg/build) ever reaches k8s.io/client-go
## or pkg/route, an embedder of the graph engine would silently inherit istio +
## ClickHouse — fail the build instead.
check-route-containment:
	@if go list -deps ./pkg/kubegraph ./pkg/build | grep -q '^k8s.io/client-go'; then \
	    echo "FAIL: pkg/kubegraph or pkg/build links k8s.io/client-go — the pkg/route containment rule is broken"; \
	    exit 1; \
	fi
	@if go list -deps ./pkg/kubegraph ./pkg/build | grep -q 'kube-state-graph/pkg/route'; then \
	    echo "FAIL: pkg/kubegraph or pkg/build imports pkg/route — only cmd/ may link the route engine"; \
	    exit 1; \
	fi
	@echo "route-engine containment OK (pkg/kubegraph and pkg/build never reach pkg/route or client-go)"

## Extract router_check_tool from the digest-pinned Envoy tools image into
## $(ROUTER_CHECK_BIN). The route-engine e2e (internal/integration TestRouteSuite)
## needs the native binary and FAILS rather than skips under CI, so this is the
## CI install step as well as a local convenience. `docker create` materialises
## the container filesystem WITHOUT running anything — no entrypoint, no port.
## The binary is a dynamically linked Linux C++ executable: it runs on a Linux
## host (or a Linux container) and NOT on macOS, where developers keep skipping.
router-check-tool:
	@mkdir -p $(BIN_DIR)
	@echo ">> extracting router_check_tool from $(ENVOY_TOOLS_IMAGE)"
	@cid=$$(docker create $(ENVOY_TOOLS_IMAGE)) && \
	    docker cp $$cid:/usr/local/bin/router_check_tool $(ROUTER_CHECK_BIN) >/dev/null && \
	    docker rm -f $$cid >/dev/null
	@chmod +x $(ROUTER_CHECK_BIN)
	@echo "router_check_tool -> $(ROUTER_CHECK_BIN)"

## Full local CI mirror — runs the same checks as the five GitHub Actions jobs
## in .github/workflows/ci.yml (lint, vuln, test, docs-drift, mocks-drift), in
## order. Invoked by the pre-push hook (see `make init-hooks`). Run directly to
## reproduce CI locally before pushing.
ci: lint vuln test check-docs verify-mocks check-route-containment
	@echo "ci: all checks passed (lint + vuln + test + docs + mocks + containment)"

cover:
	go test ./... -coverprofile=coverage.out -covermode=atomic
	go tool cover -func=coverage.out | tail -1

## Regenerate the OpenAPI spec from swag annotations. Outputs only the JSON +
## YAML spec into docs/ (no docs.go); docs/embed.go compiles them into the
## binary, which serves them at /openapi.json and /openapi.yaml. The Scalar UI
## at /docs loads from the CDN — no vendored assets to refresh.
docs:
	go tool swag init -g cmd/kube-state-graph/main.go --output docs --outputTypes json,yaml --parseDependency --parseInternal --v3.1=true

check-docs: docs
	@if ! git diff --quiet -- docs/; then \
		echo "FAIL: docs are out of sync. Run 'make docs' and commit."; \
		git --no-pager diff -- docs/; \
		exit 1; \
	fi

clean:
	rm -rf $(BIN_DIR) coverage.out

## Container image build / push.
##
## Single-arch LOCAL build (host arch). Feeds docker-run / docker-docs and
## tags the :dev tag ($(LOCAL_TAG)). This image is NEVER pushed to a registry —
## a host-arch image would not run on differently-architected nodes. Publish
## with `make docker-push` (multi-arch) instead.
docker-build:
	docker build $(DOCKER_BUILD_ARGS) \
		-f $(DOCKERFILE) \
		-t $(IMAGE):$(VERSION) \
		-t $(IMAGE):latest \
		-t $(LOCAL_TAG) \
		.

## Publish to a registry. This is the ONLY publish path and it is ALWAYS
## multi-arch (delegates to docker-buildx) so a push can never be accidentally
## single-arch. Run `docker login $(REGISTRY)` first.
docker-push: docker-buildx

## Multi-arch build + push using buildx ($(PLATFORMS)). No QEMU needed: the
## build stage pins --platform=$$BUILDPLATFORM and Go cross-compiles via
## GOARCH with CGO disabled, and the distroless runtime stage only COPYs.
## Requires `docker buildx` and a builder able to export a multi-platform
## manifest. Docker Desktop with the containerd image store works as-is;
## otherwise create a docker-container builder once:
##   docker buildx create --name ksg --use --bootstrap
docker-buildx:
	docker buildx build $(DOCKER_BUILD_ARGS) \
		--platform $(PLATFORMS) \
		-f $(DOCKERFILE) \
		-t $(IMAGE):$(VERSION) \
		-t $(IMAGE):latest \
		--push \
		.

## Run the freshly built image locally against a Prom URL.
##   make docker-run PROM_URL=http://host.docker.internal:8428
PROM_URL ?= http://host.docker.internal:8428
docker-run: docker-build
	docker run --rm -p 8080:8080 $(LOCAL_TAG) \
		--prom-url=$(PROM_URL) --listen-addr=:8080

## Docker-only docs preview. Runs the API server in Docker with a placeholder
## upstream so the static OpenAPI / Scalar UI routes are reachable. /v1/* and
## /readyz will return upstream errors (no VictoriaMetrics is started); only the
## docs surfaces are intended for verification here.
##
##   make docker-docs            # build + run, foreground (Ctrl-C to stop)
##   make docker-docs DETACH=1   # run detached; stop with `make docker-docs-stop`
##
## Then visit:
##   http://localhost:8080/docs           — Scalar UI
##   http://localhost:8080/openapi.json   — OpenAPI JSON
##   http://localhost:8080/openapi.yaml   — OpenAPI YAML
DOCS_PORT ?= 8080
DOCS_NAME ?= kube-state-graph-docs
DETACH    ?=
docker-docs: docker-build
	@echo "Starting $(DOCS_NAME) on http://localhost:$(DOCS_PORT)/docs"
	@echo "  Scalar UI : http://localhost:$(DOCS_PORT)/docs"
	@echo "  OpenAPI   : http://localhost:$(DOCS_PORT)/openapi.json"
	@echo "              http://localhost:$(DOCS_PORT)/openapi.yaml"
	docker run --rm $(if $(DETACH),-d,) --name $(DOCS_NAME) \
		-p $(DOCS_PORT):8080 \
		$(LOCAL_TAG) \
		--prom-url=http://127.0.0.1:8428 \
		--listen-addr=:8080 \
		--log-level=info

docker-docs-stop:
	-docker rm -f $(DOCS_NAME) 2>/dev/null || true
