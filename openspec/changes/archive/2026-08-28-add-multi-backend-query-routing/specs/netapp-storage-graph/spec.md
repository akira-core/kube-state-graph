## MODIFIED Requirements

### Requirement: Harvest legs under request-scoped selectors

Every NetApp Harvest query the builder issues — `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, and `node_new_status` — SHALL carry **no request-scoped matcher of any kind**: not `az`, not `env`, and never `cluster` or `namespace`. Harvest's `cluster` label is the **ONTAP** cluster name and never a Kubernetes cluster, so a Kubernetes `cluster` value pushed into it would match nothing; Harvest carries no `namespace` at all; and the `az` dimension reaches Harvest through backend selection alone (below), so a Harvest series need not carry the configured `az` / `env` labels. The `qos_*` families keep their fixed `lun=""` selector, which is therefore the only selector any Harvest query carries under any request.

These queries constitute the `harvest` query family of the `upstream-backend-routing` capability, so they MAY be served by a different upstream installation from the kube-state-metrics and kubelet legs. The family is **zone-routed**: a request's `az` values select which `harvest` backends are asked — those whose `zones` intersect the request, plus any catch-all — under the same rule as the `ksm` and `kubelet` families. Unlike those families, the selected zone is NOT additionally rendered as a matcher: for Harvest the zone boundary is the store, not a label on the series. The `env` dimension has no routing counterpart and SHALL have no effect on the Harvest legs whatsoever. Routing changes **which** installation answers a Harvest query; it changes neither the query string, the three-hop join, nor the per-hop degradation below.

Within a filtered build the storage chain is therefore narrowed **by reference**: an aggregate and its owning controller materialise only when a **loaded** claim's `volumename` joins a `volume_labels` series, so a `cluster`, `namespace`, or `env` filter reaches the NetApp graph solely through the claims it loads, and an `az` filter reaches it through the claims it loads plus the backends it selects. A filer shared across clusters, zones, or environments is one node set, reached from whichever loaded claims join it. Under a catch-all `harvest` backend, or under any `env` value, the Harvest read is the whole estate and the narrowing is by reference alone; a `volume_name` carried by volumes in two zones or environments resolves to the lexically-smallest `(ontap_cluster, aggr)` — the same collision rule an unfiltered build already applies, since an unfiltered build reads every zone.

#### Scenario: Cluster and namespace filters never reach Harvest

- **WHEN** a build runs with `cluster={cluster-alpha}` and `namespace={shop}` and no `az` / `env` value
- **THEN** every Harvest query is issued exactly as in an unfiltered build (the `qos_*` families with `lun=""` only), and the aggregates in the response are exactly those joined by the loaded `cluster-alpha` / `shop` claims

#### Scenario: Zone filter reaches Harvest

- **WHEN** two backends serve `harvest`, declaring `zones: [zone-a]` and `zones: [zone-b]`, and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued only to the `zone-a` backend, as the bare unfiltered query string (`last_over_time(volume_labels[<window>])`, the `qos_*` families with `lun=""` only) carrying no `az` matcher; a `volume_labels` series held by the `zone-b` backend is not loaded even if a loaded claim's `volumename` would join it

#### Scenario: Catch-all Harvest backend under a zone filter

- **WHEN** one backend serves `harvest` with no `zones` declared and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued to that backend as the bare unfiltered query string, every zone's Harvest series is loaded, and the aggregates in the response are exactly those joined by the loaded `zone-a` claims

#### Scenario: Environment filter does not reach Harvest

- **WHEN** a build runs with `env={prod}` against a single `harvest` backend holding series stamped `env="prod"` and `env="dev"`
- **THEN** every Harvest query is issued as the bare unfiltered query string, both environments' series are loaded, and a loaded `prod` claim whose `volumename` joins a `volume_labels` series stamped `env="dev"` still receives its `pvc-to-netapp-aggr` edge

#### Scenario: Harvest lacking the environment label under an env filter

- **WHEN** the kube-state-metrics series carry `az="zone-a"`, the Harvest series carry no `az` and no `env` label at all, and a build runs with `az={zone-a}` or `env={prod}`
- **THEN** every Harvest leg returns its rows, the `pvc-to-netapp-aggr` edges are drawn for the loaded claims that join, and the selector-coverage Warn never names a Harvest family

#### Scenario: Harvest served by its own upstream

- **WHEN** the routing table declares one backend serving `harvest` at `http://vm-netapp.example:8428` and another serving every other family at `http://vm-k8s.example:8428`
- **THEN** the thirteen Harvest queries are issued only to `http://vm-netapp.example:8428`, the kube-state-metrics and kubelet queries only to `http://vm-k8s.example:8428`, and the resulting `pvc-to-netapp-aggr` edges join claims read from one upstream to volumes read from the other

#### Scenario: Unfiltered build merges Harvest across backends

- **WHEN** two backends serve `harvest` for different zones and a build runs with no `az` value
- **THEN** every Harvest query is issued to both and the results are merged, so aggregates from both zones can join their claims in one graph

#### Scenario: Shared filer reached by reference from either filtered cluster

- **WHEN** claims in `cluster-alpha` and `cluster-beta` both join `netapp/ontap-prod/aggr/aggr1` and a build runs with `cluster={cluster-alpha}`
- **THEN** `netapp/ontap-prod/aggr/aggr1` and its owning `netapp-node` are materialised with a `pvc-to-netapp-aggr` edge from the `cluster-alpha` claim only; a `cluster={cluster-beta}` build materialises the same two nodes from the `cluster-beta` claim
