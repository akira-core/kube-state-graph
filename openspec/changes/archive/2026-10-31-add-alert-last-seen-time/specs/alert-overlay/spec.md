## MODIFIED Requirements

### Requirement: ALERTS series as the alert source

The build SHALL read the OPTIONAL `ALERTS` series over the request window from the backend(s) serving the `alerts` query family, on every build whichever endpoint or in-process call requested it. An alert SHALL be **active in the window** when at least one sample with `alertstate="firing"` falls inside `[start, end]`; `pending` alerts SHALL NOT be surfaced. Each series read SHALL carry, as its value, the Unix timestamp of its last sample inside the window. When the store rejects that read as an invalid query, the build SHALL retry it once with the portable `last_over_time` read over the same selector, whose values carry no timestamp.

#### Scenario: Active-in-window read

- **WHEN** a build reads alerts
- **THEN** it issues a single `tlast_over_time` read of `ALERTS` over the window, with the fixed selector `alertstate="firing"` rendered first
- **AND** the leg runs on every build, whether it serves `GET /v1/graph`, `GET /v1/storage-graph`, `GET /v1/k8s-graph`, or an in-process engine call

#### Scenario: Firing alert in window is read

- **WHEN** `ALERTS{alertname="KubePodCrashLooping",alertstate="firing",namespace="shop",pod="orders-0",cluster="c1"}` has a sample inside the window
- **THEN** the alert is a candidate for matching

#### Scenario: Last firing sample is the series value

- **WHEN** an `ALERTS` series has firing samples at `1761897600`, `1761897660` and `1761897720` inside the window and none later in it
- **THEN** the series is read with the value `1761897720`

#### Scenario: Portable fallback when the store rejects the read

- **WHEN** the `alerts` store answers the `tlast_over_time` read with a `bad_data` error (for example a plain Prometheus, which has no such function)
- **THEN** the build issues `last_over_time(ALERTS{…}[<window>])` with the same selector and query name, once
- **AND** the alerts it returns attach and feed `data.status` as usual, with no `time` on any entry

#### Scenario: No fallback on a transport error

- **WHEN** the `alerts` read fails with a dial, timeout or server error
- **THEN** no fallback query is issued and the build carries no `data.alerts`, as for any optional-leg failure

#### Scenario: Two backends hold the same series

- **WHEN** two backends serving the `alerts` family both return the same `ALERTS` series, with values `1761897600` and `1761897720`
- **THEN** the merged vector keeps the series once, with the value `1761897720`, whichever backend sorts first

#### Scenario: Pending alert ignored

- **WHEN** the only `ALERTS` sample for an alert in the window carries `alertstate="pending"`
- **THEN** no node carries that alert

### Requirement: ALERTS under request-scoped selectors

The `ALERTS` query SHALL carry the request's `az`, `env` and `namespace` dimensions as label matchers, composed after its fixed `alertstate="firing"` selector in the fixed order `az`, `env`, `namespace`, and SHALL be zone-routed by `az` like the `ksm` family.

#### Scenario: Zone, environment and namespace rendered

- **WHEN** a build runs with `az={zone-a}`, `env={prod}`, `cluster={c1}`, `namespace={shop}`
- **THEN** the alerts query is issued as `tlast_over_time(ALERTS{alertstate="firing",<az-key>="zone-a",<env-key>="prod",namespace=~"shop|"}[<window>])` with no `cluster` matcher, and only to the `alerts` backends whose `zones` include `zone-a` or that are catch-alls

#### Scenario: Unfiltered build reads every alert

- **WHEN** a build runs with no selector dimension
- **THEN** the alerts query is issued as `tlast_over_time(ALERTS{alertstate="firing"}[<window>])`

### Requirement: Node `alerts` attribute

Every node that matched at least one alert SHALL carry a typed `data.alerts` array of `{ name, state, severity, time }` objects, never inside `labels`, de-duplicated on `(name, severity)` and sorted by `name` then `severity`. The array SHALL be omitted entirely (never empty) from a node with no matched alert, and SHALL be resolved at build time onto the graph, so every endpoint and in-process engine call carries it alike.

#### Scenario: Entry fields

- **WHEN** a node carries an alert entry
- **THEN** `name` is the `alertname` label, `state` the `alertstate` label (`"firing"`), `severity` the `severity` label, omitted when empty, and `time` the series value truncated to a whole Unix second when that second lies inside the request window

#### Scenario: Time is last seen firing

- **WHEN** an alert began firing before the window and its last firing sample inside the window is at `1761897720`
- **THEN** the entry's `time` is `1761897720`, not the time the alert became active

#### Scenario: Unresolved time omitted

- **WHEN** the series value is below one second, NaN or infinite, or its whole second lies outside `[start, end]` (as the portable fallback's constant `1` always does)
- **THEN** the entry is still attached and carries no `time` key

#### Scenario: Attribute shape

- **WHEN** a pod matched alerts `KubePodCrashLooping` (severity `warning`, last seen `1761897720`) and `HighMemory` (severity `critical`, last seen `1761897600`)
- **THEN** the pod's `data.alerts` equals `[{"name":"HighMemory","state":"firing","severity":"critical","time":1761897600},{"name":"KubePodCrashLooping","state":"firing","severity":"warning","time":1761897720}]`

#### Scenario: Duplicate series collapse

- **WHEN** two `ALERTS` series for the same alert differ only in a label the matcher does not read
- **THEN** the node's `data.alerts` lists that `(name, severity)` once
- **AND** its `time` is the greater of the two series' times, whatever order the series arrived in

#### Scenario: Absent on unalerted nodes

- **WHEN** a node matched no alert
- **THEN** its `data` has no `alerts` key

#### Scenario: Present on both endpoints

- **WHEN** a pod carries an alert and is retained by both `GET /v1/graph` and `GET /v1/storage-graph`
- **THEN** both bodies carry the identical `data.alerts` array on that pod
- **AND** an in-process engine call building the same graph carries it too

### Requirement: Alert severity is the only alert input to node status

An alert entry's `severity` SHALL be the ONLY alert field that feeds a node's `status`; `name`, `state` and `time` SHALL NOT.

#### Scenario: Status fold reads severity only

- **WHEN** a node's `data.status` (graph-api "Node `status` attribute") is folded
- **THEN** each matched alert contributes its `severity` and nothing else

#### Scenario: Old last-seen time does not lower the verdict

- **WHEN** a node's only alert has severity `critical` and a `time` well before the window's end
- **THEN** its `data.status` is `critical`

#### Scenario: Unalerted node status

- **WHEN** a node matched no alert
- **THEN** its `data.status` is folded from its other signals alone
