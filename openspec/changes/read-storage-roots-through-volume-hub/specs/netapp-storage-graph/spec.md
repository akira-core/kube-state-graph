## MODIFIED Requirements

### Requirement: Harvest legs under request-scoped selectors

Every NetApp Harvest query the builder issues — `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency`, and `node_total_data` — SHALL carry **no request-scoped matcher of any kind**: not `az`, not `env`, and never `cluster` or `namespace`. A *selector-level dimension* is what that forbids. It does not forbid a scope derived from upstream data or from the request's storage-side ROOTS, which is a different mechanism: two Harvest families carry one — the `qos_*` families' `volume` alternation below, and `volume_labels` under a storage-side-rooted `/v1/storage-graph` build (`storage-graph-api`, "Storage-side roots narrow the Harvest topology read per component"). Neither appears in the request-scoped selector table, and neither changes what `az` and `env` reach. Harvest's `cluster` label is the **ONTAP** cluster name and never a Kubernetes cluster, so a Kubernetes `cluster` value pushed into it would match nothing; Harvest carries no `namespace` at all; and the `az` dimension reaches Harvest through backend selection alone (below), so a Harvest series need not carry the configured `az` / `env` labels.

The `qos_*` families carry one selector and no other: the `volume` alternation of the scoped-read requirement. That alternation is **derived from upstream data, not from the request**: its values are FlexVol names the volume-label family already returned. It is nonetheless *influenced* by the request, because the claims whose tokens produced those names are themselves loaded under the request's selectors. This is the "narrowed by reference" principle of this capability realised at the query layer rather than only in the parse: a `cluster`, `namespace`, `az` or `env` filter reaches the QoS read solely through the claims it loads, never as a matcher on a Harvest label. `volume_labels` carries no selector either, EXCEPT under a storage-side-rooted `/v1/storage-graph` build, where it carries the root restriction (by ONTAP cluster, aggregate or SVM), the owner-completion restriction for the aggregates an SVM restriction touched, and the token restriction of its second phase — the same "narrowed by reference" principle read from the other end of the chain, since the roots name the storage components rather than the claims. Every other Harvest query carries no selector at all, in every build.

These queries constitute the `harvest` query family of the `upstream-backend-routing` capability, so they MAY be served by a different upstream installation from the kube-state-metrics and kubelet legs. The family is **zone-routed**: a request's `az` values select which `harvest` backends are asked — those whose `zones` intersect the request, plus any catch-all — under the same rule as the `ksm` and `kubelet` families. Unlike those families, the selected zone is NOT additionally rendered as a matcher: for Harvest the zone boundary is the store, not a label on the series. The `env` dimension has no routing counterpart and SHALL have no effect on the Harvest legs whatsoever. Routing changes **which** installation answers a Harvest query; it changes neither the query string, the three-hop join, nor the per-hop degradation below.

Within a filtered build the storage chain is therefore narrowed **by reference**: an aggregate and its owning controller materialise only when a **loaded** claim's derived token matches a `volume_labels` series (or, in the storage-flow graph, when selected as a root), so a `cluster`, `namespace`, or `env` filter reaches the NetApp graph solely through the claims it loads, and an `az` filter reaches it through the claims it loads plus the backends it selects. A filer shared across clusters, zones, or environments is one node set, reached from whichever loaded claims match it. In a hub-mode `/v1/storage-graph` build (`storage-graph-api`, "Storage-side roots read the claim chain through the volume hub") the direction reverses: the claims are loaded FROM the rooted volume-label rows, under the request's `az`, `env`, `cluster` and `namespace` matchers and from the backends the request's `az` selects, so a rooted FlexVol whose claim lives in another zone or environment loads no claim. Under a catch-all `harvest` backend, or under any `env` value, the volume-label read is the whole estate and the narrowing is by reference alone; a FlexVol name carried by volumes in two zones or environments resolves to the lexically-smallest `(ontap_cluster, aggr)` — the same collision rule an unfiltered build already applies, since an unfiltered build reads every zone.

#### Scenario: Cluster and namespace filters never reach Harvest

