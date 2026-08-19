package build

import (
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// parseServiceGraphRED supplies RED vectors to the parse so these tests can
// exercise the full attach path without mocking the querier. Test-only by
// design, so it lives here rather than in servicegraph.go — production code
// must not carry test-only constructors.
func parseServiceGraphRED(vec, failed, duration model.Vector, topology Topology) ServiceGraphResult {
	return parseServiceGraphREDRoutes(vec, failed, duration, topology, nil)
}

// parseServiceGraphREDRoutes is parseServiceGraphRED with a route index, for
// the ingress-chain cases.
func parseServiceGraphREDRoutes(vec, failed, duration model.Vector, topology Topology, routes routeIndex) ServiceGraphResult {
	if len(vec) == 0 {
		return ServiceGraphResult{}
	}
	return parseWithResolver(vec, newSGResolver(topology), routes, redInputs{
		Failed:   failed,
		Duration: duration,
	})
}

// uidPodSample is a total-series sample with both pod UIDs populated.
func uidPodSample(clientUID, serverUID string, rate float64, extra ...model.Metric) model.Sample {
	m := model.Metric{
		"client":             "checkout",
		"server":             "payments",
		"cluster":            "cluster-alpha",
		"client_k8s_pod_uid": model.LabelValue(clientUID),
		"server_k8s_pod_uid": model.LabelValue(serverUID),
	}
	for _, e := range extra {
		for k, v := range e {
			m[k] = v
		}
	}
	return model.Sample{Metric: m, Value: model.SampleValue(rate)}
}

// failSample mirrors a total series' labels for the failure counter join.
func failSample(clientUID, serverUID string, rate float64, extra ...model.Metric) model.Sample {
	s := uidPodSample(clientUID, serverUID, rate, extra...)
	return s
}

// bucketSample is one duration-histogram bucket. The histogram is read RAW
// (design D4), so a bucket series carries its request-total series' full
// dimension set PLUS `le`, and joins by that identity minus `le`. The fixture
// mirrors that shape — a bucket built from a narrower label set would join
// nothing, exactly as it would in production.
func bucketSample(clientUID, serverUID, le string, count float64, extra ...model.Metric) model.Sample {
	s := uidPodSample(clientUID, serverUID, count, extra...)
	s.Metric[model.BucketLabel] = model.LabelValue(le)
	return s
}

// linkDim marks a series as a connector-materialised span-link edge (D1b).
func linkDim() model.Metric { return model.Metric{"edge_relation": "link"} }

func edgeMetrics(t *testing.T, res ServiceGraphResult, src, tgt string) *graph.EdgeMetrics {
	t.Helper()
	for _, e := range res.Edges {
		if e.Source == src && e.Target == tgt {
			return e.Metrics
		}
	}
	t.Fatalf("no edge %s → %s", src, tgt)
	return nil
}

// --- 7.1 Attachment rule scenarios ---

func TestRED_BothUIDResolvedRealPods(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", 5))
	failed := sampleVec(failSample("abc", "def", 1))
	// Cumulative buckets: p90 lands in (0.1, 0.5].
	dur := sampleVec(
		bucketSample("abc", "def", "0.05", 10),
		bucketSample("abc", "def", "0.1", 50),
		bucketSample("abc", "def", "0.5", 90),
		bucketSample("abc", "def", "+Inf", 100),
	)
	res := parseServiceGraphRED(vec, failed, dur, sampleTopology())
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	assert.InDelta(t, 5.0, m.Rate, 1e-12)
	require.NotNil(t, m.ErrorRate)
	assert.InDelta(t, 0.2, *m.ErrorRate, 1e-9)
	require.NotNil(t, m.P90ServerMs)
	// rank=90, bucket (0.1, 0.5] counts 50→90: frac=(90-50)/(90-50)=1 → 0.5 s = 500 ms
	assert.InDelta(t, 500.0, *m.P90ServerMs, 1e-6)
}

func TestRED_SynthPodTargetStillGetsMetrics(t *testing.T) {
	// server UID unknown to topology → synth pod; still UID-resolved (D1).
	vec := sampleVec(uidPodSample("abc", "unknown-uid", 3))
	res := parseServiceGraphRED(vec, nil, nil, sampleTopology())
	require.Len(t, res.SynthPods, 1)
	m := edgeMetrics(t, res, "cluster-alpha/abc", res.SynthPods[0].ID())
	require.NotNil(t, m, "synth-pod target is UID-resolved and must carry metrics")
	assert.InDelta(t, 3.0, m.Rate, 1e-12)
	assert.Nil(t, m.ErrorRate, "failure metric absent upstream → error_rate omitted, not 0")
	assert.Nil(t, m.P90ServerMs)
}

// TestRED_PeerResolvedPodIP_GetsMetrics pins the widened attachment rule:
// HOW an endpoint was identified is irrelevant, only what it resolved to. The
// server="unknown" peer-address ladder names a real topology pod here, so the
// edge is measured like any UID-resolved one.
func TestRED_PeerResolvedPodIP_GetsMetrics(t *testing.T) {
	res := parseServiceGraphRED(sampleVec(podIPPeerSample("10.244.1.9")), nil, nil, sampleTopologyPodIP())
	pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
	require.Len(t, pcp, 1)
	require.NotNil(t, pcp[0].Metrics, "peer-resolved Pod-IP target resolves to a pod and must carry metrics")
	assert.InDelta(t, 5.0, pcp[0].Metrics.Rate, 1e-12)
}

// TestRED_PeerResolvedClusterIP_GetsMetrics is the sibling case one rung up the
// ladder: the peer address matches a Service ClusterIP, so the endpoint is a
// service node and the edge is pod-calls-service — also measured. Its
// service-selects-pod fan-out stays synthesised and unmeasured.
func TestRED_PeerResolvedClusterIP_GetsMetrics(t *testing.T) {
	res := parseServiceGraphRED(sampleVec(podIPPeerSample("10.0.0.5")), nil, nil, sampleTopologyWithServices())

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target)
	require.NotNil(t, pcs[0].Metrics)
	assert.InDelta(t, 5.0, pcs[0].Metrics.Rate, 1e-12)

	for _, e := range edgesByType(res, graph.EdgeTypeServiceSelectsPod) {
		assert.Nil(t, e.Metrics, "the fan-out behind a measured service edge stays unmeasured")
	}
}

