## MODIFIED Requirements

### Requirement: Every root kind is tracked from its own tier to its claims

The storage build SHALL reach the claims a request can retain by walking from its root, never by reading a family across the requested zone and discarding what the root does not reach. For each root kind it SHALL issue the reads defined below, each restricted as "Storage build reads every family by reference" defines; the claims those reads name form the build's **tracked claim set**.

#### Scenario: Rooted volumes name their claims

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00`, the rooted read returns FlexVol `trident_pvc_ab12_cd34`, and claim `shop/orders-data` is bound to PV `pvc-ab12-cd34` and mounted by `orders-0` and `orders-1`
- **THEN** `kube_persistentvolumeclaim_info` is issued restricted to `volumename=~"pvc-ab12-cd34"`, the claim-side families are restricted to `namespace="shop",persistentvolumeclaim="orders-data"`, the pod read is restricted to `{orders-0, orders-1}`, and the body carries the claim's complete path

#### Scenario: A FlexVol with no storage prefix yields a candidate

- **WHEN** the rooted read returns FlexVol `pvc_ab12_cd34`
- **THEN** the candidate set contains `pvc-ab12-cd34`

#### Scenario: An over-generated candidate loads nothing

- **WHEN** the rooted read returns FlexVol `trident_pvc_ab12_clone`, a clone no PersistentVolume is named after
- **THEN** the candidate `pvc-ab12-clone` is issued, the claim-info read returns no row for it, and the body is unchanged by its presence

#### Scenario: Root volumes produce no candidate

- **WHEN** the rooted read returns `vol0` and `svm_shop_root` besides claim volumes
- **THEN** neither contributes a candidate, and no coverage warning fires on their account

#### Scenario: A touched aggregate another controller owns contributes no claim

- **WHEN** a client sends `?az=zone-a&env=prod&ontap_node=ontap-b`, the rows naming `ontap-b` touch `aggr1` and `aggr9`, every `aggr1` row names `ontap-b`, and `aggr9`'s rows name both `ontap-a` and `ontap-b` because a takeover happened inside the window
- **THEN** both aggregates are read whole, the vote gives `aggr1` to `ontap-b` and `aggr9` to `ontap-a`, only `aggr1`'s volumes contribute candidates, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: A Kubernetes node root reads pods by node

- **WHEN** a client sends `?az=zone-a&env=prod&node=worker-1` against an estate of 40000 pods, of which `orders-0` and `web-0` run on `worker-1`
- **THEN** the first `kube_pod_info` query is restricted to `node="worker-1"`, the incarnation-completion query to `namespace="shop",pod=~"orders-0|web-0"` (both pods run in `shop`), the claim-binding query to the same pairs, and no other pod's binding is fetched

#### Scenario: A Kubernetes node root never reads a same-named pod elsewhere

- **WHEN** a client sends `?az=zone-a&env=prod&node=worker-1`, `shop/web-0` runs on `worker-1`, and `platform/web-0` runs on `worker-2`
- **THEN** incarnation completion and the claim-binding read carry `namespace="shop",pod="web-0"`, `platform/web-0` and its claims are never fetched, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: A pod rescheduled inside the window follows its newest incarnation

- **WHEN** a client sends `?az=zone-a&env=prod&node=worker-1` and `shop/db-0` ran on `worker-1` early in the window and was recreated on `worker-2` before its end
- **THEN** incarnation completion loads both incarnations, `shop/db-0` is placed on `worker-2`, its claims are not tracked from `worker-1`, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: A pod root reads its bindings by reference

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0&pod=platform/redis-0`
- **THEN** the claim-binding query is restricted to `namespace=~"platform|shop"` and `pod=~"orders-0|redis-0"`, a binding of `shop/redis-0` or `platform/orders-0` is discarded before any claim is tracked, and no other binding is fetched

#### Scenario: A same-named claim in another namespace is filtered out

- **WHEN** the build tracks claim `shop/data` of cluster `c1` and claim `platform/data` exists in the same estate
- **THEN** the claim-binding read is issued as `{namespace="shop",persistentvolumeclaim="data"}` beside the request matchers, so no `platform/data` binding is read, its pod is not read, and no `platform/data` PVC node is built

