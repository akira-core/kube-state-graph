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
)

// REDMetricsSuite exercises data.metrics end-to-end against a real
// VictoriaMetrics container with hand-crafted total + failed + bucket series.
type REDMetricsSuite struct {
	VMSuite
}

func TestREDMetricsSuite(t *testing.T) {
	suite.Run(t, new(REDMetricsSuite))
}

func (s *REDMetricsSuite) TestFullREDMetrics() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	const step = 60.0
	// rate = 10 req/s, fail rate = 2 req/s → error_rate = 0.2
	// Histogram cumulative (rate of bucket counts): p90 lands at 0.5s → 500ms.
	exposition := fmt.Sprintf(`# HELP red full
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="checkout",uid="red-c",node="worker-0",test=%q} 1 %d
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="cart",uid="red-s",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="cluster-alpha",node="worker-0",test=%q} 1 %d
traces_service_graph_request_total{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="red-c",server_k8s_pod_uid="red-s",test=%q} 0 %d
traces_service_graph_request_total{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="red-c",server_k8s_pod_uid="red-s",test=%q} %g %d
traces_service_graph_request_failed_total{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="red-c",server_k8s_pod_uid="red-s",test=%q} 0 %d
traces_service_graph_request_failed_total{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="red-c",server_k8s_pod_uid="red-s",test=%q} %g %d
traces_service_graph_request_server_seconds_bucket{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="red-c",server_k8s_pod_uid="red-s",le="0.1",test=%q} 0 %d
traces_service_graph_request_server_seconds_bucket{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="red-c",server_k8s_pod_uid="red-s",le="0.1",test=%q} %g %d
traces_service_graph_request_server_seconds_bucket{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="red-c",server_k8s_pod_uid="red-s",le="0.5",test=%q} 0 %d
traces_service_graph_request_server_seconds_bucket{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="red-c",server_k8s_pod_uid="red-s",le="0.5",test=%q} %g %d
traces_service_graph_request_server_seconds_bucket{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="red-c",server_k8s_pod_uid="red-s",le="+Inf",test=%q} 0 %d
traces_service_graph_request_server_seconds_bucket{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="red-c",server_k8s_pod_uid="red-s",le="+Inf",test=%q} %g %d
`,
		disc, t1, disc, t1, disc, t1,
		disc, t0, disc, 10*step, t1,
		disc, t0, disc, 2*step, t1,
		// bucket rates: 0.1 → 50, 0.5 → 90, +Inf → 100 (of total rate-units)
		disc, t0, disc, 50*step, t1,
		disc, t0, disc, 90*step, t1,
		disc, t0, disc, 100*step, t1,
	)
	s.IngestExpFmt(exposition)
	s.Require().True(s.WaitForSeries(
		`rate(traces_service_graph_request_total{test=`+strconv.Quote(disc)+`}[5m]) > 0`,
		fixedNow, 30*time.Second))

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, func(q url.Values) {
		q.Set("edge_type", "pod-calls-pod")
	}))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)

	var parsed struct {
		Elements struct {
			Edges []struct {
				Data struct {
					Type    string `json:"type"`
					Source  string `json:"source"`
					Target  string `json:"target"`
					Metrics *struct {
						Rate        float64  `json:"rate"`
						ErrorRate   *float64 `json:"error_rate"`
						P90ServerMs *float64 `json:"p90_server_ms"`
					} `json:"metrics"`
				} `json:"data"`
			} `json:"edges"`
		} `json:"elements"`
	}
	s.Require().NoError(json.Unmarshal(body, &parsed))

	var found bool
	for _, e := range parsed.Elements.Edges {
		if e.Data.Source == "cluster-alpha/red-c" && e.Data.Target == "cluster-alpha/red-s" {
			found = true
			s.Require().NotNil(e.Data.Metrics, "UID-resolved pod pair must carry metrics")
			s.InDelta(10.0, e.Data.Metrics.Rate, 0.5, "rate ≈ 10 req/s")
			s.Require().NotNil(e.Data.Metrics.ErrorRate)
			s.InDelta(0.2, *e.Data.Metrics.ErrorRate, 0.05)
			s.Require().NotNil(e.Data.Metrics.P90ServerMs, "p90 from classic buckets")
			// rank=90, bucket (0.1, 0.5] with counts 50→90 → exactly 0.5s = 500ms
			s.InDelta(500.0, *e.Data.Metrics.P90ServerMs, 50.0)
		}
	}
	s.True(found, "expected red-c → red-s edge")
}

