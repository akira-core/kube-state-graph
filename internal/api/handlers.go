package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/kube-state-graph/internal/telemetry"
	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// ----- /v1/graph (Cytoscape.js) ---------------------------------------------

// handleGraph returns the multi-cluster pod / node / PVC graph for [start,end].
//
//	@Summary		Get multi-cluster graph (Cytoscape.js)
//	@Description	Returns the joined multi-cluster pod / node / PVC graph for the supplied `[start, end]` window in Cytoscape.js JSON shape (`{ elements: { nodes:[…], edges:[…] } }`).
//	@Description
//	@Description	**Window**: `start`/`end` accept RFC 3339 or Unix seconds. Only `end > start` is enforced; the pair is passed through to upstream PromQL verbatim. Bounded query cost is delegated to upstream VictoriaMetrics search limits. Each request triggers a fresh fan-out — there is no in-process result cache.
//	@Description
//	@Description	**Filters** (all repeatable; AND across param names, OR within a single name): `cluster`, `namespace`, `az`, `env`. The withdrawn `name`, `root`, `depth`, `direction` and `edge_type` parameters are ignored without error, whatever value they carry.
//	@Description
//	@Description	**Node types**: `pod`, `node`, `pvc`, `service`, `external`, `netapp-aggr`, `netapp-node`, plus the presentation-only `cluster`, `storage-cluster`, `namespace`, `application` and `controller` compound group nodes synthesised by the Cytoscape serialiser (`cluster > namespace > application > controller > pod`, with absent levels skipped; `cluster > namespace > [application >] {service, pvc}`; `cluster > node`; `storage-cluster > netapp-node > netapp-aggr`, where the real `netapp-node` is the compound parent of its aggregates). **Edge types**: `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`, `pod-to-node`, `pvc-to-netapp-aggr`. An ingress entry-point `service` node additionally carries `labels.role` — `ingress-gateway` (a routed hit's chain entry; gateway pods and a synthesized `pod-calls-service` hop to the backend exist behind it) or `ingress-lb` (the ingress LB fallback destination; no routed backend). The key is absent on every other service node.
//	@Description
//	@Description	**Edge `data.metrics`**: a union of two disjoint families. RED (trace-derived call edges): `rate` (req/s, required within the family), `error_rate`, `p90_server_ms`. I/O (`pvc-to-netapp-aggr` edges): `read_ops`, `write_ops`, `read_latency_us`, `write_latency_us`, `read_bytes_per_sec`, `write_bytes_per_sec` from Harvest, verbatim. Schema-level every field is optional (rate moved off `required`); a RED object always carries a positive `rate`. All values are JSON numbers rounded to 6 significant digits and MAY appear in exponent form. The key is omitted entirely when the edge has no measurements.
//	@Description
//	@Description	**Endpoint resolution**: for a call endpoint whose pod UID is empty, the `client`/`server` label is inspected for a `://` connection string (no operator knob — detection is hardcoded). When present, the URL host is parsed (an optional `.svc.<domain>` suffix is stripped): both a `<service>.<namespace>` host and a headless `<pod>.<service>.<namespace>` host resolve to the addressed `(namespace, service)`. Resolution is anchored on ONE cluster — the UID-recovered client-pod cluster when available, else the trace-source label — and succeeds only when that cluster itself holds the same-named Service; a family sibling holding it is not enough, and there is no cross-family fallback, so this path is always intra-cluster. It materialises a single `service` node (`<cluster>/<ns>/<service>`) and one `pod-calls-service` edge. From that node, on-demand `service-selects-pod` edges fan out to the backing pods of EVERY same-family cluster holding the same-named Service (cluster names equal after normalising digit runs), so those edges MAY cross clusters; there is no endpoint-backed pruning — a sibling with zero endpoints simply contributes none. A `server="unknown"` endpoint whose client resolved to a real pod is instead classified from its peer address (in-cluster DNS name, bare short Service name in the client pod's namespace, or a ClusterIP literal looked up in the client's own cluster) and resolves under the same anchor rule. When the optional Istio route engine is configured, a global/ingress FQDN that would otherwise fall through resolves to the Service the selected ingress cluster's Gateway + VirtualService config routed it to — that cluster may be a family sibling of the caller's, so the resulting `pod-calls-service` edge MAY cross clusters, and the ingress entry point's own fan-out is locked to the selected cluster's endpoints. Anything unresolved yields an `external` node (`external/<value>`), as does a non-URL missing-UID label. Calls whose target is not a service stay typed `pod-calls-pod`.
//	@Description
//	@Description	Example: `GET /v1/graph?start=2026-05-05T11:00:00Z&end=2026-05-05T12:00:00Z&cluster=prod-eu&namespace=payments`
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
//	@Description	      { "data": { "id": "...uuidv5...", "type": "pod-calls-pod",   "source": "prod-eu/8f8d4f1a-...-89ab", "target": "prod-us/a1b2c3d4-...-7654", "labels": { "cluster": "prod-eu" }, "metrics": { "rate": 5, "error_rate": 0.1, "p90_server_ms": 12.5 } } }
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
//	@Param			cluster		query		[]string	false	"Restrict to listed clusters (repeatable, OR-combined). Pushed into every cluster-labelled upstream query. The value `unknown` addresses series carrying no `cluster` label."	collectionFormat(multi)	example(prod-eu)
//	@Param			namespace	query		[]string	false	"Restrict to listed Kubernetes namespaces (repeatable, OR-combined). Pushed into every namespace-labelled upstream query; nodes and NetApp aggregates follow by reference."	collectionFormat(multi)	example(payments)
//	@Param			az			query		[]string	false	"Restrict to listed availability zones (repeatable, OR-combined). Pushed into every topology query as a matcher on the deployment's configured zone label (default `az`, see --az-label)."	collectionFormat(multi)	example(eu-west-1a)
//	@Param			env			query		[]string	false	"Restrict to listed environments (repeatable, OR-combined). Pushed into every topology query as a matcher on the deployment's configured environment label (default `env`, see --env-label)."	collectionFormat(multi)	example(prod)
//	@Param			prune		query		boolean		false	"Keep only workload on a connectivity edge (the default). `false` returns the inventory instead: every loaded pod with its node / PVC / NetApp chain, plus unreferenced infrastructure when no cluster or namespace filter narrows it."	default(true)	example(true)
//	@Param			X-API-Key	header		string		false	"API key. Required when the server is started with API keys configured."
//	@Success		200			{object}	cytoscape.Body
//	@Failure		400			{object}	errorBody	"Invalid parameters (missing/invalid start|end, invalid_range, invalid_scope, outside_retention)"
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
	g, err := s.runBuild(c.Request.Context(), req.start, req.end, req.sel, s.builder.Build)
	if err != nil {
		s.mapBuildError(c, err)
		return
	}

	view := s.projectWithSpan(c.Request.Context(), "kube-state-graph.project", func() graph.View {
		return graph.Project(g, req.scope)
	})
	body := s.serialiseWithSpan(c.Request.Context(), "cytoscape", func() any {
		return cytoscape.Serialise(g, view)
	}, view)
	s.writeJSON(c, body, "cytoscape")
}