func TestRED_ExternalEndpoint_NoMetrics(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "payments.example.com",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			// empty server UID, non-:// label → D27 external
		},
		Value: 2,
	})
	res := parseServiceGraphRED(vec, nil, nil, sampleTopology())
	require.NotEmpty(t, res.Edges)
	for _, e := range res.Edges {
		assert.Nil(t, e.Metrics, "external endpoint must carry no metrics")
	}
}

// TestRED_PodCallsService_GetsMetrics: a D29 connection string names the
// Service the caller actually dialled, so that is where the caller's rate
// belongs. Nothing is double-counted — the fan-out below it carries none.
func TestRED_PodCallsService_GetsMetrics(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "http://payments.shop.svc.cluster.local:8080",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
		},
		Value: 4,
	})
	res := parseServiceGraphRED(vec, nil, nil, sampleTopologyWithServices())
	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	require.NotNil(t, pcs[0].Metrics)
	assert.InDelta(t, 4.0, pcs[0].Metrics.Rate, 1e-12)
}

func TestRED_ServiceSelectsPod_NoMetrics(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "http://payments.shop.svc.cluster.local:8080",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
		},
		Value: 4,
	})
	res := parseServiceGraphRED(vec, nil, nil, sampleTopologyWithServices())
	svcEdges := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.NotEmpty(t, svcEdges)
	for _, e := range svcEdges {
		assert.Nil(t, e.Metrics, "service-selects-pod is synthesised fan-out")
	}
}

func TestRED_TopologyEdges_NoMetrics(t *testing.T) {
	// Topology edges are built outside parseServiceGraph; NewEdge alone has nil Metrics.
	e := graph.NewEdge(graph.EdgeTypePodToNode, "a", "b", nil)
	assert.Nil(t, e.Metrics)
	e = graph.NewEdge(graph.EdgeTypePodMountsPVC, "a", "b", nil)
	assert.Nil(t, e.Metrics)
	e = graph.NewEdge(graph.EdgeTypePVCToNetAppAggr, "a", "b", nil)
	assert.Nil(t, e.Metrics)
	assert.Nil(t, e.IO)
}

