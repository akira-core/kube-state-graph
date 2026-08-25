## MODIFIED Requirements

### Requirement: Harvest legs under request-scoped selectors

Every NetApp Harvest query the builder issues — `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, and `node_new_status` — SHALL carry the request-scoped `az` and `env` matchers (under the operator-configured label keys) and SHALL NEVER carry a `cluster` or `namespace` matcher. Harvest's `cluster` label is the **ONTAP** cluster name and never a Kubernetes cluster, so a Kubernetes `cluster` value pushed into it would match nothing; Harvest carries no `namespace` at all. The `qos_*` families keep their fixed `lun=""` selector, composed with the request matchers.

These queries additionally constitute the `harvest` query family of the `upstream-backend-routing` capability, so they MAY be served by a different upstream installation from the kube-state-metrics and kubelet legs, and are selected by requested availability zone under the same rule as those legs. Routing changes **which** installation answers a Harvest query; it changes neither the query string, the three-hop join, nor the per-hop degradation below.

Within a filtered build the storage chain is therefore narrowed **by reference**: an aggregate and its owning controller materialise only when a **loaded** claim's `volumename` joins a `volume_labels` series, so a `cluster` or `namespace` filter reaches the NetApp graph solely through the claims it loads. A filer shared across clusters, zones, or environments is one node set, reached from whichever loaded claims join it.

The operator SHALL ensure the deployment stamps the configured `az` / `env` labels on the Harvest series exactly as on the Kubernetes series. A Harvest family lacking the label under an `az` / `env` filter returns nothing: the build completes with no `netapp-aggr` / `netapp-node` nodes and no `pvc-to-netapp-aggr` edges for that request, which the join-coverage signal reports as unmatched claims.

#### Scenario: Cluster and namespace filters never reach Harvest

- **WHEN** a build runs with `cluster={cluster-alpha}` and `namespace={shop}` and no `az` / `env` value
- **THEN** every Harvest query is issued exactly as in an unfiltered build (the `qos_*` families with `lun=""` only), and the aggregates in the response are exactly those joined by the loaded `cluster-alpha` / `shop` claims

#### Scenario: Zone filter reaches Harvest

- **WHEN** a build runs with `az={zone-a}`
- **THEN** every Harvest query carries `az="zone-a"` in addition to any fixed selector, and a `volume_labels` series stamped `az="zone-b"` is not loaded even if a loaded claim's `volumename` would join it

#### Scenario: Harvest served by its own upstream

- **WHEN** the routing table declares one backend serving `harvest` at `http://vm-netapp.example:8428` and another serving every other family at `http://vm-k8s.example:8428`
- **THEN** the thirteen Harvest queries are issued only to `http://vm-netapp.example:8428`, the kube-state-metrics and kubelet queries only to `http://vm-k8s.example:8428`, and the resulting `pvc-to-netapp-aggr` edges join claims read from one upstream to volumes read from the other

#### Scenario: Harvest backends selected by zone

- **WHEN** two backends serve `harvest`, declaring `zones: [zone-a]` and `zones: [zone-b]`, and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued only to the `zone-a` backend, carrying the `az="zone-a"` matcher

#### Scenario: Unfiltered build merges Harvest across backends

- **WHEN** two backends serve `harvest` for different zones and a build runs with no `az` value
- **THEN** every Harvest query is issued to both and the results are merged, so aggregates from both zones can join their claims in one graph

#### Scenario: Shared filer reached by reference from either filtered cluster

- **WHEN** claims in `cluster-alpha` and `cluster-beta` both join `netapp/ontap-prod/aggr/aggr1` and a build runs with `cluster={cluster-alpha}`
- **THEN** `netapp/ontap-prod/aggr/aggr1` and its owning `netapp-node` are materialised with a `pvc-to-netapp-aggr` edge from the `cluster-alpha` claim only; a `cluster={cluster-beta}` build materialises the same two nodes from the `cluster-beta` claim

#### Scenario: Harvest lacking the environment label under an env filter

- **WHEN** the Harvest series carry no `env` label and a build runs with `env={prod}`
- **THEN** every Harvest leg returns zero rows, the build completes with no NetApp nodes or storage edges, every loaded claim with a `volumename` is counted as an unmatched claim by the join-coverage signal, and the build does not fail
