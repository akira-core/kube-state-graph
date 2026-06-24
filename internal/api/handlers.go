package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/marz32one/kube-state-graph/internal/telemetry"
	"github.com/marz32one/kube-state-graph/pkg/build"
	"github.com/marz32one/kube-state-graph/pkg/cytoscape"
	"github.com/marz32one/kube-state-graph/pkg/graph"
	"github.com/marz32one/kube-state-graph/pkg/kubegraph"
	"github.com/marz32one/kube-state-graph/pkg/promql"
)

// ----- /v1/graph (Cytoscape.js) ---------------------------------------------

// handleGraph returns the multi-cluster pod / node / PVC graph for [start,end].
//
//	@Summary		Get multi-cluster graph (Cytoscape.js)
//	@Description	Returns the joined multi-cluster pod / node / PVC graph for the supplied `[start, end]` window in Cytoscape.js JSON shape (`{ elements: { nodes:[…], edges:[…] } }`).
//	@Description
//	@Description	**Window**: `start`/`end` accept RFC 3339 or Unix seconds. Only `end > start` is enforced; the pair is passed through to upstream PromQL verbatim. Bounded query cost is delegated to upstream VictoriaMetrics search limits. Each request triggers a fresh fan-out — there is no in-process result cache.
//	@Description
//	@Description	**Filters** (all repeatable; AND across param names, OR within a single name): `cluster`, `namespace`, `edge_type`, `name`. The `name` filter matches `n.Name()` exactly across every node type (pod, K8s node, PVC, external).
//	@Description
//	@Description	**Traversal** (set `root` to enable): `depth` 0..6 (default 2), `direction` `in`/`out`/`both` (default `both`).
//	@Description
//	@Description	**Node types**: `pod`, `node`, `pvc`, `service`, `external`, plus the presentation-only `cluster` and `storageclass` compound group nodes synthesised by the Cytoscape serialiser (`cluster > node > pod`, `cluster > storageclass > pvc`). **Edge types**: `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`.
//	@Description
//	@Description	**Endpoint resolution**: for a call endpoint whose pod UID is empty, the `client`/`server` label is inspected for a `://` connection string (no operator knob — detection is hardcoded). When present, the URL host is parsed (an optional `.svc.<domain>` suffix is stripped): both a `<service>.<namespace>` host and a headless `<pod>.<service>.<namespace>` host resolve to the addressed `(namespace, service)`, looked up across every loaded cluster in the caller's family (cluster names equal after normalising digit runs; anchored on the UID-recovered client-pod cluster when available, else the trace-source label). Each surviving family cluster materialises its own `service` node (`<cluster>/<ns>/<service>`) plus on-demand `service-selects-pod` edges to its own cluster's backing pods, and yields one `pod-calls-service` edge per match — such edges MAY cross clusters. Candidates provably without backing pods (zero endpoints in an endpoint-visible cluster) are pruned when an endpoint-backed sibling exists; an anchor naming no loaded family falls back to the single loaded family holding the service (multi-family names are ambiguous and stay external). Zero surviving candidates yield an `external` node (`external/<value>`). A non-URL missing-UID label also yields an `external` node. Calls whose target is not a service stay typed `pod-calls-pod`. See `/v1/edge-types` for the authoritative per-type catalogue.
//	@Description
//	@Description	Example: `GET /v1/graph?start=2026-05-05T11:00:00Z&end=2026-05-05T12:00:00Z&cluster=prod-eu&namespace=payments&edge_type=pod-calls-pod`
//	@Description
//	@Description	<details><summary><b>Sample response</b></summary>
//	@Description
//	@Description	```json
//	@Description	{
//	@Description	  "apiVersion": "v1",
//	@Description	  "clusters": ["prod-eu", "prod-us"],
//	@Description	  "elements": {
//	@Description	    "nodes": [
//	@Description	      { "data": { "id": "prod-eu/8f8d4f1a-...-89ab", "type": "pod",  "name": "checkout-7d9f6c8b8-abcde", "owner": { "kind": "Deployment", "name": "checkout" }, "application": "checkout", "containers": [ { "name": "app", "image": "reg.example/checkout:1.4" }, { "name": "istio-proxy", "image": "reg.example/proxy:0.9" } ], "labels": { "cluster": "prod-eu", "namespace": "payments" } } },
//	@Description	      { "data": { "id": "prod-eu/ip-10-0-1-23",     "type": "node", "name": "ip-10-0-1-23.ec2.internal", "labels": { "cluster": "prod-eu" } } }
//	@Description	    ],
//	@Description	    "edges": [
//	@Description	      { "data": { "id": "...uuidv5...", "type": "pod-calls-pod",   "source": "prod-eu/8f8d4f1a-...-89ab", "target": "prod-us/a1b2c3d4-...-7654", "labels": { "cluster": "prod-eu" } } }
//	@Description	    ]
//	@Description	  }
//	@Description	}
//	@Description	```
//	@Description
//	@Description	</details>
//	@Tags			graph
//	@Produce		json
//	@Param			start		query		string		true	"Window start. RFC 3339 (`2026-05-05T11:00:00Z`) or Unix seconds (`1746442800`)."	example(2026-05-05T11:00:00Z)
//	@Param			end			query		string		true	"Window end. RFC 3339 or Unix seconds. Must be > start."	example(2026-05-05T12:00:00Z)
//	@Param			cluster		query		[]string	false	"Restrict to listed clusters (repeatable, OR-combined). Names match the upstream `cluster` label."	collectionFormat(multi)	example(prod-eu)
//	@Param			namespace	query		[]string	false	"Restrict to listed Kubernetes namespaces (repeatable, OR-combined)."	collectionFormat(multi)	example(payments)
//	@Param			edge_type	query		[]string	false	"Restrict to listed edge types. Repeatable, OR-combined."	collectionFormat(multi)	Enums(pod-mounts-pvc,pod-calls-pod,pod-calls-service,service-selects-pod)	example(pod-calls-pod)
//	@Param			name		query		[]string	false	"Restrict to nodes whose name matches exactly across every node type (pod, K8s node, PVC, service, external). Repeatable; name collisions across types or clusters return all matches. Edges incident on a matching node are kept and the partner endpoint is re-added subject to other filters."	collectionFormat(multi)	example(checkout-7d9f6c8b8-abcde)
//	@Param			root		query		string		false	"Cluster-scoped node ID anchoring a traversal. Format depends on type — pods `<cluster>/<uid>`, nodes `<cluster>/<node>`, PVCs `<cluster>/<ns>/<claim>`, services `<cluster>/<ns>/<service>`, externals `external/<value>`."	example(prod-eu/8f8d4f1a-1234-4abc-9def-0123456789ab)
//	@Param			depth		query		int			false	"BFS traversal depth in hops. Range `0..6`. Defaults to `2` when `root` is set, ignored otherwise."	minimum(0)	maximum(6)	default(2)	example(2)
//	@Param			direction	query		string		false	"Traversal direction relative to `root`. `out` = downstream edges, `in` = upstream edges, `both` = undirected. Defaults to `both`."	Enums(in,out,both)	default(both)	example(both)
//	@Param			X-API-Key	header		string		false	"API key. Required when the server is started with API keys configured."
//	@Success		200			{object}	cytoscape.Body
//	@Failure		400			{object}	errorBody	"Invalid parameters (missing/invalid start|end, invalid_range, invalid_depth, depth_too_large, invalid_scope, outside_retention)"
//	@Failure		401			{object}	errorBody	"Missing or invalid `X-API-Key` (only when API key auth is configured)"
//	@Failure		502			{object}	errorBody	"Upstream VictoriaMetrics returned an error (RFC 9110 §15.6.3)"
//	@Failure		504			{object}	errorBody	"Build exceeded --build-timeout (RFC 9110 §15.6.5)"
//	@Security		ApiKeyAuth
//	@Router			/v1/graph [get]
func (s *Server) handleGraph(c *gin.Context) {
	req, errBody := s.parseGraphRequest(c)
	if errBody != nil {
		return
	}
	g, err := s.runBuild(c.Request.Context(), req)
	if err != nil {
		s.mapBuildError(c, err)
		return
	}

	view := s.projectWithSpan(c.Request.Context(), g, req.scope)
	body := s.serialiseWithSpan(c.Request.Context(), "cytoscape", func() any {
		return cytoscape.Serialise(g, view)
	}, view)
	s.writeJSON(c, body, "cytoscape")
}

