## ADDED Requirements

### Requirement: Storage-graph end-time alignment

`GET /v1/storage-graph` SHALL apply the same end-time alignment as `GET /v1/graph` ("Time-window passthrough" in the `graph-api` capability): with a non-zero `--end-align` grid, `end` is floored to the grid and `start` shifted by the same amount before any upstream query is rendered, and the alignment SHALL happen after `start` / `end` validation, so validation errors are unchanged.

#### Scenario: Storage request aligned like a graph request

- **WHEN** `--end-align=30s` and a client sends `GET /v1/storage-graph?start=2026-05-01T12:00:10Z&end=2026-05-01T12:05:10Z&az=zone-a&env=prod`
- **THEN** every upstream query is evaluated at `2026-05-01T12:05:00Z` over a 5m window
