# pod-application-lookup Specification

## Purpose

Resolves the ArgoCD Application of one named pod on demand through the routed
label query, applying the same controller-annotation rules the graph build
applies to every pod, so a consumer that already knows the pod does not have to
run a whole graph build or re-implement those rules.

## Requirements

### Requirement: On-demand pod Application resolution

The engine SHALL expose a Go function that resolves the ArgoCD Application of exactly one pod through the routed label query, without running a graph build. Its request SHALL carry:

- a required **cluster**, the raw `cluster` label value;
- a required **namespace** and **pod name**;
- an optional **availability zone**;
- an optional **environment**;
- a required **evaluation instant** and a required positive **lookback window**;
- optional **label keys** naming the zone and environment labels.

Every upstream query SHALL declare the `ksm` family and carry the request's cluster and namespace. A request missing a required field SHALL fail before any upstream query is issued.

#### Scenario: Missing cluster is rejected

- **WHEN** a caller resolves a pod with an empty cluster
- **THEN** the call fails with a request error and no upstream query is issued

#### Scenario: Every query is a routed ksm query

- **WHEN** a caller resolves pod `web-abc` in zone `zone-a`, cluster `c1`, namespace `shop`
- **THEN** every label query issued declares family `ksm`, zone `zone-a`, and the equalities `cluster="c1"` and `namespace="shop"`

#### Scenario: Zone is optional

- **WHEN** a caller resolves a pod with an empty zone
- **THEN** the request is accepted and the pod-owner query is issued to every backend serving `ksm`

### Requirement: Resolution matches the graph build

The lookup SHALL return the same Application the graph build attaches to the pod node for the same upstream data. It SHALL:

- read the pod's controller owner from `kube_pod_owner` rows with `owner_is_controller="true"`, keeping the lexically-smallest `(owner_kind, owner_name)`;
- replace a `ReplicaSet` owner with the Deployment named by `kube_replicaset_owner` when one exists;
- read the controller's `annotation_argocd_argoproj_io_tracking_id` from the annotation family for its kind, skipping values whose Application would be empty and keeping the lexically-smallest remaining raw value;
- for a `Job` owner with no usable tracking-id, read the owning CronJob from `kube_job_owner` rows with `owner_kind="CronJob"` and `owner_is_controller="true"` and read the CronJob's tracking-id;
- return the segment of the tracking-id before the first `:`.

A pod with no controller owner, an owner kind with no annotation family, or a controller with no usable tracking-id SHALL return an empty Application and no error.

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

When the request carries a zone, every query SHALL be routed to that zone's `ksm` backends and carry the zone matcher. When the request carries an environment, every query SHALL carry it as an equality on the configured environment label.

For each of the two the request omits, the pod-owner query SHALL carry no matcher for it, and the lookup SHALL read that label off the pod-owner rows, an absent label reading as empty. When the rows carry a single combination, every later query SHALL carry it: a non-empty zone routed and matched as above, an empty zone as an equality with the empty value and no routing, an environment as an equality. When the rows carry more than one combination, the call SHALL fail with an ambiguity error and issue no further query.

#### Scenario: Cluster alone resolves

- **WHEN** a caller resolves pod `api-1` in cluster `c1` with no zone, and the pod-owner rows all carry `az="zone-a"`
- **THEN** the pod-owner query is issued with no zone, every later query is routed to `zone-a`, and a same-named Deployment in `zone-b` is never read

#### Scenario: Pod present in two zones

- **WHEN** cluster `c1` exists in both `zone-a` and `zone-b`, each holding pod `api-1`, and the request carries no zone
- **THEN** the call fails with the ambiguity error after the pod-owner query

#### Scenario: Pod present in two environments

- **WHEN** pod `api-1` exists in cluster `c1` under both `env="prod"` and `env="staging"` and the request carries no environment
- **THEN** the call fails with the ambiguity error after the pod-owner query and issues no further query

#### Scenario: Environment pins the controller read

- **WHEN** the request carries environment `staging`
- **THEN** every issued query carries `env="staging"` and the staging Deployment's Application is returned

### Requirement: Upstream errors fail the lookup

Any label-query error SHALL fail the call with an error naming the query's metric. The lookup SHALL NOT degrade a failed leg to an absent value.

#### Scenario: Job annotation read fails

- **WHEN** the `kube_job_annotations` query errors while resolving a Job-owned pod
- **THEN** the call returns an error naming `kube_job_annotations` and no Application