// projectWithSpan wraps graph.Project in a `kube-state-graph.project` span.
func (s *Server) projectWithSpan(ctx context.Context, g *graph.Graph, scope graph.Scope) graph.View {
	ctx, span := telemetry.Tracer().Start(ctx, "kube-state-graph.project")
	defer span.End()
	ptStart := time.Now()
	view := graph.Project(g, scope)
	s.metrics.ProjectDuration.Observe(time.Since(ptStart).Seconds())
	span.SetAttributes(
		attribute.Int("graph.node.count", len(view.Nodes)),
		attribute.Int("graph.edge.count", len(view.Edges)),
	)
	_ = trace.SpanFromContext(ctx) // keep ctx referenced for static analysis
	return view
}

// serialiseWithSpan wraps a serialiser callback in a `kube-state-graph.serialise`
// span carrying the chosen format and resulting node/edge counts.
func (s *Server) serialiseWithSpan(ctx context.Context, format string, fn func() any, view graph.View) any {
	_, span := telemetry.Tracer().Start(ctx, "kube-state-graph.serialise",
		trace.WithAttributes(
			attribute.String("kube_state_graph.serialiser", format),
			attribute.Int("graph.node.count", len(view.Nodes)),
			attribute.Int("graph.edge.count", len(view.Edges)),
		),
	)
	defer span.End()
	return fn()
}

