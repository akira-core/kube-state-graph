# Design — ingress-lb-service-fallback

Extends the engine of `translate-global-fqdn-to-k8s-service` (its D-numbers are referenced below).
Settled decisions:

## D1 — Ingress-cluster selection is untouched

The fallback runs strictly AFTER `ClustersWithIngressIP` + `pickIngressCluster` (parent D10) have
locked one cluster and `loadWindow` has loaded it. The two pre-window misses (`RouteNoIngress`,
`RouteAmbiguousIngress`) return before the window exists, so the fallback can never run on those
paths. `RouteRequest.CallerCluster` semantics (family key + tie-break only) are unchanged.

## D2 — Uniqueness inside the locked cluster only

Service selection is Hop 1 only (IP → ingress LB Service), inside the already-loaded
(selected-cluster) window. Multiple Services carrying the same IP in that cluster's window →
`RouteAmbiguousIngressService` → external. No lexicographic tie-break, mirroring the
ambiguous-ingress-cluster spirit: degrade rather than guess.

## D3 — Topology mismatch keeps the existing reason

A fallback hit resolves through `resolveServiceLevel(dest.Cluster, ns, svc)` exactly like
`RouteHit` (parent D11). If VictoriaMetrics topology lacks the Service, the existing
`route_engine_dest_cluster_lacks_service` external path applies — no special case.

## D4 — Coarse semantics: host/path/port ignored

The fallback answers "which ingress LB Service owns this IP", not "where did this request route".
`RouteRequest.Host`/`Path`/`Port` do not participate. The outcome name (`ingress_lb_service`) and
the debug log make the coarser semantics distinguishable from a routed `hit`.

## D5 — Non-goals

Ingress CR resolution, nginx.conf parsing, backend-Service expansion
(`docs/nginx-ingress-backend-resolution.md` paths 1–3 beyond the LB layer). No new node/edge
type, attribute, or `labels` key; `pod-calls-service` / `service-selects-pod` are reused as-is.

## D6 — Window-wide identity dedup, not per-instant evaluation

The candidate set is the in-memory analogue of the `ClustersWithIngressIP` SQL, scoped to the
locked cluster: every `store.ServiceRow` in the loaded `TrafficWindow` whose validity overlaps
`[start, end]` and whose `HasIngressIP(ip)` holds. Per IP, candidates dedup to distinct
`(namespace, name)`; no `mw.Segments()` / `asOf` evaluation. Consequences:

- Multiple versions of the SAME identity (normal version churn) dedup to one → still unique.
- An identity change within the window (svc-A deleted, svc-B created on the same IP) yields two
  identities → ambiguous → external ("don't guess").
- `ClustersWithIngressIP` itself cannot serve here: its contract returns cluster names only (the
  parent-D10 probe); the Service identity comes from the already-loaded rows — no new store read.

Rejected alternatives: single-point evaluation at `req.End` (asymmetric with the Istio path — a
Service deleted mid-window would resolve via Istio but not via the fallback); per-segment sweep
mirroring the Istio loop (handles identity churn but adds Segments/tri-state machinery for a case
that is rare in practice — user opted for the simpler window-wide rule).

## D7 — Multi-IP merge precedence (order-free)

Per IP compute `S_ip` (distinct identities). Then, in this order: any `|S_ip| > 1` → ambiguous;
else any `|S_ip| == 0` → no fallback (keep the Istio pipeline's deepest miss via the existing
`outcomeRank` fold — an IP with no LB Service means the evidence is incomplete, not conflicting);
else all singletons must be the same identity → hit, otherwise ambiguous. Order-free: the outcome
does not depend on IP iteration order.

## D8 — Hit priority and outcome plumbing

An Istio `RouteHit` in any segment always wins; the fallback is called only in the `!hit` branch
of `resolve()`. The two new outcomes do NOT enter `outcomeRank` (they are terminal results of the
fallback, not per-segment misses). On a fallback hit, `dest.Cluster` is stamped with the locked
cluster exactly like the `RouteHit` return (parent D11); `Port`/`Subset` stay zero (discarded in
v1 anyway). `pkg/build` gains only the two constants and the `routeIndexResolve` cases —
containment (parent D1, `make check-route-containment`) is unaffected.
