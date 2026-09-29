# storage-graph-api Specification

## Purpose

Serves a storage-rooted flow graph — NetApp controller → aggregate → SVM → PVC → pod → Kubernetes node — with an I/O weight on every hop, so a Sankey diagram can answer "what runs on this filer?" and "which filer does this pod use?" from one endpoint.

## Requirements

### Requirement: Storage-flow graph endpoint

The server SHALL expose `GET /v1/storage-graph` returning a storage-flow graph for a caller-specified `[start, end]` window, in the same `{ apiVersion, clusters, elements: { nodes, edges } }` Cytoscape.js shape as `GET /v1/graph`. `start` and `end` SHALL be required and validated exactly as for `/v1/graph` (RFC 3339 or Unix seconds; `end > start`; `missing_start` / `missing_end` / `invalid_start` / `invalid_end` / `invalid_range`). The endpoint SHALL sit behind the same API-key authentication, the same per-build timeout (`--build-timeout` → 504 `timeout`), and the same upstream / outside-retention / cancelled error mapping as `/v1/graph`, and SHALL be described in the served OpenAPI document.

`az` and `env` SHALL be **required** and **repeatable**: a request carrying no non-empty value of either SHALL be rejected 400 with `reason: "missing_az"` / `reason: "missing_env"`; a request MAY carry several values of each. The selected zones are the **Cartesian product** of the `az` and `env` values; a combination the estate does not hold contributes nothing. Exactly one root kind SHALL be required, as "One root kind per request" defines: a request carrying none SHALL be rejected 400 with `reason: "missing_root"`, and a request carrying two or more root kinds SHALL be rejected 400 with `reason: "invalid_scope"`. The window, `az` and `env` SHALL be validated before the roots, so a request failing several checks reports the first of them in that order. The `az` / `env` values SHALL be pushed upstream exactly as the `/v1/graph` selector-level `az` / `env` dimensions are — sorted and de-duplicated, a single value rendered as an equality and several as one anchored alternation, as matchers on every kube-state-metrics, kubelet, Harvest and `ALERTS` family, and backend selection by the `az` SET for every zone-routed family.

**The body is the union of its zones.** For a fixed root, cluster / namespace filter and window, the body of a request selecting several `(az, env)` combinations SHALL equal the union of the bodies of the single-combination requests: no node SHALL merge two objects of different zones or environments, no flow weight SHALL sum claims of different zones or environments onto one edge unless those claims genuinely share that storage entity, and every alert SHALL match zone-agreeingly as the `alert-overlay` capability defines. Kubernetes ids are composed from the cluster identity `<az>-<env>-<cluster>`, and NetApp ids are qualified by the ONTAP cluster, which the operator guarantees unique across the estate (together with ONTAP controller names); a claim joins only FlexVols of its own zone ("PVC-to-NetApp-aggregate edge join", `netapp-storage-graph`). Wherever this capability matches an upstream row to a pod or claim the build already read by its `cluster`, the key SHALL be the cluster identity — the `(az, env, cluster)` triple under the configured label keys — never the raw `cluster` label alone, so a cluster name reused in two selected zones is two clusters. `cluster` and `namespace` SHALL be accepted as optional, repeatable narrowing filters with `/v1/graph` semantics. `prune` SHALL be ignored (this endpoint applies its own reachability projection, never the connectivity prune). `edge_type` — withdrawn from `/v1/graph` as well, so no longer a parameter of any endpoint — and any other unknown parameter SHALL be ignored without error, whatever value they carry.

The top-level `clusters` array SHALL list the Kubernetes cluster identities present on emitted `pod` / `node` / `pvc` nodes and never an ONTAP cluster name.

#### Scenario: Successful request

- **WHEN** a client sends `GET /v1/storage-graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z&az=zone-a&env=prod&aggr=aggr1`
- **THEN** the server returns 200 with a body containing exactly `apiVersion: "v1"`, `clusters`, and `elements` with `nodes` and `edges`

#### Scenario: Missing az

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&env=prod`
- **THEN** the server returns 400 with `reason: "missing_az"`

#### Scenario: Repeated env

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&env=prod&env=dev&aggr=aggr1`
- **THEN** the server returns 200, and every kube-state-metrics, kubelet, Harvest and `ALERTS` query carries `<az-key>="zone-a",<env-key>=~"dev|prod"`

#### Scenario: Several zones are a Cartesian product

- **WHEN** a client sends `?az=zone-b&az=zone-a&env=prod&aggr=aggr1`
- **THEN** every zone-routed family is dispatched to the backends serving `zone-a` or `zone-b` (and catch-alls), every such query carries `<az-key>=~"zone-a|zone-b",<env-key>="prod"`, and the query strings are identical to those of `?az=zone-a&az=zone-b&env=prod&aggr=aggr1`

#### Scenario: A multi-zone body is the union of its zones

- **WHEN** filers `ontap-a` (zone-a) and `ontap-b` (zone-b) each hold an aggregate `aggr1` with mounted claims, cluster `c1` exists in both zones with a pod `shop/orders-0`, and a client sends `?az=zone-a&az=zone-b&env=prod&aggr=aggr1`
- **THEN** the body's node and edge sets equal the union of the bodies of `?az=zone-a&env=prod&aggr=aggr1` and `?az=zone-b&env=prod&aggr=aggr1`: `netapp/ontap-a/aggr/aggr1` and `netapp/ontap-b/aggr/aggr1` are distinct nodes, the two `shop/orders-0` pods are distinct nodes under `zone-a-prod-c1` and `zone-b-prod-c1`, each edge carries the same weight it carries in its single-zone body, and `clusters` lists both identities

#### Scenario: A cluster name reused across selected zones is two clusters

- **WHEN** a client sends `?az=zone-a&az=zone-b&env=prod&node=worker-1`, cluster `c1` exists in both zones, `shop/db-0` runs on `worker-1` in `zone-a`, and a same-named `shop/db-0` in `zone-b` was recreated on `worker-2`
- **THEN** only the `zone-a` pod is placed on `worker-1` and tracked from it; the `zone-b` pod's newest incarnation does not displace or hide it

