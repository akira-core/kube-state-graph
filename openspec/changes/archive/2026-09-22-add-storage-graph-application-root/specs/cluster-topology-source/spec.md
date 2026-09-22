## ADDED Requirements

### Requirement: Application-rooted recovery reads of the owner and annotation families

Under a `/v1/storage-graph` request carrying an `application=` root, the topology reader SHALL issue — in addition to the by-reference reads of "Topology series consumed" — a recovery of the Application's pods over the SAME families, restricted by a different key at each stage (the `storage-graph-api` capability's "Application roots recover their pods upstream" defines the stages, their gating, error classes and bound):

- the six controller-annotation families restricted on `annotation_argocd_argoproj_io_tracking_id` to the values whose segment before the first `:` is a root Application — beside their fixed `annotation_argocd_argoproj_io_tracking_id!=""` matcher;
- `kube_replicaset_owner` restricted to `owner_kind="Deployment"` and `owner_name` in the recovered Deployment names;
- `kube_job_owner` restricted on `owner_name` to the recovered CronJob names — beside its fixed `owner_kind="CronJob",owner_is_controller="true"` matcher;
- `kube_pod_owner` restricted to `owner_is_controller="true"`, one query per `owner_kind`, with `owner_name` in that kind's recovered names.

Every recovery query SHALL carry the family's request-scoped matchers (**[AECN]**) exactly as its by-reference read does, SHALL be issued at the family's bare name, and its returned series SHALL be counted under the family's name in the build's per-family tally, added to what the by-reference read of the same family contributes. The recovery SHALL NOT write into the vectors the by-reference reads parse: it yields pod names only, and every attribute of a recovered pod — `owner`, `application`, `status` — is resolved from the by-reference reads exactly as for a claim-binding pod. `/v1/graph`, and a `/v1/storage-graph` request without an `application=` root, SHALL issue none of these reads.

#### Scenario: Recovery queries compose the fixed selector, the request matchers and the recovery key

- **WHEN** a `/v1/storage-graph` build for `?az=zone-a&env=prod&cluster=c1&application=checkout` runs
- **THEN** every recovery query on `kube_deployment_annotations` carries `annotation_argocd_argoproj_io_tracking_id!=""`, `<az-key>="zone-a",<env-key>="prod",cluster="c1"` and the tracking-id restriction; the `kube_job_owner` recovery query carries `owner_kind="CronJob",owner_is_controller="true"`, the same request matchers and an `owner_name` restriction; and the `kube_pod_owner` recovery queries carry `owner_is_controller="true"`, one `owner_kind` equality each, the request matchers and an `owner_name` restriction

#### Scenario: A recovered pod's attributes come from the by-reference reads

- **WHEN** the recovery returns pod `web-7d9f-abc` and the by-reference `kube_pod_owner` read for that name returns its ReplicaSet owner
- **THEN** the pod's `data.owner` and `data.application` are resolved from the by-reference `kube_pod_owner`, `kube_replicaset_owner` and `kube_deployment_annotations` reads, byte-identical to the attributes the same pod carries when loaded through a claim binding

#### Scenario: Recovery series are tallied under the family name

- **WHEN** a `/v1/storage-graph` build's recovery returns two `kube_deployment_annotations` series and its by-reference controller read returns three
- **THEN** the per-family tally carries `kube_deployment_annotations` with `5`
