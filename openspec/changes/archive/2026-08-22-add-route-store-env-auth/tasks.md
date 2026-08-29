# Tasks: add-route-store-env-auth

## 1. Config

- [x] 1.1 Add `RouteStoreUsername` / `RouteStorePassword` fields to `internal/config.Config` with doc comments stating env-only sourcing and the no-flag rationale
- [x] 1.2 Read `KSG_ROUTE_STORE_USERNAME` / `KSG_ROUTE_STORE_PASSWORD` in `applyEnv`; register NO flags for them
- [x] 1.3 Pair validation in `Validate()`: exactly one set → error naming both env vars, never echoing values
- [x] 1.4 Update `--route-store-dsn` help example to credential-free form; cross-reference env creds
- [x] 1.5 Config unit tests: env parsing, both-set OK, neither-set OK, half-set fails, unknown credential flags rejected

## 2. Route store Open

- [x] 2.1 Refactor `Option` to collect into `openConfig` before dial; keep `WithUniqueRows` factory
- [x] 2.2 Add `WithAuth(username, password string) Option`; apply after `ParseDSN` before `clickhouse.Open`
- [x] 2.3 Unit tests for `applyAuth` override / no-op and option factories (no live CH)

## 3. Wiring

- [x] 3.1 `cmd/kube-state-graph/main.go`: append `WithAuth` when `RouteStoreUsername` is non-empty; log boolean `route_store_auth` only

## 4. Integration

- [x] 4.1 `RouteStoreSuite`: Open with credential-free DSN + `WithAuth` succeeds; same DSN without auth fails
- [x] 4.2 Test gated by existing Docker skip on the suite

## 5. Docs & verification

- [x] 5.1 OpenSpec proposal / design / tasks / delta specs
- [x] 5.2 Unit tests for config + store pass (`go test ./internal/config/ ./pkg/route/store/`)