#### Scenario: Zone and environment reach upstream

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr1`
- **THEN** every kube-state-metrics, kubelet, Harvest and `ALERTS` query carries `<az-key>="zone-a",<env-key>="prod"`, every Harvest query is issued only to the `harvest` backends whose `zones` include `zone-a` (or catch-alls), and no series from another zone or environment contributes to the body

#### Scenario: Unauthenticated request rejected when keys configured

- **WHEN** API keys are configured and a client sends `GET /v1/storage-graph` without `X-API-Key`
- **THEN** the server returns 401 exactly as `/v1/graph` would

#### Scenario: Graph-only and withdrawn parameters are ignored

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&env=prod&aggr=aggr1&prune=false&edge_type=not-a-type`
- **THEN** the server returns 200 with a body byte-identical to the same request without `prune` and `edge_type` — neither value is validated, and neither narrows or widens the body

#### Scenario: Missing root

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&env=prod`
- **THEN** the server returns 400 with `reason: "missing_root"`

#### Scenario: Zone is checked before the root

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&env=prod` with no root
- **THEN** the server returns 400 with `reason: "missing_az"`

### Requirement: One root kind per request

The endpoint SHALL accept the following root kinds, each a repeatable parameter whose values are validated like a `/v1/graph` selector value (≤ 253 bytes, valid UTF-8, no control characters; otherwise 400 `invalid_scope`):

- `ontap_cluster=<name>` — an ONTAP cluster: every controller, aggregate and SVM in it is a root;
- `ontap_node=<name>` — an ONTAP controller. A path is retained by it when the path's aggregate is owned by that controller — the owner the `netapp-storage-graph` capability's owning-controller vote resolves — so the root reaches the aggregates the controller owns, the SVMs and claims on those aggregates, and the pods, Kubernetes nodes, Applications and namespaces below them. It never reaches a claim on an aggregate another controller owns, even when both aggregates serve one SVM;
- `aggr=<name>` or `aggr=<ontap_cluster>/<name>` — an ONTAP aggregate. The bare form is matched on EVERY ONTAP cluster the selected zones' Harvest backends return, so a name present on two filers roots both aggregates; the qualified form roots exactly the aggregate of that name on that ONTAP cluster;
- `svm=<name>` or `svm=<ontap_cluster>/<name>` — an SVM, the bare form matched on every ONTAP cluster likewise and the qualified form on the named one only;
- `node=<name>` — a Kubernetes node, matched in every Kubernetes cluster of the selected estate. It SHALL NOT match an ONTAP controller;
- `pod=<namespace>/<pod-name>` — one pod. A value without exactly one `/` separating two non-empty segments SHALL be rejected 400 `invalid_scope`;
- `pvc=<namespace>/<claim-name>` — one PersistentVolumeClaim, matched in every Kubernetes cluster of the selected estate. A value without exactly one `/` separating two non-empty segments SHALL be rejected 400 `invalid_scope`;
- `pv=<name>` — a PersistentVolume, by its bare (cluster-scoped) name, matched in every Kubernetes cluster of the selected estate. It roots the claim(s) whose `kube_persistentvolumeclaim_info` series names it as `volumename`; a PersistentVolume bound to no claim roots nothing;
- `application=<name>` — an ArgoCD Application, in the form `data.application` carries it (the segment of the tracking-id before the first `:`). A path is retained by it when its **pod or its claim** carries that Application — the claim's own annotation or the Application it inherited from a mounting pod, exactly as the body reports it. A value containing `:` can name no Application and matches nothing.

Empty values SHALL be dropped, so a bare `?aggr=` is a no-op and does not count as a root. A request SHALL carry exactly ONE root kind with at least one non-empty value. A request carrying none SHALL be rejected 400 with `reason: "missing_root"`. A request carrying two or more root kinds SHALL be rejected 400 with `reason: "invalid_scope"` and a message naming the parameters. The values of the one kind SHALL be OR-combined: a path is retained when it touches any of them. Root names are matched exactly and case-sensitively. An `aggr` or `svm` value containing `/` SHALL be the qualified form: it SHALL split on exactly one `/` into two non-empty segments (the ONTAP cluster, then the name), and any other value containing `/` SHALL be rejected 400 `invalid_scope`. Bare and qualified values MAY be mixed within the one root kind and are OR-combined; a qualified value whose name also appears bare adds nothing. `cluster` and `namespace` are narrowing filters, not root kinds, and SHALL combine with any root kind.

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

### Requirement: Every root kind is tracked from its own tier to its claims

The storage build SHALL reach the claims a request can retain by walking from its root, never by reading a family across the requested zone and discarding what the root does not reach. For each root kind it SHALL issue the reads below, each restricted as "Storage build reads every family by reference" defines; the claims those reads name form the build's **tracked claim set**.

**Storage-side kinds.** `ontap_cluster` reads `volume_labels` restricted on `cluster` to the root values; `aggr` restricted on `aggr` to its bare values; `svm` restricted on `svm` to its bare values. The qualified values of `aggr` (resp. `svm`) SHALL be read one query per ONTAP cluster, carrying that cluster as an equality and its names as the alternation on `aggr` (resp. `svm`), so an aggregate or SVM of that name on another filer is never read for the root; an aggregate read this way is read whole, exactly as a bare `aggr` root's is. `ontap_node` reads `volume_labels` restricted on `node` to the root values, then reads WHOLE every `(ONTAP cluster, aggregate)` pair those rows name. Only the aggregates whose owning controller the vote resolves to a root value contribute claims: the vote picks one of the aggregate's own `node` values, so every aggregate a root controller owns has a row naming it and is found by the first read. For each of the four kinds, the build SHALL derive **candidate PersistentVolume names** from the `volume` label of every contributing row: for every position at which the value continues with `pvc_` and which is the start of the value or follows a `_`, the remainder of the value from that position with every `_` replaced by `-` is one candidate; candidates are sorted and de-duplicated. It SHALL then read `kube_persistentvolumeclaim_info` restricted on `volumename` to the candidates, and the claims that read returns are the tracked set. Candidate extraction is a **generator, never a judge**: a candidate naming no PersistentVolume loads nothing, and whether a loaded claim lands on a FlexVol — and on which aggregate, SVM and controller — SHALL be decided solely by the forward derivation and join of the `netapp-storage-graph` capability, exactly as on `/v1/graph`. The rewrite rules SHALL NOT change the extraction, so a custom rule set can make a storage root find fewer claims, never a wrong one.