// handleStorageGraph returns the storage-flow graph for [start,end].
//
//	@Summary		Get storage-flow graph (Cytoscape.js)
//	@Description	Returns a storage-rooted flow graph — NetApp controller → aggregate → SVM → PVC → pod → Kubernetes node — for the supplied `[start, end]` window, in the same `{apiVersion, clusters, elements}` Cytoscape.js shape as `/v1/graph`.
//	@Description
//	@Description	**Required**: `start`, `end` (same validation as `/v1/graph`), plus single-valued `az` and `env` (400 `missing_az` / `missing_env` when absent; 400 `invalid_scope` when repeated). They pin one estate so a filer shared across zones is never merged.
//	@Description
//	@Description	**Roots** (optional, repeatable; OR within a name, AND across storage vs workload sides): `ontap_cluster`, `aggr`, `svm`, `pod=<namespace>/<name>`, `application` (ArgoCD Application name, as `data.application` carries it), `node` (matched against both the ONTAP controller name and the Kubernetes node name). An empty root list returns every complete path in the selected estate. A root the upstream names is always drawn, even with no flow; a root no series names is simply absent. An `application` root keeps a path whose pod or claim carries it, and every pod that resolves it is drawn even when it mounts nothing.
//	@Description
//	@Description	`cluster` / `namespace` remain optional narrowing filters. `prune` and every unknown parameter — including the withdrawn `edge_type` — are ignored. Auth, timeout (504) and upstream error mapping match `/v1/graph`.
//	@Tags			graph
//	@Produce		json
//	@Param			start			query		string		true	"Window start. RFC 3339 or Unix seconds."	example(2026-05-01T12:00:00Z)
//	@Param			end				query		string		true	"Window end. Must be > start."	example(2026-05-01T12:05:00Z)
//	@Param			az				query		string		true	"Availability zone (required, single-valued)."	example(zone-a)
//	@Param			env				query		string		true	"Environment (required, single-valued)."	example(prod)
//	@Param			cluster			query		[]string	false	"Restrict to listed Kubernetes clusters (repeatable, OR-combined)."	collectionFormat(multi)
//	@Param			namespace		query		[]string	false	"Restrict to listed namespaces (repeatable, OR-combined)."	collectionFormat(multi)
//	@Param			ontap_cluster	query		[]string	false	"Storage root: ONTAP cluster name."	collectionFormat(multi)
//	@Param			node			query		[]string	false	"Root matched against both ONTAP controller and Kubernetes node names."	collectionFormat(multi)
//	@Param			aggr			query		[]string	false	"Storage root: ONTAP aggregate name."	collectionFormat(multi)
//	@Param			svm				query		[]string	false	"Storage root: SVM name."	collectionFormat(multi)
//	@Param			pod				query		[]string	false	"Workload root: `<namespace>/<pod-name>`."	collectionFormat(multi)	example(shop/orders-0)
//	@Param			application		query		[]string	false	"Workload root: ArgoCD Application name, as `data.application` carries it (the tracking-id segment before the first `:`); matches a path whose pod or claim carries it; every pod resolving it is drawn even with no claim"	collectionFormat(multi)	example(checkout)
//	@Param			X-API-Key		header		string		false	"API key. Required when the server is started with API keys configured."
//	@Success		200				{object}	cytoscape.Body
//	@Failure		400				{object}	errorBody	"Invalid parameters (missing/invalid start|end, missing_az, missing_env, invalid_scope, invalid_range)"
//	@Failure		401				{object}	errorBody	"Missing or invalid `X-API-Key` (only when API key auth is configured)"
//	@Failure		502				{object}	errorBody	"Upstream VictoriaMetrics returned an error"
//	@Failure		504				{object}	errorBody	"Build exceeded --build-timeout"
//	@Security		ApiKeyAuth
//	@Router			/v1/storage-graph [get]
func (s *Server) handleStorageGraph(c *gin.Context) {
	req, errBody := s.parseStorageGraphRequest(c)
	if errBody != nil {
		return
	}
	g, err := s.runBuild(c.Request.Context(), req.Start, req.End, req.Selector,
		func(ctx context.Context, window time.Duration, end time.Time, sel promql.Selector) (*graph.Graph, error) {
			// The roots reach the build: a pod root's name joins the pod scope.
			return s.builder.BuildStorage(ctx, window, end, sel, req.Scope.Roots)
		})
	if err != nil {
		s.mapBuildError(c, err)
		return
	}

	view := s.projectWithSpan(c.Request.Context(), "kube-state-graph.project_storage", func() graph.View {
		return graph.ProjectStorage(g, req.Scope)
	})
	body := s.serialiseWithSpan(c.Request.Context(), "cytoscape", func() any {
		return cytoscape.Serialise(g, view)
	}, view)
	s.writeJSON(c, body, "cytoscape")
}