// runBuild wraps Builder.Build in a per-request build-timeout context. On
// context.DeadlineExceeded the error is normalised to ReasonTimeout (504) so
// the handler-side mapBuildError surfaces the RFC 9110 §15.6.5 status.
func (s *Server) runBuild(ctx context.Context, req graphRequest) (*graph.Graph, error) {
	buildCtx, cancel := context.WithTimeout(ctx, s.cfg.BuildTimeout)
	defer cancel()

	start := time.Now()
	g, err := s.builder.Build(buildCtx, req.end.Sub(req.start), req.end)
	s.metrics.BuildDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			s.metrics.BuildRejected.WithLabelValues("timeout").Inc()
			return nil, build.NewError(build.ReasonTimeout, "build timeout", err)
		}
		return nil, err
	}
	return g, nil
}

// clustersBody is the response shape of GET /v1/clusters.
type clustersBody struct {
	APIVersion string        `json:"apiVersion"`
	Clusters   []ClusterInfo `json:"clusters"`
}

// edgeTypesBody is the response shape of GET /v1/edge-types.
type edgeTypesBody struct {
	APIVersion string                     `json:"apiVersion"`
	EdgeTypes  []graph.EdgeTypeDefinition `json:"edge_types"`
}

// ----- /v1/clusters ---------------------------------------------------------

// ClusterInfo is one entry in /v1/clusters.
type ClusterInfo struct {
	Name string `json:"name"`
}