// --- 7.2 Aggregation ---

func TestRED_TwoSeriesCollapseSumRates(t *testing.T) {
	vec := sampleVec(
		uidPodSample("abc", "def", 2, model.Metric{"connection_type": "http"}),
		uidPodSample("abc", "def", 3, model.Metric{"connection_type": "grpc"}),
	)
	res := parseServiceGraphRED(vec, nil, nil, sampleTopology())
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	assert.InDelta(t, 5.0, m.Rate, 1e-12)
}

// TestRED_UIDAndPeerResolvedSeriesSumRates: a UID-resolved series and a
// peer-resolved series landing on the SAME pair are both in scope, so their
// rates sum. (Under the first draft of D1 this pair carried no metrics at all.)
func TestRED_UIDAndPeerResolvedSeriesSumRates(t *testing.T) {
	topo := sampleTopologyPodIP()
	// Register the peer pod under UID "def" so a UID-resolved series hits the
	// same topology pod that the Pod-IP ladder finds via 10.244.1.9.
	var peerPod *graph.PodNode
	for _, p := range topo.Pods {
		for _, a := range p.IPAddress() {
			if a == "10.244.1.9" {
				peerPod = p
			}
		}
	}
	require.NotNil(t, peerPod)
	if topo.PodsByUID == nil {
		topo.PodsByUID = map[string]*graph.PodNode{}
	}
	topo.PodsByUID["def"] = peerPod

	peerSample := podIPPeerSample("10.244.1.9")
	peerSample.Value = 1
	vec := sampleVec(
		uidPodSample("abc", "def", 4),
		peerSample,
	)

	res := parseServiceGraphRED(vec, nil, nil, topo)
	var m *graph.EdgeMetrics
	for _, e := range res.Edges {
		if e.Type == graph.EdgeTypePodCallsPod && e.Source == "cluster-alpha/abc" && e.Target == peerPod.ID() {
			m = e.Metrics
		}
	}
	require.NotNil(t, m, "both series resolve to pod endpoints and are in scope")
	assert.InDelta(t, 5.0, m.Rate, 1e-12, "4 (UID-resolved) + 1 (peer-resolved)")
}

// --- D1b: span-link series measure nothing ---

// TestRED_AllLinkSeries_EdgeEmittedWithoutMetrics: the edge is a real
// dependency and must survive; only the numbers are suppressed. No special case
// produces this — the in-scope set is empty, so the rate>0 guard rejects it.
func TestRED_AllLinkSeries_EdgeEmittedWithoutMetrics(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", 9, linkDim()))
	res := parseServiceGraphRED(vec, nil, nil, sampleTopology())

	pcp := edgesByType(res, graph.EdgeTypePodCallsPod)
	require.Len(t, pcp, 1, "the span-link edge must still be emitted")
	assert.Equal(t, "cluster-alpha/abc", pcp[0].Source)
	assert.Equal(t, "cluster-beta/def", pcp[0].Target)
	assert.Equal(t, map[string]string{"cluster": "cluster-alpha", "relation": "link"}, pcp[0].Labels)
	assert.Nil(t, pcp[0].Metrics, "a span-link series measures a queue/db hop, not a request")
}

// TestRED_MixedLinkAndDirectSeries_MeasuresNonLinkSubset: unlike the older
// pair-poisoning rule, a link series does not disqualify its pair — both
// subsets describe the same dependency, so the non-link one is measured.
func TestRED_MixedLinkAndDirectSeries_MeasuresNonLinkSubset(t *testing.T) {
	vec := sampleVec(
		uidPodSample("abc", "def", 4, linkDim()),
		uidPodSample("abc", "def", 1, model.Metric{"connection_type": "http"}),
	)
	// Only the non-link series has companions; a producer-side link failure
	// series would already be dropped by the query-layer matcher.
	failed := sampleVec(failSample("abc", "def", 0.5, model.Metric{"connection_type": "http"}))
	res := parseServiceGraphRED(vec, failed, nil, sampleTopology())

	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	assert.InDelta(t, 1.0, m.Rate, 1e-12, "the link series' rate 4 must not be counted")
	require.NotNil(t, m.ErrorRate)
	assert.InDelta(t, 0.5, *m.ErrorRate, 1e-9, "numerator and denominator share one series set")
}

