## Why

The PVC → ONTAP aggregate join is keyed on `volume_name`, a label stock Harvest
does not emit. Every deployment must first install a Prometheus relabel rule
that maps each FlexVol to the PersistentVolume it backs. Deployments that have
not done so — the common case, since no Harvest template produces the label —
get a complete, plausible graph with the entire storage chain silently missing:
no `pvc-to-netapp-aggr` edge, no aggregate, no controller, no PVC `svm`.

The mapping the relabel rule encodes is not deployment-specific knowledge that
only the operator holds. Trident derives the FlexVol name from the PV name by a
published, deterministic transformation (ONTAP volume names cannot contain `-`,
so the PV name is embedded with `-` replaced by `_`, behind a configurable
`storagePrefix`). The backend can perform that derivation itself and match
against the label Harvest already emits, removing the precondition entirely.

Re-sourcing the join exposes a second problem in the same code path. The six
`qos_*` workload families are fetched unfiltered, but `resolveNetAppStorage`
consults them only for claims that already matched hop A. ONTAP collects a QoS
workload for every volume on the filer, the vast majority of which back no
PersistentVolumeClaim, so most of what is fetched is provably discarded before
it is read. Once hop A has run, the exact set of FlexVol names worth asking
about is known, and the QoS read can be scoped to it.

## What Changes

### Join key

- **BREAKING**: the Harvest `volume_labels` series and the six `qos_*` workload
  series are read at their **stock `volume` label** (the ONTAP FlexVol name).
  The `volume_name` label is no longer read anywhere in the codebase. A
  deployment whose relabel rule stamps `volume_name` keeps working only if its
  FlexVol names also satisfy the configured rewrite; the label itself is
  ignored. See "Migration" below.
- The join stops being a single label equality and becomes **derive-then-match**:
  the PVC's resolved `volumename` (the bound PV name) is rewritten into a match
  token, and that token is matched against each Harvest series' `volume`.
- **New**: an operator-configurable, ordered list of regex rewrite rules
  producing the match token from the PV name. Default: a single rule replacing
  `-` with `_`. The default deliberately does NOT prepend `trident_` — the
  prefix is per-backend configurable in Trident and the match mode below makes
  knowing it unnecessary.
- **New**: an operator-configurable match mode deciding how the token is
  compared against `volume` — suffix, exact, contains, or the token used as a
  full regular expression. Default: suffix, which is prefix-agnostic (it needs
  no knowledge of `storagePrefix`) while still rejecting derived volumes whose
  name extends past the PV name, such as a clone suffixed after it.
- The rewrite runs **per claim**, not per Harvest series, so one PV name yields
  exactly one match token and no collision rule over rewritten values is needed.
- Multiple Harvest series matching one claim collapse through the **existing**
  lexically-smallest `(ontap_cluster, aggr)` rule — unchanged determinism, now
  reachable through a broader match.

### Harvest read shape

- **The six `qos_*` legs become a second wave**, issued only after hop A has
  resolved which FlexVol names the loaded claims actually match, and scoped to
  exactly those names with an anchored `volume=~"…"` alternation composed with
  the existing `lun=""` matcher.
- Because the alternation grows with the claim count and VictoriaMetrics caps
  query length, each family's second-wave read is **batched**: the matched name
  set is sorted, de-duplicated and chunked deterministically, and one query is
  issued per chunk. Chunks of one family are issued concurrently; the merged
  vector is what the reader sees, so `resolveNetAppStorage` is unchanged by the
  batching.
- **Only those six legs move.** `volume_labels`, the aggregate/controller health
  families and the hop-C policy-ceiling families stay in the first wave,
  unfiltered and parallel with every kube-state-metrics and kubelet leg. Their
  cardinality is bounded by volume, aggregate and policy-group counts rather
  than by workload counts, so narrowing them would buy little and cost a third
  wave.
- **Zero matched names short-circuits the second wave entirely** — no `qos_*`
  query is issued at all, mirroring the existing rule that a selector loading
  no pods or services issues no `traces_service_graph_*` queries.
