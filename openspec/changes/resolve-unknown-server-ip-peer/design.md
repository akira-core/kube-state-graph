## Context

`resolveUnknownServerPeer` (`pkg/build/servicegraph.go`) is the narrow carve-out added by
`resolve-unknown-server-peer-labels`: when a service-graph series has `server=="unknown"`
and the client side resolved to a **real** topology pod, it picks a peer value —
`client_net_peer_name` first, `client_server_address` second (first non-empty wins, never
merged) — strips an optional `:port` (`stripPeerAddressPort`), then classifies the
resulting host:

1. `classifyK8sDNS(host)` — the existing D29 2-label (`<service>.<namespace>`) / 3-label
   headless (`<pod>.<service>.<namespace>`) `.svc` grammar.
2. `classifyBareShortName(host)` — a dot-free, non-IP short name, resolved as a Service in
   the **client pod's own namespace**.
3. Anything else → `external/<raw_value>`.

Some exporter configurations record a bare ClusterIP literal (e.g. `172.20.10.5`, no
`:port`, no DNS name) in `client_server_address` — this is the "IP addresses ... from the
`*_server_address` metric labels" case that motivated this change. `classifyBareShortName`
already explicitly excludes IP literals (`net.ParseIP(host) != nil` → `false`), and
`classifyK8sDNS` requires 2 or 3 dot-separated labels matching Service DNS grammar, which a
dotted-quad IP happens to superficially resemble in shape but never resolves against
`ServicesByNameNS` (that index is keyed by Service name, not IP) — so today every such
value falls to `external/<ip>`, one external node per distinct IP, with no
`pod-calls-service` edge and no fan-out.

## Goals / Non-Goals

**Goals:**
- Resolve a bare IP-literal peer value (from either `client_net_peer_name` or
  `client_server_address`, via the existing precedence — unchanged) to the Service whose
  `ClusterIP` matches, when that Service is deployed in the caller's own anchor cluster.
- Reuse `resolveServiceLevel` unchanged once `(ns, svc)` is known, so the resolved node's
  materialisation (service node shape, `service-selects-pod` family fan-out, dedup) is
  byte-for-byte identical to every other D29 resolution path — no new node/edge type, no
  parallel materialisation logic.
- Deterministic (D6): the new reverse index is a pure function of `topology.ServicesByNameNS`.

**Non-Goals:**
- No new label. `server_server_address` was considered and explicitly dropped from scope —
  the feature only reads the two labels `resolveUnknownServerPeer` already threads
  (`client_net_peer_name`, `client_server_address`). `resolveServer`'s signature is
  unchanged.
- No cross-cluster / family-wide IP matching. See D2.
- No change to `classifyK8sDNS` or `classifyBareShortName` grammar, and no change to the
  D29 connection-string path (`resolveConnString`) — this is scoped to the
  unknown-server-peer-label trigger only, same scoping discipline the parent change used
  for `classifyBareShortName`.
- No PromQL/selector change — `ServicesByNameNS` is already read by `ReadTopology`; no new
  upstream query.

## Decisions

### D1: New classification step is IP-literal, tried last

`resolveUnknownServerPeer`'s existing chain becomes: `classifyK8sDNS` →
`classifyBareShortName` → **new: IP-literal lookup** → `external`. The IP check is
`net.ParseIP(host) != nil` (handles both IPv4 and IPv6 literals; `classifyBareShortName`
already independently excludes IPs from its own branch, so the two remain mutually
exclusive by construction — no double-classification).

**Why last:** `classifyK8sDNS` and `classifyBareShortName` are cheap, already-correct
checks for the common DNS-name cases; ordering the new IP branch after them costs nothing
(an IP literal never satisfies either) and keeps the existing precedence untouched.

**Alternatives considered:**
- **Check IP-literal first** — rejected: no behavioural difference (the classifiers are
  mutually exclusive on any given host), but ordering it last keeps the diff minimal and
  reads as "the fallback grammar extension it is."

### D2: Reverse index is scoped to the anchor cluster only, not the cluster family

The new index is keyed `ipKey{cluster, ip} → serviceKey{cluster, namespace, service}`,
built once per parse from `topology.ServicesByNameNS`, and looked up as
`ipIndex[ipKey{anchorCluster, host}]` — **only** the caller's own anchor cluster, never a
family sibling.

