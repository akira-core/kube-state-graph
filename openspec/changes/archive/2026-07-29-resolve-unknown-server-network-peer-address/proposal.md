## Why

The "Unknown-server peer-label enrichment" rule (`resolve-unknown-server-peer-labels` D1–D3,
extended by `resolve-unknown-server-ip-peer`) recovers an otherwise-dropped `server="unknown"`
endpoint by reading two client-recorded peer-address labels off the same
`traces_service_graph_request_total` series: `client_net_peer_name` and
`client_server_address`.

Exporters that have migrated to the **stable** OpenTelemetry network semantic conventions
emit `network.peer.address` — surfacing as `client_network_peer_address` on the series. It
is the stable successor of `net.peer.ip` / `net.sock.peer.addr` and carries the
**socket-level** peer address (by convention an IP). For a deployment that emits it and
neither of the two labels the rule currently reads, the enrichment trigger never fires and
the endpoint is dropped: a known, instrumented caller's outbound edge stays invisible even
though the caller recorded exactly who it dialed.

The three labels are **not** three spellings of one attribute, and the change orders them
accordingly:

| label on the series | OTel attribute | status | what it holds |
|---|---|---|---|
| `client_server_address` | `server.address` | **stable** | logical destination as the caller addressed it — name, IP, or UDS name |
| `client_network_peer_address` | `network.peer.address` | **stable** | socket-level peer address — by convention an IP |
| `client_net_peer_name` | `net.peer.name` | **deprecated**, superseded by `server.address` | logical destination name |

Separately, observed real-world values of `client_network_peer_address` carry a **bracketed
suffix** after the `host:port` authority — e.g. `mongo.com:27017[-181]`, an
instrumentation-appended connection/session identifier that is not part of the network
address. The existing port-stripping helper (`stripPeerAddressPort`, a thin
`net.SplitHostPort` wrapper) **fails** on such a value (`unexpected '[' in address`) and
passes it through unchanged, after which `classifyK8sDNS` — which performs no DNS-1123
validation — misclassifies `mongo.com:27017[-181]` as `service="mongo"`,
`namespace="com:27017[-181]"`. The lookup always misses, so the outcome is accidentally
correct today; the suffix must be removed explicitly so classification sees the real host.

## What Changes

- **Read one new label:** `client_network_peer_address` is read off each
  `traces_service_graph_request_total` series in `parseServiceGraph` and threaded through
  `resolveServer` into `resolveUnknownServerPeer`, joining the two labels that rule already
  consults.
- **Precedence becomes `client_server_address` → `client_network_peer_address` →
  `client_net_peer_name`** (first non-empty wins outright, never merged — the existing
  rule, one entry longer and reordered). Labels are ranked by what the classification chain
  can actually resolve: the chain is strong on names (DNS grammar, bare short name — both
  reach `resolveServiceLevel` and its family-wide fan-out) and weak on IPs (the `ClusterIP`
  lookup is restricted to the anchor cluster, so a pod IP / loopback / off-cluster IP lands
  on `external/<ip>`). `server.address` is the name-valued stable attribute and leads;
  `network.peer.address` is the IP-valued stable attribute and follows; the deprecated
  `net.peer.name` — superseded by `server.address` — trails.
- **This reorders the two EXISTING labels** (today: `client_net_peer_name` first). A series
  carrying both with **different** values now resolves from `client_server_address`. This
  is the one non-additive part of the change, deliberate rather than incidental: leaving a
  deprecated attribute ahead of the stable one that supersedes it has no defence beyond
  inertia. No existing unit test asserts the old relative order — no existing case carries
  two peer labels with non-empty values, each either omitting `client_net_peer_name` from
  its `model.Metric` map or setting it explicitly empty — so the reorder is inert for the
  current suite and is pinned by a new test. Where neither label resolves, the reorder also
  changes the external node's `id` and `name` (raw-value convention), not just the edge's
  target.
- **New bracket-suffix normalisation, applied uniformly to all three labels.** Before the
  existing `:<port>` strip, the chosen peer value is cut at its **first `[`, when that `[`
  is not at index 0**. This runs ahead of `stripPeerAddressPort` so `net.SplitHostPort`
  sees a well-formed authority. The cut is unconditional with respect to *which label*
  supplied the value (no provenance is threaded, consistent with
  `resolve-unknown-server-ip-peer` D4) — the index-0 condition is a property of the value's
  shape, not of its source.
- **The index-0 carve-out protects bracketed IPv6.** `net.SplitHostPort` **succeeds** on
  `[2001:db8::1]:8080` and returns the host without brackets, which `net.ParseIP` accepts
  and the anchor-cluster `ClusterIP` index can hit — so a dual-stack cluster's IPv6 Service
  peer resolves today. An unconditional first-`[` cut would truncate that value to the
  empty string and regress it to `external/<raw>`. Guarding at index 0 leaves the path
  byte-for-byte unchanged while still cutting the observed `host:port[id]` shape, whose `[`
  is never at index 0.