#### Scenario: A same-named claim in another cluster is filtered out

- **WHEN** the build tracks claim `shop/data` of cluster `c1` and the claim-binding read `{namespace="shop",persistentvolumeclaim="data"}` also returns a binding of `shop/data` in cluster `c2`, which no root or seed row names
- **THEN** that binding is discarded before the pod scope is computed, its pod is not read, and no `c2` PVC node is built

#### Scenario: A claim root reads its claim by reference

- **WHEN** a client sends `?az=zone-a&env=prod&pvc=shop/orders-data&pvc=shop/cache&pvc=platform/queue`
- **THEN** `kube_persistentvolumeclaim_info` is issued as `{namespace="platform",persistentvolumeclaim="queue"}` and `{namespace="shop",persistentvolumeclaim=~"cache|orders-data"}` beside the request matchers, no `volume_labels` query precedes it, and a `platform/cache` row admitted by no query is never read

#### Scenario: A volume root reads its claim by volume name

- **WHEN** a client sends `?az=zone-a&env=prod&pv=pvc-ab12-cd34&pv=mongo-data-01`
- **THEN** `kube_persistentvolumeclaim_info` is issued restricted to `volumename=~"mongo-data-01|pvc-ab12-cd34"` beside the request matchers, no `volume_labels` query precedes it, and the claims it returns are expanded exactly as a storage-side root's claims are

#### Scenario: A statically provisioned volume is reachable from a volume root

- **WHEN** claim `db/mongo-data` is bound to PV `mongo-data-01`, the filer holds FlexVol `mongo_data_01` whose name the configured rewrite rules match to that PV, and a client sends `?az=zone-a&env=prod&pv=mongo-data-01`
- **THEN** the body contains the claim's complete path to its aggregate, SVM and controller, identical to the path `GET /v1/graph` draws for it

#### Scenario: A statically provisioned volume is not reached from a storage root

- **WHEN** claim `db/mongo-data` is bound to PV `mongo-data-01`, the filer holds FlexVol `mongo_data_01`, and a client roots at the aggregate holding it
- **THEN** no candidate names `mongo-data-01`, the body draws no path through that claim, and `GET /v1/graph` for the same zone, or a `pod=` root on a pod mounting it, still draws its storage chain

#### Scenario: No candidate is reported

- **WHEN** a client roots at `ontap_cluster=ontap-prod` and every FlexVol on it is named by a Trident `nameTemplate` that embeds no `pvc_`
- **THEN** no claim query is issued, the request returns 200 with the rooted storage entities and no path, and one `storage_root_claim_miss` warning with `reason="no_pv_candidate"` is logged

#### Scenario: Candidates naming no claim are reported

- **WHEN** candidates are produced and the claim-info read returns no row
- **THEN** no binding, pod, Kubernetes-node or controller query is issued, the request returns 200, and one `storage_root_claim_miss` warning with `reason="no_claim"` is logged

#### Scenario: An over-large root set is rejected

- **WHEN** a client sends thousands of `aggr=` values, enough that the first rooted read would take more queries than the maximum
- **THEN** the server returns 400 with `reason: "invalid_scope"`, no upstream query is issued, and no unrestricted read is performed in its place

