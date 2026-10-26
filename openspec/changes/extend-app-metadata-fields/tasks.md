# Tasks

## 1. Configuration surface

- [x] 1.1 Add `DeployTime`, `NSGroup`, `NSGroupOwner`, `NSAlias` and their `DefaultAppMetadata*Label` constants (`deploy_time`, `namespace_group`, `namespace_group_owner`, `namespace_alias`) to `promql.AppMetadataSource`; extend `OrDefault` and `Validate`, appending the four after the existing five (design D6). Verify with new cases in `pkg/promql/appmetadata_test.go` (`TestAppMetadataSource_OrDefault`, `TestAppMetadataSource_Validate`), including `User: "namespace_alias"` failing with an error naming `namespace_alias`, `user` and `ns_alias`: `go test ./pkg/promql/ -run AppMetadata`.
- [x] 1.2 Extend `appMetadataDefaults` and the source construction in `internal/config/config.go`, and update the flag usage and doc comments that say "five". Verify with `internal/config/config_test.go` cases for the spec scenarios (partial override keeps all nine defaults, `deploy_time=last_deployed,ns_alias=ns_display`, `user=namespace_alias` rejected, `user=namespace_alias,ns_alias=ns_display` accepted): `go test ./internal/config/ -run AppMetadata`.
- [x] 1.3 Update the field table in `docs/upstream-metrics.md` "Application metadata" to nine fields with defaults, plus the upgrade-collision note. Verify every field name and default matches `promql.DefaultAppMetadata*Label`.

## 2. Struct-valued metadata maps (refactor, wire unchanged)

- [x] 2.1 In `pkg/graph`, add `NamespaceMeta{Owner, Group, GroupOwner, Alias}` and `ApplicationMeta{DeployedBy, DeployedAt time.Time}`, and replace `Graph.NamespaceOwners` / `ApplicationDeployers` with `NamespaceMetadata` / `ApplicationMetadata` (design D5); port `pkg/graph/appmetadata_test.go` and `property_test.go`. Verify `go test ./pkg/graph/`.
- [x] 2.2 Replace the same two fields on `build.Topology` and fill the struct maps from the existing owner and deployer picks only (an entry exists only when a field resolved); update `Builder.freeze` and port `pkg/build` tests that read the old fields. Verify `go build ./... && go test ./pkg/build/`.
- [x] 2.3 Point the `pkg/cytoscape` group lookups at the struct maps, still emitting only `namespace_owner` / `deployed_by`; port `pkg/cytoscape/group_metadata_test.go` and the hand-built golden fixtures that assign the old maps directly (`appMetadataOwners` / `appMetadataDeployers` in `internal/api/golden_test.go`, the two assignments in `internal/api/golden_k8s_test.go`). Verify `go test ./pkg/cytoscape/ ./internal/api/`, with `git diff --exit-code internal/api/testdata` showing no golden change.
- [x] 2.4 Add a top-of-file `docs/BREAKING.md` entry: a before/after table for `graph.Graph` and `build.Topology`; the operator migration for an `--app-metadata-labels` override that collides with a new default label (rebind the new field, e.g. `user=namespace_alias,ns_alias=ns_display`); and the embedder note that a directly built `promql.AppMetadataSource` binding an existing field to a new default label now fails `Validate`, including inside `build.ResolvePodApplication`. Update the map names in `CLAUDE.md` (pkg/graph sealed-types paragraph) and `.claude/rules/graph-model.md`. Verify `grep -rn 'NamespaceOwners\|ApplicationDeployers' --include='*.go' --include='*.md' --exclude-dir=changes .` returns only the BREAKING.md "before" column.

## 3. Picks and deploy-time parsing

