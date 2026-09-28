## ADDED Requirements

### Requirement: Suffix-only PV-name-to-FlexVol-name derivation

The builder SHALL bridge the Kubernetes PersistentVolume name and the ONTAP FlexVol name by deriving a **match token** from each claim's resolved PV name and comparing that token against the stock `volume` label of the Harvest series. ONTAP volume names admit only letters, digits and `_`, so a `volume` value can never equal a `pvc-<uuid>` PV name and the two SHALL NOT be compared for equality directly.

Derivation is an **ordered list of regular-expression rewrite rules**, each a `(pattern, replacement)` pair, applied to the PV name in declaration order with every match replaced. The default list is exactly one rule replacing `-` with `_`. The default SHALL NOT prepend a storage prefix: the prefix is per-backend configurable in the provisioner and the suffix comparison below does not need it.

A `volume` value SHALL match a claim **iff it ends with the claim's token**. There is no other comparison and no setting that selects one: the join SHALL be the same on every build of `GET /v1/graph` and `GET /v1/storage-graph`. The server SHALL define no match-mode flag — passing `--netapp-volume-match-mode` fails startup as an unknown flag — and SHALL NOT read `KSG_NETAPP_VOLUME_MATCH_MODE`.

Suffix is the one comparison because a FlexVol provisioned from a claim is named by prefixing the transformed PV name, so it resolves without the deployment declaring the prefix; because it rejects a derived volume whose name extends past the PV name (a clone or snapshot suffixed after it); and because it is the one comparison `GET /v1/storage-graph` can both render as an anchored alternation branch (`.*<token>`) and invert from a FlexVol name back to candidate PersistentVolume names, which that endpoint's by-reference read requires (`storage-graph-api`, "Every root kind is tracked from its own tier to its claims"). A FlexVol named exactly the token matches, since a value ends with itself.

Derivation SHALL run **once per claim**, never per Harvest series, so one PV name yields exactly one token and no collision rule over rewritten values is required.

Every rewrite pattern SHALL be validated at startup and an invalid pattern SHALL be a fatal configuration error, never a silent fallback to the default. A claim whose PV name is empty SHALL derive no token and SHALL NOT be matched against any series.

The derivation SHALL be a pure function of the PV name and the configuration, so the same estate and the same configuration produce byte-identical output across rebuilds.

#### Scenario: Default derivation resolves a stock Trident FlexVol

- **WHEN** a PVC resolves `volumename="pvc-9f3a-11d0"`, the configuration is left at its defaults, and a `volume_labels` series carries `volume="trident_pvc_9f3a_11d0"`
- **THEN** the claim matches that series, its `pvc-to-netapp-aggr` edge is emitted, and no relabel rule was required of the deployment

#### Scenario: A clone whose name extends past the PV name never matches

- **WHEN** a PVC resolves `volumename="pvc-9f3a"` and `volume_labels` carries both `volume="trident_pvc_9f3a"` and `volume="trident_pvc_9f3a_clone"`
- **THEN** only `volume="trident_pvc_9f3a"` matches the claim, and the clone contributes neither an aggregate nor any I/O measurement

#### Scenario: A FlexVol named exactly the token matches

- **WHEN** a backend provisions with an empty storage prefix, a PVC resolves `volumename="pvc-9f3a"`, and `volume_labels` carries `volume="pvc_9f3a"`
- **THEN** the claim matches that series

#### Scenario: Custom rewrite rules replace the default

- **WHEN** the deployment configures the ordered rules `-` → `_` followed by `^` → `vol_` and a claim resolves `volumename="pvc-9f3a"`
- **THEN** the derived token is `vol_pvc_9f3a` and it matches every `volume` value ending with it

#### Scenario: Invalid pattern is fatal at startup

- **WHEN** the deployment configures a rewrite pattern that is not a valid regular expression
- **THEN** startup fails with an error naming the offending pattern, and the process does not start with the default rules substituted

#### Scenario: The match-mode setting does not exist

- **WHEN** the server is started with `--netapp-volume-match-mode=contains`
- **THEN** startup fails as it does for any unknown flag; and when `KSG_NETAPP_VOLUME_MATCH_MODE=contains` is set in the environment instead, it is not read and every claim is matched by suffix

