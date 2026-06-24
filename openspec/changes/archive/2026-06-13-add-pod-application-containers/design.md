## Context

`kube-state-graph` builds an immutable `*Graph` from a fixed fan-out of PromQL
queries against the centralised VictoriaMetrics, then projects + serialises it to
Cytoscape JSON. Pod nodes already carry typed, nullable attributes derived from
kube-state-metrics joins — `ipaddress` (from `kube_pod_info`), `owner` (from
`kube_pod_owner` with the ReplicaSet skipped to its Deployment via
`kube_replicaset_owner`), and `storageclass` (PVC-only). These live on **typed
attributes**, never in `labels` (which stay a strict `map[string]string` of
typological metadata) — see CLAUDE.md "IP addresses live on the typed `ipaddress`
attribute" and "Pod controller-owner attribute (D34)".

This change adds two more pod attributes following exactly that precedent:

- **`application`** — the ArgoCD Application a pod belongs to, derived from an
  ArgoCD tracking-id label the operator exposes on `kube_pod_owner`.
- **`containers`** — the list of `{name, image}` the pod is running, from the
  kube-state-metrics default metric `kube_pod_container_info`.

Constraints inherited from the architecture: no Kubernetes API access (all facts
from VictoriaMetrics — D1/D16); no filters pushed to PromQL; deterministic,
byte-identical response bodies (golden tests); `labels` stays strict
`map[string]string`; the sealed `graph.GraphNode` interface is the only
serialisation surface (no type switches in the serialiser).

## Goals / Non-Goals

**Goals:**

- Surface `data.application` (string) and `data.containers` (`[{name, image}]`)
  on `type="pod"` nodes, both `omitempty`.
- Read per-container name/image from one new `kube_pod_container_info` query
  (topology fan-out 10 → 11).
- Read the ArgoCD Application from the **existing** `kube_pod_owner` query — no
  extra query.
- Preserve every load-bearing contract: typed-attribute-not-labels, determinism,
  graceful degradation when the source series are absent, prefix coverage for the
  new kube_* metric.

**Non-Goals:**

- Surfacing container state/readiness, restart counts, resource requests/limits,
  `image_id` digests, or `container_id` — scope is `{name, image}` only.
- Surfacing the full ArgoCD tracking-id (`<app>:<group>/<kind>:<ns>/<name>`),
  project, sync status, or revision — scope is the Application **name** only.
- Any new node type, edge type, HTTP route, or numeric/boolean attribute.
- Pushing a label/annotation allowlist requirement onto kube-state-metrics for the
  container metric (it is a KSM default; see Decisions).

## Decisions

### D-A1: `application` is read from the existing `kube_pod_owner` query, not a new one

`kube_pod_owner` is already queried for the controller-owner attribute. The
operator exposes the ArgoCD tracking-id on that same series (via a
kube-state-metrics labels/annotations allowlist), so the Application is a free
join on data already in hand. Adding a second query would duplicate cardinality
for no gain.

*Alternative considered:* a dedicated `kube_pod_labels` / `kube_pod_annotations`
query. Rejected — the user's deployment carries the value on `kube_pod_owner`, and
reusing the existing read keeps the fan-out at +1 (only the container query).

### D-A2: the label is named `argocd_tracking_id`; the Application is the segment before the first `:`

The reader inspects each `kube_pod_owner` series for a label named
`argocd_tracking_id`. ArgoCD's annotation-based resource tracking writes a value of
the form `<app-name>:<group>/<kind>:<namespace>/<name>`; the **Application name is
the substring before the first `:`**. When the value contains no `:` (already a
bare app name, or a custom relabel), the **whole value** is surfaced verbatim. An
empty/absent label yields no `application` (nil → `omitempty`).

This parse rule is robust to both forms (full tracking-id and bare name) and is a
pure function, so it preserves determinism. The exact upstream label name is the
operator's kube-state-metrics-allowlist responsibility — the parse is independent
of it (see Open Questions for confirming the literal name).

*Alternative considered:* surface the raw tracking-id verbatim as `application`.
Rejected — the attribute is named `application`, and a raw `app:apps/Deployment:ns/name`
string is not the Application name. The leading-segment rule degrades to verbatim
anyway when the value is already bare.

### D-A3: Application value is pod-level, read independently of the controller-row pick

The controller-owner resolution filters to `owner_is_controller="true"` and picks
the lexically-smallest `(kind, name)` on collision. The ArgoCD label is a
**pod-level** fact and is expected to be identical across every `kube_pod_owner`
row for a pod, so it is read independently of which row wins the controller pick
(it must survive even when no row is a controller). On the pathological case of
differing non-empty values across rows for one pod, the reader picks the
**lexically-smallest non-empty** value — deterministic, order-free.

### D-A4: `containers` from a new `kube_pod_container_info` query, joined on `(cluster, namespace, pod)`

`kube_pod_container_info{cluster, namespace, pod, uid, container, image, ...}`
emits one series per container (per image — `image` is a label). It is a
**kube-state-metrics default** metric (no allowlist needed), unlike the
endpointslice service-name label. The reader issues one
`tlast_over_time(kube_pod_container_info[<window>])` query (the 11th in the
topology errgroup) and joins each series onto its pod by `(cluster, namespace,
pod)` — the key always present on both `kube_pod_info` and the container metric.
`container` → `name`, `image` → `image`.

