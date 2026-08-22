Adds one classification step to the unknown-server peer-label enrichment: an IP-literal
peer address that matches no Service `ClusterIP` is looked up against a new
`(cluster family, Pod IP) -> holders` index and, on an unambiguous hit, resolves directly
to that topology pod (`pod-calls-pod`, no service node, no fan-out) — including across a
cluster boundary within the family. No new PromQL query, no new node/edge type.

## 1. Reverse index (`pkg/build/servicegraph.go`)

- [x] 1.1 Add `famIPKey{family, ip}` and `podIPCandidate{cluster, pod}` types, plus the
      `podIPCandidates map[famIPKey][]podIPCandidate` field on `sgResolver`.
- [x] 1.2 Stage 1 in `newSGResolver`, inside the existing `topology.Pods` loop that builds
      `podByID`: reduce to `perCluster map[ipKey]*graph.PodNode` — cluster from
      `p.Labels()["cluster"]`, IP from `p.IPAddress()`, skipping a pod whose IP slice is
      empty or whose first element is `""` (design D2).
- [x] 1.3 On a duplicate `(cluster, ip)`, keep the lexically-smallest `pod.ID()`
      (design D3), mirroring the `ipIndex` duplicate handling.
- [x] 1.4 Stage 2: regroup the per-cluster winners by
      `famIPKey{ClusterFamilyKey(cluster), ip}`, each group sorted by cluster — the same
      shape and `sort.Slice` as the `svcCandidates` build (D6).
- [x] 1.5 Add `lookupPeerPodIP(anchorCluster, host) (*graph.PodNode, bool)` — anchor
      holder wins, else lone family holder, else no pod with `ambiguous=true`. Shared by
      the parse and the prescan.
- [x] 1.6 Rewrite the `ipKey` doc comment: it no longer claims the Pod-IP rationale is
      "identical and stronger"; it now records why the ClusterIP lookup stays
      anchor-only (overlapping Service CIDRs, and the same Service carrying a different
      ClusterIP per cluster under multi-primary).

## 2. Resolver ladder (`pkg/build/servicegraph.go`)

- [x] 2.1 In `resolveUnknownServerPeer`, inside the `!classified` +
      `net.ParseIP(host) != nil` sub-branch, consult `lookupPeerPodIP` BEFORE
      `routeExternal("unknown_server_peer_ip_literal_no_match", ...)` (design D1).
- [x] 2.2 On a hit return `[]string{pod.ID()}` directly — no `resolveServiceLevel`, no
      service node, no `service-selects-pod` edge (design D5).
- [x] 2.3 Emit a `slog.Debug` on the hit carrying both `anchor_cluster` and
      `pod_cluster`, so a cross-cluster resolution is visible in the logs.
- [x] 2.4 On `ambiguous`, degrade through `routeExternal` with the distinct reason
      `unknown_server_peer_pod_ip_ambiguous` (feeds the existing `extReasons` tally); no
      tie-break, no node beyond the usual external one.
- [x] 2.5 Leave `classifyPeerHost`, `classifyK8sDNS`, `classifyBareShortName` and the
      `ipIndex` step untouched — ClusterIP priority comes from ordering alone.
- [x] 2.6 Update the `resolveUnknownServerPeer` doc comment to describe the new step, its
      family scoping and its position in the ladder.

## 3. Prescan (`pkg/build/routeprescan.go`)

- [x] 3.1 In `collectRouteQueriesWith`, after the existing
      `classifyPeerHost` / `anchorHolds` skip, skip keys whose host `lookupPeerPodIP`
      resolves to a pod (design D4).
- [x] 3.2 Comment why: the route engine is consulted only where the parse would fall
      external — so an ambiguous family deliberately does NOT skip.

## 4. Tests (`pkg/build/servicegraph_test.go`, `pkg/build/routeprescan_test.go`)

- [x] 4.1 Fixtures: `podIPPod` constructor, `sampleTopologyPodIPFamily(extra...)` over the
      prod-1 / prod-2 family, and `reversePods` for load-order independence.
