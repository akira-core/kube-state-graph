## Context

See proposal.md — Why. Design-relevant current state only:

`parseServiceGraph` (`pkg/build/servicegraph.go`) reads two peer-address labels off each
series and threads them, as two plain `string` parameters, through `resolveServer` into
`resolveUnknownServerPeer`. That helper picks the first non-empty of the two —
`client_net_peer_name` first, `client_server_address` second — strips an optional port via
`stripPeerAddressPort`, then walks a fixed classification chain — `classifyK8sDNS` →
`classifyBareShortName` → anchor-cluster `ClusterIP` lookup (`ipIndex`) → `external`. On a
resolved `(ns, svc)` it delegates to `resolveServiceLevel(anchorCluster, ns, svc)`; on any
miss it calls `r.external(value)` with the **raw** label value.

Three properties of that code shape this design:

- `stripPeerAddressPort` is a thin `net.SplitHostPort` wrapper that returns its input
  unchanged on error. Go's `SplitHostPort` rejects a port segment containing `[` with
  `unexpected '[' in address`, so `mongo.com:27017[-181]` passes through whole.
- `classifyK8sDNS` performs **no** DNS-1123 validation — it splits on `.` and accepts any
  2- or 3-part result (`pkg/build/servicegraph.go:842-849`). The un-stripped value above
  therefore classifies as `service="mongo"`, `namespace="com:27017[-181]"`. That pair never
  exists in `ServicesByNameNS`, so the endpoint lands on `external` anyway — the current
  outcome is correct by accident, not by construction.
- `net.SplitHostPort` **succeeds** on a bracketed IPv6 authority *with* a port and returns
  the host without its brackets — `[2001:db8::1]:8080` → `2001:db8::1`, which `net.ParseIP`
  accepts and which the anchor-cluster `ipIndex` (keyed on the verbatim `cluster_ip` label
  string) can hit. A dual-stack cluster's IPv6 `ClusterIP` peer therefore **does** resolve
  to its Service today. Only the port-less `[2001:db8::1]` form fails
  (`missing port in address`). This asymmetry is what forces the index-0 carve-out in D2.

## Goals / Non-Goals

**Goals:**

- Add `client_network_peer_address` as an alternate *input* to the existing enrichment
  machinery, changing no downstream stage.
- Order the three labels by which value the classification chain can actually resolve,
  rather than by recency of spelling.
- Make the bracket suffix an explicit, tested normalisation step rather than relying on the
  `classifyK8sDNS` miss that currently absorbs it — without regressing the bracketed-IPv6
  authority that resolves today.
- Keep `resolveUnknownServerPeer` provenance-free: no stage may branch on *which* label
  supplied the value.

**Non-Goals:**

- No change to `resolveConnString` (the D29 `"://"` path). Both grammar extensions this
  requirement owns (bare short name, IP literal) were already scoped to the enrichment
  trigger only; the bracket normalisation keeps that same scoping discipline.
- No `client_network_peer_port` read (spec-level non-goal; restated here because the
  implementation must not "helpfully" add it back when widening the signature).
- No new struct to carry the peer labels. See D3.
- No IP-shape filtering (loopback / link-local / RFC1918 rejection), and no change to what
  happens when an IP literal misses the anchor-cluster `ClusterIP` index — it still falls
  to `external/<raw>`. Suppressing specific IP classes, or dropping the endpoint outright,
  is a separate judgement call that contradicts an existing spec scenario and needs its own
  change. See the external-node-cardinality risk.
- No DNS-1123 validation added to `classifyBareShortName`. It accepts any non-empty,
  dot-free, non-IP-literal host today; this change feeds it a couple of new shapes
  (bracket-truncated remainders) but does not tighten it. The delta spec is worded to
  describe what the code actually accepts rather than restating the inherited
  "DNS-1123 label" overstatement — tightening the helper would be a behaviour change
  needing its own scenario.

## Decisions

### D1: Precedence is `client_server_address` → `client_network_peer_address` → `client_net_peer_name`

The pick stays "first non-empty wins outright, never merged". The new label goes in the
**middle**, and the two existing labels **swap** relative to today's order.

**The semconv facts this rests on** (the earlier draft of this design got them wrong, so
they are stated explicitly):

| label on the series | OTel attribute | status | what it holds |
|---|---|---|---|
| `client_server_address` | `server.address` | **stable** | the logical destination as the caller addressed it — domain name, IP, or UDS name |
| `client_network_peer_address` | `network.peer.address` | **stable** | the *socket-level* peer address — by convention an IP (or UDS name) |
| `client_net_peer_name` | `net.peer.name` | **deprecated** → `server.address` | the logical destination name |