- **Degradation is now per batch, not per family.** Each chunk stays optional
  (log-and-continue, as every Harvest leg is today); a failed chunk costs I/O
  measurements only for the claims in that chunk, and the deterministic
  chunking makes which claims those are a pure function of the matched set.
- Hop A and hop B stop being independently reachable: a hop-A miss now means
  hop B is never asked. This matches the existing reader, which already
  consults the QoS index only for claims that resolved an aggregate, but the
  "three hops degrade independently" wording in the spec is restated.

### Unchanged

- The `{lun=""}` volume-granularity contract, the three-hop result structure
  (topology / measurement / ceiling), the two coverage warnings
  (`netapp_volume_join_miss`, `netapp_qos_join_miss`) and their per-family
  gating, backend routing of the Harvest family, and the response body.
- **Migration**: the relabel rule becomes unnecessary and may be deleted. A
  deployment whose FlexVol names do not embed the PV name at all (a
  hand-maintained mapping the relabel rule was encoding) loses its storage
  chain and must express the mapping through the rewrite rules instead, or
  accept the miss. `docs/BREAKING.md` records this.

## Capabilities

### New Capabilities

None. The change re-sources an existing join and re-shapes an existing read; it
introduces no new node type, edge type, metric family, or observable attribute.

### Modified Capabilities

- `netapp-storage-graph`: the label contract for `volume_labels` and the six
  `qos_*` families replaces `volume_name` with the stock `volume`; the
  deployment-precondition requirement is replaced by a configurable-rewrite
  requirement; the PVC-to-aggregate join requirement is restated as
  derive-then-match; a new requirement covers the scoped, batched QoS read and
  its per-batch degradation; the QoS volume-granularity rationale and the
  join-coverage scenarios are restated against the new key.
- `cluster-topology-source`: the PVC `svm` label requirement and its scenarios
  reference the Harvest join by `volume_name`; they are restated against the
  derived token and stock `volume` label. The `volumename` label's own
  resolution is unchanged.

## Impact

- `pkg/build/netapp.go` — the two label read sites (`volume_labels` indexing and
  `indexQoSFamily`), plus the new token derivation and matcher.
- `pkg/build/topology.go` — the `pvcVolume` claim list gains the derived token;
  `ReadTopology`'s flat errgroup gains a two-wave Harvest sub-pipeline.
- `pkg/build/build.go` — `build.Options` carries the rewrite rules and match
  mode, following the existing `LabelKeys` precedent.
- `pkg/promql` — a new, narrowly-scoped rendering entry point for the
  volume-scoped QoS queries. `Render` itself stays a pure function of
  `(query, window, keys, selector)` and is not widened.
- `internal/config` — new flags plus `KSG_*` environment variables and
  startup validation (each rewrite pattern must compile; the match mode must be
  one of the four values; the batch budget must be positive), following the
  `--az-label` precedent.
- Self-metrics: the `qos_*` families now record several
  `kube_state_graph_upstream_query_duration_seconds` observations per build
  under one `query` label value. No metric or label is added or removed.
- Tests: `pkg/build/netapp_test.go`, `pkg/build/routedquerier_test.go`,
  `pkg/promql/queries_test.go` (including the `{lun=""}` granularity pin and the
  empty-selector render baseline), `internal/config`, and
  `internal/integration` (`TestPVCNetAppHarvestJoin`). Golden bodies are
  expected to stay byte-identical once fixtures move to `volume`.
- Docs: `docs/netapp-harvest-preconditions.md` (the precondition it documents
  disappears), `docs/upstream-metrics.md`, `docs/BREAKING.md`.
- Downstream: the `kube-state-graph-demo` integration repository stamps
  `volume_name` from `tools/cmd/netapp-faker` and asserts it in
  `scripts/verify.sh`. That repository needs its own follow-up change; it is
  out of scope here.
- No new dependency. No change to the HTTP request or response surface.
