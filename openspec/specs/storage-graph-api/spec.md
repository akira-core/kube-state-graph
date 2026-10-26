# storage-graph-api Specification

## Purpose

Serves a storage-rooted flow graph — NetApp controller → aggregate → SVM → PVC → pod → Kubernetes node — with an I/O weight on every hop, so a Sankey diagram can answer "what runs on this filer?" and "which filer does this pod use?" from one endpoint.

## Requirements

### Requirement: Storage-flow graph endpoint

The server SHALL expose `GET /v1/storage-graph`, returning a storage-flow graph for a caller-specified `[start, end]` window in the same `{ apiVersion, clusters, elements: { nodes, edges } }` Cytoscape.js shape as `GET /v1/graph`, behind the same API-key authentication, per-build timeout and error mapping as `/v1/graph`, and described in the served OpenAPI document.

#### Scenario: Successful request

- **WHEN** a client sends `GET /v1/storage-graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z&az=zone-a&env=prod&aggr=aggr1`
- **THEN** the server returns 200 with a body containing exactly `apiVersion: "v1"`, `clusters`, and `elements` with `nodes` and `edges`

#### Scenario: Unauthenticated request rejected when keys configured

- **WHEN** API keys are configured and a client sends `GET /v1/storage-graph` without `X-API-Key`
- **THEN** the server returns 401 exactly as `/v1/graph` would

#### Scenario: The window is validated exactly as for the graph endpoint

- **WHEN** a client sends `GET /v1/storage-graph` without `start` or `end`, with a value that is neither RFC 3339 nor Unix seconds, or with `end` not after `start`
- **THEN** the server returns 400 with `missing_start` / `missing_end` / `invalid_start` / `invalid_end` / `invalid_range` exactly as `/v1/graph` would, since `start` and `end` are required and only `end > start` is checked

#### Scenario: Build timeout and upstream errors map as on the graph endpoint

- **WHEN** a storage build exceeds `--build-timeout`, or fails with an upstream, outside-retention or cancelled error
- **THEN** the server returns 504 `timeout` for the deadline and maps the other errors exactly as `/v1/graph` maps them

#### Scenario: The endpoint is documented

- **WHEN** a client fetches the served OpenAPI document
- **THEN** it describes `GET /v1/storage-graph`

### Requirement: Storage-graph zone selection

`az` and `env` SHALL be required and repeatable on `GET /v1/storage-graph`; the selected zones SHALL be the Cartesian product of their values, and both SHALL be pushed upstream exactly as the `/v1/graph` selector-level `az` / `env` dimensions are. The window, `az` and `env` SHALL be validated before the roots, in that order.

#### Scenario: Missing env

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&aggr=aggr1`, or carries only empty `env` values
- **THEN** the server returns 400 with `reason: "missing_env"`

#### Scenario: A combination the estate does not hold contributes nothing

- **WHEN** a client sends `?az=zone-a&az=zone-b&env=prod&aggr=aggr1` and the estate holds no `(zone-b, prod)` objects
- **THEN** the request is accepted and the `(zone-b, prod)` combination contributes nothing to the body

#### Scenario: Zone values are rendered as on the graph endpoint

- **WHEN** a request carries one or several `az` / `env` values
- **THEN** the values are sorted and de-duplicated, a single value is rendered as an equality and several as one anchored alternation, as matchers on every kube-state-metrics, kubelet and `ALERTS` family
- **AND** the NetApp Harvest families carry neither matcher
- **AND** backends are selected by the `az` SET for every zone-routed family, Harvest included

#### Scenario: The first failing check is reported

- **WHEN** a request fails several checks among the window, `az`, `env` and the root
- **THEN** the server reports the first failing check in that order

#### Scenario: Missing az

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&env=prod`
- **THEN** the server returns 400 with `reason: "missing_az"`

#### Scenario: Zone is checked before the root

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&env=prod` with no root
- **THEN** the server returns 400 with `reason: "missing_az"`

#### Scenario: Repeated env

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&env=prod&env=dev&aggr=aggr1`
- **THEN** the server returns 200, every kube-state-metrics, kubelet and `ALERTS` query carries `<az-key>="zone-a",<env-key>=~"dev|prod"`, and no Harvest query carries an `<az-key>` or `<env-key>` matcher

#### Scenario: Several zones are a Cartesian product

- **WHEN** a client sends `?az=zone-b&az=zone-a&env=prod&aggr=aggr1`
- **THEN** every zone-routed family is dispatched to the backends serving `zone-a` or `zone-b` (and catch-alls), every such query carries `<az-key>=~"zone-a|zone-b",<env-key>="prod"`, and the query strings are identical to those of `?az=zone-a&az=zone-b&env=prod&aggr=aggr1`

#### Scenario: Zone and environment reach upstream

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1`
- **THEN** every kube-state-metrics, kubelet and `ALERTS` query carries `<az-key>="zone-a",<env-key>="prod"`, every Harvest query is issued only to the `harvest` backends whose `zones` include `zone-a` (or catch-alls) and carries neither matcher, and no Kubernetes object of another zone or environment contributes to the body

#### Scenario: The Harvest side is not zone-matched

- **WHEN** one catch-all `harvest` backend holds `volume_labels` series stamped `az="zone-a"` and `az="zone-b"`, and a client sends `?az=zone-a&env=prod&aggr=aggr1`
- **THEN** no Harvest query carries an `<az-key>` or `<env-key>` matcher, the `aggr1` rows of both zones are read, and each loaded `zone-a` claim joins its own zone's series when it has one — the `zone-b` claims are never loaded, because every claim query carries `<az-key>="zone-a"`

### Requirement: Multi-zone storage body is the union of its zones

For a fixed root, cluster / namespace filter and window, the body of a `/v1/storage-graph` request selecting several `(az, env)` combinations SHALL equal the union of the bodies of the single-combination requests, except where a Harvest row of another selected zone widens a claim's candidates.

#### Scenario: Zones are never merged

- **WHEN** a request selects several `(az, env)` combinations
- **THEN** no node merges two objects of different zones or environments, and no flow weight sums claims of different zones or environments onto one edge unless those claims genuinely share that storage entity
- **AND** every alert matches zone-agreeingly as the `alert-overlay` capability defines

#### Scenario: Ids keep zones apart

- **WHEN** a multi-zone body is serialised
- **THEN** Kubernetes ids are composed from the cluster identity `<az>-<env>-<cluster>`, and NetApp ids are qualified by the ONTAP cluster, which the operator guarantees unique across the estate (together with ONTAP controller names)

#### Scenario: A claim prefers its own zone

- **WHEN** a claim's derived token matches FlexVols or QoS workload rows in its own zone and in another selected zone
- **THEN** the claim joins the FlexVols, and reads the QoS workload rows, of its own zone in preference to any other ("PVC-to-NetApp-aggregate edge join" and "Harvest QoS workload series as the I/O source", `netapp-storage-graph`)

#### Scenario: Another zone's Harvest row may widen the candidates

- **WHEN** a backend of another selected zone holds a Harvest row a claim also accepts — a colliding FlexVol, or any row for a claim with no row of its own zone
- **THEN** the multi-zone body MAY pick among more candidates than any single-zone body, because Harvest queries carry no request matcher
- **AND** the union stays exact for every claim whose matching Harvest rows all live in its own zone's `harvest` backends

#### Scenario: Upstream rows are matched by cluster identity

- **WHEN** the build matches an upstream row to a pod or claim it already read by the row's `cluster`
- **THEN** the key is the cluster identity — the `(az, env, cluster)` triple under the configured label keys — never the raw `cluster` label alone, so a cluster name reused in two selected zones is two clusters

#### Scenario: A multi-zone body is the union of its zones

- **WHEN** filers `ontap-a` (zone-a) and `ontap-b` (zone-b) each hold an aggregate `aggr1` with mounted claims, cluster `c1` exists in both zones with a pod `shop/orders-0`, and a client sends `?az=zone-a&az=zone-b&env=prod&aggr=aggr1`
- **THEN** the body's node and edge sets equal the union of the bodies of `?az=zone-a&env=prod&aggr=aggr1` and `?az=zone-b&env=prod&aggr=aggr1`: `netapp/ontap-a/aggr/aggr1` and `netapp/ontap-b/aggr/aggr1` are distinct nodes, the two `shop/orders-0` pods are distinct nodes under `zone-a-prod-c1` and `zone-b-prod-c1`, each edge carries the same weight it carries in its single-zone body, and `clusters` lists both identities

#### Scenario: A cluster name reused across selected zones is two clusters

- **WHEN** a client sends `?az=zone-a&az=zone-b&env=prod&node=worker-1`, cluster `c1` exists in both zones, `shop/db-0` runs on `worker-1` in `zone-a`, and a same-named `shop/db-0` in `zone-b` was recreated on `worker-2`
- **THEN** only the `zone-a` pod is placed on `worker-1` and tracked from it; the `zone-b` pod's newest incarnation does not displace or hide it

### Requirement: Storage-graph narrowing filters and ignored parameters

`GET /v1/storage-graph` SHALL accept `cluster` and `namespace` as optional, repeatable narrowing filters with `/v1/graph` semantics, and SHALL ignore `prune`, `edge_type` and every other unknown parameter without error, whatever value they carry.

#### Scenario: Prune never selects the connectivity prune

- **WHEN** a client sends `prune=true` or `prune=false`
- **THEN** the endpoint applies its own reachability projection and never the connectivity prune

#### Scenario: The withdrawn edge-type parameter is ignored

- **WHEN** a client sends `edge_type` with any value
- **THEN** it is ignored, since `edge_type` is withdrawn from `/v1/graph` as well and is no longer a parameter of any endpoint

#### Scenario: Repeated narrowing filters

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1&cluster=c1&cluster=c2&namespace=shop`
- **THEN** the request is accepted and `cluster` and `namespace` narrow the body exactly as they narrow `/v1/graph`

