## Context

`pkg/build/servicegraph.go` resolves each `traces_service_graph_request_total` series'
client and server endpoints independently (`resolveClient` / `resolveServer`), after the
D30 sentinel exclusion has already dropped any series with `client` or `server` exactly
`"user"` or `"unknown"` **at the PromQL layer** (`serviceGraphSentinelSelector` in
`pkg/promql/queries.go:125`). Because the exclusion runs before the query even returns,
`resolveServer` today never sees a literal `server="unknown"` value — Go-side resolution
has no branch for it.

Some exporter configurations additionally stamp `client_net_peer_name` /
`client_server_address` on a series — OTel semantic-convention attributes recorded on the
*client* span, describing the address the caller dialed, independent of whether the
`servicegraph` connector managed to pair that peer to its own span (hence `server`
collapsing to `"unknown"`). When the client side resolves to a real topology pod, this
label is a usable, if secondary, identity source for the otherwise-unresolvable peer.

This change threads a narrow carve-out through the D30 exclusion so that one specific case
— client resolved to a real pod, server unresolved, `server=="unknown"`, one of the two
peer labels present — resolves instead of being dropped, while leaving every other
`server="unknown"` case dropped exactly as before.

## Goals / Non-Goals

**Goals:**
- Recover a `pod-calls-service` or `pod-calls-pod` (external-target) edge for a
  `server="unknown"` series when the client is a real topology pod and either
  `client_net_peer_name` or `client_server_address` names a usable peer.
- Leave every other `server="unknown"` case — client not a resolved real pod, or server UID
  itself resolves — dropped, byte-for-byte identical to today's D30-excluded outcome. The
  selector loosening (required to let these series reach Go at all) must not by itself
  create new `external/unknown` nodes via the generic D27 fallback.
- Reuse the existing D29 machinery (`resolveServiceLevel`, `classifyK8sDNS`, `external`)
  rather than inventing a parallel resolution path.

**Non-Goals:**
- No change to the `client`-side sentinel exclusion (`client!~"user|unknown"` stays as-is —
  this change is server-side only; an unresolved *caller* still carries no identity worth
  recovering this way, since the new labels describe a client-recorded peer address, not a
  reverse/server-recorded caller address).
- No new node or edge type. The enrichment path terminates in the same `service` /
  `external` node types and `pod-calls-service` / `pod-calls-pod` edge types as today's D29
  / D27 paths.
- No PromQL selector change beyond the one `server` matcher edit — no new metric, no new
  query, no configurable knob for label names or detection (mirrors D29/D30's own
  "hardcoded, no knob" precedent).
- No enrichment when the *client* side is unresolved (empty/absent UID, or UID present but
  missing from topology). The client must be a real, UID-matched topology pod so the
  anchor cluster for Service resolution is unambiguous.

## Decisions

### D1: Loosen only the `server` sentinel matcher; every non-enriched case still drops

`serviceGraphSentinelSelector` (`pkg/promql/queries.go:125`) changes from the single
combined string `client!~"user|unknown",server!~"user|unknown"` to two independently
authored fragments so the server-side matcher becomes `server!~"user"` while the
client-side matcher stays `client!~"user|unknown"`. This means **every** `server="unknown"`
series now reaches Go, not just the ones eligible for enrichment.