- **External-node naming is unchanged.** When the peer does not resolve, the external node
  still carries the **RAW, wholly unnormalised** label value verbatim — port, bracket suffix
  and all (`external/mongo.com:27017[-181]`). All three labels keep this one convention.
  Consequence, accepted: a host dialed under several distinct bracket ids materialises one
  external node per id.
- **`client_network_peer_port` is deliberately NOT read.** The stable conventions split the
  port into its own attribute; it is not needed to identify a peer (Service resolution keys
  on DNS name or `ClusterIP`, never on port) and it is not used for node naming. Recorded
  here as an explicit non-goal so a future reader does not mistake it for an oversight.
- **Everything downstream is untouched:** classification chain (`classifyK8sDNS` →
  `classifyBareShortName` → anchor-cluster `ClusterIP` lookup → external), the enrichment
  trigger (client resolves to a **real** topology pod AND raw `server` label is literally
  `"unknown"`), `resolveServiceLevel`, the family-wide `service-selects-pod` fan-out, edge
  type derivation, `labels.cluster`, and determinism.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `pod-service-graph`: extend the "Unknown-server peer-label enrichment" requirement — add
  `client_network_peer_address` as a third peer-address label, restate the resolution order
  as `client_server_address` → `client_network_peer_address` → `client_net_peer_name`
  (which reorders the two existing labels), and add the bracket-suffix normalisation step
  (first `[` at index > 0) that precedes port stripping for all peer-address labels. The
  classification-stage wording is also aligned to what the code accepts (the bare-short-name
  stage matches any dot-free non-IP-literal host; it performs no DNS-1123 validation). The
  requirement's trigger conditions, classification chain, anchor rule, external fallback
  naming, and drop cases are otherwise unchanged.
- `pod-service-graph`: also touches the "Virtual sentinel endpoint exclusion (user /
  unknown)" requirement — its enumeration of the peer-address labels the narrowed
  `server!~"user"` matcher exists to expose gains `client_network_peer_address` and is
  reordered to match the new precedence, and its "unresolved client" scenario is restated
  around the new label. No matcher, no sentinel set, and no drop outcome changes.

## Impact

- **Code:**
  - `pkg/build/servicegraph.go` — read `client_network_peer_address` in `parseServiceGraph`;
    widen the `resolveServer` / `resolveUnknownServerPeer` signatures by one string
    (declaring the three peer parameters in precedence order); reorder the peer-value
    precedence pick; add a bracket-suffix strip helper (first `[` at index > 0) invoked
    before `stripPeerAddressPort`.
- **Tests:** `pkg/build/servicegraph_test.go` — new
  `TestParseServiceGraph_UnknownServerPeerLabel_NetworkPeerAddress_*` cases, **including an
  explicitly required bracket-suffix case** (`client_network_peer_address` carrying a
  `host:port[N]` value): one asserting the bracket suffix is stripped before classification
  so an in-cluster `.svc` host still resolves to its Service node, and one asserting the
  external fallback keeps the raw bracketed value verbatim. Plus an all-three-non-empty
  precedence test (the only test that fails on a parameter transposition), a
  `client_server_address` vs `client_net_peer_name` reorder test, IP-literal with bracket,
  a bracketed-IPv6 non-truncation guard, and an old-label bracket regression case.
  `internal/integration` — one fixture exercising `client_network_peer_address` end-to-end
  against the real VictoriaMetrics container.
- **Docs:** `CLAUDE.md`'s "Unknown-server peer-label enrichment" bullet gains the third label,
  the full precedence order, and the bracket-suffix step; `openspec/specs/pod-service-graph/spec.md`
  gets the modified requirement on archive.
- **Contract / compatibility:** no wire-schema break. No new HTTP route, no new node or edge
  type, no new PromQL query and no selector change (the label already rides the same
  series). Behaviour for existing deployments splits four ways:
  - Series carrying exactly one peer-address label with no `[` in its value: **unchanged**.
  - Series whose peer value contains a `[` **not** at index 0 (e.g. `host:port[id]`):
    previously garbage-classified and always missed; now classified on the real host, so
    such an endpoint may resolve to a Service node instead of falling to external.
  - Series carrying both `client_net_peer_name` and `client_server_address` with different
    values: now resolves from `client_server_address` (the D1 reorder). Same-value series
    are unaffected.
  - Series whose peer value is a bracketed IPv6 authority (`[<ipv6>]:<port>`): **unchanged**
    by the index-0 carve-out. The `[<ipv6>]:<port>[<id>]` shape stays `external/<raw>`,
    also unchanged.
- **Dependency:** builds on the archived `resolve-unknown-server-peer-labels` and
  `resolve-unknown-server-ip-peer` changes (both already promoted to
  `openspec/specs/pod-service-graph/spec.md`). No interaction with any other active change.