// projectWithSpan wraps a projection callback (graph.Project or
// graph.ProjectStorage) in a span of the given name, timing it into the
// shared ProjectDuration histogram.
func (s *Server) projectWithSpan(ctx context.Context, spanName string, project func() graph.View) graph.View {
	_, span := telemetry.Tracer().Start(ctx, spanName)
	defer span.End()
	ptStart := time.Now()
	view := project()
	s.metrics.ProjectDuration.Observe(time.Since(ptStart).Seconds())
	span.SetAttributes(
		attribute.Int("graph.node.count", len(view.Nodes)),
		attribute.Int("graph.edge.count", len(view.Edges)),
	)
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

// buildFunc is the shape shared by Builder.Build and a closure binding the
// request's roots into Builder.BuildStorage, so one runBuild serves both
// endpoints and the timeout normalisation cannot drift between them.
type buildFunc func(ctx context.Context, window time.Duration, end time.Time, sel promql.Selector) (*graph.Graph, error)

// runBuild wraps a build in a per-request build-timeout context. On
// context.DeadlineExceeded the error is normalised to ReasonTimeout (504) so
// the handler-side mapBuildError surfaces the RFC 9110 §15.6.5 status.
func (s *Server) runBuild(ctx context.Context, start, end time.Time, sel promql.Selector, run buildFunc) (*graph.Graph, error) {
	buildCtx, cancel := context.WithTimeoutCause(ctx, s.cfg.BuildTimeout, errBuildTimeout)
	defer cancel()

	began := time.Now()
	g, err := run(buildCtx, end.Sub(start), end, sel)
	s.metrics.BuildDuration.Observe(time.Since(began).Seconds())
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			s.metrics.BuildRejected.WithLabelValues("timeout").Inc()
			// Every nested deadline surfaces as DeadlineExceeded; the cause
			// names the budget that ran out in the server-side log. The
			// response still carries only the static message.
			if cause := context.Cause(buildCtx); cause != nil && !errors.Is(err, cause) {
				err = fmt.Errorf("%w: %w", cause, err)
			}
			return nil, build.NewError(build.ReasonTimeout, "build timeout", err)
		}
		return nil, err
	}
	return g, nil
}

