# metrics-label-query Specification

## Purpose

Gives a Go consumer of the graph engine a routed, family-scoped way to ask one upstream question about an arbitrary metric — "which label sets exist for this series, under these exact label equalities, optionally in this zone" — reusing the routing capability's backend selection, fan-out merge and failure semantics instead of forcing the consumer to re-implement them against a hand-built client.

## Requirements

### Requirement: Routed label query for an arbitrary metric

The engine SHALL expose a label query that a Go consumer issues against the live routing table, naming any Prometheus metric, not only the metrics of the query set the graph build issues.

#### Scenario: Request fields

- **WHEN** a caller builds a label query
- **THEN** the request carries a **metric name** (any Prometheus metric name), a **query family** (exactly one of the seven declared families, supplied by the caller), an optional **availability zone** (at most one value), an optional set of **label filters** (label key to label value, matched for equality), a required **evaluation instant**, an optional **lookback window**, and an optional **result limit**

#### Scenario: Metric outside the graph engine's own query set

- **WHEN** a caller requests metric `node_memory_MemAvailable_bytes`, family `ksm`, with no zone and no label filters
- **THEN** the query is issued to every backend serving `ksm` and the matching series' label sets are returned, even though no declared graph query names that metric

#### Scenario: Application-metadata family accepted

- **WHEN** a caller requests metric `argocd_app_pod_info`, family `appmeta`, against a table serving `appmeta`
- **THEN** the query is issued to every backend serving `appmeta` and the matching series' label sets are returned

### Requirement: Family and zone are single-valued

`family` and `az` SHALL each be single-valued. The multi-value OR semantics the graph request surface applies to `?az=` SHALL NOT apply to the label query; a caller needing two zones SHALL issue two queries and keep the two results apart.

#### Scenario: Two zones require two calls

- **WHEN** a caller wants label sets from `zone-a` and `zone-b`
- **THEN** the caller issues one query per zone and receives two independent results; the request type admits no second zone value

### Requirement: Zone is the only named label dimension

The label query request SHALL carry no named dimension other than `az`; `env`, `cluster`, `namespace` and every other label SHALL be expressed as label filters.

#### Scenario: Environment expressed as a filter

- **WHEN** a caller wants to narrow a label query by `env`, `cluster` or `namespace`
- **THEN** it supplies that label as a label filter
- **AND** `az` alone is a named field, because it decides which store is asked, which no filter can do; every other label only decides what the chosen store returns

### Requirement: Evaluation instant is required

The evaluation instant SHALL be required: an absent instant SHALL be a request error and SHALL NOT default to the current time, because the query layer holds no clock and a silently defaulted instant would hide a caller bug behind a plausible result.

#### Scenario: Evaluation instant is required

- **WHEN** a caller issues a label query whose evaluation instant is the zero time
- **THEN** the call fails with a request error naming the missing instant, and no upstream query is issued

### Requirement: Lookback window shapes the selector

An absent lookback window SHALL issue a bare instant selector at the evaluation instant; a window greater than zero SHALL issue the same selector evaluated over that lookback.

#### Scenario: No lookback window

- **WHEN** a caller issues a label query with no lookback window
- **THEN** a bare instant selector is evaluated at the evaluation instant

#### Scenario: Positive lookback window

- **WHEN** a caller issues a label query with a lookback window greater than zero
- **THEN** the same selector is evaluated over that lookback at the evaluation instant

### Requirement: Caller-declared family selects the backends

The label query SHALL select its backends by the **caller-declared** family, applying the routing capability's existing rules unchanged, and SHALL issue the identical query string to every selected backend. Routing SHALL decide which store is asked and the rendered matchers SHALL decide what it returns; the two SHALL compose rather than substitute for one another.

#### Scenario: Backend candidate rules

- **WHEN** the label query selects its backends
- **THEN** the candidates are the backends serving the declared family
- **AND** a zone-routed family is further restricted to backends whose zones set is empty or contains the requested zone
- **AND** a family that is not zone-routed ignores zones entirely

#### Scenario: Zone selects the covering backends

- **WHEN** backends `zone-a` (`zones: [zone-a]`) and `zone-b` (`zones: [zone-b]`) both serve `ksm`, and a label query names family `ksm` and zone `zone-a`
- **THEN** the query is issued only to `zone-a`

#### Scenario: Catch-all backend is selected alongside

- **WHEN** a backend declaring no zones also serves `ksm` and the same query is issued
- **THEN** that backend is selected in addition to `zone-a`

#### Scenario: Absent zone fans out to every backend serving the family

- **WHEN** the same table serves a label query naming family `ksm` and no zone
- **THEN** the query is issued to `zone-a`, `zone-b` and the catch-all backend, and the results are merged and de-duplicated

#### Scenario: Identical query string on every backend

