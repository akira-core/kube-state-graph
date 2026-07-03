## Why

The D30 virtual-sentinel exclusion drops **every** `traces_service_graph_request_total`
series whose `server` label is exactly `"unknown"` at the PromQL query layer
(`server!~"user|unknown"`), before any Go resolution runs. This is correct for the common
case — the `servicegraph` connector emits `server="unknown"` when it cannot pair a peer to
any span context at all, and there is no usable identity to build a node from.

However, when the **client side does resolve to a real topology pod**, some exporter
configurations additionally carry the caller's own recorded peer address as extra
dimensions on the same series — `client_net_peer_name` / `client_server_address` (OTel
semantic-convention attributes captured on the *client* span, describing the address it
dialed). Today this information is thrown away by the query-layer exclusion even though it
would let the reader identify a real in-cluster Service (or a genuine external dependency)
that the server-UID join alone could not resolve. Dropping it means a known, instrumented
caller's outbound edge to an unresolved peer is invisible in the graph, even though the
caller told us who it was calling.

## What Changes

- **Loosen the D30 selector on the `server` side only.** `server!~"user|unknown"` becomes
  `server!~"user"` (the `client` side matcher is unchanged: `client!~"user|unknown"`).
  `server="unknown"` series now reach Go for every case, not just the new one below — the
  design/spec must (and does) pin down what happens to the cases that are NOT the new
  enrichment branch, so no new `external/unknown` noise leaks in from a plain selector
  loosening (this is the crux of the change; see design.md D1).
- **New resolution branch, server side only, narrow trigger:** when `client_k8s_pod_uid`
  resolves to a real topology pod AND the server side has no resolvable pod (UID empty or
  UID present but absent from the global pod-UID index) AND the raw `server` label is
  literally `"unknown"`, the reader additionally consults `client_net_peer_name` (checked
  first) then `client_server_address` (checked second) on the same series. Whichever is
  non-empty first is treated as the peer's address and resolved:
  - a cluster-local 2/3-label `.svc` name, or a bare single-label short name (implicitly
    qualified by the client pod's own namespace) → resolved via the existing D29
    same-cluster Service-node path (`resolveServiceLevel`), anchored on the *already
    resolved* client pod's cluster (no anchor-recovery ambiguity here — the client is a
    real pod) — same cross-cluster `service-selects-pod` fan-out as any other D29 hit.
  - anything else (external FQDN, IP literal, unparseable) → a plain `external/<value>`
    node, verbatim, same convention as the existing D27 / D29 external fallback.
  - neither label present → the endpoint is **dropped**, reproducing today's pre-change
    (D30-excluded) outcome exactly. This is the deliberate answer to the open design
    question: loosening the selector must not, by itself, turn an unenrichable
    `server="unknown"` series into an `external/unknown` node via the generic missing-UID
    fallback (D27) — it stays invisible unless the new labels actually identify a peer.
- **Every other `server="unknown"` case is unaffected in outcome** (client does not resolve
  to a real pod, or the server UID itself resolves) — the endpoint is dropped exactly as
  under the old query-layer exclusion. The observable graph only gains nodes/edges in the
  one new branch; it loses none.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `pod-service-graph`: narrow the "Virtual sentinel endpoint exclusion (user / unknown)"
  requirement's `server` side from a blanket query-layer exclusion to "excluded unless the
  new client-resolved-pod peer-label enrichment applies"; add a new requirement for the
  peer-label enrichment resolution itself (target classification, anchor rule, fallback to
  external, and the "no enrichment label → still dropped" outcome).

## Impact

- **Code:**
  - `pkg/promql/queries.go` — `serviceGraphSentinelSelector`: split into independent
    `client`/`server` matchers so only the server side drops `"unknown"` from its
    exclusion set.
  - `pkg/build/servicegraph.go` — `resolveServer`: restructure so a literal `server ==
    "unknown"` with no resolved pod branches into the new peer-label enrichment path
    instead of falling through to `resolveEmptyUID` (which would otherwise apply the
    generic D27 external fallback and materialise a `external/unknown` node) or the
    synth-pod fallback (when a UID was present but unresolved). New helper(s) for reading
    `client_net_peer_name` / `client_server_address` off the series, classifying the value
    (cluster-local vs external, including the bare-short-name-in-client-namespace case),
    and reusing `resolveServiceLevel` / `external`.
- **Tests:** `pkg/build/servicegraph_test.go` (new `TestParseServiceGraph_UnknownServerPeerLabel_*`
  cases covering: `client_net_peer_name` hit, `client_server_address` fallback hit, both
  absent → dropped, external-address hit, bare-short-name hit, client not a resolved pod →
  dropped, server UID present-but-unresolved with `server="unknown"` → same enrichment
  path). `internal/integration` — one fixture exercising the loosened selector end-to-end.
- **Docs:** `CLAUDE.md` — the D30 bullet gains the narrowed server-side scope and a pointer
  to the new peer-label enrichment rule; `openspec/specs/pod-service-graph/spec.md` gets the
  modified sentinel requirement plus the new enrichment requirement.
- **Contract / compatibility:** behaviour change, not a wire-schema break. A previously
  invisible subset of edges (client resolved, server unresolved, `server="unknown"`, one of
  the two peer labels present) now appears; every other case is bit-for-bit unchanged
  (still dropped). No new HTTP route, no new node/edge *type* (reuses `service` / `external`
  / `pod-calls-service` / `pod-calls-pod`).
- **Dependency:** none — purely additive to `pod-service-graph`; no interaction with the
  storageclass/argo-application or infra-node-pruning changes.
