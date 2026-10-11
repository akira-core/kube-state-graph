# Spec Delta

## MODIFIED Requirements

### Requirement: One root kind per request

The endpoint SHALL accept the following root kinds, each a repeatable parameter whose values are validated like a `/v1/graph` selector value (≤ 253 bytes, valid UTF-8, no control characters; otherwise 400 `invalid_scope`):

- `ontap_cluster=<name>` — an ONTAP cluster: every controller, aggregate and SVM in it is a root;
- `ontap_node=<name>` — an ONTAP controller. A path is retained by it when the path's aggregate is owned by that controller — the owner the `netapp-storage-graph` capability's owning-controller vote resolves — so the root reaches the aggregates the controller owns, the SVMs and claims on those aggregates, and the pods, Kubernetes nodes, Applications and namespaces below them. It never reaches a claim on an aggregate another controller owns, even when both aggregates serve one SVM;
- `aggr=<name>` — an ONTAP aggregate, matched on EVERY ONTAP cluster the selected zone's Harvest backends return, so a name present on two filers roots both aggregates;
- `svm=<name>` — an SVM, matched on every ONTAP cluster likewise;
- `node=<name>` — a Kubernetes node, matched in every Kubernetes cluster of the selected estate. It SHALL NOT match an ONTAP controller;
- `pod=<namespace>/<pod-name>` — one pod. A value without exactly one `/` separating two non-empty segments SHALL be rejected 400 `invalid_scope`;
- `pvc=<namespace>/<claim-name>` — one PersistentVolumeClaim, matched in every Kubernetes cluster of the selected estate. A value without exactly one `/` separating two non-empty segments SHALL be rejected 400 `invalid_scope`;
- `pv=<name>` — a PersistentVolume, by its bare (cluster-scoped) name, matched in every Kubernetes cluster of the selected estate. It roots the claim(s) whose `kube_persistentvolumeclaim_info` series names it as `volumename`; a PersistentVolume bound to no claim roots nothing;
- `application=<name>` — an ArgoCD Application, in the form `data.application` carries it (the segment of the tracking-id before the first `:`). A path is retained by it when its **pod or its claim** carries that Application — the claim's own annotation or the Application it inherited from a mounting pod, exactly as the body reports it. A value containing `:` can name no Application and matches nothing.

Empty values SHALL be dropped, so a bare `?aggr=` is a no-op and does not count as a root. A request SHALL carry exactly ONE root kind with at least one non-empty value. A request carrying none SHALL be rejected 400 with `reason: "missing_root"`. A request carrying two or more root kinds SHALL be rejected 400 with `reason: "invalid_scope"` and a message naming the parameters. The values of the one kind SHALL be OR-combined: a path is retained when it touches any of them. Root names are matched exactly and case-sensitively. `cluster` and `namespace` are narrowing filters, not root kinds, and SHALL combine with any root kind.

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

**Storage-side kinds.** `ontap_cluster` reads `volume_labels` restricted on `cluster` to the root values; `aggr` restricted on `aggr`; `svm` restricted on `svm`. `ontap_node` reads `volume_labels` restricted on `node` to the root values, then reads WHOLE every `(ONTAP cluster, aggregate)` pair those rows name. Only the aggregates whose owning controller the vote resolves to a root value contribute claims: the vote picks one of the aggregate's own `node` values, so every aggregate a root controller owns has a row naming it and is found by the first read. For each of the four kinds, the build SHALL derive **candidate PersistentVolume names** from the `volume` label of every contributing row: for every position at which the value continues with `pvc_` and which is the start of the value or follows a `_`, the remainder of the value from that position with every `_` replaced by `-` is one candidate; candidates are sorted and de-duplicated. It SHALL then read `kube_persistentvolumeclaim_info` restricted on `volumename` to the candidates, and the claims that read returns are the tracked set. Candidate extraction is a **generator, never a judge**: a candidate naming no PersistentVolume loads nothing, and whether a loaded claim lands on a FlexVol — and on which aggregate, SVM and controller — SHALL be decided solely by the forward derivation and join of the `netapp-storage-graph` capability, exactly as on `/v1/graph`. The rewrite rules SHALL NOT change the extraction, so a custom rule set can make a storage root find fewer claims, never a wrong one.

**Kubernetes node.** `node` reads `kube_pod_info` restricted on `node` to the root values, then reads `kube_pod_info` again restricted to the `(namespace, pod)` pairs the first read returned, WITHOUT the node restriction (**incarnation completion**): a pod's node is its newest incarnation's, so a pod name recreated on another node inside the window SHALL be placed where an unrestricted read places it. It then reads the claim-binding family restricted to the `(namespace, pod)` pairs of the pods whose newest incarnation runs on a root node, keeping only rows whose `(cluster, namespace, pod)` names such a pod. The claims those rows bind are the tracked set. Both reads are keyed by `(namespace, pod)` as "Storage build reads every family by reference" requires of every read of known pods.

**Pod.** `pod` reads the claim-binding family restricted on `namespace` and `pod` to the roots, keeping only rows whose `(namespace, pod)` is a root ref, and reads `kube_pod_info` restricted to the root names. The claims those rows bind are the tracked set.

**Claim.** `pvc` reads `kube_persistentvolumeclaim_info` restricted to the root `(namespace, claim)` pairs — one query per namespace, carrying that namespace as an equality and its claim names as the alternation on `persistentvolumeclaim` — keeping only rows whose `(namespace, claim)` is a root ref. The claims those rows name, in every Kubernetes cluster of the selected estate, are the tracked set.

