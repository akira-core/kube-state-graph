Marks span-link logical edges (`edge_relation="link"`) with
`labels.relation="link"` and their pod→broker transport edges with
`labels.relation="transport"`, via a lookup-only mirror of the unknown-server
classification chain. No new node/edge type, no PromQL change, no edge-ID
change.

## 1. Registry (`pkg/graph/registry.go`)

- [x] 1.1 Add a `relation` entry (`ValueType: "string"`, description covering
      `link` / `transport` / absent) to the `Labels` array of `pod-calls-pod`
      and `pod-calls-service`. `service-selects-pod` unchanged.
- [x] 1.2 Regenerate `internal/api/testdata/golden/edge-types.json`
      (`go test ./internal/api/ -update -run Golden`).

## 2. Prescan (`pkg/build/routeprescan.go`)

- [x] 2.1 `serverPeerLabelsOf(m model.Metric) peerLabels` — the mirrored
      server-side constructor (4 `server_*` labels; `networkPeerAddress` /
      `netPeerPort` left empty).
- [x] 2.2 Extract the existing skip chain (`peerRouteKey` → classified +
      `anchorHolds` skip → `lookupPeerPodIP`-hit skip → empty-`ips` skip) into
      `(*sgResolver).viaRouteKey(ownPod, peer) (routeKey, bool)`; the
      unknown-server branch calls it (behaviour byte-for-byte).
- [x] 2.3 Link branch in `collectRouteQueriesWith`: common preamble (labels,
      cluster bucket, self-loop normalise, wholly-empty skip) hoisted before
      the branches; `edge_relation=="link"` emits a via key per side whose own
      pod resolves (anchor = that pod's cluster), through `seen`, then falls
      through to the unknown-server branch.

## 3. Parse (`pkg/build/servicegraph.go`)

- [x] 3.1 Constants `relationLabelKey` / `relationLink` / `relationTransport`.
- [x] 3.2 `viaNodeID(ownPod, peer) (string, bool)` — lookup-only mirror of
      `resolveUnknownServerPeer` (classify → ServiceID; Pod IP → pod ID;
      else `routeNodeID`; never materialises, never logs reasons).
- [x] 3.3 `routeNodeID(key, raw) string` — lookup-only twin of
      `routeIndexResolve` (RouteHit / RouteIngressLBService with a
      dest-cluster membership hit → ServiceID(dest…), backend only, no role,
      no chain; everything else → ExternalID(raw)).
- [x] 3.4 `parseWithResolver`: read `edge_relation`; a link sample with an
      unresolvable-consumer server ("unknown" label, no topology pod)
      contributes NO markers (no-consumer guard); every other link sample
      records its pairs in `linkPairs` and its per-side via pairs in
      `transportPairs` (both function-local).
- [x] 3.5 Edge-build loop: `linkPairs` → `labels["relation"]="link"`, else
      `transportPairs` → `"transport"`; `svcEdges` / `routeChainEdges` never
      consulted; two aggregated `slog.Debug` tallies (no-consumer link
      series, unmatched transport pairs).

## 4. Unit tests

- [x] 4.1 `pkg/build/servicegraph_test.go` `TestParseServiceGraph_LinkRelation_*`:
      BothPodsResolve; LinkWinsOverPlainSeries (both orders);
      TransportMarksExistingBrokerEdge; LinkBeatsTransportOnCollision;
      ServerUnknown_PairMarkedTransport; ServerGhostExternal_KeepsLink;
      ServerSynthPod_KeepsLink; ClientUnresolved_NoClientVia_ServerViaStillMarks;
      ViaLookupNeverMaterialises; FanOutNeverMarked; SelfLoopLinkSeries;
      UnmatchedTransportPair_MarkerOnly; NonLinkRelationValueIgnored; a
      hand-built `routeIndex` case proving `routeNodeID` aligns with the
      materialising path's node ID.
- [x] 4.2 `pkg/build/routeprescan_test.go`: LinkSeries_EmitsBothViaKeys;
      DedupsWithPlainNetworkSeriesKey; viaRouteKey skip-chain cases;
      ServerUnknownLinkSeries_SingleKey; SameFQDN_MultipleSeries_SingleRouteKey;
      TestReadServiceGraph_SameFQDNResolvedOnce (fakeRouteResolver,
      `requests()` exactly one).

## 5. Golden + integration

- [x] 5.1 Golden fixture `link-relation-cytoscape.json` (link edge, transport
      edge, unmarked fan-out) + `edge-types.json` regen committed.
- [x] 5.2 `internal/integration/graph_e2e_test.go` `TestSpanLinkRelationEdges`:
      two-sample ingestion; link series + unknown-server network series +
      service/endpointslice topology; assert the three relation shapes.

## 6. Docs

- [x] 6.1 Add the load-bearing CLAUDE.md bullet for span-link relation
      marking.

## 7. Verify

- [x] 7.1 `make test` (`-race -shuffle=on`) passes.
- [x] 7.2 `make lint` and `make vet` clean.
- [x] 7.3 `openspec validate "add-span-link-logical-edges"` passes.
