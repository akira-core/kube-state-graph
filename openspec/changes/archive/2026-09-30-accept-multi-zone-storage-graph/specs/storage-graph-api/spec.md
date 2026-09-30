# Spec Delta

## MODIFIED Requirements

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
