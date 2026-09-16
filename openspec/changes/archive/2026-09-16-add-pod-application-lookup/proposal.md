## Why

A pod's ArgoCD Application is resolved only inside a full graph build:
`parseTopology` joins every pod's controller owner to the controller-annotation
families in one batch. A Go consumer that already knows one pod and wants its
Application has no entry point short of `Builder.Build`, which issues the whole
37-leg topology fan-out plus the service-graph read to answer a question that
needs two to five kube-state-metrics series.

The rules are not obvious enough to re-implement safely: the Application lives
on the pod's controller, not on the pod; a ReplicaSet owner collapses to its
Deployment; a Job without its own tracking-id falls through to its CronJob; and
the tracking-id pick skips malformed values before the lexically-smallest
tie-break. A consumer hand-rolling that against `Router.QueryLabels` would
produce a plausible, wrong answer the first time one of those cases appears.

## What Changes

- **NEW** `build.ResolvePodApplication(ctx, LabelQuerier, PodApplicationRequest)
  (string, error)`: resolves one pod's Application on demand with the same rules
  the topology build applies, issuing only the kube-state-metrics series that
  pod's resolution path needs.
- **NEW** `build.LabelQuerier` — the one-method upstream seam
  (`QueryLabels(ctx, promql.LabelQuery)`), satisfied by `*promql.Router`, with a
  generated mock under `pkg/build/mocks/`.
- The pod is named by the **same `(cluster, namespace, pod)` tuple** the
  topology build keys pods on; `cluster` is the raw label, the value
  `?cluster=` takes. `az` and `env` are optional, as on the graph request: an
  absent one is pinned from the pod-owner series, and a pod found under more
  than one `(az, env)` is an error rather than a guess. A given or pinned zone
  routes the queries to that zone's `ksm` backends through the existing family
  contract.
- Every query is family `ksm`. No new query family, `Query` constant, PromQL
  rendering path, HTTP route, flag, or dependency.
- The batch resolver's tracking-id usability check is extracted into one helper
  shared by both paths, so the two tie-breaks cannot drift. The build's output
  is unchanged.

## Capabilities

### New Capabilities

- `pod-application-lookup`: on-demand resolution of one pod's ArgoCD
  Application through the routed label query.

### Modified Capabilities

None.

## Impact

- `pkg/build/podapplication.go` (new), `pkg/build/topology.go` (shared
  `usableTrackingID` helper and `argoTrackingIDLabel` constant, behaviour-free).
- `.mockery.yaml` and `pkg/build/mocks/mock_label_querier.go`.
- `docs/upstream-backend-routing.md` and `CLAUDE.md` gain a pointer to the new
  entry point.
