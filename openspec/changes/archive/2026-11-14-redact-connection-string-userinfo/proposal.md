# Proposal

## Why

When a `"://"` connection-string label cannot be resolved to a Service, the reader turns it into an `external` node whose `id` is `external/<label>` and whose `name` is the label, verbatim. Database and broker clients commonly put credentials in the authority of that string (`mongodb://app:pw@mongo.example:27017/db`). An instrumentation that copies its connection string into the span's peer attribute therefore puts a password into every `/v1/graph` body, and into every Debug log line that names the series. API-key auth is off by default, so the body may be readable by anyone who can reach the server. The series also sits in the TSDB, but the graph API should not republish it, and the operator's log store should not collect it.

## What Changes

- When a `"://"` label falls back to an `external` node, the node's `id` and `name` are the label with every URL userinfo removed (`user:password@`, or a bare `user@`). The rest of the label stays as it is, with no normalisation. A label without userinfo produces exactly the bytes it produced before.
- Two labels that differ only in their credentials name the same endpoint. They now collapse onto one `external` node and one edge. A credential rotated inside the window therefore does not split the endpoint in two.
- Resolution is unchanged. The host is still read from the raw label, so a credentialled label naming a Service the anchor cluster holds still resolves to that Service node.
- The Debug records the service-graph parse emits per series carry the same stripped form wherever they name a `client` / `server` label or a connection string.
- The missing-UID human-label fallback and the unknown-server peer-address fallback are untouched. Their values are not connection strings, and the spec already requires them raw.
- Out of scope: credentials in a query string (`?password=…`), and credentials inside a value that is not `"://"`-delimited.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `pod-service-graph`: an unresolvable connection string falls back to an `external` node whose `id` and `name` carry no URL userinfo, and that applies to the filtered-build out-of-scope form too. A new requirement states that no response body or Debug record derived from a connection-string label carries its userinfo.

## Impact

- **Code**: `pkg/build/servicegraph.go`. It gets a userinfo stripper, applied at the three external fallbacks of connection-string resolution, to the series identity threaded into Debug records, and to the per-series Debug sites that log a local label.
- **API**: **BREAKING** for affected nodes only. An `external` node built from a connection string that carried userinfo changes its `id` and `name`, and so do the UUIDv5 ids of its edges. Every other node and edge is byte-identical. `docs/BREAKING.md` gets an entry.
- **Go API**: no exported change.
- **Goldens**: unchanged. No golden fixture carries a connection string with userinfo.
- **Docs**: `.claude/rules/service-graph-resolution.md`, `docs/BREAKING.md`.
- **Tests**: new unit tests for the stripper, for the node `id`/`name` on every external fallback (server side, client side, filtered build), for credential collapse, and for the absence of the secret anywhere in the captured Debug output.
