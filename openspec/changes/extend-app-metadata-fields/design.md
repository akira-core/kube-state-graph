# Design

## Context

See proposal.md for motivation. Today one `tlast_over_time(<metric>{…}[w])`
vector feeds `resolveAppMetadata`, which builds three indexes: pod Application
(lexically-smallest), namespace owner and Application deployer (both
latest-seen through `keepLatest`, lexically-smallest on an exact tie). The two
group indexes travel `build.Topology` → `graph.Graph` (`NamespaceOwners`,
`ApplicationDeployers`, assigned in `Builder.freeze`) → `pkg/cytoscape`, which
looks a synthesised group's key up at serialise time.

Every endpoint reads the same rows. `/v1/graph` reads the metric unscoped;
`/v1/storage-graph` and `/v1/k8s-graph` read it per bucketed cluster,
restricted to the namespaces they loaded (`RenderAppMetadataByNamespace`). The
rendered query projects no labels, so every row already carries whatever labels
the exporter stamps, including the four this change reads.

## Goals / Non-Goals

**Goals:**

- Read four more fields from rows the build already fetches, with picks that
  are a pure function of the row set (no dependence on upstream row order).
- Keep the five existing fields, their picks and every existing wire key
  byte-identical.
- Make the group-metadata Go surface extensible without another API break.

**Non-Goals:**

- No new query, restriction, round or self-metric. The render baseline and
  the storage fan-out golden do not move.
- No change to `build.ResolvePodApplication`, which reads only `app`.

## Decisions

### D1 — Group fields are attributes, not a compound tier

The namespace group carries `namespace_group` / `namespace_group_owner` /
`namespace_alias`; the application group carries `deployed_at`.

- Alternative: a `namespace-group` compound between `cluster` and `namespace`.
  Rejected. It rewrites every namespace group's `data.parent`, a
  non-additive wire change (v2), and every consumer walking `data.parent`
  gains a level.

### D2 — Namespace-level fields are keyed by `(cluster, namespace)`

- Alternative: key the group owner by `(cluster, ns_group)`, or by
  `ns_group` across clusters. Rejected. The storage and k8s builds read only
  the namespaces they loaded, so an owner stamped only on another namespace's
  rows would resolve on `/v1/graph` and vanish on the scoped endpoints. Closing
  that gap needs a new restriction and an extra round.
- Consequence: two namespaces of one group can report different group owners
  when the exporter disagrees with itself. The value is shown verbatim, and
  consistency is the exporter's job.

### D3 — Paired picks under a total order

`deployed_at` comes from the row that won the deployer pick, and
`namespace_group_owner` from the row that won the group pick.

`keepLatest` keeps the FIRST row when last-seen time and value both tie. Once
a companion value rides along, that makes the companion depend on upstream row
order and breaks the deterministic-body contract. The paired pick therefore
compares, in order:

| Pair | Order |
|---|---|
| deployer + deploy time | last-seen desc → `user` asc → deploy time parses before it does not → parsed instant desc |
| group + group owner | last-seen desc → `ns_group` asc → non-empty owner before empty → owner asc |

The first two keys are exactly today's deployer pick, so `deployed_by` does
not move. `namespace_alias` stays an independent `keepLatest` pick, as
`namespace_owner` is. A row whose primary value (`user` / `ns_group`) is empty
never contributes its companion.

- Alternative: independent picks for each field. Rejected. They can pair bob
  with alice's deploy time.
- Alternative: order the deployer by the parsed deploy time. Rejected. It
  changes which `user` wins, and the existing keys stay unchanged.

### D4 — Strict deploy-time parsing in `pkg/build`

- **Slash form.** A fixed-width check `^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}$`
  runs first, then `time.ParseInLocation("2006/01/02 15:04:05", v, time.UTC)`.
  The pre-check is load-bearing. Go's `15` verb accepts a one-digit hour, and
  the parser accepts fractional seconds after `05`. Both were verified, and
  both are outside the spec's form.