// Causes stamped on this package's own timeout contexts.
var (
	errBuildTimeout  = errors.New("build exceeded --build-timeout")
	errReadyzTimeout = errors.New("readiness probe exceeded --api-timeout")
)

// parseStorageGraphRequest delegates to the shared kubegraph.ParseStorageValues
// and maps a *kubegraph.ParseError to the HTTP 400 response exactly as
// parseGraphRequest does.
func (s *Server) parseStorageGraphRequest(c *gin.Context) (kubegraph.StorageRequest, error) {
	req, err := kubegraph.ParseStorageValues(c.Request.URL.Query())
	if err != nil {
		writeParseError(c, err)
		return kubegraph.StorageRequest{}, err
	}
	return req, nil
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
//	@Description	Returns 200 only when an `up{}` probe against every configured upstream backend succeeds within --api-timeout. With no routing table configured there is exactly one implicit backend, so the behaviour is unchanged.
//	@Tags		health
//	@Produce	plain
//	@Success	200	{string}	string	"ok"
//	@Failure	503	{object}	errorBody
//	@Router		/readyz [get]
func (s *Server) handleReadyz(c *gin.Context) {
	probeCtx, cancel := context.WithTimeoutCause(c.Request.Context(), s.cfg.APITimeout, errReadyzTimeout)
	defer cancel()

	err := s.probeUpstream(probeCtx)
	if err != nil {
		// /readyz is unauthenticated; the raw upstream error embeds the internal
		// VictoriaMetrics URL/host/IP. Return a static message and keep the
		// detail server-side (the promql client already logs it at Error level).
		//
		// A routed deployment appends the BACKEND NAMES that did not answer —
		// operator-chosen labels from the operator's own routing file, never a
		// URL, host, or IP. With several upstreams, "upstream probe failed" is
		// not actionable on its own.
		message := "upstream probe failed"
		if probeErr, ok := errors.AsType[*promql.ProbeError](err); ok && len(probeErr.Failed) > 0 {
			message += ": " + strings.Join(probeErr.Failed, ", ")
		}
		s.logger.WarnContext(c.Request.Context(), "readyz upstream probe failed",
			"err", err, "cause", context.Cause(probeCtx))
		writeError(c, http.StatusServiceUnavailable, "upstream_unreachable", message)
		return
	}
	c.String(http.StatusOK, "ok")
}

// probeUpstream asks every configured backend whether it is reachable.
//
// A *promql.Router satisfies promql.Prober and probes all of its backends
// concurrently within the one budget, reporting every one that did not answer.
// Anything else — a plain *promql.Client, a mock — falls back to the single
// up{} query this endpoint has always issued.
func (s *Server) probeUpstream(ctx context.Context) error {
	if prober, ok := s.prom.(promql.Prober); ok {
		return prober.ProbeAll(ctx, s.clk.Now().UTC())
	}
	_, err := s.prom.Instant(ctx, string(promql.QUpProbe),
		promql.Render(promql.QUpProbe, 0, promql.LabelKeys{}, promql.Selector{}), s.clk.Now().UTC())
	return err
}

// ----- request parsing ------------------------------------------------------

type graphRequest struct {
	start time.Time
	end   time.Time
	scope graph.Scope
	sel   promql.Selector
}

// parseGraphRequest delegates parsing to the shared kubegraph.ParseValues (the
// single source of truth for the /v1/graph request contract, also used by
// Engine.BuildFromValues), then maps a *kubegraph.ParseError to the existing
// HTTP 400 response with its stable reason code.
func (s *Server) parseGraphRequest(c *gin.Context) (graphRequest, error) {
	req, err := kubegraph.ParseValues(c.Request.URL.Query())
	if err != nil {
		writeParseError(c, err)
		return graphRequest{}, err
	}
	return graphRequest{
		start: req.Start,
		end:   req.End,
		scope: req.Scope,
		sel:   req.Selector,
	}, nil
}

// writeParseError maps a request-parse failure to the 400 response: a
// *kubegraph.ParseError carries its stable reason code, anything else is the
// generic invalid_request.
func writeParseError(c *gin.Context, err error) {
	if pe, ok := errors.AsType[*kubegraph.ParseError](err); ok {
		writeError(c, http.StatusBadRequest, pe.Reason, pe.Message)
		return
	}
	writeError(c, http.StatusBadRequest, "invalid_request", err.Error())
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