// TestRED_LinkSeriesNotJoined pins that a link series records no join key: even
// if the query layer leaked a matching failure series (it does not — see
// promql.serviceGraphLinkExclusionSelector), it could not be attributed.
func TestRED_LinkSeriesNotJoined(t *testing.T) {
	vec := sampleVec(
		uidPodSample("abc", "def", 2, model.Metric{"connection_type": "http"}),
		uidPodSample("abc", "def", 8, linkDim()),
	)
	failed := sampleVec(failSample("abc", "def", 8, linkDim()))
	res := parseServiceGraphRED(vec, failed, nil, sampleTopology())

	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	require.NotNil(t, m.ErrorRate)
	assert.InDelta(t, 0.0, *m.ErrorRate, 1e-12, "a link failure series must join nothing")
}

// --- D1: the ingress chain's entry hop is not measured ---

// TestRED_RouteChainEntryHopUnmeasured: one series produces caller→ingress AND
// caller→backend. They are two projections of the SAME call, so measuring both
// would double the request in any sum over the chain. The backend edge — the
// one naming the actual destination — carries the measurement.
func TestRED_RouteChainEntryHopUnmeasured(t *testing.T) {
	vec := sampleVec(unknownPeerSample("api.example.com", nil))
	res := parseServiceGraphREDRoutes(vec, nil, nil, sampleTopologyWithIngress(),
		routeIndex{chainKey: chainHitEntry()})

	got := map[string]*graph.EdgeMetrics{}
	for _, e := range edgesByType(res, graph.EdgeTypePodCallsService) {
		got[e.Source+"->"+e.Target] = e.Metrics
	}
	require.Len(t, got, 3)

	backend := got["cluster-alpha/abc->cluster-alpha/shop/payments"]
	require.NotNil(t, backend, "the direct caller→backend edge carries the measurement")
	assert.InDelta(t, 5.0, backend.Rate, 1e-12)

	assert.Nil(t, got["cluster-alpha/abc->cluster-alpha/istio-system/igw"],
		"the chain's entry hop is a second projection of the same call")
	assert.Nil(t, got["cluster-alpha/igw0->cluster-alpha/shop/payments"],
		"the synthesized gateway-pod hop has no contributing series at all")
}

// TestRED_RouteHitWithoutChainMeasuresBackend: with no ingress identity the
// endpoint resolves to the backend alone, so there is no entry hop to suppress
// and the single edge is measured.
func TestRED_RouteHitWithoutChainMeasuresBackend(t *testing.T) {
	entry := chainHitEntry()
	entry.dest.IngressNamespace, entry.dest.IngressService = "", ""
	vec := sampleVec(unknownPeerSample("api.example.com", nil))
	res := parseServiceGraphREDRoutes(vec, nil, nil, sampleTopologyWithIngress(),
		routeIndex{chainKey: entry})

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target)
	require.NotNil(t, pcs[0].Metrics)
	assert.InDelta(t, 5.0, pcs[0].Metrics.Rate, 1e-12)
}

func TestRED_ErrorRateMatchingSeriesSet(t *testing.T) {
	vec := sampleVec(
		uidPodSample("abc", "def", 2, model.Metric{"connection_type": "http"}),
		uidPodSample("abc", "def", 3, model.Metric{"connection_type": "grpc"}),
	)
	// Only the http series has a matching failure.
	failed := sampleVec(failSample("abc", "def", 1, model.Metric{"connection_type": "http"}))
	res := parseServiceGraphRED(vec, failed, nil, sampleTopology())
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	require.NotNil(t, m.ErrorRate)
	assert.InDelta(t, 0.2, *m.ErrorRate, 1e-9) // 1/5
}

