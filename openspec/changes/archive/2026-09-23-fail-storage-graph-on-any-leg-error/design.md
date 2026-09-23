## Context

See proposal.md for motivation. Today every leg carries one of three error
classes, decided per family and shared by both endpoints:

- required — `fetch` (first wave) / `legRequired` (by-reference chunks): any
  error fails the build; `classifyReadError` maps it to `ReasonUpstream`
  (502), `ReasonTimeout` (504) or `ReasonCanceled`;
- optional — `fetchOptional` / `legOptional`, plus the Harvest-specific loops
  (`issueVolumeLabelsQueries`, the QoS chunk loop): error logged, counted, empty
  vector, build continues unless `optionalQueryFatal` sees the caller gone;
- optional-tracking — `fetchOptionalTracking` / `legOptionalTracking`: as
  optional, and sets `JobAnnotationsDegraded` to suppress the Job → CronJob hop.

The router already fails a query closed when any backend fails it (routing D6);
the class decides only what the build does with that query error.
`internal/api/errors.go` writes the static message `upstream query failed` so no
upstream URL leaks into the body.

## Goals / Non-Goals

**Goals:**

- One switch that makes every `/v1/storage-graph` query error except `ALERTS`
  fail the build, covering first-wave, by-reference, QoS, volume-label and
  application-recovery legs.
- The 502 names the failed family.

**Non-Goals:**

- Any change to `/v1/graph`.
- A partial-success body, a response header, or a `degraded` field (rejected:
  the body shape is fixed, and a partial storage body is what this change
  exists to stop returning).
- Retrying failed queries.
- Treating an empty vector, an unserved zone or a restriction fallback as a
  failure.

## Decisions

### D1 — A plan-level `failClosed` flag for the shared legs; required-only for the storage-only waves

`topologyPlan` gains `failClosed bool`; `storagePlan` sets it, `fullPlan` does
not. `plan.failsClosed(q)` answers true for every query but `promql.QAlerts`
when the flag is set. It is consulted where BOTH plans issue the leg:

- the first-wave launch switch in `readTopology` (`fetch` vs `fetchOptional*`);
- the scoped QoS chunk loop (`readScopedQoS` / `instantQoSChunk`), which also
  runs under `fullPlan`.

Every wave that only a by-reference plan issues — pods, Kubernetes nodes,
controllers, the application recovery, and the rooted `volume_labels` phases —
runs exclusively on `/v1/storage-graph`, so it simply fails closed: the per-family
error class (`legMode`, `scopedFamily.mode` / `.degraded`) is removed from the
scoped-read engine, `issueUnrestricted` and `issueVolumeLabelsQueries` return the
query error, and the `callerCtx` those paths only needed to tell caller
cancellation from a sibling failure is dropped. A flag there would be dead code:
no production build reaches those waves with it off.

*Alternative rejected:* flipping the `optional` bits in `topologyLegs`. That
table is shared by `/v1/graph` and would change its classes too.

`JobAnnotationsDegraded` becomes unreachable on the storage path (a
`kube_job_annotations` error now fails the build); it stays for `/v1/graph`'s
first-wave degrade.

### D2 — The failed family travels on the error

A new `promql`-independent wrapper in `pkg/build`:

```go
type QueryError struct{ Query string; Err error } // Unwrap() → Err
```

Every required-path return site wraps the query error with its bare family name
before returning it to the errgroup. `classifyReadError` keeps its Reason logic
(it unwraps through `QueryError`) and copies the name onto a new
`build.Error.Query` field. `mapBuildError` writes
`upstream query failed: <Query>` when the field is set and the existing static
message otherwise. The family name is a compile-time constant from
`promql.Query`, never text from the upstream error, so the redaction guarantee
is unchanged.

With several concurrent failures the errgroup returns the first; the named
family can therefore vary between identical failing requests. Accepted: the
status and reason are stable, and every failure is logged and counted.

`/v1/graph` gains the family name in its `upstream` message too, since both
endpoints share `mapBuildError`. The `reason` and status are unchanged; the
message text is not a contract.

### D3 — `ALERTS` stays optional

It only feeds `data.alerts` and folds into `data.status`; losing it never
removes a node, an edge or a measurement. Failing the whole storage view because
the alert store is down would couple storage visibility to the alerting stack.

### D4 — Non-failures stay non-failures

An empty vector (family not exported, annotation not allowlisted), the router's
"requested zone no backend declares" empty-plus-Warn, and the volume-label
bounded / unrenderable fallback involve no query error and are unchanged.

## Risks / Trade-offs

- [A flaky optional store now takes the storage view down] → Intended; the 502
  names the family so the operator sees which store. `ALERTS` is excepted.
- [Series-limit rejections on `kube_replicaset_annotations` /
  `kube_job_annotations` (the reason they degrade on `/v1/graph`)] → On the
  storage path both are read by reference, scoped to loaded owners, so their
  cardinality is bounded by the drawn workload.
- [Spec churn from three renamed requirements] → Mechanical; every citing
  requirement is updated in this change, and the hub change rebases once after
  this archive.

## Migration Plan

Deploy; storage requests that returned a degraded 200 now return 502
`upstream`. Frontends should surface the message. Rollback is a revert.