- **WHEN** a label query selects several backends
- **THEN** every selected backend receives the identical query string

### Requirement: Declared family is validated

The declared family SHALL be validated against the seven declared family names; an unrecognised family SHALL be a request error, never a fan-out to every backend.

#### Scenario: Unrecognised family is rejected

- **WHEN** a caller declares family `metrics`
- **THEN** the call fails with a request error naming the unknown family, and no upstream query is issued

### Requirement: Zone applies the declared family's own zone contract

A supplied zone SHALL be applied exactly as the routing capability applies the `az` dimension to the declared family. A zone supplied for a family that is not zone-routed SHALL be a request error naming the family and SHALL NEVER be silently discarded. An absent zone SHALL apply no restriction and render no `az` matcher.

#### Scenario: Zone contract per family

- **WHEN** a caller supplies a zone
- **THEN** `ksm`, `kubelet`, `alerts` and `appmeta` are routed by zone AND carry an `az` matcher, rendered with the deployment's configured `az` label key
- **AND** `harvest` is routed by zone and carries **no** `az` matcher — the per-zone store boundary is the zone filter
- **AND** `servicegraph` and `probe` are not zone-routed and carry no `az` matcher, so a zone supplied for either is a request error naming the family, because a caller who asked to narrow and received unnarrowed data has no way to tell

#### Scenario: Absent zone

- **WHEN** a caller supplies no zone
- **THEN** no zone restriction applies to backend selection and no `az` matcher is rendered

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

### Requirement: Label filters are exact matches only

A label filter SHALL match a label for **equality** only: inequality, regular-expression and negated-regular-expression matching SHALL NOT be expressible through this request, and no caller-supplied value SHALL be interpreted as a regular expression. Filters SHALL be combined with AND across keys and with the `az` matcher when one is rendered.

#### Scenario: Filters combine with AND

- **WHEN** a caller filters on `namespace=shop` and `pod=checkout`
- **THEN** only series carrying both labels with exactly those values are returned

#### Scenario: A regular expression in a value matches literally

- **WHEN** a caller supplies the filter value `check.*`
- **THEN** the query matches only the literal string `check.*`, and no series named `checkout` is returned

#### Scenario: Empty value matches an absent label

- **WHEN** a caller supplies the filter `lun=` with an empty value
- **THEN** the filter is rendered verbatim as an equality against the empty string, so the query matches series that carry no `lun` label or carry it empty
- **AND** the filter is not discarded, and this behaviour is documented rather than silently dropped

#### Scenario: Request with no filters

- **WHEN** a caller supplies no label filters at all
- **THEN** the request is valid and selects every series of the named metric on the selected backends, subject to the result bound

### Requirement: A filter on the zone label key conflicts with the zone field

A filter whose key equals the configured `az` label key SHALL be rejected when the `az` field is also set, because two matchers on one label either duplicate or contradict each other while looking like a valid request; it SHALL be accepted when the `az` field is empty.

#### Scenario: Zone filter rejected when the zone field is set

- **WHEN** a caller sets zone `zone-a` and also supplies a filter on the configured `az` label key
- **THEN** the call fails with a request error directing the caller to the zone field, and no upstream query is issued

#### Scenario: Zone filter allowed when the zone field is empty

- **WHEN** a caller issues a label query naming family `servicegraph`, no zone, and a filter `az=zone-a`
- **THEN** the query is issued to every backend serving `servicegraph` with the matcher `az="zone-a"`, matching the label literally
- **AND** this is how a caller matches the `az` label literally on a family that does not route by it

### Requirement: Caller input is validated before it reaches a query string

Every caller-supplied string rendered into an upstream query SHALL be validated **before** rendering, and a rejected value SHALL fail the request without issuing any upstream query. Accepted values SHALL be escaped for a double-quoted string literal when rendered; validation SHALL be the gate and escaping the second layer, and neither alone is the contract.

#### Scenario: Validation rules

- **WHEN** a label query is validated
- **THEN** the metric name must match the Prometheus metric-name grammar
- **AND** every label filter key must match the Prometheus label-name grammar, and the reserved metric-name label is rejected as a filter key because the metric is named by its own field
- **AND** the `az` value and every label filter value are rejected when they contain a control character or invalid UTF-8, or exceed a documented maximum length

#### Scenario: Injection attempt in a metric name

- **WHEN** a caller supplies the metric name `kube_pod_info} or up{`
- **THEN** the call fails with a request error naming the invalid metric name, and no upstream query is issued

#### Scenario: Injection attempt in a filter value

- **WHEN** a caller supplies the filter value `shop",other="x`
- **THEN** the value is escaped so it matches that literal string, and it does not introduce a second matcher

#### Scenario: Control character in a zone value