- [x] 4.2 IP literal matching a pod's `pod_ip` in the anchor cluster resolves to that
      pod: one `pod-calls-pod` edge, `target == "<cluster>/<uid>"`, no service node, no
      external node, no `service-selects-pod` edge.
- [x] 4.3 Same with a `:8080` suffix on the peer value — identical result.
- [x] 4.4 An address held by BOTH a Service `ClusterIP` and a pod `pod_ip` resolves to
      the Service (`pod-calls-service`) — ClusterIP priority.
- [x] 4.5 A lone family sibling holding the address resolves across the cluster boundary,
      with `labels.cluster` still the client side.
- [x] 4.6 The anchor cluster's holder wins when a family sibling carries the same address.
- [x] 4.7 Two family siblings holding the address degrade to `external/<ip>` with no edge
      to either.
- [x] 4.8 A holder in a different family (`staging-1` vs `prod-1`) is never matched.
- [x] 4.9 Cases 4.5-4.7 re-run with `topology.Pods` reversed — same outcome.
- [x] 4.10 Two anchor-cluster pods sharing one IP resolve to the lexically-smallest pod
      id in both load orders; the intra-cluster duplicate does not make the family
      ambiguous.
- [x] 4.11 A pod with an empty `pod_ip` is not indexed — the endpoint falls to
      `external/<ip>`.
- [x] 4.12 No match anywhere still falls to `external/<ip>` (regression guard for the
      existing behaviour).
- [x] 4.13 Prescan: an endpoint resolvable via a lone family holder produces no
      `routeKey`, while an ambiguous-family endpoint still does.

## 5. Integration (`internal/integration`)

- [x] 5.1 `TestUnknownServerPodIPPeerResolvesToPod`: ingest a backend pod's
      `kube_pod_info{cluster="cluster-alpha",...,pod_ip="10.244.1.9"}` plus two
      `traces_service_graph_request_total` samples with `server="unknown"`,
      `server_k8s_pod_uid=""`, `client_server_address="10.244.1.9"`.
- [x] 5.2 Assert the response contains a `pod-calls-pod` edge targeting the backend pod's
      cluster-scoped id, and does NOT contain `external/10.244.1.9`.
- [x] 5.3 `TestUnknownServerPodIPPeerResolvesAcrossFamily`: a `prod-1` caller and a
      `prod-2` backend pod carrying `pod_ip="10.244.9.9"`; assert the edge runs
      `prod-1/xfam-1 -> prod-2/xfam-2` and no `external/10.244.9.9` appears.

## 6. Docs

- [x] 6.1 Extend the "Unknown-server peer-label enrichment" bullet in `CLAUDE.md` with
      the Pod-IP step, its family scoping and selection rules, the ClusterIP-first
      ordering, and the "no service node / no fan-out" consequence.

## 7. Verify

- [x] 7.1 `make test` passes (`-race -shuffle=on`).
- [x] 7.2 `make lint` and `make vet` clean.
- [x] 7.3 `make check-route-containment` passes.
- [x] 7.4 `go test ./internal/api/ -run TestGolden` passes WITHOUT `-update` — no golden
      file changes.
- [x] 7.5 `openspec validate "resolve-unknown-server-pod-ip-peer"` passes.

## Archive ordering (both changes MODIFY "Unknown-server peer-label enrichment")

`resolve-unknown-server-pod-ip-peer` and `translate-global-fqdn-to-k8s-service` both carry a
MODIFIED block for the SAME `pod-service-graph` requirement, and each was rebased onto the
CURRENT promoted spec — which today has neither the Pod-IP stage nor the route-resolution
hook. A MODIFIED requirement replaces the whole block, so archiving them naively would let
the second one silently drop the first one's content.

Whichever archives FIRST is fine. Before archiving the SECOND, re-rebase its MODIFIED block
onto the then-promoted requirement (copy the promoted block, re-apply this change's own
additions, keep its own scenarios). `openspec validate` catches the omission if this is
skipped — that is exactly how this defect was found.
