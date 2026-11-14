# Proposal

## Why

The CI and image build pulled code by mutable reference at every layer. GitHub Actions ran by major tag (`actions/checkout@v6`), so whoever controls the tag controls the job. `govulncheck` ran at `@latest`, and the `golangci-lint` action resolved its own "latest" at run time, so neither matched `make vuln` or `make lint` on a developer's machine. `make init-tools` piped `install.sh` from golangci-lint's `master` branch into `sh`. The server image named its base images by tag only, so a re-pushed tag changed the build silently. No job had a timeout, so a hung step could hold a runner for the 360-minute default. Nothing proposed pin bumps, so pins would only rot. Several exec and HTTP calls also had no context, so a wedged child process or a hung connection could outlive its caller. The `static-analysis-suite` spec still states `govulncheck@latest`, which no longer matches the pinned CI.

## What Changes

- Every third-party GitHub Action is pinned to a full commit SHA with a trailing `# vX.Y.Z` comment.
- The Makefile's `GOLANGCI_LINT_VERSION` and `GOVULNCHECK_VERSION` are the single source of both tool versions. The CI `lint` job reads `GOLANGCI_LINT_VERSION` into the action's `version` input. The CI `vuln` job runs `make vuln`, which runs `go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...`. `golangci-lint` moves from v2.11.4 to v2.15.0, because releases before v2.13.0 cannot read go1.27 export data.
- `make init-tools` installs `golangci-lint` with `go install …@$(GOLANGCI_LINT_VERSION)`, never from a script on a branch. It checks and installs the binaries in `$(GOBIN)` itself, and `make lint` and `make doctor` use the `$(GOBIN)` binaries when they exist, so a different version earlier on `PATH` cannot stand in for the pin.
- `deploy/docker/server.Dockerfile` pins the `# syntax=` frontend and both `FROM` images as `tag@sha256:<index digest>`.
- A new `image-build` CI job builds the server image without pushing, so a base-image bump is built before it merges.
- Every CI job has `timeout-minutes`. A top-level `concurrency` group cancels a pull request's superseded runs and never cancels a run on `main`.
- `.github/dependabot.yml` proposes weekly, grouped bumps for Go modules, GitHub Actions and the Dockerfile's `FROM` images.
- The `noctx` linter is enabled and every hit is fixed: production exec calls (the `router_check_tool` probes) and the integration `docker info` probe run under a bounded context, and test HTTP requests use the test's context.
- The integration suites' Docker reachability probe fails the test in CI instead of skipping it, and reports a timeout separately from an unreachable daemon.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `static-analysis-suite`: pinned tool versions read from one Makefile declaration (lint and vuln), `noctx` in the curated linter set, local targets that run the pinned `$(GOBIN)` binaries, SHA-pinned actions, digest-pinned image inputs built in CI, bounded and de-duplicated CI runs, Dependabot proposals, and container-backed suites that cannot pass by skipping in CI.

## Impact

- **CI**: `.github/workflows/ci.yml` and `.github/workflows/ng-words.yml` (SHA pins, timeouts, concurrency, the new `image-build` job, `make vuln`), `.github/dependabot.yml` (new).
- **Build**: `Makefile` (tool pins, `init-tools`, `lint`, `doctor`, `vuln`), `deploy/docker/server.Dockerfile` (digest pins), `.golangci.yml` (`noctx`), `.devcontainer/` (pinned version text).
- **Code**: `pkg/route/matchcheck` (context-bounded probes), `internal/integration/vmsuite.go` (bounded Docker probe, fails in CI), test HTTP requests in `internal/api`, `internal/integration` and `pkg/promql`.
- **API**: none. No route, parameter, body, golden or self-metric changes.
- **Go API**: none. `go.mod` and `go.sum` are unchanged, and nothing in `pkg/` needs newer Go than 1.27.1.