#### Scenario: A qualified aggregate root reads one filer's aggregate

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=ontap-prod/aggr1&aggr=ontap-prod/aggr2&aggr=ontap-lab/aggr1&aggr=aggr9`
- **THEN** the phase-1 `volume_labels` reads are `{aggr="aggr9"}`, `{cluster="ontap-lab",aggr="aggr1"}` and `{cluster="ontap-prod",aggr=~"aggr1|aggr2"}` with no `<az-key>` or `<env-key>` matcher beside them, and no query reads `aggr1` of any other filer for the root

#### Scenario: A storage root stays in the request's zone

- **WHEN** two backends serve `ksm` with `zones: [zone-a]` and `zones: [zone-b]`, two backends serve `harvest` likewise, filer `ontap-prod` holds FlexVols whose claims live in a `zone-a` and a `zone-b` cluster, and a client sends `?az=zone-a&env=prod&ontap_cluster=ontap-prod`
- **THEN** no query is issued to either `zone-b` backend, every kube-state-metrics, kubelet and `ALERTS` query carries `<az-key>="zone-a",<env-key>="prod"`, the body contains the `zone-a` claim's complete path and not the `zone-b` claim, and `clusters` lists `zone-a` identities only

#### Scenario: Storage-side roots read volume labels on their own label

- **WHEN** the root kind is `ontap_cluster`, a bare `aggr` or a bare `svm`
- **THEN** the build reads `volume_labels` restricted on `cluster`, `aggr` or `svm` respectively to the root values

#### Scenario: Qualified storage roots are read one query per ONTAP cluster

- **WHEN** the root kind is `aggr` or `svm` with qualified values
- **THEN** the build reads `volume_labels` one query per ONTAP cluster, carrying that cluster as an equality and its names as the alternation on `aggr` (resp. `svm`), so an aggregate or SVM of that name on another filer is never read for the root
- **AND** an aggregate read this way is read whole, exactly as a bare `aggr` root's is

#### Scenario: An ONTAP controller root reads the aggregates its rows touch whole

- **WHEN** the root kind is `ontap_node`
- **THEN** the build reads `volume_labels` restricted on `node` to the root values, then reads WHOLE every `(ONTAP cluster, aggregate)` pair those rows name
- **AND** only the aggregates whose owning controller the vote resolves to a root value contribute claims; since the vote picks one of the aggregate's own `node` values, every aggregate a root controller owns has a row naming it and is found by the first read

#### Scenario: Candidate PersistentVolume names are derived from the volume label

- **WHEN** a contributing row of an `ontap_cluster`, `ontap_node`, `aggr` or `svm` root carries a `volume`
- **THEN** for every position at which the value continues with `pvc_` and which is the start of the value or follows a `_`, the remainder of the value from that position with every `_` replaced by `-` is one **candidate PersistentVolume name**, and candidates are sorted and de-duplicated
- **AND** the build reads `kube_persistentvolumeclaim_info` restricted on `volumename` to the candidates, and the claims that read returns are the tracked set

#### Scenario: Candidate extraction is a generator, never a judge

- **WHEN** candidates have been derived for a storage-side root
- **THEN** a candidate naming no PersistentVolume loads nothing, and whether a loaded claim lands on a FlexVol — and on which aggregate, SVM and controller — is decided solely by the forward derivation and join of the `netapp-storage-graph` capability, exactly as on `/v1/graph`
- **AND** the rewrite rules do not change the extraction, so a custom rule set can make a storage root find fewer claims, never a wrong one

#### Scenario: A Kubernetes node root completes pod incarnations

- **WHEN** the root kind is `node`
- **THEN** the build reads `kube_pod_info` restricted on `node` to the root values, then reads `kube_pod_info` again restricted to the `(namespace, pod)` pairs the first read returned, WITHOUT the node restriction (**incarnation completion**), so a pod's node is its newest incarnation's and a pod name recreated on another node inside the window is placed where an unrestricted read places it
- **AND** it reads the claim-binding family restricted to the `(namespace, pod)` pairs of the pods whose newest incarnation runs on a root node, keeping only rows whose `(cluster, namespace, pod)` names such a pod, and the claims those rows bind are the tracked set
- **AND** both reads are keyed by `(namespace, pod)`, as "Storage build reads every family by reference" requires of every read of known pods

#### Scenario: A pod root reads its own bindings

- **WHEN** the root kind is `pod`
- **THEN** the build reads the claim-binding family restricted on `namespace` and `pod` to the roots, keeping only rows whose `(namespace, pod)` is a root ref, and reads `kube_pod_info` restricted to the root names
- **AND** the claims those rows bind are the tracked set

#### Scenario: A claim root reads its claims one query per namespace

- **WHEN** the root kind is `pvc`
- **THEN** the build reads `kube_persistentvolumeclaim_info` restricted to the root `(namespace, claim)` pairs — one query per namespace, carrying that namespace as an equality and its claim names as the alternation on `persistentvolumeclaim` — keeping only rows whose `(namespace, claim)` is a root ref
- **AND** the claims those rows name, in every Kubernetes cluster of the selected estate, are the tracked set

#### Scenario: A volume root reads its claims by volume name verbatim

- **WHEN** the root kind is `pv`
- **THEN** the build reads `kube_persistentvolumeclaim_info` restricted on `volumename` to the root values, and the claims that read returns are the tracked set
- **AND** this is the storage-side kinds' claim read without the candidate derivation: no `volume_labels` read precedes it, and a root value is used verbatim, never rewritten

#### Scenario: A claim or volume root's seed read is the claim-side claim read

- **WHEN** the root kind is `pvc` or `pv`
- **THEN** the seed read IS the claim-side `kube_persistentvolumeclaim_info` read of "The tracked claims expand to both ends of the chain", which is therefore not issued again, while the expansion's other claim-side reads, mounter completion, candidate completion, owner completion and storage-side reads apply unchanged
- **AND** because candidate completion reads `volume_labels` by the derived token of every tracked claim, a claim root's aggregate, SVM and controller are found exactly as on `/v1/graph`

#### Scenario: An Application root tracks the claims its recovery names

- **WHEN** the root kind is `application`
- **THEN** the build runs the recovery of "Application roots are tracked through their controllers and claims" and tracks the claims that requirement names

#### Scenario: The tracked set is closed over what the projection can retain

- **WHEN** the projection decides whether a claim is retained
- **THEN** the tracked set already contains every claim it can retain: through a storage-side root only if the claim's picked aggregate, SVM or controller is a root, which requires at least one of its candidates on a rooted component, so the rooted rows name it
- **AND** through a `node` or `pod` root only if one of its mounting pods is or runs on a root, so that pod's bindings name it
- **AND** through a `pvc` or `pv` root only if it is itself a root claim, which the seed read names

#### Scenario: Roots with no claim are read for themselves

- **WHEN** a request carries a root, whether or not any claim reaches it
- **THEN** for the root alone the build also reads the families that materialise a root no claim reaches, as "Roots are always materialised when the upstream knows them" requires: the aggregate gauge families for `aggr` (by aggregate name for a bare value, by `(ONTAP cluster, aggregate)` pair for a qualified one) and for `ontap_cluster` (by ONTAP cluster); the controller families for `ontap_node` (by controller name) and for `ontap_cluster` (by ONTAP cluster); the four Kubernetes-node families for `node`; `kube_pod_info` for `pod`
- **AND** nothing further for `pvc` and `pv`: the seed read is the read that names a root claim

#### Scenario: Root sets are bounded before any query

- **WHEN** a root kind's first read would take more than a fixed maximum number of queries under the shared chunk bounds — the byte budget and the alternation value bound, so the cap is reached by many short values as well as by few long ones
- **THEN** the request is rejected 400 with `reason: "invalid_scope"` before any upstream query is issued — root parameters are repeatable and the parser bounds each value's length, never their count, so a root set is a scope a client can inflate
- **AND** the decision needs no upstream data, since the first read's shape is a pure function of the root values and the two chunk bounds
- **AND** the build never falls back to reading the family across the zone

#### Scenario: Claims without a candidate are reached only from other roots

- **WHEN** a claim is bound to a PersistentVolume whose name yields no candidate — a statically provisioned PV, or a provisioner configured with a non-`pvc` volume-name prefix
- **THEN** it is not found from a storage-side root, even when the forward join would match it
- **AND** `/v1/graph`, and a `node`, `pod`, `application`, `pvc` or `pv` root reaching the same claim, still join it — a `pvc` / `pv` root in particular is the way to root at a claim bound to a statically provisioned volume

#### Scenario: Coverage warnings carry their counts and never change the status

- **WHEN** a storage-side root's contributing rows carry a `volume` and none produced a candidate, or candidates were produced and the claim-info read returned no claim
- **THEN** the build logs one aggregated warning `storage_root_claim_miss`, with `reason="no_pv_candidate"` and the volume count in the first case and `reason="no_claim"` and the candidate count in the second
- **AND** neither changes the response status

#### Scenario: A filtered request logs a claim miss at Debug

- **WHEN** candidates were produced, the claim-info read returned no claim, and the request carries a `cluster` or `namespace` filter, which can legitimately exclude every candidate
- **THEN** the `no_claim` miss (`storage_root_claim_miss` with `reason="no_claim"`) is logged at Debug instead of as a warning

#### Scenario: Every tracking read stays in the request's zone

- **WHEN** the build issues any query
- **THEN** the query carries the request-scoped matchers its family accepts and is dispatched only to the backends the request's `az` values select
- **AND** a claim on a rooted filer that lives in a zone or environment the request did not select is therefore not loaded and is not drawn, while a claim of a selected zone joins the FlexVols of its own zone in preference to any other

### Requirement: Storage build reads every family by reference

The storage build SHALL NOT issue `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels` or `kube_service_annotations`. It SHALL read `ALERTS` under the request-scoped matchers alone, and every other family **by reference**: restricted to the object names the request's root or the reads before it actually carry.

#### Scenario: Skipped families never reach the upstream

- **WHEN** the upstream rejects every `kube_pod_container_info` query with a series-limit error and a client sends any valid `/v1/storage-graph` request
- **THEN** the storage build issues no `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels` or `kube_service_annotations` query, `kube_state_graph_upstream_query_failures_total` does not move, and the request returns 200

#### Scenario: Only ALERTS is read across the zone

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0`
- **THEN** every issued query except `ALERTS` carries a restriction on its family's identity label beyond the request-scoped matchers its family carries (`<az-key>="zone-a",<env-key>="prod"` on a kube-state-metrics or kubelet family, none on a Harvest family), and `ALERTS` carries the request-scoped matchers alone