**Why:** unlike a Service's DNS name (which is a mesh-wide convention `resolveServiceLevel`
deliberately unions across a cluster family — D29/D3), a `ClusterIP` is an
implementation-detail address assigned per-cluster from that cluster's own (often
overlapping or identical) Service CIDR. Two unrelated clusters routinely allocate the same
`10.96.0.0/12`-range address to two *different* Services. Matching an IP literal
family-wide would risk resolving a peer to the wrong Service whenever CIDRs collide —
silently wrong data, not just a missed resolution. Restricting the identification lookup
to the anchor cluster's own `ServicesByNameNS` slice eliminates that collision class
entirely: the only cluster consulted is the one whose CIDR actually assigned the address.
Once `(ns, svc)` is identified this way, the **existing**
`resolveServiceLevel(anchorCluster, ns, svc)` call is used unchanged — its own
family-union `service-selects-pod` fan-out still applies exactly as it does for every
other D29 resolution path, since that fan-out reasons about the Service by name, not by
IP.

**Alternatives considered:**
- **Family-wide IP index** (mirroring `svcCandidates`'s `famSvcKey`) — rejected per above:
  CIDR overlap makes cross-cluster IP identity unsound in a way DNS-name identity is not.
- **Skip the index; linear-scan `ServicesByNameNS` per lookup** — rejected: `O(services)`
  per unresolved endpoint with hot-path cost proportional to a full parse's cluster size,
  versus `O(1)` after one `O(services)` index build shared across the whole parse (the
  same trade-off `svcCandidates` already made).

### D3: Determinism on IP collision within one cluster

A single cluster should never assign one `ClusterIP` to two Services (Kubernetes itself
guarantees this), but the index build stays defensive: on a duplicate `(cluster, ip)` key,
the lexically-smaller `(namespace, service)` wins — same convention as `PodsByUID`'s
duplicate-UID tiebreak and `resolveNodeReadyStatus`'s duplicate-active-row tiebreak
elsewhere in this package. Pure function of the sample set, independent of map-iteration
order.

### D4: No new label; IP branch applies to whichever of the two existing labels supplied the value

The peer `value` `resolveUnknownServerPeer` classifies is already a single string chosen
by the existing `client_net_peer_name` → `client_server_address` precedence, with no
record of *which* label it came from once chosen. The new IP-literal branch operates on
that same `value` unconditionally — an IP literal in `client_net_peer_name` resolves
identically to one in `client_server_address`. This was a candidate design considered
(threading a 3rd label `server_server_address`, or restricting the IP branch to only fire
when the value came specifically from `client_server_address`) and explicitly rejected:
it would require threading label *provenance* through the resolver (a new field on
`sgTrace` or an extra return value from the precedence pick) for a distinction that has no
behavioural payoff — an IP is an IP regardless of which label carried it.

## Risks / Trade-offs

- **[IP-literal in a non-anchor cluster resolves to `external` instead of the Service]** —
  by design (D2): the caller dialed a ClusterIP that isn't provisioned in its own cluster's
  Service CIDR, which for a normal in-cluster dependency should not happen. If it does
  (e.g. cross-cluster VPC peering directly to a peer cluster's ClusterIP, bypassing the
  service mesh), the endpoint still degrades gracefully to `external/<ip>` — no data loss,
  no incorrect resolution, matches every other D29 miss case.
- **[Index build cost]** — one extra `O(services)` pass per parse, same order as the
  existing `svcCandidates` build it sits next to; negligible versus the upstream fetch.
- **[Sequencing with the not-yet-archived `resolve-unknown-server-peer-labels` change]** —
  this design's baseline (`resolveUnknownServerPeer`'s 2-classifier chain) is defined by
  that change's delta spec, not yet promoted to `openspec/specs/`. If archive order gets
  reversed, `openspec verify` will surface the missing base requirement. Mitigation: archive
  `resolve-unknown-server-peer-labels` first (its code is already merged to this branch), or
  archive both together.

## Migration Plan

1. `pkg/build/servicegraph.go`:
   - Build `ipIndex map[ipKey]serviceKey` in `parseServiceGraph`, alongside the existing
     `svcCandidates` build, from `topology.ServicesByNameNS` (skip empty / `"None"`
     `ClusterIP`; lexically-smallest `(namespace, service)` wins on collision).
   - Thread `ipIndex` onto `sgResolver`.
   - Add the IP-literal branch to `resolveUnknownServerPeer`, after the existing
     `classifyBareShortName` miss and before the `external` fallback.
2. `pkg/build/servicegraph_test.go`: add
   `TestParseServiceGraph_UnknownServerPeerLabel_IPLiteral_*` cases (anchor-cluster hit,
   anchor-cluster miss → external, IP present only in a family sibling → external not
   cross-resolved, collision determinism).
3. `internal/integration`: one fixture case with a ClusterIP-literal
   `client_server_address` against a real VictoriaMetrics container.
4. `CLAUDE.md` + `openspec/specs/pod-service-graph/spec.md` (after
   `resolve-unknown-server-peer-labels` archives): extend the "Unknown-server peer-label
   enrichment" requirement text with the IP-literal classification step.

**Rollback:** revert the `resolveUnknownServerPeer` branch and `ipIndex` build together —
self-contained, no selector change, no edge-ID namespace change, no persisted state.