- **RFC 3339.** A strict shape check
  `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$`
  runs first, then `time.Parse(time.RFC3339, v)`. This pre-check is
  load-bearing too. Without it, Go's parser falls back to lenient parsing and
  accepts values RFC 3339 forbids: a one-digit hour (`2025-10-08T1:14:15Z`),
  a comma before the fraction (`13:14:15,5Z`) and an offset hour of 24
  (`+24:00`). All three were verified, and all three must stay unresolved.
- **Result.** Converted to UTC and truncated to the second. An invalid
  calendar date fails `time.Parse` itself.
- **Not reused: `kubegraph.parseTimestamp`.** It accepts Unix seconds, which
  the spec rejects, and `pkg/build` cannot import `pkg/kubegraph`.
- **Logging.** One aggregated Debug line per build reports the number of
  unparsable values from winning rows. There is no per-row log, and the
  value is never escalated.

### D5 — Struct-valued metadata maps (Go API break, wire unchanged)

```go
type NamespaceMeta struct{ Owner, Group, GroupOwner, Alias string }
type ApplicationMeta struct {
	DeployedBy string
	DeployedAt time.Time // zero = unresolved; UTC, whole seconds
}

Graph.NamespaceMetadata   map[NamespaceRef]NamespaceMeta
Graph.ApplicationMetadata map[ApplicationRef]ApplicationMeta
```

- `build.Topology` carries the same two fields.
- Nil keeps its meaning (metric unset or vector not read), as does an empty
  map (the read contributed nothing). An entry exists only when at least one
  of its fields resolved.
- `pkg/cytoscape` formats `DeployedAt` with `time.RFC3339` and omits it when
  zero.
- `DeployedAt` is a `time.Time` so an embedder does not re-parse a string.
- Alternative: keep the two maps and add four parallel ones (M1). Rejected.
  It leaves six parallel maps, each future field adds another, and a pair
  chosen from one row is naturally one struct.

### D6 — Configuration surface

- `promql.AppMetadataSource` gains `DeployTime`, `NSGroup`, `NSGroupOwner`
  and `NSAlias`, plus four `DefaultAppMetadata*Label` constants. Adding fields
  is additive for keyed literals.
- `OrDefault`, `Validate` and `internal/config`'s `appMetadataDefaults` gain
  four entries, appended after the existing five.
- That order is what makes a collision error name the existing field first
  (`label "namespace_alias" is used by both "user" and "ns_alias"`).

### D7 — Cytoscape DTO key order

The new DTO fields go directly after `NamespaceOwner` and `DeployedBy`. A
golden carrying the metric then changes only by inserted keys. A golden
without the new labels does not change at all.

## Risks / Trade-offs

- **Collision on upgrade.** An override that already uses a new default label
  name fails startup. → The error names the label and both fields. The
  `docs/BREAKING.md` entry gives the one-line rebinding. Rollback is a revert.
- **Wrong time zone.** An exporter writing local time in the slash form shifts
  `deployed_at` silently. → `docs/upstream-metrics.md` states that a zone-less
  value must be UTC, and RFC 3339 with an offset is the escape hatch.
- **Series churn.** A `deploy_time` label creates a new series on every
  deploy, including a redeploy by the same user. That adds rows to the
  windowed vector. → It is bounded by deploy frequency × window. Note it in
  `docs/upstream-metrics.md` beside the `-search.maxUniqueTimeseries`
  guidance.
- **Embedder compile break.** → `docs/BREAKING.md` gets a before/after table
  for `Graph` and `Topology`.

## Migration Plan

1. Ship the image. An exporter already using the default label names needs no
   flag. An operator whose `--app-metadata-labels` collides rebinds the new
   field.
2. Rollback is a revert. The wire change is additive, so consumers that ignore
   unknown keys are unaffected in either direction.
