# Design — refine-family-fanout-resolution

## Context

`cross-cluster-service-fallback` (archived 2026-06-12) introduced the D29
cluster-family fan-out: a `"://"` endpoint classified to `(namespace, service)`
resolves to one service node per family cluster holding it, anchored on the
UID-recovered client-pod cluster, else the raw trace `cluster` label. Two
field defects follow from that shape; this change fixes both at the candidate
selection layer of `resolveServiceLevel`, touching nothing upstream
(classification, anchoring, memoisation) or downstream (materialisation, edge
emission, dedupe).

## Decisions

### D-1: Endpoint-backed pruning is *preferential*, not absolute

Rule: partition the matched candidates by endpoint-backedness
(`len(EndpointsByService[{cluster, ns, svc}]) > 0`). If the backed partition is
non-empty, resolve to it alone; otherwise resolve to ALL candidates (today's
behaviour verbatim).

Rejected alternative — *unconditionally skip every zero-endpoint candidate*
(the literal user phrasing). Two failure modes:

- A deployment that has not allowlisted
  `endpointslices=[kubernetes.io/service-name]` has an empty
  `EndpointsByService` for every service. Absolute skipping would silently
  disable ALL connection-string service resolution there — a hard regression
  for the common default-KSM install.
- A single-cluster service that momentarily has zero ready endpoints (rollout,
  crash-loop) would flip-flop between a service node and `external/<label>`
  across builds. The existing contract ("a known service with zero backing
  endpoints still materialises the service node, with no fan-out edges") is
  deliberate operator signal: the Service exists, traffic was observed, no
  backends — keep it.

Preferential pruning fixes the reported defect exactly (an endpointless
sibling is dropped *because a better candidate exists*) with zero behaviour
change in every degenerate case.

**Per-cluster visibility gate** (added after adversarial review): "zero
backing pods" is pruning evidence only in a cluster that has endpoint
visibility at all — i.e. at least one `EndpointsByService` entry for ANY
service in that cluster. A cluster with zero entries (its KSM lacks the
`kubernetes.io/service-name` allowlist — staged rollout, config drift — or
the slice→pod join produced nothing) is treated as *unknown*, never as
*unbacked*, and its candidates are always kept. Without this gate a
mixed-allowlist fleet would prune every service of the non-allowlisted
cluster the moment a sibling cluster is backed — the rejected-alternative
regression reintroduced per-cluster. The proxy is imperfect (an allowlisted
cluster where literally no service has any joined endpoint also reads as
invisible) but errs in the safe direction: no pruning, pre-change behaviour.

### D-2: Unknown-family fallback engages only for *unanchorable* series

Rule: the loaded-holder lookup runs whenever the anchor's family key is not
the family key of ANY loaded cluster. "Loaded cluster" = any cluster
evidenced by the build's topology (`ClustersObserved` ∪ service clusters ∪
pod-label clusters), with the `"unknown"` bucket sentinel explicitly
excluded — the bucket is an absence of identity, not a cluster, so an
`"unknown"` anchor always counts as unanchorable. (A real cluster literally
named `unknown` is pathological and deliberately out of scope.)

Rejected alternative — *fall back on every family miss*. That re-opens the
collision problem family scoping was built to solve: a caller known to be in
`prod-3` whose family lacks the service would resolve against e.g.
`staging-1`'s same-named service, fabricating cross-family attribution from
nothing but a name. With the gate, a known-family caller keeps today's
external fallback; only series that carry NO usable cluster identity (missing
label + non-pod client, or a bogus label naming a non-existent family) gain
resolution — and only when the name is globally unambiguous.

### D-3: Holder set = LOADED clusters only; ambiguity = more than one family

The fallback's holder set is the LOADED clusters holding
`(namespace, service)` — `"unknown"`-bucketed service entries (cluster label
missing on `kube_service_info`, bucketed by `mc.bucket`) are EXCLUDED from it
(`svcGlobal` skips them at build). Two reasons, both found by adversarial
review:

- *Poisoning*: one unlabelled duplicate of a `prod-1` service would flip the
  holder families from `{prod-0}` to `{prod-0, unknown}` = ambiguous,
  silently disabling the fallback for exactly the series it was built for.
- *Fabrication*: a bogus-label anchor must not resolve into the `unknown`
  pseudo-cluster (a service node `unknown/<ns>/<svc>` built from two pieces
  of non-identity).

The fallback resolves iff the loaded holder set spans exactly ONE family key;
the whole holder set then becomes the candidate set (D-1 pruning applies
after, including the visibility gate). Two-or-more families →
`external/<label>`. Family-granular (not cluster-granular) uniqueness matches
the mesh model: deployments of one family are one logical service, so
`prod-1`+`prod-2` holders are NOT ambiguous — the fan-out emits both, exactly
as an in-family anchor would.

**Precedence vs the `"unknown"` direct hit**: unknown-bucketed services DO
stay in the family index (`svcCandidates`) under family key `"unknown"`, so
an `"unknown"` anchor's primary lookup can hit them. Identified holders take
precedence: when ANY loaded cluster holds the name, the fallback's answer
(single-family holders, or nil on ambiguity) REPLACES the direct hit — the
pseudo-cluster never shadows real deployments and an ambiguous loaded set
yields external, not `unknown/<ns>/<svc>`. The direct hit survives ONLY when
no loaded cluster holds the name at all: a fully-unlabelled deployment
(every series bucketed to `"unknown"`) keeps connection-string resolution —
its graph is already consistently the `unknown` pseudo-cluster, and breaking
it would regress label-less single-cluster installs.

### D-4: Both rules live in `resolveServiceLevel`; indexes are per-parse

- `svcGlobal map[nsSvcKey][]svcCandidate` — `(namespace, service)` → LOADED
  holder candidates (`"unknown"`-bucketed entries excluded, D-3), built in
  the same loop as the existing `svcCandidates` family index, each bucket
  sorted by cluster (D6 determinism).
- `knownFamilies map[string]struct{}` — loaded family keys, built once per
  parse from the three evidence sources above.
- `epVisibleClusters map[string]struct{}` — clusters with ≥1
  `EndpointsByService` entry; the pruning evidence gate (D-1).

The `resolveConnString` memo key `(familyCluster, label)` stays valid:
resolution remains a pure function of `(label, anchor family, topology)`.
Pruning is a deterministic filter over an already-sorted slice; the
uniqueness check is an order-free scan. No `materializeService`,
`addServiceEdge`, edge-emission, or dedupe change.

### D-5: No wire change; registry descriptions only

`may_cross_cluster` values are already correct (`pod-calls-service: true` —
the fallback can also produce cross-cluster edges, same derivation). The
`pod-calls-pod` and `pod-calls-service` `description` strings are extended to
mention pruning and the unknown-family fallback (golden refresh). No graph-api
spec delta: catalogue scenarios pin fields, not description prose.

## Risks / Trade-offs

- **Pruning hides Service objects that exist but are endpointless** when a
  backed sibling exists. Accepted: the edge to them depicted untraversable
  traffic; the all-unbacked case still surfaces them.
- **Fallback resolves on family-uniqueness, not proof**: a series with a bogus
  label could be attributed to the single family holding the name even if the
  real caller's cluster is simply absent from VictoriaMetrics. Accepted:
  strictly better than `external/<label>` (the alternative attribution is "not
  in the cluster fleet at all"), and gated to series with no usable identity.
- **`unknown`-named real cluster** breaks the bucket-exclusion assumption.
  Documented, not handled.

## Migration

None — additive behaviour refinement; same request/response contracts. Golden
`edge-types.json` refresh for description text only.
