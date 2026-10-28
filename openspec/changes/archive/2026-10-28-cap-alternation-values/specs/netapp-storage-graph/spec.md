## MODIFIED Requirements

### Requirement: Deterministic QoS chunking with per-chunk degradation

The builder SHALL **chunk deterministically** the QoS scope and issue one query per chunk per family, merging chunk results in **chunk order**, not completion order. Each chunk SHALL degrade independently and SHALL NOT fail the build.

#### Scenario: Chunking bounds the query length

- **WHEN** the alternation grows with the number of matched volumes past what upstream installations accept as query length
- **THEN** the scope is split into chunks and one query is issued per chunk per family

#### Scenario: Chunking bounds the alternation's value count

- **WHEN** the matched scope holds more than 100 FlexVol names, even when they all fit the byte budget
- **THEN** it is split so that no chunk's `volume` alternation holds more than 100 values, so the upstream resolves each chunk by exact value lookup rather than by evaluating a regex against every `volume` value it indexes
- **AND** the merged result is identical to the one a byte-only split produces, and which claims a failed chunk costs on `/v1/graph` remains a pure function of the scope

#### Scenario: An oversized volume name is still issued

- **WHEN** a single volume name alone exceeds the chunk budget
- **THEN** it is still always issued, because dropping it would remove a claim's measurements silently

#### Scenario: Large scope is chunked and merged deterministically

- **WHEN** the matched scope is large enough to exceed the chunk budget and is split across several queries per family
- **THEN** the merged result is identical to a single unchunked read of the same scope, and two consecutive builds over the same estate produce byte-identical I/O values
- **AND** the summed I/O values are a pure function of the scope and independent of upstream timing

#### Scenario: One failed chunk costs only its own claims

- **WHEN** one chunk of one QoS family fails while every other chunk succeeds
- **THEN** the build succeeds, claims carried by the successful chunks keep their `metrics`, claims carried by the failed chunk emit their edge with no `metrics` key and are counted by the I/O-coverage signal, and no claim loses its aggregate, controller or `svm`
- **AND** which claims a failed chunk costs is a pure function of the scope, and the `pvc-to-netapp-aggr` edges, the aggregate and controller entities and the PVC `svm` labels are unaffected by any QoS chunk outcome