**Kubernetes node.** `node` reads `kube_pod_info` restricted on `node` to the root values, then reads `kube_pod_info` again restricted to the `(namespace, pod)` pairs the first read returned, WITHOUT the node restriction (**incarnation completion**): a pod's node is its newest incarnation's, so a pod name recreated on another node inside the window SHALL be placed where an unrestricted read places it. It then reads the claim-binding family restricted to the `(namespace, pod)` pairs of the pods whose newest incarnation runs on a root node, keeping only rows whose `(cluster, namespace, pod)` names such a pod. The claims those rows bind are the tracked set. Both reads are keyed by `(namespace, pod)` as "Storage build reads every family by reference" requires of every read of known pods.

**Pod.** `pod` reads the claim-binding family restricted on `namespace` and `pod` to the roots, keeping only rows whose `(namespace, pod)` is a root ref, and reads `kube_pod_info` restricted to the root names. The claims those rows bind are the tracked set.

**Claim.** `pvc` reads `kube_persistentvolumeclaim_info` restricted to the root `(namespace, claim)` pairs — one query per namespace, carrying that namespace as an equality and its claim names as the alternation on `persistentvolumeclaim` — keeping only rows whose `(namespace, claim)` is a root ref. The claims those rows name, in every Kubernetes cluster of the selected estate, are the tracked set.

**Volume.** `pv` reads `kube_persistentvolumeclaim_info` restricted on `volumename` to the root values. The claims that read returns are the tracked set. This is the storage-side kinds' claim read without the candidate derivation: no `volume_labels` read precedes it, and a root value is used verbatim, never rewritten.

For both kinds the seed read IS the claim-side `kube_persistentvolumeclaim_info` read of "The tracked claims expand to both ends of the chain", which is therefore not issued again; the expansion's other claim-side reads, mounter completion, candidate completion, owner completion and storage-side reads apply unchanged. Because candidate completion reads `volume_labels` by the derived token of every tracked claim, a claim root's aggregate, SVM and controller are found exactly as on `/v1/graph`.

**Application.** `application` runs the recovery of "Application roots are tracked through their controllers and claims" and tracks the claims that requirement names.

**Closure.** The tracked set SHALL contain every claim the projection can retain for the request. A claim is retained through a storage-side root only if its picked aggregate, SVM or controller is a root, which requires at least one of its candidates on a rooted component, so the rooted rows name it. It is retained through a `node` or `pod` root only if one of its mounting pods is or runs on a root, so that pod's bindings name it. It is retained through a `pvc` or `pv` root only if it is itself a root claim, which the seed read names.

**Roots with no claim.** For the root alone, the build SHALL also read the families that materialise a root no claim reaches, as "Roots are always materialised when the upstream knows them" requires:

- the aggregate gauge families for `aggr` (by aggregate name for a bare value, by `(ONTAP cluster, aggregate)` pair for a qualified one) and for `ontap_cluster` (by ONTAP cluster);
- the controller families for `ontap_node` (by controller name) and for `ontap_cluster` (by ONTAP cluster);
- the four Kubernetes-node families for `node`;
- `kube_pod_info` for `pod`;
- nothing further for `pvc` and `pv`: the seed read is the read that names a root claim.

**Bounded root sets.** Root parameters are repeatable and the parser bounds each value's length, never their count, so a root set is a scope a client can inflate. When a root kind's first read would take more than a fixed maximum number of queries under the shared byte budget, the request SHALL be rejected 400 with `reason: "invalid_scope"` before any upstream query is issued. The first read's shape is a pure function of the root values and the byte budget, so no upstream data is needed to decide. The build SHALL NOT fall back to reading the family across the zone.

**Static PersistentVolumes.** A claim bound to a PersistentVolume whose name yields no candidate — a statically provisioned PV, or a provisioner configured with a non-`pvc` volume-name prefix — SHALL NOT be found from a storage-side root, even when the forward join would match it. `/v1/graph`, and a `node`, `pod`, `application`, `pvc` or `pv` root reaching the same claim, SHALL still join it — a `pvc` / `pv` root in particular is the way to root at a claim bound to a statically provisioned volume.

**Coverage signal.** When a storage-side root's contributing rows carry a `volume` and none produced a candidate, the build SHALL log one aggregated warning `storage_root_claim_miss` with `reason="no_pv_candidate"` and the volume count. When candidates were produced and the claim-info read returned no claim, it SHALL log `storage_root_claim_miss` with `reason="no_claim"` and the candidate count. `no_claim` SHALL be logged at Debug instead when the request carries a `cluster` or `namespace` filter, which can legitimately exclude every candidate. Neither changes the response status.

**Zone.** Every query the build issues SHALL carry the request-scoped matchers its family accepts and SHALL be dispatched only to the backends the request's `az` values select. A claim on a rooted filer that lives in a zone or environment the request did not select is therefore not loaded and SHALL NOT be drawn; a claim of a selected zone joins only FlexVols of its own zone.

