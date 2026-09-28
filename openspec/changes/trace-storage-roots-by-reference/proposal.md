# Proposal

## Why

A `/v1/storage-graph` read still scales with the ZONE, not with the root. A `node=`, `pod=` or `application=` root reads every claim, every claim binding and the whole `volume_labels` family of the requested `az` / `env`, and even a storage-rooted (volume-hub) build still reads the zone's whole Harvest aggregate, controller and QoS-policy inventory. That puts every root on the path to the upstream per-query series limit, costs VictoriaMetrics CPU on data the projection discards, and adds latency proportional to the estate. At the same time, cross-kind root composition (`pod=` with `aggr=`, `ontap_cluster=` qualifying `aggr=`, `node=` matching two tiers) multiplies the read plans the build must keep output-preserving and forces an AND/OR projection whose flowless-root rule already draws surprising bodies.

## What Changes

- **BREAKING** A request SHALL carry exactly ONE root kind, with one or more values of it (OR-combined). A second root kind is rejected 400 `invalid_scope`. Cross-kind AND composition is withdrawn.
- **BREAKING** A root is REQUIRED. A request with no root is rejected 400 `missing_root`; the endpoint no longer serves the whole selected estate.
- **BREAKING** `node=` names a Kubernetes node ONLY. A new root kind `ontap_node=` names an ONTAP controller and retains the claims on the aggregates it owns (the existing owner vote), and the pods, applications and namespaces below them. An `ontap_node=` request and a `node=` request can no longer be mixed. A client still sending an ONTAP controller name as `node=` receives an EMPTY 200.
- **BREAKING** `ontap_cluster=` is its own root kind (every controller, aggregate and SVM of a filer) and no longer qualifies `aggr=` / `svm=`. A bare `aggr=` / `svm=` value matches that name on every filer of the zone.
- **BREAKING** The claim-to-FlexVol join always SUFFIX-matches. The `exact`, `contains` and `regex` volume match modes are removed, on both endpoints, and the `--netapp-volume-match-mode` flag and `KSG_NETAPP_VOLUME_MATCH_MODE` variable are deleted. Tracking needs a join that renders as an anchored PromQL alternation and can be inverted from a FlexVol name back to a PersistentVolume name, and only `suffix` gives both. The rewrite rules (`--netapp-volume-key-rewrite`) stay.
- Every root kind is read by TRACKING instead of a fan-out over the zone: a per-kind seed walks from the root to the set of claims it reaches, and one shared expansion walks that claim set out to both ends of the tier chain (`netapp-node → aggr → svm → pvc → pod → Kubernetes node`, plus the pod's controllers and Application). The expansion carries the completions that keep the body identical to an unrestricted read: aggregate owner completion, claim candidate completion, claim mounter completion, and pod incarnation completion.
- Every family a storage build reads is read BY REFERENCE, including the Harvest aggregate and controller gauge families and the QoS fixed-policy ceilings. `ALERTS` is the one family still read across the whole zone.
- A scope derived from the REQUEST (the number of root values) past its cap is rejected 400 `invalid_scope`; the fallback to an unrestricted read is removed. A scope derived from upstream DATA is chunked and never falls back to an unrestricted read.
- The storage projection retains a path iff it touches a root of the request's one kind; the cross-side AND, the `node=` two-tier match and the `ontap_cluster=` qualifier are removed. Roots known upstream are still materialised when nothing flows through them.
- Removed as superseded: the pod-root namespace derivation (a pod seed is keyed by `<namespace>/<pod>`), the volume-hub on/off decision and its unrestricted fallback (every storage build is by reference), and the application-root / storage-root composition rule.
- Unchanged: `/v1/graph`'s read plan and request contract (its join follows the suffix-only rule above); the storage body shape, tier chain, flow weights, determinism and fail-closed rule; `az` / `env` still required and single-valued; `cluster=` / `namespace=` still optional Kubernetes-side filters combinable with any root kind.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `storage-graph-api`: the root contract (one kind, required, `node=` Kubernetes-only, new `ontap_node=`, `ontap_cluster=` standalone, bare `aggr=` / `svm=` across filers), flowless-root materialisation per kind, the storage-reachability projection, and the read plan (per-kind seed, shared expansion and completions, every family by reference except `ALERTS`, request-derived caps rejected). The volume-hub, pod-only-namespace and application-composition requirements are replaced.
- `netapp-storage-graph`: the PV-name-to-FlexVol-name derivation is suffix-only (the match-mode setting is withdrawn). On the storage build, the Harvest topology family is additionally read by controller, and the aggregate / controller gauge families and the QoS fixed-policy families are read by reference to the aggregates, controllers and SVMs the tracked claims reach (or the root names), instead of zone-wide.
- `cluster-topology-source`: the storage build's by-reference family table gains the new scope keys (pods by Kubernetes node, claim bindings by pod, claim annotations by Application tracking-id) and loses every unrestricted leg except `ALERTS`.

## Impact

- **HTTP API** (`GET /v1/storage-graph`): new `ontap_node` parameter; new 400 `missing_root`; `invalid_scope` for mixed root kinds and over-cap root sets; `node=` semantics narrowed. OpenAPI regenerated (`make docs`), `docs/BREAKING.md` updated.
- **Engine surface** (`pkg/`): `graph.StorageRoots` / `graph.NewStorageScope` change shape (one kind plus values), which `kubegraph.Engine.BuildStorage` and `build.Builder.BuildStorage` take; `build.VolumeMatchMode` and its constants are removed, and `build.NewVolumeKeyRewriter` loses its mode argument — breaking changes for embedders such as `graph-api-gateway`.
- **Configuration**: `--netapp-volume-match-mode` / `KSG_NETAPP_VOLUME_MATCH_MODE` are deleted (no deployment sets them). Passing the flag fails startup as an unknown flag; the variable is no longer read. A deployment that set `exact` sees no body change for a FlexVol named exactly the token, since such a name also ends with it. A deployment that set `contains` loses clone attribution. A deployment that set `regex` must express its naming through the rewrite rules or stop joining. `docs/netapp-harvest-preconditions.md` is updated.
- **Code**: `pkg/kubegraph/parse.go`, `pkg/graph/storagescope.go`, `pkg/graph/project_storage.go`, `pkg/build/topologyplan.go` and the scoped-read files (`volumelabelscope.go`, `claimscope.go`, `podscope.go`, `nodescope.go`, `controllerscope.go`, `appscope.go`, `qosscope.go`), new seed files, new scoped renderers in `pkg/promql`, `internal/api/errors.go`. The storage-plan fan-out pins, the storage goldens and the storage integration tests are rewritten.
- **Upstream load**: series read per storage request becomes proportional to the root's reach; the number of sequential upstream rounds grows by roughly two to three; storage-build query-cache hits across different roots become rarer.
- **Downstream repositories** (separate changes): the frontend Sankey must send exactly one root kind and gain an `ontap_node` kind; the demo's `verify.sh` must root its storage checks accordingly and add an `ontap_node=` check.
