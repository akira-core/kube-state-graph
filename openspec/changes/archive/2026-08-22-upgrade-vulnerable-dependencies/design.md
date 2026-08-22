# Design — upgrade-vulnerable-dependencies

Settled decisions:

## D1 — Upgrade istio rather than allowlist its two findings

Both istio findings are unreachable in practice (proposal.md), and
`openspec/specs/static-analysis-suite/spec.md` explicitly permits suppression:
"suppressions SHALL be made explicit via comment plus a tracking issue, never
via silent ignoring". An allowlist was therefore a legitimate option, and is
rejected for two reasons.

First, it is permanent maintenance debt with no expiry: govulncheck offers no
suppression mechanism, so the allowlist would mean wrapping `make vuln` in
JSON-filtering tooling that every future contributor has to understand.

Second and decisively, the allowlist would have to key on the **module**, not on
the specific finding — a per-ID list goes stale the moment istio publishes its
next advisory, and a module-level exclusion would mask a future istio
vulnerability that IS exploitable here. The gate's whole value is that it
notices.

The upgrade cost turned out to be bounded: one removed constructor, one added
controller, one dropped registry. The bare-short-name resolution this branch
depends on rests on `ResolveShortnameToFQDN`, which is byte-identical at the
target commit.

## D2 — The translation world gains a lifecycle instead of staying static

The package's original contract was "no controllers running, no goroutines",
justified by `PushContext.InitContext` reading VirtualServices straight from the
store. That justification no longer holds: istio moved merging (delegates,
`exportTo` defaults) into `model.VirtualServiceController`, whose
`MergedVirtualServices()` lists a **krt collection** fed by event delivery.

Three properties forced the shape of the fix, each established by observation
rather than assumed:

1. `Environment.VirtualServiceController` is a **concrete pointer**, not an
   interface, so no minimal stand-in can be substituted — and `InitContext`
   dereferences it unconditionally (a nil one panics).
2. Constructing it over an already-populated store is **not** enough: the
   derived collections still report unsynced, `MergedVirtualServices()` lists
   nothing, and every route resolves to no cluster. Waiting for `HasSynced` is
   required, which is exactly what istiod's own harness does.
3. Its `Run(stop)` is a **shutdown hook** (`<-stop; close(c.stop)`), not a sync
   loop — so the goroutines belong to the krt collections and are reclaimed only
   when that internal stop channel closes. Without the cleanup, each translation
   leaked ~115 goroutines, which `TestTranslatorNoGoroutineLeak` catches.

Hence: create configs → construct controllers → `go Run(done)` → wait for sync →
translate → `close(done)`. The cleanup is returned by `buildScopedEnv` and
deferred by `Translate`, so it cannot be forgotten by a future caller.

The sync wait is a bounded poll (5 s ceiling, 100 µs interval) that returns an
error rather than hanging: route resolution can never fail a build, so a
translate error degrades the endpoint to an external node exactly as any other
translate error already does.

## D3 — The ServiceEntry registry is removed, not faked

istiod's `NewConfigGenTest` wires a ServiceEntry registry, and this translator
copied that. `serviceentry.NewController` now additionally requires a
`*multicluster.Controller`, which istiod's harness satisfies with
`kube.NewFakeClient()`.

Building a Kubernetes client — fake or not — is precisely what design D0
forbids in this package. The registry was also never consulted: `ScopedInput`
carries Gateway and VirtualService configs only, and backend Services arrive
through `ScopedInput.Services` into the in-memory registry. Removing it is
therefore both the compliant and the honest option; keeping a faked one would
have preserved a dependency the translator does not have.

## D4 — istio.io/api follows istio, and the beta→alpha version line is not a choice

`istio.io/api` is a direct dependency (the `networking/v1alpha3` protos), pinned
at v1.26.0-beta.0. The target `istio.io/istio` requires
v1.29.0-alpha.0.0.20260327042620-ea30db2515c3, so MVS raises it regardless; the
explicit `go get` only makes the go.mod entry honest. The `networking/v1alpha3`
package is retained at that version and the message types used here (Gateway,
VirtualService, Server, Port, ServerTLSSettings, HTTPRoute, Destination,
PortSelector, StringMatch) are unchanged.

"alpha" here labels istio's own release train, not the stability of the protos
this repository reads.

## D5 — Validation rests on the oracle sweep

Unit and e2e tests compare against expectations this repository wrote, which a
subtly different istiod could satisfy while routing differently. The
`-tags oracle` sweep is the exception: its expected clusters are computed **by
construction** from the generated corpus, independently of istiod, which makes
it the right instrument for an istiod version change. It requires the native
Linux `router_check_tool`, so it runs in a Linux container locally.

Zero mismatches is the acceptance bar.

## Risks

- **istio master snapshot, not a release**: `istio.io/istio` publishes no tagged
  versions, so any version carrying both fixes is a master commit. This is
  pre-existing (the previous pin was also a master snapshot) but the jump is
  ~11 months.
- **Per-translation cost rose ~7x**, measured rather than estimated: an
  identical 500-iteration benchmark of `Translate` over the same scoped input
  gives ~59 µs/op before the upgrade and ~433 µs/op after (medians of six runs,
  same machine). Nearly all of the increase is istio's own doing — the
  VirtualService controller builds a graph of ~20 krt collections per
  translation. Only ~55 µs of it is the sync wait's poll interval: replacing
  `time.Sleep` with `runtime.Gosched()` measures ~377 µs/op, which is not worth
  burning a core to spin for.

  In context the absolute number stays small: one route resolution also forks
  `router_check_tool`, which the oracle sweep measures at ~48 ms, so translation
  went from roughly 0.1% to 0.8% of a resolution. Worth revisiting only if the
  matcher ever stops dominating.
- **k8s.io/\* moved three minors**: linked-only, and the containment gate covers
  the rule that matters (`pkg/build` / `pkg/kubegraph` must not reach client-go).