**Volume.** `pv` reads `kube_persistentvolumeclaim_info` restricted on `volumename` to the root values. The claims that read returns are the tracked set. This is the storage-side kinds' claim read without the candidate derivation: no `volume_labels` read precedes it, and a root value is used verbatim, never rewritten.

For both kinds the seed read IS the claim-side `kube_persistentvolumeclaim_info` read of "The tracked claims expand to both ends of the chain", which is therefore not issued again; the expansion's other claim-side reads, mounter completion, candidate completion, owner completion and storage-side reads apply unchanged. Because candidate completion reads `volume_labels` by the derived token of every tracked claim, a claim root's aggregate, SVM and controller are found exactly as on `/v1/graph`.

**Application.** `application` runs the recovery of "Application roots are tracked through their controllers and claims" and tracks the claims that requirement names.

**Closure.** The tracked set SHALL contain every claim the projection can retain for the request. A claim is retained through a storage-side root only if its picked aggregate, SVM or controller is a root, which requires at least one of its candidates on a rooted component, so the rooted rows name it. It is retained through a `node` or `pod` root only if one of its mounting pods is or runs on a root, so that pod's bindings name it. It is retained through a `pvc` or `pv` root only if it is itself a root claim, which the seed read names.

**Roots with no claim.** For the root alone, the build SHALL also read the families that materialise a root no claim reaches, as "Roots are always materialised when the upstream knows them" requires:

- the aggregate gauge families for `aggr` (by aggregate name) and for `ontap_cluster` (by ONTAP cluster);
- the controller families for `ontap_node` (by controller name) and for `ontap_cluster` (by ONTAP cluster);
- the four Kubernetes-node families for `node`;
- `kube_pod_info` for `pod`;
- nothing further for `pvc` and `pv`: the seed read is the read that names a root claim.

**Bounded root sets.** Root parameters are repeatable and the parser bounds each value's length, never their count, so a root set is a scope a client can inflate. When a root kind's first read would take more than a fixed maximum number of queries under the shared byte budget, the request SHALL be rejected 400 with `reason: "invalid_scope"` before any upstream query is issued. The first read's shape is a pure function of the root values and the byte budget, so no upstream data is needed to decide. The build SHALL NOT fall back to reading the family across the zone.

**Static PersistentVolumes.** A claim bound to a PersistentVolume whose name yields no candidate — a statically provisioned PV, or a provisioner configured with a non-`pvc` volume-name prefix — SHALL NOT be found from a storage-side root, even when the forward join would match it. `/v1/graph`, and a `node`, `pod`, `application`, `pvc` or `pv` root reaching the same claim, SHALL still join it — a `pvc` / `pv` root in particular is the way to root at a claim bound to a statically provisioned volume.

**Coverage signal.** When a storage-side root's contributing rows carry a `volume` and none produced a candidate, the build SHALL log one aggregated warning `storage_root_claim_miss` with `reason="no_pv_candidate"` and the volume count. When candidates were produced and the claim-info read returned no claim, it SHALL log `storage_root_claim_miss` with `reason="no_claim"` and the candidate count. `no_claim` SHALL be logged at Debug instead when the request carries a `cluster` or `namespace` filter, which can legitimately exclude every candidate. Neither changes the response status.

**Zone.** Every query the build issues SHALL carry the request-scoped matchers its family accepts and SHALL be dispatched only to the backends the request's `az` selects. A claim on a rooted filer that lives in another zone or environment is therefore not loaded and SHALL NOT be drawn.

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

#### Scenario: A storage root stays in the request's zone

- **WHEN** two backends serve `ksm` with `zones: [zone-a]` and `zones: [zone-b]`, two backends serve `harvest` likewise, filer `ontap-prod` holds FlexVols whose claims live in a `zone-a` and a `zone-b` cluster, and a client sends `?az=zone-a&env=prod&ontap_cluster=ontap-prod`
- **THEN** no query is issued to either `zone-b` backend, every kube-state-metrics, kubelet and `ALERTS` query carries `<az-key>="zone-a",<env-key>="prod"`, the body contains the `zone-a` claim's complete path and not the `zone-b` claim, and `clusters` lists `zone-a` identities only

### Requirement: Roots are always materialised when the upstream knows them

A root the upstream names in the window SHALL appear in the body even when no flow passes through it. A root "exists" when a series the build reads for it names it: an `ontap_cluster` root when any Harvest series read for it carries that ONTAP cluster (its controllers, aggregates and SVMs are then all roots); an `aggr` root when a `volume_labels` or `aggr_*` series names that aggregate; an `svm` root when a `volume_labels` series names it; an `ontap_node` root when a `volume_labels`, `node_labels`, `node_new_status` or node performance series names that controller; a `node` root when `kube_node_info` names that Kubernetes node; a `pod` root when `kube_pod_info` names it in the selected estate; a `pvc` root when a `kube_persistentvolumeclaim_info` series names that `(namespace, claim)` in the selected estate; a `pv` root when a `kube_persistentvolumeclaim_info` series names it as `volumename`, in which case the claim that series names is the materialised root. "Every root kind is tracked from its own tier to its claims" defines the reads that make a root with no claim visible. A flowless root SHALL be emitted with its ordinary attributes and its compound parent (an aggregate root also materialises the controller currently owning it, so that `data.parent` never dangles) and **no** edges. A root NO series names SHALL NOT be drawn: the body is simply empty of it, with no error and no marker.

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