- **WHEN** a build runs with `cluster={cluster-alpha}` and `namespace={shop}` and no `az` / `env` value
- **THEN** no Harvest query carries a `cluster` or `namespace` matcher; the request carries no storage-side root, so the `volume_labels`, aggregate, controller, hardware, performance and policy queries are issued exactly as in an unfiltered build; the QoS queries restrict `volume` to the FlexVol names matched by the loaded `cluster-alpha` / `shop` claims alone; and the aggregates in the response are exactly those those claims matched

#### Scenario: Zone filter reaches Harvest

- **WHEN** two backends serve `harvest`, declaring `zones: [zone-a]` and `zones: [zone-b]`, and a build runs with `az={zone-a}`
- **THEN** every Harvest query — including `node_labels` and the four `system_node` counters — is issued only to the `zone-a` backend, carrying no `az` matcher; a `volume_labels` series held by the `zone-b` backend is not loaded even if a loaded claim's derived token would match it

#### Scenario: Catch-all Harvest backend under a zone filter

- **WHEN** one backend serves `harvest` with no `zones` declared and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued to that backend carrying no `az` matcher, every zone's `volume_labels` series is loaded, and the aggregates in the response are exactly those matched by the loaded `zone-a` claims

#### Scenario: Environment filter does not reach Harvest

- **WHEN** a build runs with `env={prod}` against a single `harvest` backend holding series stamped `env="prod"` and `env="dev"`
- **THEN** no Harvest query carries an `env` matcher, both environments' series are loaded, and a loaded `prod` claim whose derived token matches a `volume_labels` series stamped `env="dev"` still receives its `pvc-to-netapp-aggr` edge

#### Scenario: Shared filer reached by reference from either filtered cluster

- **WHEN** claims in `cluster-alpha` and `cluster-beta` both match `netapp/ontap-prod/aggr/aggr1` and a build runs with `cluster={cluster-alpha}`
- **THEN** `netapp/ontap-prod/aggr/aggr1` and its owning `netapp-node` are materialised with a `pvc-to-netapp-aggr` edge from the `cluster-alpha` claim only; a `cluster={cluster-beta}` build materialises the same two nodes from the `cluster-beta` claim

#### Scenario: Harvest lacking the environment label under an env filter

- **WHEN** the kube-state-metrics series carry `az="zone-a"`, the Harvest series carry no `az` and no `env` label at all, and a build runs with `az={zone-a}` or `env={prod}`
- **THEN** every Harvest leg returns its rows, the `pvc-to-netapp-aggr` edges are drawn for the loaded claims that match, and the selector-coverage Warn never names a Harvest family

#### Scenario: Harvest served by its own upstream

- **WHEN** the routing table declares one backend serving `harvest` at `http://vm-netapp.example:8428` and another serving every other family at `http://vm-k8s.example:8428`
- **THEN** every Harvest query — including each chunk of the scoped QoS read and the five node legs — is issued only to `http://vm-netapp.example:8428`, the kube-state-metrics and kubelet queries only to `http://vm-k8s.example:8428`, and the resulting `pvc-to-netapp-aggr` edges join claims read from one upstream to volumes read from the other

#### Scenario: Unfiltered build merges Harvest across backends

- **WHEN** two backends serve `harvest` for different zones and a build runs with no `az` value
- **THEN** every Harvest query is issued to both and the results are merged, so aggregates from both zones can match their claims in one graph

#### Scenario: Hub mode loads claims from the rooted rows

- **WHEN** a hub-mode build runs with `az={zone-a}`, `env={prod}`, and the `zone-a` `harvest` backend returns two rooted FlexVols, one whose claim lives in a `zone-a` / `prod` cluster and one whose claim lives in an `env="dev"` cluster
- **THEN** the `prod` claim is loaded and receives its aggregate and SVM exactly as an unfiltered build would give them, and the `dev` claim is not loaded, because every claim query carries `<env-key>="prod"`

### Requirement: Harvest volume-label series as the storage topology source

