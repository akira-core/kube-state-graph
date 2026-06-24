## 1. Cluster-family key helper (`pkg/build`)

- [x] 1.1 Implement `clusterFamilyKey(name string) string` in `pkg/build`: replace every maximal ASCII digit run `[0-9]+` with a single `#` sentinel. Hardcoded pure string function — no flag, env var, or config field (D-A, D-H)
- [x] 1.2 Unit tests for the key function in `pkg/build`: `prod-03`/`prod-12` → `prod-#` (same family); `prod-03` ↔ `prod-3` equal (maximal-run collapse); `staging-1` (`staging-#`) ≠ `prod-1` (`prod-#`); bare-number names `1`, `2`, `42` → `#` (one family); digit-free name → itself (exact-name family); `unknown` → `unknown` (D-A, D-I). Verify: `go test ./pkg/build/ -run ClusterFamilyKey -v`

## 2. Topology candidate lookup (`pkg/build/topology.go`)

- [x] 2.1 Add a by-`(namespace, service)` candidate-cluster lookup for the family fan-out: either a small index on `build.Topology` keyed by `(namespace, service)` → **sorted** cluster list built alongside `ServicesByNameNS`, or a sorted scan of `ClustersObserved` filtered by family key + `ServicesByNameNS` membership. Whichever shape, candidate iteration MUST be lexicographically sorted (D-B, D-G)
- [x] 2.2 Unit-test the lookup path: candidates come back sorted; a cluster present in `ClustersObserved` but lacking the `(namespace, service)` entry is not a match. Verify: `go test ./pkg/build/ -count=1`

## 3. Resolver fan-out (`pkg/build/servicegraph.go`)