func TestRED_P90FromSummedBuckets(t *testing.T) {
	// Two contributing series (differing only in connection_type) collapse onto
	// one pair. Each joins its OWN bucket set by full identity minus `le`; the
	// two sets are summed per boundary before the quantile is taken — combining
	// two per-series quantiles would be wrong and invisible.
	http := model.Metric{"connection_type": "http"}
	grpc := model.Metric{"connection_type": "grpc"}
	vec := sampleVec(
		uidPodSample("abc", "def", 5, http),
		uidPodSample("abc", "def", 5, grpc),
	)
	dur := sampleVec(
		bucketSample("abc", "def", "0.1", 20, http),
		bucketSample("abc", "def", "0.5", 40, http),
		bucketSample("abc", "def", "+Inf", 50, http),
		bucketSample("abc", "def", "0.1", 20, grpc),
		bucketSample("abc", "def", "0.5", 40, grpc),
		bucketSample("abc", "def", "+Inf", 50, grpc),
	)
	res := parseServiceGraphRED(vec, nil, dur, sampleTopology())
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	require.NotNil(t, m.P90ServerMs)
	// Summed: 0.1→40, 0.5→80, +Inf→100. rank=90 > 80, so the quantile lands in
	// the +Inf bucket and clamps to the highest finite boundary: 0.5s = 500ms.
	assert.InDelta(t, 500.0, *m.P90ServerMs, 1e-6)
}

// TestRED_BucketJoinRequiresFullIdentity pins the join contract: a bucket
// series whose labels drift from its request-total series (an extra dimension
// on one metric family, a relabel applied to only one) matches nothing and
// omits p90 — the failure mode accumulateDuration's unmatched tally exists to
// make diagnosable.
func TestRED_BucketJoinRequiresFullIdentity(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", 5))
	dur := sampleVec(
		bucketSample("abc", "def", "0.1", 50, model.Metric{"exporter_instance": "otel-1"}),
		bucketSample("abc", "def", "+Inf", 100, model.Metric{"exporter_instance": "otel-1"}),
	)
	res := parseServiceGraphRED(vec, nil, dur, sampleTopology())
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	assert.Nil(t, m.P90ServerMs, "a drifted bucket label set must join nothing, not mis-attribute")
}

// --- 7.3 Determinism ---

func TestRED_ShuffleInputByteIdentical(t *testing.T) {
	mk := func(seed int64) (model.Vector, model.Vector, model.Vector) {
		rng := rand.New(rand.NewSource(seed))
		totals := []model.Sample{
			uidPodSample("abc", "def", 2, model.Metric{"connection_type": "http"}),
			uidPodSample("abc", "def", 3, model.Metric{"connection_type": "grpc"}),
		}
		fails := []model.Sample{
			failSample("abc", "def", 1, model.Metric{"connection_type": "http"}),
		}
		http := model.Metric{"connection_type": "http"}
		durs := []model.Sample{
			bucketSample("abc", "def", "0.1", 50, http),
			bucketSample("abc", "def", "0.5", 90, http),
			bucketSample("abc", "def", "+Inf", 100, http),
		}
		rng.Shuffle(len(totals), func(i, j int) { totals[i], totals[j] = totals[j], totals[i] })
		rng.Shuffle(len(fails), func(i, j int) { fails[i], fails[j] = fails[j], fails[i] })
		rng.Shuffle(len(durs), func(i, j int) { durs[i], durs[j] = durs[j], durs[i] })
		return sampleVec(totals...), sampleVec(fails...), sampleVec(durs...)
	}

	serialise := func(res ServiceGraphResult) []byte {
		// Build a minimal graph and project so we can use cytoscape.Serialise.
		nodes := []graph.GraphNode{}
		seen := map[string]bool{}
		for _, p := range sampleTopology().Pods {
			nodes = append(nodes, p)
			seen[p.ID()] = true
		}
		for _, sp := range res.SynthPods {
			if !seen[sp.ID()] {
				nodes = append(nodes, sp)
			}
		}
		for _, ext := range res.ExternalNodes {
			if !seen[ext.ID()] {
				nodes = append(nodes, ext)
			}
		}
		g := graph.NewGraph(nodes, res.Edges, time.Unix(0, 0).UTC())
		// Empty scope = full connectivity-connected projection.
		view := graph.Project(g, graph.Scope{})
		body := cytoscape.Serialise(g, view)
		b, err := json.Marshal(body)
		require.NoError(t, err)
		return b
	}

	v1, f1, d1 := mk(1)
	v2, f2, d2 := mk(99)
	b1 := serialise(parseServiceGraphRED(v1, f1, d1, sampleTopology()))
	b2 := serialise(parseServiceGraphRED(v2, f2, d2, sampleTopology()))
	assert.Equal(t, string(b1), string(b2), "shuffled input must produce byte-identical serialised output")
}