#### Scenario: Rooted volumes name their claims

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00`, the rooted read returns FlexVol `trident_pvc_ab12_cd34`, and claim `shop/orders-data` is bound to PV `pvc-ab12-cd34` and mounted by `orders-0` and `orders-1`
- **THEN** `kube_persistentvolumeclaim_info` is issued restricted to `volumename=~"pvc-ab12-cd34"`, the claim-side families are restricted to `persistentvolumeclaim=~"orders-data"`, the pod read is restricted to `{orders-0, orders-1}`, and the body carries the claim's complete path

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

- **WHEN** the build tracks claim `shop/data` and the claim-binding read, restricted to `persistentvolumeclaim=~"data"`, also returns a binding of `platform/data`
- **THEN** the `platform/data` binding is discarded before the pod scope is computed, its pod is not read, and no `platform/data` PVC node is built

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
- **THEN** the phase-1 `volume_labels` reads are `{aggr="aggr9"}`, `{cluster="ontap-lab",aggr="aggr1"}` and `{cluster="ontap-prod",aggr=~"aggr1|aggr2"}` beside the request matchers, and no query reads `aggr1` of any other filer for the root

#### Scenario: A storage root stays in the request's zone

- **WHEN** two backends serve `ksm` with `zones: [zone-a]` and `zones: [zone-b]`, two backends serve `harvest` likewise, filer `ontap-prod` holds FlexVols whose claims live in a `zone-a` and a `zone-b` cluster, and a client sends `?az=zone-a&env=prod&ontap_cluster=ontap-prod`
- **THEN** no query is issued to either `zone-b` backend, every kube-state-metrics, kubelet and `ALERTS` query carries `<az-key>="zone-a",<env-key>="prod"`, the body contains the `zone-a` claim's complete path and not the `zone-b` claim, and `clusters` lists `zone-a` identities only

### Requirement: The tracked claims expand to both ends of the chain

From the tracked claim set, the build SHALL read everything the body draws for those claims, restricted by reference, with four **completions** that keep each reader's whole-population rules intact:

- **Claim side.** `kube_persistentvolumeclaim_info` (when the root's reads did not already return the claim), `kube_persistentvolumeclaim_annotations`, the two kubelet volume-stats families and the claim-binding family, each restricted on `persistentvolumeclaim` to the tracked claim names. Rows SHALL be kept only when their `(cluster, namespace, claim)` names a tracked claim.
- **Mounter completion.** The claim-binding read by claim names EVERY pod mounting a tracked claim, and every such pod SHALL be loaded. A shared claim's `pvc-pod` weight is split over the mounters present in the built graph, and an unannotated claim inherits the Application of its mounters, so a mounter left unloaded would change both.
- **Workload side.** The mounters' pods, their Kubernetes nodes and their controllers, as "Storage build reads every family by reference" defines.
- **Candidate completion.** `volume_labels` restricted on `volume` to the derived tokens of every tracked claim, rendered as the suffix comparison `.*<token>`, so each claim's aggregate and SVM picks run over its WHOLE candidate set — a clone or a same-named FlexVol on another filer included. Its rows SHALL NOT add claims to the tracked set.
- **Owner completion.** Every `(ONTAP cluster, aggregate)` pair any volume-label row of the build names SHALL be read whole, except the aggregates a read of the build already covered whole, so the owning-controller vote runs over every row of every aggregate the body can draw. Under an `svm` root this SHALL include the aggregates candidate completion alone named: the aggregate and SVM picks are separate, so a claim retained through its SVM can land on such an aggregate. Its rows SHALL NOT add claims.
- **Storage side.** Then the aggregate and controller gauge families, the QoS workload families and the fixed-policy families, as "Storage build reads every family by reference" defines.

Every volume-label read of the build SHALL be merged, de-duplicated by label set, before the parse, and every downstream consumer — the aggregate, SVM and owner picks, the inventory and the QoS `volume` scope — SHALL run over the merged result.

The body SHALL be **byte-identical** to the body an unrestricted read of the same zone produces under the same projection, for every root kind. The one permitted divergence is an estate in which an aggregate or controller is named by the volume-label family alone, with no gauge series of its own, outside what the build reached: such an entity is not materialised, which can change whether an alert without a `cluster` label matches a unique entity. The stock Harvest templates name every one.

A scope derived from upstream data — tracked claims, mounters, touched aggregates, controllers, tokens — SHALL be chunked under the shared byte budget, issued with bounded concurrency and merged in chunk order however large it is. It SHALL NEVER be replaced by a read across the zone. An empty tracked set SHALL issue no claim-side, workload-side or storage-side query beyond the root's own reads.

The join-coverage signal of the `netapp-storage-graph` capability SHALL count over the tracked claims exactly as `/v1/graph` counts over its loaded claims.

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
- **THEN** the claim-binding read by claim loads all three mounters, the body retains only the `orders-0` path, its `pvc-pod` edge carries one third of the claim's figures with `labels.attribution="split"`, and the body is byte-identical to the unrestricted body

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

### Requirement: Storage build reads every family by reference

The storage build SHALL NOT issue `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels` or `kube_service_annotations`: the body carries no `service` or `external` node, no `service-selects-pod` edge and no `containers` attribute. `ALERTS` SHALL be read under the request-scoped matchers alone — the only family the build reads across the requested zone. Every other family SHALL be read **by reference**, restricted to the object names the request's root or the reads before it actually carry, as this requirement and "Every root kind is tracked from its own tier to its claims" and "The tracked claims expand to both ends of the chain" define.

A by-reference restriction SHALL be a sorted, de-duplicated, anchored alternation on the family's own identity label, **composed with** the family's fixed, request-invariant selector where it has one and with the request-scoped matchers the family already carries — never replacing either. A family keyed by an `(ONTAP cluster, name)` pair SHALL be issued one query per ONTAP cluster, with that cluster as an equality and its names as the alternation, so no row outside the key set is read. Every restriction SHALL be chunked deterministically under one byte budget shared by every data-derived alternation, one query per chunk per family, results merged in chunk order, each chunk issued under the bare family name for self-metrics and span dimensions, and a single value SHALL always be issued even when it alone exceeds the budget. A by-reference family whose scope is empty SHALL NOT be issued and SHALL be absent from the build's per-family series tally; an issued family's entry is the total count of series its restrictions matched across every read of the build. A chunk error of any of these families SHALL fail the build, as "Storage build fails closed on upstream query errors" requires. Caller-originated cancellation SHALL fail the request whatever the family.

**Pods.** A pod is identified by its namespace and name, never by its name alone. `kube_pod_info` and `kube_pod_owner` SHALL be read restricted to the `(namespace, pod)` pairs of the mounters of the tracked claims, the `pod` roots, the pods a `node` root placed on a root node, and the pods an `application` root recovered: one query per namespace, carrying that namespace as an equality and its pod names as the alternation, so no pair outside the set is read and a same-named pod in another namespace — with its node and its controllers, which the next waves scope from what this read returns — is never fetched. The same pair keying SHALL apply to every other read of known pods: the `node` root's incarnation completion and claim-binding read, and the `application` root's claim-binding read. A `pod` root that mounts no claim, and every recovered pod that resolves a root Application, SHALL still be materialised.

**Kubernetes nodes.** `kube_node_info`, `kube_node_status_addresses`, `kube_node_labels` and `kube_node_status_condition` SHALL be read restricted on `node` to the nodes of the loaded pods and the `node` roots. An unscheduled pod contributes no name.

**Controllers.** The eight controller families SHALL be read in two stages, each restricted to the owner names the loaded pods carry. From every loaded `kube_pod_owner` series with `owner_is_controller="true"` and a non-empty `owner_name`, the first stage groups the names by `owner_kind` and issues, restricted on the family's identity label:

- `kube_replicaset_owner` and `kube_replicaset_annotations` on `replicaset` for the `ReplicaSet` names;
- `kube_job_owner` and `kube_job_annotations` on `job_name` for the `Job` names;
- `kube_statefulset_annotations` on `statefulset` for the `StatefulSet` names;
- `kube_daemonset_annotations` on `daemonset` for the `DaemonSet` names.

The second stage, waiting on the first alone, issues:

- `kube_deployment_annotations` on `deployment` for the union of the pods' direct `Deployment` owner names and the `owner_name` of every loaded `kube_replicaset_owner` series naming a `Deployment` owner;
- `kube_cronjob_annotations` on `cronjob` for the union of the pods' direct `CronJob` owner names and the `owner_name` of every loaded `kube_job_owner` series.

A kind no loaded pod is owned by, and a second-stage name set no first-stage series populated, SHALL issue no query. An owner kind the reader resolves no Application for contributes no name.

**Harvest.** The families SHALL be restricted as follows:

- `aggr_new_status`, `aggr_space_used` and `aggr_space_total` — to the `(ONTAP cluster, aggregate)` pairs the build's volume-label rows name, plus the aggregates an `aggr` root names (by name for a bare value, by `(ONTAP cluster, aggregate)` pair — one query per ONTAP cluster — for a qualified one) or an `ontap_cluster` root holds (by cluster).
- `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency` and `node_total_data` — to the `(ONTAP cluster, controller)` pairs the build's volume-label rows and aggregate gauge rows name, plus an `ontap_node` root (by name) or an `ontap_cluster` root (by cluster).
- The two `qos_policy_fixed_max_throughput_*` families — to the `(ONTAP cluster, SVM)` pairs of the tracked claims. Keying on the SVM rather than the policy group lets them run beside the QoS workload read instead of after it.
- The six QoS workload families — as the `netapp-storage-graph` capability's "Scoped and batched QoS workload read" defines.

**Output preservation.** Each reader consults a family only at keys inside that family's restriction:

- a node family at the node of a loaded pod or a root;
- an owner or annotation family at a loaded pod's owner chain;
- an aggregate gauge at an aggregate the volume-label rows or a root name;
- a controller family at an aggregate's owner or a root;
- a fixed-policy series at a tracked claim's SVM.

So the body SHALL be byte-identical to the body an unrestricted read would produce. Names are unique within a namespace, a cluster or an ONTAP cluster only, so a restriction MAY admit a same-named object elsewhere inside the request's selectors. Such a series is keyed under its own cluster, is consulted by no loaded object, and SHALL leave the body unchanged.

#### Scenario: Skipped families never reach the upstream

- **WHEN** the upstream rejects every `kube_pod_container_info` query with a series-limit error and a client sends any valid `/v1/storage-graph` request
- **THEN** the storage build issues no `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels` or `kube_service_annotations` query, `kube_state_graph_upstream_query_failures_total` does not move, and the request returns 200

#### Scenario: Only ALERTS is read across the zone

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0`
- **THEN** every issued query except `ALERTS` carries a restriction on its family's identity label beyond `<az-key>="zone-a",<env-key>="prod"`, and `ALERTS` carries the request-scoped matchers alone

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

