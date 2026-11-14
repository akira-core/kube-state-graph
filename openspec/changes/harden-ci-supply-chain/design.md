# Design

## Context

See proposal.md, section Why. The current state before this change:

- `.github/workflows/ci.yml` runs `lint`, `vuln`, `embedder-go`, `test`, `docs-drift`, `route-containment` and `mocks-drift` as parallel jobs with no `needs` edges. `.github/workflows/ng-words.yml` runs one `check` job. Every `uses:` names a major tag, and no job sets `timeout-minutes` or `concurrency`.
- The `lint` job runs `golangci/golangci-lint-action` with no `version` input, so the action picks "latest". The `vuln` job runs `go install golang.org/x/vuln/cmd/govulncheck@latest`. The Makefile pins `GOLANGCI_LINT_VERSION ?= v2.11.4` and `GOVULNCHECK_VERSION ?= latest`, but CI reads neither.
- `make init-tools` installs golangci-lint by piping `raw.githubusercontent.com/golangci/golangci-lint/master/install.sh` into `sh`. It and `make lint` test whichever `golangci-lint` comes first on `PATH`.
- `deploy/docker/server.Dockerfile` pins `ENVOY_TOOLS_IMAGE` by digest, which `make router-check-tool` reads. The `# syntax=docker/dockerfile:1.7` frontend, `golang:1.27.4-alpine` and `gcr.io/distroless/cc:nonroot` are tags only. No CI job builds the image.
- `.golangci.yml` does not enable `noctx`. `pkg/route/matchcheck.NewRunner` probes `router_check_tool --version` with `exec.Command`, and `internal/integration.SkipIfDockerUnavailable` runs `docker info` the same way. Neither has a bound.

## Goals / Non-Goals

**Goals:**

- Every input that CI or the image build fetches is named immutably: actions by commit SHA, images by index digest, and tools by module version.
- One declaration per tool version, read by both CI and the local targets.
- Pins move forward only through a reviewable PR that CI has built.
- A hung step or child process ends at a known bound.

**Non-Goals:**

- Signing or attesting the published image (cosign, SLSA provenance).
- Pinning the GitHub-hosted runner image (`ubuntu-latest`).
- Automating bumps of the `# syntax=` frontend, `ENVOY_TOOLS_IMAGE`, or the two Makefile tool versions. Dependabot cannot see them (D6).
- Changing `go.mod` or `go.sum`.

## Decisions

### D1. Actions pinned by full commit SHA with a version comment

Each `uses:` names the 40-hex commit with a trailing `# vX.Y.Z`. The SHA is resolved with `gh api repos/<owner>/<repo>/git/ref/tags/<tag>`, and an annotated tag is dereferenced through `git/tags/<sha>`. The comment is what Dependabot's `github-actions` ecosystem rewrites alongside the SHA. The first-party `akira-core/action-workflow` is pinned the same way, because a moving `v1` tag is the same exposure whoever owns it.

- *Alternative: keep major tags and trust the publishers.* Rejected. A tag is mutable, and a compromised or re-pointed tag runs with the job's token.
- *Alternative: pin the SHA without the comment.* Rejected. Reviewers and Dependabot both need the version beside the SHA.

### D2. The Makefile is the single source of both tool versions

`GOLANGCI_LINT_VERSION` and `GOVULNCHECK_VERSION` stay in the Makefile as `NAME ?= value`. The `lint` job reads `GOLANGCI_LINT_VERSION` with `sed`, fails if it reads nothing, and passes the value to the action's `version` input. The `vuln` job runs `make vuln`, which always runs `go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...` and never whatever is on `PATH`. So the local target and the CI job cannot disagree.

- *Alternative: repeat the version in `ci.yml`.* Rejected. Two declarations drift, and the old `latest` / `v2.11.4` split is exactly that drift.
- *Alternative: run golangci-lint through `make lint` in CI as well.* Rejected. The action brings result caching and annotations. Reading the pin keeps the single source.
- *Alternative: keep `govulncheck@latest`.* Rejected. A new release could fail or pass a PR with no repository change. A Makefile bump is reviewable.

### D3. golangci-lint v2.15.0, not v2.11.4

The audit plan names v2.11.4 as the pin. Running it fails every package with "export data version 5 is greater than maximum supported version 2", because the module is built with the go1.27 toolchain and only golangci-lint v2.13.0 and later read go1.27 export data. The pin is therefore v2.15.0, the release in use locally. The `.devcontainer` text that names the version moves with it.

- *Alternative: keep v2.11.4 as the plan says.* Rejected. It cannot lint this module at all.

### D4. `init-tools` installs from the module proxy and tests `$(GOBIN)` itself

