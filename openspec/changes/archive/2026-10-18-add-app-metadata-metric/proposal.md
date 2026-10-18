# Proposal

## Why

A pod's ArgoCD Application is recovered today by walking its controller chain —
`kube_pod_owner` → `kube_replicaset_owner` → six `kube_*_annotations` families
→ `kube_job_owner` — and only resolves when the operator has allowlisted the
tracking-id annotation on every controller kind. Some estates already publish a
purpose-built metric that names each pod's Application directly, together with
facts the controller chain can never yield: the last user who deployed the
Application and the owner of the namespace. There is no way to feed that metric
in, so consumers see no deployer or namespace owner, and the Application of a
pod whose controller is unannotated (or a CRD kind) stays empty.

## What Changes

1. **An optional, operator-named application-metadata metric.** A new setting
   names a per-pod metric (`--app-metadata-metric` / `KSG_APP_METADATA_METRIC`);
   unset (the default) disables the feature and the build is byte-for-byte what
   it is today. A second setting (`--app-metadata-labels` /
   `KSG_APP_METADATA_LABELS`, `key=label` pairs) maps the five logical fields —
   `app`, `user`, `ns_owner`, `namespace`, `pod` — to the metric's label names,
   each with a default. `cluster` is always read from `cluster`; `az` / `env`
   use the existing `--az-label` / `--env-label` binding. Configuration is env /
   flag only, not a file.
2. **A seventh query family, `appmeta`, optional like `alerts`.** The metric is
   dispatched to the backends declaring `appmeta`. The implicit single-backend
   table serves it; an existing backends file that does not list it stays
   valid and the leg is simply not issued.
3. **The metric takes precedence over the controller chain for a pod's
   Application, per pod.** A pod with a metric row carrying a non-empty
   Application takes that value; every other pod resolves through the existing
   controller chain unchanged. Service Applications are unchanged; PVC
   inheritance follows the pod's resolved value as it does today.
4. **New typed attributes on two synthesised compound groups.** The `namespace`
   group carries `data.namespace_owner` and the `application` group carries
   `data.deployed_by`, both `omitempty` strings sourced only from the metric,
   never inside `labels` (which stay `{}`).
5. **`/v1/storage-graph` reads the metric in every build**, restricted to the
   namespaces of the loaded pods and claims, so a pod's Application and a
   group's owner / deployer are identical across the two endpoints. An
   `application=` root additionally seeds pods from the metric's own `app`
   label, unioned with the existing annotation recovery. Any error on either
   read fails the storage build closed.
6. **`ResolvePodApplication` consults the metric first** when `appmeta` is
   served, falling back to the controller chain, so the on-demand lookup keeps
   matching the build.
7. Docs and rules follow: `CLAUDE.md`, `.claude/rules/{graph-model,
   upstream-queries,storage-graph}.md`, `docs/upstream-backend-routing.md`,
   `docs/upstream-metrics.md`, the OpenAPI node schema.

No BREAKING change: with the metric unset every body, query and golden is
unchanged; with it set, the new fields are additive and a pod's Application can
only move to the value the operator's metric declares.

## Capabilities

### New Capabilities

- `app-metadata-source`: the operator-named application-metadata metric — its
  configuration surface and validation, the label contract, how rows resolve
  per pod / namespace / Application within a window, and its degrade rules.

### Modified Capabilities

- `cluster-topology-source`: the pod ArgoCD Application attribute gains the
  metric as a first source ahead of the controller chain; the topology read
  gains the optional application-metadata leg.
- `graph-api`: the `namespace` and `application` compound groups carry
  `data.namespace_owner` / `data.deployed_by`; the pod `application` attribute
  states the new precedence.
- `storage-graph-api`: the namespace-restricted metric read joins the storage
  build; `application` roots seed from the metric as well as from controller
  annotations; both reads fail closed; the attribute carry-over covers the two
  new group fields.
- `upstream-backend-routing`: the family set grows to seven, with `appmeta` a
  second optional family.
- `metrics-label-query`: the caller-declared family is validated against the
  seven family names.
- `pod-application-lookup`: the lookup reads the metric first when `appmeta` is
  served; the "every query declares `ksm`" rule admits the one `appmeta` query.

## Impact

- **Code**: `pkg/promql` (family, `queryDims` / `queryFamily` entry for a
  logical `QAppMetadata` query, a renderer taking the configured metric name);
  `pkg/build` (options, first-wave optional leg, resolver, precedence merge,
  storage namespace-scoped read and application seed, `ResolvePodApplication`);
  `pkg/graph` (`Graph` carries namespace-owner and Application-deployer
  indexes); `pkg/cytoscape` (two group fields); `pkg/kubegraph` (options
  pass-through); `internal/config` (two flags / env vars, validation, startup
  warning when the metric is set but no backend serves `appmeta`).
- **API**: additive `data.namespace_owner` / `data.deployed_by` on group nodes
  of both graph endpoints; regenerated `docs/swagger.{json,yaml}`.
- **Upstream**: one extra query per `/v1/graph` build and one or more per
  `/v1/storage-graph` build when configured; none otherwise.
- **Dependencies**: none.