func (s *REDMetricsSuite) TestRateOnlyWhenNoFailureOrHistogram() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	const step = 60.0
	exposition := fmt.Sprintf(`# HELP rate only
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="a",uid="ro-a",node="worker-0",test=%q} 1 %d
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="b",uid="ro-b",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="cluster-alpha",node="worker-0",test=%q} 1 %d
traces_service_graph_request_total{client="a",server="b",cluster="cluster-alpha",client_k8s_pod_uid="ro-a",server_k8s_pod_uid="ro-b",test=%q} 0 %d
traces_service_graph_request_total{client="a",server="b",cluster="cluster-alpha",client_k8s_pod_uid="ro-a",server_k8s_pod_uid="ro-b",test=%q} %g %d
`,
		disc, t1, disc, t1, disc, t1,
		disc, t0, disc, 3*step, t1,
	)
	s.IngestExpFmt(exposition)
	s.Require().True(s.WaitForSeries(
		`rate(traces_service_graph_request_total{test=`+strconv.Quote(disc)+`}[5m]) > 0`,
		fixedNow, 30*time.Second))

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, func(q url.Values) {
		q.Set("edge_type", "pod-calls-pod")
	}))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)

	var parsed struct {
		Elements struct {
			Edges []struct {
				Data struct {
					Source  string `json:"source"`
					Target  string `json:"target"`
					Metrics *struct {
						Rate        float64  `json:"rate"`
						ErrorRate   *float64 `json:"error_rate"`
						P90ServerMs *float64 `json:"p90_server_ms"`
					} `json:"metrics"`
				} `json:"data"`
			} `json:"edges"`
		} `json:"elements"`
	}
	s.Require().NoError(json.Unmarshal(body, &parsed))

	var found bool
	for _, e := range parsed.Elements.Edges {
		if e.Data.Source == "cluster-alpha/ro-a" && e.Data.Target == "cluster-alpha/ro-b" {
			found = true
			s.Require().NotNil(e.Data.Metrics)
			s.InDelta(3.0, e.Data.Metrics.Rate, 0.5)
			// This case ingests neither companion metric, so no histogram
			// series can join → no p90, unconditionally.
			s.Nil(e.Data.Metrics.P90ServerMs)
			// error_rate is deliberately NOT pinned to one value here: the
			// selectors carry no `test` discriminator, so whether the failure
			// counter reads as "absent upstream" (omitted) or as "read, no
			// series for this edge" (0) depends on whether a sibling case in
			// the shared container has already ingested _failed_total. Both
			// are spec-correct; what must never happen is a non-zero value.
			// The absent-vs-zero contract itself is pinned deterministically
			// by the pkg/build unit tests.
			if er := e.Data.Metrics.ErrorRate; er != nil {
				s.InDelta(0.0, *er, 1e-12)
			}
		}
	}
	s.True(found)
}