#### Scenario: Harvest gauges are read for the reached components only

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0`, the pod's claim resolves to `aggr1` on `ontap-prod`, owned by `ontap-prod-01`, in SVM `svm_shop`, and the filer holds 200 aggregates
- **THEN** the aggregate gauge queries carry `cluster="ontap-prod"` and `aggr=~"aggr1"`, the controller queries carry `cluster="ontap-prod"` and `node=~"ontap-prod-01"`, the fixed-policy queries carry `cluster="ontap-prod"` and `svm=~"svm_shop"`, and no other aggregate's or controller's series is fetched

#### Scenario: A rooted aggregate with no claim is still drawn

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00`, no volume on `aggr00` matches any claim, and the aggregate and controller families name `aggr00` and its owner
- **THEN** the aggregate gauges are read for `aggr00` by name, the controller families for its owner, and the body contains `aggr00` with its health and usage attributes and its owning controller, and no path through them

#### Scenario: Cross-namespace name collision is harmless

- **WHEN** claim-binding series name `shop/web-0` and the estate also holds a claimless `platform/web-0`
- **THEN** the pod read carries `namespace="shop",pod="web-0"`, `platform/web-0` is never fetched, and the body is byte-identical to the body an unrestricted pod read would produce

#### Scenario: Pods are read one query per namespace

