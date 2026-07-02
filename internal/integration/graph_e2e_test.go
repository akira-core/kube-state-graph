package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/clock"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// fixedNow is the absolute timestamp anchor every fixture and query uses.
// Per D20 / D5, integration tests MUST NOT use time.Now()-relative values for
// time-bucket alignment.
var fixedNow = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// GraphSuite covers the API contract end-to-end against a real VM container.
type GraphSuite struct {
	VMSuite
}

func TestGraphSuite(t *testing.T) {
	suite.Run(t, new(GraphSuite))
}

// SetupTest seeds the standard multi-cluster fixture set before each test
// using the per-test name as a discriminator label.
//
// Service-graph series are ingested as TWO monotonic counter samples (t0 and
// t1 = t0 + 60s) so that `rate(traces_service_graph_request_total[w])` over
// the test window can recover a non-zero per-second rate. Without two samples
// the rate() result is empty and every pod-call edge silently disappears.
func (s *GraphSuite) SetupTest() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000 // ms timestamps for /api/v1/import/prometheus
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	const counterStep = 60.0 // seconds between t0 and t1 (matches rate denominator)

	// Per-series rates (req/s).
	const (
		rateCheckoutCart       = 5.0
		rateCheckoutPayments   = 2.0
		rateExternalToCheckout = 1.0
	)
	v := func(rate float64) float64 { return rate * counterStep }

	exposition := fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="checkout",uid="alpha-1",node="worker-0",test=%q} 1 %d
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="cart",uid="alpha-2",node="worker-0",test=%q} 1 %d
kube_pod_info{cluster="cluster-beta",namespace="billing",pod="payments",uid="beta-1",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="cluster-alpha",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="cluster-beta",node="worker-0",test=%q} 1 %d
kube_node_status_addresses{cluster="cluster-alpha",node="worker-0",type="ExternalIP",address="203.0.113.10",test=%q} 1 %d
kube_node_status_addresses{cluster="cluster-alpha",node="worker-0",type="InternalIP",address="10.0.0.7",test=%q} 1 %d
kube_node_status_addresses{cluster="cluster-beta",node="worker-0",type="InternalIP",address="10.0.1.7",test=%q} 1 %d
traces_service_graph_request_total{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="alpha-2",client_k8s_namespace_name="shop",server_k8s_namespace_name="shop",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="alpha-2",client_k8s_namespace_name="shop",server_k8s_namespace_name="shop",connection_type="virtual_node",test=%q} %g %d
traces_service_graph_request_total{client="checkout",server="payments",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="beta-1",client_k8s_namespace_name="shop",server_k8s_namespace_name="billing",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="checkout",server="payments",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="beta-1",client_k8s_namespace_name="shop",server_k8s_namespace_name="billing",connection_type="virtual_node",test=%q} %g %d
traces_service_graph_request_total{client="https://payments.partner.example/api",server="checkout",cluster="cluster-alpha",client_k8s_pod_uid="",server_k8s_pod_uid="alpha-1",client_k8s_namespace_name="",server_k8s_namespace_name="shop",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="https://payments.partner.example/api",server="checkout",cluster="cluster-alpha",client_k8s_pod_uid="",server_k8s_pod_uid="alpha-1",client_k8s_namespace_name="",server_k8s_namespace_name="shop",connection_type="virtual_node",test=%q} %g %d
`,
		disc, t1, disc, t1, disc, t1,
		disc, t1, disc, t1, disc, t1,
		disc, t1, disc, t1,
		disc, t0, disc, v(rateCheckoutCart), t1,
		disc, t0, disc, v(rateCheckoutPayments), t1,
		disc, t0, disc, v(rateExternalToCheckout), t1,
	)
	s.IngestExpFmt(exposition)
	s.Require().True(s.WaitForSeries(`kube_pod_info{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_pod_info")
	s.Require().True(
		s.WaitForSeries(`rate(traces_service_graph_request_total{test=`+strconv.Quote(disc)+`}[5m]) > 0`, fixedNow, 30*time.Second),
		"VM did not observe non-zero service-graph rate")
}

func (s *GraphSuite) TestSingleClusterGraph() {
	srv := s.StartAPIServer(func(cfg *config.Config) {

	})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var body map[string]any
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	elements, _ := body["elements"].(map[string]any)
	nodes, _ := elements["nodes"].([]any)
	edges, _ := elements["edges"].([]any)
	s.NotEmpty(nodes, "expected at least one node")
	s.NotEmpty(edges, "expected at least one edge")

	// Regression guard for fixture/rate() drift: at least one pod-calls-pod
	// edge MUST survive. If service-graph fixtures lose counter movement, this
	// drops to zero before any other assertion notices.
	var podCalls int
	for _, raw := range edges {
		e, _ := raw.(map[string]any)
		data, _ := e["data"].(map[string]any)
		if data["type"] == "pod-calls-pod" {
			podCalls++
		}
	}
	s.GreaterOrEqual(podCalls, 1, "service-graph rate() returned no pod-call edges; fixture counter movement likely broken")
}

func (s *GraphSuite) TestCrossClusterEdgePresent() {
	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, func(q url.Values) { q.Set("edge_type", "pod-calls-pod") }))
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	// Cross-cluster status is recovered via the topology pod-UID index: the
	// metric stamped `cluster=cluster-alpha`, but `server_k8s_pod_uid=beta-1`
	// resolves to a topology pod whose own cluster is `cluster-beta`. The
	// edge should target that resolved pod and carry `cluster=cluster-alpha`
	// (the trace source / client side).
	bodyStr := string(body)
	s.Contains(bodyStr, `"target":"cluster-beta/beta-1"`, "cross-cluster target resolved via UID index")
	s.Contains(bodyStr, `"source":"cluster-alpha/alpha-1"`)
	s.Contains(bodyStr, `"cluster":"cluster-alpha"`)
	s.NotContains(bodyStr, `"client_cluster"`, "v1 edges must not carry client_cluster")
	s.NotContains(bodyStr, `"server_cluster"`, "v1 edges must not carry server_cluster")
}