### Requirement: Application roots are tracked through their controllers and claims

When a `/v1/storage-graph` request's root kind is `application`, the build SHALL track two sets:

- the pods owned by a controller whose ArgoCD tracking-id names one of the root Applications, recovered in the three stages below;
- the claims carrying a root Application of their own, read from `kube_persistentvolumeclaim_annotations` restricted on `annotation_argocd_argoproj_io_tracking_id` exactly as stage 1 restricts the controller families.

The claim-binding family SHALL be read restricted to the recovered pods' `(namespace, pod)` pairs — the namespace the recovering `kube_pod_owner` row carries, one query per namespace — keeping only rows whose `(cluster, namespace, pod)` is a recovered pod, so a same-named pod elsewhere contributes no claim. The claims those rows bind, together with the own-annotated claims, SHALL form the tracked claim set of "Every root kind is tracked from its own tier to its claims". Every mounter of a tracked claim is then loaded by the mounter completion of "The tracked claims expand to both ends of the chain".

The recovery is a **candidate generator, never a judge**: a recovered pod is read exactly like any other scoped pod, its Application is resolved by the same controller-annotation rules every pod's is, and whether it is a root is decided solely by the projection over that resolved value. A recovered pod whose resolved Application is not a root value — a controller whose lexically-smallest tracking-id in the window names a different Application — is loaded and then dropped, so the body is a pure function of the forward resolution and never of the recovery.

The recovery SHALL run in three stages, each waiting on the previous alone. Stage 1 is derived from the request, so nothing precedes it:

