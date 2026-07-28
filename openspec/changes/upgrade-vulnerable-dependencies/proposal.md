# Upgrade the vulnerable dependencies

## Why

CI's `vuln` job has been red since before the current branch, so the gate the
repository documents as blocking merges has in fact been blocking nothing.
`govulncheck` reports four vulnerabilities across three modules that it
classifies as reachable from this binary:

| Module | Vulnerability | Reachability |
|---|---|---|
| `golang.org/x/text` v0.38.0 | GO-2026-5970 — infinite loop on invalid input | `promql.Client` → `http.Transport.RoundTrip` → `norm.Form.*` (IDNA normalisation): **every upstream PromQL request** |
| `google.golang.org/grpc` v1.81.1 | GO-2026-6061 — xDS RBAC engine and HTTP/2 transport server | the OTLP gRPC exporter (`telemetry.Init`) and net/http's HTTP/2 client |
| `istio.io/istio` 20250506 | GO-2026-5363 — SSRF via `RequestAuthentication` jwksUri | package-init linkage only (see below) |
| `istio.io/istio` 20250506 | GO-2026-5289 — `AuthorizationPolicy` serviceAccounts regex injection | package-init linkage only |

The first two are genuinely reachable. The two istio findings are **not
exploitable in this binary**: `pkg/route/translate` feeds an in-memory
`ConfigGenerator` with Gateway and VirtualService configs only — it never
receives a `RequestAuthentication` or `AuthorizationPolicy`, never serves xDS,
and never fetches a JWKS. govulncheck flags them because linking istiod's
package graph runs its `init` functions; the reported traces are literally
`translate.init calls core.init, which calls authn.init`, and one claims
`promql.Client.Instant calls agent.Validation.Error`.

Suppressing the two istio findings via an allowlist was considered and
rejected — see design D1.

## What Changes

- **`golang.org/x/text` → v0.39.0** and **`google.golang.org/grpc` → v1.82.1**
  (both indirect; version bumps only).
- **`istio.io/istio` → v0.0.0-20260410004459-189832a289c1**, the first snapshot
  carrying both istio fixes, which forces **`istio.io/api` →
  v1.29.0-alpha.0.0.20260327042620-ea30db2515c3** and `k8s.io/*` → v0.35.3.
  There is no tagged release line for `istio.io/istio` to target instead.
- **The scoped translation environment gains a lifecycle.** istio moved
  VirtualService merging out of `PushContext` into a krt-backed
  `VirtualServiceController` that `InitContext` now dereferences
  unconditionally. krt collections are populated by event delivery rather than
  read straight from the store, so the translator can no longer assemble a
  purely static world: `buildScopedEnv` now creates the configs, starts the
  config-store and VirtualService controllers, **waits for them to report
  synced**, and returns a cleanup that reclaims their goroutines when the
  translation returns. Gateways are still listed directly from the store.
- **The ServiceEntry registry is dropped.** It was carried over from istiod's
  own test harness and never consulted: this translator's configs are Gateway +
  VirtualService only, and every backend Service arrives through
  `ScopedInput.Services`. Constructing one now requires a
  `*multicluster.Controller`, i.e. Kubernetes clients — forbidden here (design
  D0) — so the unused registry is removed rather than faked.
- `memory.NewSyncController` is gone upstream; the store is built with
  `memory.NewController`.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `static-analysis-suite`: "govulncheck on every PR" — the repository carries no
  standing suppression list, and an unreachable finding is still resolved by
  upgrading, so a later finding in the same module still fails the job.

No other capability changes: the route engine resolves the same destinations
from the same configuration. This is a dependency and internal-lifecycle change.

## Impact

- **Modified**: `go.mod` / `go.sum`, `pkg/route/translate/translate.go`.
- **Behaviour**: none intended. `pilot/pkg/model.ResolveShortnameToFQDN` — which
  the bare-short-name destination resolution depends on — is byte-identical at
  the target commit, and the `-tags oracle` sweep (whose expected clusters are
  computed by construction, independently of istiod) passes unchanged.
- **Performance**: each translation now starts and stops two controllers and
  waits for krt sync, where it previously started nothing. Measured against the
  existing per-resolution cost this is small — one `router_check_tool` fork/exec
  already dominates at ~50–60 ms — but it is no longer zero.
- **Dependencies**: no new direct dependency. `k8s.io/*` moves to v0.35.3 as a
  transitive consequence; it remains linked-only, and
  `make check-route-containment` still passes.