- **WHEN** a caller supplies a zone containing a newline
- **THEN** the call fails with a request error, and no upstream query is issued

#### Scenario: Control character in a filter value

- **WHEN** a caller supplies a filter value containing a newline
- **THEN** the call fails with a request error, and no upstream query is issued

#### Scenario: Reserved label key rejected

- **WHEN** a caller supplies a filter whose key is the reserved metric-name label
- **THEN** the call fails with a request error directing the caller to the metric-name field

### Requirement: Result is the matched series' label sets

The result SHALL be the label sets of the matched series — one label map per series, with no sample values or timestamps — de-duplicated by label set and returned in a deterministic order that is a pure function of the returned label sets, never of backend response arrival order. A query that matched nothing SHALL return an empty result and no error.

#### Scenario: One map per matched series

- **WHEN** a query matches three series
- **THEN** the result is three label maps, each carrying that series' own labels, and no sample value appears anywhere in the result
- **AND** a caller that needs a value uses the ordinary query path

#### Scenario: Series held by two backends appears once

- **WHEN** a catch-all backend and a zone backend both hold the identical series and both are selected
- **THEN** the series contributes exactly one label map to the result

#### Scenario: Metric-name label is not returned

- **WHEN** a query matches series that carry the reserved metric-name label
- **THEN** that label is absent from every returned map and every other label is present unchanged, carried verbatim and uninterpreted
- **AND** the label is removed because whether an upstream preserves it depends on how the query was evaluated, and the caller already named the metric

#### Scenario: Order does not depend on which backend answered first

- **WHEN** the same query is issued twice against the same data and the backends answer in a different order
- **THEN** both results are identical, element for element

#### Scenario: No match is not an error

- **WHEN** a filter matches no series on any selected backend
- **THEN** the result is empty and no error is returned

### Requirement: Result size is bounded

The label query SHALL enforce a maximum number of returned label sets. The bound SHALL be caller-settable per request and SHALL fall back to a documented package default when unset.

Exceeding the bound SHALL be an **error** naming the bound and the observed count. The result SHALL NOT be silently truncated: a truncated label set list is indistinguishable from a narrower estate and would be acted on as if it were complete.

#### Scenario: Over-broad query fails loudly

- **WHEN** a caller queries a high-cardinality metric with no filters and the match count exceeds the effective bound
- **THEN** the call fails with an error naming the bound and the observed count, and the caller narrows the filters

#### Scenario: Caller raises the bound

- **WHEN** a caller sets a bound above the match count
- **THEN** the full result is returned

### Requirement: Degradation reuses the routing capability, except for an unserved family

The label query SHALL reuse the routing capability's empty-result outcome for an unmatched zone and its fail-closed rule for a backend that errors. It SHALL depart from the server's optional-leg outcome in exactly one case: a declared family that **no backend in the live table serves** SHALL fail the call with an error naming the family, and no upstream query SHALL be issued.

#### Scenario: Unserved optional family

- **WHEN** the routing table serves `alerts` on no backend and a caller issues a label query naming family `alerts`
- **THEN** the call fails with an error naming the `alerts` family, and no upstream query is issued
- **AND** this differs from the server's own optional `alerts` leg, which returns empty at Debug because it is issued unconditionally on every build and silence is its normal state; the label query is invoked deliberately, so an unserved family is a configuration gap the caller must see rather than an empty estate

#### Scenario: Unmatched zone

- **WHEN** a caller names family `ksm` and zone `zone-z`, which no backend serving `ksm` declares or covers
- **THEN** the result is empty, no error is returned, and a Warn log names the family and `zone-z`

#### Scenario: Backend failure fails the call

- **WHEN** two backends are selected and one returns an error
- **THEN** the call fails with an error naming the failing backend, and no partial result is returned
- **AND** a partial fan-out is never returned, because it is indistinguishable from a smaller estate

### Requirement: Self-metric cardinality stays bounded

Upstream observations for the label query SHALL be recorded under a single fixed query name, never the caller's metric name. The existing per-query duration and failure series SHALL keep their label sets, and per-backend failures SHALL continue to be attributed through the existing per-backend failure counter.

#### Scenario: Arbitrary metric names do not create metric series

- **WHEN** a thousand label queries are issued naming a thousand distinct metrics
- **THEN** the upstream query duration and failure metrics carry exactly one additional label value between them, not a thousand

### Requirement: The routed query path is unchanged for existing consumers

Introducing the label query SHALL NOT change the signature or behaviour of the existing query interfaces, the graph build, or any HTTP route. A consumer that does not call it SHALL observe byte-identical behaviour.

#### Scenario: Existing graph responses are unchanged

- **WHEN** the graph build runs against an unchanged routing table and upstream data
- **THEN** every response body is byte-identical to the one produced before this capability existed