// --- 7.4 Degradation ---

func TestRED_FailureQueryError_OmitsErrorRate(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", 5))
	res := parseWithResolver(vec, newSGResolver(sampleTopology()), nil, redInputs{
		FailedErr: assertAnError{},
	})
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	assert.InDelta(t, 5.0, m.Rate, 1e-12)
	assert.Nil(t, m.ErrorRate, "failure query error → error_rate absent, not 0")
}

type assertAnError struct{}

func (assertAnError) Error() string { return "upstream failed" }

// The counter WAS read (non-empty result) but carries no series matching this
// edge — the one case the spec defines error_rate == 0 for.
func TestRED_FailureQueryNoMatch_ErrorRateZero(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", 5))
	failed := sampleVec(failSample("other-client", "other-server", 3))
	res := parseServiceGraphRED(vec, failed, nil, sampleTopology())
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	require.NotNil(t, m.ErrorRate)
	assert.InDelta(t, 0.0, *m.ErrorRate, 1e-12)
}

// An EMPTY failure result means the metric does not exist upstream, which the
// spec's *RED graceful degradation* requirement puts on the same footing as a
// query error: error_rate is OMITTED so an absent measurement is never
// presented as a measured absence of errors.
func TestRED_FailureMetricAbsent_OmitsErrorRate(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", 5))
	res := parseServiceGraphRED(vec, sampleVec(), nil, sampleTopology())
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	assert.InDelta(t, 5.0, m.Rate, 1e-12)
	assert.Nil(t, m.ErrorRate, "failure metric absent upstream → error_rate omitted, not 0")
}

func TestRED_DurationEmpty_OmitsP90(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", 5))
	res := parseServiceGraphRED(vec, nil, sampleVec(), sampleTopology())
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	assert.Nil(t, m.P90ServerMs)
}

func TestRED_DurationNoLe_OmitsP90(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", 5))
	// A series that JOINS its request-total series but carries no `le` label —
	// the native-histogram / vmrange case, distinct from "joined nothing".
	dur := sampleVec(uidPodSample("abc", "def", 10))
	res := parseServiceGraphRED(vec, nil, dur, sampleTopology())
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	assert.Nil(t, m.P90ServerMs)
}

func TestRED_BothREDQueriesError_RateOnly(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", 7))
	res := parseWithResolver(vec, newSGResolver(sampleTopology()), nil, redInputs{
		FailedErr:   assertAnError{},
		DurationErr: assertAnError{},
	})
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	assert.InDelta(t, 7.0, m.Rate, 1e-12)
	assert.Nil(t, m.ErrorRate)
	assert.Nil(t, m.P90ServerMs)
}

func TestRED_LabelsNeverGainNumericKeys(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", 5))
	failed := sampleVec(failSample("abc", "def", 1))
	res := parseServiceGraphRED(vec, failed, nil, sampleTopology())
	for _, e := range res.Edges {
		_, hasRate := e.Labels["rate"]
		_, hasER := e.Labels["error_rate"]
		_, hasP90 := e.Labels["p90_server_ms"]
		assert.False(t, hasRate)
		assert.False(t, hasER)
		assert.False(t, hasP90)
	}
}