`server.address` is **not** a pre-stable spelling of anything — it is the stable successor
*of* `net.peer.name`. `network.peer.address` is a different attribute entirely (the stable
successor of `net.peer.ip` / `net.sock.peer.addr`), not a newer spelling of either existing
label. So "prefer the stable spelling" does not by itself order these three.

**Why this order:** rank by what the classification chain can resolve.

- This resolver is **strong on names**: `classifyK8sDNS` (step 3) and `classifyBareShortName`
  (step 4) both yield a `(ns, svc)` pair that resolves through `resolveServiceLevel`,
  including the family-wide `service-selects-pod` fan-out.
- It is **weak on IPs**: the `ipIndex` lookup (step 5) is deliberately restricted to the
  anchor cluster's own `ClusterIP` set (`resolve-unknown-server-ip-peer` D2). A peer that
  is a pod IP, a loopback (mesh sidecar), a NodePort/LB address, or any off-cluster IP
  matches nothing and lands on `external/<ip>` — worse, an `external/127.0.0.1` node
  collects edges from *every* client pod that reports one, which is pure noise.
- `server.address` is the name-valued attribute; `network.peer.address` is the IP-valued
  one. Preferring the IP over the name systematically trades a resolvable Service node for
  an unresolvable external node whenever both are present.
- Between the two name-valued labels, `server.address` outranks `net.peer.name` because the
  latter is deprecated *in favour of it*: an exporter emitting both is mid-migration and
  will keep `server.address`.

Placing the new label in the middle rather than last is still a real gain: it fires for
every deployment that emits `network.peer.address` **and no** `server.address` — the case
that motivates this change — while never displacing a name when one is present.

**Sub-decision, called out because it exceeds the change's headline scope:** this reorders
the two *existing* labels, so a series carrying both `client_net_peer_name` and
`client_server_address` with **different** values now resolves from the latter. Today's
spec pins the opposite order (`openspec/specs/pod-service-graph/spec.md`, resolution-order
steps 1–2). Justification is the deprecation direction above, and the blast radius is
confined to series carrying both with divergent values. No existing unit test asserts the
old relative order — no existing case carries two peer labels with non-empty values, each
either omitting `client_net_peer_name` from the `model.Metric` map or setting it explicitly
empty — so the reorder is inert for the current suite and must be pinned by a new test
(tasks 2.2b).

**Alternatives considered:**

- **New label first** (the earlier draft). Rejected: it was justified by the incorrect
  premise that both existing labels are deprecated predecessors of the new one, and it
  puts the IP-valued attribute ahead of both name-valued ones — the exact trade the
  resolver's name/IP asymmetry argues against.
- **New label last (zero regression for the existing pair).** Rejected only for the
  `net.peer.name` slot: leaving a deprecated, `server.address`-superseded name ahead of a
  stable attribute has no defence beyond inertia. Ahead of `server.address` it *is* the
  right call, which is what this order encodes.
- **Fall-through: try each label in order until one classifies to `(ns, svc)`.** Rejected
  for now, but the strongest alternative. It removes the name-vs-IP ranking question
  entirely — an unresolvable `server.address` would defer to a `network.peer.address` that
  matches a `ClusterIP`. Costs: it breaks the "first non-empty wins outright, values are
  never merged" invariant the requirement has carried since
  `resolve-unknown-server-peer-labels`; it makes the external fallback ambiguous (which of
  N tried values names the node?); and it turns a single classification into up to three,
  each with its own `noteExternal` accounting. Revisit if field data shows the
  highest-precedence label losing to a lower one at a meaningful rate.
- **Middle position justified by attribute-name similarity** (grouping the two "peer"
  attributes). Rejected: right slot, wrong reason — resolvability, not naming, is what
  orders these.

### D2: Bracket truncation runs before port stripping, uniformly for all three labels, and never at index 0

New pure helper — cut at the first `[` **when that `[` is not at index 0**, discarding it
and the remainder — invoked on the chosen peer value *before* `stripPeerAddressPort`. A
value with no `[`, or whose only `[` is at index 0, is returned unchanged.

**Why before:** the bracket is what makes `net.SplitHostPort` fail. Reversing the order
leaves the port strip a no-op and the bracket in the host, which is the current defect.

**Why the index-0 carve-out:** a leading `[` is the IPv6 bracket form, and
`[<ipv6>]:<port>` is the one bracketed shape `net.SplitHostPort` handles correctly today —
it strips the brackets and yields a host `net.ParseIP` accepts, so such a peer can and does
hit the anchor-cluster `ipIndex` (see Context). An unconditional first-`[` cut would
truncate `[2001:db8::1]:8080` to the empty string and drop a dual-stack cluster's IPv6
`ClusterIP` peers to `external/<raw>` — a regression, not a no-op. Guarding at index 0
leaves that path byte-for-byte unchanged while still cutting the observed
`host:port[id]` shape, whose `[` is never at index 0.