// TestNodeIPAddressFallback — K8s node ipaddress resolution against real
// ingested kube_node_status_addresses series: cluster-alpha/worker-0 has both
// ExternalIP and InternalIP rows (ExternalIP wins); cluster-beta/worker-0 has
// only an InternalIP row (fallback surfaces it). IPs never appear in labels.
func (s *GraphSuite) TestNodeIPAddressFallback() {
	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var body struct {
		Elements struct {
			Nodes []struct {
				Data struct {
					ID        string            `json:"id"`
					Type      string            `json:"type"`
					IPAddress []string          `json:"ipaddress"`
					Labels    map[string]string `json:"labels"`
				} `json:"data"`
			} `json:"nodes"`
		} `json:"elements"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))

	ips := map[string][]string{}
	for _, n := range body.Elements.Nodes {
		if n.Data.Type != "node" {
			continue
		}
		ips[n.Data.ID] = n.Data.IPAddress
		s.NotContains(n.Data.Labels, "external_ip", "labels.external_ip must not be emitted")
		s.NotContains(n.Data.Labels, "internal_ip", "labels.internal_ip must not be emitted")
	}
	s.Equal([]string{"203.0.113.10"}, ips["cluster-alpha/worker-0"],
		"ExternalIP wins when both address types present")
	s.Equal([]string{"10.0.1.7"}, ips["cluster-beta/worker-0"],
		"InternalIP fallback when no ExternalIP row exists")
}

// TestNodeReadyStatusAttribute — ingest kube_node_status_condition for dedicated
// nodes: one Ready (status=true active), one Unknown (status=unknown active —
// kubelet lost contact), and one with NO Ready series; assert /v1/graph sets each
// node's typed data.ready_status, that Unknown is DISTINCT from omitted, and that
// the status never leaks into labels. Dedicated `ready-probe-*` node names avoid
// the shared-VM series leak into other suite tests (which assert on worker-0).
func (s *GraphSuite) TestNodeReadyStatusAttribute() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	// Real KSM emits all three status rows per condition with value 1 for the
	// active one; replicate that so the resolver's active-row pick is exercised.
	s.IngestExpFmt(fmt.Sprintf(`# HELP kube_node_info dummy
kube_node_info{cluster="cluster-alpha",node="ready-probe-ready",test=%q} 1 %d
kube_node_info{cluster="cluster-alpha",node="ready-probe-unknown",test=%q} 1 %d
kube_node_info{cluster="cluster-alpha",node="ready-probe-nocond",test=%q} 1 %d
kube_node_status_condition{cluster="cluster-alpha",node="ready-probe-ready",condition="Ready",status="true",test=%q} 1 %d
kube_node_status_condition{cluster="cluster-alpha",node="ready-probe-ready",condition="Ready",status="false",test=%q} 0 %d
kube_node_status_condition{cluster="cluster-alpha",node="ready-probe-ready",condition="Ready",status="unknown",test=%q} 0 %d
kube_node_status_condition{cluster="cluster-alpha",node="ready-probe-unknown",condition="Ready",status="true",test=%q} 0 %d
kube_node_status_condition{cluster="cluster-alpha",node="ready-probe-unknown",condition="Ready",status="false",test=%q} 0 %d
kube_node_status_condition{cluster="cluster-alpha",node="ready-probe-unknown",condition="Ready",status="unknown",test=%q} 1 %d
`,
		disc, t1, disc, t1, disc, t1,
		disc, t1, disc, t1, disc, t1,
		disc, t1, disc, t1, disc, t1,
	))
	s.Require().True(s.WaitForSeries(`kube_node_status_condition{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_node_status_condition")

	// These probe nodes host no pod, so the default view hides them (generalised
	// D6: a no-filter response only carries nodes referenced by an in-scope pod).
	// `ready_status` still surfaces — fetch each node directly via ?name= (the
	// name-filter exception admits a podless infra node by name).
	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, func(q url.Values) {
		q.Add("name", "ready-probe-ready")
		q.Add("name", "ready-probe-unknown")
		q.Add("name", "ready-probe-nocond")
	}))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var body struct {
		Elements struct {
			Nodes []struct {
				Data struct {
					ID          string            `json:"id"`
					Type        string            `json:"type"`
					ReadyStatus string            `json:"ready_status"`
					Labels      map[string]string `json:"labels"`
				} `json:"data"`
			} `json:"nodes"`
		} `json:"elements"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))

	status := map[string]string{}
	present := map[string]bool{}
	for _, n := range body.Elements.Nodes {
		if n.Data.Type != "node" {
			continue
		}
		present[n.Data.ID] = true
		status[n.Data.ID] = n.Data.ReadyStatus
		s.NotContains(n.Data.Labels, "ready_status", "ready_status must not be a label")
		s.NotContains(n.Data.Labels, "condition", "condition must not be a label")
		s.NotContains(n.Data.Labels, "status", "status must not be a label")
	}

	s.Equal(graph.ReadyStatusReady, status["cluster-alpha/ready-probe-ready"],
		"active status=true → Ready")
	s.Equal(graph.ReadyStatusUnknown, status["cluster-alpha/ready-probe-unknown"],
		"active status=unknown → Unknown (kubelet lost contact)")
	s.Require().True(present["cluster-alpha/ready-probe-nocond"],
		"node with no Ready series is still present")
	s.Empty(status["cluster-alpha/ready-probe-nocond"],
		"no Ready series → ready_status omitted, DISTINCT from Unknown")
}

func (s *GraphSuite) TestNameFilter_PodAnchor() {
	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, func(q url.Values) { q.Set("name", "checkout") }))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	s.Contains(bodyStr, `"id":"cluster-alpha/alpha-1"`, "checkout pod present")
	// Cross-cluster partner pod IS re-added by the unified edge-endpoint
	// rule on pod-calls-pod, so the cross-cluster edge can render with
	// both endpoints visible.
	s.Contains(bodyStr, `"id":"cluster-beta/beta-1"`,
		"cross-cluster partner pod re-added as edge endpoint of named anchor")
}

func (s *GraphSuite) TestNameFilter_UnknownReturnsEmpty() {
	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, func(q url.Values) { q.Set("name", "does-not-exist") }))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	s.Contains(bodyStr, `"nodes":[]`)
	s.Contains(bodyStr, `"edges":[]`)
}

// TestPodOwnerAttributeSkipReplicaSet — D34. Ingest kube_pod_owner for the
// `checkout` pod pointing at a ReplicaSet, plus kube_replicaset_owner mapping
// that ReplicaSet to a Deployment; assert /v1/graph sets the pod node's typed
// data.owner = {kind:"Deployment", name:<deployment>} (the ReplicaSet is
// skipped), while the `cart` pod (no owner series) carries no data.owner. The
// owner must never appear inside labels.
func (s *GraphSuite) TestPodOwnerAttributeSkipReplicaSet() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	s.IngestExpFmt(fmt.Sprintf(`# HELP kube_pod_owner dummy
kube_pod_owner{cluster="cluster-alpha",namespace="shop",pod="checkout",owner_kind="ReplicaSet",owner_name="checkout-7f9c",owner_is_controller="true",test=%q} 1 %d
kube_replicaset_owner{cluster="cluster-alpha",namespace="shop",replicaset="checkout-7f9c",owner_kind="Deployment",owner_name="checkout-deploy",test=%q} 1 %d
`, disc, t1, disc, t1))
	s.Require().True(s.WaitForSeries(`kube_pod_owner{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_pod_owner")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	type podData struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Owner *struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"owner"`
		Labels map[string]string `json:"labels"`
	}
	var body struct {
		Elements struct {
			Nodes []struct {
				Data podData `json:"data"`
			} `json:"nodes"`
		} `json:"elements"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))

	podByName := func(name string) (podData, bool) {
		for _, n := range body.Elements.Nodes {
			if n.Data.Type == "pod" && n.Data.Name == name {
				return n.Data, true
			}
		}
		return podData{}, false
	}

	checkout, ok := podByName("checkout")
	s.Require().True(ok, "checkout pod node must be present")
	s.Require().NotNil(checkout.Owner, "checkout pod must carry data.owner")
	s.Equal("Deployment", checkout.Owner.Kind, "ReplicaSet must be skipped to its Deployment")
	s.Equal("checkout-deploy", checkout.Owner.Name)
	_, ownerInLabels := checkout.Labels["owner_kind"]
	s.False(ownerInLabels, "owner must NOT appear inside labels")

	cart, ok := podByName("cart")
	s.Require().True(ok, "cart pod node must be present")
	s.Nil(cart.Owner, "pod with no owner series must omit data.owner")
}

// TestPodApplicationAndContainersAttributes — ingest kube_pod_container_info
// (two containers) and a kube_pod_owner carrying an argocd_tracking_id for a
// dedicated pod; assert /v1/graph sets the pod node's typed data.application
// (segment before the first ":") and ordered data.containers, neither leaking
// into labels, while a sibling pod with no such series omits both. The pods
// live in a dedicated namespace ("appcat") so the owner/container series cannot
// collide with the shared `checkout`/`cart` fixtures other tests assert on.
//
// COVERAGE NOTE: this exercises one image per container, so it does NOT cover the
// tlast_over_time argmax-by-recency "latest image wins" path — at the suite's
// fixedNow (~6 weeks before the container's real clock) VM returns only one
// image-variant per container regardless of rollup, so the multi-image case is
// unverifiable here (see design.md D-A4). The recency/argmax logic is covered by
// TestParseTopology_PodContainersLatestImageWins / _TieIsDeterministic (unit,
// deterministic vectors); that tlast_over_time stamps s.Value with the
// last-sample timestamp was verified by manual probe against VM v1.107.0.
func (s *GraphSuite) TestPodApplicationAndContainersAttributes() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	// A pod-calls-pod edge ksg-enriched→ksg-bare keeps both pods connected (so
	// they survive the default prune) without adding owner/container/application
	// data — ksg-bare must still report no application and no containers.
	s.IngestExpFmt(fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="cluster-alpha",namespace="appcat",pod="ksg-enriched",uid="alpha-app-1",node="worker-0",test=%q} 1 %d
kube_pod_info{cluster="cluster-alpha",namespace="appcat",pod="ksg-bare",uid="alpha-app-2",node="worker-0",test=%q} 1 %d
kube_pod_owner{cluster="cluster-alpha",namespace="appcat",pod="ksg-enriched",owner_kind="DaemonSet",owner_name="ksg-ds",owner_is_controller="true",argocd_tracking_id="storefront:apps/Deployment:appcat/ksg",test=%q} 1 %d
kube_pod_container_info{cluster="cluster-alpha",namespace="appcat",pod="ksg-enriched",container="app",image="reg/ksg:1.4",test=%q} 1 %d
kube_pod_container_info{cluster="cluster-alpha",namespace="appcat",pod="ksg-enriched",container="istio-proxy",image="reg/proxy:0.9",test=%q} 1 %d
traces_service_graph_request_total{client="ksg-enriched",server="ksg-bare",cluster="cluster-alpha",client_k8s_pod_uid="alpha-app-1",server_k8s_pod_uid="alpha-app-2",client_k8s_namespace_name="appcat",server_k8s_namespace_name="appcat",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="ksg-enriched",server="ksg-bare",cluster="cluster-alpha",client_k8s_pod_uid="alpha-app-1",server_k8s_pod_uid="alpha-app-2",client_k8s_namespace_name="appcat",server_k8s_namespace_name="appcat",connection_type="virtual_node",test=%q} 60 %d
`, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t0, disc, t1))
	s.Require().True(s.WaitForSeries(`kube_pod_container_info{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_pod_container_info")
	s.Require().True(s.WaitForSeries(`rate(traces_service_graph_request_total{client="ksg-enriched",test=`+strconv.Quote(disc)+`}[5m]) > 0`, fixedNow, 30*time.Second),
		"VM did not observe non-zero ksg-enriched→ksg-bare service-graph rate")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	type container struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	}
	type podData struct {
		Name        string            `json:"name"`
		Type        string            `json:"type"`
		Application string            `json:"application"`
		Containers  []container       `json:"containers"`
		Labels      map[string]string `json:"labels"`
	}
	var body struct {
		Elements struct {
			Nodes []struct {
				Data podData `json:"data"`
			} `json:"nodes"`
		} `json:"elements"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))

	podByName := func(name string) (podData, bool) {
		for _, n := range body.Elements.Nodes {
			if n.Data.Type == "pod" && n.Data.Name == name {
				return n.Data, true
			}
		}
		return podData{}, false
	}

	enriched, ok := podByName("ksg-enriched")
	s.Require().True(ok, "enriched pod node must be present")
	s.Equal("storefront", enriched.Application, "data.application = segment before the first ':'")
	s.Equal([]container{
		{Name: "app", Image: "reg/ksg:1.4"},
		{Name: "istio-proxy", Image: "reg/proxy:0.9"},
	}, enriched.Containers, "data.containers ordered by (name, image)")
	_, appInLabels := enriched.Labels["application"]
	_, trackInLabels := enriched.Labels["argocd_tracking_id"]
	s.False(appInLabels || trackInLabels, "application must NOT appear inside labels")

	bare, ok := podByName("ksg-bare")
	s.Require().True(ok, "bare pod node must be present")
	s.Empty(bare.Application, "pod with no argocd label omits data.application")
	s.Nil(bare.Containers, "pod with no container series omits data.containers")
}