// TestRED_NonFiniteRateOmitsMetrics pins the serialisability guard: encoding/json
// rejects Inf/NaN, so a single +Inf upstream sample must degrade to "no metrics
// on that edge" rather than making the whole /v1/graph body unencodable (500).
// The zero-rate/NaN guard in the parse loop lets +Inf through — `+Inf > 0` is
// true — so the check has to live at attach time.
func TestRED_NonFiniteRateOmitsMetrics(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", math.Inf(1)))
	res := parseServiceGraphRED(vec, nil, nil, sampleTopology())

	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	assert.Nil(t, m, "+Inf rate must not produce a metrics object")

	// The whole body must still encode.
	pods := sampleTopology().Pods
	nodes := make([]graph.GraphNode, 0, len(pods))
	for _, p := range pods {
		nodes = append(nodes, p)
	}
	g := graph.NewGraph(nodes, res.Edges, time.Unix(0, 0).UTC())
	body := cytoscape.Serialise(g, graph.Project(g, graph.Scope{}))
	_, err := json.Marshal(body)
	require.NoError(t, err, "response body must stay JSON-encodable")
}

// Guard that classicQuantile Inf-clamp path is exercised via attach.
func TestRED_P90InfClamp(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", 1))
	dur := sampleVec(
		bucketSample("abc", "def", "0.1", 10),
		bucketSample("abc", "def", "1.0", 50),
		bucketSample("abc", "def", "+Inf", 100),
	)
	res := parseServiceGraphRED(vec, nil, dur, sampleTopology())
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	require.NotNil(t, m.P90ServerMs)
	assert.InDelta(t, 1000.0, *m.P90ServerMs, 1e-6) // 1.0s → 1000ms
	assert.False(t, math.IsInf(*m.P90ServerMs, 0))
}

// TestRED_D33ClearedUIDDoesNotAttachToExternal guards the interaction between
// the D33 self-loop UID normalisation and the RED attachment rule (design D1).
//
// D33 clears the "://" side's UID when both UIDs are non-empty and equal, so
// that side falls through to connection-string resolution. The raw-UID in-scope
// test runs BEFORE that normalisation, and an unresolvable "://" target
// materialises an `external` node WITHOUT downgrading the edge type (only a
// service target does that). So neither the raw UIDs nor the edge type can
// catch this pair on its own — the resolved endpoint types must be checked.
func TestRED_D33ClearedUIDDoesNotAttachToExternal(t *testing.T) {
	// client and server UID are identical and non-empty; the server label is an
	// unresolvable connection string, so the server side becomes external/…
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "redis://nowhere.invalid:6379",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "abc",
		},
		Value: 7,
	})

	res := parseServiceGraphRED(vec, nil, nil, sampleTopology())

	require.NotEmpty(t, res.Edges)
	sawExternal := false
	for _, e := range res.Edges {
		if strings.HasPrefix(e.Target, "external/") || strings.HasPrefix(e.Source, "external/") {
			sawExternal = true
			assert.Nilf(t, e.Metrics,
				"external endpoint must never carry RED: %s -> %s", e.Source, e.Target)
		}
	}
	require.True(t, sawExternal, "fixture must produce an external endpoint")
}

// TestRED_D33ClearedUIDAttachesToService is the sibling case: a RESOLVABLE
// "://" target names a real Service, so the pod-calls-service edge IS measured.
// Pinned alongside the external case above so the pair is read together — the
// discriminator is the resolved node type, never the edge type.
func TestRED_D33ClearedUIDAttachesToService(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "http://payments.shop.svc.cluster.local:8080/api",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "pay0",
			"server_k8s_pod_uid": "pay0",
		},
		Value: 3,
	})

	res := parseServiceGraphRED(vec, nil, nil, sampleTopologyWithServices())

	pcs := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, pcs, 1)
	assert.Equal(t, "cluster-alpha/shop/payments", pcs[0].Target)
	require.NotNil(t, pcs[0].Metrics)
	assert.InDelta(t, 3.0, pcs[0].Metrics.Rate, 1e-12)
}