func (s *REDMetricsSuite) TestPeerResolvedPodIPCarriesMetrics() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	const step = 60.0
	// Client dials peer pod IP directly with server="unknown" and empty server UID.
	exposition := fmt.Sprintf(`# HELP peer ip
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="client",uid="peer-c",node="worker-0",pod_ip="10.244.0.1",test=%q} 1 %d
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="backend",uid="peer-s",node="worker-0",pod_ip="10.244.1.9",test=%q} 1 %d
kube_node_info{cluster="cluster-alpha",node="worker-0",test=%q} 1 %d
traces_service_graph_request_total{client="client",server="unknown",cluster="cluster-alpha",client_k8s_pod_uid="peer-c",server_k8s_pod_uid="",client_server_address="10.244.1.9",test=%q} 0 %d
traces_service_graph_request_total{client="client",server="unknown",cluster="cluster-alpha",client_k8s_pod_uid="peer-c",server_k8s_pod_uid="",client_server_address="10.244.1.9",test=%q} %g %d
`,
		disc, t1, disc, t1, disc, t1,
		disc, t0, disc, 4*step, t1,
	)
	s.IngestExpFmt(exposition)
	s.Require().True(s.WaitForSeries(
		`rate(traces_service_graph_request_total{test=`+strconv.Quote(disc)+`}[5m]) > 0`,
		fixedNow, 30*time.Second))

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, func(q url.Values) {
		q.Set("edge_type", "pod-calls-pod")
	}))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)

	var parsed struct {
		Elements struct {
			Edges []struct {
				Data struct {
					Type    string `json:"type"`
					Source  string `json:"source"`
					Target  string `json:"target"`
					Metrics *struct {
						Rate      float64  `json:"rate"`
						ErrorRate *float64 `json:"error_rate"`
					} `json:"metrics"`
				} `json:"data"`
			} `json:"edges"`
		} `json:"elements"`
	}
	s.Require().NoError(json.Unmarshal(body, &parsed))

	var found bool
	for _, e := range parsed.Elements.Edges {
		if e.Data.Source == "cluster-alpha/peer-c" && e.Data.Target == "cluster-alpha/peer-s" {
			found = true
			s.Equal("pod-calls-pod", e.Data.Type)
			s.Require().NotNil(e.Data.Metrics,
				"a peer-resolved endpoint that names a real pod is measured like any other")
			s.InDelta(4.0, e.Data.Metrics.Rate, 0.5)
		}
	}
	s.True(found, "expected peer-resolved pod-calls-pod edge")
}

// TestConnStringServiceCarriesMetricsFanOutDoesNot pins the widened rule at the
// wire: a D29 connection-string endpoint resolves to a Service, that
// pod-calls-service edge carries data.metrics, and the service-selects-pod
// fan-out beneath it carries none — so the caller's rate is reported exactly
// once.
func (s *REDMetricsSuite) TestConnStringServiceCarriesMetricsFanOutDoesNot() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	const step = 60.0
	// A dedicated namespace / service name keeps every id in this case unique
	// against the other suites sharing the container.
	exposition := fmt.Sprintf(`# HELP conn string service
kube_pod_info{cluster="cluster-alpha",namespace="cs-shop",pod="cs-checkout",uid="cs-c",node="worker-0",test=%q} 1 %d
kube_pod_info{cluster="cluster-alpha",namespace="cs-shop",pod="cs-payments-0",uid="cs-p",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="cluster-alpha",node="worker-0",test=%q} 1 %d
kube_service_info{cluster="cluster-alpha",namespace="cs-shop",service="cs-payments",cluster_ip="10.0.0.5",test=%q} 1 %d
kube_endpointslice_endpoints{cluster="cluster-alpha",namespace="cs-shop",endpointslice="cs-payments-x",targetref_kind="Pod",targetref_name="cs-payments-0",targetref_namespace="cs-shop",test=%q} 1 %d
kube_endpointslice_labels{cluster="cluster-alpha",namespace="cs-shop",endpointslice="cs-payments-x",label_kubernetes_io_service_name="cs-payments",test=%q} 1 %d
traces_service_graph_request_total{client="cs-checkout",server="http://cs-payments.cs-shop.svc.cluster.local:8080",cluster="cluster-alpha",client_k8s_pod_uid="cs-c",server_k8s_pod_uid="",test=%q} 0 %d
traces_service_graph_request_total{client="cs-checkout",server="http://cs-payments.cs-shop.svc.cluster.local:8080",cluster="cluster-alpha",client_k8s_pod_uid="cs-c",server_k8s_pod_uid="",test=%q} %g %d
`,
		disc, t1, disc, t1, disc, t1, disc, t1, disc, t1, disc, t1,
		disc, t0, disc, 6*step, t1,
	)
	s.IngestExpFmt(exposition)
	s.Require().True(s.WaitForSeries(
		`rate(traces_service_graph_request_total{test=`+strconv.Quote(disc)+`}[5m]) > 0`,
		fixedNow, 30*time.Second))

	// No `name` filter: the fan-out edge touches neither the caller nor any
	// node a name filter would anchor on, so it would be projected away.
	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, func(q url.Values) {
		q.Set("namespace", "cs-shop")
	}))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)

	var parsed struct {
		Elements struct {
			Edges []struct {
				Data struct {
					Type    string          `json:"type"`
					Source  string          `json:"source"`
					Target  string          `json:"target"`
					Metrics json.RawMessage `json:"metrics"`
				} `json:"data"`
			} `json:"edges"`
		} `json:"elements"`
	}
	s.Require().NoError(json.Unmarshal(body, &parsed))

	const svcID = "cluster-alpha/cs-shop/cs-payments"
	var sawSvcEdge, sawFanOut bool
	for _, e := range parsed.Elements.Edges {
		switch {
		case e.Data.Type == "pod-calls-service" && e.Data.Source == "cluster-alpha/cs-c" && e.Data.Target == svcID:
			sawSvcEdge = true
			s.NotEmpty(e.Data.Metrics, "the connection-string service edge must carry data.metrics")
			s.NotEqual("null", string(e.Data.Metrics))
		case e.Data.Type == "service-selects-pod" && e.Data.Source == svcID:
			sawFanOut = true
			s.True(len(e.Data.Metrics) == 0 || string(e.Data.Metrics) == "null",
				"synthesised fan-out must omit data.metrics, got %s", e.Data.Metrics)
		}
	}
	s.True(sawSvcEdge, "expected the caller → service edge")
	s.True(sawFanOut, "expected the service → pod fan-out")
}