#### Scenario: Graph-only and withdrawn parameters are ignored

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&env=prod&aggr=aggr1&prune=false&edge_type=not-a-type`
- **THEN** the server returns 200 with a body byte-identical to the same request without `prune` and `edge_type` — neither value is validated, and neither narrows or widens the body

### Requirement: Storage-graph clusters array lists Kubernetes identities only

The top-level `clusters` array of a `/v1/storage-graph` body SHALL list the Kubernetes cluster identities present on emitted `pod` / `node` / `pvc` nodes and SHALL NOT list an ONTAP cluster name.

#### Scenario: ONTAP clusters stay out of the clusters array

- **WHEN** a body holds the NetApp nodes of ONTAP cluster `ontap-prod` and the pods, Kubernetes nodes and PVCs of `zone-a-prod-c1`
- **THEN** `clusters` is `["zone-a-prod-c1"]`

### Requirement: One root kind per request

A `/v1/storage-graph` request SHALL carry exactly ONE root kind with at least one non-empty value: none SHALL be rejected 400 with `reason: "missing_root"`, and two or more root kinds 400 with `reason: "invalid_scope"` and a message naming the parameters. The values of the one kind SHALL be OR-combined and matched exactly and case-sensitively.

#### Scenario: Storage root finds its consumers

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1` and two claims on `aggr1` are mounted by pods `shop/orders-0` and `shop/catalog-0`
- **THEN** the body contains the controller owning `aggr1`, `aggr1`, the SVMs of both claims, both PVCs, both pods and their Kubernetes nodes, and no other pod

#### Scenario: Workload root finds its storage

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0` and that pod mounts one NetApp-backed claim on `(ontap-prod, ontap-prod-01, aggr1, svm_shop)`
- **THEN** the body contains exactly the chain `netapp/ontap-prod/ontap-prod-01 → netapp/ontap-prod/aggr/aggr1 → netapp/ontap-prod/svm/svm_shop → <pvc> → <pod> → <node>`

#### Scenario: ONTAP controller root finds the claims on the aggregates it owns

- **WHEN** a client sends `?az=zone-a&env=prod&ontap_node=ontap-prod-01`, `ontap-prod-01` owns `aggr1`, `ontap-prod-02` owns `aggr2`, and `svm_shop` holds mounted claims on both aggregates
- **THEN** the body contains `ontap-prod-01`, `aggr1`, `svm_shop`, the `aggr1` claims and their pods and Kubernetes nodes, and neither `aggr2` nor any claim on it

#### Scenario: Kubernetes node root finds the storage of its pods

- **WHEN** a client sends `?az=zone-a&env=prod&node=worker-1`, `worker-1` hosts `shop/orders-0`, which mounts a claim on `aggr1`, and `worker-2` hosts `shop/catalog-0`, which mounts a claim on `aggr2`
- **THEN** the body contains `worker-1`, `shop/orders-0`, its claim, SVM, `aggr1` and the controller owning it, and neither `shop/catalog-0` nor `aggr2`

#### Scenario: A Kubernetes node root never matches an ONTAP controller

- **WHEN** a client sends `?az=zone-a&env=prod&node=ontap-prod-01` and `ontap-prod-01` names only an ONTAP controller
- **THEN** the server returns 200 with empty `nodes` and `edges` and an empty `clusters` array

#### Scenario: A bare aggregate name roots every filer

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1` and both `ontap-prod` and `ontap-lab` hold an aggregate `aggr1` with mounted claims
- **THEN** the body contains `netapp/ontap-prod/aggr/aggr1` and `netapp/ontap-lab/aggr/aggr1` with each one's complete paths

#### Scenario: Application root finds its storage across namespaces

- **WHEN** a client sends `?az=zone-a&env=prod&application=checkout`, pods `shop/orders-0` and `platform/queue-0` resolve `data.application="checkout"` and mount claims on `aggr1` and `aggr2` respectively, and pod `shop/ledger-0` resolves `data.application="ledger"` and mounts a claim on `aggr1`
- **THEN** the body contains the complete paths of the two `checkout` claims — both controllers, both aggregates, both SVMs, both PVCs, both pods and their nodes — and neither `shop/ledger-0` nor its claim

#### Scenario: Application root matches a claim's own Application

- **WHEN** a client sends `?az=zone-a&env=prod&application=billing`, claim `shop/ledger-data` carries its own tracking-id naming `billing` and is mounted by pod `shop/ledger-0`, which resolves `data.application="ledger"`
- **THEN** the claim's complete path, `shop/ledger-0` included, is retained; `shop/ledger-0` is present as part of that path and would not be drawn on its own

#### Scenario: Two root kinds are rejected

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1&pod=shop/orders-0`
- **THEN** the server returns 400 with `reason: "invalid_scope"` and a message naming `aggr` and `pod`, and issues no upstream query

#### Scenario: An ONTAP cluster no longer qualifies an aggregate

- **WHEN** a client sends `?az=zone-a&env=prod&ontap_cluster=ontap-prod&aggr=aggr1`
- **THEN** the server returns 400 with `reason: "invalid_scope"`, because `ontap_cluster` and `aggr` are two root kinds

#### Scenario: A qualified aggregate roots one filer

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=ontap-prod/aggr1` and both `ontap-prod` and `ontap-lab` hold an aggregate `aggr1` with mounted claims
- **THEN** the body contains `netapp/ontap-prod/aggr/aggr1` with its complete paths and does not contain `netapp/ontap-lab/aggr/aggr1` or any claim reachable only through it

#### Scenario: Bare and qualified values combine

- **WHEN** a client sends `?az=zone-a&env=prod&svm=ontap-prod/svm0&svm=svm_shop`, `svm0` exists on `ontap-prod` and `ontap-lab`, and `svm_shop` on `ontap-lab` only
- **THEN** the roots are `netapp/ontap-prod/svm/svm0` and `netapp/ontap-lab/svm/svm_shop`; `netapp/ontap-lab/svm/svm0` is not a root

#### Scenario: Malformed qualified value

- **WHEN** a client sends `?aggr=ontap-prod/` or `?aggr=/aggr1` or `?svm=a/b/c`
- **THEN** the server returns 400 with `reason: "invalid_scope"`

#### Scenario: A request with no root is rejected

- **WHEN** a client sends `?start=...&end=...&az=zone-a&env=prod` with no root parameter, or only empty root values such as `?aggr=`
- **THEN** the server returns 400 with `reason: "missing_root"` and issues no upstream query

#### Scenario: Filters combine with any root kind

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1&namespace=shop&cluster=c1`
- **THEN** the request is accepted with `aggr` as its one root kind, and `namespace` and `cluster` narrow the Kubernetes side as they do for every root kind

#### Scenario: Malformed pod root

- **WHEN** a client sends `?pod=orders-0`
- **THEN** the server returns 400 with `reason: "invalid_scope"`

#### Scenario: Application root value is validated like every selector value

- **WHEN** a client sends `?application=` followed by a value of 300 bytes, or a value carrying a control character
- **THEN** the server returns 400 with `reason: "invalid_scope"`; a bare `?application=` is a no-op

#### Scenario: Claim root finds its storage and its consumers

- **WHEN** a client sends `?az=zone-a&env=prod&pvc=shop/orders-data`, the claim joins `(ontap-prod, ontap-prod-01, aggr1, svm_shop)` and is mounted by `shop/orders-0` on `worker-1`
- **THEN** the body contains exactly the chain `netapp/ontap-prod/ontap-prod-01 → netapp/ontap-prod/aggr/aggr1 → netapp/ontap-prod/svm/svm_shop → <shop/orders-data> → <shop/orders-0> → <worker-1>`, and no other claim sharing `aggr1` or `svm_shop`

#### Scenario: Volume root resolves to its claim

- **WHEN** a client sends `?az=zone-a&env=prod&pv=pvc-ab12-cd34` and claim `shop/orders-data` is bound to PersistentVolume `pvc-ab12-cd34`
- **THEN** the body is identical to the body of `?az=zone-a&env=prod&pvc=shop/orders-data`

#### Scenario: A claim root matches the claim in every cluster

- **WHEN** a client sends `?az=zone-a&env=prod&pvc=shop/data` and clusters `c1` and `c2` both hold a claim `shop/data`
- **THEN** both claims are roots and the body contains each one's path; adding `&cluster=c1` narrows it to the `c1` claim

#### Scenario: Malformed claim root

- **WHEN** a client sends `?pvc=orders-data` or `?pvc=shop/orders/data`
- **THEN** the server returns 400 with `reason: "invalid_scope"`

#### Scenario: Claim roots combine with no other root kind

- **WHEN** a client sends `?az=zone-a&env=prod&pvc=shop/orders-data&pv=pvc-ab12-cd34`
- **THEN** the server returns 400 with `reason: "invalid_scope"` and a message naming `pvc` and `pv`, and issues no upstream query

#### Scenario: Missing root

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&env=prod`
- **THEN** the server returns 400 with `reason: "missing_root"`

#### Scenario: The accepted root kinds

- **WHEN** a request names a root
- **THEN** the endpoint accepts exactly these root kinds, each a repeatable parameter: `ontap_cluster=<name>`, `ontap_node=<name>`, `aggr=<name>` or `aggr=<ontap_cluster>/<name>`, `svm=<name>` or `svm=<ontap_cluster>/<name>`, `node=<name>`, `pod=<namespace>/<pod-name>`, `pvc=<namespace>/<claim-name>`, `pv=<name>` and `application=<name>`
- **AND** `cluster` and `namespace` are narrowing filters, not root kinds, and combine with any root kind

#### Scenario: Root values are validated like selector values

