# Proposal

## Why

`GET /v1/storage-graph` requires `az` and `env` to be **single-valued**, so an
operator investigating storage that spans zones or environments has to issue
one request per `(az, env)` pair and stitch the Sankey bodies together
client-side. `GET /v1/graph` has accepted repeated `az` / `env` since
push-request-filters-upstream, and the router already dispatches a zone SET
(`Router.QuerierFor` normalises `sel.AZ`), so the restriction is a parser and
spec contract, not a routing limit.

The contract existed to guarantee "the body describes one estate: a filer
shared across zones or environments is never merged into one diagram". That
guarantee does not need a single zone: Kubernetes ids are already composed
`<az>-<env>-<cluster>`, and every NetApp id is qualified by its ONTAP cluster
(`netapp/<oc>/…`). The operator guarantees **ONTAP cluster and controller names
are unique** across the estate, so no NetApp id can merge two filers.

Aggregate and SVM names, however, are NOT unique — `aggr1` / `svm0` recur on
every filer. A bare `aggr=` / `svm=` root already fans out to every filer in the
zone, and #31 removed the only way to qualify it (`ontap_cluster=` + `aggr=` is
now two root kinds). Widening the zone set widens that fan-out, so this change
also adds a qualified root form.

## What Changes

- **Repeatable `az` / `env`.** Each stays **required** (at least one non-empty
  value; `missing_az` / `missing_env` unchanged) but MAY repeat; the
  `invalid_scope` "must be single-valued" rejection is removed. The pair is a
  **Cartesian product**, rendered exactly as on `/v1/graph`: sorted,
  de-duplicated values, a single value as `key="v"`, several as one anchored
  `key=~"a|b"`. A combination the estate does not hold simply contributes
  nothing. `az` selects the zone-routed backends as a set.
- **The body is the union of its zones.** Replace the "one estate" sentence
  with the invariant this change pins: for a fixed root, the body of a
  multi-zone request equals the union of the single-zone bodies of each
  selected `(az, env)` combination — no node merges across zones, no flow
  weight sums across zones, and every alert still matches zone-agreeingly
  (D11, unchanged).
- **Zone-agreeing claim → FlexVol join.** To make the union invariant hold, a
  claim's `volume_labels` candidates SHALL be restricted to series whose
  `az` / `env` agree with the claim's own zone (an unknown zone on either side
  never excludes — the D11 rule). Without it, a multi-zone read lets a
  zone-a claim pick a same-token FlexVol on a zone-b filer (a Trident clone
  collision) that its single-zone body could never see. The join is shared, so
  this also applies to an unfiltered `/v1/graph`; bodies change only in an
  estate with a cross-zone FlexVol-token collision.
- **Qualified aggregate / SVM roots.** `aggr=` and `svm=` additionally accept
  `<ontap_cluster>/<name>`, naming exactly one component; the bare form keeps
  its meaning (every filer in the selected zones). Bare and qualified values
  MAY be mixed within the one root kind and are OR-combined. ONTAP aggregate
  and SVM names cannot contain `/`, so the split is unambiguous; a value with
  more than one `/` or an empty segment is 400 `invalid_scope`. The qualified
  seed reads `volume_labels` one query per ONTAP cluster (cluster equality +
  name alternation — the existing `(ONTAP cluster, name)` keying rule); the
  flowless gauge / materialisation reads and the projection match on the pair.
- `ontap_cluster=` and `ontap_node=` need no qualified form (unique names).
- Document the uniqueness precondition in
  `docs/netapp-harvest-preconditions.md`: two filers sharing an ONTAP cluster
  name (or a controller name) merge into one node on BOTH endpoints.

Not in scope: a paired `(az, env)` parameter (combinations are Cartesian);
naming a FlexGroup PVC's filer on the PVC itself (`labels.svm` stays a bare
name — a PVC `ontap_cluster` label is NOT an option, because the Cytoscape
serialiser synthesises a `storage-cluster` group from any node carrying that
key).

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `storage-graph-api`: "Storage-flow graph endpoint" (repeatable `az` / `env`,
  the union invariant replaces "one estate", and every row-to-object key uses
  the cluster identity rather than the raw `cluster`), "One root kind per
  request" (qualified `aggr` / `svm` value forms; the projection's root
  matching follows from it, so "Storage-reachability projection" is
  unchanged), "Every root kind is tracked from its own tier to its claims"
  (qualified seed reads; "Zone" paragraph over a zone set), "Roots are always
  materialised when the upstream knows them" (existence of a qualified root),
  "Storage build reads every family by reference" (qualified aggregate gauge
  reads).
- `netapp-storage-graph`: "PVC-to-NetApp-aggregate edge join" (zone-agreeing
  candidate filter) and "Harvest legs carry the request zone and environment"
  (a zone SET under `/v1/storage-graph`).

## Impact

- `pkg/kubegraph/parse.go` — `exactlyOne` → at-least-one for `az` / `env`.
- `pkg/graph/storagescope.go` — qualified `aggr` / `svm` refs in
  `StorageRoots`; `pkg/graph/project_storage.go` root matching on the pair.
- `pkg/build` — seed / flowless reads for qualified roots
  (`volumelabelscope.go`, `topologyplan.go`, `flowless.go`), the zone filter in
  `resolveNetAppStorage`'s candidate gathering, and an audit of hub code that
  assumes one zone (the archived hub design notes a helper that "only ever sees
  one zone" and compares the raw `cluster` alone).
- `pkg/promql` — phase-1 / flowless renderers grouped per ONTAP cluster; chunk
  budgets already charge `RequestMatcherCost`, which grows with the zone
  alternation (a large root set reaches the `invalid_scope` cap sooner).
- `docs/BREAKING.md` (the relaxed 400 and the `/v1/graph` join tightening),
  `docs/netapp-harvest-preconditions.md`, OpenAPI parameter docs; parser tests,
  a multi-zone union test (unit + integration), goldens unchanged for
  single-zone requests.
