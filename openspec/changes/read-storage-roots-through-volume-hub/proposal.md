## Why

A `/v1/storage-graph` request rooted at a filer component reads the claim side of
the chain from the wrong end. The Harvest read is already narrowed to the rooted
components (`scope-volume-labels-by-storage-root`), but the Kubernetes side is
still read as a whole-estate scan: `kube_persistentvolumeclaim_info`, the
claim-binding family, `kube_persistentvolumeclaim_annotations` and the two kubelet
volume-stats families are first-wave legs under `az` / `env`, and the pod scope is
every pod that mounts ANY claim in that zone and environment — whether or not its
claim lives on the rooted filer. The pod, Kubernetes-node and controller waves
then scale with the zone, not with the answer, and the projection discards almost
all of it.

`volume_labels` already carries every storage-side coordinate in one row
(`cluster`, `node`, `aggr`, `svm`, `volume`), and a dynamically provisioned PV is
named `pvc-<claim UID>` — a name that is recoverable from the FlexVol name and
unique across every cluster. The rooted Harvest rows can therefore name the claims
directly, and the claim chain can be read forward from them — still inside the
request's zone and environment.

## What Changes

1. **Volume hub.** When a `/v1/storage-graph` request carries at least one
   storage-exclusive root (`ontap_cluster=`, `aggr=`, `svm=`) and the Harvest
   restriction applies, the build reads the claim chain FROM the rooted
   `volume_labels` rows: FlexVol name → candidate PV names → claims → bindings →
   pods → Kubernetes nodes and controllers. This is "hub mode".
2. **PV candidate extraction.** Every suffix of a FlexVol name that starts with
   `pvc_` at the start of the name or right after a `_` yields one candidate PV name
   (`_` rewritten to `-`), e.g. `trident_pvc_ab12_…` → `pvc-ab12-…`. Extraction is a
   **candidate generator, never a judge**: a candidate that names no PV loads
   nothing, and the existing forward derive-then-match join still decides every
   pick. The operator-configured forward derivation is unchanged.
3. **Claim-keyed families are read by reference in hub mode.**
   `kube_persistentvolumeclaim_info` is restricted on `volumename` to the
   candidates; the claim-binding family, `kube_persistentvolumeclaim_annotations`
   and the two kubelet volume-stats families are restricted on
   `persistentvolumeclaim` to the loaded claims. All five leave the first wave.
   When a scope would exceed a fixed number of chunks, the family is read with no
   scope and filtered in the reader instead.
4. **Hub mode stays in the request's zone.** Every query of a hub build carries
   the request's `az` / `env` matchers where it did before and is sent only to the
   backends the request's `az` selects, exactly as outside hub mode. A filer
   shared across zones is drawn with the requested zone's claims only. (An
   earlier revision of this change read every zone; it was withdrawn before
   release — design D7.)
5. **`svm=` is restricted too.** Phase 1 of the Harvest read gains a
   `{svm=~…}` query (AND-ed with `ontap_cluster=` when present), unioned with the
   `aggr=` / cluster query. A new **owner-completion** phase re-reads
   `volume_labels` for every aggregate an SVM query touched but did not read whole,
   so the owning-controller vote still runs over all of the aggregate's series.
6. **`node=` no longer disables the restriction** when a storage-exclusive root
   is present: the projection ANDs the node root with the storage roots, and every
   controller is still drawable from the unrestricted `node_*` / `aggr_*` families.
   A request whose only storage-side root is `node=` keeps today's read.
7. **Static PVs are out of reach of hub mode.** A claim bound to a PV whose name is
   not `pvc-…` is not found from a storage root. `/v1/graph`, rootless and
   workload-rooted storage requests keep the forward join and are unchanged.
   Documented as a known limitation.
8. **Hub coverage signal.** A new aggregated warning reports when the rooted
   Harvest rows yielded no PV candidate, or candidates that named no claim — the
   case that is silent today.