#### Scenario: Derivation is deterministic

- **WHEN** two consecutive builds run over the same estate with the same configuration
- **THEN** every claim derives the same token and the emitted node, edge and label sets are byte-identical

## MODIFIED Requirements

### Requirement: Harvest volume-label series as the storage topology source

The builder SHALL consume the NetApp Harvest volume-object label series `volume_labels` from the same centralised VictoriaMetrics endpoint as every other series. The fixed, case-sensitive label contract it MUST carry: `cluster` (the ONTAP cluster name — NOT a Kubernetes cluster; the two namespaces never mix), `node` (the ONTAP controller currently owning the containing aggregate), `aggr` (the containing aggregate), `svm` (the serving Storage Virtual Machine), and `volume` (the ONTAP FlexVol name). It is an **info series**: its sample value SHALL be ignored entirely and only its label set consumed.

Every label in that contract is **stock Harvest output**. The builder SHALL NOT require the deployment to install a Prometheus relabel rule, and SHALL NOT read any non-stock label naming the Kubernetes PersistentVolume.

This one series is the SOLE source of the graph's storage topology — the `pvc-to-netapp-aggr` edge, the `netapp-aggr` and `netapp-node` entities, and the PVC `svm` and `aggr` labels all derive from it and from nothing else. The I/O measurements and the throughput ceiling ride on separate families (the two requirements below) and SHALL NOT contribute to any topological decision; conversely, a claim SHALL NEVER lose its storage topology because an I/O family failed to match.

The issued query SHALL read the series at the window end without `rate()`, in the same shape as every other Harvest leg. A `/v1/graph` build SHALL issue a single read carrying no restriction on `volume` — it carries no root, and the set of interesting FlexVol names is not known until this family has been read. A `/v1/storage-graph` build SHALL NOT read this family across the zone: it reads it by reference — restricted by the request's storage-side root (ONTAP cluster, aggregate, SVM or controller) where it has one, restricted on `volume` to the derived tokens of the claims the build tracked, and whole for every aggregate those reads touched — merging every read before the parse. That shape and its byte-identical-body obligation are defined by the `storage-graph-api` capability's "Every root kind is tracked from its own tier to its claims" and "The tracked claims expand to both ends of the chain".

The bridge from a claim's PV name to this family's `volume` label is the derivation described in "Suffix-only PV-name-to-FlexVol-name derivation". The graph inherits three blind spots from it: a FlexVol whose name does not embed the claim's PV name under the configured derivation never joins (its claim's `svm` and `aggr` are absent and no edge is drawn); the Trident "economy" drivers pack many claims into one shared FlexVol, so no per-claim series exists at all; and a FlexGroup volume spans aggregates, so its series carries no single usable `aggr` label (no aggregate edge can be drawn and its PVC carries no `aggr` label — see the join-coverage requirement).

The family is OPTIONAL. When it is absent from the window — the normal case for a deployment without NetApp Harvest — the builder SHALL produce a valid graph with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` or `aggr` labels; PVC `volumename` labels are unaffected and the build SHALL NOT fail.

#### Scenario: Volume label series consumed for its labels only

- **WHEN** the builder issues the `volume_labels` query for a window
- **THEN** the query references the bare series evaluated at the window end, does not wrap it in `rate()`, carries no `volume` restriction on a `/v1/graph` build, and the resolver derives the aggregate, owning controller, and SVM from the matched series' labels while its sample value plays no part in any output

#### Scenario: Stock Harvest output joins without a relabel rule

- **WHEN** the upstream carries `volume_labels` exactly as stock Harvest emits it, with no deployment-installed relabel rule and no label naming the Kubernetes PersistentVolume
- **THEN** claims whose derived tokens match the `volume` label still resolve their aggregate, controller and `svm`, their PVCs carry the `aggr` label, and their `pvc-to-netapp-aggr` edges are emitted

#### Scenario: Harvest absent entirely

- **WHEN** the upstream contains topology series but no `volume_labels` series for the window
- **THEN** the build completes successfully with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` or `aggr` labels, while PVC `volumename` labels still resolve from `kube_persistentvolumeclaim_info`

