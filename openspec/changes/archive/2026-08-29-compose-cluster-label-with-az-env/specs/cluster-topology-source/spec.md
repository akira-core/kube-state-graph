## ADDED Requirements

### Requirement: Cluster identity composed from zone and environment labels

The topology reader SHALL identify a Kubernetes cluster by the composite `<az>-<env>-<cluster>`, composed at the moment a series' `cluster` label is read from that series' own zone and environment labels under the operator-configured label keys ("Configurable `az` / `env` label keys"). The identity is the value every cluster-scoped structure is keyed on — entity ids, entity `cluster` labels, every join key and index, the cluster-family key, the observed-clusters set, and the `cluster` label of the self-metric gauges — so two clusters sharing a raw name under different zones or environments SHALL be distinct in every one of them.

Every cluster name the reader encounters — on a topology or kubelet series, on a service-graph series' `cluster` label, or arriving from the route store — SHALL be resolved through one ladder, in order:

1. **Compose**: the series carries non-empty values for both configured keys → `<az>-<env>-<raw>`.
2. **Adopt**: otherwise, when the raw name maps to exactly one identity in the build's identity table, that identity.
3. **Verbatim**: otherwise the raw name itself (the `unknown` bucket when the `cluster` label is absent). The reader SHALL count such series per metric and log one aggregated `cluster_identity_unresolved` warning per affected metric per build.

The identity table SHALL be built once per build from the four families that mint cluster-labelled entities — `kube_pod_info`, `kube_node_info`, `kube_service_info`, `kube_pod_spec_volumes_persistentvolumeclaims_info` — as the set of step-1 compositions keyed by raw name. Every other family resolves through the table and SHALL NOT add to it. The outcome SHALL NOT depend on series order.

The built graph SHALL expose the identity table (identity → `{az, env, name}`), and the raw-name component SHALL be recoverable from any identity in it. The cluster-family key of the `pod-service-graph` capability SHALL be computed over the identity string with its existing rule, so a family is scoped to one zone and one environment.

A build whose series carry no zone/environment pair SHALL produce every identity, id, label and index exactly as it does today.

#### Scenario: Same raw name in two zones yields two clusters

- **WHEN** `kube_node_info{cluster="c1",node="worker-0",az="us",env="dev"}` and `kube_node_info{cluster="c1",node="worker-0",az="eu",env="prod"}` both exist in the window
- **THEN** the reader emits two distinct K8s node entities with ids `us-dev-c1/worker-0` and `eu-prod-c1/worker-0`, each with `labels.cluster` equal to its own identity, and the observed-clusters set holds both `us-dev-c1` and `eu-prod-c1`

#### Scenario: Every cluster-scoped structure carries the identity

- **WHEN** `cluster-alpha`'s pod, node, service and claim series all carry `az="zone-a"` and `env="prod"`
- **THEN** pod ids are `zone-a-prod-cluster-alpha/<uid>`, PVC ids `zone-a-prod-cluster-alpha/<ns>/<claim>`, the service index is keyed `(zone-a-prod-cluster-alpha, <ns>, <svc>)`, a pod's `labels.node` is `zone-a-prod-cluster-alpha/<node>`, and the observed-clusters set holds `zone-a-prod-cluster-alpha` and not `cluster-alpha`

#### Scenario: Unambiguous raw name adopts the identity

- **WHEN** the only cluster identity composed from a raw `cluster="c1"` in the build is `us-dev-c1`, and a `kubelet_volume_stats_used_bytes{cluster="c1",...}` series carries neither zone nor environment label
- **THEN** the kubelet series resolves to `us-dev-c1`, its usage joins the `us-dev-c1/<ns>/<claim>` PVC, and no `cluster_identity_unresolved` warning is logged

#### Scenario: Ambiguous raw name stays verbatim and is warned

- **WHEN** the build composed both `us-dev-c1` and `eu-prod-c1` from raw `c1`, and a `kube_pod_owner{cluster="c1",...}` series carries neither zone nor environment label
- **THEN** that series resolves to the verbatim `c1`, joins neither identity (the affected pods keep no `owner` from it), and the build logs one `cluster_identity_unresolved` warning naming `kube_pod_owner` with the count of such series

#### Scenario: Partially-stamped series never composes