9. **Alert matching agrees on zone.** An alert's `az` / `env` pair must agree
   with the zone of the node it attaches to, wherever one build holds several
   zones' objects or alerts (an unfiltered `/v1/graph`, a catch-all backend). NetApp controllers and aggregates take their zone from the
   `az` / `env` their Harvest series carry, Kubernetes objects from their cluster
   identity; a known, different zone never matches, and an unknown one falls back
   to today's label comparison. A no-`cluster` alert on a pod whose name another
   zone reuses resolves by zone instead of being dropped as ambiguous.
11. **BREAKING: Harvest queries carry the request's `az` / `env`.** Every NetApp
   Harvest query — the unrestricted legs and every restricted `volume_labels` /
   `qos_*` read — renders the request's `az` and `env` matchers, like every
   kube-state-metrics query. Every Harvest series must carry the configured
   `az` / `env` labels; one without them no longer reaches a filtered build.

**Sequencing.** This change goes AFTER `fail-storage-graph-on-any-leg-error`.
That change renames three `storage-graph-api` requirements this change modifies
or removes and edits two `cluster-topology-source` requirements this change also
edits; once it is archived, this change's deltas are rebased onto the result
(task 0.1) before this change is archived. Its error classes are that change's
fail-closed classes.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `storage-graph-api`: **Storage build reads only what the body
  draws** — the five claim-keyed families leave the unrestricted class in hub mode.
  **Storage-side roots restrict the Harvest topology read** (replaced by **Storage-side roots narrow the Harvest topology read per component**) — `svm=` is restricted
  with owner completion, `node=` no longer disables the restriction beside a
  storage-exclusive root. New requirement **Storage-side roots read the claim chain
  through the volume hub** — extraction, claim-keyed scopes, unchanged matchers
  and routing, bound fallback, static-PV limitation, coverage signal.
  **Application roots compose with the Harvest restriction** — citation of the
  renamed requirement.
- `cluster-topology-source`: **Topology series consumed** — a hub-mode storage
  build reads the claim families by reference. **Request-scoped upstream
  selectors** and **Backend routing composes with request-scoped selectors** —
  `az` / `env` render on every Harvest query.
- `alert-overlay`: **Label-set matching to graph nodes** — a zone-agreement rule
  over the alert's `az` / `env` and the candidate node's zone.
- `netapp-storage-graph`: **Harvest legs under request-scoped selectors** is
  replaced by **Harvest legs carry the request zone and environment** — `az` /
  `env` matchers on every Harvest query, the labelling precondition, and the
  "narrowed by reference through the loaded claims" statement gains the hub-mode
  direction (claims loaded FROM the rooted Harvest rows), and the `volume_labels`
  scope list gains the SVM and owner-completion restrictions.

## Impact

- `pkg/build/topologyplan.go` — plan gains the hub decision (`hubMode`) and the SVM
  roots; `issuesFirstWave` withholds the five claim-keyed families in hub mode.
- `pkg/build/volumelabelscope.go` — SVM phase-1 query, owner-completion phase,
  PV candidate extraction.
- `pkg/build/claimscope.go` (new) — the claim-keyed by-reference reads, their
  bound fallback and reader-side filter.
- `pkg/build/topology.go` — wave wiring: claim families gated on phase 1, pods
  gated on the claim families.
- `pkg/build/alerts.go`, `pkg/build/topology.go` — per-ONTAP-cluster zone sets and
  the zone-agreeing alert match.
- `pkg/promql` — `scopedLabel` entries for the five claim-keyed families; an
  SVM-rooted and an aggregate-completion renderer for `volume_labels`;
  `dimsHarvest = dimAZ | dimEnv` (the routing-only bit is removed) and the
  restricted Harvest renderers take `(keys, sel)`.
- Docs: `docs/BREAKING.md`, `docs/upstream-metrics.md`,
  `docs/netapp-harvest-preconditions.md`, `docs/upstream-backend-routing.md`,
  `README.md`, `README.zh-tw.md`, `CLAUDE.md`, OpenAPI description of
  `/v1/storage-graph`.
- No new request parameter, node type, edge type, flag or dependency. `/v1/graph`
  is untouched.