#### Scenario: I/O families present without the label series

- **WHEN** the upstream carries QoS workload series whose `volume` matches a claim's derived token but no `volume_labels` series matches it
- **THEN** no `pvc-to-netapp-aggr` edge is emitted for that claim, its PVC carries no `aggr` label, no aggregate or controller is materialised from the QoS series, and the build does not fail

### Requirement: Scoped and batched QoS workload read

The six QoS workload queries SHALL be issued **only for FlexVol names the volume-label family has already matched**, never as an unfiltered read of every workload on the filer. ONTAP collects a QoS workload for every volume, the overwhelming majority of which back no claim, and the builder consults the QoS families only for claims that already resolved an aggregate — so an unfiltered read fetches series that are provably discarded before they are read.

The scope SHALL be the sorted, de-duplicated set of `volume` values matched by the loaded claims' derived tokens, and each issued query SHALL restrict `volume` to that set with an anchored alternation. That restriction SHALL be the query's ONLY matcher — volume granularity is a reader rule, not a selector one (see "Harvest QoS workload series as the I/O source"). Because the scope holds FlexVol names that already matched, the query-layer restriction is **exact**: the suffix comparison of the derivation requirement is applied once, in the builder, and SHALL NOT reach the query layer.

The scope SHALL be computed from the claim-info family and the volume-label family alone; the QoS read SHALL wait on those two families only, and SHALL NOT wait on families it does not read.

When the scope is **empty** — no claim matched any volume-label series, or the volume-label family was absent or degraded — the builder SHALL issue **no QoS workload query at all**, mirroring the rule that a selector loading no pods or services issues no service-graph queries.

Because the alternation grows with the number of matched volumes and upstream installations cap query length, the scope SHALL be **chunked deterministically** and one query issued per chunk per family. A single volume name SHALL always be issued even when it alone exceeds the chunk budget: dropping it would remove a claim's measurements silently. Chunk results SHALL be merged in **chunk order**, not completion order, so that the summed I/O values are a pure function of the scope and independent of upstream timing.

Each chunk SHALL degrade independently and SHALL NOT fail the build: a failed chunk costs I/O measurements only for the claims whose volumes it carried, and the deterministic chunking makes which claims those are a pure function of the scope. The `pvc-to-netapp-aggr` edges, the aggregate and controller entities, and the PVC `svm` labels are unaffected by any QoS chunk outcome.

#### Scenario: QoS read is restricted to matched volumes

- **WHEN** a build loads 3 claims that match `volume` values `v_a`, `v_b` and `v_c` while the filer carries 40000 other QoS workloads
- **THEN** every issued QoS query restricts `volume` to exactly `{v_a, v_b, v_c}` and carries no other matcher, and no series for any other workload is fetched

#### Scenario: No matched volumes issues no QoS query

- **WHEN** a build reads a `volume_labels` vector in which no series matches any loaded claim's derived token
- **THEN** no QoS workload query is issued at all, every claim's outcome is unchanged, and the build succeeds

#### Scenario: Volume-label family absent issues no QoS query

- **WHEN** the volume-label family is absent from the window or its read degraded
- **THEN** no QoS workload query is issued, no `pvc-to-netapp-aggr` edge is emitted, and the build succeeds

#### Scenario: Large scope is chunked and merged deterministically

- **WHEN** the matched scope is large enough to exceed the chunk budget and is split across several queries per family
- **THEN** the merged result is identical to a single unchunked read of the same scope, and two consecutive builds over the same estate produce byte-identical I/O values

#### Scenario: One failed chunk costs only its own claims

- **WHEN** one chunk of one QoS family fails while every other chunk succeeds
- **THEN** the build succeeds, claims carried by the successful chunks keep their `metrics`, claims carried by the failed chunk emit their edge with no `metrics` key and are counted by the I/O-coverage signal, and no claim loses its aggregate, controller or `svm`

### Requirement: PVC-to-NetApp-aggregate edge join

