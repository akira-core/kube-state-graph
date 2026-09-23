## ADDED Requirements

### Requirement: Storage-side roots read the claim chain through the volume hub

A `/v1/storage-graph` build SHALL be in **hub mode** iff it carries at least one `ontap_cluster=`, `aggr=` or `svm=` root AND its Harvest volume-label read is restricted under "Storage-side roots narrow the Harvest topology read per component" (a configured match mode that renders as an alternation branch, and a phase 1 within its query bound). Every other build — `/v1/graph`, a rootless storage request, a request rooted only at `pod=`, `application=` or `node=`, a `contains` / `regex` match mode, or an unbounded root set — SHALL read the claim families exactly as before this requirement.

**PV candidates.** In hub mode the build SHALL derive candidate PersistentVolume names from the `volume` label of every phase-1 volume-label row: for every position at which the value continues with `pvc_` and which is the start of the value or follows a `_`, the remainder of the value from that position with every `_` replaced by `-` is one candidate. Candidates SHALL be sorted and de-duplicated. Owner-completion and phase-2 rows SHALL NOT produce candidates. Candidate extraction is a **candidate generator, never a judge**: a candidate naming no PersistentVolume loads nothing, and whether a loaded claim lands on a FlexVol — and on which aggregate, SVM and controller — SHALL be decided solely by the configured forward derivation and join of the `netapp-storage-graph` capability, exactly as in every other build. The operator's volume-key rewrite rules and match mode SHALL NOT change the extraction.

**Claim-keyed reads.** In hub mode the build SHALL NOT issue `kube_persistentvolumeclaim_info`, `kube_pod_spec_volumes_persistentvolumeclaims_info`, `kube_persistentvolumeclaim_annotations`, `kubelet_volume_stats_used_bytes` or `kubelet_volume_stats_capacity_bytes` in its first wave. It SHALL instead issue `kube_persistentvolumeclaim_info` restricted on `volumename` to the candidates, once phase 1 has returned, and then the other four families restricted on `persistentvolumeclaim` to the claim names the claim-info read returned. Rows of those four families SHALL be kept only when their `(cluster, namespace, claim)` names a claim the claim-info read returned, before the pod scope is computed and before the parse, so a same-named claim in another namespace or cluster neither loads a pod nor creates a PVC node. Each restriction SHALL follow the mechanics of every other data-derived restriction of "Storage build reads only what the body draws" — a sorted, de-duplicated, anchored alternation composed with the family's fixed selector and the request matchers, chunked under the shared byte budget, merged in chunk order, issued under the bare family name — and a query error of any of them SHALL fail the build, as "Storage build fails closed on upstream query errors" requires. An empty scope SHALL issue no query for the families it would restrict: no candidate issues no claim query, and no claim issues no binding, annotation, kubelet, pod, Kubernetes-node or controller query.

**Bounded claim scopes.** When a claim-keyed family's restriction would take more than a fixed maximum number of queries, the build SHALL issue that family once with no restriction — its fixed selector and the request matchers only — and SHALL keep only the rows the restriction would have admitted. The body SHALL be identical either way.

**Relaxed request matchers.** In hub mode every kube-state-metrics, kubelet and `ALERTS` query of the build — first wave, claim-keyed reads, pod, Kubernetes-node and controller reads, and the application recovery — SHALL carry the request's `cluster` and `namespace` matchers and SHALL NOT carry the `az` or `env` matcher, and SHALL be dispatched to every backend serving its family regardless of `az`. The Harvest queries SHALL still be dispatched to the backends `az` selects. Both dispatch decisions SHALL be taken from one routing snapshot for the whole build. Cluster identities SHALL still be composed from each series' own zone and environment labels, so a cluster name reused across zones stays distinct.

**Static PersistentVolumes.** A claim bound to a PersistentVolume whose name yields no candidate — a statically provisioned PV, or a provisioner configured with a non-`pvc` volume-name prefix — SHALL NOT be found from a storage root, even when the forward join would match it. `/v1/graph` and every build outside hub mode SHALL still join it.

**Coverage signal.** When phase 1 returned rows carrying a `volume` and none produced a candidate, the build SHALL log one aggregated warning `storage_root_claim_miss` with `reason="no_pv_candidate"` and the volume count. When candidates were produced and the claim-info read returned no claim, it SHALL log `storage_root_claim_miss` with `reason="no_claim"` and the candidate count. Neither changes the response status.