// handleClusters returns the list of clusters with data in centralised
// VictoriaMetrics over a fixed 1 h discovery lookback.
//
//	@Summary		List clusters
//	@Description	Returns the set of clusters observed in `kube_node_info` over a fixed 1 h lookback. Each request hits VictoriaMetrics directly under `--api-timeout`.
//	@Description
//	@Description	<details><summary><b>Sample response</b></summary>
//	@Description
//	@Description	```json
//	@Description	{
//	@Description	  "apiVersion": "v1",
//	@Description	  "clusters": [
//	@Description	    { "name": "prod-eu" },
//	@Description	    { "name": "prod-us" },
//	@Description	    { "name": "stage-eu" }
//	@Description	  ]
//	@Description	}
//	@Description	```
//	@Description
//	@Description	</details>
//	@Tags			discovery
//	@Produce		json
//	@Param			X-API-Key	header		string		false	"API key. Required when the server is started with API keys configured."
//	@Success		200	{object}	clustersBody
//	@Failure		401	{object}	errorBody	"Missing or invalid `X-API-Key` (only when API key auth is configured)"
//	@Failure		502	{object}	errorBody	"Upstream VictoriaMetrics returned an error"
//	@Failure		504	{object}	errorBody	"Discovery query exceeded --api-timeout"
//	@Security		ApiKeyAuth
//	@Router			/v1/clusters [get]
func (s *Server) handleClusters(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), s.cfg.APITimeout)
	defer cancel()

	clusters, err := s.discoverClusters(ctx)
	if err != nil {
		// Classify into the typed build.Reason and delegate, so the
		// status/reason/redaction contract lives in exactly one switch
		// (mapBuildError): canceled → 499, deadline → 504 with the static
		// build-authored message, anything else → sanitised 502 — the raw
		// error embeds the internal VictoriaMetrics URL/host/IP and is kept
		// server-side only.
		switch {
		case errors.Is(err, context.Canceled):
			err = build.NewError(build.ReasonCanceled, "request canceled", err)
		case errors.Is(err, context.DeadlineExceeded):
			err = build.NewError(build.ReasonTimeout, "cluster discovery timed out", err)
		default:
			err = build.NewError(build.ReasonUpstream, "cluster discovery failed", err)
		}
		s.mapBuildError(c, err)
		return
	}
	body := clustersBody{
		APIVersion: APIVersion,
		Clusters:   clusters,
	}
	raw, _ := json.Marshal(body)
	c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
}