For every PVC entity whose resolved PV name (`volumename`) is non-empty, the builder SHALL derive that PV name into a match token and match it against the `volume` label of the Harvest `volume_labels` series by the suffix comparison of "Suffix-only PV-name-to-FlexVol-name derivation". On a match with a **non-empty `aggr` label** it SHALL emit one directed `pvc-to-netapp-aggr` edge from the PVC node to the NetApp aggregate node `netapp/<ontap-cluster>/aggr/<aggr>` derived from the same matched series' `cluster` and `aggr` labels — no separate topology query is issued. The edge is a pure function of this one family: whether it is drawn SHALL NOT depend on the QoS families, which only decide what it carries. The join is rooted at the PV name alone (CSI-provisioned PV names are UUID-derived, so cross-cluster collisions are not a practical concern).

The edge SHALL carry empty `labels` (`{}`), a deterministic UUIDv5 `id` (canonical input `<type>|<source>|<target>`), and SHALL de-duplicate by `(pvc, netapp-aggr)`. When matched series disagree on the containing aggregate for one claim — including when several distinct FlexVol names end with the token (the same FlexVol name on a second filer, or a name carrying a different prefix) — the builder SHALL pick deterministically the lexically-smallest `(ontap-cluster, aggr)` pair, so the emitted edge set is byte-stable across rebuilds, independent of vector order. A PVC with no resolved `volumename` SHALL emit no `pvc-to-netapp-aggr` edge. A matched series whose `aggr` label is **empty** (the FlexGroup shape) SHALL emit no edge and SHALL be counted by the join-coverage signal.

#### Scenario: Joined claim emits the edge

- **WHEN** PVC `cluster-alpha/db/data-mongo-0` resolves `volumename="pvc-9f3a"` and a `volume_labels` series carries `volume="trident_pvc_9f3a"`, `cluster="ontap-prod"`, `node="ontap-prod-01"`, `aggr="aggr1"`
- **THEN** the graph contains a directed `pvc-to-netapp-aggr` edge from `cluster-alpha/db/data-mongo-0` to `netapp/ontap-prod/aggr/aggr1` with empty `labels`

#### Scenario: PVC without a PV name emits no edge

- **WHEN** a PVC entity has no resolved `volumename`
- **THEN** no `pvc-to-netapp-aggr` edge originates from it and the build does not fail

#### Scenario: Matched series with an empty aggr label emits no edge

- **WHEN** a claim's derived token matches a `volume_labels` series whose `aggr` label is empty (a FlexGroup volume spanning aggregates)
- **THEN** no `pvc-to-netapp-aggr` edge is emitted for that claim, the claim is counted by the join-coverage signal, and the build does not fail

#### Scenario: Deterministic pick on conflicting aggregates

- **WHEN** two series matched by one claim's token report `(ontap-prod, aggr-b)` and `(ontap-prod, aggr-a)`
- **THEN** the edge targets `netapp/ontap-prod/aggr/aggr-a` (the lexically-smallest pair) deterministically across rebuilds

#### Scenario: Edge id stable across rebuilds

- **WHEN** the same `(pvc, netapp-aggr)` join is produced by two consecutive builds for the same window
- **THEN** the edge `id` (UUIDv5 over `<type>|<source>|<target>`) is byte-identical between the two builds

### Requirement: Harvest legs carry the request zone and environment

Every NetApp Harvest query the builder issues — `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency`, and `node_total_data` — SHALL carry the request's `az` and `env` matchers, in that order, ahead of any restriction of its own, and SHALL NEVER carry a `cluster` or `namespace` matcher. Those two are the only selector-level dimensions that reach Harvest. That does not forbid a scope derived from upstream data or from the request's ROOT, which is a different mechanism: on `/v1/graph` one Harvest family carries one — the `qos_*` families' `volume` alternation below — and on `/v1/storage-graph` EVERY Harvest family carries one, because that build reads each by reference (`storage-graph-api`, "Storage build reads every family by reference"). No such scope appears in the request-scoped selector table, and each is composed AFTER the `az` / `env` matchers. Harvest's `cluster` label is the **ONTAP** cluster name and never a Kubernetes cluster, so a Kubernetes `cluster` value pushed into it would match nothing; Harvest carries no `namespace` at all. **Operator precondition:** every Harvest series MUST carry the configured `az` / `env` labels (the ones kube-state-metrics carries); a series without them matches nothing under any `az` / `env` value — and `/v1/storage-graph` always carries both — so its filer silently drops out of every filtered body.

