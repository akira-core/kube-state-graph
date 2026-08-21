# Implementation notes

## Task 4.4 — upstream metric names

The target store was not reachable from this implementation environment, so
exported names could not be confirmed live with
`group by (__name__) ({__name__=~"traces_service_graph.*"})`.

Contract assumed (and documented in README):

| Metric | Notes |
|---|---|
| `traces_service_graph_request_total` | already in use |
| `traces_service_graph_request_failed_total` | OTel servicegraph connector default |
| `traces_service_graph_request_server_seconds_bucket` | requires Prometheus exporter `add_metric_suffixes` (default on) |

If `add_metric_suffixes` is off the histogram becomes
`traces_service_graph_request_server_bucket` and `p90_server_ms` degrades
gracefully (logged as duration query empty / no series).
