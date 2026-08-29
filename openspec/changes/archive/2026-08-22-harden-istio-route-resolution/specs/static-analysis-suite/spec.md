## ADDED Requirements

### Requirement: Route-engine end-to-end coverage runs in CI

The CI workflow's `test` job SHALL make the native `router_check_tool` binary available to
the route-engine end-to-end suite and SHALL point `KSG_ROUTER_CHECK_BIN` at it. The suite
SHALL fail the job when the binary is unavailable **in CI** — a missing tool SHALL NOT
present as a skipped (and therefore passing) test. Outside CI the suite SHALL continue to
skip, so a developer machine without the Linux binary is unaffected.

The binary SHALL be obtained from the same pinned Envoy tools image the server image uses,
so the matcher exercised by CI is the matcher shipped in the image.

#### Scenario: Route e2e executes on a pull request

- **WHEN** a developer opens a pull request
- **THEN** the `test` job's log shows the route-engine end-to-end suite executing its cases rather than skipping

#### Scenario: Missing matcher binary fails CI

- **GIVEN** `router_check_tool` is not available to the job
- **WHEN** the route-engine end-to-end suite starts in CI
- **THEN** the job fails rather than reporting the suite as passed

### Requirement: Dependency-containment gate runs in CI

The CI workflow SHALL run the route-engine dependency-containment check on every pull
request, as an independent job with no `needs` edges. A change that makes `pkg/build` or
`pkg/kubegraph` reach `pkg/route` or `k8s.io/client-go` SHALL fail the required checks
rather than merge green.

The same check SHALL remain reproducible locally via `make check-route-containment`.

#### Scenario: Containment job visible in PR checks

- **WHEN** a developer opens a pull request
- **THEN** the PR check list shows the containment job as an independent required check

#### Scenario: Containment breach fails the PR

- **GIVEN** a change that makes the embeddable graph engine link the route engine
- **WHEN** CI runs
- **THEN** the containment job fails

### Requirement: CI test job invokes the Makefile target

The CI workflow's `test` job SHALL invoke the repository's `make test` target rather than
re-spelling the `go test` command, so that flags the target carries — notably the timeout
the container-backed suites need — apply in CI as well as locally. This mirrors the
OpenAPI drift gate's use of `make check-docs`.

#### Scenario: Container suites get the Makefile timeout

- **WHEN** the CI `test` job runs
- **THEN** it executes the `make test` target and the timeout that target specifies applies to the container-backed suites

### Requirement: Envoy tools image pinned by digest

The container image build SHALL reference the Envoy tools image that supplies
`router_check_tool` by immutable digest, not by a floating tag. The digest SHALL be
declared in exactly one place, and any other consumer of that image — including CI —
SHALL derive it from that declaration rather than repeating it.

#### Scenario: Image reference is immutable

- **WHEN** the server image is built
- **THEN** the stage supplying `router_check_tool` resolves to a digest-pinned image reference

#### Scenario: CI uses the same pinned image

- **WHEN** CI obtains `router_check_tool`
- **THEN** it derives the image reference from the same single declaration the image build uses