- **WHEN** a `kube_pod_info{cluster="c1",az="us"}` series carries a zone but no environment label and no identity for raw `c1` exists in the build
- **THEN** the pod's cluster is the verbatim `c1` (never `us--c1` or `us-c1`), and the series is counted as unresolved

#### Scenario: Identity table is built only from entity families

- **WHEN** every `kube_pod_info` series of `c1` carries `az="us",env="dev"` while a `kube_pod_owner` series of `c1` carries `az="eu",env="prod"`
- **THEN** the identity table maps `c1` to `us-dev-c1` only, the owner series composes to `eu-prod-c1` by step 1 and joins nothing, and no second `c1` cluster is created by it

#### Scenario: Cluster family is scoped to zone and environment

- **WHEN** the build holds identities `us-dev-c1`, `us-dev-c2`, `eu-prod-c1` and `eu-prod-c2`
- **THEN** `us-dev-c1` and `us-dev-c2` share a family key (`us-dev-c0`), `eu-prod-c1` and `eu-prod-c2` share another (`eu-prod-c0`), and no cross-family fan-out or ingress-cluster candidacy exists between the two families

#### Scenario: Foreign cluster names resolve through the same ladder

- **WHEN** a service-graph series carries `cluster="c1"` with no zone/environment labels and the build's only identity for `c1` is `us-dev-c1`, and a route-store destination names cluster `c1`
- **THEN** both resolve to `us-dev-c1`, the trace-cluster anchor and the route destination look up topology under `us-dev-c1`, and neither is counted as unresolved

#### Scenario: Unstamped estate is unchanged

- **WHEN** no series in the build carries a zone or environment label
- **THEN** every identity equals the raw `cluster` label, the identity table is empty, no warning is logged, and every id, label and index is byte-identical to the pre-change reader

## MODIFIED Requirements

### Requirement: Cluster-scoped IDs

The reader SHALL produce topology entities whose stable identifiers are cluster-scoped, where `<cluster>` is the cluster **identity** of "Cluster identity composed from zone and environment labels" — `<az>-<env>-<cluster>` when the series composed one, the raw `cluster` label otherwise:

- Pod ID = `<cluster>/<pod-uid>` (composite of the cluster identity and the `uid` label).
- K8s node ID = `<cluster>/<node>` (composite of the cluster identity and the `node` label).
- PVC ID = `<cluster>/<namespace>/<claim_name>`.
- StorageClass ID = `<cluster>/storageclass/<storageclass>` (composite of `cluster` and the `storageclass` name label; StorageClass is a cluster-scoped Kubernetes object whose name is not globally unique across clusters).

#### Scenario: Two clusters with same node name

- **WHEN** `kube_node_info{cluster="cluster-alpha", node="worker-0"}` and `kube_node_info{cluster="cluster-beta", node="worker-0"}` both exist in the window
- **THEN** the reader emits two distinct K8s node entities with IDs `cluster-alpha/worker-0` and `cluster-beta/worker-0`

#### Scenario: Two clusters with the same raw name in different zones

- **WHEN** `kube_node_info{cluster="c1", node="worker-0", az="us", env="dev"}` and `kube_node_info{cluster="c1", node="worker-0", az="eu", env="prod"}` both exist in the window
- **THEN** the reader emits two distinct K8s node entities with IDs `us-dev-c1/worker-0` and `eu-prod-c1/worker-0`

#### Scenario: Pod ID derives from uid label

- **WHEN** `kube_pod_info{cluster="cluster-alpha", uid="abc-123", ...}` is present
- **THEN** the reader emits a pod entity with ID `cluster-alpha/abc-123`

#### Scenario: Pod ID carries the composed identity

- **WHEN** `kube_pod_info{cluster="c1", uid="abc-123", az="us", env="dev", ...}` is present
- **THEN** the reader emits a pod entity with ID `us-dev-c1/abc-123` and `labels.cluster: "us-dev-c1"`

#### Scenario: Two clusters with same StorageClass name

- **WHEN** `kube_storageclass_info{cluster="cluster-alpha", storageclass="gp3"}` and `kube_storageclass_info{cluster="cluster-beta", storageclass="gp3"}` both exist in the window
- **THEN** the reader emits two distinct StorageClass entities with IDs `cluster-alpha/storageclass/gp3` and `cluster-beta/storageclass/gp3`