- **WHEN** a root value exceeds 253 bytes, is not valid UTF-8, or carries a control character
- **THEN** the server returns 400 with `reason: "invalid_scope"`, exactly as for a `/v1/graph` selector value

#### Scenario: Empty root values are dropped

- **WHEN** a request carries an empty root value such as a bare `?aggr=`
- **THEN** the value is dropped, is a no-op, and does not count as a root

#### Scenario: Root values are OR-combined and matched exactly

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1&aggr=aggr2` and the estate also holds `AGGR1`
- **THEN** a path is retained when it touches `aggr1` or `aggr2`, and `AGGR1` is not a root

#### Scenario: An ONTAP cluster root

- **WHEN** a client roots at `ontap_cluster=<name>`
- **THEN** every controller, aggregate and SVM in that ONTAP cluster is a root

#### Scenario: An ONTAP controller root follows aggregate ownership

- **WHEN** a client roots at `ontap_node=<name>`
- **THEN** a path is retained by it when the path's aggregate is owned by that controller — the owner the `netapp-storage-graph` capability's owning-controller vote resolves — so the root reaches the aggregates the controller owns, the SVMs and claims on those aggregates, and the pods, Kubernetes nodes, Applications and namespaces below them
- **AND** it never reaches a claim on an aggregate another controller owns, even when both aggregates serve one SVM

#### Scenario: Bare and qualified aggregate and SVM roots

- **WHEN** a client roots at `aggr` or `svm`
- **THEN** a bare value is matched on EVERY ONTAP cluster the selected zones' Harvest backends return, so a name present on two filers roots both, and a qualified value roots exactly the aggregate or SVM of that name on that ONTAP cluster
- **AND** a value containing `/` is the qualified form and SHALL split on exactly one `/` into two non-empty segments, the ONTAP cluster then the name; any other value containing `/` is rejected 400 `invalid_scope`

#### Scenario: A qualified value whose name is also given bare adds nothing

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1&aggr=ontap-prod/aggr1`
- **THEN** bare and qualified values are OR-combined within the one root kind, and the body equals that of `?az=zone-a&env=prod&aggr=aggr1`

#### Scenario: A Kubernetes node root matches every cluster

- **WHEN** a client roots at `node=worker-1` and clusters `c1` and `c2` of the selected estate both hold a node `worker-1`
- **THEN** both Kubernetes nodes are roots, and an ONTAP controller named `worker-1` is not

#### Scenario: Pod and claim roots name a namespace and an object

- **WHEN** a client roots at `pod=<namespace>/<pod-name>` or `pvc=<namespace>/<claim-name>`
- **THEN** a `pod` value names one pod, and a `pvc` value names one PersistentVolumeClaim, matched in every Kubernetes cluster of the selected estate
- **AND** a value without exactly one `/` separating two non-empty segments is rejected 400 `invalid_scope`

#### Scenario: A volume root roots the claims bound to it

- **WHEN** a client roots at `pv=<name>`
- **THEN** the value is the PersistentVolume's bare (cluster-scoped) name, matched in every Kubernetes cluster of the selected estate, and roots the claim(s) whose `kube_persistentvolumeclaim_info` series names it as `volumename`
- **AND** a PersistentVolume bound to no claim roots nothing

#### Scenario: An Application root matches pods and claims

- **WHEN** a client roots at `application=<name>`
- **THEN** the value is matched in the form `data.application` carries it (the segment of the tracking-id before the first `:`), and a path is retained when its **pod or its claim** carries that Application — the claim's own annotation or the Application it inherited from a mounting pod, exactly as the body reports it
- **AND** a value containing `:` can name no Application and matches nothing

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

- **WHEN** a root kind's first read would take more than a fixed maximum number of queries under the shared byte budget
- **THEN** the request is rejected 400 with `reason: "invalid_scope"` before any upstream query is issued — root parameters are repeatable and the parser bounds each value's length, never their count, so a root set is a scope a client can inflate
- **AND** the decision needs no upstream data, since the first read's shape is a pure function of the root values and the byte budget
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

### Requirement: The tracked claims expand to both ends of the chain

From the tracked claim set, the build SHALL read everything the body draws for those claims, restricted by reference, with four **completions** that keep each reader's whole-population rules intact. The body SHALL be **byte-identical** to the body an unrestricted read of the same zone produces under the same projection, for every root kind.

#### Scenario: A clone on a lexically-smaller aggregate keeps its pick

- **WHEN** a claim's derived token matches FlexVol `trident_pvc_x` on `aggr09` and a clone `snap_trident_pvc_x` on `aggr00`, and the request roots at `aggr=aggr09`
- **THEN** candidate completion loads both series, the aggregate pick resolves to `aggr00` exactly as an unrestricted read resolves it, the claim is not retained by the `aggr09` root, and the body is byte-identical to the unrestricted body

#### Scenario: A cross-filer FlexVol-name collision keeps its pick

- **WHEN** one FlexVol name exists on `ontap-prod` and on `ontap-lab`, the claim's token matches both, and the request is `?pod=` on a pod mounting that claim
- **THEN** candidate completion loads both series, the pick resolves to the lexically-smallest `(ontap-cluster, aggr)` exactly as an unrestricted read resolves it, and the body is byte-identical to the unrestricted body

#### Scenario: Takeover ownership survives a tracked read

- **WHEN** a request reaches a claim on `aggr09`, whose volume-label series disagree on the owning `node` because a takeover happened inside the window
- **THEN** owner completion loads every `aggr09` series, the vote resolves to the lexically-smallest non-empty `node` exactly as an unrestricted read resolves it, and the `node-aggr` tier names that controller

#### Scenario: Owner completion under an SVM root

- **WHEN** a client roots at `svm=svm_shop`, the `svm_shop` volumes on `aggr09` all name controller `ontap-prod-02`, and another SVM's volume on `aggr09` names `ontap-prod-01`
- **THEN** owner completion loads every `aggr09` series, the vote resolves to `ontap-prod-01`, and the `node-aggr` tier names that controller

#### Scenario: A shared claim keeps its split across nodes

- **WHEN** a client sends `?az=zone-a&env=prod&node=worker-1` and the RWX claim `shop/shared-data` is mounted by `orders-0` on `worker-1` and by `report-0` and `report-1` on `worker-2`
- **THEN** the claim-binding read by `(namespace, claim)` loads all three mounters, the body retains only the `orders-0` path, its `pvc-pod` edge carries one third of the claim's figures with `labels.attribution="split"`, and the body is byte-identical to the unrestricted body

#### Scenario: The QoS scope follows the tracked claims

- **WHEN** a build's tracked claims match 60 FlexVols, against 1200 an unrestricted build would match
- **THEN** the QoS workload queries restrict `volume` to those 60 names, and every I/O measurement on a retained path is identical to the unrestricted build's

#### Scenario: A large tracked set is chunked, never read across the zone

- **WHEN** a client roots at `ontap_cluster=` on a filer whose candidates would take many more claim-info queries than the concurrency bound
- **THEN** `kube_persistentvolumeclaim_info` is issued in as many chunks as the byte budget requires, merged in chunk order, and no query without a `volumename` restriction is issued

