# Tasks: node-internal-ip-fallback

## 1. Query layer

- [x] 1.1 Widen `QNodeAddresses` render in `pkg/promql/queries.go` to `{type=~"ExternalIP|InternalIP"}`; update `pkg/promql/queries_test.go` selector + prefix expectations

## 2. Parse layer

- [x] 2.1 Rework node-address pick in `pkg/build/topology.go` `parseTopology`: per-`(cluster, node)` per-type lexically-smallest, ExternalIP beats InternalIP, other types ignored
- [x] 2.2 Unit tests in `pkg/build`: InternalIP-only fallback, both-present ExternalIP wins (order-independent), no rows → no IP, duplicate-sample determinism within each type, non-IP `type` rows ignored

## 3. API surface verification

- [x] 3.1 Component test in `internal/api`: node `data.ipaddress` carries InternalIP fallback; `labels.internal_ip` / `labels.external_ip` never emitted; refresh goldens only if fixtures change
- [x] 3.2 Integration fixture in `internal/integration`: add InternalIP series (one node InternalIP-only, one node both) and assert emitted `ipaddress`

## 4. Docs / spec sync

- [x] 4.1 Update CLAUDE.md `ipaddress` load-bearing rule (K8sNode: ExternalIP, falls back to InternalIP) and `pkg/graph/node.go` doc comment
- [x] 4.2 Run `make build vet lint test`; `openspec validate "node-internal-ip-fallback"` (`openspec verify` is not a subcommand in this CLI version — `validate` is the equivalent)
