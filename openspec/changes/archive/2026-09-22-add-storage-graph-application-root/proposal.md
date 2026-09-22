## Why

`GET /v1/storage-graph` can be rooted at a filer, an aggregate, an SVM, a node
or one pod — but not at the unit an operator actually owns: an ArgoCD
Application. "What does `checkout` run, and which filer holds its data?" is
today answered by enumerating every pod of the Application by hand and sending
each as a `pod=<ns>/<name>` root, which the caller cannot do without a second
data source (the graph API resolves `data.application` on the way OUT, and the
label-values endpoint the front end enumerates roots from carries no
Application). The body already carries the answer on every pod and claim
(`data.application`, joined from the controller's tracking-id); what is missing
is a root that starts from it — and a build that loads the Application's pods
even when none of them mounts a claim, since the storage build reads pods BY
REFERENCE from claim bindings and a root that is never read is never drawn.

## What Changes

1. **A new workload-side root, `application=<argo-app>`**, optional and
   repeatable, on `GET /v1/storage-graph` and on the in-process
   `ParseStorageValues` / `BuildStorageFromValues` surface. A value is the
   ArgoCD Application name exactly as `data.application` carries it (the
   segment before the first `:` of the tracking-id), matched exactly and
   case-sensitively. Values of the selector are OR-combined; it is a
   **workload-side** root, so it unions with `pod=` (and `node=` hits on a
   Kubernetes node) on that side and is AND-combined with the storage side, per
   the existing root rule.
2. **A path is retained by an application root when its pod OR its claim
   carries that Application** — the claim's own annotation or the Application
   it inherited from a mounting pod, exactly as `data.application` already
   reports it. PVCs stay "never a root": they participate in retention, they
   are not materialised on their own.
3. **The Application's pods are recovered upstream before the pod read** — a
   new by-reference wave, gated on nothing but the request, that walks the
   controller-annotation families restricted by tracking-id prefix, then
   `kube_replicaset_owner` / `kube_job_owner` restricted by owner name (the
   Deployment → ReplicaSet and CronJob → Job hops in REVERSE), then
   `kube_pod_owner` restricted by owner kind and name, and adds the recovered
   pod names to the pod scope. This is a **candidate generator, never a
   judge**: the recovered pods are read like any other scoped pod, their
   Application is resolved by the existing forward resolver, and only the
   projection decides what is a root. An over-admitted pod (a controller whose
   lexically-smallest tracking-id names a different Application) is loaded and
   then dropped, so the body is a pure function of the forward resolution.
   Under an application root the claim-binding half of the pod scope is
   **narrowed** to the related pods only — those binding a claim that is
   own-annotated with a root Application or that a recovered / `pod=`-rooted
   pod mounts — so a large estate's unrelated mounting pods are never read.
   Every mounter of a drawable claim is still loaded, which keeps the split
   weight of a shared claim computed over the same mounter set as today.
4. **Every loaded pod carrying a root Application is materialised** with its
   ordinary attributes and compound parents and no edges — the `pod=` root
   rule, applied to the whole Application — so a stateless Application returns
   its pods rather than an empty body, and `?application=typo` returns an empty
   body because no pod resolves it.
5. `application=` **suppresses the pod-only namespace derivation** (an
   Application spans namespaces) and **composes with the storage-side
   `volume_labels` restriction** exactly as `pod=` does (the projection ANDs the
   two sides, so the restriction stays output-preserving).
6. Request-invariant everything else: no new node or edge type, no new
   `labels` key, no new flag, `queryDims` unchanged, the Harvest legs unchanged,
   `/v1/graph` unchanged. A request without `application=` issues byte-identical
   queries and produces a byte-identical body.

**BREAKING (Go embedders only):** `graph.NewStorageScope` gains an
`applications []string` parameter and `graph.StorageRoots` gains an
`Applications map[string]struct{}` field. The HTTP contract is purely additive.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `storage-graph-api`: "Root selectors from either end of the flow" gains
  `application=`; "Roots are always materialised when the upstream knows them"
  defines when an application root exists and what it materialises;
  "Storage-reachability projection" adds the pod-or-claim retention rule;
  "Storage build reads only what it draws" adds the application reverse read
  as a further input to the pod scope and specifies its three stages, error
  classes, chunking, tally and the bounded-or-unrestricted fallback for its
  request-derived first stage; "Pod-only roots narrow the upstream read" is
  suppressed by an application root; "Storage-side roots narrow the Harvest
  topology read" states that `application=` composes like `pod=`;
  "Deterministic storage-graph body" adds the new root to the identity tuple.
- `cluster-topology-source`: "Topology series consumed" — the by-reference
  paragraph names the application reverse read as a further restriction the
  storage build issues the controller-annotation, owner and pod-owner families
  under (tracking-id prefix and owner kind/name, beside the identity-label
  restrictions), and states how those extra reads are tallied.

## Impact

- `pkg/graph`: `StorageRoots.Applications`, `NewStorageScope` signature,
  `RequestedWorkload`, `resolveStorageRoots` / `ProjectStorage` (pod-or-claim
  retention, pod materialisation), `project_storage_test.go`,
  `storagescope_test.go`.
- `pkg/kubegraph`: `ParseStorageValues` accepts and validates `application`;
  `deriveStorageNamespaces` treats it as a non-pod root; parse tests.
- `pkg/promql`: two new renderers — a tracking-id **prefix** restriction on the
  six controller-annotation families and an **owner-kind/owner-name**
  restriction on `kube_replicaset_owner`, `kube_job_owner` and `kube_pod_owner`
  — both composed with the fixed selector and request matchers like
  `RenderScoped`; `render-baseline.txt` unchanged.
- `pkg/build`: `topologyPlan.applicationRoots`; a new `appscope.go` wave
  (three stages) launched before the pod wave; `readScopedPods` waits on it as
  well as on the claim-binding family; a tally hook for reads that do not land
  in a `topologyVectors` slot; `TestBuildStorage_FanOutLegCount` gains the
  application rows; parity pin extended with an Application root.
- `internal/api`: swag annotations for the new parameter; `make docs`
  (`docs/swagger.{json,yaml}`); a golden `storage-graph-application-root`
  fixture; `storage_graph_test.go` captures the reverse-read queries.
- Docs: `docs/upstream-metrics.md` (storage fan-out section), `docs/BREAKING.md`
  (non-breaking HTTP entry + the embedder signature move), `README.md`,
  `README.zh-tw.md`, `CLAUDE.md` (request surface, lifecycle block, test list).
- Consumers: the demo front end (`kube-state-graph-frontend`) can add an
  `Application` root control; its values come from the label values of
  `annotation_argocd_argoproj_io_tracking_id` on the annotation families,
  split at the first `:` — out of scope here, noted so the demo's `verify.sh`
  §10/§11 are not touched by this change.
- No new dependency, no new flag, no new self-metric label.
