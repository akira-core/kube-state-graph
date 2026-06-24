# Tasks: add-prom-basic-auth

## 1. Config

- [x] 1.1 Add `PromUsername` / `PromPassword` fields to `internal/config.Config` with doc comments stating env-only sourcing and the no-flag rationale (D-A1)
- [x] 1.2 Read `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD` in `applyEnv`; register NO flags for them
- [x] 1.3 Pair validation in `Validate()`: exactly one set (non-empty) → error naming both env vars, never echoing values (D-A2)
- [x] 1.4 Config unit tests: env parsing, both-set OK, neither-set OK, half-set fails with env-var names in message and no value leakage, unknown `--prom-username`/`--prom-password` flags rejected by flag parsing

## 2. promql client

- [x] 2.1 Add `Option` type and `WithBasicAuth(username, password string) Option` to `pkg/promql`; change `New` to `New(promURL string, metrics Metrics, opts ...Option)` (D-A3)
- [x] 2.2 Implement `basicAuthTransport` RoundTripper (clone request, `SetBasicAuth`, delegate); wire chain `otelhttp → basicAuth → base` (D-A4)
- [x] 2.3 Unit test `basicAuthTransport` with a fake inner RoundTripper (no socket): header present and correct with auth, absent without, original request not mutated
- [x] 2.4 Verify no log/span/error path in `pkg/promql` can emit credentials (D-A5) — review `Instant` logging and `New` error wrapping

## 3. Wiring

- [x] 3.1 `cmd/kube-state-graph/main.go`: pass `promql.WithBasicAuth(cfg.PromUsername, cfg.PromPassword)` when both are non-empty

## 4. Integration

- [x] 4.1 Extend the testcontainers VictoriaMetrics helper with an option to start the container with `-httpAuth.username` / `-httpAuth.password`; fixture ingestion must authenticate too
- [x] 4.2 Plumb upstream credentials into `StartAPIServer` / `vmsuite.go` (uses the new `promql.New` options)
- [x] 4.3 Integration test: credentialed build succeeds end-to-end against auth-enabled container (`/v1/graph` 200 with expected elements)
- [x] 4.4 Integration test: unauthenticated server against the same auth-enabled container → upstream-failure error mapping, no credential text in response
- [x] 4.5 Both tests gated by `SkipIfDockerUnavailable`

## 5. Docs & verification

- [x] 5.1 README / configuration docs: document `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD`, env-only rationale, Secret `secretKeyRef` example, restart-to-rotate note
- [x] 5.2 Run `make test`, `make vet`, `make lint`; confirm golden tests untouched
- [x] 5.3 `openspec validate add-prom-basic-auth` passes