**Stage 1 — controllers by tracking-id.** Each of the six controller-annotation families (`kube_deployment_annotations`, `kube_statefulset_annotations`, `kube_daemonset_annotations`, `kube_replicaset_annotations`, `kube_job_annotations`, `kube_cronjob_annotations`) SHALL be issued restricted on `annotation_argocd_argoproj_io_tracking_id` to exactly the values whose segment before the first `:` is one of the root values (a value with no `:` matches when it equals a root value verbatim), composed with the family's fixed selector and the request-scoped matchers — never replacing either. Root values SHALL be sorted, de-duplicated and escaped so a value carrying a regex metacharacter matches itself and nothing else. The stage yields one name set per kind from each family's identity label (`deployment`, `statefulset`, `daemonset`, `replicaset`, `job_name`, `cronjob`).

**Stage 2 — the reverse hops.** `kube_replicaset_owner` SHALL be issued restricted to `owner_kind="Deployment"` and `owner_name` in the stage-1 Deployment names, yielding ReplicaSet names. `kube_job_owner` SHALL be issued restricted on `owner_name` to the stage-1 CronJob names beside its fixed selector, yielding Job names. A stage-1 name set that is empty SHALL issue no query for its hop.

**Stage 3 — pods by owner.** `kube_pod_owner` SHALL be issued restricted to `owner_is_controller="true"`, one query per owner kind whose name set is non-empty:

- `ReplicaSet` — stage-1 ReplicaSet names ∪ stage-2 ReplicaSet names;
- `Job` — stage-1 Job names ∪ stage-2 Job names;
- `StatefulSet` and `DaemonSet` — their stage-1 names;
- `Deployment` and `CronJob` — their stage-1 names, for a pod directly owned by either.

The recovered `pod` labels are the recovered pods.

Every restriction SHALL be chunked deterministically under the shared byte budget, one query per chunk, results merged in chunk order, each chunk issued under the bare family name. A chunk error on any recovery family — the six controller-annotation families, `kube_persistentvolumeclaim_annotations`, `kube_replicaset_owner`, `kube_job_owner` and `kube_pod_owner` — SHALL fail the build, as "Storage build fails closed on upstream query errors" requires. Caller-originated cancellation fails the request whatever the family.

**Stage 1 and the claim-annotation read are request-derived.** When either would take more than the fixed maximum number of queries, the request SHALL be rejected 400 with `reason: "invalid_scope"` before any upstream query is issued, as "Every root kind is tracked from its own tier to its claims" requires of every root set. Stages 2 and 3 are derived from upstream data and SHALL NOT be bounded.

**Tally.** Series a stage returns SHALL be counted under the family's name in the build's per-family tally, added to what the by-reference read of the same family contributes.

The tracked set is output-preserving:

- a pod can be a root only if it resolves a root Application, and every such pod is recovered;
- a path is retained only through a pod hit, whose pod is recovered and whose claims are therefore tracked;
- or through a claim hit, where the claim is own-annotated (read by tracking-id) or inherits the Application from a mounter that is recovered.

An empty stage yields nothing downstream. When no controller and no claim carries a root Application, no binding, pod or later query is issued.

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

- **WHEN** a client sends `?application=typo` and no controller-annotation or claim-annotation series in the selected estate carries a tracking-id naming `typo`
- **THEN** stage 1 and the claim-annotation read issue their queries, no stage-2, stage-3, claim-binding or pod query is issued, and the body is empty

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
- **THEN** no query restricted on `annotation_argocd_argoproj_io_tracking_id` or on `owner_name` is issued

#### Scenario: The recovery is tallied under the family name

- **WHEN** a client sends `?application=checkout`, stage 1 returns two `kube_deployment_annotations` series, and the by-reference controller read later returns three for the loaded pods' Deployments
- **THEN** the per-family tally carries `kube_deployment_annotations` with `5`, and a family only the recovery issued is present with the count of series its restriction matched

#### Scenario: A stage-1 annotation chunk failure fails the build

- **WHEN** a stage-1 `kube_job_annotations` chunk fails with an upstream error while every other query succeeds
- **THEN** the build returns an error, the request is mapped as an upstream failure naming `kube_job_annotations`, and no body is returned

### Requirement: Roots are always materialised when the upstream knows them

A root the upstream names in the window SHALL appear in the body even when no flow passes through it. A root "exists" when a series the build reads for it names it: an `ontap_cluster` root when any Harvest series read for it carries that ONTAP cluster (its controllers, aggregates and SVMs are then all roots); an `aggr` root when a `volume_labels` or `aggr_*` series names that aggregate (for a qualified value, that aggregate on that ONTAP cluster); an `svm` root when a `volume_labels` series names it (for a qualified value, on that ONTAP cluster); an `ontap_node` root when a `volume_labels`, `node_labels`, `node_new_status` or node performance series names that controller; a `node` root when `kube_node_info` names that Kubernetes node; a `pod` root when `kube_pod_info` names it in the selected estate; a `pvc` root when a `kube_persistentvolumeclaim_info` series names that `(namespace, claim)` in the selected estate; a `pv` root when a `kube_persistentvolumeclaim_info` series names it as `volumename`, in which case the claim that series names is the materialised root. "Every root kind is tracked from its own tier to its claims" defines the reads that make a root with no claim visible. A flowless root SHALL be emitted with its ordinary attributes and its compound parent (an aggregate root also materialises the controller currently owning it, so that `data.parent` never dangles) and **no** edges. A root NO series names SHALL NOT be drawn: the body is simply empty of it, with no error and no marker.

A root claim (of a `pvc` or `pv` root) is NOT flowless merely because no pod mounts it: when its volume joins the Harvest topology it keeps its storage-side path, as "Storage-reachability projection" defines. Only a root claim whose volume joins no Harvest topology (a non-NetApp claim, or a join miss) is emitted alone, with its ordinary attributes and compound parents and no edges; its mounting pods are then NOT drawn, exactly as a pod root's non-NetApp claims are not.

