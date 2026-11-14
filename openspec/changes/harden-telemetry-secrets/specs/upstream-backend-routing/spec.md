# Spec Delta

## ADDED Requirements

### Requirement: Upstream endpoint URLs are rendered with userinfo redacted

Wherever the server renders an upstream endpoint URL for humans — `--prom-url` / `KSG_PROM_URL` and each routing-file backend `url` in startup and reload log lines, in the rendered form of a backend or table, and in a configuration or validation error — it SHALL render the URL with any userinfo password replaced (`url.URL.Redacted()`), and SHALL NOT echo a URL that cannot be parsed into a host-bearing form. The URL used to dial the backend SHALL be unchanged.

#### Scenario: Prom URL with userinfo in the startup log

- **WHEN** the server starts with `--prom-url=http://ksg:s3cret@vm.example:8428` and no `--backends-file`
- **THEN** every startup log line naming the endpoint carries `http://ksg:xxxxx@vm.example:8428` and none contains `s3cret`
- **AND** upstream requests still go to `vm.example:8428`

#### Scenario: Routing-file URL with userinfo in the table rendering

- **WHEN** a routing file declares a backend `url: http://ksg:s3cret@vm-a:8428` and the table is logged at startup or after a reload
- **THEN** the backend renders as `url=http://ksg:xxxxx@vm-a:8428` and the log contains no `s3cret`

#### Scenario: Invalid URL errors do not echo the input

- **WHEN** `--prom-url` or a routing-file `url` carries userinfo and is rejected as invalid (unparseable, hostless, or with an unsupported scheme)
- **THEN** the startup or reload error names the setting or backend and contains no password from the input
