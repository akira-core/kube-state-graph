# Proposal

## Why

The application-metadata metric already names each pod's ArgoCD Application,
its deployer and its namespace owner. The operator's metric carries four more
facts the graph's consumers need on the same groups: when an Application was
last deployed, which namespace group a namespace belongs to, who owns that
group, and the namespace's display alias. Reading them from the row the
engine already fetches avoids a second metadata source and a second query.

## What Changes

- Four new logical fields on the application-metadata metric, each bound to a
  label name and overridable through `--app-metadata-labels` /
  `KSG_APP_METADATA_LABELS` exactly like the existing five:
  `deploy_time` (default label `deploy_time`), `ns_group` (default
  `namespace_group`), `ns_group_owner` (default `namespace_group_owner`) and
  `ns_alias` (default `namespace_alias`). The five existing fields (`app`,
  `user`, `ns_owner`, `namespace`, `pod`) keep their names, defaults, picks and
  outputs.
- A synthesised `namespace` group gains `data.namespace_group`,
  `data.namespace_group_owner` and `data.namespace_alias`; a synthesised
  `application` group gains `data.deployed_at`. All are `omitempty` strings
  outside `labels`, on `/v1/graph`, `/v1/storage-graph` and `/v1/k8s-graph`
  alike. A deployment whose metric carries none of the four labels, or with
  the metric unset, keeps a byte-identical body.
- Paired picks: `deployed_at` is the deploy time of the row whose `user` the
  deployer pick selected; `namespace_group_owner` is the group owner of the
  row whose `ns_group` the group pick selected. `namespace_alias` is an
  independent latest-seen pick. Every namespace-level field is keyed by
  `(cluster, namespace)`.
- `deploy_time` is parsed from `YYYY/MM/DD hh:mm:ss` (read as UTC) or RFC 3339
  and emitted as RFC 3339 UTC with whole seconds. A value that does not parse
  is omitted, never borrowed from another row.
- No rendered query changes: the same metric is read with the same matchers
  and restrictions on every endpoint.
- **BREAKING** (in-process embedders only; the wire is unchanged):
  `graph.Graph.NamespaceOwners` / `ApplicationDeployers` and the same two
  fields on `build.Topology` are replaced by
  `NamespaceMetadata map[graph.NamespaceRef]graph.NamespaceMeta` and
  `ApplicationMetadata map[graph.ApplicationRef]graph.ApplicationMeta`.
- **BREAKING** (operators, narrow): an existing `--app-metadata-labels`
  override that binds one of the five existing fields to `deploy_time`,
  `namespace_group`, `namespace_group_owner` or `namespace_alias` now fails
  startup on the shared-label rule until the colliding new field is bound
  elsewhere.

## Non-goals

- No `namespace-group` compound tier: the group is an attribute of the
  namespace group, and `data.parent` does not change.
- No group owner resolved across namespaces or clusters; each namespace reads
  its own rows.
- No time-zone setting: a zone-less deploy time is UTC.
- The pod Application lookup (`pod-application-lookup`,
  `build.ResolvePodApplication`) is unchanged; it reads only `app`.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `app-metadata-source`: nine logical fields instead of five; startup
  validation covers the new fields; new requirements for the namespace group,
  group owner and alias picks and for the paired, parsed deploy time; the
  graph-build degrade covers every group field.
- `graph-api`: the namespace and application group metadata requirement adds
  the four new group fields.
- `storage-graph-api`: the attribute carry-over requirement carries every
  group metadata field, not only owner and deployer.

## Impact

- Code: `pkg/promql/appmetadata.go` (fields, defaults, validation),
  `internal/config/config.go` (field table), `pkg/build/appmetadata.go`
  (indexes and picks), `pkg/build/topology.go` / `topology_types.go` /
  `build.go` (wiring), `pkg/graph/graph.go` (metadata maps),
  `pkg/cytoscape/cytoscape.go` (group DTO), `internal/api/handlers.go` swag
  text and the regenerated `docs/swagger.*`.
- Tests: golden bodies carrying the metric (`with-app-metadata`, both
  `k8s-graph-*-root`) gain the new fields; the storage fan-out golden and the
  render baseline must not move.
- Docs: `docs/upstream-metrics.md`, a new `docs/BREAKING.md` entry,
  `CLAUDE.md` and `.claude/rules/graph-model.md`.
- No new dependency, query, self-metric or route.
