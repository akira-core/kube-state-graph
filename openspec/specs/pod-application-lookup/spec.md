# pod-application-lookup Specification

## Purpose

Resolves the ArgoCD Application of one named pod on demand through the routed
label query, applying the same controller-annotation rules the graph build
applies to every pod, so a consumer that already knows the pod does not have to
run a whole graph build or re-implement those rules.

## Requirements

### Requirement: On-demand pod Application resolution

The engine SHALL expose a Go function that resolves the ArgoCD Application of exactly one pod through the routed label query, without running a graph build. Every upstream query SHALL declare the `ksm` family and carry the request's cluster and namespace, except the application-metadata query, which SHALL declare the `appmeta` family and carry the request's cluster and namespace on `cluster` and the source's configured namespace label.

#### Scenario: Every query is a routed ksm query

- **WHEN** a caller resolves pod `web-abc` in zone `zone-a`, cluster `c1`, namespace `shop`
- **THEN** every label query issued declares family `ksm`, zone `zone-a`, and the equalities `cluster="c1"` and `namespace="shop"`

#### Scenario: Zone is optional

- **WHEN** a caller resolves a pod with an empty zone
- **THEN** the request is accepted and the pod-owner query is issued to every backend serving `ksm`

#### Scenario: The application-metadata query is a routed appmeta query

- **WHEN** a caller resolves pod `web-abc` in zone `zone-a`, cluster `c1`, namespace `shop`, with a source naming metric `argocd_app_pod_info` whose namespace label is `k8s_namespace`
- **THEN** the application-metadata label query declares family `appmeta`, zone `zone-a`, and the equalities `cluster="c1"`, `k8s_namespace="shop"` and the source's pod label equal to `web-abc`

### Requirement: Pod lookup request fields

A lookup request SHALL carry a cluster, a namespace, a pod name, an evaluation instant and a positive lookback window, and MAY carry an availability zone, an environment, label keys and an application-metadata source. A request missing a required field SHALL fail before any upstream query is issued.

#### Scenario: Request shape

- **WHEN** a caller builds a lookup request
- **THEN** it carries a required **cluster** (the raw `cluster` label value), a required **namespace** and **pod name**, and a required **evaluation instant** and positive **lookback window**
- **AND** it may carry an optional **availability zone**, an optional **environment**, optional **label keys** naming the zone and environment labels, and an optional **application-metadata source** — the metric name and its five label names (the `app-metadata-source` capability)

#### Scenario: Missing cluster is rejected

- **WHEN** a caller resolves a pod with an empty cluster
- **THEN** the call fails with a request error and no upstream query is issued

### Requirement: Resolution matches the graph build

The lookup SHALL return the same Application the graph build attaches to the pod node for the same upstream data. When the request carries an application-metadata source and the routing table serves `appmeta`, the lookup SHALL first return the lexically-smallest non-empty `app` among the pod's application-metadata rows, if any, and otherwise SHALL resolve through the controller-annotation chain.

#### Scenario: Metric Application wins

- **WHEN** the source is set, `appmeta` is served, pod `web-abc` has application-metadata rows with `app` values `shop-b` and `shop-a`, and its Deployment carries tracking-id `web-app:apps/Deployment:shop/web`
- **THEN** the lookup returns `shop-a` and issues no controller-annotation query

#### Scenario: No metric row falls back to the controller chain

- **WHEN** the source is set, `appmeta` is served, pod `web-abc` has no application-metadata row, and its Deployment carries tracking-id `shop-app:apps/Deployment:shop/web`
- **THEN** the lookup returns `shop-app`

#### Scenario: Unserved family skips the metric step

- **WHEN** the source is set but the routing table serves `appmeta` on no backend
- **THEN** no `appmeta` query is issued, the call does not fail, and the controller chain decides

#### Scenario: Ownerless pod resolves from the metric

- **WHEN** the source is set, `appmeta` is served, pod `static-1` has no controller owner, and its application-metadata row carries `app="infra"`
- **THEN** the lookup returns `infra`

#### Scenario: Nothing resolves

- **WHEN** a pod has no application-metadata Application and either no controller owner, an owner kind with no annotation family, or a controller with no usable tracking-id
- **THEN** the lookup returns an empty Application and no error

### Requirement: Controller-annotation chain resolution

Without an application-metadata Application, the lookup SHALL read the pod's controller owner, follow it to the controller whose ArgoCD tracking-id it reads, and return the segment of the tracking-id before the first `:`.

#### Scenario: Controller owner pick

- **WHEN** the lookup reads the pod's controller owner
- **THEN** it reads `kube_pod_owner` rows with `owner_is_controller="true"`, keeping the lexically-smallest `(owner_kind, owner_name)`

#### Scenario: ReplicaSet owner replaced by its Deployment

- **WHEN** the controller owner is a `ReplicaSet` and `kube_replicaset_owner` names a Deployment for it
- **THEN** the Deployment replaces the `ReplicaSet` as the controller

