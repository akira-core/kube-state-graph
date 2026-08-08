Adds one classification step to the unknown-server peer-label enrichment: an IP-literal
peer address that matches no Service `ClusterIP` is looked up against a new
`(anchor cluster, Pod IP) -> pod` reverse index and, on a hit, resolves directly to that
topology pod (`pod-calls-pod`, no service node, no fan-out). No new PromQL query, no new
node/edge type.

## 1. Reverse index (`pkg/build/servicegraph.go`)

- [x] 1.1 Add `podIPIndex map[ipKey]*graph.PodNode` to `sgResolver`, documented as the
      Pod-IP analogue of `ipIndex` (same `ipKey` type, same anchor-cluster scoping
      rationale).
- [x] 1.2 Build it in `newSGResolver` inside the existing `topology.Pods` loop that
      builds `podByID`: cluster from `p.Labels()["cluster"]`, IP from `p.IPAddress()`.
      Skip a pod whose IP slice is empty or whose first element is `""` (design D2).
- [x] 1.3 On a duplicate `(cluster, ip)`, keep the lexically-smallest `pod.ID()`
      (design D3), mirroring the `ipIndex` duplicate handling.
- [x] 1.4 Add `lookupPeerPodIP(anchorCluster, host string) *graph.PodNode` — a pure map
      read, shared by the parse and the prescan.

## 2. Resolver ladder (`pkg/build/servicegraph.go`)

- [x] 2.1 In `resolveUnknownServerPeer`, inside the `!classified` +
      `net.ParseIP(host) != nil` sub-branch, consult `lookupPeerPodIP` BEFORE
      `routeExternal("unknown_server_peer_ip_literal_no_match", ...)` (design D1).
- [x] 2.2 On a hit return `[]string{pod.ID()}` directly — no `resolveServiceLevel`, no
      service node, no `service-selects-pod` edge (design D5).
- [x] 2.3 Emit a `slog.Debug` on the hit, matching the shape of the existing
      "resolved to service node" debug line (side, peer address, pod id, anchor cluster,
      client/server labels).
- [x] 2.4 Leave `classifyPeerHost`, `classifyK8sDNS`, `classifyBareShortName` and the
      `ipIndex` step untouched — ClusterIP priority comes from ordering alone.
- [x] 2.5 Update the `resolveUnknownServerPeer` doc comment to describe the new step and
      its position in the ladder.

## 3. Prescan (`pkg/build/routeprescan.go`)

- [x] 3.1 In `collectRouteQueriesWith`, after the existing
      `classifyPeerHost` / `anchorHolds` skip, skip keys whose host is an IP literal that
      `lookupPeerPodIP` resolves in the anchor cluster (design D4).
- [x] 3.2 Comment why: the route engine is consulted only where the parse would fall
      external.

## 4. Tests (`pkg/build/servicegraph_test.go`, `pkg/build/routeprescan_test.go`)

- [x] 4.1 Fixture: a topology whose anchor-cluster pod carries a `pod_ip`, plus a
      family-sibling cluster pod carrying the same IP.
- [x] 4.2 IP literal matching a pod's `pod_ip` in the anchor cluster resolves to that
      pod: one `pod-calls-pod` edge, `target == "<cluster>/<uid>"`, no service node, no
      external node, no `service-selects-pod` edge.
- [x] 4.3 Same with a `:8080` suffix on the peer value — identical result.
- [x] 4.4 An address held by BOTH a Service `ClusterIP` and a pod `pod_ip` resolves to
      the Service (`pod-calls-service`) — ClusterIP priority.
- [x] 4.5 Pod IP present only in a family-sibling cluster falls to `external/<ip>`.
- [x] 4.6 Two anchor-cluster pods sharing one IP resolve to the lexically-smallest pod
      id; assert the same result with `topology.Pods` in reversed order.
- [x] 4.7 A pod with an empty `pod_ip` is not indexed — the endpoint falls to
      `external/<ip>`.
- [x] 4.8 No match anywhere still falls to `external/<ip>` (regression guard for the
      existing behaviour).
- [x] 4.9 Prescan: an endpoint resolvable via `podIPIndex` produces no `routeKey`.

## 5. Integration (`internal/integration`)

- [x] 5.1 `TestUnknownServerPodIPPeerResolvesToPod`: ingest a backend pod's
      `kube_pod_info{cluster="cluster-alpha",...,pod_ip="10.244.1.9"}` plus two
      `traces_service_graph_request_total` samples with `server="unknown"`,
      `server_k8s_pod_uid=""`, `client_server_address="10.244.1.9"`.
- [x] 5.2 Assert the response contains a `pod-calls-pod` edge targeting the backend pod's
      cluster-scoped id, and does NOT contain `external/10.244.1.9`.

## 6. Docs

- [x] 6.1 Extend the "Unknown-server peer-label enrichment" bullet in `CLAUDE.md` with
      the Pod-IP step, its anchor-cluster scoping, the ClusterIP-first ordering, and the
      "no service node / no fan-out" consequence.

## 7. Verify

- [x] 7.1 `make test` passes (`-race -shuffle=on`).
- [x] 7.2 `make lint` and `make vet` clean.
- [x] 7.3 `make check-route-containment` passes.
- [x] 7.4 `go test ./internal/api/ -run TestGolden` passes WITHOUT `-update` — no golden
      file changes.
- [x] 7.5 `openspec validate "resolve-unknown-server-pod-ip-peer"` passes.
