# Tasks

Extends `resolveUnknownServerPeer` with a bare IP-literal classification path. No PromQL
change, no new label — builds directly on the already-merged
`resolve-unknown-server-peer-labels` machinery in `pkg/build/servicegraph.go`.

## 1. Reverse index (`pkg/build/servicegraph.go`)

- [ ] 1.1 Add `ipKey{cluster, ip string}` type.
- [ ] 1.2 In `parseServiceGraph`, alongside the existing `svcCandidates` build, build
      `ipIndex map[ipKey]serviceKey` from `topology.ServicesByNameNS`: skip entries whose
      `ClusterIP` is empty or `"None"`; on a duplicate `(cluster, ip)` key, keep the
      lexically-smaller `(namespace, service)` pair (determinism, D6).
- [ ] 1.3 Thread `ipIndex` onto `sgResolver`.

## 2. Classification (`pkg/build/servicegraph.go`)

- [ ] 2.1 In `resolveUnknownServerPeer`, after the existing `classifyBareShortName` miss and
      before the `external` fallback, add: if `net.ParseIP(host) != nil`, look up
      `r.ipIndex[ipKey{anchorCluster, host}]`; a hit sets `(ns, svc)` from the matched
      `serviceKey` and falls through to the existing `resolveServiceLevel(anchorCluster, ns,
      svc)` call unchanged.
- [ ] 2.2 A miss (not an IP literal, or an IP literal absent from `ipIndex` for
      `anchorCluster`) falls to the existing `external(rawValue)` path unchanged.
- [ ] 2.3 Add a distinct `noteExternal` reason string for the IP-literal-miss case
      (analogous to `unknown_server_peer_not_k8s_dns` /
      `unknown_server_peer_anchor_lacks_service`), so operators can grep it separately from
      the DNS-classification misses.

## 3. Tests (`pkg/build/servicegraph_test.go`)

- [ ] 3.1 `TestParseServiceGraph_UnknownServerPeerLabel_IPLiteralResolvesService` — bare IP in
      `client_server_address` matches a Service's `ClusterIP` in the anchor cluster →
      `pod-calls-service` edge + normal `service-selects-pod` fan-out.
- [ ] 3.2 `TestParseServiceGraph_UnknownServerPeerLabel_IPLiteralWithPort` — `IP:port` value
      strips the port before IP-literal matching, same resolution as 3.1.
- [ ] 3.3 `TestParseServiceGraph_UnknownServerPeerLabel_IPLiteralFamilySiblingNotMatched` — IP
      matches a Service's `ClusterIP` only in a family-sibling cluster, not the anchor →
      `external/<ip>`, confirming the lookup does NOT fall back to family-wide matching.
- [ ] 3.4 `TestParseServiceGraph_UnknownServerPeerLabel_IPLiteralNoMatch` — IP absent from the
      anchor cluster's `ClusterIP` set entirely → `external/<ip>`.
- [ ] 3.5 `TestParseServiceGraph_UnknownServerPeerLabel_IPLiteralDuplicateClusterIP` —
      defensive case: two Services in the same anchor cluster share a `ClusterIP` →
      resolves to the lexically-smaller `(namespace, service)`, stable across repeated runs.
- [ ] 3.6 Regression: confirm existing `classifyBareShortName`/`classifyK8sDNS` cases from
      `resolve-unknown-server-peer-labels` are unaffected (IP branch never fires for a
      non-IP host).

## 4. Integration (`internal/integration`)

- [ ] 4.1 One fixture-driven case against the real VictoriaMetrics testcontainer: a
      `server="unknown"` series with `client_server_address` set to a Service's ClusterIP,
      confirm the end-to-end graph contains the resolved `pod-calls-service` edge.

## 5. Docs

- [ ] 5.1 Update `CLAUDE.md`'s "Missing pod-UID human-label fallback" / unknown-server bullet
      (or wherever `resolve-unknown-server-peer-labels` landed in `CLAUDE.md`) to mention the
      IP-literal classification step and its anchor-cluster-only scoping.

## 6. Verify

- [ ] 6.1 `go test ./pkg/build/... -run TestParseServiceGraph -v` green.
- [ ] 6.2 `go vet ./...` clean; `make lint` 0 new issues.
- [ ] 6.3 Affected integration subset green against a real VM (the new fixture from 4.1, plus
      existing unknown-server-peer-label integration tests for regressions).
- [ ] 6.4 Full `make test` (race + shuffle) green locally.
- [ ] 6.5 `openspec validate "resolve-unknown-server-ip-peer"` passes.
