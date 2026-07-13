## Why

`resolveUnknownServerPeer` in `pkg/build/servicegraph.go` recovers a `server="unknown"` endpoint from
the client-recorded `client_net_peer_name` / `client_server_address` labels, but only when the value
names something *in-cluster*: Kubernetes `.svc` DNS (`classifyK8sDNS`), a bare short Service name
(`classifyBareShortName`), or a ClusterIP literal (the `ipIndex` lookup). A **global / ingress FQDN**
such as `api.example.com` matches none of them and becomes a dead-end `external/api.example.com` node.

Those calls are not leaving the mesh. They hit an Istio **ingress Gateway**, and a **VirtualService**
routes them to a real in-cluster Service. The graph should show the `pod-calls-service` edge to that
Service, not an `external` box — the caller's real dependency is invisible today.

`docs/istio-virtualservice-routing-history-design.md` §5.B specifies the route-resolution engine that
closes this, and `poc/route2a` is a working, benchmarked implementation of it (600 gateways × 100
VirtualServices, 0 mismatches against a by-construction oracle).

## What Changes

- Add a new, **last** classification step to `resolveUnknownServerPeer`: at every point where it would
  today fall back to an `external` node, consult an **Istio route-resolution engine** with
  `(anchor cluster, host, path, port, dst IPs, [start, end])` and, on a hit, resolve the returned
  destination through the existing `resolveServiceLevel` — producing an ordinary `service` node and a
  `pod-calls-service` edge, indistinguishable from any other D29-resolved service node.
- The engine is consulted at **all three** existing external-fallback branches, not just the obvious
  one. `classifyK8sDNS` splits on dots, so a 3-label FQDN like `api.example.com` is *successfully*
  classified as service `example` in namespace `com` and only then misses in `resolveServiceLevel` —
  a global FQDN therefore reaches `external` through `unknown_server_peer_anchor_lacks_service`, not
  through `unknown_server_peer_not_k8s_dns`.
- Introduce `build.RouteResolver`, a new injected interface on `build.Options` (mirrored on
  `kubegraph.Options`). **A nil resolver means the feature is off and behaviour is byte-for-byte
  unchanged** — this is the default and the regression safety net.
- Add `pkg/route/`, the concrete engine, ported from `poc/route2a/internal/`: read-only ClickHouse
  versioned-config store (`cluster`-scoped), in-process istiod translation to an Envoy
  `RouteConfiguration`, and route matching via the native `router_check_tool` binary. **`pkg/build`
  declares only the interface and MUST NOT import `pkg/route`**, so an embedder of `pkg/kubegraph`
  never links istio or ClickHouse.
- Read two **new optional** service-graph dimensions, both degrading gracefully when absent:
  `client_dns_answers` (destination IP → the Gateway 3-hop; absent ⇒ resolve the host over all the
  cluster's Gateways) and `client_server_port` / `client_net_peer_port` (ingress listener port).
  `stripPeerAddressPort` now **returns** the port it currently discards; port precedence is
  peer-address `:port` → the optional label → default **443**.
- **No PromQL / selector change**, no new node type, no new edge type, no new node attribute, no new
  `labels` key. The destination's port and DestinationRule subset are parsed but discarded in v1.
- New config knobs: `--route-store-dsn` (empty ⇒ feature off), `--router-check-bin`,
  `--route-resolve-timeout`. The container image gains the `router_check_tool` binary.

## Capabilities

### New Capabilities

(none — this extends an existing capability rather than introducing one)

### Modified Capabilities

- `pod-service-graph`: extends the "Unknown-server peer-label enrichment" requirement with a
  route-resolution step that runs at every external fallback, resolving a global/ingress FQDN to the
  Kubernetes Service the Istio Gateway + VirtualService config actually routed it to over the
  request's own time window. Adds the optional `client_dns_answers` and `client_server_port` /
  `client_net_peer_port` dimensions and the listener-port derivation rule.

## Impact

- **New**: `pkg/build/routeresolve.go` (the `RouteResolver` interface, `RouteRequest`,
  `RouteDestination`); `pkg/route/{store,gwresolve,translate,memwindow,matchcheck}` plus
  `pkg/route/resolver.go`.
- **Modified**: `pkg/build/servicegraph.go` (prescan + prefetched route index, `classifyPeerHost`
  extraction, `stripPeerAddressPort` signature, new label reads, new `noteExternal` reasons),
  `pkg/build/options.go`, `pkg/kubegraph/engine.go`, `internal/config/config.go`,
  `cmd/kube-state-graph/main.go`, `.mockery.yaml`, `Dockerfile`, `CLAUDE.md`.
- **Dependencies**: `istio.io/istio` (→ `k8s.io/client-go` transitively), `istio.io/api`,
  `github.com/ClickHouse/clickhouse-go/v2`, `github.com/envoyproxy/go-control-plane/envoy` — a
  documented exception to CLAUDE.md's "don't add dependencies casually". They are contained by the
  `pkg/build` ↛ `pkg/route` import rule, so they are never linked into an embedder's binary.
- **CLAUDE.md's "don't import client-go into the API server" rule is restated, not excepted.** Its
  stated purpose (archived design D1/D16) is to forbid *informers and live Kubernetes API access*,
  because informers only know the current state and cannot answer this API's historical time-range
  contract, and because multi-cluster would need N watch streams plus per-cluster RBAC. The route
  engine reads versioned historical config from ClickHouse over the request's own window, and never
  constructs a Kubernetes client, opens a watch, or needs a kubeconfig — istio is used purely as a
  library (protos in, an in-memory `RouteConfiguration` out). The rule is reworded to say what it
  meant: **don't talk to the Kubernetes API; linking a library that vendors Kubernetes types is not
  the same thing.**
- **Runtime**: the image must carry the native `router_check_tool` binary. `router_check_tool` costs
  ~50–60ms per invocation, which dominates a resolution; v1 accepts this (correctness first) and
  bounds it with the existing `--build-timeout`, degrading any failed or timed-out endpoint to today's
  `external` node.
- **Cross-repo dependency**: the versioned Istio-config store is written by the metadata-exporter repo
  (`pkg/history` + `pkg/store`); kube-state-graph is strictly read-only against it. The **`cluster`
  column is a new requirement on that repo** — this change is blocked on it landing there.