- [x] 3.1 Rewrite `resolveServiceLevel` as family iteration: compute `clusterFamilyKey(traceCluster)` once; iterate candidate clusters (all loaded clusters with an equal family key) in sorted order; for EACH cluster `c` where `ServicesByNameNS[{c, ns, svc}]` exists, call the existing `materializeService(c, ns, svc, obs)` byte-for-byte (service node `id="<c>/<ns>/<svc>"`, `labels={cluster, namespace}`, `ipaddress=[cluster_ip]` unless headless `"None"`, plus intra-cluster `service-selects-pod` fan-out from that SAME cluster's `EndpointsByService`); return the SET of matched service-node IDs. The trace-source cluster is not special — just a family member (D-B)
- [x] 3.2 Change `resolveConnString` and `resolveEmptyUID` to return `[]string` (resolved node IDs): family matches → the matched-ID set; zero family matches or unparseable host → one-element `external/<label>` slice (today's fallback verbatim — D-C); non-`"://"` D27 promotion → one-element slice; both-empty endpoint → empty slice (drop)
- [x] 3.3 Adapt `resolveClient` / `resolveServer` to return ID sets; pod-UID, synth-pod, and human-label paths return one-element sets (steps 2–4 of the resolution order still yield exactly one ID or drop — D-J). `srcIsPod` stays uniform per series (a pod source is always a single ID)
- [x] 3.4 Emit the cross product `srcIDs × tgtIDs` in `parseServiceGraph` into the existing `(src, tgt)` pairs map — `(src, tgt)` dedupe and lexically-smaller `srcCluster` tie-break unchanged; edge type stays target-driven (`pod-calls-service` iff target is a materialised service node); edge `labels.cluster` still present iff the client side resolved to a pod, omitted for service/external client sides (D-D, D-E, D-G)
- [x] 3.5 Confirm `normalizeSelfLoopUIDs` (D33 guard) and the 4-step resolution-order structure are byte-identical — only step 1's lookup scope widened and step 1 may now yield multiple IDs (D-J); confirm zero PromQL changes (`pkg/promql` query strings untouched, D30 sentinel selector included) and zero new flags/env knobs (D-H). Verify: `git diff pkg/promql/` is empty
- [x] 3.6 Unit tests in `pkg/build/servicegraph_test.go`:
  - multi-cluster family fan-out: service in `prod-1` AND `prod-2`, trace from `prod-1` → TWO `pod-calls-service` edges (`prod-1` pod → each service node), each service node's `service-selects-pod` fan-out targets only its OWN cluster's pods, both edges carry `labels.cluster="prod-1"`
  - out-of-family exclusion: same `(namespace, service)` also in `staging-1` → NOT matched, no `staging-1` service node materialised
  - zero family matches → `external/<label>`, edge type `pod-calls-pod`, exactly today's shape
  - both sides `"://"` resolving to 2×2 sets → FOUR `pod-calls-service` edges (cross product), every edge `labels` map without `cluster` key
  - `cluster="unknown"` bucketed series with a resolvable-looking `"://"` label → zero candidates → external fallback (D-I)
  - D33 interaction: self-loop UID collision with a `"://"` side now fans out across the family on the cleared side
  - determinism: run the parse twice over a shuffled fixture vector → identical sorted node/edge sets; edge IDs remain UUIDv5 over `<type>|<source>|<target>` (D-G)
  Verify: `go test ./pkg/build/ -count=1 -race -shuffle=on`

## 4. Edge-type registry (`pkg/graph`)

- [x] 4.1 In `pkg/graph/registry.go`: flip the `pod-calls-service` entry's `MayCrossCluster` to `true` AND rewrite its `Description` (current "Always intra-cluster — the service is resolved in the trace-source (client) cluster." contradicts the flipped boolean) to describe cluster-family fan-out; review `pod-calls-pod` Description (unresolved-`"://"`-→-external clause now means "no family cluster holds it") and `service-selects-pod` Description (gains "intra-cluster within the resolved service's own cluster" phrasing — `MayCrossCluster` stays `false`) (D-F)
- [x] 4.2 Update `pkg/graph/service_test.go`: invert the hard `if podCallsService.MayCrossCluster { t.Error(...) }` assertion (it fails the build the moment the registry flips); KEEP the `service-selects-pod` `MayCrossCluster=false` assertion. Verify: `go test ./pkg/graph/ -count=1`

## 5. Golden & API tests (`internal/api`)

- [x] 5.1 Refresh goldens via `go test ./internal/api -update -run Golden` — `testdata/golden/edge-types.json` picks up the `may_cross_cluster: true` flip and the rewritten Description strings; review the diff is exactly the expected catalogue delta
- [x] 5.2 Refresh/extend graph goldens whose fixtures include `"://"` endpoints (`with-service-cytoscape.json` etc.); add or extend a fixture exercising family fan-out (service present in two same-family clusters) if none exists, then re-run `-update` and re-verify `go test ./internal/api -count=1` passes WITHOUT `-update` (byte-determinism intact)

## 6. Integration tests (`internal/integration`)

- [x] 6.1 testcontainers VictoriaMetrics fixture (extend `graph_e2e_test.go` fixtures via `POST /api/v1/import/prometheus`): client pod's `kube_pod_info` in `prod-1`; `kube_service_info` (+ endpointslice series for fan-out) for the addressed `(namespace, service)` ONLY in `prod-2`; one `traces_service_graph_request_total` series with `cluster="prod-1"`, populated `client_k8s_pod_uid`, empty `server_k8s_pod_uid`, `server="<scheme>://<svc>.<ns>.svc..."` → assert the response contains the cross-cluster `pod-calls-service` edge (`prod-1` pod → `prod-2/<ns>/<svc>`, `labels.cluster="prod-1"`), the `prod-2` service node, and its intra-`prod-2` `service-selects-pod` fan-out
- [x] 6.2 Negative cases in the same suite: (a) same service name only in out-of-family `staging-1` → NOT resolved, endpoint falls back to `external/<label>` (D-C); (b) `/v1/edge-types` body shows `pod-calls-service` `may_cross_cluster: true` and `service-selects-pod` `false`. Verify: `go test ./internal/integration/ -count=1 -v` (Docker required; skips gracefully without)

## 7. Docs & contracts

- [x] 7.1 Update CLAUDE.md load-bearing rules: replace the "always intra-cluster" wording for `pod-calls-service` (D29 rule bullet AND the `/v1/edge-types` bullet) with cluster-family fan-out phrasing ("a `(namespace, service)` is resolved against every loaded cluster in the trace-source cluster's family — digit runs normalised to `#` — one `pod-calls-service` edge per match, external fallback on zero matches"); keep `service-selects-pod` documented as always intra-cluster; document the `cluster="unknown"` → zero-family-match → external behaviour (D-I) and that no PromQL/knob changed (D-H)
- [x] 7.2 Audit OpenAPI `@`-annotations (`cmd/.../main.go`, `internal/api/*.go`) for "intra-cluster" wording on `pod-calls-service`; if any change, run `make docs`, commit `docs/swagger.{json,yaml}`, and confirm `make check-docs` is clean

## 8. Full verification gate

- [x] 8.1 `make build` succeeds; `make test` (race + shuffle), `make vet`, `make lint` all green; `make verify-mocks` clean (no `.mockery.yaml` interface changed — `Querier`/`Validator`/`Clock` signatures untouched)
- [x] 8.2 `openspec validate cross-cluster-service-fallback` passes; spec-delta scenarios in `specs/pod-service-graph/spec.md` and `specs/graph-api/spec.md` each map to a passing test from sections 3–6; ready for `openspec verify` / archive
