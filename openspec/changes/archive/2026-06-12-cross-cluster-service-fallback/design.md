# Design — cross-cluster service fallback for connection-string resolution

## Context

D29 Stage 0 (connection-string resolution) today resolves a `"://"` label whose
host classifies to `(service, namespace)` against **exactly one** cluster's
topology: the trace-source cluster carried on the series' `cluster` label.
`sgResolver.resolveServiceLevel` performs a single
`ServicesByNameNS[{traceCluster, ns, svc}]` lookup; a miss falls through to an
`external/<label>` node. The original rationale was that `.svc.cluster.local`
is in-cluster DNS, so the addressed Service must live in the caller's cluster.

That assumption breaks under a service mesh. In the deployment environments
this API serves, a mesh (east-west gateways / multi-primary Istio-style
routing) resolves the *same* Kubernetes Service DNS name across cluster
boundaries: a pod in `prod-03` dialing `nats://my-nats.messaging.svc:4222` may
be served by the `messaging/my-nats` Service deployed in `prod-12`. The same
`(namespace, service)` is commonly deployed in several clusters of one
environment at once. When the Service happens not to exist in the caller's own
cluster, today's lookup misses and the endpoint degrades to an external node —
losing the service node, its `service-selects-pod` fan-out, and precisely the
cross-cluster dependency picture the API exists to provide.

The environments use **numbered cluster naming**: clusters belonging to one
environment share a name shape that differs only in digits (`prod-03`,
`prod-12`; `staging-1`, `staging-2`). The mesh routes within such a family,
never across families (`staging-1` traffic is never served by `prod-1`). This
naming regularity is what makes a safe, knob-free widening of the Stage 0
lookup scope possible.

## Goals / Non-Goals

**Goals**

- Keep `pod-calls-service` edges (and their service nodes +
  `service-selects-pod` fan-out) for mesh-routed cross-cluster service calls,
  instead of degrading them to `external/<label>` nodes.
- Stay byte-deterministic: same upstream data → byte-identical response body.
- No new flags, env vars, or config knobs; no PromQL query changes.

**Non-Goals**

- Mesh-telemetry-based actual-destination resolution (e.g. consuming Istio's
  `destination_cluster` or equivalent labels to pin the one real target
  cluster). Future work; this change deliberately surfaces the ambiguity
  instead of guessing a single winner.
- Per-pod resolution of `"://"` endpoints. A connection string still never
  resolves to a pod (D29 contract unchanged).
- Changing `service-selects-pod` semantics. It remains directed, intra-cluster
  fan-out from a service to its own cluster's backing pods.
- Caching, indexing for scale, or any cross-request state.

## Decisions

### D-A. Cluster-family key: digit-run normalisation

Two clusters are in the same **family** iff their names are equal after
replacing every maximal ASCII digit run `[0-9]+` with a single `0` sentinel:

- `prod-03` → `prod-0`, `prod-12` → `prod-0` — same family.
- `staging-1` → `staging-0`, `prod-1` → `prod-0` — different families.
- Bare-number names `1`, `2`, `42` all normalise to `0` — one family.
- A name with no digits normalises to itself — its family is exact-name match.

The sentinel is itself a digit, which makes the mapping collision-free without
escaping: every `0` in a key came from a digit run (any literal digit is part
of a run), and a non-digit byte can never equal the sentinel. A non-digit
sentinel such as `#` would collide with a cluster name literally containing it
(`prod-#` vs `prod-1`) — review finding, fixed by sentinel choice.

The key is a hardcoded pure string function (`clusterFamilyKey` in
`pkg/build`), no config knob.

**Rationale.** Digit runs are exactly where same-environment cluster names
vary in the target deployments, while the non-digit shape (environment,
region, product prefix) is what separates mesh-routing domains. Collapsing
each *maximal* run (not each digit) makes `prod-03` and `prod-3` equal too,
which matches operator intent. A pure function keeps determinism trivial and
keeps the "no knobs" contract.

**Alternatives considered.**