#### Scenario: An empty tracked set issues no expansion

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1` and no candidate names a claim
- **THEN** no claim-binding, pod, Kubernetes-node, controller, QoS or fixed-policy query is issued, and the body contains `aggr1` and its owning controller only

#### Scenario: A failed chunk of any read fails the build

- **WHEN** one chunk of a root, candidate-completion, owner-completion or claim-side read fails with an upstream error while every other query succeeds
- **THEN** the build returns an error, the request is mapped as an upstream failure naming that family, the failure is counted on `kube_state_graph_upstream_query_failures_total`, and no body is returned

#### Scenario: The claim side is read by namespace and claim

- **WHEN** the tracked claim set is non-empty
- **THEN** `kube_persistentvolumeclaim_info` (when the root's reads did not already return the claim), `kube_persistentvolumeclaim_annotations`, the two kubelet volume-stats families and the claim-binding family are each restricted to the tracked `(namespace, claim)` pairs — one query per namespace, carrying that namespace as an equality and its tracked claim names as the alternation on `persistentvolumeclaim` — so a same-named claim in another namespace is never read
- **AND** rows are kept only when their `(az, env, cluster, namespace, claim)` names a tracked claim

#### Scenario: Mounter completion loads every mounting pod

- **WHEN** the claim-binding read by `(namespace, claim)` names the pods mounting a tracked claim
- **THEN** EVERY such pod is loaded, because a shared claim's `pvc-pod` weight is split over the mounters present in the built graph and an unannotated claim inherits the Application of its mounters, so a mounter left unloaded would change both

#### Scenario: The workload side follows the mounters

- **WHEN** the mounters are known
- **THEN** their pods, their Kubernetes nodes and their controllers are read as "Storage build reads every family by reference" defines

#### Scenario: Candidate completion reads every FlexVol a claim can match

- **WHEN** the tracked claims are known
- **THEN** `volume_labels` is read restricted on `volume` to the derived tokens of every tracked claim, rendered as the suffix comparison `.*<token>`, so each claim's aggregate and SVM picks run over its WHOLE candidate set — a clone or a same-named FlexVol on another filer included
- **AND** its rows do not add claims to the tracked set

#### Scenario: Owner completion reads every touched aggregate whole

- **WHEN** any volume-label row of the build names an `(ONTAP cluster, aggregate)` pair
- **THEN** that aggregate is read whole, except an aggregate a read of the build already covered whole, so the owning-controller vote runs over every row of every aggregate the body can draw
- **AND** under an `svm` root this includes the aggregates candidate completion alone named, because the aggregate and SVM picks are separate and a claim retained through its SVM can land on such an aggregate
- **AND** its rows do not add claims

#### Scenario: The storage side is read last

- **WHEN** the volume-label reads are complete
- **THEN** the aggregate and controller gauge families, the QoS workload families and the fixed-policy families are read as "Storage build reads every family by reference" defines

#### Scenario: Volume-label reads are merged before the parse

- **WHEN** a build issued several volume-label reads
- **THEN** they are merged, de-duplicated by label set, before the parse, and every downstream consumer — the aggregate, SVM and owner picks, the inventory and the QoS `volume` scope — runs over the merged result

#### Scenario: The one permitted divergence from an unrestricted read

- **WHEN** an aggregate or controller is named by the volume-label family alone, with no gauge series of its own, outside what the build reached
- **THEN** that entity is not materialised, which can change whether an alert without a `cluster` label matches a unique entity; this is the only permitted divergence from the unrestricted body, and the stock Harvest templates name every one

#### Scenario: Data-derived scopes are chunked, never widened

- **WHEN** a scope derived from upstream data — tracked claims, mounters, touched aggregates, controllers, tokens — is read
- **THEN** it is chunked under the shared byte budget, issued with bounded concurrency and merged in chunk order however large it is, and it is NEVER replaced by a read across the zone
- **AND** an empty tracked set issues no claim-side, workload-side or storage-side query beyond the root's own reads

#### Scenario: Join coverage counts over the tracked claims

- **WHEN** the build reports the join-coverage signal of the `netapp-storage-graph` capability
- **THEN** it counts over the tracked claims exactly as `/v1/graph` counts over its loaded claims

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

### Requirement: Application roots are tracked through their controllers and claims

When a `/v1/storage-graph` request's root kind is `application`, the build SHALL track two sets: the pods a three-stage recovery (and, when configured, the application-metadata seed) attributes to the root Applications, and the claims carrying a root Application of their own. The recovery SHALL be a **candidate generator, never a judge**.

#### Scenario: A Deployment-managed stateless pod is recovered and drawn

- **WHEN** a client sends `?az=zone-a&env=prod&application=checkout`, Deployment `shop/web` carries tracking-id `checkout:apps/Deployment:shop/web`, `kube_replicaset_owner` names ReplicaSet `web-7d9f` as owned by Deployment `web`, `kube_pod_owner` names pod `web-7d9f-abc` as controlled by that ReplicaSet, and the pod mounts no claim
- **THEN** stage 1 issues the six annotation families each with a matcher on `annotation_argocd_argoproj_io_tracking_id` admitting exactly the values whose segment before the first `:` is `checkout`, alongside `annotation_argocd_argoproj_io_tracking_id!=""` and `<az-key>="zone-a",<env-key>="prod"`; stage 2 issues `kube_replicaset_owner` restricted to `owner_kind="Deployment"` and `owner_name` in `{web}` and no `kube_job_owner`; stage 3 issues `kube_pod_owner` restricted to `owner_is_controller="true"`, `owner_kind="ReplicaSet"` and `owner_name` in `{web-7d9f}`; the pod read restricts `pod` to `{web-7d9f-abc}` plus every mounter of a tracked claim; and the body contains `web-7d9f-abc` with `data.application="checkout"`, its compound parents, and no edge

#### Scenario: A CronJob-managed pod is recovered through the reverse Job hop

- **WHEN** a client sends `?application=reports`, CronJob `batch/nightly` carries tracking-id `reports:batch/CronJob:batch/nightly`, `kube_job_owner` names Job `nightly-28901` as controlled by it, and `kube_pod_owner` names pod `nightly-28901-x` as controlled by that Job
- **THEN** stage 2 issues `kube_job_owner` with `owner_kind="CronJob",owner_is_controller="true"` and `owner_name` in `{nightly}`, stage 3 issues `kube_pod_owner` for kind `Job` with `owner_name` in `{nightly-28901}`, and the body contains `nightly-28901-x` with `data.application="reports"` and `data.owner={kind:"Job", name:"nightly-28901"}`

#### Scenario: An over-admitted pod is loaded and dropped

- **WHEN** a client sends `?application=beta`, and StatefulSet `shop/db` carries two tracking-ids in the window, `alpha:apps/StatefulSet:shop/db` and `beta:apps/StatefulSet:shop/db`
- **THEN** stage 1 admits `db`, its pods are read, each resolves `data.application="alpha"` (the lexically-smallest tracking-id), none is a root, and the body is byte-identical to the body of the same request against an estate where `db` carries only the `alpha` tracking-id

#### Scenario: No controller or claim carries the Application

- **WHEN** a client sends `?application=typo`, no controller-annotation or claim-annotation series in the selected estate carries a tracking-id naming `typo`, and no application-metadata row carries `app="typo"`
- **THEN** stage 1, the claim-annotation read and the application-metadata seed issue their queries, no stage-2, stage-3, claim-binding or pod query is issued, and the body is empty

#### Scenario: Only the recovered pods' bindings are read

- **WHEN** a client sends `?application=checkout`, the recovery returns `orders-0`, and the estate also holds pods `catalog-0` (claim `catalog-data`, unannotated, mounted by nobody else) and `ledger-0` (claim `ledger-data`, own-annotated `billing:…`)
- **THEN** the claim-binding query by pod is restricted to `namespace="shop",pod="orders-0"`, `catalog-0` and `ledger-0` are never read, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: A same-named pod in another namespace contributes no claim

- **WHEN** a client sends `?application=checkout`, the recovery returns `shop/web-0`, and `platform/web-0`, owned by a controller carrying no root Application, mounts claim `platform/platform-data`
- **THEN** the claim-binding query by pod carries `namespace="shop",pod="web-0"`, `platform-data` is never tracked, and no claim-side, pod, node or Harvest query is issued on its account

#### Scenario: Every mounter of a tracked claim is read

- **WHEN** a client sends `?application=checkout`, the recovery returns `orders-0`, and the RWX claim `shared-data` is mounted by `orders-0` and by `report-0`, whose controller resolves `alpha`
- **THEN** the pod scope is `{orders-0, report-0}`, the claim inherits `alpha` (the lexically-smallest mounter Application) so `report-0`'s path is not retained, and the `pvc-pod` edge to `orders-0` carries half the claim's figures with `labels.attribution="split"` — exactly as when every binding pod is read

#### Scenario: A co-mounter that sorts after the root is drawn through inheritance

- **WHEN** the same claim is instead mounted by `orders-0` (`checkout`) and `zeta-0` (`zeta`)
- **THEN** the claim inherits `checkout`, both `pvc-pod` paths are retained, and `zeta-0` is present as part of the claim's path with a split edge of its own

#### Scenario: A claim annotated with the Application is tracked without its pods

- **WHEN** a client sends `?application=billing`, claim `shop/ledger-data` carries tracking-id `billing:…`, and its only mounter `shop/ledger-0` resolves `ledger`
- **THEN** the claim-annotation read tracks `shop/ledger-data`, mounter completion loads `shop/ledger-0`, and the claim's complete path is retained

#### Scenario: A required stage failure fails the build

- **WHEN** a stage-3 `kube_pod_owner` chunk fails with an upstream error
- **THEN** the build returns an error and the request is mapped as an upstream failure, exactly as an unrestricted `kube_pod_owner` failure is

#### Scenario: An over-large Application set is rejected

- **WHEN** a client sends thousands of `application=` values, enough that a stage-1 family's restriction would take more queries than the maximum
- **THEN** the server returns 400 with `reason: "invalid_scope"`, no upstream query is issued, and no family is read across the zone in its place

#### Scenario: The namespace filter narrows the recovery

- **WHEN** a client sends `?application=checkout&namespace=shop` and `checkout` owns controllers in `shop` and `platform`
- **THEN** every stage-1, stage-2 and stage-3 query and the claim-annotation query carry `namespace="shop"`, only the `shop` pods and claims are tracked, and the body holds no `platform` pod

#### Scenario: A request with another root kind issues no recovery

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1`
- **THEN** no query restricted on `annotation_argocd_argoproj_io_tracking_id` or on `owner_name`, and no application-metadata query restricted on the `app` label, is issued

#### Scenario: The recovery is tallied under the family name

- **WHEN** a client sends `?application=checkout`, stage 1 returns two `kube_deployment_annotations` series, and the by-reference controller read later returns three for the loaded pods' Deployments
- **THEN** the per-family tally carries `kube_deployment_annotations` with `5`, and a family only the recovery issued is present with the count of series its restriction matched

#### Scenario: A stage-1 annotation chunk failure fails the build

- **WHEN** a stage-1 `kube_job_annotations` chunk fails with an upstream error while every other query succeeds
- **THEN** the build returns an error, the request is mapped as an upstream failure naming `kube_job_annotations`, and no body is returned

#### Scenario: A pod named only by the metric is seeded and drawn

- **WHEN** the metric is configured, a client sends `?application=canary-app`, no controller carries a tracking-id naming `canary-app`, and an application-metadata row carries `app="canary-app"` for pod `shop/canary-0` (owned by a `Rollout`), which mounts claim `shop/canary-data`
- **THEN** the seed returns `shop/canary-0`, its claim is tracked, and the body contains `canary-0` with `data.application="canary-app"` and its complete storage path

#### Scenario: A seeded pod whose resolved Application differs is dropped

- **WHEN** a client sends `?application=x`, and pod `shop/web-0` has application-metadata rows with `app` values `x` and `w` in the window
- **THEN** the seed loads `web-0`, it resolves `data.application="w"` (the lexically-smallest value), it is not a root, and the body equals the same request against an estate where its only row carries `app="w"`

#### Scenario: Unserved metric issues no seed

- **WHEN** the metric is configured but no backend serves `appmeta`, and a client sends `?application=checkout`
- **THEN** no application-metadata query is issued and the body equals that of a deployment with the metric unset