#### Scenario: Rooted volumes name their claims

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00`, phase 1 returns FlexVol `trident_pvc_ab12_cd34`, and claim `shop/orders-data` is bound to PV `pvc-ab12-cd34` and mounted by `orders-0` and `orders-1`
- **THEN** `kube_persistentvolumeclaim_info` is issued restricted to `volumename=~"pvc-ab12-cd34"`, the claim-binding, claim-annotation and kubelet queries are restricted to `persistentvolumeclaim=~"orders-data"`, the pod read is restricted to `{orders-0, orders-1}` plus any pod or recovered root, and the body carries the claim's complete path

#### Scenario: A FlexVol with no storage prefix yields a candidate

- **WHEN** phase 1 returns FlexVol `pvc_ab12_cd34`
- **THEN** the candidate set contains `pvc-ab12-cd34`

#### Scenario: An over-generated candidate loads nothing

- **WHEN** phase 1 returns FlexVol `trident_pvc_ab12_clone`, a clone no PersistentVolume is named after
- **THEN** the candidate `pvc-ab12-clone` is issued, the claim-info read returns no row for it, and the body is unchanged by its presence

#### Scenario: Root volumes produce no candidate

- **WHEN** phase 1 returns `vol0` and `svm_shop_root` besides claim volumes
- **THEN** neither contributes a candidate, and no coverage warning fires on their account

#### Scenario: A same-named claim in another namespace is filtered out

- **WHEN** the hub loads claim `shop/data` and the claim-binding read, restricted to `persistentvolumeclaim=~"data"`, also returns a binding of `platform/data`
- **THEN** the `platform/data` binding is discarded before the pod scope is computed, its pod is not read, and no `platform/data` PVC node is built

#### Scenario: A statically provisioned volume is not reached from a storage root

- **WHEN** claim `db/mongo-data` is bound to PV `mongo-data-01`, the filer holds FlexVol `mongo_data_01`, and a client roots at the aggregate holding it
- **THEN** no candidate names `mongo-data-01`, the hub-mode body draws no path through that claim, and `GET /v1/graph` for the same zone still draws its `pvc-to-netapp-aggr` edge

#### Scenario: No candidate is reported

- **WHEN** a client roots at `ontap_cluster=ontap-prod` and every FlexVol on it is named by a Trident `nameTemplate` that embeds no `pvc_`
- **THEN** no claim query is issued, the request returns 200 with the rooted storage entities and no path, and one `storage_root_claim_miss` warning with `reason="no_pv_candidate"` is logged

#### Scenario: Candidates naming no claim are reported

- **WHEN** candidates are produced and the claim-info read returns no row
- **THEN** no binding, pod, Kubernetes-node or controller query is issued, the request returns 200, and one `storage_root_claim_miss` warning with `reason="no_claim"` is logged

#### Scenario: A large claim scope falls back to a filtered read

- **WHEN** a client roots at `ontap_cluster=` on a filer whose candidates would take more claim-info queries than the maximum
- **THEN** `kube_persistentvolumeclaim_info` is issued once with no `volumename` restriction and no `<az-key>` / `<env-key>` matcher, only the candidate rows are kept, and the body is byte-identical to the body the chunked restriction would have produced

#### Scenario: A request outside hub mode reads claims as before

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0`, or `?az=zone-a&env=prod&node=ontap-prod-01`, or roots at `aggr=` under the `contains` match mode
- **THEN** the claim families are issued in the first wave with `<az-key>="zone-a",<env-key>="prod"` and no claim-keyed restriction, exactly as before this requirement

### Requirement: Storage-side roots narrow the Harvest topology read per component

When a `/v1/storage-graph` request carries at least one `ontap_cluster=`, `aggr=` or `svm=` root, the build SHALL read the Harvest volume-label topology family restricted to the rooted components instead of reading the whole filer, and the body SHALL be byte-identical to the body an unrestricted read produces for every estate whose aggregates and controllers are each named by their own Harvest gauge families (the stock `aggr_*` and `node_*` templates). An aggregate or controller named by the volume-label family alone, outside the rooted components, is not materialised by a restricted read; that can change whether an alert without a `cluster` label matches a unique entity, and is the only divergence.

**Phase 1 — the root restriction.** The build SHALL issue the family restricted by the request's `ontap_cluster=`, `aggr=` and `svm=` values: `ontap_cluster=` restricts the family's `cluster` label, `aggr=` its `aggr` label and `svm=` its `svm` label. `ontap_cluster=` values SHALL be carried on the aggregate query and on the SVM query as a narrowing, because the projection combines them that way — an `aggr=` or `svm=` root names a component only within the `ontap_cluster=` values, so a same-named component on another filer is not a root and its volumes need not be read. A request carrying only `ontap_cluster=` roots SHALL issue the cluster restriction alone. A request carrying both `aggr=` and `svm=` roots SHALL issue both an aggregate-restricted and an SVM-restricted query and SHALL union their results, de-duplicated by label set, because the projection UNIONS the two roots. Each value set SHALL be rendered as a sorted, de-duplicated, anchored alternation, escaped so a value carrying a regex metacharacter matches itself and nothing else. The restriction is the ONLY matcher this query carries; Harvest takes no request-scoped selector.

The restriction is derived from the request's roots, not from a selector-level dimension, so the request-scoped selector table is unchanged and `az` still reaches Harvest through backend selection alone. A `pod=` root does not prevent it: the projection ANDs the workload roots with the storage roots, so every retained path is one the restriction keeps.