The builder SHALL consume the NetApp Harvest volume-object label series `volume_labels` from the same centralised VictoriaMetrics endpoint as every other series. The fixed, case-sensitive label contract it MUST carry: `cluster` (the ONTAP cluster name — NOT a Kubernetes cluster; the two namespaces never mix), `node` (the ONTAP controller currently owning the containing aggregate), `aggr` (the containing aggregate), `svm` (the serving Storage Virtual Machine), and `volume` (the ONTAP FlexVol name). It is an **info series**: its sample value SHALL be ignored entirely and only its label set consumed.

Every label in that contract is **stock Harvest output**. The builder SHALL NOT require the deployment to install a Prometheus relabel rule, and SHALL NOT read any non-stock label naming the Kubernetes PersistentVolume.

This one series is the SOLE source of the graph's storage topology — the `pvc-to-netapp-aggr` edge, the `netapp-aggr` and `netapp-node` entities, and the PVC `svm` and `aggr` labels all derive from it and from nothing else. The I/O measurements and the throughput ceiling ride on separate families (the two requirements below) and SHALL NOT contribute to any topological decision; conversely, a claim SHALL NEVER lose its storage topology because an I/O family failed to match.

The issued query SHALL read the series at the window end without `rate()`, in the same shape as every other Harvest leg. Its FIRST read SHALL carry no restriction on `volume` — the set of interesting FlexVol names is not known until this family has been read. A `/v1/storage-graph` build whose request names the storage components it should read MAY restrict that first read by those components, re-read whole every aggregate an SVM restriction touched so the owning-controller vote stays complete, and then issue a SECOND read restricted on `volume` to the derived tokens of the claims the first read matched, merging all of them before the parse; that phased shape, the roots and match modes it applies to, and its byte-identical-body obligation are defined by the `storage-graph-api` capability's "Storage-side roots narrow the Harvest topology read per component". A `/v1/graph` build carries no roots and always issues the single unrestricted read.

The bridge from a claim's PV name to this family's `volume` label is the derivation described in "PV-name-to-FlexVol-name derivation". The graph inherits three blind spots from it: a FlexVol whose name does not embed the claim's PV name under the configured derivation never joins (its claim's `svm` and `aggr` are absent and no edge is drawn); the Trident "economy" drivers pack many claims into one shared FlexVol, so no per-claim series exists at all; and a FlexGroup volume spans aggregates, so its series carries no single usable `aggr` label (no aggregate edge can be drawn and its PVC carries no `aggr` label — see the join-coverage requirement).

The family is OPTIONAL. When it is absent from the window — the normal case for a deployment without NetApp Harvest — the builder SHALL produce a valid graph with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` or `aggr` labels; PVC `volumename` labels are unaffected and the build SHALL NOT fail.

#### Scenario: Volume label series consumed for its labels only

- **WHEN** the builder issues the `volume_labels` query for a window
- **THEN** the query references the bare series evaluated at the window end, does not wrap it in `rate()`, carries no `volume` restriction on its first read, and the resolver derives the aggregate, owning controller, and SVM from the matched series' labels while its sample value plays no part in any output

#### Scenario: Stock Harvest output joins without a relabel rule

- **WHEN** the upstream carries `volume_labels` exactly as stock Harvest emits it, with no deployment-installed relabel rule and no label naming the Kubernetes PersistentVolume
- **THEN** claims whose derived tokens match the `volume` label still resolve their aggregate, controller and `svm`, their PVCs carry the `aggr` label, and their `pvc-to-netapp-aggr` edges are emitted

#### Scenario: Harvest absent entirely

- **WHEN** the upstream contains topology series but no `volume_labels` series for the window
- **THEN** the build completes successfully with no `netapp-aggr` or `netapp-node` nodes, no `pvc-to-netapp-aggr` edges, and no PVC `svm` or `aggr` labels, while PVC `volumename` labels still resolve from `kube_persistentvolumeclaim_info`

#### Scenario: I/O families present without the label series

- **WHEN** the upstream carries QoS workload series whose `volume` matches a claim's derived token but no `volume_labels` series matches it
- **THEN** no `pvc-to-netapp-aggr` edge is emitted for that claim, its PVC carries no `aggr` label, no aggregate or controller is materialised from the QoS series, and the build does not fail