#### Scenario: The two tracked sets and their sources

- **WHEN** the root kind is `application`
- **THEN** the first set is fed by two candidate sources: the pods owned by a controller whose ArgoCD tracking-id names one of the root Applications, recovered in three stages, together with — when the `app-metadata-source` metric is configured and served — the `(cluster, namespace, pod)` of every application-metadata row whose `app` equals a root value
- **AND** the second set is the claims carrying a root Application of their own, read from `kube_persistentvolumeclaim_annotations` restricted on `annotation_argocd_argoproj_io_tracking_id` exactly as stage 1 restricts the controller families

#### Scenario: Recovered pods' bindings name the tracked claims

- **WHEN** the recovered pods are known
- **THEN** the claim-binding family is read restricted to their `(namespace, pod)` pairs — the namespace the recovering `kube_pod_owner` row carries, one query per namespace — keeping only rows whose `(cluster, namespace, pod)` is a recovered pod, so a same-named pod elsewhere contributes no claim
- **AND** the claims those rows bind, together with the own-annotated claims, form the tracked claim set of "Every root kind is tracked from its own tier to its claims", and every mounter of a tracked claim is then loaded by the mounter completion of "The tracked claims expand to both ends of the chain"

#### Scenario: The forward resolution alone decides a root

- **WHEN** a pod is recovered
- **THEN** it is read exactly like any other scoped pod, its Application is resolved by the same controller-annotation rules every pod's is, and whether it is a root is decided solely by the projection over that resolved value
- **AND** a recovered pod whose resolved Application is not a root value — a controller whose lexically-smallest tracking-id in the window names a different Application — is loaded and then dropped, so the body is a pure function of the forward resolution and never of the recovery

#### Scenario: The stages are gated in order

- **WHEN** the recovery runs
- **THEN** it runs in three stages, each waiting on the previous alone, and stage 1 is derived from the request, so nothing precedes it

#### Scenario: Stage 1 reads controllers by tracking-id

- **WHEN** stage 1 runs
- **THEN** each of the six controller-annotation families (`kube_deployment_annotations`, `kube_statefulset_annotations`, `kube_daemonset_annotations`, `kube_replicaset_annotations`, `kube_job_annotations`, `kube_cronjob_annotations`) is issued restricted on `annotation_argocd_argoproj_io_tracking_id` to exactly the values whose segment before the first `:` is one of the root values (a value with no `:` matches when it equals a root value verbatim), composed with the family's fixed selector and the request-scoped matchers — never replacing either
- **AND** root values are sorted, de-duplicated and escaped so a value carrying a regex metacharacter matches itself and nothing else
- **AND** the stage yields one name set per kind from each family's identity label (`deployment`, `statefulset`, `daemonset`, `replicaset`, `job_name`, `cronjob`)

#### Scenario: Stage 2 takes the reverse hops

- **WHEN** stage 2 runs
- **THEN** `kube_replicaset_owner` is issued restricted to `owner_kind="Deployment"` and `owner_name` in the stage-1 Deployment names, yielding ReplicaSet names, and `kube_job_owner` is issued restricted on `owner_name` to the stage-1 CronJob names beside its fixed selector, yielding Job names
- **AND** a stage-1 name set that is empty issues no query for its hop

#### Scenario: Stage 3 reads pods by owner

- **WHEN** stage 3 runs
- **THEN** `kube_pod_owner` is issued restricted to `owner_is_controller="true"`, one query per owner kind whose name set is non-empty: `ReplicaSet` for stage-1 ∪ stage-2 ReplicaSet names; `Job` for stage-1 ∪ stage-2 Job names; `StatefulSet` and `DaemonSet` for their stage-1 names; `Deployment` and `CronJob` for their stage-1 names, for a pod directly owned by either
- **AND** the recovered `pod` labels are the recovered pods

#### Scenario: Recovery reads are chunked and fail closed

- **WHEN** a recovery restriction is issued
- **THEN** it is chunked deterministically under the shared byte budget, one query per chunk, results merged in chunk order, each chunk issued under the bare family name
- **AND** a chunk error on any recovery family — the six controller-annotation families, `kube_persistentvolumeclaim_annotations`, `kube_replicaset_owner`, `kube_job_owner` and `kube_pod_owner` — fails the build, as "Storage build fails closed on upstream query errors" requires, and caller-originated cancellation fails the request whatever the family

#### Scenario: The application-metadata seed runs beside stage 1

- **WHEN** the metric is configured and served
- **THEN** one application-metadata query is issued in parallel with stage 1, restricted on the configured `app` label to the sorted, de-duplicated, escaped root values (an exact match — the value is not a tracking-id), composed with the request-scoped matchers and chunked under the shared byte budget, and its pods are unioned with stage 3's before the claim-binding and pod reads
- **AND** like the recovery it is a candidate generator: a seeded pod is a root only if its resolved Application is a root value, and a chunk error fails the build

#### Scenario: Only request-derived reads are bounded

- **WHEN** stage 1, the claim-annotation read or the application-metadata seed would take more than the fixed maximum number of queries
- **THEN** the request is rejected 400 with `reason: "invalid_scope"` before any upstream query is issued, as "Every root kind is tracked from its own tier to its claims" requires of every root set
- **AND** stages 2 and 3, derived from upstream data, are not bounded

#### Scenario: Recovery series are tallied with the by-reference read

- **WHEN** a recovery stage returns series
- **THEN** they are counted under the family's name in the build's per-family tally, added to what the by-reference read of the same family contributes

#### Scenario: The tracked set is output-preserving

- **WHEN** the projection retains a path for an `application` root
- **THEN** a pod can be a root only if it resolves a root Application, and every such pod is recovered
- **AND** a path is retained only through a pod hit, whose pod is recovered and whose claims are therefore tracked, or through a claim hit, where the claim is own-annotated (read by tracking-id) or inherits the Application from a mounter that is recovered

#### Scenario: An empty stage yields nothing downstream

- **WHEN** a stage returns no series
- **THEN** nothing downstream of it is issued on its account, and when no controller and no claim carries a root Application, no binding, pod or later query is issued

### Requirement: Roots are always materialised when the upstream knows them

A root the upstream names in the window SHALL appear in the body even when no flow passes through it — a root "exists" when a series the build reads for it names it; a flowless root SHALL be emitted with its ordinary attributes, its compound parent and **no** edges. A root NO series names SHALL NOT be drawn: the body is simply empty of it, with no error and no marker.

#### Scenario: Aggregate with no claims still shows

- **WHEN** a client sends `?aggr=aggr9` and Harvest reports `aggr9` on `ontap-prod-02` but no loaded claim joins it
- **THEN** the body contains `netapp/ontap-prod/aggr/aggr9` (parent `netapp/ontap-prod/ontap-prod-02`, which is also present) and no edge

#### Scenario: Pod with no NetApp-backed claim still shows

- **WHEN** a client sends `?pod=shop/web-0` and that pod mounts no claim that joins the Harvest topology
- **THEN** the body contains the pod node (with its namespace / application / controller groups) and no edge

#### Scenario: Stateless Application still shows

- **WHEN** a client sends `?az=zone-a&env=prod&application=checkout` and the three pods resolving `checkout` mount no claim
- **THEN** the body contains the three pods, each with `data.application="checkout"`, their controller, application, namespace and cluster groups, and no edge

