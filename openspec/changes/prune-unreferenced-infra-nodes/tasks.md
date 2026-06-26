# Tasks

Projection-only behaviour change; implemented test-first against `pkg/graph/project_test.go`.

## 1. Projection (`pkg/graph/project.go`)

- [x] 1.1 Build the `hostNodes` (in-scope pods' `labels.node`) and `referencedSC` (in-scope PVCs' `StorageClassID`) reference sets **unconditionally** in `filterNodes` (drop the `if len(scope.Namespaces) > 0` gate).
- [x] 1.2 Rewrite `infraNodePassesFilters`: cluster filter first; then if a name filter is active admit iff the node is named (the exception); else admit iff its id is in `referenced`. Update the doc comment.

## 2. Tests

- [x] 2.1 Generalise `TestProject_NamespaceFilter_DropsPodlessK8sNode` (drop the no-filter "podless node retained" sanity that contradicts the new rule).
- [x] 2.2 Add `TestProject_NoFilter_DropsUnreferencedInfraNodes` (no-filter drops a podless node + a PVC-less StorageClass; keeps the host node + backed StorageClass).
- [x] 2.3 Add `TestProject_NameFilter_MatchesUnreferencedInfraNode` (`?name=<podless-node>` still returns the node).
- [x] 2.4 Update `internal/integration` `TestNodeReadyStatusAttribute` to fetch its podless probe nodes via `?name=` (they no longer appear in the default view); assertions otherwise unchanged.

## 3. Docs

- [x] 3.1 Update `CLAUDE.md`: the D6 infra-node retention rule is now all-request with the `?name=` exception, plus the consequence note (podless `ready_status` / `ipaddress` and PVC-less StorageClass attrs absent from the default view).

## 4. Verify

- [x] 4.1 `go test ./pkg/graph/ -run TestProject` green; `go vet ./...` clean; `make lint` 0 issues.
- [x] 4.2 Affected integration subset green against a real VM (`TestNodeReadyStatusAttribute`, storageclass / node-ip / pvc / metric-prefix sanity).
- [x] 4.3 Full `make test` (race + shuffle + Docker integration) green.
- [x] 4.4 `openspec validate "prune-unreferenced-infra-nodes"` passes.