The carve-out is a rule about the **value's shape**, not about which label carried it, so
the provenance-free property below is preserved.

**Why uniform across all three labels:** `resolve-unknown-server-ip-peer` D4 already settled
that this resolver does not track label provenance — the value is picked once and every
later stage sees an opaque string. Scoping the truncation to the new label alone would
require reintroducing provenance (a second return value from the pick, or an `sgTrace`
field) to distinguish cases that behave identically. It also *fixes* the deprecated labels:
a bracketed `client_server_address` today garbage-classifies and always misses.

**Residual case, accepted:** `[<ipv6>]:<port>[<id>]` (a bracketed IPv6 authority, in its
plain non-IPv4-embedded form, that also carries a bracketed identifier) has its first `[`
at index 0, so it passes through untruncated and `SplitHostPort` still fails on the
trailing `[`. The value then reaches `classifyBareShortName`, which **matches** it — that
helper only requires non-empty, no `.`, and not an IP literal, and a plain IPv6 authority
contains colons, not dots — so classification yields `svc = <the whole raw string>`,
`ns = <client pod namespace>`. No such Service exists, `resolveServiceLevel` misses, and
the endpoint lands on `external/<raw>` — exactly what it does today. The `noteExternal`
reason is therefore `unknown_server_peer_anchor_lacks_service`, **not**
`unknown_server_peer_not_k8s_dns`; that matters because the reason counters are the
observability surface this design leans on (see Risks). Not a regression, and not worth a
matched-pair scan until such a value is actually observed. **Caveat:** an IPv6 authority
with an embedded IPv4 suffix (e.g. `[::ffff:10.0.0.5]:8080[-1]`) contains dots, so it fails
`classifyBareShortName`'s dot-free test and instead falls straight through to stage (f)
with reason `unknown_server_peer_not_k8s_dns` — the outcome (`external/<raw>`) is the same,
but the reason bucket differs from the plain-IPv6 case above.

**Alternatives considered:**

- **Unconditional first-`[` cut** (the earlier draft). Rejected on the IPv6 evidence above.
  The draft's stated justification — that `net.ParseIP("[2001:db8::1]")` fails so a
  bracketed IPv6 peer never resolved anyway — only holds for the **port-less** form; with a
  port, `SplitHostPort` removes the brackets before `ParseIP` ever sees the value.
- **Cut at the LAST `[`.** Rejected: it mangles `[<ipv6>]:<port>[<id>]` into
  `[<ipv6>]:<port>` — arguably an improvement for that one shape, but it also changes the
  meaning of the rule from "the authority ends where the annotation begins" to a
  positional heuristic, and it still needs the index-0 reasoning for the plain IPv6 case.
- **Validate DNS-1123 inside `classifyK8sDNS` instead.** Rejected as a larger blast radius —
  `classifyK8sDNS` is shared with the D29 connection-string path, and tightening it would
  change resolution for `"://"` labels that this change has no mandate to touch. It is
  also the wrong layer: the bracket is a transport-level annotation to strip, not a
  malformed DNS name to reject.
- **Regex-strip a trailing `[-?\d+]` specifically.** Rejected: the observed `[-181]` is one
  instrumentation's format and its meaning is not confirmed (see Open Questions). A
  shape-agnostic "authority ends at the first `[`" rule survives a different bracketed
  payload; a digit-shaped regex does not.

### D3: Widen the existing string parameters rather than introduce a peer-label struct

`resolveServer` and `resolveUnknownServerPeer` each take one more `string`.

**Why:** three parameters is the point where a struct starts to look attractive, and it is
still the wrong trade here — `resolveServer` has exactly one caller, the parameters are
consumed by exactly one helper, and a `peerLabels` struct would add a type whose only
purpose is to be destructured immediately. Revisit if a fourth label ever appears.

**Mitigation for the swap hazard this creates:** the three peer parameters are adjacent and
all `string`, and their *order now encodes the D1 precedence*, so transposing two at the
call site compiles, passes every test whose lower-precedence labels are empty, and silently
changes resolution. Two guards, both cheap: declare the parameters in precedence order
(`clientServerAddress, clientNetworkPeerAddress, clientNetPeerName`) so the signature reads
as the rule, and pin the order with the all-three-non-empty precedence test (tasks 2.2 /
2.2b) — which is the only test that fails on a transposition.