#### Scenario: Qualified aggregate with no claims still shows on its filer only

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=ontap-prod/aggr9`, Harvest reports `aggr9` on `ontap-prod` and on `ontap-lab`, and no loaded claim joins either
- **THEN** the body contains `netapp/ontap-prod/aggr/aggr9` and its owning controller, and not `netapp/ontap-lab/aggr/aggr9`

#### Scenario: Unknown root is not drawn

- **WHEN** a client sends `?aggr=typo` and no Harvest series in the window carries `aggr="typo"`
- **THEN** the server returns 200 with empty `nodes` and `edges` and an empty `clusters` array

#### Scenario: Unknown Application is not drawn

- **WHEN** a client sends `?application=typo` and no loaded pod resolves that Application
- **THEN** the server returns 200 with empty `nodes` and `edges` and an empty `clusters` array

#### Scenario: Non-NetApp claim root still shows

- **WHEN** a client sends `?az=zone-a&env=prod&pvc=shop/cache` and `shop/cache` is bound to a PersistentVolume that joins no Harvest series, mounted by `shop/redis-0`
- **THEN** the body contains the PVC node `shop/cache` (with its namespace / application groups and its `usage` / `storageclass`) and no edge, and does not contain `shop/redis-0`

#### Scenario: Unknown claim root is not drawn

- **WHEN** a client sends `?pvc=shop/typo` or `?pv=pvc-typo` and no `kube_persistentvolumeclaim_info` series in the window names it
- **THEN** the server returns 200 with empty `nodes` and `edges` and an empty `clusters` array

#### Scenario: Controller with no claims still shows

- **WHEN** a client sends `?az=zone-a&env=prod&ontap_node=ontap-prod-03` and Harvest reports `ontap-prod-03` in `node_labels` and `node_new_status` but no loaded claim sits on an aggregate it owns
- **THEN** the body contains `netapp/ontap-prod/ontap-prod-03` with its hardware, performance and health attributes, and no edge

#### Scenario: When a storage-side root exists

- **WHEN** the build reads the series for a storage-side root
- **THEN** an `ontap_cluster` root exists when any Harvest series read for it carries that ONTAP cluster, and its controllers, aggregates and SVMs are then all roots
- **AND** an `aggr` root exists when a `volume_labels` or `aggr_*` series names that aggregate, an `svm` root when a `volume_labels` series names it — for a qualified value, on that ONTAP cluster — and an `ontap_node` root when a `volume_labels`, `node_labels`, `node_new_status` or node performance series names that controller

#### Scenario: When a Kubernetes-side root exists

- **WHEN** the build reads the series for a Kubernetes-side root
- **THEN** a `node` root exists when `kube_node_info` names that Kubernetes node, a `pod` root when `kube_pod_info` names it in the selected estate, and a `pvc` root when a `kube_persistentvolumeclaim_info` series names that `(namespace, claim)` in the selected estate
- **AND** a `pv` root exists when a `kube_persistentvolumeclaim_info` series names it as `volumename`, in which case the claim that series names is the materialised root
- **AND** the reads that make a root with no claim visible are the ones "Every root kind is tracked from its own tier to its claims" defines

#### Scenario: An aggregate root brings its owning controller

- **WHEN** a flowless aggregate root is materialised
- **THEN** the controller currently owning it is materialised too, so that `data.parent` never dangles

#### Scenario: An unmounted root claim keeps its storage-side path

- **WHEN** a root claim of a `pvc` or `pv` root is mounted by no pod and its volume joins the Harvest topology
- **THEN** it is NOT flowless merely because no pod mounts it, and keeps its storage-side path as "Storage-reachability projection" defines

#### Scenario: A root claim that joins no Harvest topology is emitted alone

- **WHEN** a root claim's volume joins no Harvest topology — a non-NetApp claim, or a join miss
- **THEN** the claim is emitted alone, with its ordinary attributes and compound parents and no edges, and its mounting pods are NOT drawn, exactly as a pod root's non-NetApp claims are not

#### Scenario: Every pod resolving a root Application is materialised

- **WHEN** at least one pod loaded in the selected estate resolves a root Application
- **THEN** the `application=` root exists, and **every** such pod is materialised with its ordinary attributes and compound parents and no edges — including pods that mount no claim, which the build loads for exactly this purpose (see "Application roots are tracked through their controllers and claims") — so a stateless Application returns its pods rather than an empty body
- **AND** an Application no loaded pod resolves is not drawn

#### Scenario: A claim carrying a root Application is not materialised on its own

- **WHEN** a claim carries a root Application but lies on no retained path
- **THEN** it is not drawn: such a claim participates in path retention only, and appears only on a retained path

### Requirement: Fixed tier chain and the `storage-flow` edge

The body SHALL express the storage chain as directed edges of ONE type, `storage-flow`, oriented storage → workload, one edge per adjacent pair on the fixed tier chain `netapp-node → netapp-aggr → netapp-svm → pvc → pod → node`. Each `(source, target)` pair SHALL appear at most once regardless of how many claims flow through it.

#### Scenario: One claim draws one path

- **WHEN** claim `shop/orders-data` joins `(ontap-prod, ontap-prod-01, aggr1, svm_shop)` and is mounted by pod `shop/orders-0` scheduled on `worker-1`
- **THEN** the body contains exactly five `storage-flow` edges with tiers `node-aggr` (`netapp/ontap-prod/ontap-prod-01` → `netapp/ontap-prod/aggr/aggr1`), `aggr-svm`, `svm-pvc`, `pvc-pod`, `pod-node` (`<pod> → <cluster>/worker-1`), and no edge of any other type

#### Scenario: Shared upstream hops are emitted once

- **WHEN** two claims on `aggr1` in `svm_shop` are mounted by different pods
- **THEN** the body contains one `node-aggr` edge and one `aggr-svm` edge, and two of each downstream tier

#### Scenario: FlexGroup claim starts at the SVM

- **WHEN** a claim's matched `volume_labels` series carries `svm="svm_big"` and an empty `aggr`
- **THEN** the claim's path begins with an `svm-pvc` edge from `netapp/ontap-prod/svm/svm_big` and no `node-aggr` or `aggr-svm` edge is emitted for it

#### Scenario: Edge id stable across rebuilds

- **WHEN** the same estate is built twice
- **THEN** every `storage-flow` edge carries the same `id` in both bodies

#### Scenario: Every edge names its tier

- **WHEN** a `storage-flow` edge is emitted
- **THEN** it carries `labels.tier` naming its hop — exactly one of `node-aggr`, `aggr-svm`, `svm-pvc`, `pvc-pod`, `pod-node`
- **AND** its `id` is the UUIDv5 of `storage-flow|<source>|<target>` under the server's fixed edge namespace, so the edge is byte-stable across rebuilds

#### Scenario: The chain is derived from the storage join

- **WHEN** the chain is built for a claim
- **THEN** the claim's aggregate and SVM come from its matched `volume_labels` series, its owning controller from that aggregate's `node`, its mounting pods from `kube_pod_spec_volumes_persistentvolumeclaims_info`, and each pod's node from `kube_pod_info`

#### Scenario: A FlexGroup claim enters at the SVM tier

- **WHEN** a claim's match resolved an SVM but no aggregate (the FlexGroup shape)
- **THEN** it enters the chain at the `svm-pvc` tier with no `node-aggr` / `aggr-svm` edge

#### Scenario: An unscheduled pod ends its path at the pod

- **WHEN** a mounting pod is not scheduled on a node
- **THEN** its path ends at the `pvc-pod` tier

#### Scenario: No graph-endpoint edge or node kind appears

- **WHEN** any storage-graph body is inspected
- **THEN** it contains no `pod-mounts-pvc`, `pod-to-node`, `pvc-to-netapp-aggr`, `pod-calls-*` or `service-selects-pod` edge, and no `service` or `external` node

### Requirement: Flow weights on every tier

Every `storage-flow` edge on a path with at least one measured claim SHALL carry `data.metrics` with `read_ops`, `write_ops`, `read_bytes_per_sec` and `write_bytes_per_sec`, each the sum — over every claim whose path passes through that edge — of the claim's own I/O measurement from the storage join (the `pvc-to-netapp-aggr` figures of `/v1/graph`).

#### Scenario: Weights conserve through the chain

- **WHEN** two claims on `aggr1` measure `read_ops` 100 and 250, in SVMs `svm_a` and `svm_b`, each mounted by one pod on distinct nodes
- **THEN** the `node-aggr` edge carries `read_ops: 350`, the two `aggr-svm` edges carry 100 and 250, and each downstream tier carries its claim's figure unchanged

#### Scenario: RWX claim split across its mounters

- **WHEN** one claim measuring `read_ops: 300` is mounted by three pods
- **THEN** each of the three `pvc-pod` edges carries `read_ops: 100` and `labels.attribution="split"`, and the `svm-pvc` edge carries `read_ops: 300` with no `attribution` label

#### Scenario: Latency and ceiling only on the claim-level edge

- **WHEN** a claim resolves `read_latency_us: 450` and `max_iops: 5000`
- **THEN** its `svm-pvc` edge carries both fields and its `node-aggr`, `aggr-svm`, `pvc-pod` and `pod-node` edges carry neither

#### Scenario: Unmounted root claim is a sink

- **WHEN** a client sends `?az=zone-a&env=prod&pvc=shop/orphan-data&pvc=shop/orders-data`, both claims sit on `aggr1` in `svm_shop`, `shop/orphan-data` measures `read_ops: 40` and no pod mounts it, and `shop/orders-data` measures `read_ops: 100` and is mounted by `shop/orders-0`
- **THEN** the `node-aggr` and `aggr-svm` edges carry `read_ops: 140`, the `svm-pvc` edges carry 40 and 100, the `pvc-pod` edge from `shop/orders-data` carries 100, and no edge leaves `shop/orphan-data`

#### Scenario: Unmeasured claim draws a weightless path

- **WHEN** a claim joins the topology but matches no QoS workload series
- **THEN** its path is emitted and none of its edges carries a `metrics` key, unless another measured claim shares an upstream hop — in which case that hop carries only the measured claim's figures

#### Scenario: Latency and ceiling are claim-level only

- **WHEN** a claim's measurement carries latency or a resolved ceiling
- **THEN** its `svm-pvc` edge, being the claim-level edge, additionally carries `read_latency_us`, `write_latency_us` and, when resolved, `max_iops` / `max_bytes_per_sec`, and no other tier carries a latency or ceiling field

#### Scenario: Unmeasured claims contribute nothing

- **WHEN** a claim has no I/O measurement
- **THEN** it contributes nothing to any sum, and an edge whose every contributing claim is unmeasured carries no `metrics` key

#### Scenario: Sums are order-independent on the wire

- **WHEN** an edge's figures are summed
- **THEN** they are accumulated in ascending contribution order and rounded to 6 significant digits at serialisation, so the wire form is order-independent

#### Scenario: A multiply-mounted claim is split equally

- **WHEN** a claim is mounted by `n` pods with `n` greater than one
- **THEN** its weight is **split equally** across the `pvc-pod` edges to its mounting pods, each receiving `1/n` of every summed figure and carrying `labels.attribution="split"`
- **AND** a `pvc-pod` edge from a singly-mounted claim carries no `attribution` label, and a `pod-node` edge sums the (possibly split) `pvc-pod` weights of its pod

#### Scenario: An unmounted root claim is summed upstream

- **WHEN** an **unmounted root claim** (see "Storage-reachability projection") is measured
- **THEN** its whole measurement is summed onto its `svm-pvc` edge and every upstream hop of its path exactly as a mounted claim's is, with no split and no `pvc-pod` edge below it

#### Scenario: Weights conserve tier to tier

- **WHEN** any non-root interior node of the body is inspected
- **THEN** the sum of its incoming `read_ops` equals the sum of its outgoing `read_ops`, likewise for the other three figures, up to rounding

### Requirement: Storage-reachability projection

The body SHALL retain a node iff it lies on a **complete** storage-flow path that touches a root of the request's one root kind ("One root kind per request"), or it is a materialised root or a root's real compound parent. The `/v1/graph` connectivity prune SHALL NOT apply.

#### Scenario: Unmounted claim dropped with its lonely aggregate

- **WHEN** aggregate `aggr7` is joined only by claims no pod mounts and is not a root
- **THEN** neither the claims nor `aggr7` nor (if it owns nothing else retained) its controller appear in the body

#### Scenario: Unmounted root claim keeps its storage-side path

- **WHEN** a client sends `?az=zone-a&env=prod&pvc=shop/orphan-data`, the claim joins `(ontap-prod, ontap-prod-01, aggr1, svm_shop)` and no pod mounts it
- **THEN** the body contains exactly the `node-aggr`, `aggr-svm` and `svm-pvc` edges of `netapp/ontap-prod/ontap-prod-01 → netapp/ontap-prod/aggr/aggr1 → netapp/ontap-prod/svm/svm_shop → <shop/orphan-data>` and no `pvc-pod` or `pod-node` edge

#### Scenario: An unmounted claim that is not a root stays dropped

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1`, and `aggr1` serves a mounted claim and an unmounted claim
- **THEN** the body contains the mounted claim's path only; the unmounted claim is not drawn

