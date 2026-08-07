# Tasks

Clears the four `govulncheck` findings that have kept CI's `vuln` job red. No
capability behaviour changes, so the work is a dependency bump plus the internal
adaptation istio's newer API requires — and the validation carries the weight.

## 1. Dependency bumps

- [x] 1.1 `golang.org/x/text` → v0.39.0, `google.golang.org/grpc` → v1.82.1
      (both indirect).
- [x] 1.2 `istio.io/istio` → v0.0.0-20260410004459-189832a289c1 and
      `istio.io/api` → v1.29.0-alpha.0.0.20260327042620-ea30db2515c3, then
      `go mod tidy`. Leave the `go` / `toolchain` directives alone.

## 2. Adapt the scoped translation environment

- [x] 2.1 `memory.NewSyncController` → `memory.NewController` (removed upstream;
      the memory store is krt-backed now and no longer has a sync variant).
- [x] 2.2 Drop the ServiceEntry registry (design D3) — it was never consulted and
      constructing one now needs Kubernetes clients.
- [x] 2.3 Wire `model.NewVirtualServiceController` into `env`, and reorder
      `buildScopedEnv` so configs are created BEFORE the controllers are built
      (design D2).
- [x] 2.4 Start the config-store and VirtualService controllers, wait for
      `HasSynced` with a bounded poll, and return a cleanup that `Translate`
      defers — without it each translation leaks ~115 goroutines.
- [x] 2.5 Correct the package and `buildScopedEnv` doc comments: the world is no
      longer static and no longer goroutine-free.

## 3. Verification

- [x] 3.1 `go build ./... && go vet ./...`, `make lint`
- [x] 3.2 `go test ./pkg/... ./internal/api/ ./internal/config/ -count=1 -race`
      — including `TestTranslatorNoGoroutineLeak` and the bare-short-name
      destination test that rests on `ResolveShortnameToFQDN`
- [x] 3.3 **`make vuln` → "No vulnerabilities found"** (the change's whole point)
- [x] 3.4 `make check-route-containment` after the `k8s.io/*` move
- [x] 3.5 `-tags oracle` sweep in a Linux container (design D5): PASS at 20×10
      and at 200×50 (10,000 VirtualServices), zero mismatches
- [x] 3.6 `make check-docs`, `make verify-mocks`
- [x] 3.7 Re-read `CLAUDE.md`'s dependency paragraph against the new pins
- [x] 3.8 `openspec validate upgrade-vulnerable-dependencies`
- [x] 3.9 Real GitHub Actions run: **`vuln` green**, `test` still green
      (`internal/integration` complete, zero SKIPs — the istio bump would show up
      here as host matching, listener selection, or RDS naming drift),
      `route-containment`, `lint`, `docs-drift`, `mocks-drift` all green
- [x] 3.10 Note on PR #7: what moved, why the two istio findings were not
      exploitable yet still fixed by upgrading, and the oracle-sweep result
