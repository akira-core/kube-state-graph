# Design — harden-istio-route-resolution

Corrective follow-up to the route-resolution engine (parent:
translate-global-fqdn-to-k8s-service; time semantics per
simplify-route-resolution-to-point-in-time; gateway scoping per
scope-gateway-candidates-to-ingress-namespace). Settled decisions:

## D1 — Multi-IP dedup belongs at the snapshot boundary, not in `translate`

`translate.Translate` is a pure function of its `ScopedInput`; making it tolerate a
duplicated config would hide the defect from every other consumer and make the input's
contract "whatever istiod happens to accept". The union is created in
`Resolver.loadSnapshot`, so that is where identity dedup belongs.

`ScopedFor` and `backendServices` additionally refuse to emit a duplicate. This is not
redundancy for its own sake: `store.Store` is an interface, a different implementation may
return overlapping rows from a single call, and the snapshot package's stated job is to
answer exactly what a point-in-time store query would. Two layers, one contract each —
"the union carries each version once" and "the translate input names each resource once".

Row identity is `(Cluster, Namespace, Name, ValidFrom)`. `Name` alone is insufficient
(the cross-namespace hazard of D3); `IngestSeq` must NOT participate — a rewrite twin and
its closing row share a version slot and are collapsed by `dedupLiveAtCounted` before the
union ever sees them.

Duplicate `model.Service` entries are harmless today (istiod's mem registry tolerates
them), but the same rule applies for the same reason.

## D2 — Bare short destination hosts resolve; dotted relative forms do not

istiod's `ResolveShortnameToFQDN` expands a destination host **only when it contains no
dot**: a dot-free name gets `.<namespace>` and then `.svc.<meta.Domain>`, and everything
else is returned verbatim. Our `config.Meta` carried no `Domain`, so the expansion stopped
half-way and produced `checkout.shop` — a string that is neither a short name nor an FQDN
and that no parser downstream can classify.

Setting `Domain` reproduces production istiod exactly (the real config store stamps the
mesh domain suffix on every config it serves), so the fix is to supply the field rather
than to loosen a parser.

`checkout.shop` and `checkout.shop.svc` written by hand in a VirtualService are therefore
**not** made to work. istiod leaves them verbatim, looks them up as literal registry
hostnames, finds nothing, and a real Envoy 503s. Accepting them in `ParseBackendHost`
would invent a destination Istio itself does not route to and would report a Service edge
for traffic that is in fact failing. The external fallback is the honest answer.

Consequently `ParseBackendHost`'s input is now guaranteed to be a full FQDN, and it is
tightened from "at least two labels" to "exactly two": `mysql-0.mysql.db.svc.cluster.local`
stops silently parsing as `mysql/mysql-0`.