- *Configurable family regex / mapping flag* — rejected: adds a knob the
  project's design philosophy avoids, invites per-deployment drift, and a
  wrong mapping silently corrupts the graph. The hardcoded rule degrades
  gracefully (no digits → exact-name → status quo).
- *Common-prefix matching* (`prod-*` matches `prod-eu-1`) — rejected: too
  loose; `prod-1` and `prod-canary` would family-match across genuinely
  distinct routing domains.
- *Exact-name only (no family)* — that is the status quo; see D-B.

### D-B. Stage 0 fan-out across the trace-source cluster's family

When connection-string resolution runs (endpoint pod UID empty AND label
contains `"://"`, host classified by the existing K8s DNS grammar to
`(service, namespace)` — detection and grammar unchanged), the lookup scope
widens from the trace-source cluster to its **family**:

- Candidate clusters = all loaded clusters whose family key equals the
  trace-source cluster's family key, iterated in **sorted order**. The
  trace-source cluster is not special — it is just one family member.
- For **each** candidate cluster `c` where `ServicesByNameNS[{c, ns, svc}]`
  exists: materialise that cluster's service node (`id="<c>/<ns>/<svc>"`,
  `labels={cluster, namespace}`, `ipaddress=[cluster_ip]` unless headless
  `"None"`) plus the intra-cluster `service-selects-pod` fan-out from
  `EndpointsByService[{c, ns, svc}]` — exactly today's per-cluster
  materialisation, run once per match.
- The endpoint resolves to the **set** of matched service-node IDs; one
  `pod-calls-service` edge is emitted per (resolved source, service ID) pair.
  One upstream series can therefore produce N edges.

**Rationale.** With mesh routing, the honest answer to "which cluster served
this call?" is "some member of the family that deploys this Service" — the
trace data does not say which. Emitting an edge to every family deployment is
the truthful representation of that ambiguity; family scoping (rather than
all-clusters) prevents same-name collisions across unrelated environments
(`staging` and `prod` both running `messaging/my-nats` must not cross-link).

**Alternatives considered.**

- *Status quo: trace-cluster-only lookup* — rejected: it is the bug being
  fixed; mesh-routed calls to a Service absent from the caller's cluster
  degrade to external nodes and the cross-cluster dependency disappears.
- *Unique-match-only* (resolve only when exactly one family cluster holds the
  service, else external) — rejected: in the target environments the same
  `(namespace, service)` is *commonly* deployed in several family clusters at
  once, so the unique-match condition would almost never hold and the fix
  would not fire precisely where it is needed.
- *Local-first, fan-out only on miss* (keep trace-cluster hit as sole result;
  fan out to the family only when the local lookup misses) — rejected: makes
  the local deployment mask real cross-cluster routing (the mesh routes to
  any family member even when a local Service exists), produces a
  discontinuity where deploying the Service locally silently deletes
  cross-cluster edges, and complicates the rule for no determinism or
  precision gain.
- *ALL-cluster fan-out without family scoping* — rejected: same-name Services
  in unrelated environments (staging vs prod, tenant A vs tenant B) would
  cross-link, fabricating dependencies the mesh can never realise. Family
  scoping bounds the fan-out to the actual routing domain implied by the
  user's numbered-cluster naming.

Family fan-out wins because it matches the user environment exactly: numbered
cluster families, mesh routes to any family member, family scoping avoids
cross-environment same-name collisions.

### D-C. Zero family matches → external fallback, unchanged

When no family cluster holds the `(namespace, service)` — or the host never
classified to one — the endpoint falls back to `external/<label>`
(`labels={}`, verbatim label as `name`): exactly today's behaviour. The
"unresolvable" condition merely narrows from "absent from the trace cluster"
to "absent from every family cluster".

**Rationale / alternatives.** Dropping the edge instead was rejected when D29
was designed and nothing here changes that calculus: an unresolvable URL is
still a real dependency worth surfacing. No new node type is warranted — the
external node already models "addressed by URL, not in topology".

### D-D. Both sides `"://"` → cross product

When BOTH sides of a series are `"://"` labels resolving to sets, the resolver
emits the cross product of edges: each resolved source ID × each resolved
target ID. Non-`"://"` sides resolve to a single ID exactly as today (pod via
UID, synth pod, or `external/<label>`), i.e. a one-element set.