### Requirement: Series missing the cluster label

A topology series that is missing the `cluster` label SHALL be bucketed under the raw name `unknown`, which then resolves through the identity ladder like any other raw name: a series carrying both zone and environment labels composes to `<az>-<env>-unknown`, one carrying neither resolves to the verbatim `unknown`. The reader SHALL surface the count of such series via the `kube_state_graph_clusters_observed` gauge (an identity with the raw component `unknown` will appear in the gauge's label set when present). A request-scoped `cluster` filter whose value is `unknown` SHALL be rendered as an anchored alternation carrying BOTH spellings of the bucket — the literal `unknown` and the empty string, i.e. `cluster=~"unknown|"` — because a series whose `cluster` label is literally `unknown` and one carrying no `cluster` label are indistinguishable after bucketing and both belong to the bucket. (PromQL's empty alternative matches a series that carries no such label.) The alternation form SHALL be used even when `unknown` is the only requested value. Any other value renders as the literal value. At projection the same filter matches every identity whose raw component is `unknown`.

#### Scenario: Legacy series without cluster label

- **WHEN** a `kube_pod_info` series has no `cluster` label
- **THEN** the resulting pod entity has `cluster: "unknown"` and contributes to the `unknown` value in the observed-clusters set

#### Scenario: Cluster-less series with zone and environment composes

- **WHEN** a `kube_pod_info` series has no `cluster` label but carries `az="us"` and `env="dev"`
- **THEN** the resulting pod entity has id `us-dev-unknown/<uid>` and `cluster: "us-dev-unknown"`, and a request with `?cluster=unknown` still loads it upstream and admits it at projection

#### Scenario: Filtering on the unknown bucket

- **WHEN** a build runs with the cluster filter `unknown` against an upstream where some `kube_pod_info` series carry no `cluster` label, one carries `cluster="unknown"`, and others carry `cluster="cluster-alpha"`
- **THEN** the query is issued with `cluster=~"unknown|"`, the unlabelled series AND the literally-`unknown` series are loaded, `cluster-alpha` is not, and the resulting pod entities all have `cluster: "unknown"`

### Requirement: Configurable `az` / `env` label keys

The upstream label names that the `az` and `env` dimensions bind to SHALL default to `az` and `env` and SHALL be overridable per deployment via the environment variables `KSG_AZ_LABEL` / `KSG_ENV_LABEL` and the flags `--az-label` / `--env-label` (flag overrides environment, following the existing precedence). Each configured key SHALL be validated at startup as a PromQL label name (`[a-zA-Z_][a-zA-Z0-9_]*`); an invalid key SHALL fail startup with an error naming the setting. The two keys SHALL be distinct. The request parameter names (`az`, `env`) SHALL NOT change with the configured keys. The engine's embeddable options SHALL expose the same two keys so an in-process consumer configures them identically.

The same two keys SHALL name the labels the reader composes a cluster's identity from ("Cluster identity composed from zone and environment labels"): the matcher a request renders and the identity a series composes are bound to one pair of labels, so rebinding a key moves both together and a deployment can never filter on one label while identifying by another.

#### Scenario: Defaults apply when unset

- **WHEN** the server starts with neither variable nor flag set and a request carries `az=zone-a`
- **THEN** the rendered matcher is `az="zone-a"`

#### Scenario: Environment variable rebinds the key

- **WHEN** the server starts with `KSG_ENV_LABEL=deployment_tier` and a request carries `env=prod`
- **THEN** the rendered matcher is `deployment_tier="prod"`

#### Scenario: Rebound key drives the identity read

- **WHEN** the server starts with `KSG_ENV_LABEL=deployment_tier` and `c1`'s series carry `az="us"` and `deployment_tier="dev"` but also an unrelated `env="ignored"` label
- **THEN** the composed identity is `us-dev-c1`

#### Scenario: Invalid key fails startup

- **WHEN** the server starts with `KSG_AZ_LABEL=topology.kubernetes.io/zone`
- **THEN** startup fails with an error naming `KSG_AZ_LABEL` and stating that the value is not a valid PromQL label name

#### Scenario: Identical keys are rejected

- **WHEN** the server starts with `--az-label=scope --env-label=scope`
- **THEN** startup fails with an error stating that the two keys must differ