Destination hosts are normalised at **collection** time (`vsDestHosts`, which has the
owning VirtualService's namespace at both call sites) rather than at parse time, so the
backend-Service key list, the ClickHouse backend query, and the translate registry all
agree on one identity. The store reader and the snapshot package keep byte-identical
copies of that helper, as their comments already require.

## D3 — Gateway identity is `(namespace, name)` end-to-end

scope-gateway-candidates-to-ingress-namespace argued that namespace-scoping Hop 3 made the
bare-name identity unambiguous *by construction*. That holds for the candidate set, but
not for `ScopedFor`, which scans the whole loaded `Gateways` slice — and the load is a
deliberate superset: the `gw_versions` SQL binds `nsList`, the union of every ingress
Service namespace carrying the IP, and `candidates()` unions per-IP results whose Hop-1
namespaces may differ. A same-named Gateway in a second namespace could therefore be
selected, and because the selected row's namespace also feeds `boundTo`, its
VirtualServices came with it — a wrong destination, not a miss.

This is the option-1 extension path that change explicitly deferred. The narrowing it
chose (a candidate Gateway must live in the ingress Service's namespace) is retained;
only the identity carried downstream is widened, so no resolution outcome changes for any
configuration the previous change already resolved.

`gwresolve` keeps operating on host patterns; the namespace travels alongside rather than
inside the matcher, so `PickHosts`' numeric-index semantics and `sortPats`'
lexically-smallest tie-break are untouched.

## D4 — Hop 1 degrades, Hop 2 unions

The two hops take a first match today, but the correct fix differs per hop because the
question each answers differs.

**Hop 1 asks "which Service owns this IP".** More than one live answer means the question
has no answer — exactly the situation `ingressServiceIdentity` already degrades on for the
LB fallback. Hop 1 now matches that rule, so one IP cannot resolve through the Istio path
and be ambiguous on the LB path simultaneously.

**Hop 2 asks "what labels does the ingress workload carry".** Several matching Deployments
is a *normal* state — a revision-based canary gateway upgrade runs two Deployments whose
pods both satisfy the Service selector — so degrading would break a healthy cluster. The
union is also what the SQL layer already computes (`labelUnion`), and Hop 3 is a subset
test over that set, so the union is the semantically faithful answer and re-aligns the two
layers that were supposed to mirror each other.

Neither hop gains a lexical tie-break: Hop 1 has no answer to pick from and Hop 2 needs no
pick.

## D5 — Bounded concurrency, and what determinism it does and does not owe

Per key the engine performs one cross-cluster probe, four to five ClickHouse queries per
IP, one istiod translation, and one `router_check_tool` fork/exec (~50–60 ms). The loop
was serial and uncapped, on the request path, with no result cache — a few hundred
distinct FQDNs meant tens of seconds per `/v1/graph`.

Resolution runs under an `errgroup` with a fixed limit. A fixed constant is chosen over a
flag deliberately: the useful value is bounded by `router_check_tool` process cost rather
than by anything an operator can observe, and the repo's rule is to not add tuning knobs
speculatively.

`routeIndex` is keyed by `routeKey` and every entry is independent, so concurrency cannot
change the index's contents. What it does change is *which* keys are answered when the
build deadline fires first — but that was already wall-clock dependent in the serial loop,
so no determinism guarantee is weakened. The deterministic-body contract covers a
successful build, and a successful build answers every key in either scheme.

The key set gains a cap. Truncation is logged with the dropped count, never applied
silently, per the repo's no-silent-caps rule.

`scopedResolver`'s probe memo becomes mutex-guarded. Two concurrent misses on one IP may
both reach the store; that is accepted rather than serialised behind a singleflight,
because the probe is idempotent, the window is one build, and the alternative adds a
dependency to save at most one query.

## D6 — Error returns carry no outcome

`Resolver.resolve` returned `RouteNoGateway` alongside a non-nil error at three sites, and
`RouteNoGateway` is precisely the ingress-LB-fallback gate. Nothing misbehaves today only
because `resolveRouteQueries` inspects `err` first — an unwritten ordering contract. The
sites now return the empty outcome, which `resolveConfig` already does internally. A
no-op that removes a trap.

## D7 — `parseActuals` fails closed on an incomplete parse

`matchcheck.Resolve` is an N-query batch API by signature; the scraped `--details` text
format is not a stable contract (D8 of the parent change rests on the *matcher* being
Envoy's, not on its output format). Today only n=1 is exercised and the `filled`
first-wins guard makes a stray numeric line fail closed. Requiring one recovered result
per query makes that property hold for any n instead of by accident.

## D8 — Backend Service rows carrying an ingress IP stay ingress candidates

Rejected as a defect. `external_ips` / `loadbalancer_ips` are the only signal the schema
offers, and a Service publishing an externally reachable address **is** an entry point,
whatever else it also serves — it is displayed through the same ingress-service path as a
LoadBalancer. Several Services live at one IP is a genuine ambiguity (MetalLB's shared-IP
annotation makes it a supported configuration), and degrading is the correct answer there,
consistent with D4's Hop 1.

The discriminator proposed in review — an empty port list identifies an ingress row —
would have been actively harmful: it describes the test corpus, not production, where the
exporter writes the full Service spec and a real ingress Service carries ports 80 and 443.
It would have filtered every production ingress row out of Hop 1.

## D9 — Toolchain image pinned by digest, Dockerfile as the single source

`matchcheck`'s correctness argument is that the matcher is Envoy's own, and the result is
scraped from its text output. A floating `tools-v1.34-latest` tag can change either with no
code change, no CI signal, and nothing to diff. The digest is declared once, as a
Dockerfile `ARG` consumed by its own `FROM`, and the Makefile extracts it from there for
the CI install step — one value, no drift check needed.

## Risks

- **Bare-short-name resolution is new behaviour**: an endpoint that previously fell to
  `external` may now produce a `pod-calls-service` edge. That is the intent, but it does
  change the graph for such deployments.
- **Hop 1 degradation is new behaviour**: an IP owned by several live Service identities
  previously resolved through whichever row came first; it now degrades. Affected
  configurations were already non-deterministic.
- **Concurrency**: the resolver documented itself as safe for concurrent use, but that is
  now load-bearing rather than aspirational. `scopedResolver` is the one piece that was
  not, and is fixed here.
