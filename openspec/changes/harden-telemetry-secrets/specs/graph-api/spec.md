# Spec Delta

## ADDED Requirements

### Requirement: Boot warning when a configured key source yields no keys

The server SHALL log a warning at boot, identifying that authentication is disabled, when a configured API-key source yields zero keys, and SHALL NOT log an "auth enabled" line in that case. The warning SHALL carry no key value and SHALL NOT change which requests are accepted.

#### Scenario: Key file with no keys

- **WHEN** the server starts with `--api-keys-file` naming a file that is empty or holds only comments
- **THEN** the boot log carries a warning that API-key auth is disabled because the file yielded no keys, and no line claims auth is enabled
- **AND** every `/v1/*` route accepts requests with no `X-API-Key` header
- **AND** when `--api-keys-reload-interval` is positive the file is still re-read on that cadence, so keys written to it later enable auth without a restart

#### Scenario: Key list with no keys

- **WHEN** the server starts with `--api-keys=,` (or `KSG_API_KEYS=,`) and no `--api-keys-file`
- **THEN** the boot log carries a warning that API-key auth is disabled, and no line claims auth is enabled

### Requirement: Boot warning for API keys passed as a literal flag

The server SHALL log one warning at boot when keys were supplied as a literal `--api-keys` command-line flag, whether or not `--api-keys-file` takes precedence over it, naming `--api-keys-file` and `KSG_API_KEYS` as the alternatives. The warning SHALL carry no key value and SHALL NOT change which requests are accepted.

#### Scenario: Keys passed as a literal flag

- **WHEN** the server starts with `--api-keys=k1` on the command line and no `--api-keys-file`
- **THEN** the server enforces `k1` exactly as before, and the boot log carries one warning that the keys are visible in the process command line, naming `--api-keys-file` and `KSG_API_KEYS`
- **AND** the warning does not contain `k1`

#### Scenario: Keys from the environment

- **WHEN** the server starts with `KSG_API_KEYS=k1` and no `--api-keys` flag
- **THEN** no literal-flag warning is logged