#### Scenario: Namespace filter narrows the workload side only

- **WHEN** a client sends `?aggr=aggr1&namespace=shop` and `aggr1` serves claims in `shop` and `platform`
- **THEN** the body contains `aggr1`, its controller, and only the `shop` claims' SVMs, PVCs, pods and nodes

#### Scenario: Namespace filter narrows an application root

- **WHEN** a client sends `?application=checkout&namespace=shop` and `checkout` pods exist in `shop` and `platform`
- **THEN** only the `shop` pods and their paths are in the body, whether drawn on a path or materialised as roots

#### Scenario: What makes a path complete

- **WHEN** the projection evaluates a path
- **THEN** the path is complete when it runs `netapp-node → … → pod`, or `netapp-svm → … → pod` for a FlexGroup claim
- **AND** for a root claim of a `pvc` or `pv` root that no pod mounts, `netapp-node → … → pvc` or `netapp-svm → pvc`, ending at the claim

#### Scenario: What satisfies an Application root

- **WHEN** the root kind is `application=`
- **THEN** a path is retained when its pod or its claim carries the Application, and the pods the root materialises are those whose own `data.application` is a root value

#### Scenario: What satisfies a claim or volume root

- **WHEN** the root kind is `pvc` / `pv`
- **THEN** a path is retained when its claim is a root claim, and every mounting pod's path of that claim is retained

#### Scenario: What the projection drops

- **WHEN** the body is projected
- **THEN** an **unmounted** claim that is not a root claim is dropped, and so is any aggregate, SVM or controller reachable only through unmounted claims
- **AND** a pod none of whose claims joins the Harvest topology is dropped unless it is a materialised root, and a Kubernetes node hosting only dropped pods is dropped

#### Scenario: Narrowing filters never drop a storage root

- **WHEN** a request carries `cluster` / `namespace`
- **THEN** they narrow the claim / pod / node side upstream and are re-applied at projection, and a storage root is never dropped by them

### Requirement: Attributes and compound groups carry over

Every retained real node SHALL carry the same `data` attributes it carries in `/v1/graph`, except `data.containers`, and the body SHALL include the same synthesised compound groups with the same `data.parent` rules and the same group metadata values — `namespace_owner`, `namespace_group`, `namespace_group_owner`, `namespace_alias`, `deployed_by` and `deployed_at` (graph-api "Namespace and application group metadata"). Namespace and ArgoCD Application SHALL NOT be tiers of the flow.

#### Scenario: Pod keeps its attributes and groups

- **WHEN** a retained pod carries `data.application="checkout"` and `data.owner={kind:"StatefulSet", name:"orders"}` in `/v1/graph`
- **THEN** the storage-graph body carries the same attributes on that pod and its `data.parent` names the same controller group, whose ancestry reaches `cluster/<cluster>`

#### Scenario: No namespace or application tier

- **WHEN** any storage-graph body is inspected
- **THEN** no `storage-flow` edge has a `namespace` or `application` group node as source or target

#### Scenario: Pod carries no containers

- **WHEN** a retained pod carries `data.containers` in `/v1/graph`
- **THEN** the storage-graph body's node for that pod has no `containers` field, while its `owner`, `application`, `status` and `data.parent` are unchanged

#### Scenario: PVC keeps its ceiling when its claim-level edge is unweighted

- **WHEN** a retained claim's ceiling resolved to policy group `gold-tier` with `max_iops: 5000`, and its `svm-pvc` edge carries no `metrics` because the claim reported neither flow nor latency in the window
- **THEN** the PVC node carries `data.qos: {"policy_group": "gold-tier", "max_iops": 5000}` and the `svm-pvc` edge has no `metrics` key

#### Scenario: FlexGroup PVC carries its ceiling in the storage graph

- **WHEN** a retained claim enters the chain at `netapp/ontap-prod/svm/svm_big` (no aggregate) and its ceiling resolved
- **THEN** the PVC node carries `data.qos` with the resolved fields, exactly as it does in `/v1/graph`

#### Scenario: Group metadata matches the graph endpoint

- **WHEN** `/v1/graph` and `/v1/storage-graph` for the same window and zone both render namespace group `us-dev-c1/namespace/shop` and application group `us-dev-c1/namespace/shop/application/checkout`
- **THEN** both bodies carry the same `data.namespace_owner`, `data.namespace_group`, `data.namespace_group_owner` and `data.namespace_alias` on the namespace group and the same `data.deployed_by` and `data.deployed_at` on the application group

#### Scenario: The carried attributes and groups

- **WHEN** a retained real node or its groups are serialised
- **THEN** the node carries its `/v1/graph` attributes, including `ipaddress`, `owner`, `application`, `ready_status`, `health`, `usage`, `storageclass`, `qos`, `hardware`, `perf`, `alerts` and `status`
- **AND** the compound groups nest as `cluster > namespace > application > controller > pod`, `cluster > namespace > [application >] pvc`, `cluster > node`, `storage-cluster > netapp-node > netapp-aggr` and `storage-cluster > netapp-svm`

#### Scenario: Namespace and Application views are derived by the consumer

- **WHEN** a consumer needs a namespace- or Application-level Sankey
- **THEN** it derives one by walking `data.parent` and summing the conserved weights, since neither is a tier of the flow

#### Scenario: A PVC's ceiling is independent of its flow

- **WHEN** a claim's ceiling resolved
- **THEN** the PVC carries its `qos` attribute, including when the claim's `svm-pvc` edge carries no `metrics` (no flow and no latency to report) and when the claim enters the chain at the SVM (the FlexGroup shape)
- **AND** whenever the `svm-pvc` edge carries `max_iops` / `max_bytes_per_sec`, the PVC's `data.qos` carries the same values

#### Scenario: Containers are the one attribute that differs

- **WHEN** a storage-graph pod is serialised
- **THEN** it does NOT carry `data.containers`, because the storage build does not read the container family (see "Storage build reads every family by reference") and a storage-flow consumer has no use for a container list
- **AND** this is the ONE attribute on which the two bodies differ for the same pod

### Requirement: Each claim names its aggregate

A PVC node in the body SHALL carry the same `labels.aggr` it carries in `/v1/graph` (see "PVC `aggr` label" in `graph-api`): the node id of the aggregate its volume sits on, absent for a FlexGroup claim. Whenever a PVC carrying `labels.aggr` is in the body, the node it names SHALL be in the same body, and no `storage-flow` edge SHALL carry a label naming a claim's aggregate.

#### Scenario: An SVM spanning two aggregates

- **WHEN** SVM `svm_shop` on ONTAP cluster `ontap-prod` holds claim `shop/orders-data` on `aggr1` and claim `shop/catalog-data` on `aggr2`, both mounted, and a client sends `?az=zone-a&env=prod&svm=svm_shop`
- **THEN** the body carries an `aggr-svm` edge from each aggregate to `svm_shop` and one `svm-pvc` edge to each PVC; the `shop/orders-data` PVC carries `labels.aggr="netapp/ontap-prod/aggr/aggr1"`, the `shop/catalog-data` PVC carries `labels.aggr="netapp/ontap-prod/aggr/aggr2"`, and both aggregate nodes are in the body