To avoid turning this into a silent behaviour change for the non-enriched majority,
`resolveServer` gains an explicit branch: whenever the raw `server` label is literally
`"unknown"` AND no real topology pod is found for it (whether because the UID was empty or
because it didn't match `podByUID`), resolution goes to the new
`resolveUnknownServerPeer` helper — **never** to `resolveEmptyUID` (which owns the generic
D27 missing-UID-label fallback) and **never** to the synth-pod fallback. Inside
`resolveUnknownServerPeer`:
- if the client side did not resolve to a real topology pod, or neither
  `client_net_peer_name` nor `client_server_address` is present → return `nil` (dropped;
  identical outward behaviour to the old query-layer exclusion for this series).
- otherwise → proceed to D2/D3 below.

**Why:** the alternative (leave the exclusion at `client!~"user|unknown",server!~"user|unknown"`
and add a *second*, unfiltered query just to recover these two labels) was considered and
rejected — it doubles the service-graph query cost for a metric that is otherwise unused,
and every needed label is already present on the one series once the selector no longer
excludes it. A single loosened selector plus an explicit "still drop by default" branch
keeps one query and makes the one behaviour change (the new branch) the only place the
outcome differs from today.

**Alternatives considered:**
- **Loosen both `client` and `server` matchers** — *rejected:* out of scope (see Non-Goals);
  the proposal's decision tree only enriches the server side.
- **Let the loosened `server="unknown"` series fall through to the existing D27 fallback
  when enrichment doesn't apply** — *rejected:* that would materialise a `external/unknown`
  node for every un-enrichable case, which is exactly the noise D30 exists to prevent. D1's
  explicit early-return preserves D30's outcome for every case outside the new branch.

### D2: Peer-label precedence and value classification

`resolveUnknownServerPeer` reads the two labels in a fixed order — `client_net_peer_name`
first, `client_server_address` second — and uses whichever is non-empty first (never both,
never a merge). The chosen raw value is then classified:

1. Strip an optional trailing `:<port>` (best-effort `net.SplitHostPort`; on failure, e.g.
   no colon present, use the value unchanged as the host).
2. Run the existing `classifyK8sDNS(host)` (2-label `<service>.<namespace>` / 3-label
   headless `<pod>.<service>.<namespace>`, `.svc[.<domain>]` suffix stripped) — same
   function the D29 connection-string path already uses.
3. **New for this change only:** if `classifyK8sDNS` does not match (not 2 or 3 labels)
   AND the host is a single DNS-1123 label with no dots and is not an IP literal, treat it
   as a **bare short Service name resolved in the client pod's own namespace** —
   `(service=host, namespace=<client_k8s_namespace_name>)`. This is the one grammar
   extension over D29's connection-string classification, needed because
   `client_net_peer_name` / `client_server_address` commonly carry a bare in-cluster short
   name (no `"://"`, no `.svc` suffix) that a same-namespace caller would resolve via
   Kubernetes' own unqualified-name DNS search path.
4. Anything else (multi-label non-`.svc` FQDN, IP literal, unparseable) is **external**.

**Why:** reusing `classifyK8sDNS` keeps the two-label/three-label `.svc` grammar in exactly
one place. The bare-short-name extension is scoped narrowly (single label, no dots) so it
cannot misfire on a real external hostname (which is virtually always multi-label).

**Alternatives considered:**
- **Treat every non-`.svc`-suffixed value as external, dropping the bare-short-name case**
  — *rejected:* a bare short name is the single most common in-cluster form these
  attributes carry when the caller and callee share a namespace; dropping it would make the
  new branch resolve almost nothing.
- **Guess the namespace from other series' data when the bare name doesn't specify one** —
  *rejected:* the client pod's own namespace is already known and unambiguous; guessing
  from unrelated series would violate determinism (D6).

### D3: Anchor cluster is the already-resolved client pod's cluster; resolution reuses `resolveServiceLevel`

Unlike the D29 connection-string path (where the client side may itself be unresolved,
forcing a raw-trace-label fallback for the anchor), this branch only runs when the client
**is** a resolved topology pod, so the anchor cluster is simply that pod's own
`labels["cluster"]` — no ambiguity, no fallback chain. The classified `(namespace,
service)` is resolved via the existing `resolveServiceLevel(anchorCluster, ns, svc)`
unchanged: same anchor-membership test (the anchor cluster itself must hold the Service),
same cross-cluster `service-selects-pod` fan-out over the family, same "no
endpoint-backed pruning". When `classifyK8sDNS`/bare-name classification fails, or
`resolveServiceLevel` returns nil (anchor cluster doesn't hold the Service), the endpoint
resolves to `external/<raw_value>` (the raw, unstripped label value — verbatim, same
convention as every other external fallback in this package) via the existing
`sgResolver.external` helper.

**Why:** this is the smallest correct change — it slots the new label as an alternate
*input* to the same D29 resolution machinery rather than duplicating anchor/family logic.

### D4: Edge type, `labels.cluster`, and determinism are unaffected

No change to edge-type derivation (`pod-calls-service` iff the resolved target is in
`res.services`, `pod-calls-pod` otherwise — `parseServiceGraph`'s existing generic check),
`labels.cluster` (present because the client side is a pod), or determinism (candidate
clusters are already iterated in sorted order by `resolveServiceLevel`; the new branch adds
no new source of nondeterminism — label precedence is a fixed order, not a set union).

## Risks / Trade-offs

- **[Selector loosening widens what reaches Go]** `server!~"user"` alone (no `"unknown"`
  exclusion) means every un-enrichable `server="unknown"` series now costs one extra
  resolution call (immediately short-circuited to a `nil`/dropped result by D1's early
  return) instead of being filtered upstream by VictoriaMetrics. → Negligible: the
  short-circuit is a few label comparisons, dwarfed by the network fetch; the series volume
  is unchanged (VM still returns them, just one query-selector byte fewer excluded).
- **[Silent behaviour drift if the D1 early-return is ever weakened]** Any future change
  that lets an un-enriched `server="unknown"` fall through to `resolveEmptyUID` would
  reintroduce `external/unknown` noise. → Guarded by an explicit unit test
  (`TestParseServiceGraph_UnknownServerPeerLabel_NeitherLabelPresent_Dropped`) asserting no
  node/edge is produced, and a code comment on the branch pointing at D30/D1.
- **[Bare short-name heuristic could misfire]** A genuinely external single-label hostname
  (rare, but e.g. an internal corporate DNS zone using bare names) would be misclassified
  as an in-cluster Service lookup. → Bounded: `resolveServiceLevel` only succeeds when the
  anchor cluster's own `ServicesByNameNS` actually holds that (namespace, name) — a bare
  external name essentially never collides with a real Service name in the caller's own
  namespace, and a miss still falls back to `external/<raw_value>` (no data loss, worst case
  a wasted map lookup).

## Migration Plan

1. `pkg/promql/queries.go`: split `serviceGraphSentinelSelector` into independent
   `client!~"user|unknown"` and `server!~"user"` fragments.
2. `pkg/build/servicegraph.go`:
   - Add a `sentinelUnknown = "unknown"` (or reuse an existing constant if one already
     covers the literal comparison) and branch in `resolveServer` before falling to
     `resolveEmptyUID` / the synth-pod path.
   - Add `resolveUnknownServerPeer(clientResolvedPod bool, anchorCluster string, series
     labels, t sgTrace) []string` implementing D1–D3.
   - Add the port-stripping + bare-short-name classification helper (D2).
3. `pkg/build/servicegraph_test.go`: add the `TestParseServiceGraph_UnknownServerPeerLabel_*`
   cases enumerated in proposal.md's Impact section.
4. `internal/integration`: one fixture-driven test exercising the loosened selector against
   a real VictoriaMetrics container.
5. `CLAUDE.md` + `openspec/specs/pod-service-graph/spec.md`: update per the spec deltas in
   this change.

**Rollback:** revert the selector split and the `resolveServer` branch together — they are
coupled (reverting only the selector would silently regress to D30's original, stricter
exclusion; reverting only the Go branch while the selector stays loosened would leak
`external/unknown` nodes via D27). No persisted state, no edge-ID namespace change.