### D4: External-node naming stays the RAW value; the truncated host is classification-only

`r.external(value)` continues to receive the untouched label value — bracket suffix, port
and all. Only the classification chain sees the truncated, port-stripped host.

**Why:** the raw-verbatim external convention is stated for every external fallback in this
package, and holding all three peer labels to one rule keeps the resolver free of naming
special-cases. Normalising the node name would also silently merge peers the operator may
want to see separately.

**Accepted consequence:** a host dialed under N distinct bracketed identifiers materialises
N external nodes. This is spec'd explicitly (see the two-identifier scenario in the delta
spec) so it reads as a known trade rather than a bug. Mitigation, if the identifier turns
out to be per-connection and high-cardinality, is a follow-up change to the external naming
convention across all peer labels at once — not a special case bolted onto this one.

## Risks / Trade-offs

- **[Reordering the two existing labels changes resolution for dual-emitting exporters]** A
  series carrying both `client_net_peer_name` and `client_server_address` with *different*
  values now resolves from `client_server_address`, so an existing edge can retarget. This
  is the one part of the change that is not additive. → Bounded: both attributes name the
  same logical destination on a correctly configured exporter (`net.peer.name` is
  deprecated *in favour of* `server.address`), so a divergence is itself a signal.
  **Second-order effect, easy to miss:** when both old labels are non-empty and *neither*
  resolves, D4 pins the external node's `id` and `name` to the raw winning value, so those
  change from `external/<client_net_peer_name>` to `external/<client_server_address>` — a
  node-identity change, not merely an edge retarget. Pinned by an explicit precedence test,
  spec'd as a MODIFIED resolution order, and called out in the CLAUDE.md bullet so the
  behaviour is discoverable rather than surprising.
- **[Bracketed IPv6 authority]** Guarded by the D2 index-0 carve-out — `[<ipv6>]:<port>`
  resolves exactly as it does today. The residual `[<ipv6>]:<port>[<id>]` shape stays
  `external/<raw>`, also as today. Both pinned by tests (tasks 2.10, 2.11) so a later
  "simplification" of the helper to an unconditional cut fails loudly.
- **[Degenerate truncation inputs]** `"[foo]"` has its `[` at index 0, so the carve-out
  leaves it whole and `SplitHostPort` fails — but classification does **not** reject it:
  `classifyBareShortName` matches (non-empty, no `.`, not an IP literal), yielding
  `svc = "[foo]"` in the client's own namespace, which no Service answers, so it ends at
  `external/<raw>`. `"a[b"` truncates to `"a"`, a dot-free label that the same stage
  accepts and that resolves only if the client's own namespace holds a Service named `a` —
  the same outcome the un-bracketed value `"a"` already produces. Neither shape can yield
  an empty host, and both end at `external/<raw>` when no Service matches. Pinned by tasks
  2.11.
- **[Bracket truncation can promote a value into a bare-short-name hit]** Truncation
  reduces a dot-free authority to a bare label, which stage (d) then resolves **in the
  client pod's own namespace**: `mongo:27017[-181]` → `mongo:27017` → `mongo` →
  `(ns=<client ns>, svc=mongo)`. Today that value garbage-classifies (it reaches
  `classifyBareShortName` as the whole raw string) and always misses; after this change a
  client namespace that happens to hold a Service named `mongo` gets an **external database
  peer resolved to an unrelated in-cluster Service** — a wrong edge, not merely a missing
  one. → The aggressive part is stage (d) itself, which predates this change and already
  makes exactly this trade for an un-bracketed `mongo:27017`; truncation only widens the
  set of shapes that can reach it. Accepted as inherited, not new, and spec'd with a
  scenario so it is a pinned decision rather than an emergent surprise. The stricter
  alternative — DNS-1123-validating `classifyBareShortName` — is a behaviour change outside
  this change's mandate (see Non-Goals).