#### Scenario: A storage root keeps each claim's own aggregate

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1` and `svm_shop` also holds mounted claims on `aggr2`
- **THEN** every PVC in the body carries `labels.aggr="netapp/ontap-prod/aggr/aggr1"`, and none of `svm_shop`'s `aggr2` claims is retained

#### Scenario: A FlexGroup claim names no aggregate

- **WHEN** a retained claim's volume is a FlexGroup, so its path starts at its `svm-pvc` edge
- **THEN** its PVC carries `labels.svm` and no `labels.aggr`, and no `node-aggr` or `aggr-svm` edge is emitted for it

#### Scenario: No storage-flow edge names a claim's aggregate

- **WHEN** any storage-graph body is inspected
- **THEN** every `storage-flow` edge's `labels` holds only `tier` and, on a split `pvc-pod` edge, `attribution`

#### Scenario: The label is the one statement of a claim's aggregate

- **WHEN** an SVM spans more than one aggregate
- **THEN** walking up from a PVC through its SVM cannot tell which aggregate the claim sits on, because the `aggr-svm` tier is shared by every claim the SVM holds on that aggregate, so `labels.aggr` is the one place the body states a claim's aggregate

#### Scenario: The named aggregate lies on the claim's own path

- **WHEN** a PVC carrying `labels.aggr` is in the body
- **THEN** the aggregate it names lies on the claim's path (`netapp-node → netapp-aggr → netapp-svm → pvc`), which the storage-reachability projection retains whole, and has an `aggr-svm` edge to the PVC's SVM
- **AND** the tier chain and the edge labels (`tier`, `attribution`) are unchanged

### Requirement: Deterministic storage-graph body

The body SHALL be byte-identical for identical `(window, az, env, roots, cluster, namespace)` against identical upstream state, `roots` covering every root selector, `application` included. Root selector values given in a different order or repeated SHALL produce an identical body and SHALL issue identical upstream queries.

#### Scenario: Root order does not matter

- **WHEN** a client sends `?aggr=b&aggr=a` and then `?aggr=a&aggr=b&aggr=a`
- **THEN** both bodies are byte-identical

#### Scenario: Application root order does not matter

- **WHEN** a client sends `?application=b&application=a` and then `?application=a&application=b&application=a`
- **THEN** both requests issue identical recovery queries and return byte-identical bodies

#### Scenario: The body is sorted and carries no build-time field

- **WHEN** a storage-graph body is serialised
- **THEN** nodes and edges are sorted as in `/v1/graph`, `clusters` is sorted, weights are order-independent, and the body carries no time-of-build or echo-of-input field

### Requirement: In-process storage-graph engine surface

The reusable graph engine SHALL expose the storage-graph build in-process with the same contract as the HTTP endpoint: a request parser accepting the endpoint's query parameters and returning the same validation failures (same `reason` codes), and a single call producing the identical Cytoscape body from those parameters. The HTTP handler SHALL use that same parser.

#### Scenario: Embedder and server agree

- **WHEN** the same query parameters are given to the in-process call and to `GET /v1/storage-graph` against the same upstream state
- **THEN** the two bodies are byte-identical, and an invalid parameter set fails both with the same `reason`

#### Scenario: An embedder needs no HTTP hop

- **WHEN** an embedding module calls the in-process storage-graph build
- **THEN** it obtains byte-for-byte the `/v1/storage-graph` body without an HTTP hop, and because the HTTP handler parses with the same parser, the request contract cannot drift between server and embedder

### Requirement: Storage build fails closed on upstream query errors

A `/v1/storage-graph` build SHALL fail when ANY upstream query it issues returns an error — whatever the family, and whichever chunk, phase, stage or backend of that family failed — with the single exception of `ALERTS`, and the endpoint SHALL NOT return a body with a silently missing family. `GET /v1/graph` SHALL keep every existing error class unchanged.

#### Scenario: A volume-label failure fails the storage request

- **WHEN** a client sends `GET /v1/storage-graph?...&az=zone-a&env=prod&aggr=aggr00` and the `volume_labels` query fails with an upstream error
- **THEN** the server returns 502 with `reason: "upstream"` and `message: "upstream query failed: volume_labels"`, and returns no graph body

#### Scenario: A QoS chunk failure fails the storage request

- **WHEN** the scoped QoS read is split into three chunks and the second `qos_read_ops` chunk fails
- **THEN** the server returns 502 with `reason: "upstream"` naming `qos_read_ops`

#### Scenario: A kubelet failure fails the storage request

- **WHEN** the `kubelet_volume_stats_used_bytes` query fails with an upstream error
- **THEN** the server returns 502 with `reason: "upstream"` naming `kubelet_volume_stats_used_bytes`

#### Scenario: One backend failing a Harvest query fails the storage request

- **WHEN** two backends serve `harvest` for the request's zone and one of them fails the `aggr_space_used` query
- **THEN** the server returns 502 with `reason: "upstream"` naming `aggr_space_used`, and the message names no backend URL or host

#### Scenario: An ALERTS failure does not fail the storage request

- **WHEN** the `ALERTS` query fails with an upstream error and every other query succeeds
- **THEN** the server returns 200, no node carries an alert, every node's `status` is folded from its remaining signals, and the failure is counted on `kube_state_graph_upstream_query_failures_total{query="ALERTS"}`

#### Scenario: An empty family is not a failure

- **WHEN** `kube_job_annotations` returns an empty vector because the deployment does not allowlist Job annotations
- **THEN** the server returns 200 and the Job-owned pods resolve their Application through the CronJob hop exactly as on `/v1/graph`

#### Scenario: A zone with no backend is not a failure

- **WHEN** a client sends `?az=zone-c&env=prod&aggr=aggr1` and no backend serving `harvest` declares `zone-c`
- **THEN** the Harvest legs return empty vectors, the existing warning is logged, and the server returns 200

#### Scenario: The graph endpoint keeps degrading

- **WHEN** a client sends `GET /v1/graph` and the `volume_labels` query fails with an upstream error
- **THEN** the server returns 200, with its error classes unchanged and no `pvc-to-netapp-aggr` edge

#### Scenario: An application-metadata failure fails the storage request

- **WHEN** the metric is configured and a chunk of the namespace-restricted application-metadata read fails
- **THEN** the server returns 502 with `reason: "upstream"` and `message: "upstream query failed: app_metadata"`, naming neither the configured metric nor a backend URL

#### Scenario: Families that degrade on the graph endpoint fail closed here

- **WHEN** a storage build's query of a family `/v1/graph` reads as OPTIONAL or degrading fails — `volume_labels` (every read), the six `qos_*` workload families (every chunk), the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency`, `node_total_data`, `kubelet_volume_stats_used_bytes`, `kubelet_volume_stats_capacity_bytes`, `kube_replicaset_annotations` and `kube_job_annotations` (by-reference read and application recovery alike), or `app_metadata` (the namespace-restricted read and the application seed alike)
- **THEN** the build fails, because a storage body with a silently missing family is indistinguishable from a smaller estate — a filer with no flow, an aggregate with no I/O
- **AND** the per-family log-and-continue, per-chunk degrade and degraded-family hop-suppression rules of the `netapp-storage-graph` and `cluster-topology-source` capabilities apply to `/v1/graph` builds only

#### Scenario: The failure is mapped as a required-leg failure

- **WHEN** a storage build fails on an upstream query error
- **THEN** the server returns HTTP 502 with `reason: "upstream"`, exactly as for a required-leg failure, and a `message` naming the bare family whose query failed (`upstream query failed: <family>`) that carries no upstream URL, host, address or credential
- **AND** the failed query is still logged server-side with its full error and counted on `kube_state_graph_upstream_query_failures_total{query}`

#### Scenario: Timeout and cancellation keep their mappings

- **WHEN** a storage build exceeds `--build-timeout`, or the caller cancels it
- **THEN** the request still maps to 504 `timeout`, or to the existing `canceled` mapping, respectively

#### Scenario: The in-process surface returns the typed upstream error

- **WHEN** a storage build run through the in-process storage-graph engine surface fails on an upstream query error
- **THEN** it returns the same typed upstream error, carrying the family name

#### Scenario: What is not a failure

- **WHEN** a query returns an empty vector (a family the deployment does not export, an annotation family that is not allowlisted, a scope that matched nothing), a requested zone is declared by no backend serving the family (the existing empty-vector-and-warning rule), or the build does not issue a query
- **THEN** the build does not fail, because no query failed

#### Scenario: ALERTS stays optional

- **WHEN** an `ALERTS` query fails
- **THEN** it is logged and counted, the build continues with no alert attached to any node, and every node's `status` is folded from its remaining signals

### Requirement: Storage-graph end-time alignment

`GET /v1/storage-graph` SHALL apply the same end-time alignment as `GET /v1/graph` ("Time-window passthrough" in the `graph-api` capability): with a non-zero `--end-align` grid, `end` is floored to the grid and `start` shifted by the same amount before any upstream query is rendered, and the alignment SHALL happen after `start` / `end` validation, so validation errors are unchanged.

#### Scenario: Storage request aligned like a graph request

- **WHEN** `--end-align=30s` and a client sends `GET /v1/storage-graph?start=2026-05-01T12:00:10Z&end=2026-05-01T12:05:10Z&az=zone-a&env=prod`
- **THEN** every upstream query is evaluated at `2026-05-01T12:05:00Z` over a 5m window

### Requirement: Storage build reads the application-metadata metric by namespace

When the `app-metadata-source` metric is configured and served, every storage build SHALL read it after pods and claims are loaded, once per loaded cluster: restricted to that cluster and, on the configured `namespace` label, to the namespaces its loaded pods and claims carry, composed with the request matchers. A namespace SHALL NOT be read in a cluster that did not load it.

#### Scenario: Namespaces of pods and claims are read whole

- **WHEN** a storage build loads pods in namespace `shop` and an unmounted root claim in namespace `archive`
- **THEN** one application-metadata read carries that cluster's `cluster` matcher and the configured namespace label restricted to `archive|shop`, and returns every row of those namespaces in that cluster, including rows of pods the build did not load

#### Scenario: A namespace is read only in the cluster that loaded it

- **WHEN** a storage build loads pods in namespace `shop` of cluster `c1` and namespace `pay` of cluster `c2`, and cluster `c2` also has a namespace `shop`
- **THEN** one read carries `cluster="c1"` restricted to `shop`, another carries `cluster="c2"` restricted to `pay`, and no row of `c2`'s `shop` namespace is read

#### Scenario: Pod Application and group metadata come from the read

- **WHEN** the read returns a row naming Application `shop-v2`, owner `team-a` and user `alice` for loaded pod `shop/web-0`
- **THEN** `web-0` carries `data.application="shop-v2"`, the `shop` namespace group carries `data.namespace_owner="team-a"` and the `shop-v2` application group carries `data.deployed_by="alice"`

#### Scenario: No loaded namespace issues no read

- **WHEN** a storage build loads no pod and no claim
- **THEN** no application-metadata query is issued

#### Scenario: Unset or unserved metric issues no read

- **WHEN** the metric is unset, or no backend serves `appmeta`
- **THEN** no application-metadata query is issued and every pod resolves its Application through the controller chain

#### Scenario: The namespace restriction is data-derived

- **WHEN** the loaded namespaces exceed what one query can carry
- **THEN** the restriction is chunked, never widened, and never rejected