**Latest-image pick via `tlast_over_time`.** A container that changed image in the
window has two series (old + new `image` label). `last_over_time` returns each
series' last *value* (always `1`) stamped at the eval instant — recency is lost,
so it cannot distinguish current from stale. `tlast_over_time` instead returns
each series' **last-sample timestamp** as the value; the resolver argmaxes over it
to pick the current image (greatest timestamp; lexically-smallest image on an
exact tie for determinism). This was verified empirically against the pinned VM
`v1.107.0`: for near-now query windows both image series come back with correct
last-sample timestamps and the newer one wins. **Caveat (load-bearing):** for
query windows far from the real wall clock (verified at the integration suite's
`fixedNow`, ~6 weeks back, and worse multi-year), VM returns only ONE
image-variant series per container — true for `last_over_time`, `tlast_over_time`,
AND `query_range` alike (a single-variant series still resolves correctly there).
So "latest" is meaningful for the near-now case (the dominant one — the API's
default and typical windows are recent); a far-past window surfaces whatever
single variant VM returns. This is never worse than a fixed lexical pick, and the
multi-variant "latest" path is therefore covered by unit tests (which control the
vector) rather than the `fixedNow`-anchored integration suite.

### D-A5: container list is sorted by `(name, image)`; both attributes degrade gracefully

The per-pod container slice is sorted by `(name, image)` before assembly so the
serialised body is byte-identical across rebuilds (golden-test contract). Both
reads are **OPTIONAL**: absent `kube_pod_container_info` → every pod omits
`containers`, no build failure; absent/empty ArgoCD label → pod omits
`application`. This mirrors the owner / PVC / service families' tolerance.

### D-A6: both surface as typed attributes on the sealed `GraphNode`, never in `labels`

Extend `graph.GraphNode` with `Application() string` and `Containers()
[]Container`, returning `""` / `nil` for the four non-pod kinds (same shape as
`Owner()` / `StorageClass()`). Add `type Container struct { Name, Image string }`
and the backing fields on `PodNode`. The serialiser emits `data.application`
(`omitempty` on `""`) and `data.containers` (`omitempty` on empty). `labels` is
untouched — it stays a strict `map[string]string`.

### D-A7: `kube_pod_container_info` joins the `KSG_METRIC_PREFIX` set

The new metric is kube-state-metrics-shaped, so the configurable upstream prefix
(`promql.Renderer{Prefix}`) applies to it, and the prefix-coverage requirement's
enumeration gains it. The `argocd_tracking_id` label rides on the already-prefixed
`kube_pod_owner` — no separate prefix concern. The `Query` constant
(`QPodContainerInfo`) stays the bare metric name so the `query_name=` self-metric
dimension and span attributes are stable across prefixed/unprefixed deployments;
the new query contributes one new `query_name` value.

## Risks / Trade-offs

- **[Cardinality] `kube_pod_container_info` is per-container, higher cardinality
  than the per-pod metrics.** → It is a single `last_over_time` snapshot query like
  the other ten; bounded cost is delegated to upstream VictoriaMetrics search
  limits (the existing no-pushdown contract). No per-request filtering added.
- **[ArgoCD label name uncertainty] the literal upstream label name may differ
  from `argocd_tracking_id` (KSM sanitisation could yield
  `label_argocd_argoproj_io_instance` / `annotation_argocd_argoproj_io_tracking_id`).**
  → Committed default is `argocd_tracking_id` (the operator's stated name); the
  parse rule is name-independent. Confirm the literal name before implementation
  (Open Questions) — it is a one-line constant.
- **[Image mutability within window] a container whose image changed inside the
  window surfaces as TWO distinct series (image is a label).** → The query is
  `tlast_over_time` (value = each series' last-sample timestamp) and the resolver
  argmaxes over it to pick the **latest** image (see D-A4). For near-now windows
  (the dominant case) this returns the current image. **Caveat:** for query
  windows far from the real wall clock VM returns only ONE image-variant per
  container (verified at `fixedNow`; affects `last_over_time`, `tlast_over_time`,
  and `query_range` alike), so a far-past window surfaces whatever single variant
  VM returns — never worse than a fixed lexical pick. A truly window-agnostic
  "latest" would need a `query_range` matrix plus client-side recency
  reconciliation (a new `Querier.Range` method) — deferred unless historical-window
  image accuracy becomes a requirement.
- **[Tracking-id format assumption] a value using `:` for something other than the
  app delimiter would mis-parse.** → ArgoCD's tracking-id format fixes the leading
  segment as the app name; the no-`:` fallback covers bare values. Low risk.

## Migration Plan

Purely additive — no schema break, no data migration, no rollback complexity.
Existing golden bodies for pods without ArgoCD/container data are unchanged
(`omitempty`). Golden snapshots that *do* introduce the new fixtures are refreshed
with `go test ./internal/api/ -update -run Golden`. Operator prerequisites:
`kube_pod_container_info` ships by default in kube-state-metrics (no action); the
ArgoCD Application requires the operator's existing labels-allowlist exposing the
tracking-id on `kube_pod_owner` (already in place per the request).

## Open Questions

- **Literal upstream label name for the ArgoCD tracking-id on `kube_pod_owner`.**
  Committed default: `argocd_tracking_id`. Confirm against the operator's actual
  kube-state-metrics configuration before coding the constant.
- **Should `containers` later carry `image_id` (resolved digest) for
  reproducibility?** Deferred; current scope is `{name, image}`. If added, it is an
  additive `omitempty` field on the `Container` struct — not a v1 break.
