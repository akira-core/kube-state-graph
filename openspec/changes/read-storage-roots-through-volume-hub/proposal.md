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

The same shape produces a wrong-looking answer. A filer is routinely shared by
Kubernetes clusters in several zones or environments, but `az` / `env` are
required and single-valued, so `?ontap_cluster=X` draws X's aggregates and SVMs
as roots and draws NO path whenever the claims on X belong to a different zone or
environment than the one the request named. Operators read that as "the graph
cannot trace a filer back to its pods".

`volume_labels` already carries every storage-side coordinate in one row
(`cluster`, `node`, `aggr`, `svm`, `volume`), and a dynamically provisioned PV is
named `pvc-<claim UID>` — a name that is recoverable from the FlexVol name and
unique across every cluster. The rooted Harvest rows can therefore name the claims
directly, and the claim chain can be read forward from them, in every zone.

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
4. **BREAKING (storage-graph, hub mode only): `az` / `env` no longer bound the
   Kubernetes side.** In hub mode every kube-state-metrics, kubelet and `ALERTS`
   query drops the `az` / `env` matchers and is dispatched to every backend
   serving its family; `az` keeps selecting the `harvest` backends. A filer shared
   across zones or environments is drawn with every claim on it. `cluster` and
   `namespace` keep narrowing as today. `az` and `env` stay required and
   single-valued on the request.
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
9. **Alert matching agrees on zone.** Because hub mode reads every zone's
   `ALERTS`, an alert's `az` / `env` pair must agree with the zone of the node it
   attaches to. NetApp controllers and aggregates take their zone from the
   `az` / `env` their Harvest series carry, Kubernetes objects from their cluster
   identity; a known, different zone never matches, and an unknown one falls back
   to today's label comparison. Also fixes a no-`cluster` alert on a request-zone
   pod turning ambiguous once another zone's same-named pod is loaded.

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

- `storage-graph-api`: **Storage-flow graph endpoint** — `az` / `env` stop
  bounding the Kubernetes side in hub mode. **Storage build reads only what the body
  draws** — the five claim-keyed families leave the unrestricted class in hub mode.
  **Storage-side roots restrict the Harvest topology read** (replaced by **Storage-side roots narrow the Harvest topology read per component**) — `svm=` is restricted
  with owner completion, `node=` no longer disables the restriction beside a
  storage-exclusive root. New requirement **Storage-side roots read the claim chain
  through the volume hub** — extraction, claim-keyed scopes, selector relaxation,
  routing, bound fallback, static-PV limitation, coverage signal.
- `cluster-topology-source`: **Topology series consumed**, **Request-scoped
  upstream selectors**, **Backend routing composes with request-scoped selectors**
  and **Application-rooted recovery reads of the owner and annotation families** —
  hub mode drops the `az` / `env` matchers and `az` routing on the Kubernetes
  families, and reads the claim families by reference.
- `alert-overlay`: **Label-set matching to graph nodes** — a zone-agreement rule
  over the alert's `az` / `env` and the candidate node's zone.
- `netapp-storage-graph`: **Harvest legs under request-scoped selectors** — the
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
- `pkg/build/build.go` — relaxed selector and routing in hub mode.
- `pkg/build/alerts.go`, `pkg/build/netapp.go` — per-ONTAP-cluster zone sets and
  the zone-agreeing alert match.
- `pkg/promql` — `scopedLabel` entries for the five claim-keyed families; an
  SVM-rooted and an aggregate-completion renderer for `volume_labels`; an optional
  querier-source upgrade that binds ONE routing snapshot with `az` applied to the
  `harvest` family only.
- Docs: `docs/BREAKING.md`, `docs/upstream-metrics.md`,
  `docs/netapp-harvest-preconditions.md`, `docs/upstream-backend-routing.md`,
  `README.md`, `README.zh-tw.md`, `CLAUDE.md`, OpenAPI description of
  `/v1/storage-graph`.
- No new request parameter, node type, edge type, flag or dependency. `/v1/graph`
  is untouched.