**Phase 2 — candidate recovery.** A claim's aggregate and SVM are picked lexically-smallest over that claim's WHOLE candidate set, so a phase-1-only read could place a claim on a rooted aggregate where an unrestricted read would place it on a lexically-smaller one — a clone whose FlexVol name also matches the claim's derived token, or the same FlexVol name on a second filer. After phase 1, and after the claim-info family has landed, the build SHALL therefore issue a second restricted read of the same family, restricted on `volume` to the derived tokens of exactly the claims phase 1 matched, expressed in the **forward** direction of the configured derivation — the direction the join already computes. Phase 2 inverts nothing: the claims it completes are the claims the hub loaded ("Storage-side roots read the claim chain through the volume hub"), and their tokens are derived from their PersistentVolume names exactly as the join derives them. The two phases' results SHALL be merged and de-duplicated by label set before the parse, and every downstream consumer — the aggregate and SVM picks, the owning-controller vote, the inventory, and the QoS workload read's `volume` scope — SHALL run over the merged result.

When phase 1 matched no claim, phase 2 SHALL NOT be issued.

**Owner completion.** An SVM-restricted query returns only that SVM's volumes on each aggregate it touches, while the owning controller of an aggregate is a vote over ALL of that aggregate's volume-label series (see the `netapp-storage-graph` capability's "NetApp aggregate entity"). After phase 1, the build SHALL therefore issue the family restricted to every `(ONTAP cluster, aggregate)` pair named by an SVM-restricted row, except the aggregates an aggregate-restricted query of the same request already read whole, and SHALL merge the result, de-duplicated by label set, before the parse. Completion rows SHALL feed the owner vote and the storage inventory and SHALL NOT be a source of claims. Completion waits on phase 1 alone. Under an `svm=` root the build SHALL additionally complete, once phase 2 has returned, every `(ONTAP cluster, aggregate)` pair a phase-2 row names that neither an aggregate-restricted query nor the first completion read whole: the aggregate and SVM picks are separate, so a claim retained through its rooted SVM can land on an aggregate only phase 2 saw. A request with no `svm=` root issues no completion query.

**Roots stay drawable.** The aggregate, controller and policy Harvest families are unrestricted in every build, so a rooted component with no claim on it is materialised from those families exactly as it is today. A phase 1 that returns nothing therefore still draws its roots; it draws no path through them, which is the same outcome an unrestricted read gives for a component no claim reaches.

**`node=` roots.** A `node=` root SHALL NOT disable the restriction when the request also carries an `ontap_cluster=`, `aggr=` or `svm=` root: the projection ANDs it with those roots, so every retained path is one the restriction keeps; an ONTAP controller it names is materialised from the unrestricted controller and aggregate families; and a Kubernetes node it names enters the Kubernetes-node read as a root. A request whose only storage-side roots are `node=` SHALL read this family unrestricted and single-phase: `node=` also names a Kubernetes node, and a path retained through that node is found from its pods, not from the filer.

**Match modes that SHALL NOT restrict.** Phase 2 expresses the `exact` and `suffix` volume-match modes exactly. A build configured with `contains` or `regex` SHALL read the family unrestricted and single-phase, because those modes cannot be rendered into an anchored alternation without changing their semantics.

**Mechanics.** Each phase's restriction SHALL be chunked deterministically under the byte budget shared with the QoS workload read, one query per chunk, results merged in chunk order, each chunk issued under the bare family name so self-metrics and span dimensions carry one value per family however many queries a build issues. A single value SHALL always be issued even when it alone exceeds the budget. Each phase-1 query chunks ONE of its alternations and repeats the `cluster` alternation verbatim in every chunk; the repeated matcher SHALL be charged against the budget at its RENDERED length, escaping and wrapper included, so the budget bounds what is actually sent. A failed chunk of any phase — phase 1, owner completion or phase 2 — SHALL fail the build, as "Storage build fails closed on upstream query errors" requires. The per-family series tally SHALL report the merged series count under the one family name.

**A restriction SHALL be bounded, or not applied.** `ontap_cluster=` and `aggr=` are repeatable and the request parser bounds each value's length but never their COUNT, so this is the one scope a client can inflate. When phase 1 would take more than a fixed maximum number of queries in total across its aggregate, SVM and cluster queries, the build SHALL read the family unrestricted and single-phase instead, and SHALL log that it did so. That is the read this leg performed before the restriction existed: one query, the same body, and a fan-out one request cannot enlarge. A restriction that cannot be rendered at all — every value normalising away, which an embedder filling the root sets directly can produce where the request parser cannot — SHALL take the same path. Neither reason is a query error, and neither SHALL fail a build.

**Coverage signalling.** Under a restricted read the join-coverage signal that counts loaded claims resolving no aggregate SHALL count only the claims that matched at least one volume-label series. A claim that matched none is outside the rooted components — the request did not ask about it — and counting it would fire the signal on nearly the whole estate; a claim that DID match a series and still resolved no aggregate is a FlexGroup, which is a genuine coverage miss under either read and SHALL still be counted. An unrestricted read SHALL count both, unchanged.

`GET /v1/graph` carries no roots and SHALL always read this family unrestricted and single-phase.

#### Scenario: Aggregate root restricts the topology read

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00` against a filer holding 20000 volume-label series of which 834 carry `aggr="aggr00"`
- **THEN** phase 1 issues the family restricted to `aggr=~"aggr00"` and loads those 834 series, phase 2 issues it restricted to the derived tokens of the claims that matched, no other filer volume is loaded, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: A request with no storage-side root is unrestricted

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/orders-0`
- **THEN** the volume-label family is issued once with no matcher of any kind, no second phase is issued, and the read is exactly the read a build performs today

#### Scenario: Cluster and aggregate roots narrow one query

- **WHEN** a client sends `?az=zone-a&env=prod&ontap_cluster=ontap-prod&aggr=aggr00`
- **THEN** phase 1 issues one query carrying both `cluster=~"ontap-prod"` and `aggr=~"aggr00"`, a volume on an `aggr00` of a different filer is not loaded, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: Cluster root alone restricts by cluster

- **WHEN** a client sends `?az=zone-a&env=prod&ontap_cluster=ontap-prod`
- **THEN** phase 1 issues the family restricted to `cluster=~"ontap-prod"` alone, every SVM of that filer is loaded and is a root, and the body is byte-identical to the unrestricted body

#### Scenario: A clone on a lexically-smaller aggregate keeps its pick

- **WHEN** a claim's derived token matches FlexVol `trident_pvc_x` on `aggr09` and a clone `snap_trident_pvc_x` on `aggr00`, and the request roots at `aggr=aggr09`
- **THEN** phase 2 loads both series, the aggregate pick resolves to `aggr00` exactly as an unrestricted read resolves it, the claim is not retained by the `aggr09` root, and the body is byte-identical to the unrestricted body

#### Scenario: A cross-filer FlexVol-name collision keeps its pick

- **WHEN** one FlexVol name exists on `ontap-prod` and on `ontap-lab`, the claim's token matches both, and the request roots at `aggr=` on `ontap-prod` while `ontap-lab` sorts first
- **THEN** phase 2 loads the `ontap-lab` series too, the pick resolves to the lexically-smallest `(ontap-cluster, aggr)` exactly as an unrestricted read resolves it, and the body is byte-identical to the unrestricted body

#### Scenario: A pod root composes with an aggregate root

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00&pod=shop/orders-0`
- **THEN** phase 1 is restricted to `aggr=~"aggr00"`, the pod root joins the pod scope as it does today, and the body is byte-identical to the unrestricted body

#### Scenario: A non-default match mode opts out

- **WHEN** the deployment configures the `contains` volume-match mode and a client sends `?az=zone-a&env=prod&aggr=aggr00`
- **THEN** the volume-label family is issued unrestricted and single-phase and the body is byte-identical to the body a build produces today

#### Scenario: A rooted aggregate with no claim is still drawn

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00`, no volume on `aggr00` matches any loaded claim, and the aggregate and controller families name `aggr00` and its owner
- **THEN** phase 2 is not issued, and the body contains `aggr00` with its health and usage attributes and its owning controller, and no path through them

#### Scenario: Takeover ownership survives the restriction

- **WHEN** a rooted aggregate's volume-label series disagree on the owning `node` because a takeover happened inside the window
- **THEN** phase 1 loads every series of that aggregate, the vote resolves to the lexically-smallest non-empty `node` exactly as an unrestricted read resolves it, and the `node-aggr` tier names that controller

#### Scenario: Join-coverage signal counts only what the restriction still explains

- **WHEN** a restricted build loads claims of which one joins, one matches a FlexGroup series carrying no `aggr`, and the rest match no series at all
- **THEN** the join-coverage warning counts the FlexGroup claim and not the claims that matched nothing, and the same estate read unrestricted counts both

#### Scenario: An unbounded root set reads the family unrestricted

- **WHEN** a client sends thousands of `aggr=` values, enough that the restriction would take more queries than the maximum
- **THEN** the build issues one unrestricted `volume_labels` query and no second phase, logs that the roots did not yield a bounded restriction, returns 200, and the body is byte-identical to the body the restriction would have produced

#### Scenario: A cluster-only restriction does not re-read what it already has

- **WHEN** a client sends `?az=zone-a&env=prod&ontap_cluster=ontap-prod` and a matched claim also has a candidate on `ontap-lab`
- **THEN** phase 2 excludes `ontap-prod` from its own selector, loads the `ontap-lab` candidate, and the aggregate pick is the one the whole-filer read makes; a request that also names an `aggr=` root excludes no cluster, because phase 1 then read only part of one

#### Scenario: The QoS scope narrows with the topology read

- **WHEN** a restricted build's merged volume-label result names 60 FlexVols that loaded claims matched, against 1200 in an unrestricted build
- **THEN** the QoS workload queries restrict `volume` to those 60 names, are chunked under the same budget, and every I/O measurement on a retained path is identical to the unrestricted build's

#### Scenario: A failed restricted chunk fails the build

- **WHEN** one phase-1, owner-completion or phase-2 chunk fails with an upstream error while every other query succeeds
- **THEN** the build returns an error, the request is mapped as an upstream failure naming `volume_labels`, the failure is counted on `kube_state_graph_upstream_query_failures_total{query="volume_labels"}`, and no body is returned

#### Scenario: An SVM root restricts the topology read

- **WHEN** a client sends `?az=zone-a&env=prod&svm=svm_shop` and `svm_shop` has volumes on `aggr03` and `aggr07`
- **THEN** phase 1 issues the family restricted to `svm=~"svm_shop"`, owner completion issues it restricted to `aggr03` and `aggr07` on their ONTAP cluster, every other volume of the filer is not loaded, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: An SVM root and an aggregate root are unioned

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00&svm=svm_shop` and `svm_shop` holds a claim on `aggr09`
- **THEN** phase 1 issues one aggregate-restricted and one SVM-restricted query, owner completion reads `aggr09` but not `aggr00`, the `aggr09` claim is retained through its SVM, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: Owner completion preserves the takeover vote

- **WHEN** a client roots at `svm=svm_shop`, the `svm_shop` volumes on `aggr09` all name controller `ontap-prod-02`, and another SVM's volume on `aggr09` names `ontap-prod-01` because a takeover happened inside the window
- **THEN** owner completion loads every `aggr09` series, the vote resolves to `ontap-prod-01` exactly as an unrestricted read resolves it, and the `node-aggr` tier names that controller

#### Scenario: A node root composes with an aggregate root

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00&node=ontap-prod-01`
- **THEN** phase 1 is restricted to `aggr=~"aggr00"`, the node root still resolves against both the ONTAP controller and the Kubernetes node of that name, and the body is byte-identical to the body an unrestricted read produces

#### Scenario: A node-only root does not restrict the read

- **WHEN** a client sends `?az=zone-a&env=prod&node=ontap-prod-01`
- **THEN** the volume-label family is issued unrestricted and single-phase, and the build is not in hub mode

## MODIFIED Requirements

### Requirement: Storage-flow graph endpoint

The server SHALL expose `GET /v1/storage-graph` returning a storage-flow graph for a caller-specified `[start, end]` window, in the same `{ apiVersion, clusters, elements: { nodes, edges } }` Cytoscape.js shape as `GET /v1/graph`. `start` and `end` SHALL be required and validated exactly as for `/v1/graph` (RFC 3339 or Unix seconds; `end > start`; `missing_start` / `missing_end` / `invalid_start` / `invalid_end` / `invalid_range`). The endpoint SHALL sit behind the same API-key authentication, the same per-build timeout (`--build-timeout` → 504 `timeout`), and the same upstream / outside-retention / cancelled error mapping as `/v1/graph`, and SHALL be described in the served OpenAPI document.

`az` and `env` SHALL be **required** and **single-valued**: a request lacking either SHALL be rejected 400 with `reason: "missing_az"` / `reason: "missing_env"`, and a request repeating either SHALL be rejected 400 with `reason: "invalid_scope"`. Outside hub mode (see "Storage-side roots read the claim chain through the volume hub"), the two values SHALL be pushed upstream exactly as the `/v1/graph` selector-level `az` / `env` dimensions are (matchers on the Kubernetes families, backend selection for Harvest), so the body describes one estate. In hub mode — a request carrying an `ontap_cluster=`, `aggr=` or `svm=` root whose Harvest topology read is restricted — `az` SHALL still select the `harvest` backends, but neither value SHALL be rendered as a matcher on, or select the backends of, any kube-state-metrics, kubelet or `ALERTS` query: a filer shared across zones or environments is drawn with the claims of every zone and environment that use it. The two values stay required and single-valued in both modes. `cluster` and `namespace` SHALL be accepted as optional, repeatable narrowing filters with `/v1/graph` semantics. `prune` SHALL be ignored (this endpoint applies its own reachability projection, never the connectivity prune). `edge_type` — withdrawn from `/v1/graph` as well, so no longer a parameter of any endpoint — and any other unknown parameter SHALL be ignored without error, whatever value they carry.

The top-level `clusters` array SHALL list the Kubernetes cluster identities present on emitted `pod` / `node` / `pvc` nodes and never an ONTAP cluster name.

#### Scenario: Successful request

- **WHEN** a client sends `GET /v1/storage-graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z&az=zone-a&env=prod&aggr=aggr1`
- **THEN** the server returns 200 with a body containing exactly `apiVersion: "v1"`, `clusters`, and `elements` with `nodes` and `edges`

#### Scenario: Missing az

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&env=prod`
- **THEN** the server returns 400 with `reason: "missing_az"`

#### Scenario: Repeated env

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&env=prod&env=dev`
- **THEN** the server returns 400 with `reason: "invalid_scope"` and a message naming `env`

#### Scenario: Zone and environment reach upstream

- **WHEN** a client sends `?az=zone-a&env=prod` with no `ontap_cluster=`, `aggr=` or `svm=` root
- **THEN** every kube-state-metrics, kubelet and `ALERTS` query carries `<az-key>="zone-a",<env-key>="prod"`, every Harvest query is issued only to the `harvest` backends whose `zones` include `zone-a` (or catch-alls) with no matcher, and no series from another zone or environment contributes to the body

#### Scenario: Unauthenticated request rejected when keys configured

- **WHEN** API keys are configured and a client sends `GET /v1/storage-graph` without `X-API-Key`
- **THEN** the server returns 401 exactly as `/v1/graph` would

#### Scenario: Graph-only and withdrawn parameters are ignored

- **WHEN** a client sends `GET /v1/storage-graph?start=...&end=...&az=zone-a&env=prod&prune=false&edge_type=not-a-type`
- **THEN** the server returns 200 with a body byte-identical to the same request without `prune` and `edge_type` — neither value is validated, and neither narrows or widens the body

#### Scenario: A storage-exclusive root reads claims in every zone

- **WHEN** two backends serve `ksm` with `zones: [zone-a]` and `zones: [zone-b]`, one backend serves `harvest` with `zones: [zone-a]`, filer `ontap-prod` holds FlexVol `trident_pvc_ab12` whose claim lives in a `zone-b` cluster, and a client sends `?az=zone-a&env=prod&ontap_cluster=ontap-prod`
- **THEN** every Harvest query is issued to the `zone-a` `harvest` backend only, every kube-state-metrics and `ALERTS` query carries no `<az-key>` / `<env-key>` matcher and is issued to both `ksm` backends, the body contains the `zone-b` claim's complete path, and `clusters` lists that claim's `zone-b` cluster identity

### Requirement: Storage build reads only what the body draws

The storage-graph build SHALL NOT issue the following kube-state-metrics families: `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels`, `kube_service_annotations`. The body contains no `service` or `external` node and no `service-selects-pod` edge, so the four service-side families can contribute nothing to it, and it does not carry `containers` (see "Attributes and compound groups carry over"). A family the build does not issue SHALL be absent from the build's per-family series tally, never reported as zero.

Every other family the `/v1/graph` topology read issues falls into one of two classes. The **unrestricted** class is read exactly as `/v1/graph` reads it, under the request-scoped matchers alone: outside hub mode, the claim-binding family `kube_pod_spec_volumes_persistentvolumeclaims_info` (the root every by-reference scope is computed from — nothing precedes it that could restrict it) and `kube_persistentvolumeclaim_info`, `kube_persistentvolumeclaim_annotations` and the two kubelet volume-stats families (one series per claim the binding family already names, so a restriction could not select fewer than the binding read already does) — in hub mode these five families are claim-keyed by-reference reads instead, as "Storage-side roots read the claim chain through the volume hub" defines; every Harvest family except the volume-label topology family under a storage-side-rooted request (a storage root SHALL be drawable whether or not a claim reaches it, and an SVM is named by `volume_labels` alone; `volume_labels` alone leaves this class when the request names the components it should read, per "Storage-side roots narrow the Harvest topology read per component"); and `ALERTS` (already restricted to firing alerts). The **by-reference** class — the two pod families, the four Kubernetes-node families and the eight controller families below — is read restricted to the object names the families read before it actually carry. A by-reference family whose scope is empty SHALL NOT be issued at all and SHALL be absent from the tally; when issued, its tally entry is the count of series its restriction matched.

Every by-reference restriction SHALL be a sorted, de-duplicated, anchored alternation on the family's own identity label, **composed with** the family's fixed, request-invariant selector where it has one (`type=~"ExternalIP|InternalIP"`, `condition="Ready"`, `owner_kind="CronJob",owner_is_controller="true"`, `annotation_argocd_argoproj_io_tracking_id!=""`) and with the request-scoped matchers the family already carries — never replacing either. It is derived from upstream data and from the request's roots, not from a selector-level dimension, so the request-scoped selector table is unchanged. Every restriction SHALL be **chunked deterministically** under one byte budget shared with the QoS workload read, one query per chunk per family, results merged in **chunk order**, every chunk issued under the bare family name for self-metrics and span dimensions, and a single name SHALL always be issued even when it alone exceeds the budget. A chunk error of any of these families SHALL fail the build, as "Storage build fails closed on upstream query errors" requires — including `kube_replicaset_annotations` and `kube_job_annotations`, which `/v1/graph` reads as degrading families. Caller-originated cancellation SHALL fail the request whatever the family.

**Pods.** The build SHALL read `kube_pod_info` and `kube_pod_owner` restricted to the union of (a) the `pod` names of every claim-binding series it loaded (in hub mode, only the series whose claim is one the hub loaded), (b) the pod-name segment of every `pod=<namespace>/<pod-name>` root the request carries, and (c) the pod names the application recovery of "Application roots recover their pods before the pod read" returned. Under an `application=` root, (a) SHALL be narrowed to the binding pods of claims that are own-annotated with a root Application or mounted by a pod in (b) ∪ (c) — every mounter of such a claim included — as that requirement defines. The pod read SHALL wait on the claim-binding family and — when the request carries an `application=` root — on the recovery and on `kube_persistentvolumeclaim_annotations` alone, and SHALL NOT wait on families it does not read. Because a root has to be in the restriction to be loaded, the storage build SHALL receive the request's roots as an input. A `pod=` root that mounts no claim SHALL still be materialised, as "Roots are always materialised when the upstream knows them" requires; so SHALL every recovered pod that resolves a root Application. When the pod scope is empty — no (possibly narrowed) claim-binding pod, no `pod=` root, and no recovered name — no pod query is issued, no controller query is issued, and the body holds no complete path: it contains only the roots the storage side materialised and any `node=` root the Kubernetes-node read below still draws.

**Kubernetes nodes.** The build SHALL read `kube_node_info`, `kube_node_status_addresses`, `kube_node_labels` and `kube_node_status_condition` restricted on `node` to the union of (a) the `node` label of every pod the pod read loaded and (b) every `node=` root the request carries. The node read SHALL wait on the pod read alone. A `node=` root naming a Kubernetes node that no loaded pod is scheduled on SHALL still be materialised, so a request with node roots and no mounting pod issues the node families for the roots alone. An unscheduled pod contributes no name.

**Controllers.** The build SHALL read the eight controller families in two stages, each restricted to the owner names the loaded pods carry. From every loaded `kube_pod_owner` series with `owner_is_controller="true"` and a non-empty `owner_name`, the first stage groups the names by `owner_kind` and issues, restricted on the family's identity label: `kube_replicaset_owner` and `kube_replicaset_annotations` on `replicaset` for the `ReplicaSet` names; `kube_job_owner` and `kube_job_annotations` on `job_name` for the `Job` names; `kube_statefulset_annotations` on `statefulset` for the `StatefulSet` names; `kube_daemonset_annotations` on `daemonset` for the `DaemonSet` names. The second stage, waiting on the first alone, issues `kube_deployment_annotations` on `deployment` for the union of the pods' direct `Deployment` owner names and the `owner_name` of every loaded `kube_replicaset_owner` series naming a `Deployment` owner, and `kube_cronjob_annotations` on `cronjob` for the union of the pods' direct `CronJob` owner names and the `owner_name` of every loaded `kube_job_owner` series. A kind no loaded pod is owned by, and a second-stage name set no first-stage series populated, SHALL issue no query for its families. The first stage SHALL wait on the pod read alone. An owner kind the reader resolves no Application for (`ReplicationController`, `Node`, a custom-resource controller) contributes no name and issues nothing.

The by-reference reads SHALL be **output-preserving**: the reader consults a node family only at the `(cluster, node)` of a loaded pod's node or a node root, consults `kube_replicaset_owner` only at the ReplicaSet names of loaded pods, consults the annotation families only at the resolved owner of a loaded pod (a Deployment recovered from its ReplicaSet, a CronJob recovered from its Job), and consults `kube_job_owner` only at the Job names of loaded pods — so every consulted key names an object in its scope, and the body SHALL be byte-identical to the body an unrestricted read would produce. Names are unique within a namespace (controllers, pods) or a cluster (nodes) only, so a restriction MAY admit a same-named object from another namespace or cluster inside the request's selectors; such a series is keyed under its own cluster and namespace, is consulted by no loaded pod, and SHALL leave the body unchanged.

#### Scenario: Pod read is restricted to mounting pods and roots

- **WHEN** a build for `?az=zone-a&env=prod&pod=shop/web-0` loads claim-binding series naming pods `orders-0`, `orders-1` and `catalog-0` while the estate holds 40000 pods
- **THEN** every issued `kube_pod_info` and `kube_pod_owner` query restricts `pod` to exactly `{catalog-0, orders-0, orders-1, web-0}` alongside `<az-key>="zone-a",<env-key>="prod"`, no series for any other pod is fetched, and the body is byte-identical to the body an unrestricted pod read would produce

#### Scenario: Pod read is restricted to mounting pods, roots and recovered pods

- **WHEN** a build for `?az=zone-a&env=prod&application=checkout` loads claim-binding series naming pods `orders-0` and `catalog-0` (unrelated claims, unannotated), the recovery returns `web-7d9f-abc` and `orders-0`, and `catalog-0`'s claim is mounted by nobody else
- **THEN** every issued `kube_pod_info` and `kube_pod_owner` query restricts `pod` to exactly `{orders-0, web-7d9f-abc}`, and the body is byte-identical to the body an unrestricted pod read would produce for the same root

#### Scenario: Empty scope issues no pod query

- **WHEN** a build for `?az=zone-a&env=prod&aggr=aggr1` loads no claim-binding series
- **THEN** no `kube_pod_info`, `kube_pod_owner`, Kubernetes-node or controller query is issued, the per-family tally carries none of those families, and the body contains `aggr1`, its owning controller and no other node

#### Scenario: Claimless pod root is still drawn

- **WHEN** a client sends `?az=zone-a&env=prod&pod=shop/web-0` and `shop/web-0` mounts no claim
- **THEN** the pod's name is in the restriction, `kube_pod_info` names it, and the body contains that pod with its ordinary attributes and compound parent and no edges

#### Scenario: Skipped families never reach the upstream

- **WHEN** the upstream rejects every `kube_pod_container_info` query with a series-limit error and a client sends any `/v1/storage-graph` request
- **THEN** the storage build issues no `kube_pod_container_info`, `kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels` or `kube_service_annotations` query, `kube_state_graph_upstream_query_failures_total` does not move, and the request returns 200

#### Scenario: Cross-namespace name collision is harmless

- **WHEN** claim-binding series name `shop/web-0` and the estate also holds a claimless `platform/web-0`
- **THEN** `platform/web-0` may be fetched but does not appear in the body, which is byte-identical to the body an unrestricted pod read would produce

#### Scenario: A pod chunk failure fails the build

- **WHEN** the restriction is split into three chunks and the second `kube_pod_info` chunk fails with an upstream error
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

#### Scenario: Node read is restricted to the pods' nodes and node roots

- **WHEN** a client sends `?az=zone-a&env=prod&node=n9` and the loaded pods are scheduled on Kubernetes nodes `n1` and `n2` while the estate holds 5000 nodes
- **THEN** every issued `kube_node_info`, `kube_node_status_addresses`, `kube_node_labels` and `kube_node_status_condition` query restricts `node` to exactly `{n1, n2, n9}` alongside its fixed selector and the request's matchers, no series for any other node is fetched, and the body is byte-identical to the body an unrestricted node read would produce

#### Scenario: Kubernetes node root with no mounting pod is still drawn

- **WHEN** a client sends `?az=zone-a&env=prod&node=n9`, no claim-binding series is loaded, and `kube_node_info` names Kubernetes node `n9`
- **THEN** the four node families are issued restricted to `{n9}`, no pod or controller query is issued, and the body contains the node `n9` with its ordinary attributes and no edges

#### Scenario: Controller scope cross-namespace collision is harmless

- **WHEN** a loaded pod in `shop` is owned by StatefulSet `db` and the estate also holds StatefulSet `platform/db` carrying a different tracking-id
- **THEN** `kube_statefulset_annotations{statefulset="db"}` may return both series, the pod resolves the `shop/db` Application only, and the body is byte-identical to the body an unrestricted read would produce

#### Scenario: An annotation chunk failure fails the build

- **WHEN** one `kube_job_annotations` or `kube_replicaset_annotations` chunk fails with an upstream error while every other query succeeds
- **THEN** the build returns an error, the request is mapped as an upstream failure naming that family, and no body is returned

#### Scenario: Hub mode withholds the claim families from the first wave

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00` and the build is in hub mode
- **THEN** no `kube_persistentvolumeclaim_info`, claim-binding, `kube_persistentvolumeclaim_annotations` or kubelet volume-stats query is issued before phase 1 of the volume-label read returns, each is issued only in the claim-keyed form the hub defines, and none is issued as a whole-zone read

### Requirement: Application roots compose with the Harvest restriction

An `application=` root SHALL NOT disable the restricted volume-label read of "Storage-side roots narrow the Harvest topology read per component": it is a workload-side root, the projection ANDs the workload roots with the storage roots, so every path an `ontap_cluster=` / `aggr=` restriction keeps is one the application root can still retain, and the body is byte-identical to the unrestricted body exactly as it is for `pod=`. In hub mode the recovery SHALL be issued under the hub's relaxed request matchers, like every other kube-state-metrics query of the build, so an Application's pods are recovered in every zone and environment.

#### Scenario: An application root composes with an aggregate root

- **WHEN** a client sends `?az=zone-a&env=prod&aggr=aggr00&application=checkout`
- **THEN** phase 1 of the volume-label read is restricted to `aggr=~"aggr00"`, the application recovery runs with no `<az-key>` / `<env-key>` matcher, the body holds only the `checkout` paths on `aggr00`, and it is byte-identical to the body an unrestricted volume-label read produces

## REMOVED Requirements

### Requirement: Storage-side roots restrict the Harvest topology read

**Reason**: Replaced by "Storage-side roots narrow the Harvest topology read per component". `svm=` roots are now restricted (with an owner-completion read that keeps the owning-controller vote over every series of an aggregate), and a `node=` root no longer disables the restriction beside an `ontap_cluster=` / `aggr=` / `svm=` root. The four scenarios that pinned the old opt-outs ("An SVM root does not restrict the read", "An SVM root disables an aggregate root", "A node root does not restrict the read", "A node root disables an aggregate root") describe behaviour this change withdraws, so the requirement is replaced rather than modified.

**Migration**: No client action. A request rooted at `svm=`, or at `node=` beside a storage-exclusive root, returns the same body from a smaller Harvest read. A request rooted only at `node=` is read exactly as before.
