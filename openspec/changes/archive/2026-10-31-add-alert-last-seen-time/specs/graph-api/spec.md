## MODIFIED Requirements

### Requirement: Node `alerts` attribute

Each `type="pod"`, `type="node"`, `type="pvc"`, `type="netapp-node"` and `type="netapp-aggr"` node MAY carry a typed `data.alerts` array of `{ name, state, severity, time }` objects — the active alerts the `alert-overlay` capability matched to it — which SHALL be serialised with `omitempty` semantics and **never inside `labels`** (the `ipaddress` / `owner` / `ready_status` precedent). `time` SHALL be an integer Unix epoch second.

#### Scenario: Entry order and de-duplication

- **WHEN** a node carries alerts
- **THEN** the array is sorted by `name` then `severity` and de-duplicated on that pair, and `severity` is omitted from an entry when empty

#### Scenario: Time is an integer epoch second

- **WHEN** an entry's last firing sample resolved
- **THEN** `time` is a JSON integer of Unix seconds (for example `1761897720`), never a string, a fraction or milliseconds
- **AND** an entry whose time did not resolve carries no `time` key

#### Scenario: Kinds that never carry alerts

- **WHEN** a body contains `service`, `external`, `netapp-svm` or synthesised group nodes
- **THEN** none of them carries `alerts`

#### Scenario: Both graph endpoints

- **WHEN** a node is served by `GET /v1/graph`, `GET /v1/storage-graph` or `GET /v1/k8s-graph`
- **THEN** the attribute, including each entry's `time`, appears on each alike, since it is resolved onto the built graph

#### Scenario: Alerted pod serialised

- **WHEN** the build attached alerts `KubePodCrashLooping` (`warning`, last seen `1761897720`) and `HighMemory` (`critical`, last seen `1761897600`) to a pod
- **THEN** the pod's `data.alerts` equals `[{"name":"HighMemory","state":"firing","severity":"critical","time":1761897600},{"name":"KubePodCrashLooping","state":"firing","severity":"warning","time":1761897720}]` and `labels` carries no alert key

#### Scenario: Unalerted node has no key

- **WHEN** a node matched no alert
- **THEN** its `data` object has no `alerts` key, so a body with no alerts differs from the pre-change golden only by the `status` keys the "Node `status` attribute" requirement adds
