# Spec Delta

## MODIFIED Requirements

### Requirement: Routed label query for an arbitrary metric

The engine SHALL expose a label query that a Go consumer issues against the live routing table. Its request SHALL carry:

- a **metric name** — any Prometheus metric name, not restricted to the query set the graph build issues;
- a **query family** — exactly one of the seven declared families, supplied by the caller;
- an optional **availability zone** — at most one value;
- an optional set of **label filters** — label key to label value, matched for equality;
- a required **evaluation instant**;
- an optional **lookback window**;
- an optional **result limit**.

`family` and `az` SHALL each be single-valued. A caller needing two zones SHALL issue two queries and keep the two results apart; the multi-value OR semantics the graph request surface applies to `?az=` SHALL NOT apply here.

The request SHALL carry no other named dimension. `env`, `cluster`, `namespace` and every other label are expressed as label filters: `az` is named because it decides which store is asked, which no filter can do; every other label only decides what the chosen store returns.

The evaluation instant SHALL be required. An absent instant SHALL be a request error and SHALL NOT default to the current time — the query layer holds no clock, and a silently defaulted instant would hide a caller bug behind a plausible result.

An absent lookback window SHALL issue a bare instant selector at that instant. A window greater than zero SHALL issue the same selector evaluated over that lookback.

#### Scenario: Metric outside the graph engine's own query set

- **WHEN** a caller requests metric `node_memory_MemAvailable_bytes`, family `ksm`, with no zone and no label filters
- **THEN** the query is issued to every backend serving `ksm` and the matching series' label sets are returned, even though no declared graph query names that metric

#### Scenario: Evaluation instant is required

- **WHEN** a caller issues a label query whose evaluation instant is the zero time
- **THEN** the call fails with a request error naming the missing instant, and no upstream query is issued

#### Scenario: Two zones require two calls

- **WHEN** a caller wants label sets from `zone-a` and `zone-b`
- **THEN** the caller issues one query per zone and receives two independent results; the request type admits no second zone value

#### Scenario: Application-metadata family accepted

- **WHEN** a caller requests metric `argocd_app_pod_info`, family `appmeta`, against a table serving `appmeta`
- **THEN** the query is issued to every backend serving `appmeta` and the matching series' label sets are returned

### Requirement: Caller-declared family selects the backends

The label query SHALL select its backends by the **caller-declared** family, applying the routing capability's existing rules unchanged: candidates serve the family; a zone-routed family is further restricted to backends whose zones set is empty or contains the requested zone; a family that is not zone-routed ignores zones entirely.

The declared family SHALL be validated against the seven declared family names. An unrecognised family SHALL be a request error, never a fan-out to every backend.

The identical query string SHALL be issued to every selected backend. Routing SHALL decide which store is asked and the rendered matchers SHALL decide what it returns; the two SHALL compose rather than substitute for one another.

#### Scenario: Zone selects the covering backends

- **WHEN** backends `zone-a` (`zones: [zone-a]`) and `zone-b` (`zones: [zone-b]`) both serve `ksm`, and a label query names family `ksm` and zone `zone-a`
- **THEN** the query is issued only to `zone-a`

#### Scenario: Catch-all backend is selected alongside

- **WHEN** a backend declaring no zones also serves `ksm` and the same query is issued
- **THEN** that backend is selected in addition to `zone-a`

#### Scenario: Absent zone fans out to every backend serving the family

- **WHEN** the same table serves a label query naming family `ksm` and no zone
- **THEN** the query is issued to `zone-a`, `zone-b` and the catch-all backend, and the results are merged and de-duplicated

#### Scenario: Unrecognised family is rejected

- **WHEN** a caller declares family `metrics`
- **THEN** the call fails with a request error naming the unknown family, and no upstream query is issued

### Requirement: Zone applies the declared family's own zone contract

When a zone is supplied, it SHALL be applied exactly as the routing capability applies the `az` dimension to that family:

- `ksm`, `kubelet`, `alerts` and `appmeta` are routed by zone AND carry an `az` matcher, rendered with the deployment's configured `az` label key.
- `harvest` is routed by zone and carries **no** `az` matcher — the per-zone store boundary is the zone filter.
- `servicegraph` and `probe` are not zone-routed and carry no `az` matcher. A zone supplied for either SHALL be a request error naming the family; it SHALL NEVER be silently discarded, because a caller who asked to narrow and received unnarrowed data has no way to tell.

A caller who wants to match the `az` label literally on a family that does not route by it SHALL express it as an ordinary label filter with the `az` field left empty.

An absent zone SHALL apply no restriction and render no `az` matcher.

#### Scenario: Zone routes Harvest without a matcher

- **WHEN** a caller issues a label query naming family `harvest`, zone `zone-b`, and metric `volume_labels`
- **THEN** the query is issued only to the backends covering `zone-b`, and the query string carries no `az` matcher

#### Scenario: Zone rendered as a matcher on kube-state-metrics

- **WHEN** a caller issues a label query naming family `ksm` and zone `zone-a`
- **THEN** the query is issued only to the backends covering `zone-a`, and the query string carries the `az` matcher for `zone-a`

#### Scenario: Zone rejected on the service-graph family

- **WHEN** a caller issues a label query naming family `servicegraph` and zone `zone-a`
- **THEN** the call fails with a request error naming the family, and no upstream query is issued

#### Scenario: Configured label key is honoured

- **WHEN** the deployment binds the `az` dimension to the upstream label `zone` and a caller issues a label query naming family `ksm` and zone `zone-a`
- **THEN** the issued query carries the matcher `zone="zone-a"`, not `az="zone-a"`

#### Scenario: Zone rendered as a matcher on application metadata

- **WHEN** a caller issues a label query naming family `appmeta` and zone `zone-a`
- **THEN** the query is issued only to the `appmeta` backends covering `zone-a`, and the query string carries the `az` matcher for `zone-a`