An `application=` root exists when at least one pod loaded in the selected estate resolves that Application, and **every** such pod SHALL be materialised with its ordinary attributes and compound parents and no edges — including pods that mount no claim, which the build loads for exactly this purpose (see "Application roots are tracked through their controllers and claims") — so a stateless Application returns its pods rather than an empty body. A claim carrying a root Application SHALL NOT be materialised on its own: it participates in path retention only, and appears only on a retained path. An Application no loaded pod resolves is not drawn.

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

### Requirement: Fixed tier chain and the `storage-flow` edge

The body SHALL express the storage chain as directed edges of ONE type, `storage-flow`, oriented storage → workload, one edge per adjacent pair on the fixed tier chain `netapp-node → netapp-aggr → netapp-svm → pvc → pod → node`. Each edge SHALL carry `labels.tier` naming its hop — exactly one of `node-aggr`, `aggr-svm`, `svm-pvc`, `pvc-pod`, `pod-node` — and its `id` SHALL be the UUIDv5 of `storage-flow|<source>|<target>` under the server's fixed edge namespace, so an edge is byte-stable across rebuilds. Each `(source, target)` pair SHALL appear at most once regardless of how many claims flow through it.

The chain SHALL be derived from the existing storage join: a claim's aggregate and SVM from its matched `volume_labels` series, its owning controller from that aggregate's `node`, its mounting pods from `kube_pod_spec_volumes_persistentvolumeclaims_info`, and each pod's node from `kube_pod_info`. A claim whose match resolved an SVM but no aggregate (the FlexGroup shape) SHALL enter the chain at the `svm-pvc` tier with no `node-aggr` / `aggr-svm` edge. A pod not scheduled on a node SHALL end its path at the `pvc-pod` tier. The body SHALL NOT contain `pod-mounts-pvc`, `pod-to-node`, `pvc-to-netapp-aggr`, `pod-calls-*`, or `service-selects-pod` edges, and SHALL NOT contain `service` or `external` nodes.

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

### Requirement: Flow weights on every tier

Every `storage-flow` edge on a path with at least one measured claim SHALL carry `data.metrics` with `read_ops`, `write_ops`, `read_bytes_per_sec` and `write_bytes_per_sec`, each the sum — over every claim whose path passes through that edge — of the claim's own I/O measurement from the storage join (the `pvc-to-netapp-aggr` figures of `/v1/graph`). The `svm-pvc` edge, being the claim-level edge, SHALL additionally carry the claim's `read_latency_us`, `write_latency_us` and, when resolved, `max_iops` / `max_bytes_per_sec`; no other tier SHALL carry latency or ceiling fields. A claim with no I/O measurement contributes nothing to any sum, and an edge whose every contributing claim is unmeasured SHALL carry no `metrics` key. Sums SHALL be accumulated in ascending contribution order and rounded to 6 significant digits at serialisation, so the wire form is order-independent.

A claim mounted by more than one pod SHALL have its weight **split equally** across the `pvc-pod` edges to its mounting pods (each receiving `1/n` of every summed figure), and each such `pvc-pod` edge SHALL carry `labels.attribution="split"`; a `pod-node` edge SHALL sum the (possibly split) `pvc-pod` weights of its pod. A `pvc-pod` edge from a singly-mounted claim SHALL carry no `attribution` label. An **unmounted root claim** (see "Storage-reachability projection") is a sink: its whole measurement SHALL be summed onto its `svm-pvc` edge and every upstream hop of its path exactly as a mounted claim's is, with no split and no `pvc-pod` edge below it. Weights therefore conserve tier to tier: for every non-root interior node, the sum of incoming `read_ops` equals the sum of outgoing `read_ops` (likewise the other three figures), up to rounding.

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

### Requirement: Storage-reachability projection

The body SHALL retain a node iff it lies on a **complete** storage-flow path (`netapp-node → … → pod`, or `netapp-svm → … → pod` for a FlexGroup claim; for a root claim of a `pvc` or `pv` root that no pod mounts, `netapp-node → … → pvc` or `netapp-svm → pvc`, ending at the claim) that touches a root of the request's one root kind ("One root kind per request"), or it is a materialised root (or a root's real compound parent). An `application=` root is satisfied by a path whose pod or whose claim carries the Application; the pods it materialises are those whose own `data.application` is a root value. A `pvc` / `pv` root is satisfied by a path whose claim is a root claim — every mounting pod's path of that claim is retained. An **unmounted** claim that is not a root claim SHALL be dropped, and so SHALL any aggregate, SVM or controller reachable only through unmounted claims; a pod none of whose claims joins the Harvest topology SHALL be dropped unless it is a materialised root; a Kubernetes node hosting only dropped pods SHALL be dropped. The `/v1/graph` connectivity prune SHALL NOT apply. `cluster` / `namespace` narrow the claim / pod / node side upstream and are re-applied at projection; a storage root is never dropped by them.

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

### Requirement: Attributes and compound groups carry over

Every retained real node SHALL carry the same `data` attributes it carries in `/v1/graph` — including `ipaddress`, `owner`, `application`, `ready_status`, `health`, `usage`, `storageclass`, `qos`, `hardware`, `perf`, `alerts` and `status` — and the body SHALL include the same synthesised compound groups with the same `data.parent` rules (`cluster > namespace > application > controller > pod`, `cluster > namespace > [application >] pvc`, `cluster > node`, `storage-cluster > netapp-node > netapp-aggr`, `storage-cluster > netapp-svm`). Namespace and ArgoCD Application SHALL NOT be tiers of the flow; a consumer derives a namespace- or Application-level Sankey by walking `data.parent` and summing the conserved weights.

A PVC's `qos` attribute is independent of its flow: it SHALL be carried whenever the claim's ceiling resolved, including when the claim's `svm-pvc` edge carries no `metrics` (no flow and no latency to report) and when the claim enters the chain at the SVM (the FlexGroup shape). Whenever the `svm-pvc` edge carries `max_iops` / `max_bytes_per_sec`, the PVC's `data.qos` carries the same values.

A storage-graph pod SHALL NOT carry `data.containers`: the storage build does not read the container family (see "Storage build reads every family by reference"), and a storage-flow consumer has no use for a container list. This is the ONE attribute on which the two bodies differ for the same pod.

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