**Rationale.** The two endpoints' ambiguities are independent — any family
deployment of the source service may call any family deployment of the target
service — so the cross product is the faithful closure. Special-casing (e.g.
pairing same-cluster matches only) would encode a routing assumption the data
does not support.

**Alternatives considered.** *Same-cluster pairing only* — rejected: mesh
routing is precisely the case where client and server deployments live in
different family members. *Cap or collapse to one synthetic edge* — rejected:
loses per-target-cluster resolution and would need an arbitrary
representative; the product is already bounded by (family size)².

### D-E. Edge `labels.cluster` rule unchanged

`labels.cluster` is present (= the trace-source cluster) iff the **client**
side resolved to a pod; omitted when the client side is a service or external
node. Fan-out does not perturb this: a `"://"` client side never resolves to a
pod, so all its cross-product edges omit the key; a pod client side yields the
same single trace cluster on every fan-out edge from that series.

**Rationale.** The label answers "which cluster originated the RPC", which is
a property of the caller, not of how many candidate targets resolved. Changing
it would be a wire-contract break for zero information gain (the target
cluster is already on the target node's `labels.cluster`).

### D-F. `/v1/edge-types` catalogue: `pod-calls-service` may cross clusters

The `pod-calls-service` catalogue entry flips `may_cross_cluster` from `false`
to `true`. `service-selects-pod` stays `may_cross_cluster: false` — it is
always intra-cluster, since a service and its backing pods (joined via that
cluster's own `EndpointsByService`) share a cluster by construction.
`pod-calls-pod` and `pod-mounts-pvc` entries are untouched.

**Rationale.** The catalogue is a static contract describing what the builder
can produce; the builder can now produce a `pod-calls-service` edge whose
source pod and target service carry different `labels.cluster`. This is a
contract change but not a wire-shape change — cross-cluster status was always
derived by comparing source/target node `labels.cluster`, and that derivation
is unchanged.

**Filter-projection note.** The promoted graph-api "Filter parameters"
requirement's unified edge-retention rule (retain an edge when at least one
endpoint survives node filtering; re-add the missing partner if it passes the
non-cluster filters) is **edge-type-agnostic** in both the spec text and the
implementation (`graph.Project`/`filterEdges`), so it already covers
cross-cluster `pod-calls-service` edges with no behaviour change. The rule is
intentionally NOT re-engineered here; the graph-api spec delta only
generalises the requirement's enumerated example (b) — formerly phrased around
cross-cluster `pod-calls-pod` partner *pods* — so the re-added partner is
documented as "a pod or a service node".

### D-G. Determinism preserved

- Candidate clusters are iterated in **sorted order** (the loaded-cluster list
  is already sorted; a derived index must sort its value lists).
- The existing `(src, tgt)` pair-dedupe map in `parseServiceGraph` absorbs the
  fan-out: each cross-product pair is one key, and the existing
  lexically-smaller `srcCluster` tie-break still resolves conflicting trace
  clusters independent of vector arrival order.
- Edge IDs stay UUIDv5 over `<type>|<source>|<target>` with the unchanged
  compiled-in namespace — each fan-out edge has a distinct `(source, target)`
  and therefore a stable, distinct ID.
- Output remains byte-identical for the same upstream data: the resolution is
  a pure function of (series labels, topology), and the serialiser's
  `SortNodes`/`SortEdges` ordering is untouched.

**Alternatives considered.** None viable — determinism is a load-bearing
project invariant (golden tests, future cache); any design failing it is
disqualified at the door.

### D-H. No PromQL changes, no new knobs

The service-graph and topology queries are byte-identical to today (the D30
sentinel selector included). Family filtering happens entirely in-memory at
the resolution layer over the already-loaded `Topology`, preserving the
"no filters pushed to PromQL" contract: every build still loads every cluster
upstream holds, and the family rule consumes that superset.

**Rationale.** All facts needed (every cluster's `ServicesByNameNS` /
`EndpointsByService`) are already fetched by the existing fan-out, because
builds are never cluster-filtered upstream. Pushing family scoping into
PromQL would couple query shape to a string-normalisation rule and break the
projection-over-graph contract a future cache relies on.

**Alternatives considered.** *A flag to disable family fan-out* — rejected:
the no-digits degradation (D-A) already yields status-quo behaviour for
deployments without numbered families, so the knob would guard nothing.

### D-I. Family anchor: UID-recovered client cluster, then the trace label

The family is anchored on the **client side's authoritative cluster**, in
order of preference:

1. **UID-recovered client-pod cluster** — when the client side resolved to a
   topology pod (directly or via the global UID index). The trace `cluster`
   label "is frequently missing ... or disagrees with the client pod's real
   topology cluster" (resolveClient's own rationale), and `.svc` DNS is
   in-cluster relative to the CALLER, whose authoritative cluster is the
   resolved pod's. Without this, deployments with missing labels would bucket
   to `"unknown"`, find zero family candidates, and the fan-out feature would
   silently never fire (review finding).
2. **Raw trace label** — when the client side is not a topology pod (a
   `"://"` client, a D27 external, or a synth pod whose cluster IS the label).

Edge `labels.cluster` is unaffected — it stays the raw trace label per D9.

A series missing its `cluster` label whose client side is ALSO non-pod is
bucketed to `"unknown"`; `clusterFamilyKey("unknown")` is `"unknown"` (no
digits), so its family matches no real cluster — zero candidates →
`external/<label>` fallback. This residual case is deliberate: with neither a
resolvable client pod nor a label, there is no basis for choosing a family,
and guessing one would fabricate cross-environment edges.

### D-J. Resolution order and D33 guard keep their shape

The four-step per-endpoint resolution order is unchanged in structure:
(1) connection-string resolution → (2) pod-UID resolution / synth-pod →
(3) missing-UID human-label fallback → (4) drop. The only delta is **inside
step 1**: its lookup scope changes from the trace cluster to the cluster
family, and it may now yield **multiple** service nodes instead of at most
one. Steps 2–4 still yield exactly one ID (or drop). The D33 self-loop UID
guard (`normalizeSelfLoopUIDs`) is untouched — it runs before resolution,
operates only on UIDs and `"://"` presence, and its output feeds step 1
exactly as before; a cleared side now simply enjoys the wider step-1 scope.

**Rationale.** The order encodes hard-won contracts ("populated UID means
pod", "`"://"` never reaches the human-label fallback") that this change has
no reason to disturb; confining the delta to step 1's scope keeps the blast
radius reviewable and the existing spec scenarios mostly intact.

### Implementation shape

- `clusterFamilyKey(name string) string` — new pure helper in `pkg/build`
  (digit-run → `0`), unit-tested directly.
- `sgResolver.resolveConnString` and `resolveEmptyUID` change signature to
  return `[]string` (resolved node IDs). Non-`"://"` paths (external fallback,
  D27 promotion) return one-element slices; the empty slice means drop.
- `resolveServiceLevel` becomes a family iteration: compute the trace
  cluster's family key once, iterate candidate clusters in sorted order
  (either by filtering the sorted loaded-cluster list, or via a small
  `Topology` index keyed by `(namespace, service)` → sorted cluster list built
  alongside `ServicesByNameNS`), and call the existing `materializeService`
  per hit. Materialisation and `service-selects-pod` fan-out per cluster are
  byte-for-byte today's logic.
- `resolveClient` / `resolveServer` return ID sets; `parseServiceGraph` emits
  the cross product `srcIDs × tgtIDs` into the existing `(src, tgt)` pairs
  map (dedupe + `srcCluster` tie-break unchanged). `srcIsPod` is uniform per
  series (a pod source is always a single ID), so the aggregate struct is
  unchanged.
- `pkg/graph` edge-type registry (`pkg/graph/registry.go`): the
  `pod-calls-service` entry flips `may_cross_cluster` to `true` AND its
  `Description` string is rewritten — the current text ("Always intra-cluster
  — the service is resolved in the trace-source (client) cluster.") would
  contradict the flipped boolean in the served `/v1/edge-types` catalogue.
  New text describes cluster-family fan-out: the service may resolve in any
  cluster of the trace-source cluster's family, so the edge may be
  cross-cluster. The `pod-calls-pod` `Description` (its "a '://' string that
  does NOT resolve to a known service falls back to an 'external' node"
  clause now means "no family cluster holds it") and the `service-selects-pod`
  `Description` (gains "intra-cluster within the resolved service's own
  cluster" phrasing) are reviewed in the same edit. The registry is served
  verbatim, so the `internal/api/testdata/golden/edge-types.json` golden
  refresh picks up the new strings.

## Risks / Trade-offs

- [Non-numbered cluster names get no fan-out — the family is exact-name only]
  -> This is the status quo by construction, not a regression: a digit-free
  name's family key equals the name itself, so step 1 looks up exactly the
  trace cluster, today's behaviour. Documented in the spec delta.
- [Speculative edges to family clusters that hold the Service but did not
  actually serve the traffic] -> Bounded by family size (clusters sharing the
  name shape, typically a handful) and by actual Service deployment; this is
  honest ambiguity — the trace data cannot identify the real destination, and
  showing every routable candidate is more truthful than guessing one or
  showing an external node. Mesh-telemetry destination resolution is named
  future work that would tighten this.
- [Bare-number cluster names (`1`, `2`, …) all collapse into one `0` family]
  -> Accepted consequence of the pure rule; such naming implies the clusters
  are one numbered family anyway, and fan-out is still bounded by which of
  them deploy the Service. Called out explicitly in tests.
- [Golden / integration test churn] -> Expected and contained: catalogue
  golden (`may_cross_cluster` flip + rewritten `Description` strings) plus any
  graph goldens with `"://"` fixtures refresh via the standard `-update` flow;
  `pkg/graph/service_test.go` hard-asserts the old contract
  (`if podCallsService.MayCrossCluster { t.Error(...) }`) and MUST have that
  assertion inverted for `pod-calls-service` (the parallel
  `service-selects-pod` intra-cluster assertion stays); new integration
  fixtures (client in `prod-1`, Service only in `prod-2`) pin the new
  behaviour.
- [CLAUDE.md and the archived design doc say `pod-calls-service` is "always
  intra-cluster" — divergence corrupts future changes] -> CLAUDE.md's
  load-bearing rules MUST be updated in this change (the "always
  intra-cluster" wording for `pod-calls-service`, and the D29 rule text's
  trace-cluster scoping); the archived `add-k8s-pod-graph-api` design doc
  stays immutable as history, with this change's artifacts as the superseding
  record. OpenAPI annotations mentioning intra-cluster are updated alongside.
- [Cross product on double-`"://"` series inflates edge count] -> Bounded by
  (family size)² per series and deduped by `(src, tgt)`; in practice
  double-`"://"` series are rare (they require both UIDs absent) and family
  sizes are small.

## Migration Plan

Additive behaviour change; no wire-shape change. Existing responses only gain
nodes/edges (service nodes where external nodes previously appeared, plus
extra `pod-calls-service` / `service-selects-pod` edges); no field is renamed,
removed, or retyped, so clients need no migration. The `/v1/edge-types`
`may_cross_cluster` flip is a metadata value change within the existing
schema.

- Deploy is a plain binary roll; no data, schema, or upstream-query migration
  exists (the server is stateless per request and PromQL is unchanged).
- Rollback is equally plain: redeploy the previous binary; no rollback data
  concerns (nothing persisted, nothing renegotiated with upstream).
- Golden refresh: `go test ./internal/api -update` after the registry and
  resolver changes land, committed with the change.
- Docs: update `openspec/specs/pod-service-graph/spec.md` and
  `openspec/specs/graph-api/spec.md` deltas per the proposal, and the
  CLAUDE.md wording noted in Risks.

## Open Questions

None — all decision points (family-key rule, fan-out scope and ordering,
zero-match fallback, cross-product emission, label rules, catalogue flip,
determinism, no-knob constraint, `unknown` bucketing, resolution-order shape)
were explicitly resolved by user input during proposal review.
