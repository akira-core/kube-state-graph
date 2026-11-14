## MODIFIED Requirements

### Requirement: Curated golangci-lint configuration

The repository SHALL ship a `.golangci.yml` at its root enabling at minimum the linters listed in this requirement's scenarios, applied to all Go source files in the repository, including `cmd/`, `internal/`, and `tests/`. Test files MAY relax `errcheck` and the strictest complexity / duplication rules. Test files SHALL NOT relax `noctx`: a test HTTP request or exec call SHALL carry the test's context rather than a `//nolint:noctx` directive.

#### Scenario: All linters enabled

- **WHEN** an operator runs `golangci-lint linters` against the repository
- **THEN** every linter below appears in the "Enabled" list
- **AND** correctness: `errcheck`, `gosimple`, `govet`, `ineffassign`, `staticcheck`, `unused`, `gocritic`, `exhaustive`
- **AND** modern Go idioms: `copyloopvar`, `intrange`, `revive`
- **AND** error handling: `errorlint`, `nilerr`
- **AND** cancellation: `noctx`
- **AND** security: `gosec`
- **AND** complexity: `gocyclo`, `gocognit`, `funlen`
- **AND** performance: `prealloc`, `bodyclose`, `unconvert`
- **AND** style: `misspell`, `gofmt`, `goimports`
- **AND** dead code / duplication: `dupl`, `unparam`
- **AND** magic numbers: `mnd`
- **AND** testify usage: `testifylint` (with `enable-all: true`)

#### Scenario: Test files relaxed

- **WHEN** a test file legitimately repeats table-driven structure
- **THEN** `golangci-lint run` does NOT flag it for `dupl` or `errcheck`

#### Scenario: A context-free call is fixed, not suppressed

- **WHEN** `noctx` reports an HTTP request or `exec.Command` call, in production or test code
- **THEN** the call is rewritten to carry a context (`http.NewRequestWithContext`, `exec.CommandContext`), bounded by a timeout where no caller context exists
- **AND** no `//nolint:noctx` directive is added

### Requirement: golangci-lint runs on every PR

The repository SHALL include a CI workflow that runs `golangci-lint` on every pull request via `golangci/golangci-lint-action` v8 or a newer major, referenced by commit SHA, with `args: --timeout=5m`. The workflow SHALL gate merges on lint success. The job SHALL pass the action an explicit `version` read from the Makefile's `GOLANGCI_LINT_VERSION` declaration, which SHALL be the only place that version is declared. The job SHALL fail when it cannot read that declaration, rather than letting the action resolve a version at run time.

#### Scenario: Lint job runs on PR

- **WHEN** a developer opens a pull request
- **THEN** the CI workflow's `lint` job runs and any failure marks the PR as failing required checks

#### Scenario: CI lints at the Makefile's version

- **GIVEN** the Makefile declares `GOLANGCI_LINT_VERSION ?= v2.15.0`
- **WHEN** the `lint` job runs
- **THEN** the action runs golangci-lint v2.15.0
- **AND** changing that one Makefile line changes the version both CI and `make init-tools` use

#### Scenario: Missing declaration fails the job

- **WHEN** the Makefile no longer carries a `GOLANGCI_LINT_VERSION ?= …` line
- **THEN** the `lint` job fails before running the action

### Requirement: govulncheck on every PR

The repository SHALL include a CI workflow step that runs `govulncheck ./...` on every pull request at the version declared by the Makefile's `GOVULNCHECK_VERSION`, by invoking `make vuln`, which runs `go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...`. `GOVULNCHECK_VERSION` SHALL name a released version, never `latest`. Detected vulnerabilities SHALL gate merges; suppressions SHALL be made explicit via comment plus a tracking issue, never via silent ignoring.

#### Scenario: govulncheck flags a known CVE

- **WHEN** a vulnerable transitive dependency reachable from the binary is on the dependency graph
- **THEN** `govulncheck ./...` exits non-zero and the PR's `vuln` job fails

#### Scenario: Suppressions documented

- **WHEN** a contributor needs to suppress a finding (e.g., a vulnerability that does not affect the reachable code path)
- **THEN** the suppression appears as a code comment referencing a tracked issue, not as a removal of the `vuln` job

#### Scenario: CI and local scans run the same govulncheck

- **WHEN** the `vuln` job runs and a developer runs `make vuln`
- **THEN** both run govulncheck at `GOVULNCHECK_VERSION`, regardless of any `govulncheck` on `PATH`

### Requirement: Local equivalents

The repository's `Makefile` (or equivalent) SHALL provide developer-facing targets that run the same checks locally — `make lint`, `make vuln`, `make test`, `make docs` and `make check-docs` — and each target SHALL exit non-zero on any failure. `make init-tools` SHALL install golangci-lint at `GOLANGCI_LINT_VERSION` and govulncheck at `GOVULNCHECK_VERSION` into `$(GOBIN)` with `go install <module>@<version>`, SHALL decide whether to install by checking the version of the binary in `$(GOBIN)`, not the first one on `PATH`, and SHALL NOT execute an install script fetched over the network. `make lint` SHALL run `$(GOBIN)/golangci-lint` when that file exists, and SHALL fall back to `PATH` only when it does not.

#### Scenario: Local targets run the CI checks

- **WHEN** a developer inspects the `Makefile`
- **THEN** `make lint` runs `golangci-lint run`, `make vuln` runs `govulncheck ./...`, and `make test` runs `go test ./...`
- **AND** `make docs` runs `swag init -g cmd/kube-state-graph/main.go --output docs --parseDependency --parseInternal`
- **AND** `make check-docs` runs `make docs`, then `git diff --exit-code docs/`, and fails when generated files would change