#### Scenario: Tracking-id read

- **WHEN** the lookup reads the controller's tracking-id
- **THEN** it reads `annotation_argocd_argoproj_io_tracking_id` from the annotation family for the controller's kind, skipping values whose Application would be empty and keeping the lexically-smallest remaining raw value

#### Scenario: Job falls back to its CronJob

- **WHEN** the controller owner is a `Job` with no usable tracking-id
- **THEN** the lookup reads the owning CronJob from `kube_job_owner` rows with `owner_kind="CronJob"` and `owner_is_controller="true"` and reads the CronJob's tracking-id

#### Scenario: Deployment-managed pod

- **WHEN** pod `web-abc` is owned by ReplicaSet `web-7d9`, which is owned by Deployment `web` carrying tracking-id `shop-app:apps/Deployment:shop/web`
- **THEN** the lookup returns `shop-app`

#### Scenario: CronJob-managed pod

- **WHEN** a pod is owned by Job `cron-job`, which carries no tracking-id and is controlled by CronJob `nightly` carrying tracking-id `batch-app:batch/CronJob:shop/nightly`
- **THEN** the lookup returns `batch-app`

#### Scenario: Job annotation wins over its CronJob

- **WHEN** a pod's Job carries its own tracking-id `job-app` and its CronJob carries a different one
- **THEN** the lookup returns `job-app` and issues no `kube_job_owner` query

#### Scenario: Malformed tracking-id does not win

- **WHEN** a StatefulSet carries tracking-ids `:apps/StatefulSet:shop/db` and `db-app:apps/StatefulSet:shop/db`
- **THEN** the lookup returns `db-app`

#### Scenario: Owner kind without an annotation family

- **WHEN** a pod's controller owner is a `ReplicationController`
- **THEN** the lookup returns an empty Application, no error, and issues no annotation query

### Requirement: Zone and environment are pinned, never guessed

When the request carries a zone, every query SHALL be routed to that zone's `ksm` backends and carry the zone matcher. When the request carries an environment, every query SHALL carry it as an equality on the configured environment label. A zone or environment the request omits SHALL be pinned from the rows the lookup reads, never guessed.

#### Scenario: Environment pins the controller read

- **WHEN** the request carries environment `staging`
- **THEN** every issued query carries `env="staging"` and the staging Deployment's Application is returned

### Requirement: Omitted zone and environment are pinned from the rows

For each of zone and environment the request omits, the pod-owner query SHALL carry no matcher for it and the lookup SHALL read that label off the pod-owner rows, an absent label reading as empty. A single combination in the rows SHALL be carried by every later query; more than one SHALL fail the call with an ambiguity error and issue no further query.

#### Scenario: Pinning from the application-metadata rows

- **WHEN** the pod-owner query returns no rows and the request carries an application-metadata source
- **THEN** the application-metadata query carries no matcher for the omitted dimensions and the same pinning rule applies to its rows instead

#### Scenario: How a pinned combination is carried

- **WHEN** the rows carry a single combination
- **THEN** every later query carries a non-empty zone routed and matched as for a requested zone, an empty zone as an equality with the empty value and no routing, and an environment as an equality

#### Scenario: Cluster alone resolves

- **WHEN** a caller resolves pod `api-1` in cluster `c1` with no zone, and the pod-owner rows all carry `az="zone-a"`
- **THEN** the pod-owner query is issued with no zone, every later query is routed to `zone-a`, and a same-named Deployment in `zone-b` is never read

#### Scenario: Pod present in two zones

- **WHEN** cluster `c1` exists in both `zone-a` and `zone-b`, each holding pod `api-1`, and the request carries no zone
- **THEN** the call fails with the ambiguity error after the pod-owner query

#### Scenario: Pod present in two environments

- **WHEN** pod `api-1` exists in cluster `c1` under both `env="prod"` and `env="staging"` and the request carries no environment
- **THEN** the call fails with the ambiguity error after the pod-owner query and issues no further query

#### Scenario: Pinned zone carries into the application-metadata query

- **WHEN** a caller resolves pod `api-1` in cluster `c1` with no zone and a source set, and the pod-owner rows all carry `az="zone-a"`
- **THEN** the application-metadata query is routed to `zone-a`'s `appmeta` backends and carries the zone matcher

#### Scenario: Ownerless pod pins from the application-metadata rows

- **WHEN** a caller resolves an ownerless pod with no zone and a source set, so the pod-owner query returns no rows, and the application-metadata rows carry both `az="zone-a"` and `az="zone-b"`
- **THEN** the call fails with the ambiguity error

### Requirement: Upstream errors fail the lookup

Any label-query error SHALL fail the call with an error naming the query's metric. The lookup SHALL NOT degrade a failed leg to an absent value.

#### Scenario: Job annotation read fails

- **WHEN** the `kube_job_annotations` query errors while resolving a Job-owned pod
- **THEN** the call returns an error naming `kube_job_annotations` and no Application
