## MODIFIED Requirements

### Requirement: govulncheck on every PR

The repository SHALL include a CI workflow step that runs `golang.org/x/vuln/cmd/govulncheck@latest ./...` on every pull request. Detected vulnerabilities SHALL gate merges; suppressions SHALL be made explicit via comment plus a tracking issue, never via silent ignoring.

The repository SHALL NOT carry a standing suppression list. When govulncheck reports a finding, the default resolution is to **upgrade the affected module**, including when the vulnerable code path is judged unreachable from this binary — govulncheck's reachability analysis is symbol-level and cannot distinguish a linked-but-unused control-plane feature from a live one, so a module-scoped exclusion would also mask the next finding in that module, which may be reachable. Suppressing a specific finding remains permitted where an upgrade is genuinely unavailable, under the documentation rule above.

#### Scenario: govulncheck flags a known CVE

- **WHEN** a vulnerable transitive dependency reachable from the binary is on the dependency graph
- **THEN** `govulncheck ./...` exits non-zero and the PR's `vuln` job fails

#### Scenario: Suppressions documented

- **WHEN** a contributor needs to suppress a finding (e.g., a vulnerability that does not affect the reachable code path)
- **THEN** the suppression appears as a code comment referencing a tracked issue, not as a removal of the `vuln` job

#### Scenario: An unreachable finding is still resolved by upgrading

- **GIVEN** a finding whose vulnerable code path this binary never executes
- **WHEN** a fixed version of the module is available
- **THEN** the module is upgraded rather than excluded, so a later finding in the same module still fails the job