// TestSpanLinkEdgeEmittedWithoutMetrics pins D1b at the wire: an
// edge_relation="link" series still produces its edge, but the two RED
// selectors exclude it upstream and the parse marks it out of scope, so the
// edge carries no data.metrics at all.
func (s *REDMetricsSuite) TestSpanLinkEdgeEmittedWithoutMetrics() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	const step = 60.0
	exposition := fmt.Sprintf(`# HELP span link
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="producer",uid="lk-c",node="worker-0",test=%q} 1 %d
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="consumer",uid="lk-s",node="worker-0",test=%q} 1 %d
kube_node_info{cluster="cluster-alpha",node="worker-0",test=%q} 1 %d
traces_service_graph_request_total{client="producer",server="consumer",cluster="cluster-alpha",client_k8s_pod_uid="lk-c",server_k8s_pod_uid="lk-s",edge_relation="link",test=%q} 0 %d
traces_service_graph_request_total{client="producer",server="consumer",cluster="cluster-alpha",client_k8s_pod_uid="lk-c",server_k8s_pod_uid="lk-s",edge_relation="link",test=%q} %g %d
traces_service_graph_request_failed_total{client="producer",server="consumer",cluster="cluster-alpha",client_k8s_pod_uid="lk-c",server_k8s_pod_uid="lk-s",edge_relation="link",test=%q} 0 %d
traces_service_graph_request_failed_total{client="producer",server="consumer",cluster="cluster-alpha",client_k8s_pod_uid="lk-c",server_k8s_pod_uid="lk-s",edge_relation="link",test=%q} %g %d
`,
		disc, t1, disc, t1, disc, t1,
		disc, t0, disc, 5*step, t1,
		disc, t0, disc, 1*step, t1,
	)
	s.IngestExpFmt(exposition)
	s.Require().True(s.WaitForSeries(
		`rate(traces_service_graph_request_total{test=`+strconv.Quote(disc)+`}[5m]) > 0`,
		fixedNow, 30*time.Second))

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	resp := s.httpGet(s.graphURL(srv.URL, func(q url.Values) {
		q.Set("edge_type", "pod-calls-pod")
	}))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)

	var parsed struct {
		Elements struct {
			Edges []struct {
				Data struct {
					Source  string          `json:"source"`
					Target  string          `json:"target"`
					Metrics json.RawMessage `json:"metrics"`
				} `json:"data"`
			} `json:"edges"`
		} `json:"elements"`
	}
	s.Require().NoError(json.Unmarshal(body, &parsed))

	var found bool
	for _, e := range parsed.Elements.Edges {
		if e.Data.Source == "cluster-alpha/lk-c" && e.Data.Target == "cluster-alpha/lk-s" {
			found = true
			s.True(len(e.Data.Metrics) == 0 || string(e.Data.Metrics) == "null",
				"a span-link edge must omit data.metrics, got %s", e.Data.Metrics)
		}
	}
	s.True(found, "the span-link edge must still be emitted")
}
