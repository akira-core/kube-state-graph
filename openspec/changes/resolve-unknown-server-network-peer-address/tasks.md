# Tasks

Server-side service-graph resolution change, scoped entirely to
`resolveUnknownServerPeer` and its call chain. Implemented test-first against
`pkg/build/servicegraph_test.go`.

## 1. Resolver (`pkg/build/servicegraph.go`)

- [ ] 1.1 Read `client_network_peer_address` off the series in `parseServiceGraph`, next to
      the existing `client_net_peer_name` / `client_server_address` reads. Add a comment
      recording that `client_network_peer_port` is deliberately NOT read (design Non-Goals,
      3rd bullet + the spec's port-only scenario) so a later reader does not add it for
      symmetry. Update the existing comment above the reads: the three labels are distinct
      OTel attributes (`server.address` stable / `network.peer.address` stable /
      `net.peer.name` deprecated), not three spellings of one.
- [ ] 1.2 Widen `resolveServer` and `resolveUnknownServerPeer` by one `string` parameter for
      the new label (design D3 — plain parameters, no `peerLabels` struct). Declare the
      three peer parameters **in precedence order** — `clientServerAddress,
      clientNetworkPeerAddress, clientNetPeerName` — so the signature reads as the D1 rule
      and a transposition is visible at the call site. Update both call sites and the doc
      comments on each function.
- [ ] 1.3 Reorder the first-non-empty peer-value pick in `resolveUnknownServerPeer` to
      `client_server_address` → `client_network_peer_address` → `client_net_peer_name`
      (design D1). This **swaps the two existing labels** — doc-comment why (the chain is
      strong on names, weak on IPs; `net.peer.name` is deprecated in favour of
      `server.address`). "All three empty" still returns `nil` (dropped) unchanged. No
      fall-through: a non-empty higher-precedence label that fails to classify must NOT
      defer to a lower-precedence one.
- [ ] 1.4 Add a pure bracket-truncation helper — cut the value at the **first `[` whose
      index is > 0**, discarding that byte and everything after it; a value with no `[`, or
      whose only `[` is at index 0, is returned unchanged — and call it on the chosen peer
      value **before** `stripPeerAddressPort` (design D2). Doc comment must state both
      reasons: (a) `net.SplitHostPort` rejects a port segment containing `[`, and
      `classifyK8sDNS` performs no DNS-1123 validation, so today a bracketed value
      garbage-classifies into a namespace that can never match; (b) the index-0 guard exists
      because `net.SplitHostPort` **succeeds** on `[<ipv6>]:<port>` and returns the host
      without brackets, so an unconditional cut would truncate a resolvable dual-stack
      `ClusterIP` peer to the empty string.
- [ ] 1.5 Confirm the external fallback still passes the RAW, wholly unnormalised label
      value to `r.external(...)` — neither bracket-truncated nor port-stripped (design D4).
      This is a no-change assertion on existing code; verify it was not disturbed by 1.4.
- [ ] 1.5b Update the `stripPeerAddressPort` doc comment
      (`pkg/build/servicegraph.go:608-612`): it names only the two old labels, and needs the
      third. Its "either failure falls back to the raw, unstripped value" sentence is now
      where the bracket interaction lives — cross-reference the 1.4 helper and state that a
      bracketed IPv6 authority (`[<ipv6>]:<port>`) is the one bracketed shape this function
      handles correctly, which is why 1.4 guards index 0.
- [ ] 1.6 Confirm no stage below the pick branches on which label supplied the value
      (`noteExternal` reasons, debug logs, `sgTrace`) — the resolver stays provenance-free
      per `resolve-unknown-server-ip-peer` D4. The index-0 guard in 1.4 is keyed on the
      value's shape, not its source, so it does not violate this.

## 2. Unit tests (`pkg/build/servicegraph_test.go`)

- [ ] 2.1 `TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddressResolvesService` —
      only `client_network_peer_address` present, a 2-label `.svc` host held by the anchor
      cluster → `pod-calls-service` edge + `service-selects-pod` fan-out.
- [ ] 2.2 `TestParseServiceGraph_UnknownServerPeerLabel_ServerAddressWinsPrecedence` —
      all three labels non-empty, each addressing a *different* resolvable target in the
      anchor cluster → resolution targets the `client_server_address` Service only; neither
      other label contributes a node or an edge. This is the one test that fails on a
      parameter transposition (design D3), so keep all three values distinct.
- [ ] 2.2b `TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddressBeatsNetPeerName`
      — no `client_server_address`, the other two non-empty and addressing different
      resolvable Services → resolves from `client_network_peer_address`. Pins the middle
      slot independently of 2.2.
- [ ] 2.2c `TestParseServiceGraph_UnknownServerPeerLabel_NoFallThroughOnNonClassifying` —
      `client_server_address` holds an unresolvable multi-label host AND
      `client_network_peer_address` holds a host that WOULD resolve → the endpoint lands on
      `external/<server_address raw>`, not the Service. Pins "first non-empty wins
      outright" against a well-meaning fall-through refactor (design D1 alternative).
- [ ] 2.3 **(explicitly required)** `TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddressBracketSuffixResolves`
      — `client_network_peer_address` carrying a bracketed value in the observed
      `host:port[id]` shape (e.g. `payments.payments-ns.svc.cluster.local:27017[-181]`),
      with the Service held by the anchor cluster → the bracket suffix is truncated, then
      the port stripped, and the endpoint resolves to the Service node. Assert the resolved
      target id, NOT merely that no external node appeared.
- [ ] 2.4 **(explicitly required)** `TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddressBracketSuffixExternalKeepsRaw`
      — `client_network_peer_address="mongo.com:27017[-181]"` with no matching Service →
      `external/mongo.com:27017[-181]`, asserting the node `id` AND `name` retain the
      bracket suffix and the port verbatim, and the edge is `pod-calls-pod`.
- [ ] 2.5 `TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddressBracketDistinctExternals`
      — two series differing only in the bracketed identifier on the same host → two
      distinct external nodes (the accepted D4 cardinality consequence, pinned as intended
      behaviour rather than left to drift).
- [ ] 2.6 `TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddressBracketIPLiteral` —
      `client_network_peer_address="<clusterIP>:27017[-9]"` → bracket then port stripped,
      anchor-cluster `ClusterIP` lookup hits, resolves to that Service node.
- [ ] 2.7 `TestParseServiceGraph_UnknownServerPeerLabel_BracketSuffixOnNetPeerName` —
      regression guard for the uniform-truncation decision: a bracketed
      `client_net_peer_name` (no higher-precedence label) resolves the same way.
- [ ] 2.8 `TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerPortIgnored` — a series
      with `client_network_peer_port` set and all three peer-address labels empty → dropped,
      no node and no edge (guards the spec's explicit non-goal).
- [ ] 2.9 Verify the existing `TestParseServiceGraph_UnknownServerPeerLabel_*` cases still
      pass unchanged. None of them set `client_network_peer_address`, and no existing case
      carries two peer labels with non-empty values — `ServerAddressFallback` sets
      `client_net_peer_name: ""` explicitly, while `IPLiteralResolvesService`,
      `IPLiteralFamilySiblingNotMatched`, and `IPLiteralDuplicateClusterIP` omit the key
      from the `model.Metric` map (which reads as `""`). So both the new label and the D1
      reorder must be inert for them. If any case DOES need editing, stop — that means the
      reorder's blast radius is wider than proposal.md claims.
- [ ] 2.9b Add an IPv6 topology fixture for 2.10 / 2.11: `sampleTopologyWithServices()`
      carries only IPv4 `ClusterIP`s (`10.0.0.5`, plus two headless `"None"`), so add a
      `sampleTopologyIPv6()` helper (or an IPv6 entry to an existing helper) with a Service
      whose `ClusterIP` is an IPv6 literal in `cluster-alpha`. Fixture work, listed so it is
      visible in the plan rather than discovered mid-test.
- [ ] 2.10 `TestParseServiceGraph_UnknownServerPeerLabel_BracketedIPv6NotTruncated` — the
      D2 index-0 guard: `client_network_peer_address="[<ipv6 clusterIP>]:8080"` with a
      matching anchor-cluster `cluster_ip` → resolves to that Service node. This test FAILS
      if the helper is ever "simplified" to an unconditional first-`[` cut.
- [ ] 2.11 `TestParseServiceGraph_UnknownServerPeerLabel_BracketedIPv6WithIdentifierExternal`
      — `"[<ipv6>]:8080[-181]"` → no truncation (first `[` at index 0), host/port split
      fails on the trailing `[`, the value is then MATCHED by `classifyBareShortName` (it is
      dot-free and not an IP literal) as a candidate Service name, the Service lookup misses,
      → `external/[<ipv6>]:8080[-181]` with the raw value verbatim. Pins the accepted
      residual case in design D2. Assert the external node id/name; do NOT assert a
      `noteExternal` reason of `unknown_server_peer_not_k8s_dns` — the reason here is
      `unknown_server_peer_anchor_lacks_service`.
- [ ] 2.12 `TestParseServiceGraph_UnknownServerPeerLabel_TruncationPromotesToBareShortName`
      — `client_network_peer_address="payments:8080[-181]"` with the client pod (`checkout`,
      namespace `shop`) → resolves to `cluster-alpha/shop/payments`, using the `payments`
      Service already present in `sampleTopologyWithServices()` (no fixture change needed —
      `mongo` lives in namespace `db`, not the client's own `shop`, so it cannot be used for
      this scenario without extending 2.9b). Pins the design Risks entry "bracket truncation
      can promote a value into a bare-short-name hit" as intended (inherited) behaviour
      rather than an emergent surprise. Pair it with a no-such-Service case (e.g. a bare name
      not present in either namespace) asserting `external/<raw>` verbatim.

## 3. Integration (`internal/integration`)

- [ ] 3.1 Add one fixture-driven case to `internal/integration/graph_e2e_test.go`, alongside
      the existing `client_net_peer_name` enrichment fixture (~L1009): ingest a
      `server="unknown"` series carrying `client_network_peer_address` and a resolvable
      client pod, `WaitForSeries` on the ingested series, and assert the graph contains the
      expected `pod-calls-service` edge end-to-end.

## 4. Docs

- [ ] 4.1 Update the "Unknown-server peer-label enrichment" bullet in `CLAUDE.md`: three
      labels with the full precedence order `client_server_address` →
      `client_network_peer_address` → `client_net_peer_name` (**noting that this reorders
      the two pre-existing labels**), the one-line reason for that order (chain strong on
      names, weak on IPs; `net.peer.name` deprecated in favour of `server.address`), the
      bracket-truncation step ahead of port stripping (uniform across labels, first `[` at
      index > 0 only, IPv6 carve-out), the raw-verbatim external naming restated, and the
      `client_network_peer_port` non-goal. Note the two accepted consequences the bullet
      should make discoverable: truncation can reduce a value to a bare short name that
      resolves in the client's own namespace, and an IP-valued peer that is not an
      anchor-cluster `ClusterIP` becomes an `external/<ip>` node.

## 5. Verify

- [ ] 5.1 `go test ./pkg/build/... -run TestParseServiceGraph -v` green.
- [ ] 5.2 `go vet ./...` clean; `make lint` reports no NEW issues (confirm any finding
      predates this change via `git stash`).
- [ ] 5.3 Full `make test` (race + shuffle) green locally. The Docker-gated integration
      suite skips without a daemon — record that explicitly rather than implying it ran.
- [ ] 5.4 Integration subset from 3.1 green against a real VictoriaMetrics container, or
      explicitly deferred to CI with the reason stated.
- [ ] 5.5 `openspec validate "resolve-unknown-server-network-peer-address"` passes.
