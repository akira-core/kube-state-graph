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
	if len(vec) == 0 {
		return ServiceGraphResult{}
	}
	return parseWithResolver(vec, newSGResolver(topology), nil, redInputs{
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

// bucketSample is one pre-aggregated duration histogram bucket.
func bucketSample(clientUID, serverUID, le string, count float64) model.Sample {
	return model.Sample{
		Metric: model.Metric{
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": model.LabelValue(clientUID),
			"server_k8s_pod_uid": model.LabelValue(serverUID),
			"le":                 model.LabelValue(le),
		},
		Value: model.SampleValue(count),
	}
}

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
	require.NotNil(t, m.ErrorRate)
	assert.InDelta(t, 0.0, *m.ErrorRate, 1e-12, "failed query succeeded with no match → 0")
	assert.Nil(t, m.P90ServerMs)
}

func TestRED_PeerResolvedPodIP_NoMetrics(t *testing.T) {
	// server="unknown", empty server UID, peer address resolves to a Pod IP.
	// Edge is pod-calls-pod but peer-resolved → no metrics (D1).
	res := parseServiceGraph(sampleVec(podIPPeerSample("10.244.1.9")), sampleTopologyPodIP())
	require.NotEmpty(t, res.Edges)
	var found bool
	for _, e := range res.Edges {
		if e.Type == graph.EdgeTypePodCallsPod {
			found = true
			assert.Nil(t, e.Metrics, "peer-resolved Pod-IP target must carry no metrics")
		}
	}
	assert.True(t, found, "expected a pod-calls-pod edge from Pod-IP resolution")
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

func TestRED_PodCallsService_NoMetrics(t *testing.T) {
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
	for _, e := range res.Edges {
		if e.Type == graph.EdgeTypePodCallsService {
			assert.Nil(t, e.Metrics)
		}
	}
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
	e = graph.NewEdge(graph.EdgeTypePVCToStorageClass, "a", "b", nil)
	assert.Nil(t, e.Metrics)
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

func TestRED_MixedInScopeOutOfScope_NoMetrics(t *testing.T) {
	// One UID-resolved series + one peer-resolved series collapsing onto the
	// same target pod → pair is ineligible (partially measured).
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
	assert.Nil(t, m, "mixed in-scope / out-of-scope pair must carry no metrics")
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
	// Two redKeys (different cluster labels) mapping to same pair: buckets sum.
	vec := sampleVec(
		uidPodSample("abc", "def", 5, model.Metric{"cluster": "cluster-alpha"}),
		// Same UIDs, cluster missing → bucketed to "unknown"; client recovered via UID.
		// Need cluster empty and client still resolves via podByUID.
		model.Sample{
			Metric: model.Metric{
				"client": "checkout",
				"server": "payments",
				// no cluster
				"client_k8s_pod_uid": "abc",
				"server_k8s_pod_uid": "def",
			},
			Value: 5,
		},
	)
	// Buckets for cluster-alpha: total 50 at +Inf
	// Buckets for unknown: total 50 at +Inf
	// Combined: same shape doubled.
	dur := sampleVec(
		bucketSample("abc", "def", "0.1", 20),
		bucketSample("abc", "def", "0.5", 40),
		bucketSample("abc", "def", "+Inf", 50),
		// unknown cluster buckets
		model.Sample{Metric: model.Metric{
			"cluster": "unknown", "client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "def", "le": "0.1",
		}, Value: 20},
		model.Sample{Metric: model.Metric{
			"cluster": "unknown", "client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "def", "le": "0.5",
		}, Value: 40},
		model.Sample{Metric: model.Metric{
			"cluster": "unknown", "client_k8s_pod_uid": "abc", "server_k8s_pod_uid": "def", "le": "+Inf",
		}, Value: 50},
	)
	res := parseServiceGraphRED(vec, nil, dur, sampleTopology())
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	require.NotNil(t, m.P90ServerMs)
	// Combined: 0.1→40, 0.5→80, +Inf→100; rank=90 → in (0.5, +Inf] → clamp to 0.5s = 500ms
	// Wait: 0.1:20+20=40, 0.5:40+40=80, +Inf:50+50=100. rank=90. 80<90 so +Inf clamp → 500ms
	assert.InDelta(t, 500.0, *m.P90ServerMs, 1e-6)
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
		durs := []model.Sample{
			bucketSample("abc", "def", "0.1", 50),
			bucketSample("abc", "def", "0.5", 90),
			bucketSample("abc", "def", "+Inf", 100),
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

func TestRED_FailureQueryNoMatch_ErrorRateZero(t *testing.T) {
	vec := sampleVec(uidPodSample("abc", "def", 5))
	res := parseServiceGraphRED(vec, sampleVec(), nil, sampleTopology())
	m := edgeMetrics(t, res, "cluster-alpha/abc", "cluster-beta/def")
	require.NotNil(t, m)
	require.NotNil(t, m.ErrorRate)
	assert.InDelta(t, 0.0, *m.ErrorRate, 1e-12)
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
	// Series with no le label (native histogram / vmrange case).
	dur := sampleVec(model.Sample{
		Metric: model.Metric{
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "def",
			// no "le"
		},
		Value: 10,
	})
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

// TestRED_D33ClearedUIDDoesNotAttachToService is the sibling case: a RESOLVABLE
// "://" target downgrades the edge to pod-calls-service, which the attach
// already excludes by type. Pinned so a future refactor of the type check
// cannot silently start attaching metrics to service endpoints.
func TestRED_D33ClearedUIDDoesNotAttachToService(t *testing.T) {
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

	for _, e := range res.Edges {
		if e.Type != graph.EdgeTypePodCallsPod {
			assert.Nilf(t, e.Metrics, "non pod-calls-pod edge carries RED: %s", e.Type)
		}
	}
}

// TestRED_InvariantMetricsOnlyOnPodPairs is the real form of the "metrics only
// on pod↔pod edges" invariant: it drives the ACTUAL parse over a corpus of
// series shapes and checks the emitted graph, rather than seeding a graph with
// attachments that already satisfy the rule. A tautological version of this
// check cannot catch a resolution-path defect (see
// TestRED_D33ClearedUIDDoesNotAttachToExternal).
func TestRED_InvariantMetricsOnlyOnPodPairs(t *testing.T) {
	connString := func(server, clientUID, serverUID string) model.Sample {
		return model.Sample{Metric: model.Metric{
			"client": "checkout", "server": model.LabelValue(server),
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": model.LabelValue(clientUID),
			"server_k8s_pod_uid": model.LabelValue(serverUID),
		}, Value: 3}
	}

	cases := []struct {
		name string
		vec  model.Vector
		topo Topology
	}{
		{"uid-resolved pods", sampleVec(uidPodSample("abc", "def", 5)), sampleTopology()},
		{"synth pod target", sampleVec(uidPodSample("abc", "no-such-uid", 2)), sampleTopology()},
		{"d27 external", sampleVec(connString("payments.example.com", "abc", "")), sampleTopology()},
		{"conn-string service", sampleVec(connString("http://payments.shop.svc.cluster.local:8080", "abc", "")), sampleTopologyWithServices()},
		{"d33 collision → external", sampleVec(connString("redis://nowhere.invalid:6379", "abc", "abc")), sampleTopology()},
		{"d33 collision → service", sampleVec(connString("http://payments.shop.svc.cluster.local:8080", "pay0", "pay0")), sampleTopologyWithServices()},
		{"peer-resolved pod ip", sampleVec(podIPPeerSample("10.244.1.9")), sampleTopologyPodIP()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := parseServiceGraphRED(tc.vec, nil, nil, tc.topo)

			pods := map[string]struct{}{}
			for _, p := range tc.topo.Pods {
				pods[p.ID()] = struct{}{}
			}
			for _, p := range res.SynthPods {
				pods[p.ID()] = struct{}{}
			}

			for _, e := range res.Edges {
				if e.Metrics == nil {
					continue
				}
				assert.Equalf(t, graph.EdgeTypePodCallsPod, e.Type,
					"metrics on %s edge %s → %s", e.Type, e.Source, e.Target)
				_, srcPod := pods[e.Source]
				_, tgtPod := pods[e.Target]
				assert.Truef(t, srcPod, "metrics source %s is not a pod node", e.Source)
				assert.Truef(t, tgtPod, "metrics target %s is not a pod node", e.Target)
			}
		})
	}
}
