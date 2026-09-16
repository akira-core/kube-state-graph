## 1. Shared rules

- [x] 1.1 Extract `usableTrackingID` and the `argoTrackingIDLabel` constant in `pkg/build/topology.go` and use them in `resolveApplications` and its three callers; verify `go test ./pkg/build/ ./internal/api/` passes with goldens byte-identical.

## 2. Lookup

- [x] 2.1 Add `LabelQuerier`, `PodApplicationRequest`, `ErrAmbiguousPod` and `ResolvePodApplication` in `pkg/build/podapplication.go` (design D1–D5).
- [x] 2.2 Register `LabelQuerier` in `.mockery.yaml` and run `make mocks`; verify `make verify-mocks` is clean.

## 3. Tests

- [x] 3.1 Table test over every resolution path (ReplicaSet → Deployment, bare ReplicaSet, StatefulSet malformed sibling, Job own annotation, Job → CronJob, unsupported kind, multiple controller owners, non-controller row, missing pod), asserting the Application and the exact query sequence.
- [x] 3.2 Request-shape test: family `ksm`, zone, instant, window and filters on each leg, env absent on the first leg and pinned after.
- [x] 3.3 Zone and environment tests: unset az pinned and routed after the pod-owner query, two-zone and two-environment ambiguity errors, explicit az / env pin every leg, absent az label pinned as an `az=""` filter, configured label keys.
- [x] 3.4 Upstream error on each leg fails the call and names the metric; validation failures issue no query.
- [x] 3.5 Parity test feeding one fixture to the batch resolver chain and to the lookup, with and without `AZ`.

## 4. Docs

- [x] 4.1 Add a usage pointer to `docs/upstream-backend-routing.md` and to the pod `application` bullet in `CLAUDE.md`.
- [x] 4.2 `openspec validate add-pod-application-lookup`.