// TestServiceAndPVCApplicationNesting — ingest kube_persistentvolumeclaim_annotations
// (a PVC) and kube_service_annotations (a connection-string-resolved service),
// each carrying annotation_argocd_argoproj_io_tracking_id, and assert /v1/graph
// (1) sets the typed data.application on the service AND PVC nodes (segment before
// the first ":", never in labels) and (2) nests both under the synthesised
// application compound group cluster > namespace > application > {service, pvc}.
// The objects live in a dedicated namespace ("argons") so they cannot collide
// with the shared shop fixtures. The service materialises via the D29
// connection-string path (checkout pod → https://argo-svc.argons.svc...), whose
// anchor cluster (cluster-alpha, recovered from the client pod UID) holds the
// same-named service, so the service node is created in cluster-alpha.
func (s *GraphSuite) TestServiceAndPVCApplicationNesting() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	// argo-pod is a real, connectivity-connected pod (it calls argo-svc via the
	// connection string), so under the default prune it and the argo-data PVC it
	// mounts survive. Its kube_pod_info also lets the volume binding resolve the
	// pod name → uid, wiring the pod-mounts-pvc edge that drives PVC retention.
	s.IngestExpFmt(fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="cluster-alpha",namespace="argons",pod="argo-pod",uid="argo-1",node="worker-0",test=%q} 1 %d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="cluster-alpha",namespace="argons",pod="argo-pod",claim_name="argo-data",test=%q} 1 %d
kube_persistentvolumeclaim_annotations{cluster="cluster-alpha",namespace="argons",persistentvolumeclaim="argo-data",annotation_argocd_argoproj_io_tracking_id="argowf:apps/StatefulSet:argons/argowf",test=%q} 1 %d
kube_service_info{cluster="cluster-alpha",namespace="argons",service="argo-svc",cluster_ip="10.96.0.50",test=%q} 1 %d
kube_service_annotations{cluster="cluster-alpha",namespace="argons",service="argo-svc",annotation_argocd_argoproj_io_tracking_id="argowf:apps/StatefulSet:argons/argowf",test=%q} 1 %d
traces_service_graph_request_total{client="argo-pod",server="https://argo-svc.argons.svc.cluster.local/api",cluster="cluster-alpha",client_k8s_pod_uid="argo-1",server_k8s_pod_uid="",client_k8s_namespace_name="argons",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="argo-pod",server="https://argo-svc.argons.svc.cluster.local/api",cluster="cluster-alpha",client_k8s_pod_uid="argo-1",server_k8s_pod_uid="",client_k8s_namespace_name="argons",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 120 %d
`, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t0, disc, t1))
	s.Require().True(s.WaitForSeries(`kube_service_annotations{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_service_annotations")
	s.Require().True(s.WaitForSeries(`kube_persistentvolumeclaim_annotations{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_persistentvolumeclaim_annotations")
	s.Require().True(s.WaitForSeries(`rate(traces_service_graph_request_total{server=~"https://argo-svc.*",test=`+strconv.Quote(disc)+`}[5m]) > 0`, fixedNow, 30*time.Second),
		"VM did not observe the argo-svc connection-string trace")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	byID := map[string]cytoscape.NodeData{}
	for _, n := range body.Elements.Nodes {
		byID[n.Data.ID] = n.Data
	}

	const appGroup = "cluster-alpha/namespace/argons/application/argowf"

	pvc, ok := byID["cluster-alpha/argons/argo-data"]
	s.Require().True(ok, "PVC node must be present")
	s.Equal("argowf", pvc.Application, "PVC data.application = segment before the first ':'")
	s.Equal(appGroup, pvc.Parent, "PVC nests under its application group")
	_, pvcAppLabel := pvc.Labels["application"]
	_, pvcTrackLabel := pvc.Labels["annotation_argocd_argoproj_io_tracking_id"]
	s.False(pvcAppLabel || pvcTrackLabel, "PVC application must NOT appear inside labels")

	svc, ok := byID["cluster-alpha/argons/argo-svc"]
	s.Require().True(ok, "service node must be present (resolved from the connection string)")
	s.Equal("service", svc.Type)
	s.Equal("argowf", svc.Application, "service data.application = segment before the first ':'")
	s.Equal(appGroup, svc.Parent, "service nests under its application group")

	grp, ok := byID[appGroup]
	s.Require().True(ok, "the application group node must be synthesised")
	s.Equal("application", grp.Type)
	s.Equal("cluster-alpha/namespace/argons", grp.Parent, "application group nests under its namespace group")
}

// TestPVCInheritsApplicationFromMountingPod — D13: a PVC with NO tracking-id
// annotation of its own inherits the ArgoCD Application of the pod that mounts
// it (via the pod-mounts-pvc binding), surfacing data.application and nesting
// under the inherited application group — indistinguishable from an
// annotation-sourced value. A PVC carrying its OWN annotation keeps it (own
// wins over inheritance). Objects live in a dedicated namespace ("argoinh") so
// they cannot collide with the shared fixtures.
func (s *GraphSuite) TestPVCInheritsApplicationFromMountingPod() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	// inh-pod and own-pod are wired together by a pod-calls-pod edge so both
	// survive the default connectivity prune (and with them the PVCs they
	// mount); the two monotonic samples let rate() recover a non-zero rate.
	s.IngestExpFmt(fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="cluster-alpha",namespace="argoinh",pod="inh-pod",uid="inh-1",node="worker-0",test=%q} 1 %d
kube_pod_info{cluster="cluster-alpha",namespace="argoinh",pod="own-pod",uid="own-1",node="worker-0",test=%q} 1 %d
kube_pod_owner{cluster="cluster-alpha",namespace="argoinh",pod="inh-pod",owner_kind="Deployment",owner_name="inh",owner_is_controller="true",argocd_tracking_id="checkout:apps/Deployment:argoinh/checkout",test=%q} 1 %d
kube_pod_owner{cluster="cluster-alpha",namespace="argoinh",pod="own-pod",owner_kind="Deployment",owner_name="own",owner_is_controller="true",argocd_tracking_id="web:apps/Deployment:argoinh/web",test=%q} 1 %d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="cluster-alpha",namespace="argoinh",pod="inh-pod",claim_name="inh-data",test=%q} 1 %d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="cluster-alpha",namespace="argoinh",pod="own-pod",claim_name="own-data",test=%q} 1 %d
kube_persistentvolumeclaim_annotations{cluster="cluster-alpha",namespace="argoinh",persistentvolumeclaim="own-data",annotation_argocd_argoproj_io_tracking_id="mongo:apps/StatefulSet:argoinh/mongo",test=%q} 1 %d
traces_service_graph_request_total{client="inh-pod",server="own-pod",cluster="cluster-alpha",client_k8s_pod_uid="inh-1",server_k8s_pod_uid="own-1",client_k8s_namespace_name="argoinh",server_k8s_namespace_name="argoinh",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="inh-pod",server="own-pod",cluster="cluster-alpha",client_k8s_pod_uid="inh-1",server_k8s_pod_uid="own-1",client_k8s_namespace_name="argoinh",server_k8s_namespace_name="argoinh",connection_type="virtual_node",test=%q} 60 %d
`, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t0, disc, t1))
	s.Require().True(s.WaitForSeries(`kube_pod_spec_volumes_persistentvolumeclaims_info{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_pod_spec_volumes_persistentvolumeclaims_info")
	s.Require().True(s.WaitForSeries(`kube_pod_owner{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_pod_owner")
	s.Require().True(s.WaitForSeries(`rate(traces_service_graph_request_total{client="inh-pod",test=`+strconv.Quote(disc)+`}[5m]) > 0`, fixedNow, 30*time.Second),
		"VM did not observe non-zero inh-pod→own-pod service-graph rate")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	byID := map[string]cytoscape.NodeData{}
	for _, n := range body.Elements.Nodes {
		byID[n.Data.ID] = n.Data
	}

	// Inherited: app-less PVC adopts its mounting pod's Application "checkout".
	inh, ok := byID["cluster-alpha/argoinh/inh-data"]
	s.Require().True(ok, "inherited PVC node must be present")
	s.Equal("checkout", inh.Application, "PVC inherits its mounting pod's Application")
	s.Equal("cluster-alpha/namespace/argoinh/application/checkout", inh.Parent,
		"inherited PVC nests under the mounting pod's application group")
	_, inhAppLabel := inh.Labels["application"]
	s.False(inhAppLabel, "inherited application must NOT appear inside labels")

	// Own-wins: PVC with its own annotation keeps "mongo", not the pod's "web".
	own, ok := byID["cluster-alpha/argoinh/own-data"]
	s.Require().True(ok, "own-annotation PVC node must be present")
	s.Equal("mongo", own.Application, "PVC's own annotation wins over inheritance")
	s.Equal("cluster-alpha/namespace/argoinh/application/mongo", own.Parent,
		"own-annotation PVC nests under its own application group")
}

func (s *GraphSuite) TestConnStringUnresolvableProducesExternalNode() {
	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, func(q url.Values) { q.Set("edge_type", "pod-calls-pod") }))
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	s.Contains(string(body), `"type":"external"`)
	s.Contains(string(body), `"name":"https://payments.partner.example/api"`)
}

// TestMissingUIDNonURLClientProducesExternalNode exercises the D27 missing-UID
// fallback end-to-end against a real VM (tasks §31.E.4): a service-graph series
// whose client_k8s_pod_uid is EMPTY and whose client label is a plain non-URL
// human name ("stray-caller", no "://") promotes that endpoint to an
// external/<label> node (rather than dropping the edge), and the resulting
// pod-calls-pod edge to the resolved server pod omits labels.cluster because the
// client side is not a pod. This is the non-URL counterpart to the "://"
// unresolvable case above — distinct code path (D27, not D29).
func (s *GraphSuite) TestMissingUIDNonURLClientProducesExternalNode() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	// server resolves to the standard checkout pod (uid alpha-1); client UID is
	// empty with a plain non-URL label. Two counter samples so rate() > 0.
	extra := fmt.Sprintf(`# HELP traces_service_graph_request_total dummy
traces_service_graph_request_total{client="stray-caller",server="checkout",cluster="cluster-alpha",client_k8s_pod_uid="",server_k8s_pod_uid="alpha-1",client_k8s_namespace_name="",server_k8s_namespace_name="shop",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="stray-caller",server="checkout",cluster="cluster-alpha",client_k8s_pod_uid="",server_k8s_pod_uid="alpha-1",client_k8s_namespace_name="",server_k8s_namespace_name="shop",connection_type="virtual_node",test=%q} 120 %d
`, disc, t0, disc, t1)
	s.IngestExpFmt(extra)
	s.Require().True(
		s.WaitForSeries(`rate(traces_service_graph_request_total{client="stray-caller",test=`+strconv.Quote(disc)+`}[5m]) > 0`, fixedNow, 30*time.Second),
		"VM did not observe the non-URL missing-UID series")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))

	// external/stray-caller node present with the human label as name, empty labels.
	var ext *cytoscape.NodeData
	for i := range body.Elements.Nodes {
		if body.Elements.Nodes[i].Data.ID == "external/stray-caller" {
			ext = &body.Elements.Nodes[i].Data
		}
	}
	s.Require().NotNil(ext, "missing-UID non-URL client must promote to external/stray-caller (edge not dropped)")
	s.Equal("external", ext.Type)
	s.Equal("stray-caller", ext.Name, "external node carries the verbatim human label as name")
	s.Empty(ext.Labels, "missing-UID external node carries empty labels")

	// pod-calls-pod edge external/stray-caller → cluster-alpha/alpha-1, no cluster label.
	var sawEdge bool
	for _, e := range body.Elements.Edges {
		if e.Data.Source == "external/stray-caller" && e.Data.Target == "cluster-alpha/alpha-1" {
			sawEdge = true
			s.Equal("pod-calls-pod", e.Data.Type)
			s.NotContains(e.Data.Labels, "cluster",
				"edge omits labels.cluster when the client side is external (D27/D9)")
		}
	}
	s.True(sawEdge, "expected pod-calls-pod edge from external/stray-caller to the resolved server pod")
}