The `qos_*` families carry one selector and no other: the `volume` alternation of the scoped-read requirement. That alternation is **derived from upstream data, not from the request**: its values are FlexVol names the volume-label family already returned. It is nonetheless *influenced* by the request, because the claims whose tokens produced those names are themselves loaded under the request's selectors. This is the "narrowed by reference" principle of this capability realised at the query layer rather than only in the parse: a `cluster`, `namespace`, `az` or `env` filter reaches the QoS read solely through the claims it loads, never as a matcher on a Harvest label. On `/v1/graph`, `volume_labels` and every other Harvest query carry nothing beyond `az` / `env`. On `/v1/storage-graph`, `volume_labels` carries the root restriction (by ONTAP cluster, aggregate, SVM or controller), the token restriction of the tracked claims, or the whole-aggregate restriction of owner completion; the aggregate gauge families carry the `(ONTAP cluster, aggregate)` pairs the build reached or the root names; the controller families carry the `(ONTAP cluster, controller)` pairs the build reached or the root names; and the two fixed-policy families carry the `(ONTAP cluster, SVM)` pairs of the tracked claims — the same "narrowed by reference" principle, read from whichever end of the chain the root sits on.

These queries constitute the `harvest` query family of the `upstream-backend-routing` capability, so they MAY be served by a different upstream installation from the kube-state-metrics and kubelet legs. The family is **zone-routed**: a request's `az` values select which `harvest` backends are asked — those whose `zones` intersect the request, plus any catch-all — under the same rule as the `ksm` and `kubelet` families, and, like them, the zone is ALSO rendered as a matcher, so a catch-all backend or a store holding several zones returns the requested zone's series only. The `env` dimension has no routing counterpart and reaches Harvest as a matcher alone. Routing changes **which** installation answers a Harvest query; it changes neither the query string, the three-hop join, nor the per-hop degradation below.

Within a filtered build the storage chain is therefore narrowed **by reference**: an aggregate and its owning controller materialise only when a **loaded** claim's derived token matches a `volume_labels` series (or, in the storage-flow graph, when selected as a root), so a `cluster` or `namespace` filter reaches the NetApp graph solely through the claims it loads, and an `az` or `env` filter reaches it through the claims it loads plus its own matcher on every Harvest query (and, for `az`, the backends it selects). A filer shared across clusters, zones, or environments is one node set, reached from whichever loaded claims match it. In a `/v1/storage-graph` build rooted at a storage component (`storage-graph-api`, "Every root kind is tracked from its own tier to its claims") the direction reverses: the claims are loaded FROM the rooted volume-label rows, under the request's `az`, `env`, `cluster` and `namespace` matchers and from the backends the request's `az` selects, so a rooted FlexVol whose claim lives in another zone or environment loads no claim. Only an unfiltered build reads every zone's and environment's Harvest series; there, a FlexVol name carried by volumes in two zones or environments resolves to the lexically-smallest `(ontap_cluster, aggr)`.

#### Scenario: Cluster and namespace filters never reach Harvest

- **WHEN** a build runs with `cluster={cluster-alpha}` and `namespace={shop}` and no `az` / `env` value
- **THEN** no Harvest query carries a `cluster` or `namespace` matcher; the request carries neither `az` nor `env` and no storage-side root, so the `volume_labels`, aggregate, controller, hardware, performance and policy queries are issued exactly as in an unfiltered build; the QoS queries restrict `volume` to the FlexVol names matched by the loaded `cluster-alpha` / `shop` claims alone; and the aggregates in the response are exactly those those claims matched

#### Scenario: Zone filter reaches Harvest

- **WHEN** two backends serve `harvest`, declaring `zones: [zone-a]` and `zones: [zone-b]`, and a build runs with `az={zone-a}`
- **THEN** every Harvest query — including `node_labels` and the four `system_node` counters — is issued only to the `zone-a` backend, carrying `<az-key>="zone-a"`; a `volume_labels` series held by the `zone-b` backend is not loaded even if a loaded claim's derived token would match it