- [x] 3.1 Add the deploy-time parser in `pkg/build`: fixed-width `^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}$` pre-check, then `2006/01/02 15:04:05` in UTC, or `time.RFC3339`; result in UTC, truncated to the second (design D4). Verify a table test covering every "Deploy time format" scenario (slash form, RFC 3339 with `+08:00` and a fraction, one-digit hour, slash form with a fraction, leading space, Unix seconds, `2025/02/30`): `go test ./pkg/build/ -run DeployTime`.
- [x] 3.2 Replace the deployer `keepLatest` with the paired deployer + deploy-time pick under the D3 total order, so `deployed_by` is unchanged. Verify `pkg/build/appmetadata_test.go` cases for every "Application deploy time from the metric" scenario, plus a permutation test asserting identical output for every row order: `go test ./pkg/build/ -run AppMetadata -count=3 -shuffle=on`.
- [x] 3.3 Add the paired namespace group + group owner pick and the independent alias pick, keyed `(cluster, namespace)`, with empty-`pod` rows contributing. Verify cases for every scenario of "Namespace group and group owner from the metric" and "Namespace alias from the metric", including the permutation test: `go test ./pkg/build/ -run AppMetadata`.
- [x] 3.4 Emit one aggregated Debug line per build counting unparsable deploy times from winning rows (no per-row log). Verify with a test capturing the build logger: one record with the count, none when every value parses.
- [x] 3.5 Extend the `/v1/graph` degrade test so the failing `app_metadata` fixture carries all four new labels, and assert no group carries any of the six fields and the body equals the metric-unset body. Verify `go test ./pkg/build/ ./internal/api/ -run AppMetadata`.
- [x] 3.6 Document in `docs/upstream-metrics.md` the two accepted deploy-time forms, that a zone-less value is UTC, both pairings, and the series-churn note beside the `-search.maxUniqueTimeseries` guidance. Verify the examples match the spec scenarios byte for byte.

## 4. Wire output

- [x] 4.1 Add `namespace_group`, `namespace_group_owner`, `namespace_alias` (after `namespace_owner`) and `deployed_at` (after `deployed_by`, RFC 3339, omitted when zero) to the cytoscape group DTO (design D7). Verify `pkg/cytoscape/group_metadata_test.go` covers the new keys on groups only, omission when unresolved, and no key on any real node: `go test ./pkg/cytoscape/`.
- [x] 4.2 The `with-app-metadata` and both `k8s-graph-*-root` goldens are hand-built graphs: set the new `NamespaceMeta` / `ApplicationMeta` fields (`Group`, `GroupOwner`, `Alias`, `DeployedAt`) in their fixtures, then run `go test ./internal/api/ -update -run Golden`. Verify the diff in `internal/api/testdata/golden/` is inserted keys only, and that every other golden is unchanged.
- [x] 4.3 Extend `internal/api/server_app_metadata_test.go`, which drives mock metric rows end to end (parse, pick, serialise): (a) rows carrying none of the new labels yield a body with none of the new keys; (b) rows carrying all four new labels, with a slash-form `deploy_time`, yield the exact non-empty values (`namespace_group`, `namespace_group_owner`, `namespace_alias`, `deployed_at` as RFC 3339 UTC) on both `/v1/graph` and `/v1/storage-graph` for the same window. Verify `go test ./internal/api/ -run AppMetadata`.
- [x] 4.4 Update the swag `@Description` text in `internal/api/handlers.go` for `/v1/graph`, `/v1/storage-graph` and `/v1/k8s-graph` to list the six group fields, then run `make docs`. Verify `make check-docs` passes with the regenerated `docs/swagger.*` committed.
- [x] 4.5 Sweep the two-field wording to the six group fields: the "Compound-group metadata" bullet and sealed-types paragraph in `.claude/rules/graph-model.md`, the compound-nodes rule and sealed-types paragraph in `CLAUDE.md`, the fan-out note in `docs/upstream-metrics.md` (~line 77), and "pod Application, namespace owner and deployer" in `.claude/rules/storage-graph.md`. Verify `grep -rn 'namespace_owner\|deployed_by\|owner and deployer\|owner or deployer' docs/ .claude/rules/ CLAUDE.md internal/api/handlers.go`: every hit either names all six fields or is deliberately owner/deployer-only (the existing pick rows of the field table).

## 5. Integration checks

- [x] 5.1 Confirm no rendered query moved: `go test ./pkg/promql/ -run TestRender_EmptySelectorMatchesBaseline` and `go test ./pkg/build/ -run TestBuildStorage_FanOutUnchanged`, both without update flags, then `git diff --exit-code pkg/promql/testdata pkg/build/testdata`.
- [ ] 5.2 Run `make ci` and verify it is green (lint, vuln, test, check-docs, verify-mocks, containment checks).
- [x] 5.3 Run `openspec validate extend-app-metadata-fields --strict` and verify it reports the change valid.

## Workflow follow-up

- Run `/opsx:verify` against this change before archiving.
- Archive with `openspec archive extend-app-metadata-fields` after review, then confirm `openspec validate --specs --strict` still passes.
