## Why

In a large estate `GET /v1/storage-graph` still fails with VictoriaMetrics'
`the number of matching timeseries exceeds <N>; either narrow down the search
or increase -search.maxUniqueTimeseries` — now on `kube_job_owner`, a REQUIRED
leg the storage build issues across the whole estate although the body consults
it only for the handful of Job-owned pods that mount a claim. After
`harden-topology-read-cardinality` the storage build reads its pods by
reference, but every family those pods REFER TO — the eight controller-owner /
controller-annotation legs and the four Kubernetes-node legs — is still read
unrestricted. Four of the eight controller legs accumulate one series per
retained object (`kube_replicaset_owner`, `kube_replicaset_annotations`,
`kube_job_owner`, `kube_job_annotations`); a CronJob firing every few minutes
puts hundreds of Job objects into the upstream's per-day index whatever the
request window, so the leg crosses a memory-derived series cap in an estate
whose live object count is unremarkable. The limit applies to the series a
query SELECTS, so the only lever the build holds is to name the objects it
needs — which it already knows, one wave earlier.

## What Changes

1. **The storage build reads controllers by reference.** After the scoped pod
   read lands, the eight controller legs are issued restricted to the owner
   names the loaded pods actually carry, per kind: `kube_replicaset_owner` and
   `kube_replicaset_annotations` on `replicaset`, `kube_job_owner` and
   `kube_job_annotations` on `job_name`, `kube_statefulset_annotations` on
   `statefulset`, `kube_daemonset_annotations` on `daemonset`; then — because a
   Deployment is only known once its ReplicaSet resolved, and a CronJob once its
   Job did — `kube_deployment_annotations` on `deployment` and
   `kube_cronjob_annotations` on `cronjob` in a following stage. A kind no loaded
   pod is owned by issues no query for its families at all.
2. **The storage build reads Kubernetes nodes by reference.** The four
   `kube_node_*` legs are issued restricted to the `node` names the loaded pods
   are scheduled on plus the request's `node=` roots, in parallel with the
   controller read. An empty scope issues nothing.
3. **Every data-derived restriction composes with the request matchers and the
   family's own fixed selector**, is chunked under the existing byte budget,
   merged in chunk order, and keeps the family's existing error class: a chunk
   of a required family fails the build as its unrestricted read would; a chunk
   of a degrading family (`kube_replicaset_annotations`,
   `kube_job_annotations`) degrades on its own, and a degraded
   `kube_job_annotations` chunk suppresses the Job → CronJob hop for the build
   exactly as a degraded unrestricted read does.
4. **The storage first wave shrinks from 30 to 18 legs.** What remains
   unrestricted is what the body draws whole or what the scopes are computed
   from: the claim families (`kube_pod_spec_volumes_persistentvolumeclaims_info`
   is the root of every scope; `kube_persistentvolumeclaim_info`,
   `kube_persistentvolumeclaim_annotations` and the two kubelet families
   co-move with it and gain nothing from a restriction), the Harvest inventory
   (roots must be drawable whether or not a claim reaches them; SVMs are named
   by `volume_labels` alone) and `ALERTS` (firing only).

Every storage-graph body is byte-identical to today's. `queryDims`, the request
surface, the flag set, the `/v1/graph` fan-out (37 / 43 legs) and the dependency
set are unchanged. No new node type, edge type, request parameter or flag.

This change is sequenced AFTER `harden-topology-read-cardinality`: its
storage-graph-api delta MODIFIES the "Storage build reads only what it draws"
requirement that change ADDS, and its cluster-topology-source delta builds on
that change's "Topology series consumed" wording. Sync or archive that change
first.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `storage-graph-api`: **Storage build reads only what it draws** — the "every
  other family is issued exactly as `/v1/graph` issues it" clause is replaced
  by the by-reference rules for the eight controller families and the four
  Kubernetes-node families (staged scopes, empty-scope rule, chunking, error
  class, node roots), and the list of families read unrestricted is stated
  exhaustively with the reason each stays so (items 1–4).
- `cluster-topology-source`: **Topology series consumed** — the
  endpoint-dependent paragraph names the storage build's by-reference families
  (pods, Kubernetes nodes, controllers) instead of "two are read restricted to
  the pods"; the per-family tally rule gains "a family whose scope came out
  empty is absent, not zero".

## Impact

- `pkg/promql/scope.go` — the scopeable-query table gains the eight controller
  families and the four node families with the label each is restricted on;
  `RenderScoped` renders the family's fixed selector (today only the two pod
  families are scopeable and neither carries one), sharing one fixed-selector
  table with `Render` so `render-baseline.txt` cannot move.
- `pkg/build/topologyplan.go` — the storage plan's first wave drops the twelve
  by-reference families; the tally reports a by-reference family iff at least
  one chunk was issued.
- `pkg/build/podscope.go` — the pod wave signals completion; a shared
  chunk-issue-merge helper is extracted for the new waves.
- `pkg/build/nodescope.go`, `pkg/build/controllerscope.go` (new) — the node
  wave and the two-stage controller wave, both gated on the pod wave; the
  controller wave's second stage gated on its first.
- `pkg/build/topology.go` — `readTopology` launches the two new waves under a
  by-reference plan; `resolvePodOwners` / `resolvePodApplications` are untouched
  (the scopes are supersets of every key they consult).
- Tests: storage fan-out pins (18 first-wave legs; per-kind formula in
  design.md), scoped-render pins per family, parity pins covering every
  controller kind, bare ReplicaSets, Jobs with and without their own
  annotation, unscheduled pods and `node=` roots; chunk-order, empty-scope,
  chunk-failure and cross-namespace-collision tests per wave; component
  fixtures for the storage suite; integration storage suite.
- Docs: `docs/upstream-metrics.md` (storage fan-out section),
  `docs/BREAKING.md` (queries change, bodies do not), `README.md`,
  `README.zh-tw.md`, `CLAUDE.md`, the `--netapp-qos-scope-batch-bytes` help
  text.