func (s *Server) discoverClusters(ctx context.Context) ([]ClusterInfo, error) {
	q := s.r.Render(promql.QClusterDiscovery, promql.ClusterDiscoveryLookback)
	vec, err := s.prom.Instant(ctx, string(promql.QClusterDiscovery), q, s.clk.Now().UTC())
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, sample := range vec {
		c := string(sample.Metric["cluster"])
		if c == "" {
			c = "unknown"
		}
		seen[c] = struct{}{}
	}
	out := make([]ClusterInfo, 0, len(seen))
	for c := range seen {
		out = append(out, ClusterInfo{Name: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ----- /v1/edge-types -------------------------------------------------------

// handleEdgeTypes returns the static catalogue of edge types this server
// can produce.
//
//	@Summary		Edge-type catalogue
//	@Description	Static catalogue of edge types this server can produce — directionality, valid source/target node types, supported labels, and whether the edge may cross cluster boundaries. No upstream calls; served with `Cache-Control: public, max-age=3600`. Use this to validate the `edge_type` filter on `/v1/graph` and to drive UI legends.
//	@Description
//	@Description	<details><summary><b>Edge type matrix</b></summary>
//	@Description
//	@Description	| type | source → target | directed | cross-cluster |
//	@Description	|---|---|---|---|
//	@Description	| `pod-mounts-pvc` | pod → pvc | yes | no |
//	@Description	| `pod-calls-pod` | pod \| service \| external → pod \| external | yes | yes |
//	@Description	| `pod-calls-service` | pod \| service \| external → service | yes | yes |
//	@Description	| `service-selects-pod` | service → pod | yes | no |
//	@Description
//	@Description	A call endpoint whose pod UID is empty and whose `client`/`server` label is a `://` connection string is resolved (detection hardcoded, no operator knob) against every loaded cluster in the caller's family (cluster names equal after normalising digit runs; anchored on the UID-recovered client-pod cluster when available, else the trace-source label): each surviving family cluster holding the addressed Service yields a `service` node and a `pod-calls-service` edge (which may therefore cross clusters). Candidates provably without backing pods (zero endpoints in an endpoint-visible cluster) are pruned when an endpoint-backed sibling exists; an anchor naming no loaded family falls back to the single loaded family holding the Service (multi-family names are ambiguous and stay external). Zero surviving candidates yield an `external` node. A non-URL missing-UID label resolves to an `external` node. `service-selects-pod` edges are materialised on demand from `service` nodes to their own cluster's backing pods.
//	@Description
//	@Description	</details>
//	@Tags			discovery
//	@Produce		json
//	@Param			X-API-Key	header		string		false	"API key. Required when the server is started with API keys configured."
//	@Success		200	{object}	edgeTypesBody
//	@Failure		401	{object}	errorBody	"Missing or invalid `X-API-Key` (only when API key auth is configured)"
//	@Security		ApiKeyAuth
//	@Router			/v1/edge-types [get]
func (s *Server) handleEdgeTypes(c *gin.Context) {
	body := edgeTypesBody{
		APIVersion: APIVersion,
		EdgeTypes:  graph.EdgeTypes,
	}
	raw, _ := json.Marshal(body)
	c.Header("Cache-Control", "public, max-age=3600")
	c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
}

// ----- /livez, /readyz ------------------------------------------------------

// handleLivez is the liveness probe.
//
//	@Summary	Liveness
//	@Tags		health
//	@Produce	plain
//	@Success	200	{string}	string	"ok"
//	@Router		/livez [get]
func (s *Server) handleLivez(c *gin.Context) {
	c.String(http.StatusOK, "ok")
}

// handleReadyz is the readiness probe — issues an upstream `up{}` query under
// --api-timeout. Probe failure → 503 Service Unavailable (k8s probe convention).
//
//	@Summary	Readiness
//	@Description	Returns 200 only when an `up{}` probe against the configured upstream succeeds within --api-timeout.
//	@Tags		health
//	@Produce	plain
//	@Success	200	{string}	string	"ok"
//	@Failure	503	{object}	errorBody
//	@Router		/readyz [get]
func (s *Server) handleReadyz(c *gin.Context) {
	probeCtx, cancel := context.WithTimeout(c.Request.Context(), s.cfg.APITimeout)
	defer cancel()
	_, err := s.prom.Instant(probeCtx, string(promql.QUpProbe), s.r.Render(promql.QUpProbe, 0), s.clk.Now().UTC())
	if err != nil {
		// /readyz is unauthenticated; the raw upstream error embeds the internal
		// VictoriaMetrics URL/host/IP. Return a static message and keep the
		// detail server-side (the promql client already logs it at Error level).
		s.logger.WarnContext(c.Request.Context(), "readyz upstream probe failed", "err", err)
		writeError(c, http.StatusServiceUnavailable, "upstream_unreachable", "upstream probe failed")
		return
	}
	c.String(http.StatusOK, "ok")
}

// ----- request parsing ------------------------------------------------------

type graphRequest struct {
	start  time.Time
	end    time.Time
	scope  graph.Scope
	format string
}

// parseGraphRequest delegates parsing to the shared kubegraph.ParseValues (the
// single source of truth for the /v1/graph request contract, also used by
// Engine.BuildFromValues), then maps a *kubegraph.ParseError to the existing
// HTTP 400 response with its stable reason code.
func (s *Server) parseGraphRequest(c *gin.Context) (graphRequest, error) {
	start, end, scope, err := kubegraph.ParseValues(c.Request.URL.Query())
	if err != nil {
		var pe *kubegraph.ParseError
		if errors.As(err, &pe) {
			writeError(c, http.StatusBadRequest, pe.Reason, pe.Message)
		} else {
			writeError(c, http.StatusBadRequest, "invalid_request", err.Error())
		}
		return graphRequest{}, err
	}
	return graphRequest{
		start:  start,
		end:    end,
		scope:  scope,
		format: "cytoscape",
	}, nil
}

// ----- response helpers -----------------------------------------------------

func (s *Server) writeJSON(c *gin.Context, body any, format string) {
	start := time.Now()
	raw, err := json.Marshal(body)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "encode", err.Error())
		return
	}
	s.metrics.SerialiseDuration.WithLabelValues(format).Observe(time.Since(start).Seconds())
	c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
}