// TestRED_InvariantMetricsOnlyOnNamedEndpoints is the real form of the
// attachment invariant: it drives the ACTUAL parse over a corpus of series
// shapes and checks the emitted graph, rather than seeding a graph with
// attachments that already satisfy the rule. A tautological version cannot
// catch a resolution-path defect (see TestRED_D33ClearedUIDDoesNotAttachToExternal).
//
// The invariant: an edge carrying metrics has BOTH endpoints resolved to a pod
// or a service node, is not a synthesised edge, and is not the ingress chain's
// entry hop.
func TestRED_InvariantMetricsOnlyOnNamedEndpoints(t *testing.T) {
	connString := func(server, clientUID, serverUID string) model.Sample {
		return model.Sample{Metric: model.Metric{
			"client": "checkout", "server": model.LabelValue(server),
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": model.LabelValue(clientUID),
			"server_k8s_pod_uid": model.LabelValue(serverUID),
		}, Value: 3}
	}

	cases := []struct {
		name   string
		vec    model.Vector
		topo   Topology
		routes routeIndex
	}{
		{name: "uid-resolved pods", vec: sampleVec(uidPodSample("abc", "def", 5)), topo: sampleTopology()},
		{name: "synth pod target", vec: sampleVec(uidPodSample("abc", "no-such-uid", 2)), topo: sampleTopology()},
		{name: "span-link series", vec: sampleVec(uidPodSample("abc", "def", 5, linkDim())), topo: sampleTopology()},
		{name: "d27 external", vec: sampleVec(connString("payments.example.com", "abc", "")), topo: sampleTopology()},
		{name: "conn-string service", vec: sampleVec(connString("http://payments.shop.svc.cluster.local:8080", "abc", "")), topo: sampleTopologyWithServices()},
		{name: "d33 collision → external", vec: sampleVec(connString("redis://nowhere.invalid:6379", "abc", "abc")), topo: sampleTopology()},
		{name: "d33 collision → service", vec: sampleVec(connString("http://payments.shop.svc.cluster.local:8080", "pay0", "pay0")), topo: sampleTopologyWithServices()},
		{name: "peer-resolved pod ip", vec: sampleVec(podIPPeerSample("10.244.1.9")), topo: sampleTopologyPodIP()},
		{name: "peer-resolved cluster ip", vec: sampleVec(podIPPeerSample("10.0.0.5")), topo: sampleTopologyWithServices()},
		{
			name: "route hit ingress chain", vec: sampleVec(unknownPeerSample("api.example.com", nil)),
			topo: sampleTopologyWithIngress(), routes: routeIndex{chainKey: chainHitEntry()},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := parseServiceGraphREDRoutes(tc.vec, nil, nil, tc.topo, tc.routes)

			named := map[string]struct{}{}
			for _, p := range tc.topo.Pods {
				named[p.ID()] = struct{}{}
			}
			for _, p := range res.SynthPods {
				named[p.ID()] = struct{}{}
			}
			for _, sv := range res.ServiceNodes {
				named[sv.ID()] = struct{}{}
			}
			// The chain's entry hop is the ONE named-endpoint edge that must
			// stay unmeasured: it re-projects the caller→backend call.
			entryHops := map[string]struct{}{}
			for _, e := range res.Edges {
				if e.Labels["role"] != "" { // never set on edges; guard against drift
					t.Fatalf("edge carries a role label: %v", e.Labels)
				}
			}
			for _, sv := range res.ServiceNodes {
				if sv.Labels()["role"] == roleIngressGateway {
					entryHops[sv.ID()] = struct{}{}
				}
			}

			for _, e := range res.Edges {
				if e.Metrics == nil {
					continue
				}
				assert.NotEqualf(t, graph.EdgeTypeServiceSelectsPod, e.Type,
					"metrics on a synthesised fan-out edge %s → %s", e.Source, e.Target)
				_, srcNamed := named[e.Source]
				_, tgtNamed := named[e.Target]
				assert.Truef(t, srcNamed, "metrics source %s is neither a pod nor a service node", e.Source)
				assert.Truef(t, tgtNamed, "metrics target %s is neither a pod nor a service node", e.Target)
				_, isEntry := entryHops[e.Target]
				assert.Falsef(t, isEntry, "metrics on the ingress chain's entry hop %s → %s", e.Source, e.Target)
			}
		})
	}
}
