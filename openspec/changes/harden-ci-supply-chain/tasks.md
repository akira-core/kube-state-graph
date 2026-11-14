# Tasks

## 1. Context on every exec and HTTP call (`noctx`)

- [x] 1.1 Bound the `router_check_tool --version` probe in `pkg/route/matchcheck` with `exec.CommandContext` (10s) and `WaitDelay`, reporting a timeout separately from a non-executable binary; add `probe_test.go` cases for success, failure and timeout. Verify `go test -race ./pkg/route/matchcheck` passes.
- [x] 1.2 Bind the integration `docker info` probe to a 30s context (design D8).
- [x] 1.3 Enable `noctx` in `.golangci.yml`, run golangci-lint at `GOLANGCI_LINT_VERSION`, and fix every hit: test HTTP requests use `http.NewRequestWithContext(t.Context(), …)` (`internal/api` `httpGet` helper). Verify the lint run reports no `noctx` finding.
- [x] 1.4 Replace the remaining `//nolint:noctx` `/livez` request in `internal/integration/graph_e2e_test.go` with a context-bound request, so no `nolint:noctx` directive remains. Verify `grep -rn 'nolint:noctx' .` finds nothing.

## 2. Single-source tool pins (`Makefile`, CI)

- [x] 2.1 Pin `GOLANGCI_LINT_VERSION ?= v2.15.0` and `GOVULNCHECK_VERSION ?= v1.9.0`, recording why the linter must be at least v2.13.0 (design D3). Update the `.devcontainer` text that names the version.
- [x] 2.2 `make init-tools` installs golangci-lint with `go install …@$(GOLANGCI_LINT_VERSION)` (design D4); `make vuln` always runs `go run …/govulncheck@$(GOVULNCHECK_VERSION)`.
- [x] 2.3 `init-tools` and `doctor` check `$(GOBIN)/golangci-lint` and `$(GOBIN)/govulncheck` themselves; `make lint` runs `$(GOBIN)/golangci-lint` when present, else `PATH`. Verify with a scratch `GOBIN`: the first `make init-tools` installs, a second is a no-op, and `make -n lint GOBIN=<scratch>` names the scratch binary.
- [x] 2.4 `.githooks/pre-commit` prefers the same `$(GOBIN)` golangci-lint over `PATH`.
- [x] 2.5 CI `lint` reads `GOLANGCI_LINT_VERSION` into the action's `version` input and fails if it is missing; CI `vuln` runs `make vuln`.

## 3. Immutable CI and image inputs

- [x] 3.1 Pin every `uses:` in `.github/workflows/` to a commit SHA resolved with `gh api` (annotated tags dereferenced), with a `# vX.Y.Z` comment (design D1).
- [x] 3.2 Pin the `# syntax=` frontend and both `FROM` images in `deploy/docker/server.Dockerfile` by the index digest from `docker buildx imagetools inspect`, keeping the tag (design D5).
- [x] 3.3 Narrow the Dockerfile header: Dependabot tracks the two `FROM` images only; the frontend and `ENVOY_TOOLS_IMAGE` are re-pinned by hand.
- [x] 3.4 Add an `image-build` CI job (no `needs`, `timeout-minutes`) running `make docker-build` without push; list it in the `ci.yml` header comment and the Makefile `ci` comment. Verify `make docker-build` succeeds locally.

## 4. Bounded runs and update proposals

- [x] 4.1 Add `timeout-minutes` to every job in `ci.yml` and `ng-words.yml`, and a top-level `concurrency` group that cancels superseded PR runs only (design D7).
- [x] 4.2 Add `.github/dependabot.yml` for `gomod`, `github-actions` and `docker` (`/deploy/docker`), weekly and grouped, with a header naming the untracked pins and the job that gates each ecosystem (design D6).

## 5. Container-backed suites cannot pass by skipping in CI

- [x] 5.1 Extract the skip-or-fail decision of `SkipIfDockerUnavailable` into a pure function and unit-test it: outside CI each condition skips; with `CI` set each condition fails; a timed-out probe's message names the timeout. Verify `go test -race -run TestDocker ./internal/integration` passes without Docker.
- [x] 5.2 Wire it into `SkipIfDockerUnavailable` (design D9). Verify `go test -race -count=1 ./internal/integration` passes with Docker.

## 6. Verification

- [x] 6.1 `go build ./... && go vet ./...`, `gofmt -l .` empty, `GOTOOLCHAIN=go1.27.1 go build ./pkg/...`, and `go.mod` / `go.sum` unchanged.
- [x] 6.2 `openspec validate harden-ci-supply-chain` passes.