`init-tools` runs `GOBIN=$(GOBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)`. That build goes through the Go module proxy and the checksum database, never through a script fetched from a branch. Both tools are version-checked at `$(GOBIN)/<tool>`, the path `init-tools` installs to. `make lint` runs `$(GOBIN)/golangci-lint` when it exists and falls back to `PATH`. `make doctor` reports both `$(GOBIN)` binaries, and `.githooks/pre-commit` prefers the same `$(GOBIN)` linter. A different `golangci-lint` earlier on `PATH` (for example a Homebrew one) can therefore neither suppress the install nor run in its place.

- *Alternative: the release-tag `install.sh` (`…/<tag>/install.sh`).* Rejected. The script is still fetched and executed unverified. `go install` is verified by `sum.golang.org`.
- *Alternative: test whatever is on `PATH`.* Rejected. It reported "already installed" for a binary that `init-tools` never installed and that may be a different version.

### D5. Base images and the frontend pinned by multi-arch index digest; built in CI

The `# syntax=` frontend and both `FROM` lines read `tag@sha256:<digest>`. The digest is the top-level `Digest:` that `docker buildx imagetools inspect <tag>` prints, which is the multi-arch index, so `make docker-buildx` still resolves one manifest per platform. The tag stays beside the digest so a reader sees what the image is. A new `image-build` job runs `make docker-build`, a host-arch build without push, so a Dependabot PR that moves a `FROM` digest is built before it merges. `GOTOOLCHAIN=local` in the build stage turns a golang base image that disagrees with `go.mod`'s `toolchain` into a build error in that job.

- *Alternative: per-platform manifest digests.* Rejected. A multi-platform build would then pull one architecture's image for every platform.
- *Alternative: build multi-arch in CI.* Rejected. The default `docker` buildx driver on the hosted runner cannot export a multi-platform image without a separate builder action, which would be one more SHA to pin. Both `FROM` digests are indexes, so the arm64 manifest comes from the same pinned index.
- *Alternative: leave image builds out of CI and say docker bumps need a manual build.* Rejected. A Dependabot docker PR would then be green without the image ever being built.

### D6. Dependabot for gomod, github-actions and docker, weekly and grouped

One grouped PR per ecosystem per week. The docker ecosystem watches `/deploy/docker` and tracks the two `FROM` images. It does not track the `# syntax=` frontend or the `ENVOY_TOOLS_IMAGE` ARG. Those, and the two Makefile tool versions, are re-pinned by hand. `dependabot.yml` and the Dockerfile header both say so.

- *Alternative: a PR per dependency.* Rejected. With every action and image pinned, the PR volume would bury review.

### D7. Bounded jobs, and superseded PR runs cancelled

Every job sets `timeout-minutes`, sized to its work: 10 to 15 for the checks, 25 for `embedder-go`, 30 for the container-backed `test` job, and 20 for `image-build`. The workflow-level `concurrency` group is keyed on the PR ref for `pull_request` events and on `github.sha` for pushes, and `cancel-in-progress` is true only for `pull_request`. A main group keyed on the ref would let a later push drop a queued main run, leaving that commit with no status.

### D8. `noctx` enabled and every hit fixed

`noctx` joins the curated linter set. Every reported call gets a context; none is suppressed. The `router_check_tool --version` probe runs under a 10s `exec.CommandContext` with `WaitDelay`. The integration `docker info` probe runs under a 30s bound. Test HTTP requests use `http.NewRequestWithContext(t.Context(), …)`, through a small `httpGet` helper in `internal/api`.

- *Alternative: `//nolint:noctx` on test code.* Rejected. A test request bound to `t.Context()` is cancelled when the test ends, instead of leaking.

### D9. In CI, a container-backed suite cannot pass by skipping

`SkipIfDockerUnavailable` skips on a developer machine without Docker, as before. When `CI` is set, the same conditions fail the test instead: no `docker` binary, a `docker info` error, or a probe that timed out. `go test` prints a skipped suite as `ok`, so a skip in CI would merge a regression green. This is the rule `TestRouteSuite` already follows for `router_check_tool`. A timed-out probe has its own message ("did not answer within 30s"), so it is not confused with an unreachable daemon. The decision lives in a small pure function that unit tests drive.

- *Alternative: always fail on a timeout, even locally.* Rejected. A developer whose Docker Desktop is paused should still get the rest of the tree.

## Risks / Trade-offs

- [A SHA or digest pin freezes security fixes as well as regressions.] Dependabot proposes them weekly, and the `image-build` and `embedder-go` jobs gate them.
- [Hand-maintained pins, namely the frontend, `ENVOY_TOOLS_IMAGE` and the two Makefile tools, can age.] Both headers name them and give the re-pin command.
- [`make vuln` builds govulncheck through `go run` on each invocation.] The Go build cache makes repeat runs cheap.
- [The `image-build` job adds minutes of runner time to every PR.] It runs in parallel with the other jobs, so it does not lengthen PR latency beyond the slowest job.

## Migration Plan

No runtime migration. A developer whose `golangci-lint` on `PATH` is older than v2.13.0 runs `make init-tools`, and `make lint` then uses the `$(GOBIN)` binary.