### Requirement: Each claim names its aggregate

A PVC node in the body SHALL carry the same `labels.aggr` it carries in `/v1/graph` (see "PVC `aggr` label" in `graph-api`): the node id of the aggregate its volume sits on, absent for a FlexGroup claim. It is the one place the body states a claim's aggregate. The `aggr-svm` tier is shared by every claim an SVM holds on that aggregate, so once an SVM spans more than one aggregate, walking up from a PVC through its SVM cannot tell which aggregate the claim sits on.

Whenever a PVC carrying `labels.aggr` is in the body, the node it names SHALL be in the same body: it lies on the claim's path (`netapp-node → netapp-aggr → netapp-svm → pvc`), which the storage-reachability projection retains whole, and it has an `aggr-svm` edge to the PVC's SVM. No `storage-flow` edge SHALL carry a label naming a claim's aggregate: the tier chain and the edge labels (`tier`, `attribution`) are unchanged.

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

### Requirement: Deterministic storage-graph body

The body SHALL be byte-identical for identical `(window, az, env, roots, cluster, namespace)` against identical upstream state — `roots` covering every root selector, `application` included — with nodes and edges sorted as in `/v1/graph`, `clusters` sorted, weights order-independent, and no time-of-build or echo-of-input field. Root selector values given in a different order or repeated SHALL produce an identical body, and SHALL issue identical upstream queries.

#### Scenario: Root order does not matter

- **WHEN** a client sends `?aggr=b&aggr=a` and then `?aggr=a&aggr=b&aggr=a`
- **THEN** both bodies are byte-identical

#### Scenario: Application root order does not matter

- **WHEN** a client sends `?application=b&application=a` and then `?application=a&application=b&application=a`
- **THEN** both requests issue identical recovery queries and return byte-identical bodies

### Requirement: In-process storage-graph engine surface

The reusable graph engine SHALL expose the storage-graph build in-process with the same contract as the HTTP endpoint: a request parser accepting the endpoint's query parameters and returning the same validation failures (same `reason` codes), and a single call producing the identical Cytoscape body from those parameters, so an embedding module obtains byte-for-byte the `/v1/storage-graph` body without an HTTP hop. The HTTP handler SHALL use that same parser, so the request contract cannot drift between server and embedder.

#### Scenario: Embedder and server agree

- **WHEN** the same query parameters are given to the in-process call and to `GET /v1/storage-graph` against the same upstream state
- **THEN** the two bodies are byte-identical, and an invalid parameter set fails both with the same `reason`

### Requirement: Storage build fails closed on upstream query errors

A `/v1/storage-graph` build SHALL fail when ANY upstream query it issues returns an error — whatever the family, and whichever chunk, phase, stage or backend of that family failed — with the single exception of `ALERTS`. This covers every family `/v1/graph` reads as OPTIONAL or degrading: `volume_labels` (every read), the six `qos_*` workload families (every chunk), the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency`, `node_total_data`, `kubelet_volume_stats_used_bytes`, `kubelet_volume_stats_capacity_bytes`, `kube_replicaset_annotations` and `kube_job_annotations` (by-reference read and application recovery alike). The per-family log-and-continue, per-chunk degrade and degraded-family hop-suppression rules of the `netapp-storage-graph` and `cluster-topology-source` capabilities SHALL apply to `/v1/graph` builds only. A storage body with a silently missing family is indistinguishable from a smaller estate — a filer with no flow, an aggregate with no I/O — so the endpoint SHALL NOT return one.

The failure SHALL be mapped exactly as a required-leg failure is: HTTP 502 with `reason: "upstream"`. The `message` SHALL name the family whose query failed (`upstream query failed: <family>`, the bare family name) and SHALL NOT carry an upstream URL, host, address or credential. A build that exceeds `--build-timeout` SHALL still map to 504 `timeout`, and caller cancellation to the existing `canceled` mapping. Every failed query SHALL still be logged server-side with its full error and counted on `kube_state_graph_upstream_query_failures_total{query}`. The in-process storage-graph engine surface SHALL return the same typed upstream error, carrying the family name.

`ALERTS` SHALL stay OPTIONAL: a failed `ALERTS` query is logged and counted, the build continues with no alert attached to any node, and every node's `status` is folded from its remaining signals.

The following SHALL NOT fail the build, because no query failed: a query that returns an empty vector (a family the deployment does not export, an annotation family that is not allowlisted, a scope that matched nothing); a requested zone that no backend serving the family declares (the existing empty-vector-and-warning rule); and a query the build does not issue.

`GET /v1/graph` SHALL keep every existing error class unchanged.

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
- **THEN** the server returns 200 and the Job-owned pods resolve their Application through the CronJob hop exactly as before this requirement

#### Scenario: A zone with no backend is not a failure

- **WHEN** a client sends `?az=zone-c&env=prod&aggr=aggr1` and no backend serving `harvest` declares `zone-c`
- **THEN** the Harvest legs return empty vectors, the existing warning is logged, and the server returns 200

#### Scenario: The graph endpoint keeps degrading

- **WHEN** a client sends `GET /v1/graph` and the `volume_labels` query fails with an upstream error
- **THEN** the server returns 200 exactly as before this requirement, with no `pvc-to-netapp-aggr` edge

### Requirement: Storage-graph end-time alignment

`GET /v1/storage-graph` SHALL apply the same end-time alignment as `GET /v1/graph` ("Time-window passthrough" in the `graph-api` capability): with a non-zero `--end-align` grid, `end` is floored to the grid and `start` shifted by the same amount before any upstream query is rendered, and the alignment SHALL happen after `start` / `end` validation, so validation errors are unchanged.

#### Scenario: Storage request aligned like a graph request

- **WHEN** `--end-align=30s` and a client sends `GET /v1/storage-graph?start=2026-05-01T12:00:10Z&end=2026-05-01T12:05:10Z&az=zone-a&env=prod`
- **THEN** every upstream query is evaluated at `2026-05-01T12:05:00Z` over a 5m window
