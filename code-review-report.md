# Code Review Report — PR #7

**PR:** [#7 feat(route): 將全域 FQDN / unknown peer 解析為 Kubernetes Service](https://github.com/akira-core/kube-state-graph/pull/7)  
**Branch:** `translate-global-FQDN-to-k8s-service` → `main`  
**HEAD:** `1c7ff2156a6fff9bde7cacf2a92620ec5071c359`  
**State:** OPEN (not draft)  
**Review date:** 2026-07-27  
**Posted comment:** https://github.com/akira-core/kube-state-graph/pull/7#issuecomment-5091701451  
**Relevant guidance:** [`CLAUDE.md`](./CLAUDE.md)  
**Method:** multi-agent review (eligibility → CLAUDE.md paths → summary → 5 parallel audits → confidence scoring → filter score &lt; 80 → re-eligibility → `gh pr comment`)

---

## 1. Eligibility

| Check | Result |
|-------|--------|
| Closed? | No — OPEN |
| Draft? | No |
| Automated / trivial? | No — large feature PR |
| Prior review from this process? | No (0 reviews, 0 PR comments before this run) |
| Still eligible at post time? | Yes |

---

## 2. PR summary

### Problem

When `server="unknown"`, peer enrichment recovers only in-cluster names (`.svc` DNS, bare Service, ClusterIP). Global ingress FQDNs like `api.example.com` become dead-end `external/*` nodes even though traffic hits an Istio Gateway / VirtualService and lands on a real in-cluster Service — the real dependency is invisible.

### Main behavioral changes

- On every path that would emit `external` for an unknown-server peer, optionally consult an Istio route engine: `(host, "/", port, DNS IPs) @ request window end` → destination Service.
- Hit → normal `service` node + `pod-calls-service` (may cross clusters via family fan-out); miss/error → existing external (build never fails on routing).
- New optional series dims: `client_dns_answers` (required for engine use), optional peer port labels; port precedence `:port` → label → **443**.
- Ingress cluster chosen from DNS answer IPs (family-first, caller tie-break); pure prescan + prefetched index so parse stays pure.
- Follow-ons on branch: LB Service fallback (nginx), RouteHit ingress chain, `role=ingress-gateway|ingress-lb`, ns-scoped gateways, point-in-time (not interval) resolution.
- No new node/edge types; `pod-calls-service` becomes `may_cross_cluster: true`.

### Key packages / files

| Area | Paths |
|------|--------|
| Engine | `pkg/route/` (`resolver`, `store`/ClickHouse, `translate`, `gwresolve`, `matchcheck`, `snapshot`, `ingresslb`, `ingresspick`) |
| Wiring | `pkg/build/{routeresolve,routeprescan,servicegraph,options}.go` |
| Facade / config | `pkg/kubegraph/`, `cmd/kube-state-graph/main.go`, `internal/config/` |
| Tests | `pkg/route/*_test.go`, `pkg/build/routeprescan_test.go`, `internal/integration/route_e2e_test.go` |
| Ops / docs | Dockerfile (`router_check_tool`), Makefile containment check, openspec + CLAUDE.md |

### Opt-in vs always-on

**Opt-in / off by default.** Empty `--route-store-dsn` / `KSG_ROUTE_STORE_DSN` ⇒ nil `RouteResolver` ⇒ byte-identical to pre-change. Needs DSN + `router_check_tool` + timeouts when enabled.

### Risk / complexity hotspots

- Heavy deps (`istio.io/istio`, ClickHouse, Envoy protos) contained by **`pkg/build` ↛ `pkg/route`**; wrong import leaks into embedders.
- `router_check_tool` ~50–60 ms/call; build-timeout degradation paths.
- Cross-repo store schema (metadata-exporter); schema drift fails startup.
- Multi-IP / multi-cluster ingress selection ambiguity → external.
- Pure-parse + prefetched index must stay aligned with resolve paths.

### Deferred / out of scope

- DestinationRule subsets; destination port/subset on graph; HTTP path ≠ `"/"`.
- D29 `"://"` connection-string path not extended.
- Write path to config store; broad caching (beyond per-build probe memo).
- Cross-namespace Gateway selector attachment (degrades to external/LB).

---

## 3. CLAUDE.md paths considered

- [`./CLAUDE.md`](./CLAUDE.md) (repo root; only CLAUDE.md present under the workspace)

No nested per-package CLAUDE.md files were found under modified directories.

---

## 4. Review agents and raw findings

Five independent review agents ran in parallel:

| Agent | Focus | Outcome |
|-------|--------|---------|
| #1 | CLAUDE.md compliance | 4 issues |
| #2 | Shallow obvious bugs | 3 issues |
| #3 | Historical git context | 3 issues (docs/contract lag; no runtime regressions) |
| #4 | Prior PR comments | None (repo has no GitHub review threads on PRs #1–#7) |
| #5 | Code-comment compliance | 9 issues (mostly stale comments / registry prose) |

Issues were then confidence-scored on the rubric below. **Only scores ≥ 80 were posted to the PR.**

### Confidence rubric

| Score | Meaning |
|------:|---------|
| 0 | False positive / pre-existing / not PR-owned |
| 25 | Somewhat confident; may be FP; unverified; stylistic not in CLAUDE.md |
| 50 | Real but nitpick / rare / not very important relative to PR |
| 75 | Highly confident real issue; will hit in practice or called out in CLAUDE.md |
| 100 | Certain; frequent; evidence confirms |

---

## 5. Issues posted to PR (score ≥ 80)

### Issue 1 — Multi-IP `loadSnapshot` union without dedup breaks route translate

| Field | Value |
|-------|--------|
| **Confidence** | **90** |
| **Classification** | REAL |
| **Severity** | high |
| **Flagged by** | Agent #2 (bug); confirmed by confidence scorer |
| **Why flagged** | bug |

#### Description

`loadSnapshot` unions `LoadTrafficAt` rows per destination IP with **no cross-IP dedup**. Dual-stack / multi-A `client_dns_answers` that hit the same ingress Service load the same Gateway/VS set twice. `ScopedFor` appends each matching VS twice into `Configs`. istiod’s in-memory store `Create` then fails with already-exists (`errAlreadyExists`), `Translate` errors, and the endpoint degrades to **`external`** instead of the routed Service.

Multi-IP is a first-class path (`parseDNSAnswers`, design D10) and is the common cloud LB / dual-stack shape. `candidates()` already dedups gateways by `namespace/name`, but the snapshot row union does not.

#### Failure chain

1. `loadSnapshot` appends rows for each IP in `req.IPs` with no identity dedup.
2. Same multi-A / dual-stack LB ⇒ same Gateway/VS rows twice.
3. `ScopedFor` does not dedup VSes when building `Configs` (Gateway taken once via first name match + `break`; VSes are not).
4. `buildScopedEnv` fails on second `Create` of the same `(GVK, namespace, name)`.
5. Error → `resolveConfig` error → `ResolveRoute` error → prescan `route_engine_error` → external node.

#### Evidence (code)

**`loadSnapshot` — no row dedup:**

```147:164:pkg/route/resolver.go
// loadSnapshot loads the selected ingress cluster's configuration state at
// req.At: one store.LoadTrafficAt per destination IP, rows unioned —
// multi-A-record DNS answers are a union of candidates WITHIN the one selected
// cluster (design §9.2 / D10; cross-cluster unions are forbidden).
func (r *Resolver) loadSnapshot(ctx context.Context, cluster string, req build.RouteRequest) (store.TrafficSnapshot, error) {
	var out store.TrafficSnapshot
	for _, ip := range req.IPs {
		w, err := r.st.LoadTrafficAt(ctx, cluster, ip, req.At)
		if err != nil {
			return store.TrafficSnapshot{}, err
		}
		out.Services = append(out.Services, w.Services...)
		out.Deploys = append(out.Deploys, w.Deploys...)
		out.Gateways = append(out.Gateways, w.Gateways...)
		out.VSes = append(out.VSes, w.VSes...)
	}
	return out, nil
}
```

**`ScopedFor` — VS list not deduped:**

```142:157:pkg/route/snapshot/snapshot.go
	var destHosts []string
	for i := range s.w.VSes {
		r := &s.w.VSes[i]
		if !s.live(r.ValidFrom, r.ValidTo) || !boundTo(r.Namespace, r.BoundGateways, gw.Namespace, gwName) {
			continue
		}
		var vsSpec networking.VirtualService
		if err := pjUnmarshal.Unmarshal([]byte(r.SpecJSON), &vsSpec); err != nil {
			return translate.ScopedInput{}, false, err
		}
		destHosts = append(destHosts, vsDestHosts(&vsSpec)...)
		cfgs = append(cfgs, config.Config{
			Meta: config.Meta{GroupVersionKind: gvk.VirtualService, Name: r.Name, Namespace: r.Namespace},
			Spec: &vsSpec,
		})
	}
```

**`buildScopedEnv` — Create fails on duplicate:**

```298:302:pkg/route/translate/translate.go
	for _, cfg := range in.Configs {
		if _, err := configController.Create(cfg); err != nil {
			return nil, err
		}
	}
```

#### GitHub links (full SHA)

- https://github.com/akira-core/kube-state-graph/blob/1c7ff2156a6fff9bde7cacf2a92620ec5071c359/pkg/route/resolver.go#L150-L164
- https://github.com/akira-core/kube-state-graph/blob/1c7ff2156a6fff9bde7cacf2a92620ec5071c359/pkg/route/snapshot/snapshot.go#L142-L157
- https://github.com/akira-core/kube-state-graph/blob/1c7ff2156a6fff9bde7cacf2a92620ec5071c359/pkg/route/translate/translate.go#L297-L302

#### Test gap

`TestResolveRoute_LockedClusterScopesSnapshotLoads` exercises multi-IP with **empty** snapshots — never multi-IP + non-empty config through `Translate`.

#### Suggested fix

Dedup rows in `loadSnapshot` (or in `ScopedFor` configs) by resource identity — e.g. `(namespace, name[, valid_from])` for Gateways/VSes/Services/Deploys — so multi-IP union is a *set* of candidates, not a multiset that breaks `Create`. Mirror the existing `candidates()` `namespace/name` dedupe pattern.

#### CLAUDE.md

CLAUDE.md documents multi-IP agreement / same-cluster load and does **not** claim row-level dedup. This is a **code bug** relative to intended multi-A behavior, not a CLAUDE.md inconsistency.

---

## 6. Issues reviewed but filtered (score &lt; 80)

These were raised by review agents and confidence-scored; they did **not** meet the ≥ 80 bar and were **not** posted to the PR. Captured here for local follow-up.

---

### F1 — Gateway identity drops namespace → `ScopedFor` name-only can pick wrong Gateway

| Field | Value |
|-------|--------|
| **Confidence** | **75** |
| **Classification** | REAL |
| **Severity** | high (Agent #1) / medium (Agent #2) |
| **Flagged by** | Agent #1 (CLAUDE.md), Agent #2 (bug) |
| **Why filtered** | score &lt; 80; uncommon multi-ns / multi-IP path |

#### Description

`GatewayCand` carries `Namespace` + `Name`, but adapters pass only bare `Name` into `gwresolve` / `candNames`. `ScopedFor(gwName)` scans **all** loaded Gateways and takes the first live row matching **name only**. Multi-IP `loadSnapshot` unions gateway rows across ingress namespaces; same bare name in two namespaces can desync pick vs load → wrong destination or miss/external.

#### Evidence

```263:277:pkg/route/resolver.go
// candsToGateways / candNames pass only Name (Namespace dropped)
```

```121:128:pkg/route/snapshot/snapshot.go
func (s *Snapshot) ScopedFor(gwName string) (translate.ScopedInput, bool, error) {
	// first live row with matching Name only
```

#### Reachability

| Path | Risk |
|------|------|
| Single IP, single ingress ns | Safe in practice |
| Single IP, multi-owner IP → multi-`nsList` | Candidates filtered; `w.Gateways` not |
| Multi-IP across namespaces | Unioned load + name-only pick |

#### CLAUDE.md

CLAUDE.md states hop 3 is namespace-scoped so bare-name gateway identity through `gwresolve`/`ScopedFor` is unambiguous **by construction**. That holds for the hop-3 **candidate set** under a single-ns load; it is **not** true for `ScopedFor` over a multi-IP / multi-ns loaded superset.

#### Links

- https://github.com/akira-core/kube-state-graph/blob/1c7ff2156a6fff9bde7cacf2a92620ec5071c359/pkg/route/resolver.go#L263-L277
- https://github.com/akira-core/kube-state-graph/blob/1c7ff2156a6fff9bde7cacf2a92620ec5071c359/pkg/route/snapshot/snapshot.go#L121-L128

---

### F2 — `ResolveIPToGateways` hop 1/2 first-match is order-dependent

| Field | Value |
|-------|--------|
| **Confidence** | **75** |
| **Classification** | REAL |
| **Severity** | high (Agent #1) / medium (Agent #2) |
| **Flagged by** | Agent #1 (CLAUDE.md / determinism), Agent #2 (bug) |
| **Why filtered** | score &lt; 80; trigger usually rare |

#### Description

Hop 1 `break`s on the first live Service with the IP; hop 2 `break`s on the first matching Deploy. No sort/dedup and no ambiguity outcome. Sibling path `ingressServiceIdentity` / `ResolveIPToIngressServices` deliberately collects all live identities and returns `identityAmbiguous` / `RouteAmbiguousIngressService` (“never a lexicographic tie-break”). `LoadTrafficAt` has no `ORDER BY`, so row order can flip hop1/2 selection.

Multi-Service same ExternalIP/LB IP is uncommon on cloud LBs but possible (MetalLB share, manual ExternalIPs, dual controllers); multi-Deploy matching one selector is more plausible (canary).

#### Evidence

```61:80:pkg/route/snapshot/snapshot.go
// Hop 1: first live HasIngressIP(ip) wins, then break
// Hop 2: first matching deploy wins, then break
```

#### Contrast

- LB fallback path: multi-identity → `RouteAmbiguousIngressService` → external.
- Istio hop path: multi-identity → **guess** via first match.

#### Links

- https://github.com/akira-core/kube-state-graph/blob/1c7ff2156a6fff9bde7cacf2a92620ec5071c359/pkg/route/snapshot/snapshot.go#L56-L84

---

### F3 — `/v1/edge-types` `service-selects-pod` description is stale

| Field | Value |
|-------|--------|
| **Confidence** | **75** |
| **Classification** | REAL |
| **Severity** | high (Agent #5) |
| **Flagged by** | Agent #5 (code comment / registry text) |
| **Why filtered** | score &lt; 80; prose drift on served catalogue, not a runtime graph bug |

#### Description

`pkg/graph/registry.go` `service-selects-pod` Description claims materialisation only for a `://` connection-string in the caller’s own cluster with **family-wide** fan-out. Actual paths also materialise for:

| Path | Fan-out |
|------|---------|
| D29 `://` connection-string | Family-wide (`resolveServiceLevel`) |
| Unknown-server peer-label enrichment | Family-wide |
| Route engine `RouteHit` backend | Family-wide (anchor = ingress cluster) |
| RouteHit ingress-chain entry hop | **Locked-cluster** (`resolveServiceLevelInCluster`) |
| `RouteIngressLBService` (nginx) fallback | **Locked-cluster** |

Wrong claims: “only for … `://`”, “caller’s own cluster”, always family-wide. Structural fields (`source_type`, `target_type`, `directed`, `may_cross_cluster`) remain correct.

#### Evidence

```79:80:pkg/graph/registry.go
// Description still connection-string-only / family-wide-only narrative
```

```823:876:pkg/build/servicegraph.go
// resolveServiceLevel vs resolveServiceLevelInCluster
```

#### Also noted (lower severity)

- `pod-calls-service` description mostly aligned; omits peer-label enrichment as a third producer; understates locked-cluster fan-out for ingress paths.
- Several internal comments in `servicegraph.go` / `routeprescan.go` / `routeresolve.go` still say “every path yields one ID” / “same `resolveServiceLevel`” (see F8).

#### Links

- https://github.com/akira-core/kube-state-graph/blob/1c7ff2156a6fff9bde7cacf2a92620ec5071c359/pkg/graph/registry.go#L68-L80

---

### F4 — `make check-route-containment` not in GitHub Actions CI

| Field | Value |
|-------|--------|
| **Confidence** | **50** |
| **Classification** | REAL (process gap) |
| **Severity** | medium |
| **Flagged by** | Agent #1 (CLAUDE.md) |
| **Why filtered** | score &lt; 80; tree already contained |

#### Description

CLAUDE.md says: *“`make check-route-containment` enforces this in CI.”*  
`make ci` includes the target, but `.github/workflows/ci.yml` only runs: lint, vuln, test, docs-drift, mocks-drift — **no** containment job and no full `make ci`.

Current tree is clean (`pkg/build` / `pkg/kubegraph` do not import `pkg/route`). Hygiene / process only unless someone merges a bad import without running `make ci`.

#### Verification table

| Check | Result |
|-------|--------|
| Makefile has target? | Yes (~163–172) |
| `make ci` includes it? | Yes (~178) |
| GHA runs it? | No |
| CLAUDE.md claims CI enforces it? | Yes |
| Code already contained? | Yes |

---

### F5 — No panic recovery around route-engine work

| Field | Value |
|-------|--------|
| **Confidence** | **25** |
| **Classification** | FALSE_POSITIVE (as CLAUDE.md / “never fail a build” violation) |
| **Severity** | high (as claimed by Agent #1) |
| **Flagged by** | Agent #1 |
| **Why filtered** | score &lt; 80; overstated |

#### Description

Agent claimed: topology `fetch` has `recover()`, route path only handles `err != nil`, so a panic in istiod / `protojson` would fail the HTTP build instead of degrading to external.

#### Scorer findings

| Claim | Finding |
|-------|---------|
| Topology has `recover()` | Yes — for **errgroup process-safety** (panic would kill process); recover still **fails the build** (500), does not degrade |
| Route path has no `recover()` | Yes — serial on handler goroutine |
| Gin recovery | Already turns panic into sanitised 500 |
| CLAUDE.md requires panic recovery | **No** — covers miss/error → external only |

Hardening with `recover()` → `failed: true` would be reasonable but is not a contract gap CLAUDE.md documents.

---

### F6 — OpenAPI still documents removed pre-localize D29 machinery

| Field | Value |
|-------|--------|
| **Confidence** | **0** |
| **Classification** | FALSE_POSITIVE (pre-existing; not PR-owned) |
| **Severity** | medium (if treated as backlog) |
| **Flagged by** | Agent #3 (historical git context) |
| **Why filtered** | score 0; skill excludes pre-existing / unmodified narrative |

#### Description

`internal/api/handlers.go` OpenAPI narrative still describes:

- Per-family service nodes  
- Endpoint-backed pruning  
- Unknown-family fallback  

Runtime uses `localize-pod-calls-service` single-anchor rules. This PR only adds `labels.role` on **adjacent** description lines; the stale Endpoint-resolution paragraphs themselves were not rewritten by this PR. Regenerating swagger re-emits the old sentences.

Useful backlog fix; **not** a PR #7 defect.

#### Related (also filtered / lower)

- OpenAPI still mentions superseded D31 `cluster > node > pod` hierarchy (Agent #3, low–medium).  
- Promoted openspec still says `pod-calls-service` is ALWAYS intra-cluster while registry is `may_cross_cluster: true` (Agent #3, low; intentional registry flip for route path).

---

### F7 — Agent #3: no runtime history regressions

Agent #3 explicitly verified that careful past invariants still hold:

| Invariant | Status |
|-----------|--------|
| No D27 `external/unknown` leak for non-enriched `server="unknown"` | Holds |
| Localized D29 single local service node | Holds |
| No unknown-family fallback / no endpoint-backed pruning | Holds |
| D33 self-loop UID guard | Unchanged |
| Wholly-empty side drop | Unchanged |
| PROM/route creds env-only | Follows pattern |
| Route opt-in nil = pre-change behavior | Holds |
| `may_cross_cluster: true` for `pod-calls-service` | **Intentional** revise of localize D-L4 for route-engine edges |

---

### F8 — Additional code-comment drift (Agent #5, not re-scored individually)

All medium/low comment mismatches; treated as documentation hygiene under the same bar as F3.

| # | Location | Stale claim | Actual behavior |
|---|----------|-------------|-----------------|
| 1 | `servicegraph.go` ~80–84 (`sgResolver`) | Services only from `://` labels | Also route / ingress LB / chain |
| 2 | `servicegraph.go` ~310–314 (parse loop) | Every path yields exactly one ID | RouteHit chain returns `[ingress, backend]` |
| 3 | `servicegraph.go` ~662–669 (`resolveUnknownServerPeer`) | Route hit uses `resolveServiceLevel` | Also chain + `resolveServiceLevelInCluster` for LB |
| 4 | `routeresolve.go` ~47–54 (`RouteDestination`) | All destinations → `resolveServiceLevel` | LB / chain ingress hop use locked-cluster |
| 5 | `routeprescan.go` ~324–331 (`routeIndexResolve`) | Hit → `resolveServiceLevel` only | Also LB + chain expansion |
| 6 | `ingresspick.go` ~10–12 | Probe = window-overlap | Point-in-time live-at-`at` after simplification |
| 7 | `store/store.go` ~132–135 | “Range path” row types | As-of `LoadTrafficAt` only |

**Not flagged (Agent #5 checked OK):** D30 selector comments; D29/D27/D33 order; containment; ingress-role monotone; `pickIngressCluster` table; no open TODO/FIXME made worse.

---

### F9 — Agent #4: prior PR comments

**No applicable previous PR comments found.**

| Source | Result |
|--------|--------|
| PR #7 reviews / comments | Empty before this run |
| PRs #1–#6 | 0 issue comments, 0 review comments each |
| Repo-wide PR/issue comments API | Empty |

Offline / commit-message reviews (e.g. PR #4 “resolve 15 verified findings”) were never posted as GitHub threads, so nothing transferable was re-raised.

---

## 7. What was checked and found OK (cross-agent)

- Containment imports today: `pkg/build` / `pkg/kubegraph` do not import `pkg/route`.
- No Kubernetes client construction (linking types OK).
- Route opt-in via DSN; nil resolver preserves pre-change graph.
- I/O out of parse (prescan + prefetched index).
- `client_dns_answers` required for engine consult.
- D30 server matcher narrowed only (`server!~"user"`).
- Ingress `role` monotone set (`ingress-gateway` overwrites; `ingress-lb` only if unset).
- Chain edges UUIDv5 + traced-edge-wins.
- `pod-calls-service` `may_cross_cluster: true` intentional for route path.
- Locked-cluster fan-out for ingress LB / chain entry.
- Labels remain strict `map[string]string`.
- No new node / edge types.
- D29 localize core not re-expanded to multi-node fan-out.
- Auth / env credentials pattern consistent with PROM basic auth.

---

## 8. Posted GitHub comment (verbatim)

> ### Code review
>
> Found 1 issue:
>
> 1. Multi-IP `loadSnapshot` unions store rows with no dedup, so dual-stack / multi-A `client_dns_answers` that hit the same ingress Service load duplicate Gateway/VS rows. `ScopedFor` appends each matching VS twice into `Configs`, then istiod's in-memory `Create` fails with already-exists, `Translate` errors, and the endpoint degrades to `external` instead of the routed Service. Multi-IP is a first-class path (`parseDNSAnswers`, design D10) and is the common cloud LB / dual-stack shape; `candidates()` already dedups by `namespace/name`, but the snapshot union does not. Dedup rows (or `ScopedFor` configs) by resource identity before `Create`.
>
> https://github.com/akira-core/kube-state-graph/blob/1c7ff2156a6fff9bde7cacf2a92620ec5071c359/pkg/route/resolver.go#L150-L164
>
> https://github.com/akira-core/kube-state-graph/blob/1c7ff2156a6fff9bde7cacf2a92620ec5071c359/pkg/route/snapshot/snapshot.go#L142-L157
>
> https://github.com/akira-core/kube-state-graph/blob/1c7ff2156a6fff9bde7cacf2a92620ec5071c359/pkg/route/translate/translate.go#L297-L302
>
> 🤖 Generated with [Claude Code](https://claude.ai/code)
>
> <sub>- If this code review was useful, please react with 👍. Otherwise, react with 👎.</sub>

---

## 9. Recommended follow-ups (priority)

| Priority | Item | Notes |
|----------|------|--------|
| **P0** | Dedup multi-IP snapshot rows / ScopedFor configs | Posted issue; confidence 90; breaks common dual-stack / multi-A |
| P1 | Thread Gateway namespace through gwresolve / ScopedFor | Confidence 75; wrong-dest risk on multi-ns load |
| P1 | Ambiguity (not first-match) on multi Service/Deploy for same IP | Confidence 75; align with LB fallback “never guess” |
| P2 | Update `service-selects-pod` (and related) registry / edge-types prose | Confidence 75; user-facing catalogue |
| P2 | Wire `check-route-containment` into GHA | Confidence 50; CLAUDE.md claims CI enforcement |
| P3 | Refresh stale OpenAPI D29 localize paragraphs + D31 hierarchy | Pre-existing backlog |
| P3 | Align internal comments with chain / locked-cluster paths | Hygiene |

---

## 10. Metadata

| Item | Value |
|------|--------|
| Repo | `akira-core/kube-state-graph` |
| PR number | 7 |
| HEAD SHA | `1c7ff2156a6fff9bde7cacf2a92620ec5071c359` |
| Comment URL | https://github.com/akira-core/kube-state-graph/pull/7#issuecomment-5091701451 |
| Local report path | `code-review-report.md` |
| Build/typecheck | Not run as part of this review (by design — CI handles separately) |
