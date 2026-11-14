# Design

## Context

See proposal.md, under Why. The current state:

- `resolveConnString` (`pkg/build/servicegraph.go`) handles a `"://"` label whose side has no pod UID. It parses the host with `url.Parse` (`connStringHost`), classifies it with `classifyK8sDNS`, and asks `resolveServiceLevel` for a Service node in the anchor cluster. Three outcomes fall back to an `external` node: `conn_host_unparseable`, `conn_host_not_k8s_dns` and `anchor_cluster_lacks_service`. All three call `r.external(label)`, which sets `id = graph.ExternalID(label)` and `name = label`.
- `r.external` is shared with two other fallbacks. One is the missing-UID human-label fallback (a non-`"://"` label). The other is the unknown-server peer ladder (`external/<raw_peer_address_value>`). `routeNodeID`, the lookup-only twin used for span-link `transport` marking, derives `graph.ExternalID(raw)` independently and must keep matching the peer ladder's id.
- Every per-series Debug record names the series through `sgTrace` (`client`, `server`, `label`). `sgTrace` is built once per series in `parseWithResolver` and is also read by the route-engine Debug sites in `routeprescan.go`. Three more sites log a local label: the wholly-empty-side drop, `noteConnStringShadowed`, and the `:// resolved to service node` line.
- The spec (`pod-service-graph`, "Unresolvable connection string falls back to an external node") requires the external `name` to be the label "verbatim, no normalisation, no trimming", and it does not discuss credentials.

## Goals / Non-Goals

**Goals:**

- No URL userinfo from a connection-string label reaches a response body, whether as an `external` node's `id`, its `name`, or an edge endpoint.
- No URL userinfo from a connection-string label reaches a per-series Debug record of the service-graph parse.
- Every label that carries no userinfo produces the same bytes as before, so no golden and no unaffected node id moves.
- Resolution is unchanged.

**Non-Goals:**

- Secrets outside the authority: a query string (`?password=`), a path, a fragment.
- Values that are not `"://"`-delimited, such as peer addresses and human labels.
- A credential containing an unescaped `/`, `?` or `#`. That is not a valid URI authority (RFC 3986 requires them percent-encoded), and `url.Parse` draws the same boundary.
- Redacting the series in the TSDB, or in any log outside this parse.

## Decisions

### D1. Remove the userinfo entirely rather than mask it

The stripped form drops `user:password@` (or `user@`) completely: `mongodb://app:pw@h:27017/db` becomes `mongodb://h:27017/db`.

- *Alternative: `url.URL.Redacted()` (`mongodb://app:xxxxx@h:27017/db`).* Rejected. The masked form keeps the username, which is often sensitive too (it names a service account). Its placeholder tells a reader that a secret was there. And because the id still embeds the username, an endpoint whose username differs per pod or per rotation still splits into several nodes. With full removal the id is a function of the endpoint alone.
- *Alternative: hash the userinfo into the id.* Rejected. The id would then still change on every rotation, and the hash invites an offline guess of a weak password.

A consequence of D1 is intended. Two labels differing only in their userinfo collapse onto one `external` node, and their series sum onto one edge. That is the same endpoint, and the graph is a picture of endpoints, not of credentials. Determinism holds because the node's `name` is the stripped label, so it is a pure function of the label and does not depend on which series arrives first.

### D2. A string scan, not `url.Parse`

`stripConnStringUserinfo` finds each `"://"`. It takes the authority as running up to the first `/`, `?` or `#`, and cuts through the LAST `@` inside it. Every other byte is returned unchanged.

- *Alternative: `url.Parse` and re-serialise without `User`.* Rejected for three reasons:
  - An opaque double-scheme label (`jdbc:postgresql://app:pw@h/db`) parses with `Opaque` set and no `User`, so its credentials would pass through. That label shape is exactly the `conn_host_unparseable` fallback.
  - A password holding an invalid percent-escape makes `url.Parse` fail outright, so that label would pass through too.
  - Re-serialising normalises labels that carry no credential (escaping, case), which changes ids that have nothing to hide.
- Cutting at the last `@` keeps an unescaped `@` inside a password on the userinfo side, which is the common real-world mistake. An `@` in a path, query or fragment is outside the authority and is kept, so `https://api.example/users/@me` is untouched.
- Every `"://"` occurrence is processed, so a fallback URL nested in a query string loses its userinfo as well.
- A label with no `@` in any authority is returned without an allocation. The helper runs on both labels of every series (D4), so the fast path matters.