- **[External-node cardinality from IP-valued peers]** `network.peer.address` is by
  convention an **IP**, and stage (e) only matches an IP that is a `ClusterIP` *in the
  anchor cluster*. For exactly the population this change targets — emitting
  `network.peer.address` and **no** `server.address` — every other IP shape becomes an
  `external/<ip>` node: pod IPs (headless Services, direct pod-to-pod, StatefulSet
  addressing), sidecar loopback (`127.0.0.1`, Istio inbound `127.0.0.6`), node IPs
  (NodePort / hostNetwork), and off-cluster LB addresses. An `external/127.0.0.1` node
  collects edges from *every* client pod that reports one. D1 uses this failure mode to
  justify the **ordering**; it is booked here as a risk of **admitting the label at all**,
  because under D1 the new label fires only where `server.address` is absent — precisely
  where the noise is unmitigated. → Not mitigated in this change (see the "no IP-shape
  filtering" non-goal). The candidate mitigation, recorded so it is not rediscovered
  mid-implementation: **drop** an IP literal that misses the anchor `ClusterIP` index
  instead of externalising it. It is provenance-free (keys on the value's shape, so it does
  not violate `resolve-unknown-server-ip-peer` D4) but it **contradicts an existing spec
  scenario** — *IP-literal peer address absent from the anchor cluster's Service set —
  external* — so it belongs in its own change. Observable meanwhile via the
  `unknown_server_peer_ip_literal_no_match` reason counter; see the Open Question.
- **[Preferring a name over an IP loses a resolvable IP peer]** Where `server.address`
  carries an unresolvable name and `network.peer.address` carries a matching `ClusterIP`,
  D1's first-non-empty rule yields `external/<name>` even though the lower-precedence label
  would have resolved. → Accepted for now; the fall-through alternative in D1 is the fix if
  this shows up in the field. `noteExternal`'s `unknown_server_peer_*` reason counters plus
  the logged `host` / `peer_address` pair make the rate observable without new telemetry —
  with the caveat that a degenerate or dot-free value reaches `classifyBareShortName` and
  therefore lands in the `unknown_server_peer_anchor_lacks_service` bucket, not
  `unknown_server_peer_not_k8s_dns`. Only a genuinely dotted non-`.svc` FQDN lands in the
  latter.
- **[External node cardinality]** See D4's accepted consequence.
- **[Widening a signature invites the port label back in]** Someone adding
  `client_network_peer_port` "for symmetry" would break the spec's explicit non-goal. →
  Guarded by a spec scenario asserting a port-only series is dropped, plus a code comment
  on the read site stating the omission is deliberate.

## Migration Plan

1. `pkg/build/servicegraph.go`
   - Read `client_network_peer_address` in `parseServiceGraph` alongside the existing two;
     comment that `client_network_peer_port` is deliberately not read.
   - Widen `resolveServer` / `resolveUnknownServerPeer` by one string (D3), declaring the
     three peer parameters in precedence order; update both call sites and the doc comments.
   - Reorder the first-non-empty pick to `client_server_address` →
     `client_network_peer_address` → `client_net_peer_name` (D1).
   - Add the bracket-truncation helper — first `[` at index > 0 only (D2) — and call it
     before `stripPeerAddressPort`.
2. `pkg/build/servicegraph_test.go` — new `..._NetworkPeerAddress_*` cases per tasks.md,
   including the explicitly required bracketed-value pair (resolves, and external-keeps-raw),
   the all-three-non-empty precedence test, and the bracketed-IPv6 non-truncation guard.
3. `internal/integration` — one fixture with `client_network_peer_address` end-to-end.
4. `CLAUDE.md` — extend the "Unknown-server peer-label enrichment" bullet.

**Rollback:** revert the resolver change wholesale. Self-contained — no selector change, no
new query, no edge-ID namespace change, no persisted state. Reverting only part of it is
safe too (unlike `resolve-unknown-server-peer-labels`, whose selector and Go branch were
coupled), since nothing here spans the PromQL and Go layers. The one ordering caveat: the
D1 reorder and the new label's read are independent reverts — dropping the reorder alone
restores the pre-change behaviour for deployments that emit no `network.peer.address`.

## Open Questions

- **What does the bracketed suffix (`[-181]`) actually denote?** Answering it would let the
  external-naming trade in D4 be re-evaluated with real cardinality data: a stable
  per-endpoint identifier is harmless, a per-connection one argues for normalising the
  external node name. Deferrable — the truncation rule is shape-agnostic and correct either
  way, so neither the specs, the approach, nor the task breakdown depends on the answer.
- **For deployments emitting only `network.peer.address`, what share of peers are
  non-`ClusterIP` IPs?** This sizes the external-node cardinality risk above and decides
  whether the "drop an IP literal that misses the anchor index" mitigation is worth its own
  change. Measurable today from the `unknown_server_peer_ip_literal_no_match` reason
  counter against the resolved-endpoint total — no new telemetry needed. Not blocking: the
  current rule (externalise) is the status quo for the two existing labels, so shipping
  without the answer changes no established behaviour.
- **Do the exporters in play emit `server.address` alongside `network.peer.address`?** If
  they never co-occur, D1's ordering is moot in practice and the fall-through alternative
  costs nothing to skip. If they co-occur often *and* disagree, the fall-through alternative
  becomes the better rule. Not blocking: the ordering is a one-line change under either
  answer, and the spec pins whichever one ships.
