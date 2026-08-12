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
		q.Set("name", "checkout")
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
		q.Set("name", "a")
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
			// Failure query succeeded (empty) → error_rate == 0; no histogram → no p90.
			s.Require().NotNil(e.Data.Metrics.ErrorRate)
			s.InDelta(0.0, *e.Data.Metrics.ErrorRate, 1e-12)
			s.Nil(e.Data.Metrics.P90ServerMs)
		}
	}
	s.True(found)
}

func (s *REDMetricsSuite) TestPeerResolvedPodIP_NoMetrics() {
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
		q.Set("name", "client")
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

	var found bool
	for _, e := range parsed.Elements.Edges {
		if e.Data.Source == "cluster-alpha/peer-c" && e.Data.Target == "cluster-alpha/peer-s" {
			found = true
			s.Equal("pod-calls-pod", e.Data.Type)
			s.True(len(e.Data.Metrics) == 0 || string(e.Data.Metrics) == "null",
				"peer-resolved Pod-IP edge must omit data.metrics, got %s", e.Data.Metrics)
		}
	}
	s.True(found, "expected peer-resolved pod-calls-pod edge")
}
