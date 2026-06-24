## ADDED Requirements

### Requirement: Pod `application` and `containers` attributes

Every `data` object for a `type="pod"` node in the Cytoscape response SHALL be
able to expose two additional top-level attributes with `omitempty` semantics,
both **outside `labels`** (which stays a strict `map[string]string`):

- `application` — a `string`, the pod's ArgoCD Application name as resolved by the
  `cluster-topology-source` capability. Emitted only when the pod has a resolved
  Application; omitted entirely otherwise (never an empty string).
- `containers` — an array of objects `[{ name: string, image: string }]`, one per
  container, as resolved by the `cluster-topology-source` capability and ordered
  deterministically by `(name, image)`. Emitted only when the pod has at least one
  resolved container; omitted entirely otherwise (never an empty array).

These attributes SHALL appear only on `type="pod"` nodes. `type="node"`,
`type="pvc"`, `type="service"`, `type="external"`, and the synthesised
`type="cluster"` / `type="storageclass"` group nodes SHALL NOT emit `application`
or `containers`. The attributes SHALL NOT appear inside `labels`, and SHALL NOT be
encoded as numbers or booleans. Because both are `omitempty`, a pod with neither a
resolved Application nor container info produces a `data` object byte-identical to
the pre-change shape.

#### Scenario: Pod node carries application when resolved

- **WHEN** the response contains a pod node whose `kube_pod_owner` series carried an `argocd_tracking_id` resolving to Application `checkout`
- **THEN** the corresponding `type="pod"` node carries `data.application: "checkout"` and `data.labels` contains no `argocd_tracking_id` / `application` key

#### Scenario: Pod node carries containers when resolved

- **WHEN** the response contains a pod node whose `kube_pod_container_info` series listed containers `app` (`reg/app:1.2`) and `sidecar` (`reg/proxy:0.9`)
- **THEN** the corresponding `type="pod"` node carries `data.containers: [{"name":"app","image":"reg/app:1.2"},{"name":"sidecar","image":"reg/proxy:0.9"}]` ordered by `(name, image)` and `data.labels` contains no container key

#### Scenario: Pod node omits application and containers when unresolved

- **WHEN** the response contains a pod node with no resolved ArgoCD Application and no container info
- **THEN** the corresponding `type="pod"` node's `data` object includes neither an `application` field nor a `containers` field

#### Scenario: Non-pod nodes never carry application or containers

- **WHEN** the response contains nodes of `type="node"`, `type="pvc"`, `type="service"`, or `type="external"`
- **THEN** those node `data` objects include neither an `application` field nor a `containers` field

#### Scenario: Deterministic body with new attributes

- **WHEN** the same pod (same Application and container set) is produced by two consecutive builds for the same time bucket
- **THEN** the pod node's `data.application` and `data.containers` are byte-identical between the two builds, with `data.containers` ordered by `(name, image)`