#### Scenario: Local lint matches CI

- **GIVEN** a developer has run `make init-tools`
- **WHEN** the developer runs `make lint` after making a change
- **THEN** the linter version is the Makefile's `GOLANGCI_LINT_VERSION`, the same version the CI `lint` job runs
- **AND** the linter set, complexity caps, and per-file exemptions are identical to those applied by the CI workflow's `lint` job

#### Scenario: A different linter on PATH does not stand in for the pin

- **GIVEN** a `golangci-lint` of another version comes before `$(GOBIN)` on `PATH`
- **WHEN** the developer runs `make init-tools` and then `make lint`
- **THEN** `init-tools` installs the pinned version into `$(GOBIN)` unless `$(GOBIN)/golangci-lint` already reports it
- **AND** `make lint` runs `$(GOBIN)/golangci-lint`

#### Scenario: Re-running init-tools is a no-op

- **GIVEN** `$(GOBIN)` already holds both tools at their pinned versions
- **WHEN** the developer runs `make init-tools` again
- **THEN** nothing is downloaded or rebuilt

## ADDED Requirements

### Requirement: Third-party actions pinned by commit SHA

Every `uses:` in `.github/workflows/` that names an action SHALL reference a full 40-character commit SHA followed by a `# vX.Y.Z` (or `# vN`) comment naming the release that SHA resolves from. A major or minor tag reference (`@v6`) SHALL NOT appear. This applies to first-party actions as well.

#### Scenario: A tag reference is rejected in review

- **WHEN** a workflow change adds `uses: actions/checkout@v6`
- **THEN** it violates this requirement, and the reference is replaced by the SHA that `v6.x.y` resolves to, with the version as a trailing comment

#### Scenario: The SHA is resolved, not remembered

- **WHEN** a contributor pins or bumps an action
- **THEN** the SHA is the commit the release tag points to, dereferenced through an annotated tag object where there is one

### Requirement: Server image inputs pinned by digest and built in CI

`deploy/docker/server.Dockerfile` SHALL reference the `# syntax=` frontend and every `FROM` image as `<tag>@sha256:<digest>`, where the digest is the image's multi-arch index digest. The CI workflow SHALL include an `image-build` job, with no `needs` edges, that builds that Dockerfile without pushing, so a change to any pinned image input is built before it merges.

#### Scenario: A re-pushed tag does not change the build

- **WHEN** an upstream re-pushes `golang:1.27.4-alpine` or `gcr.io/distroless/cc:nonroot`
- **THEN** the server image build still resolves the pinned digest

#### Scenario: A base-image bump is built before merge

- **WHEN** a pull request moves a `FROM` digest
- **THEN** the `image-build` job builds the image, and a broken base image or a golang image that disagrees with `go.mod`'s `toolchain` fails the PR

### Requirement: CI runs are bounded and superseded PR runs cancelled

Every job in `.github/workflows/` SHALL set `timeout-minutes`. Each workflow SHALL declare a top-level `concurrency` group that cancels an in-progress run of the same pull request when a newer push arrives, and SHALL NOT cancel runs triggered by pushes to `main`.

#### Scenario: A hung step ends at the job bound

- **WHEN** a step stops making progress
- **THEN** the job fails at its `timeout-minutes`, not at the platform's 360-minute default

#### Scenario: A new push supersedes the PR's run

- **WHEN** a developer pushes to a pull request while its previous run is still in progress
- **THEN** the previous run is cancelled

#### Scenario: Every main commit keeps a status

- **WHEN** two commits land on `main` in quick succession
- **THEN** both runs complete, and neither cancels the other

### Requirement: Automated dependency update proposals

The repository SHALL carry a `.github/dependabot.yml` that proposes updates weekly for the `gomod`, `github-actions` and `docker` (`/deploy/docker`) ecosystems, with one grouped pull request per ecosystem. The pinned supply-chain inputs Dependabot does not track — the Dockerfile's `# syntax=` frontend, its `ENVOY_TOOLS_IMAGE` ARG, and the Makefile's `GOLANGCI_LINT_VERSION` and `GOVULNCHECK_VERSION` — SHALL each be named, with how to re-pin it, in the file that declares it.

#### Scenario: Pins move forward through a reviewable PR

- **WHEN** a new release of a pinned action, Go module or `FROM` image is published
- **THEN** within a week a grouped Dependabot pull request proposes it, and the PR runs the same required checks as any other

#### Scenario: Untracked pins are documented

- **WHEN** a maintainer reads `deploy/docker/server.Dockerfile` or the Makefile's tool versions
- **THEN** the file says that the `# syntax=` frontend, `ENVOY_TOOLS_IMAGE` and the two tool versions are re-pinned by hand, and how

### Requirement: Container-backed suites do not skip in CI

The integration suites' Docker availability check SHALL bound its `docker info` probe with a timeout. Outside CI, when the `docker` binary is missing, the daemon is unreachable or the probe times out, the suite SHALL skip. When the `CI` environment variable is set, the same conditions SHALL fail the test. The failure or skip message SHALL distinguish a probe that timed out from a daemon that answered with an error.

#### Scenario: A wedged daemon in CI fails the job

- **GIVEN** `CI` is set
- **WHEN** `docker info` does not answer within the probe timeout
- **THEN** the suite fails with a message naming the timeout, and does not report `ok`

#### Scenario: No Docker on a developer machine

- **GIVEN** `CI` is unset
- **WHEN** the `docker` binary is missing or the daemon is unreachable
- **THEN** the suite skips and the rest of the test tree runs