// TestConnStringServiceResolvesToServiceNodeWithBackingPods exercises the full
// D29 connection-string pipeline end-to-end against a real VM: kube_service_info
// + kube_endpointslice_{labels,endpoints} are read into the topology indexes,
// a checkout→https://payments-svc.shop.svc.cluster.local/api call (empty server
// UID) resolves the server to a type="service" node, and a service-selects-pod
// edge fans out to the backing "cart" pod (uid alpha-2, from the standard
// fixture). These extra series are ingested on top of SetupTest's standard set
// under the per-test discriminator.
func (s *GraphSuite) TestConnStringServiceResolvesToServiceNodeWithBackingPods() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	extra := fmt.Sprintf(`# HELP kube_service_info dummy
kube_service_info{cluster="cluster-alpha",namespace="shop",service="payments-svc",cluster_ip="10.96.0.9",test=%q} 1 %d
kube_endpointslice_labels{cluster="cluster-alpha",namespace="shop",endpointslice="payments-svc-x1",label_kubernetes_io_service_name="payments-svc",test=%q} 1 %d
kube_endpointslice_endpoints{cluster="cluster-alpha",namespace="shop",endpointslice="payments-svc-x1",targetref_kind="Pod",targetref_name="cart",targetref_namespace="shop",hostname="cart",test=%q} 1 %d
traces_service_graph_request_total{client="checkout",server="https://payments-svc.shop.svc.cluster.local/api",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="checkout",server="https://payments-svc.shop.svc.cluster.local/api",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 120 %d
`, disc, t1, disc, t1, disc, t1, disc, t0, disc, t1)
	s.IngestExpFmt(extra)
	s.Require().True(s.WaitForSeries(`kube_service_info{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_service_info")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Service node materialised from the connection string.
	s.Contains(bodyStr, `"type":"service"`, "ClusterIP connection string must resolve to a service node")
	s.Contains(bodyStr, `"id":"cluster-alpha/shop/payments-svc"`)
	s.Contains(bodyStr, `"10.96.0.9"`, "service node carries cluster_ip on ipaddress")
	// pod-calls-service edge points the client pod at the service node.
	s.Contains(bodyStr, `"type":"pod-calls-service"`,
		"call edge to a resolved service node is typed pod-calls-service")
	s.Contains(bodyStr, `"target":"cluster-alpha/shop/payments-svc"`)
	// service-selects-pod edge fans out to the backing pod (cart = alpha-2).
	s.Contains(bodyStr, `"type":"service-selects-pod"`)
	s.Contains(bodyStr, `"target":"cluster-alpha/alpha-2"`,
		"service-selects-pod edge resolves the backing cart pod via endpointslice targetref")
}

// TestConnStringHeadlessResolvesToServiceNodeNotPod exercises the D29 unified
// resolution end-to-end against a real VM: a headless per-pod connection string
// (checkout→redis://redis-0.redis-svc.shop.svc.cluster.local:6379, empty server
// UID) drops the leading pod-hostname `redis-0` and resolves to the SERVICE node
// `cluster-alpha/shop/redis-svc` (NOT the specific pod), fanning out a
// service-selects-pod edge to the backing pod. The headless service carries
// cluster_ip="None", so the service node has no ipaddress.
func (s *GraphSuite) TestConnStringHeadlessResolvesToServiceNodeNotPod() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	extra := fmt.Sprintf(`# HELP kube_service_info dummy
kube_service_info{cluster="cluster-alpha",namespace="shop",service="redis-svc",cluster_ip="None",test=%q} 1 %d
kube_endpointslice_labels{cluster="cluster-alpha",namespace="shop",endpointslice="redis-svc-x1",label_kubernetes_io_service_name="redis-svc",test=%q} 1 %d
kube_endpointslice_endpoints{cluster="cluster-alpha",namespace="shop",endpointslice="redis-svc-x1",targetref_kind="Pod",targetref_name="cart",targetref_namespace="shop",test=%q} 1 %d
traces_service_graph_request_total{client="checkout",server="redis://redis-0.redis-svc.shop.svc.cluster.local:6379",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="checkout",server="redis://redis-0.redis-svc.shop.svc.cluster.local:6379",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 120 %d
`, disc, t1, disc, t1, disc, t1, disc, t0, disc, t1)
	s.IngestExpFmt(extra)
	s.Require().True(s.WaitForSeries(`kube_service_info{service="redis-svc",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested headless kube_service_info")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// The headless per-pod string resolves to the SERVICE node, not pod redis-0.
	s.Contains(bodyStr, `"id":"cluster-alpha/shop/redis-svc"`, "headless string must resolve to its service node")
	s.Contains(bodyStr, `"target":"cluster-alpha/shop/redis-svc"`,
		"pod-calls-service target is the service node (pod-hostname dropped), not a specific pod")
	// service-selects-pod fan-out reaches the backing pod (cart = alpha-2).
	s.Contains(bodyStr, `"type":"service-selects-pod"`)
	s.Contains(bodyStr, `"target":"cluster-alpha/alpha-2"`,
		"service-selects-pod edge resolves the backing pod via endpointslice targetref")
}

// TestConnStringSelfLoopUIDResolvesToServiceNode exercises design.md D33
// end-to-end against a real VM: a buggy exporter stamps the SAME pod UID on
// BOTH client_k8s_pod_uid and server_k8s_pod_uid for a "://" peer (the real
// remote lives only in the server label). Without the self-loop guard the
// server would collapse onto the caller's own pod (a self-loop pod-calls-pod)
// and no service node would materialise. The guard clears the bogus colliding
// UID on the "://" side so it falls through to D29 Stage 0, resolves to the
// service node, and fans out to the backing pod. A unique service name
// (selfloop-svc) proves the resolution came from THIS colliding-UID series.
func (s *GraphSuite) TestConnStringSelfLoopUIDResolvesToServiceNode() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	extra := fmt.Sprintf(`# HELP kube_service_info dummy
kube_service_info{cluster="cluster-alpha",namespace="shop",service="selfloop-svc",cluster_ip="10.96.0.77",test=%q} 1 %d
kube_endpointslice_labels{cluster="cluster-alpha",namespace="shop",endpointslice="selfloop-svc-x1",label_kubernetes_io_service_name="selfloop-svc",test=%q} 1 %d
kube_endpointslice_endpoints{cluster="cluster-alpha",namespace="shop",endpointslice="selfloop-svc-x1",targetref_kind="Pod",targetref_name="cart",targetref_namespace="shop",test=%q} 1 %d
traces_service_graph_request_total{client="checkout",server="https://selfloop-svc.shop.svc.cluster.local/api",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="alpha-1",client_k8s_namespace_name="shop",server_k8s_namespace_name="shop",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="checkout",server="https://selfloop-svc.shop.svc.cluster.local/api",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="alpha-1",client_k8s_namespace_name="shop",server_k8s_namespace_name="shop",connection_type="virtual_node",test=%q} 120 %d
`, disc, t1, disc, t1, disc, t1, disc, t0, disc, t1)
	s.IngestExpFmt(extra)
	// One poll gates the whole batch: IngestExpFmt POSTs all series (gauge +
	// both counter samples) in a single request, so once kube_service_info is
	// queryable the rate() series are too — matching the sibling conn-string
	// tests, which assert their pod-calls-service edges off this one wait.
	s.Require().True(s.WaitForSeries(`kube_service_info{service="selfloop-svc",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested selfloop kube_service_info")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Despite the colliding self-loop UID, the "://" server resolves to its
	// service node — proves the D33 guard cleared the bogus UID.
	s.Contains(bodyStr, `"id":"cluster-alpha/shop/selfloop-svc"`,
		"colliding self-loop UID must not block service resolution of the '://' side")
	s.Contains(bodyStr, `"target":"cluster-alpha/shop/selfloop-svc"`,
		"call edge targets the resolved service node, not the caller's own pod")
	s.Contains(bodyStr, `"type":"pod-calls-service"`)
	s.Contains(bodyStr, `"type":"service-selects-pod"`,
		"resolved service fans out to its backing pod")
}

// TestConnStringFamilyFanoutCrossCluster exercises the localised D29 model
// end-to-end against a real VM: the client pod lives in prod-1, which holds the
// addressed Service (the mesh precondition), and prod-2 — a same-family cluster
// (both normalise to "prod-0") — holds the same-named Service with its own
// backing pod. The "://" resolves to a SINGLE service node in the caller's own
// cluster (prod-1) with an intra-cluster pod-calls-service edge; its
// service-selects-pod fan-out unions the prod-0 family's backing pods, reaching
// BOTH prod-1's pod (intra-cluster) AND prod-2's pod (cross-cluster).
func (s *GraphSuite) TestConnStringFamilyFanoutCrossCluster() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	extra := fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="prod-1",namespace="shop",pod="fanout-client",uid="fam-1",node="worker-0",test=%q} 1 %d
kube_pod_info{cluster="prod-1",namespace="shop",pod="fanout-nats-1",uid="fam-n1",node="worker-0",test=%q} 1 %d
kube_pod_info{cluster="prod-2",namespace="shop",pod="fanout-nats-0",uid="fam-n2",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="prod-1",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="prod-2",node="worker-0",test=%q} 1 %d
kube_service_info{cluster="prod-1",namespace="shop",service="fanout-svc",cluster_ip="10.96.0.87",test=%q} 1 %d
kube_service_info{cluster="prod-2",namespace="shop",service="fanout-svc",cluster_ip="10.96.0.88",test=%q} 1 %d
kube_endpointslice_labels{cluster="prod-1",namespace="shop",endpointslice="fanout-svc-p1",label_kubernetes_io_service_name="fanout-svc",test=%q} 1 %d
kube_endpointslice_endpoints{cluster="prod-1",namespace="shop",endpointslice="fanout-svc-p1",targetref_kind="Pod",targetref_name="fanout-nats-1",targetref_namespace="shop",test=%q} 1 %d
kube_endpointslice_labels{cluster="prod-2",namespace="shop",endpointslice="fanout-svc-x1",label_kubernetes_io_service_name="fanout-svc",test=%q} 1 %d
kube_endpointslice_endpoints{cluster="prod-2",namespace="shop",endpointslice="fanout-svc-x1",targetref_kind="Pod",targetref_name="fanout-nats-0",targetref_namespace="shop",test=%q} 1 %d
traces_service_graph_request_total{client="fanout-client",server="nats://fanout-svc.shop.svc:4222",cluster="prod-1",client_k8s_pod_uid="fam-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="fanout-client",server="nats://fanout-svc.shop.svc:4222",cluster="prod-1",client_k8s_pod_uid="fam-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 120 %d
`, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t0, disc, t1)
	s.IngestExpFmt(extra)
	s.Require().True(s.WaitForSeries(`count(kube_service_info{service="fanout-svc",test=`+strconv.Quote(disc)+`}) == 2`, fixedNow, 30*time.Second),
		"VM did not observe BOTH fanout kube_service_info rows")
	s.Require().True(s.WaitForSeries(`count(kube_endpointslice_labels{label_kubernetes_io_service_name="fanout-svc",test=`+strconv.Quote(disc)+`}) == 2`, fixedNow, 30*time.Second),
		"VM did not observe both fanout endpointslice label rows")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// EXACTLY ONE service node — in the caller's own cluster (prod-1).
	s.Contains(bodyStr, `"id":"prod-1/shop/fanout-svc"`,
		"the connection string resolves to a single service node in the caller's own cluster")
	s.NotContains(bodyStr, `"id":"prod-2/shop/fanout-svc"`,
		"no service node materialises in the family sibling — pod-calls-service is same-cluster only")
	// Intra-cluster pod-calls-service edge from the prod-1 pod to the prod-1 node.
	s.Contains(bodyStr, `"target":"prod-1/shop/fanout-svc"`,
		"pod-calls-service targets the caller's own cluster's service node")
	s.Contains(bodyStr, `"source":"prod-1/fam-1"`)
	s.NotContains(bodyStr, `external/nats://fanout-svc.shop.svc:4222`,
		"a family-resolved connection string must not also produce an external node")
	// Cross-cluster service-selects-pod fan-out: the prod-1 service node reaches
	// BOTH prod-1's backing pod (intra) and prod-2's backing pod (cross-cluster).
	s.Contains(bodyStr, `"target":"prod-1/fam-n1"`,
		"service-selects-pod fans out to the local backing pod")
	s.Contains(bodyStr, `"target":"prod-2/fam-n2"`,
		"service-selects-pod fans out CROSS-CLUSTER to the family sibling's backing pod")
}

// TestConnStringOutOfFamilyServiceFallsBackToExternal is the family-scoping
// negative: the addressed Service exists ONLY in staging-1, whose family key
// ("staging-0") differs from the trace source prod-1 ("prod-0"). The
// connection string must NOT resolve cross-family and falls back to the
// external/<label> node (D-C).
func (s *GraphSuite) TestConnStringOutOfFamilyServiceFallsBackToExternal() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	extra := fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="prod-1",namespace="shop",pod="outfam-client",uid="outfam-1",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="prod-1",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="staging-1",node="worker-0",test=%q} 1 %d
kube_service_info{cluster="staging-1",namespace="shop",service="outfam-svc",cluster_ip="10.96.0.99",test=%q} 1 %d
traces_service_graph_request_total{client="outfam-client",server="nats://outfam-svc.shop.svc:4222",cluster="prod-1",client_k8s_pod_uid="outfam-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="outfam-client",server="nats://outfam-svc.shop.svc:4222",cluster="prod-1",client_k8s_pod_uid="outfam-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 120 %d
`, disc, t1, disc, t1, disc, t1, disc, t1, disc, t0, disc, t1)
	s.IngestExpFmt(extra)
	s.Require().True(s.WaitForSeries(`kube_service_info{service="outfam-svc",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested out-of-family kube_service_info")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	s.NotContains(bodyStr, `staging-1/shop/outfam-svc`,
		"an out-of-family cluster's service must NOT be resolved for a prod-1 trace")
	s.Contains(bodyStr, `"id":"external/nats://outfam-svc.shop.svc:4222"`,
		"zero family matches must fall back to the external/<label> node")
}

// TestConnStringFamilyEndpointlessAnchorFansOutToSibling exercises the
// localised model when the caller's OWN cluster holds the Service object but
// has no local backing pods, while a same-family sibling does. The eps-svc
// Service exists in BOTH prod clusters; only prod-2 has a backing pod (the mesh
// routes the DNS name there). The anchor (prod-1) still materialises its single
// local service node (materialisation is gated on Service-object presence, not
// endpoints), and its service-selects-pod fan-out reaches ONLY prod-2's backing
// pod — a cross-cluster edge.
func (s *GraphSuite) TestConnStringFamilyEndpointlessAnchorFansOutToSibling() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	extra := fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="prod-1",namespace="shop",pod="eps-client",uid="eps-1",node="worker-0",test=%q} 1 %d
kube_pod_info{cluster="prod-2",namespace="shop",pod="eps-nats-0",uid="eps-n2",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="prod-1",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="prod-2",node="worker-0",test=%q} 1 %d
kube_service_info{cluster="prod-1",namespace="shop",service="eps-svc",cluster_ip="10.96.1.88",test=%q} 1 %d
kube_service_info{cluster="prod-2",namespace="shop",service="eps-svc",cluster_ip="10.96.2.88",test=%q} 1 %d
kube_endpointslice_labels{cluster="prod-2",namespace="shop",endpointslice="eps-svc-x1",label_kubernetes_io_service_name="eps-svc",test=%q} 1 %d
kube_endpointslice_endpoints{cluster="prod-2",namespace="shop",endpointslice="eps-svc-x1",targetref_kind="Pod",targetref_name="eps-nats-0",targetref_namespace="shop",test=%q} 1 %d
traces_service_graph_request_total{client="eps-client",server="nats://eps-svc.shop.svc:4222",cluster="prod-1",client_k8s_pod_uid="eps-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="eps-client",server="nats://eps-svc.shop.svc:4222",cluster="prod-1",client_k8s_pod_uid="eps-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 120 %d
`, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t0, disc, t1)
	s.IngestExpFmt(extra)
	// Gate the series the assertions depend on: BOTH kube_service_info rows (the
	// prod-1 anchor must provably hold the Service), the prod-2 endpointslice
	// join, and a non-zero trace rate.
	s.Require().True(s.WaitForSeries(`count(kube_service_info{service="eps-svc",test=`+strconv.Quote(disc)+`}) == 2`, fixedNow, 30*time.Second),
		"VM did not observe BOTH eps-svc kube_service_info rows")
	s.Require().True(s.WaitForSeries(`kube_endpointslice_labels{endpointslice="eps-svc-x1",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the prod-2 eps-svc endpointslice labels")
	s.Require().True(s.WaitForSeries(`rate(traces_service_graph_request_total{client="eps-client",test=`+strconv.Quote(disc)+`}[5m]) > 0`, fixedNow, 30*time.Second),
		"VM did not observe a non-zero eps trace rate")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// The anchor's local node materialises despite zero local endpoints.
	s.Contains(bodyStr, `"id":"prod-1/shop/eps-svc"`,
		"the anchor cluster's service node materialises on Service-object presence")
	s.NotContains(bodyStr, `"id":"prod-2/shop/eps-svc"`,
		"no service node materialises in the sibling — pod-calls-service is same-cluster only")
	s.Contains(bodyStr, `"target":"prod-1/shop/eps-svc"`)
	// The only backing pod is in prod-2 → a cross-cluster service-selects-pod edge.
	s.Contains(bodyStr, `"target":"prod-2/eps-n2"`,
		"service-selects-pod fans out cross-cluster to the only backing pod (in prod-2)")
	s.NotContains(bodyStr, `external/nats://eps-svc.shop.svc:4222`,
		"a resolved connection string must not produce an external node")
}

// TestConnStringUnanchorableFallsBackToExternal exercises the removal of the
// unknown-family fallback end-to-end against a real VM: the service-graph series
// carries NO cluster label (bucketed to "unknown") and its client side is a
// non-pod human label, so the anchor is the "unknown" pseudo-cluster. The
// addressed Service is held ONLY by prod-2 — not by "unknown" — and there is NO
// cross-family fallback, so the "://" server stays external (a caller with no
// own-cluster Service cannot resolve a local service node).
//
// The fixture uses a test-unique namespace (uffb-ns) on top of the unique
// service name to shrink shared-VM blast radius (every test's series persist;
// the API does not filter on the `test` discriminator).
func (s *GraphSuite) TestConnStringUnanchorableFallsBackToExternal() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	extra := fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="prod-2",namespace="uffb-ns",pod="uffb-nats-0",uid="uffb-n2",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="prod-2",node="worker-0",test=%q} 1 %d
kube_service_info{cluster="prod-2",namespace="uffb-ns",service="uffb-svc",cluster_ip="10.96.3.88",test=%q} 1 %d
kube_endpointslice_labels{cluster="prod-2",namespace="uffb-ns",endpointslice="uffb-svc-x1",label_kubernetes_io_service_name="uffb-svc",test=%q} 1 %d
kube_endpointslice_endpoints{cluster="prod-2",namespace="uffb-ns",endpointslice="uffb-svc-x1",targetref_kind="Pod",targetref_name="uffb-nats-0",targetref_namespace="uffb-ns",test=%q} 1 %d
traces_service_graph_request_total{client="uffb-admin",server="nats://uffb-svc.uffb-ns.svc:4222",client_k8s_pod_uid="",server_k8s_pod_uid="",client_k8s_namespace_name="",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="uffb-admin",server="nats://uffb-svc.uffb-ns.svc:4222",client_k8s_pod_uid="",server_k8s_pod_uid="",client_k8s_namespace_name="",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 120 %d
`, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t0, disc, t1)
	s.IngestExpFmt(extra)
	s.Require().True(s.WaitForSeries(`kube_service_info{service="uffb-svc",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested uffb kube_service_info")
	s.Require().True(s.WaitForSeries(`rate(traces_service_graph_request_total{client="uffb-admin",test=`+strconv.Quote(disc)+`}[5m]) > 0`, fixedNow, 30*time.Second),
		"VM did not observe a non-zero uffb trace rate")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// No cross-family fallback: the unanchorable caller cannot resolve a service
	// it does not hold in its own (pseudo-)cluster.
	s.NotContains(bodyStr, `prod-2/uffb-ns/uffb-svc`,
		"the removed unknown-family fallback must NOT resolve a foreign family's service")
	s.Contains(bodyStr, `"id":"external/nats://uffb-svc.uffb-ns.svc:4222"`,
		"an unanchorable connection string falls back to the external/<label> node")
	s.Contains(bodyStr, `"source":"external/uffb-admin"`,
		"the non-pod client side stays an external node")
}

// TestSentinelPeersExcludedAtQueryLayer exercises design.md D30 end-to-end
// against a real VM: the servicegraph connector's virtual peers are dropped by
// the anchored selector matchers (client!~"user|unknown",server!~"user") —
// client="user" is excluded upstream at the QUERY layer, so it never reaches
// the API. server="unknown" is narrower (resolve-unknown-server-peer-labels
// D1): the series itself now reaches Go, but this fixture carries neither
// client_net_peer_name nor client_server_address, so it still resolves to
// nothing (dropped in Go by resolveUnknownServerPeer) — the SAME observable
// outcome (no node, no edge) as the old blanket query-layer exclusion.
// Crucially the raw sentinel series ARE ingested into VM (asserted below), so a
// missing node proves the matchers/resolution excluded them — not that the
// data was absent. A connection string whose host merely CONTAINS "user"
// ("http://user/api") is NOT excluded (the match is fully anchored), proving
// the matcher is exact rather than substring.
func (s *GraphSuite) TestSentinelPeersExcludedAtQueryLayer() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	extra := fmt.Sprintf(`# HELP traces_service_graph_request_total dummy
traces_service_graph_request_total{client="user",server="checkout",cluster="cluster-alpha",client_k8s_pod_uid="",server_k8s_pod_uid="alpha-1",client_k8s_namespace_name="",server_k8s_namespace_name="shop",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="user",server="checkout",cluster="cluster-alpha",client_k8s_pod_uid="",server_k8s_pod_uid="alpha-1",client_k8s_namespace_name="",server_k8s_namespace_name="shop",connection_type="virtual_node",test=%q} 120 %d
traces_service_graph_request_total{client="checkout",server="unknown",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="checkout",server="unknown",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 120 %d
traces_service_graph_request_total{client="checkout",server="http://user/api",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="checkout",server="http://user/api",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",connection_type="virtual_node",test=%q} 120 %d
`, disc, t0, disc, t1, disc, t0, disc, t1, disc, t0, disc, t1)
	s.IngestExpFmt(extra)

	// Prove VM actually holds the sentinel series (so a later absent node is
	// the matcher's doing, not missing data) and that the substring series
	// produces a non-zero rate the API build will pick up.
	s.Require().True(
		s.WaitForSeries(`traces_service_graph_request_total{client="user",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested sentinel client=\"user\" series")
	s.Require().True(
		s.WaitForSeries(`rate(traces_service_graph_request_total{server="http://user/api",test=`+strconv.Quote(disc)+`}[5m]) > 0`, fixedNow, 30*time.Second),
		"VM did not observe non-zero rate for the http://user/api series")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Sentinel peers are excluded at the query layer: no node, no edge.
	s.NotContains(bodyStr, `external/user`, "client=\"user\" virtual peer must be excluded upstream")
	s.NotContains(bodyStr, `external/unknown`, "server=\"unknown\" virtual peer must be excluded upstream")
	s.NotContains(bodyStr, `"name":"user"`, "no node named user should appear")
	s.NotContains(bodyStr, `"name":"unknown"`, "no node named unknown should appear")

	// The anchored matcher does NOT catch a host that merely contains "user":
	// http://user/api survives and resolves to an external node.
	s.Contains(bodyStr, `"name":"http://user/api"`,
		"connection string containing (but not equal to) user must survive the anchored matcher")
}

// TestUnknownServerPeerLabelResolvesToServiceNode exercises the
// resolve-unknown-server-peer-labels change end-to-end against a real VM: the
// D30 server-side matcher no longer excludes server="unknown" at the query
// layer (only server!~"user"), so a series with server="unknown" now reaches
// Go. Because the client side resolves to a real topology pod (checkout =
// alpha-1) and the series carries client_net_peer_name, resolveUnknownServerPeer
// resolves it via the same D29 machinery as connection-string resolution — a
// pod-calls-service edge to the addressed service, with its usual
// service-selects-pod fan-out — instead of dropping the endpoint.
func (s *GraphSuite) TestUnknownServerPeerLabelResolvesToServiceNode() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	extra := fmt.Sprintf(`# HELP kube_service_info dummy
kube_service_info{cluster="cluster-alpha",namespace="shop",service="peer-svc",cluster_ip="10.96.0.44",test=%q} 1 %d
kube_endpointslice_labels{cluster="cluster-alpha",namespace="shop",endpointslice="peer-svc-x1",label_kubernetes_io_service_name="peer-svc",test=%q} 1 %d
kube_endpointslice_endpoints{cluster="cluster-alpha",namespace="shop",endpointslice="peer-svc-x1",targetref_kind="Pod",targetref_name="cart",targetref_namespace="shop",test=%q} 1 %d
traces_service_graph_request_total{client="checkout",server="unknown",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",client_net_peer_name="peer-svc.shop.svc.cluster.local",connection_type="virtual_node",test=%q} 0 %d
traces_service_graph_request_total{client="checkout",server="unknown",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="",client_k8s_namespace_name="shop",server_k8s_namespace_name="",client_net_peer_name="peer-svc.shop.svc.cluster.local",connection_type="virtual_node",test=%q} 120 %d
`, disc, t1, disc, t1, disc, t1, disc, t0, disc, t1)
	s.IngestExpFmt(extra)
	s.Require().True(
		s.WaitForSeries(`traces_service_graph_request_total{server="unknown",client_net_peer_name="peer-svc.shop.svc.cluster.local",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the ingested server=\"unknown\" series with client_net_peer_name — the loosened selector must still return it")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	s.Contains(bodyStr, `"id":"cluster-alpha/shop/peer-svc"`, "peer-label enrichment must resolve to the addressed service node")
	s.Contains(bodyStr, `"type":"pod-calls-service"`)
	s.Contains(bodyStr, `"target":"cluster-alpha/shop/peer-svc"`)
	s.Contains(bodyStr, `"type":"service-selects-pod"`)
	s.Contains(bodyStr, `"target":"cluster-alpha/alpha-2"`,
		"service-selects-pod edge resolves the backing cart pod via endpointslice targetref")
	s.NotContains(bodyStr, `external/unknown`, "the literal server label must never leak as an external node")
	s.NotContains(bodyStr, `"name":"unknown"`)
}

func (s *GraphSuite) TestClustersDiscovery() {
	// Discovery handler evaluates "now" via the injected Clock. Pin it to
	// fixedNow so the 1h discovery lookback covers the statically-timestamped
	// fixtures.
	srv := s.StartAPIServer(nil, WithClock(clock.Fake{T: fixedNow}))
	resp := s.httpGet(srv.URL + "/v1/clusters")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	s.Contains(string(body), "cluster-alpha")
	s.Contains(string(body), "cluster-beta")
}

func (s *GraphSuite) TestEdgeTypesCatalogue() {
	srv := s.StartAPIServer(nil)
	resp := s.httpGet(srv.URL + "/v1/edge-types")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	// Assert the full registry (graph.EdgeTypes) is advertised — including
	// service-selects-pod, previously omitted (F9).
	for _, et := range []string{"pod-mounts-pvc", "pod-calls-pod", "pod-calls-service", "service-selects-pod"} {
		s.Contains(string(body), et)
	}

	// may_cross_cluster contract (localised model): pod-calls-service resolves to
	// a Service node in the caller's OWN cluster — always intra-cluster — while
	// service-selects-pod fans out across same-family clusters and MAY cross.
	var catalogue struct {
		EdgeTypes []graph.EdgeTypeDefinition `json:"edge_types"`
	}
	s.Require().NoError(json.Unmarshal(body, &catalogue))
	got := map[graph.EdgeType]bool{}
	for _, et := range catalogue.EdgeTypes {
		got[et.Type] = et.MayCrossCluster
	}
	s.False(got["pod-calls-service"], "pod-calls-service resolves to a local service node — always intra-cluster")
	s.True(got["service-selects-pod"], "service-selects-pod may cross clusters via the same-family endpoint union")
}

// TestPodMountsPVCEdgePresent (F8) closes the integration gap for the
// pod-mounts-pvc edge: ingest a kube_pod_spec_volumes_persistentvolumeclaims_info
// binding for the base checkout pod against a real VictoriaMetrics, then assert
// the HTTP response carries the PVC node and the pod→pvc edge (the only edge
// type previously lacking real-VM round-trip coverage).
func (s *GraphSuite) TestPodMountsPVCEdgePresent() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	s.IngestExpFmt(fmt.Sprintf(`# HELP kube_pod_spec_volumes_persistentvolumeclaims_info dummy
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="cluster-alpha",namespace="shop",pod="checkout",persistentvolumeclaim="checkout-data",volume="data",test=%q} 1 %d
`, disc, t1))
	s.Require().True(
		s.WaitForSeries(`kube_pod_spec_volumes_persistentvolumeclaims_info{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested PVC binding")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))

	// Look the PVC up by its exact ID rather than "first pvc node": the
	// integration suite shares one VictoriaMetrics, so sibling tests may have
	// ingested other PVCs (sorting before this one) into the same response.
	var pvc *cytoscape.NodeData
	for i := range body.Elements.Nodes {
		if body.Elements.Nodes[i].Data.ID == "cluster-alpha/shop/checkout-data" {
			pvc = &body.Elements.Nodes[i].Data
			break
		}
	}
	s.Require().NotNil(pvc, "checkout-data pvc node must be present in the response")
	s.Equal("pvc", pvc.Type)
	s.Equal("checkout-data", pvc.Name)

	var found bool
	for _, e := range body.Elements.Edges {
		if e.Data.Type == "pod-mounts-pvc" &&
			e.Data.Source == "cluster-alpha/alpha-1" &&
			e.Data.Target == "cluster-alpha/shop/checkout-data" {
			found = true
		}
	}
	s.True(found, "pod-mounts-pvc edge checkout→checkout-data must be present")
}

// TestPVCStorageClassNodeAndEdge — ingest a PVC binding, its matching
// kube_persistentvolumeclaim_info storageclass, and a kube_storageclass_info
// series (provisioner + NetApp/Ceph parameter labels) against a real
// VictoriaMetrics, then assert the response carries a REAL type="storageclass"
// node (with typed provisioner/parameters, labels {cluster}, nested under its
// cluster group), the PVC nests under its NAMESPACE group, and a
// pvc-to-storageclass edge links them. End-to-end coverage of the new
// StorageClass design (supersedes the old cluster > storageclass > pvc grouping).
func (s *GraphSuite) TestPVCStorageClassNodeAndEdge() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	s.IngestExpFmt(fmt.Sprintf(`# HELP kube_pod_spec_volumes_persistentvolumeclaims_info dummy
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="cluster-alpha",namespace="shop",pod="checkout",persistentvolumeclaim="mongo-data",volume="data",test=%q} 1 %d
# HELP kube_persistentvolumeclaim_info dummy
kube_persistentvolumeclaim_info{cluster="cluster-alpha",namespace="shop",persistentvolumeclaim="mongo-data",storageclass="gp3-ssd",test=%q} 1 %d
# HELP kube_storageclass_info dummy
kube_storageclass_info{cluster="cluster-alpha",storageclass="gp3-ssd",provisioner="ebs.csi.aws.com",storagePools="aggr1",fsType="ext4",test=%q} 1 %d
`, disc, t1, disc, t1, disc, t1))
	s.Require().True(
		s.WaitForSeries(`kube_storageclass_info{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_storageclass_info")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))

	byID := map[string]cytoscape.NodeData{}
	for _, n := range body.Elements.Nodes {
		byID[n.Data.ID] = n.Data
	}

	sc, ok := byID["cluster-alpha/storageclass/gp3-ssd"]
	s.Require().True(ok, "real storageclass node must be present")
	s.Equal("storageclass", sc.Type)
	s.Equal("gp3-ssd", sc.Name)
	s.Equal("cluster/cluster-alpha", sc.Parent, "storageclass node nests under its cluster group")
	s.Equal(map[string]string{"cluster": "cluster-alpha"}, sc.Labels, "labels stay {cluster}")
	s.Equal("ebs.csi.aws.com", sc.Provisioner)
	s.Equal("aggr1", sc.Parameters["pool"])
	s.Equal("ext4", sc.Parameters["fs"])

	pvc, ok := byID["cluster-alpha/shop/mongo-data"]
	s.Require().True(ok, "pvc node must be present")
	s.Equal("cluster-alpha/namespace/shop", pvc.Parent,
		"pvc nests under its namespace group (pvc→storageclass is an edge now)")
	_, hasLabel := pvc.Labels["storageclass"]
	s.False(hasLabel, "storageclass must not leak into pvc labels")

	var found bool
	for _, e := range body.Elements.Edges {
		if e.Data.Type == "pvc-to-storageclass" &&
			e.Data.Source == "cluster-alpha/shop/mongo-data" &&
			e.Data.Target == "cluster-alpha/storageclass/gp3-ssd" {
			found = true
		}
	}
	s.True(found, "expected a pvc-to-storageclass edge from the PVC to the StorageClass node")
}

// TestPVCWithoutStorageClassNestsUnderNamespace — a PVC binding with NO matching
// kube_persistentvolumeclaim_info series nests under its NAMESPACE group and
// emits no pvc-to-storageclass edge, exercising the graceful-degradation path
// end-to-end.
func (s *GraphSuite) TestPVCWithoutStorageClassNestsUnderNamespace() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	s.IngestExpFmt(fmt.Sprintf(`# HELP kube_pod_spec_volumes_persistentvolumeclaims_info dummy
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="cluster-alpha",namespace="shop",pod="checkout",persistentvolumeclaim="legacy-data",volume="legacy",test=%q} 1 %d
`, disc, t1))
	s.Require().True(
		s.WaitForSeries(`kube_pod_spec_volumes_persistentvolumeclaims_info{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested PVC binding")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, nil))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))

	var pvc *cytoscape.NodeData
	for i := range body.Elements.Nodes {
		if body.Elements.Nodes[i].Data.ID == "cluster-alpha/shop/legacy-data" {
			pvc = &body.Elements.Nodes[i].Data
			break
		}
	}
	s.Require().NotNil(pvc, "pvc node must be present")
	s.Equal("cluster-alpha/namespace/shop", pvc.Parent,
		"a PVC with no resolved StorageClass nests under its namespace group")
	// This class-less PVC emits no pvc-to-storageclass edge of its own (other
	// PVCs in the shared suite topology may have StorageClasses, so the check is
	// scoped to this PVC's id).
	for _, e := range body.Elements.Edges {
		if e.Data.Source == "cluster-alpha/shop/legacy-data" {
			s.NotEqual("pvc-to-storageclass", e.Data.Type, "no pvc-to-storageclass edge for a class-less PVC")
		}
	}
}

// TestMetricPrefix_ResolvesPrefixedSeries covers design.md D26 end-to-end
// against a real VictoriaMetrics container: ingest a topology under an
// `o11y_`-prefixed metric-name family, start the API with
// `cfg.MetricPrefix="o11y_"`, and assert the resulting graph contains the
// pod node. Without the prefix knob the build would issue queries for stock
// `kube_pod_info` / `kube_node_info` which the fixture deliberately does NOT
// publish, so an empty graph would result.
func (s *GraphSuite) TestMetricPrefix_ResolvesPrefixedSeries() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000

	// The service-graph metric is never metric-prefixed (D26), so this
	// (unprefixed) edge keeps prefixed-pod connectivity-connected — without it
	// the default prune would drop the only pod and the assertion would see an
	// empty graph. server="ext" (no UID, non-"://") resolves to an external node.
	exposition := fmt.Sprintf(`# HELP o11y_kube_pod_info dummy
o11y_kube_pod_info{cluster="cluster-prefix",namespace="ops",pod="prefixed-pod",uid="prefix-uid-1",node="worker-x",test=%q} 1 %d
o11y_kube_node_info{cluster="cluster-prefix",node="worker-x",test=%q} 1 %d
traces_service_graph_request_total{cluster="cluster-prefix",client="prefixed-pod",server="ext",client_k8s_pod_uid="prefix-uid-1",server_k8s_pod_uid="",test=%q} 0 %d
traces_service_graph_request_total{cluster="cluster-prefix",client="prefixed-pod",server="ext",client_k8s_pod_uid="prefix-uid-1",server_k8s_pod_uid="",test=%q} 60 %d
`,
		disc, t1,
		disc, t1,
		disc, t0,
		disc, t1,
	)
	s.IngestExpFmt(exposition)
	s.Require().True(
		s.WaitForSeries(`o11y_kube_pod_info{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested o11y_kube_pod_info",
	)
	s.Require().True(
		s.WaitForSeries(`rate(traces_service_graph_request_total{cluster="cluster-prefix"}[5m]) > 0`, fixedNow, 30*time.Second),
		"VM did not observe non-zero prefixed-pod service-graph rate",
	)

	srv := s.StartAPIServer(func(cfg *config.Config) {
		cfg.MetricPrefix = "o11y_"
	})
	resp := s.httpGet(s.graphURL(srv.URL, func(q url.Values) { q.Set("cluster", "cluster-prefix") }))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	s.Contains(bodyStr, `"id":"cluster-prefix/prefix-uid-1"`,
		"pod resolved from o11y_-prefixed topology series")
	s.Contains(bodyStr, `"name":"prefixed-pod"`)
}

func (s *GraphSuite) TestAPIKey_FileBacked_Enforced() {
	srv := s.StartAPIServer(func(cfg *config.Config) {

		cfg.APIKeys = "secret-test-key-1,secret-test-key-2"
	})

	// Without header → 401.
	without, err := http.NewRequestWithContext(s.T().Context(), http.MethodGet, s.graphURL(srv.URL, nil), nil)
	s.Require().NoError(err)
	resp1, err := http.DefaultClient.Do(without)
	s.Require().NoError(err)
	_ = resp1.Body.Close()
	s.Equal(http.StatusUnauthorized, resp1.StatusCode)

	// With wrong key → 401.
	wrong, err := http.NewRequestWithContext(s.T().Context(), http.MethodGet, s.graphURL(srv.URL, nil), nil)
	s.Require().NoError(err)
	wrong.Header.Set("X-API-Key", "nope")
	resp2, err := http.DefaultClient.Do(wrong)
	s.Require().NoError(err)
	_ = resp2.Body.Close()
	s.Equal(http.StatusUnauthorized, resp2.StatusCode)

	// With valid key → 200.
	good, err := http.NewRequestWithContext(s.T().Context(), http.MethodGet, s.graphURL(srv.URL, nil), nil)
	s.Require().NoError(err)
	good.Header.Set("X-API-Key", "secret-test-key-2")
	resp3, err := http.DefaultClient.Do(good)
	s.Require().NoError(err)
	_ = resp3.Body.Close()
	s.Equal(http.StatusOK, resp3.StatusCode)

	// /livez stays open even with auth enabled.
	live, err := http.Get(srv.URL + "/livez") //nolint:noctx,gosec // local httptest URL
	s.Require().NoError(err)
	_ = live.Body.Close()
	s.Equal(http.StatusOK, live.StatusCode)
}
