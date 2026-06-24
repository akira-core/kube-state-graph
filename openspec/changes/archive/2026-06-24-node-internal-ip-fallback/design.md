# Design: node-internal-ip-fallback

## Context

`QNodeAddresses` renders `last_over_time(<prefix>kube_node_status_addresses{type="ExternalIP"}[w])` (`pkg/promql/queries.go`). `parseTopology` (`pkg/build/topology.go`) folds the vector into an `externalIPs` map keyed `(cluster, node)`, lexically-smallest address on duplicates (D6 determinism). Clusters whose nodes have no ExternalIP (private/on-prem, NATed node pools) emit no node `ipaddress` at all, though `InternalIP` rows always exist in KSM.

## Goals / Non-Goals

**Goals:**
- K8s node `ipaddress` = ExternalIP when present, else InternalIP, else omitted.
- Deterministic output unchanged in spirit: pure function of data, not vector order.
- Existing ExternalIP deployments see byte-identical responses.

**Non-Goals:**
- No multi-element `ipaddress` (still single-element slice).
- No `labels.internal_ip` / `labels.external_ip` (IPs never in labels).
- No new config knob — fallback is hardcoded, like other resolution rules.
- No change to pod / service / PVC / external `ipaddress` semantics.

## Decisions

1. **Widen the existing selector, not a second query.** `type=~"ExternalIP|InternalIP"` (PromQL `=~` is fully anchored → exact alternation). One query keeps the build fan-out at 10 queries, keeps `QNodeAddresses` constant bare (self-metric `query_name` dimensions unchanged), and the prefix renderer untouched apart from the selector literal. Alternative — an 11th query for InternalIP — rejected: more fan-out, a new `query_name`, contract churn for no benefit.
2. **Two-tier pick at parse time.** `parseTopology` keeps per-`(cluster, node)` best address **per type** (lexically-smallest within type), then ExternalIP wins over InternalIP at assembly. Equivalent formulation: single map storing `(rank, addr)` where ExternalIP rank < InternalIP rank; smaller rank wins, ties broken by smaller address. Pure function of the sample set — order-free (D6).
3. **Other `type` values ignored.** `Hostname`/`InternalDNS`/`ExternalDNS` rows never reach the parser (selector excludes them); parser still guards on type string so a future selector widening cannot silently leak hostnames into `ipaddress`.

## Risks / Trade-offs

- [Selector widening doubles node-address sample count] → negligible: per-node cardinality is tiny vs pods; bounded by upstream VM search limits like everything else.
- [Existing golden tests / integration fixtures asserting "no IP without ExternalIP"] → updated fixtures must distinguish "no ExternalIP row" (now may fall back) from "no address rows at all" (still omitted); add explicit fallback + both-present tests.

## Migration Plan

Additive behaviour; no API shape change. Deploy normally. Rollback = previous binary (nodes lose InternalIP fallback only).

## Open Questions

None.