### D3. Strip at the connection-string fallbacks only, not inside `r.external`

The three `r.external(label)` calls in `resolveConnString` pass `stripConnStringUserinfo(label)`. `r.external` itself still takes its argument verbatim.

- *Alternative: strip inside `r.external`, so that every external node is covered.* Rejected. The peer ladder and its lookup-only twin `routeNodeID` must derive the same id for the same raw value, or a span-link `transport` marker silently stops matching its edge. Stripping in `r.external` would force the same change into `routeNodeID` and would contradict the many peer-ladder scenarios that require `external/<raw_peer_address_value>`. Peer values are host or host:port by OTel convention and never `"://"` strings, so the narrower scope leaves no realistic gap.
- `connStringHost` still reads the raw label, so the host and therefore resolution are unchanged. The stripped value is never written back into the series' `Metric` map, because cached vectors are shared and must not be mutated.

### D4. Logs: strip the series identity once, where it is built

`parseWithResolver` builds `sgTrace` from `stripConnStringUserinfo(clientLabel)` and `stripConnStringUserinfo(serverLabel)`. `sgTrace` is read only by Debug sites, and it never influences resolution. Every record that names the series is covered from that single point, including the route-engine records in `routeprescan.go`, which this change does not otherwise touch. The three sites that log a local label (the wholly-empty-side drop, the shadowed-by-UID line, and the resolved-to-service line) strip it at the call.

- *Alternative: strip at each Debug call.* Rejected. A future Debug site that reads `t.clientLabel` would leak by default.
- The test scans the whole captured Debug output of each scenario for the secret, not a single message, so a new leaking site fails it.

### D5. Per-series Debug argument construction is gated on Debug being enabled (audit #26)

This is in the same branch, but it is not a contract change. The per-series Debug sites in `servicegraph.go` built their argument slices, and boxed every attribute, on every series even when Debug is off. They now first ask `slog.Default().Enabled(ctx, slog.LevelDebug)`, the same question `slog.Debug` itself asks. The helper is `debugEnabled`. `noteExternal` is split into a counter (`countExternal`), which stays unconditional because it feeds the Info summary and the filtered-build rollback journal, and a logger (`logExternal`). The hot call sites in this file gate the logger and its arguments. `noteExternal` stays for the route-engine call sites in `routeprescan.go`, and it gates its own record.

- *Alternative: cache the answer on the resolver at construction.* Rejected. The resolver literal is shared with a parallel change, and one `Enabled` call is an atomic load plus a level compare.
- Measured with `BenchmarkParseServiceGraph_DebugOff` (6000 series spread over every per-series Debug path, Apple M3 Max, 5 runs). Before: about 80,236 allocs/op, 4.86 MB/op, about 4.1 ms/op. After: 28,231 allocs/op, 2.08 MB/op, about 3.6 ms/op. The after numbers already include D4's stripping. With Debug on, the count rises by about 1,000 (85,245 to 86,241), which is the one stripped copy each credentialled label in the vector now costs.
- Limitation: under the OTLP fan-out handler, `Enabled` currently answers true whenever the bridge is installed (audit C4), so the gate takes effect there only once C4 lands.

### D6. Audit #24 (per-series service fan-out) is kept

Every series that resolves to a Service re-runs that Service's `service-selects-pod` fan-out. The work is idempotent (`svcEdges` dedupes), and memoising it per Service would need its own rollback in a filtered build: a memo entry recorded by a series that is later rolled back must be undone with it. That requires a new `journalKind`, plus a test of rollback ordering against `jService` and `jSvcEdge`. Given the cost (a map walk per series, bounded by one Service's endpoints) that is not worth it now, so the behaviour is kept.

## Risks / Trade-offs

- [An external node whose label carried userinfo changes `id` and `name`, and its edges change their UUIDv5 ids] → This is intended and confined to those nodes. It is documented in `docs/BREAKING.md`. A client that pinned such an id was pinning a credential.
- [Credentials elsewhere in a label survive] → These are listed as non-goals. A query-string secret has no syntactic marker that would be safe to strip without guessing.
- [Two endpoints that differ only in credentials can no longer be told apart] → This is accepted (D1). They are one network endpoint.

## Migration Plan

None is needed for deployments whose connection-string labels carry no userinfo, because their bodies are byte-identical. Rollback is a revert.