- **WHEN** the pods to read are `shop/orders-0`, `shop/web-0` and `platform/orders-0`
- **THEN** `kube_pod_info` and `kube_pod_owner` are each issued as `{namespace="platform",pod="orders-0"}` and `{namespace="shop",pod=~"orders-0|web-0"}` beside the request matchers, `platform/web-0` is never read, and the merged vector follows namespace order

#### Scenario: A same-named pod in another namespace never widens the node or controller read

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/postgres-0` and `platform/postgres-0`, owned by StatefulSet `pg-other`, runs on `worker-9`
- **THEN** no `kube_pod_info` or `kube_pod_owner` query admits `platform/postgres-0`, no Kubernetes-node query names `worker-9`, and no controller query names `pg-other`

#### Scenario: A pod chunk failure fails the build

- **WHEN** the pod restriction is split into three chunks and the second `kube_pod_info` chunk fails with an upstream error
- **THEN** the build returns an error and the request is mapped as an upstream failure, exactly as an unrestricted `kube_pod_info` failure is

#### Scenario: Controller read is restricted to the loaded pods' owners

- **WHEN** the loaded pods are owned by StatefulSet `orders` and by ReplicaSet `web-7d9f` (whose `kube_replicaset_owner` series names Deployment `web`), while the estate holds thousands of other controllers
- **THEN** `kube_statefulset_annotations` restricts `statefulset` to `{orders}`, `kube_replicaset_owner` and `kube_replicaset_annotations` restrict `replicaset` to `{web-7d9f}`, `kube_deployment_annotations` restricts `deployment` to `{web}` and is issued only after the ReplicaSet read returned, every one of those queries also carries its fixed selector and the request's matchers, no `kube_job_owner`, `kube_job_annotations`, `kube_daemonset_annotations` or `kube_cronjob_annotations` query is issued, and the body is byte-identical to the body an unrestricted read would produce

#### Scenario: Accumulated Job history does not reach the storage build

- **WHEN** the upstream's index for the day holds 300000 `kube_job_owner{owner_kind="CronJob",owner_is_controller="true"}` series, more than its series limit, and the loaded pods are owned by Jobs `backup-28901` and `backup-28902`
- **THEN** `kube_job_owner` is issued restricted to `job_name=~"backup-28901|backup-28902"` alongside its fixed selector, returns two series, and the request returns 200 with each pod's Application resolved through its CronJob exactly as `/v1/graph` resolves it

#### Scenario: Job-owned mounting pod resolves its CronJob Application through two stages

- **WHEN** a loaded pod is owned by Job `nightly-28901`, that Job carries no annotation of its own, `kube_job_owner` names CronJob `nightly` as its controller, and `kube_cronjob_annotations{cronjob="nightly"}` carries `reports:batch/CronJob:batch/nightly`
- **THEN** the first stage issues `kube_job_owner` and `kube_job_annotations` restricted to `{nightly-28901}`, the second stage issues `kube_cronjob_annotations` restricted to `{nightly}` only after the first returned, and the pod carries `data.application="reports"` with `data.owner={kind:"Job", name:"nightly-28901"}` unchanged

#### Scenario: A kind no loaded pod is owned by issues no query

- **WHEN** every loaded pod is owned by a StatefulSet
- **THEN** no `kube_replicaset_owner`, `kube_job_owner`, `kube_deployment_annotations`, `kube_daemonset_annotations`, `kube_replicaset_annotations`, `kube_job_annotations` or `kube_cronjob_annotations` query is issued and the per-family tally carries none of them

#### Scenario: A required controller chunk failure fails the build

- **WHEN** the ReplicaSet restriction is split into two chunks and the second `kube_replicaset_owner` chunk fails with an upstream error
- **THEN** the build returns an error and the request is mapped as an upstream failure, exactly as an unrestricted `kube_replicaset_owner` failure is

#### Scenario: Node read is restricted to the loaded pods' nodes and node roots

- **WHEN** a client sends `?az=zone-a&env=prod&node=n9`, the pods on `n9` mount a claim also mounted by a pod on `n1`, and the estate holds 5000 nodes
- **THEN** every issued `kube_node_info`, `kube_node_status_addresses`, `kube_node_labels` and `kube_node_status_condition` query restricts `node` to exactly `{n1, n9}` alongside its fixed selector and the request's matchers, no series for any other node is fetched, and the body is byte-identical to the body an unrestricted node read would produce

#### Scenario: Kubernetes node root with no mounting pod is still drawn

- **WHEN** a client sends `?az=zone-a&env=prod&node=n9`, no pod on `n9` mounts a claim, and `kube_node_info` names Kubernetes node `n9`
- **THEN** the four node families are issued restricted to `{n9}`, no claim-side or controller query is issued, and the body contains the node `n9` with its ordinary attributes and no edges

#### Scenario: Controller scope cross-namespace collision is harmless

- **WHEN** a loaded pod in `shop` is owned by StatefulSet `db` and the estate also holds StatefulSet `platform/db` carrying a different tracking-id
- **THEN** `kube_statefulset_annotations{statefulset="db"}` may return both series, the pod resolves the `shop/db` Application only, and the body is byte-identical to the body an unrestricted read would produce

#### Scenario: An annotation chunk failure fails the build

- **WHEN** one `kube_job_annotations` or `kube_replicaset_annotations` chunk fails with an upstream error while every other query succeeds
- **THEN** the build returns an error, the request is mapped as an upstream failure naming that family, and no body is returned

#### Scenario: The body carries nothing the skipped families feed

- **WHEN** any storage-graph body is inspected
- **THEN** it carries no `service` or `external` node, no `service-selects-pod` edge and no `containers` attribute
- **AND** `ALERTS` is the only family the build reads across the requested zone

#### Scenario: A by-reference restriction composes with the existing selectors

- **WHEN** a family is read by reference
- **THEN** the restriction is a sorted, de-duplicated, anchored alternation on the family's own identity label, **composed with** the family's fixed, request-invariant selector where it has one and with the request-scoped matchers the family already carries — never replacing either

#### Scenario: Pair-keyed families are read one query per ONTAP cluster

- **WHEN** a family is keyed by an `(ONTAP cluster, name)` pair
- **THEN** it is issued one query per ONTAP cluster, with that cluster as an equality and its names as the alternation, so no row outside the key set is read

#### Scenario: Restrictions are chunked under one shared byte budget

- **WHEN** a restriction is rendered
- **THEN** it is chunked deterministically under one byte budget shared by every data-derived alternation, one query per chunk per family, results merged in chunk order, each chunk issued under the bare family name for self-metrics and span dimensions
- **AND** a single value is always issued, even when it alone exceeds the budget

#### Scenario: A literal alternation holds at most 100 values

- **WHEN** a restriction renders its values as a literal alternation — every by-reference read, the per-namespace pod and claim reads, the pair-keyed Harvest reads and the root-derived reads on their identity label
- **THEN** no chunk carries more than 100 values in that alternation, whatever the byte budget admits, so the upstream resolves it by exact value lookup rather than by evaluating a regex against every value of the label
- **AND** the bound holds when the byte budget is unset or non-positive, and the split stays a pure function of the value set, so the body is byte-identical to the one an unbounded split produces
- **AND** the namespace alternation of a `pod` root's seed is taken from the request's roots, like a repeated `namespace` parameter, and is not bounded

#### Scenario: A suffix-token alternation is chunked by bytes alone

- **WHEN** the restricted `volume_labels` read renders FlexVol tokens as suffix branches (`.*<token>`)
- **THEN** it is chunked under the byte budget alone and is NOT split by value count, because the upstream resolves a `.*`-prefixed alternation by regex at any count and a further split would only multiply that evaluation

#### Scenario: An empty scope is neither issued nor tallied

- **WHEN** a by-reference family's scope is empty
- **THEN** it is not issued and is absent from the build's per-family series tally
- **AND** an issued family's tally entry is the total count of series its restrictions matched across every read of the build

#### Scenario: Chunk errors and cancellation

- **WHEN** a chunk of a by-reference family fails, or the caller cancels the request
- **THEN** the chunk error fails the build, as "Storage build fails closed on upstream query errors" requires, and caller-originated cancellation fails the request whatever the family

#### Scenario: Pods are read by namespace and name

- **WHEN** the build reads pods
- **THEN** `kube_pod_info` and `kube_pod_owner` are restricted to the `(namespace, pod)` pairs of the mounters of the tracked claims, the `pod` roots, the pods a `node` root placed on a root node, and the pods an `application` root recovered — one query per namespace, carrying that namespace as an equality and its pod names as the alternation — because a pod is identified by its namespace and name, never by its name alone
- **AND** no pair outside the set is read, and a same-named pod in another namespace — with its node and its controllers, which the next waves scope from what this read returns — is never fetched

#### Scenario: Every read of known pods is pair-keyed

- **WHEN** the build reads a family for pods it already knows
- **THEN** the same `(namespace, pod)` keying applies: to the `node` root's incarnation completion and claim-binding read, and to the `application` root's claim-binding read

#### Scenario: Claimless root pods are still materialised

- **WHEN** a `pod` root mounts no claim, or a recovered pod resolves a root Application
- **THEN** the pod is still materialised

#### Scenario: Kubernetes nodes are read for the loaded pods and node roots

- **WHEN** the build reads Kubernetes nodes
- **THEN** `kube_node_info`, `kube_node_status_addresses`, `kube_node_labels` and `kube_node_status_condition` are restricted on `node` to the nodes of the loaded pods and the `node` roots
- **AND** an unscheduled pod contributes no name

#### Scenario: First controller stage reads the pods' direct owners

- **WHEN** the build reads the controller families for the loaded pods
- **THEN** the eight controller families are read in two stages, each restricted to the owner names the loaded pods carry
- **AND** the first stage groups, from every loaded `kube_pod_owner` series with `owner_is_controller="true"` and a non-empty `owner_name`, the names by `owner_kind`, and issues `kube_replicaset_owner` and `kube_replicaset_annotations` on `replicaset` for the `ReplicaSet` names, `kube_job_owner` and `kube_job_annotations` on `job_name` for the `Job` names, `kube_statefulset_annotations` on `statefulset` for the `StatefulSet` names, and `kube_daemonset_annotations` on `daemonset` for the `DaemonSet` names

#### Scenario: Second controller stage reads the owners' owners

- **WHEN** the first controller stage has returned
- **THEN** the second stage, waiting on the first alone, issues `kube_deployment_annotations` on `deployment` for the union of the pods' direct `Deployment` owner names and the `owner_name` of every loaded `kube_replicaset_owner` series naming a `Deployment` owner
- **AND** `kube_cronjob_annotations` on `cronjob` for the union of the pods' direct `CronJob` owner names and the `owner_name` of every loaded `kube_job_owner` series

#### Scenario: Unpopulated controller scopes issue nothing

- **WHEN** no loaded pod is owned by a kind, a second-stage name set no first-stage series populated is empty, or an owner kind is one the reader resolves no Application for
- **THEN** the kind and the empty second-stage set issue no query, and the unresolved owner kind contributes no name

#### Scenario: Harvest aggregate gauges are read for the reached aggregates

- **WHEN** the build reads `aggr_new_status`, `aggr_space_used` and `aggr_space_total`
- **THEN** they are restricted to the `(ONTAP cluster, aggregate)` pairs the build's volume-label rows name, plus the aggregates an `aggr` root names (by name for a bare value, by `(ONTAP cluster, aggregate)` pair — one query per ONTAP cluster — for a qualified one) or an `ontap_cluster` root holds (by cluster)

#### Scenario: Harvest controller families are read for the reached controllers

- **WHEN** the build reads `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency` and `node_total_data`
- **THEN** they are restricted to the `(ONTAP cluster, controller)` pairs the build's volume-label rows and aggregate gauge rows name, plus an `ontap_node` root (by name) or an `ontap_cluster` root (by cluster)

#### Scenario: Fixed-policy families are keyed on the tracked claims' SVMs

- **WHEN** the build reads the two `qos_policy_fixed_max_throughput_*` families
- **THEN** they are restricted to the `(ONTAP cluster, SVM)` pairs of the tracked claims, which — unlike keying on the policy group — lets them run beside the QoS workload read instead of after it

#### Scenario: QoS workload families follow the scoped read

- **WHEN** the build reads the six QoS workload families
- **THEN** they are restricted as the `netapp-storage-graph` capability's "Scoped and batched QoS workload read" defines

#### Scenario: Readers consult families only inside their restriction

- **WHEN** a reader consults a by-reference family
- **THEN** it does so only at keys inside that family's restriction — a node family at the node of a loaded pod or a root; an owner or annotation family at a loaded pod's owner chain; an aggregate gauge at an aggregate the volume-label rows or a root name; a controller family at an aggregate's owner or a root; a fixed-policy series at a tracked claim's SVM — so the body is byte-identical to the body an unrestricted read would produce

#### Scenario: A same-named object admitted elsewhere leaves the body unchanged

- **WHEN** a restriction admits a same-named object elsewhere inside the request's selectors, since names are unique within a namespace, a cluster or an ONTAP cluster only
- **THEN** that series is keyed under its own cluster, is consulted by no loaded object, and leaves the body unchanged