#### Scenario: Catch-all Harvest backend under a zone filter

- **WHEN** one backend serves `harvest` with no `zones` declared and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued to that backend carrying `<az-key>="zone-a"`, only `zone-a`'s `volume_labels` series are loaded, and the aggregates in the response are exactly those matched by the loaded `zone-a` claims

#### Scenario: Environment filter reaches Harvest

- **WHEN** a build runs with `env={prod}` against a single `harvest` backend holding series stamped `env="prod"` and `env="dev"`
- **THEN** every Harvest query carries `<env-key>="prod"`, only the `prod` series are loaded, and a loaded `prod` claim whose derived token matches only a `volume_labels` series stamped `env="dev"` receives no `pvc-to-netapp-aggr` edge

#### Scenario: Shared filer reached by reference from either filtered cluster

- **WHEN** claims in `cluster-alpha` and `cluster-beta` both match `netapp/ontap-prod/aggr/aggr1` and a build runs with `cluster={cluster-alpha}`
- **THEN** `netapp/ontap-prod/aggr/aggr1` and its owning `netapp-node` are materialised with a `pvc-to-netapp-aggr` edge from the `cluster-alpha` claim only; a `cluster={cluster-beta}` build materialises the same two nodes from the `cluster-beta` claim

#### Scenario: Harvest lacking the zone labels under a filter

- **WHEN** the kube-state-metrics series carry `az="zone-a"`, the Harvest series carry no `az` and no `env` label at all, and a build runs with `az={zone-a}` or `env={prod}`
- **THEN** every Harvest leg returns no rows, no `pvc-to-netapp-aggr` edge is drawn, and the build succeeds — the operator precondition above is what prevents this; the selector-coverage Warn does not name a Harvest family, because an empty Harvest read is also the normal state of a deployment without NetApp storage

#### Scenario: Harvest served by its own upstream

- **WHEN** the routing table declares one backend serving `harvest` at `http://vm-netapp.example:8428` and another serving every other family at `http://vm-k8s.example:8428`
- **THEN** every Harvest query — including each chunk of the scoped QoS read and the five node legs — is issued only to `http://vm-netapp.example:8428`, the kube-state-metrics and kubelet queries only to `http://vm-k8s.example:8428`, and the resulting `pvc-to-netapp-aggr` edges join claims read from one upstream to volumes read from the other

#### Scenario: Unfiltered build merges Harvest across backends

- **WHEN** two backends serve `harvest` for different zones and a build runs with no `az` value
- **THEN** every Harvest query is issued to both and the results are merged, so aggregates from both zones can match their claims in one graph

#### Scenario: Hub mode loads claims from the rooted rows

- **WHEN** a `/v1/storage-graph` build rooted at `ontap_cluster=ontap-prod` runs with `az={zone-a}`, `env={prod}`, and the `zone-a` `harvest` backend returns two rooted FlexVols, one whose claim lives in a `zone-a` / `prod` cluster and one whose claim lives in an `env="dev"` cluster
- **THEN** the `prod` claim is loaded and receives its aggregate and SVM exactly as an unfiltered build would give them, and the `dev` claim is not loaded, because every claim query carries `<env-key>="prod"`

## REMOVED Requirements

### Requirement: PV-name-to-FlexVol-name derivation

**Reason**: Replaced by "Suffix-only PV-name-to-FlexVol-name derivation". The `exact`, `contains` and `regex` match modes are withdrawn together with the setting that selected them: `/v1/storage-graph` reads every family by reference, which needs a comparison it can render as an anchored alternation and invert from a FlexVol name to candidate PersistentVolume names, and only `suffix` is both. The scenario "Contains mode admits what suffix mode rejects" describes behaviour this change withdraws, so the requirement is replaced rather than modified.

**Migration**: Remove `--netapp-volume-match-mode` / `KSG_NETAPP_VOLUME_MATCH_MODE` from the deployment. `exact`: no change — a FlexVol named exactly the token also ends with it. `contains`: a clone or snapshot suffixed after the PV name no longer attributes its aggregate to the claim. `regex`: express the naming through `--netapp-volume-key-rewrite` so the token is a suffix of the FlexVol name, or the claim stops joining.
