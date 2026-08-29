## Why

`resolve-pod-application-from-controller` added seven topology legs that resolve a
pod's ArgoCD Application from its controller. Two of its decisions make a purely
decorative attribute able to take the whole API down.

Every one of the seven is queried at its bare name and issued with `fetch`, so it
both costs more than it needs to and fails hard when it costs too much:

- `kube_job_owner` is read in full and then filtered in Go to
  `owner_kind="CronJob" AND owner_is_controller="true"` (`resolveJobCronJobOwners`).
  In an estate with CronJob history limits, CI Jobs and Helm hook Jobs, that is
  six figures of series fetched per request to keep a low-single-digit percentage.
- The six annotation families are read in full and then filtered in Go to a
  non-empty `annotation_argocd_argoproj_io_tracking_id` (`resolveApplications`).
  kube-state-metrics emits one series per workload object whether or not the
  annotation is allowlisted, so an operator who enables the collectors but not the
  annotation allowlist pays for every Deployment, ReplicaSet and Job in the estate
  and resolves nothing.

`kube_replicaset_annotations` and `kube_job_annotations` are the two whose
cardinality **accumulates over time** rather than tracking the live object count —
one series per retained ReplicaSet under every Deployment's `revisionHistoryLimit`,
one per Job retained by `successfulJobsHistoryLimit` / `failedJobsHistoryLimit`.
When either exceeds VictoriaMetrics' `-search.maxUniqueTimeseries` or
`-search.maxSamplesPerQuery`, `fetch` propagates the error, the errgroup cancels,
and **every `/v1/graph` request returns 5xx** — the entire multi-cluster graph is
unavailable because a string decoration on bare-ReplicaSet pods could not be
fetched.

## What Changes

- **Push both reader-side filters into PromQL as fixed, request-invariant
  metric-selection contracts** — the same class as `lun=""`, `condition="Ready"`,
  `type=~"ExternalIP|InternalIP"` and the service-graph sentinel, rendered ahead of
  any request-scoped matcher and never replaced by one:
  - `kube_job_owner{owner_kind="CronJob",owner_is_controller="true"}`
  - each of the six annotation families gains
    `{annotation_argocd_argoproj_io_tracking_id!=""}`

  Both are **output-preserving**: the Go readers already drop exactly the series
  these matchers exclude, and they drop them *before* the missing-`cluster` tally
  runs, so not even the diagnostic counters move.

- **Demote the two accumulating-cardinality legs to `fetchOptional`** —
  `kube_replicaset_annotations` and `kube_job_annotations` — so an upstream query
  failure logs and yields an empty vector instead of failing the build. Caller
  cancellation (build timeout, client disconnect) still fails the request, via the
  existing `optionalQueryFatal`. The other five legs keep `fetch`.

- **`RawSeriesCount` for the six annotation families changes meaning** (observable
  telemetry, not API): it now counts series that actually carry a tracking-id
  rather than every workload object of that kind. This makes the counter equal to
  the `count(kube_<kind>_annotations{annotation_argocd_argoproj_io_tracking_id!=""})`
  probe already documented as the allowlist verification query. Not a breaking API
  change — `RawSeriesCount` appears only in the build's debug log.

- Revises two decisions of the archived `resolve-pod-application-from-controller`:
  its design **D4** (`fetch` for all seven) and its **tasks 1.2** ("with no fixed
  selector"). Neither is a spec-level reversal — the spec never forbade a fixed
  selector, and it already declares all seven families OPTIONAL.

No new query, metric, node type, edge type, attribute, or `labels` key. No change
to any response body, and no change to which pods resolve an Application.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `cluster-topology-source`: two requirements change.
  - **Topology series consumed** — the seven controller-annotation series gain
    their fixed selectors in the declared series list, and the optionality
    paragraph gains the distinction between the five legs whose query error still
    fails the build and the two whose error degrades.
  - **Request-scoped upstream selectors** — the enumerated set of fixed,
    request-invariant selectors composed with request matchers grows by the seven
    new ones.

## Impact

- `pkg/promql/queries.go` — seven `Render` cases gain a fixed selector; two new
  selector constants alongside `qosVolumeGranularitySelector` /
  `serviceGraphSentinelSelector`.
- `pkg/promql/testdata/render-baseline.txt` — seven rendered lines change.
- `pkg/build/topology.go` — two `g.Go(fetch(...))` become
  `g.Go(fetchOptional(...))`. Fan-out stays 37 legs; the split moves from
  22 `fetch` + 15 `fetchOptional` to 20 + 17.
- `pkg/promql/selector_test.go`, `pkg/build/topology_test.go`,
  `pkg/build/netapp_test.go` — new pins for both contracts.
- `README.md`, `README.zh-tw.md`, `docs/upstream-metrics.md`,
  `docs/kube-state-metrics-preconditions.md` — the query-error matrix and the
  per-series tables carry the fixed selectors and the revised abort/degrade split.
- No dependency, configuration, flag, or HTTP surface change.
