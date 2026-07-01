# Tasks

Server-side service-graph resolution change; implemented test-first against
`pkg/build/servicegraph_test.go`.

## 1. Query selector (`pkg/promql/queries.go`)

- [ ] 1.1 Split `serviceGraphSentinelSelector` into an independent client matcher
      (`client!~"user|unknown"`, unchanged) and a narrowed server matcher (`server!~"user"`).
      Update the doc comment above the constant to explain the split and point at D1/D30.
- [ ] 1.2 `pkg/promql/queries_test.go` (or equivalent): assert the rendered selector string
      for `QServiceGraphTotal` reflects the split matchers.

## 2. Resolver (`pkg/build/servicegraph.go`)

- [ ] 2.1 In `resolveServer`, before falling through to `resolveEmptyUID` (empty-UID path)
      or the synth-pod fallback (non-empty, unresolved UID path), add a branch: if the raw
      `server` label is exactly `"unknown"` AND no real topology pod was found, dispatch to
      the new `resolveUnknownServerPeer` helper instead — never to `resolveEmptyUID` or the
      synth-pod path for this literal value.
- [ ] 2.2 Add `resolveUnknownServerPeer`: takes whether the client side resolved to a real
      topology pod, the client pod's own cluster + namespace (when resolved), and the raw
      `client_net_peer_name` / `client_server_address` series labels. Returns `nil`
      (dropped) when the client did not resolve to a real pod, or when neither label is
      present.
- [ ] 2.3 Add the peer-address classification helper: strip an optional `:<port>` suffix
      (best-effort `net.SplitHostPort`, fall back to the unstripped value on failure), run
      the existing `classifyK8sDNS`, and — new — treat a single dot-free non-IP-literal
      label as a bare short Service name scoped to the client pod's own namespace.
- [ ] 2.4 Wire a successful classification into the existing `resolveServiceLevel(anchorCluster,
      ns, svc)` (anchor = the client pod's own cluster — no anchor-recovery fallback chain
      needed here). Wire an unresolvable classification, or a `resolveServiceLevel` miss,
      into the existing `sgResolver.external(rawValue)` helper (using the RAW, unstripped
      peer-address value, not the port-stripped host).
- [ ] 2.5 Extend `sgTrace` / debug logging as needed so a peer-label enrichment hit/miss is
      distinguishable in `noteExternal` reasons and debug logs (a new reason string,
      analogous to `missing_uid_nonurl_label` / `anchor_cluster_lacks_service`).

## 3. Tests (`pkg/build/servicegraph_test.go`)

- [ ] 3.1 `TestParseServiceGraph_UnknownServerPeerLabel_NetPeerNameResolvesService` — client
      resolved, `client_net_peer_name` is a 2-label `.svc` name held by the anchor cluster →
      `pod-calls-service` edge + `service-selects-pod` fan-out.
- [ ] 3.2 `TestParseServiceGraph_UnknownServerPeerLabel_ServerAddressFallback` — `client_net_peer_name`
      absent, `client_server_address` (with a port suffix) used instead; same resolution as 3.1.
- [ ] 3.3 `TestParseServiceGraph_UnknownServerPeerLabel_BareShortName` — bare single-label
      value resolves within the client pod's own namespace.
- [ ] 3.4 `TestParseServiceGraph_UnknownServerPeerLabel_ExternalAddress` — multi-label
      non-`.svc` value → `external/<value>` node, `pod-calls-pod` edge.
- [ ] 3.5 `TestParseServiceGraph_UnknownServerPeerLabel_AnchorLacksService` — classifies to a
      valid `(ns, svc)` but the anchor cluster doesn't hold it → external, not dropped.
- [ ] 3.6 `TestParseServiceGraph_UnknownServerPeerLabel_NeitherLabelPresent_Dropped` — both
      labels empty/absent → no node, no edge.
- [ ] 3.7 `TestParseServiceGraph_UnknownServerPeerLabel_ClientUnresolved_Dropped` — client
      side does not resolve to a real pod, peer label present → still dropped (enrichment
      does not apply).
- [ ] 3.8 `TestParseServiceGraph_UnknownServerPeerLabel_ServerUIDPresentButUnresolved` —
      non-empty, topology-miss `server_k8s_pod_uid` alongside `server="unknown"` still goes
      through enrichment, not the synth-pod fallback.
- [ ] 3.9 Regression: an existing test asserting the old combined `server!~"user|unknown"`
      selector string is updated for the split matchers.

## 4. Integration (`internal/integration`)

- [ ] 4.1 One fixture-driven test against the real VictoriaMetrics testcontainer: ingest a
      `server="unknown"` series with `client_net_peer_name` set and a resolvable client pod,
      confirm the loosened selector actually returns the series from VM and the graph
      contains the expected `pod-calls-service` edge end-to-end.

## 5. Docs

- [ ] 5.1 Update `CLAUDE.md`'s D30 bullet: narrow the server-side sentinel scope and add a
      pointer to the new peer-label enrichment rule (state the "every other case still
      drops" invariant explicitly).

## 6. Verify

- [ ] 6.1 `go test ./pkg/build/... ./pkg/promql/... -run TestParseServiceGraph -v` and
      `-run TestQuer` (selector rendering) green.
- [ ] 6.2 `go vet ./...` clean; `make lint` 0 issues.
- [ ] 6.3 Affected integration subset green against a real VM (the new fixture from 4.1,
      plus existing service-graph / sentinel-exclusion integration tests for regressions).
- [ ] 6.4 Full `make test` (race + shuffle + Docker integration) green.
- [ ] 6.5 `openspec validate "resolve-unknown-server-peer-labels"` passes.
