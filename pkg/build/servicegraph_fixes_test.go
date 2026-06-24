package build

import (
	"math"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/graph"
)

// Regression tests for reviewed service-graph findings:
//  1. NaN-valued rate samples must be dropped like zero-rate samples.
//  2. classifyK8sDNS must strip the bare ".svc" suffix before the
//     cluster-domain ".svc." strip (which takes the LAST occurrence) so
//     namespaces / services literally named "svc" resolve in both forms.
//  3. pod-calls-* pair dedupe must be order-independent (D6): on a duplicate
//     (src, tgt) pair an identified srcCluster beats "unknown", then the
//     lexically-smaller name wins.

// svcNamedTopology is a D29 topology where "svc" is used as a literal
// namespace name and as a literal service name (both legal DNS-1123 labels):
//   - service "myservice" in namespace "svc"  (ClusterIP 10.0.0.7)
//   - headless service "svc" in namespace "myns" (ClusterIP None) → pod b0
//   - service "myservice" in namespace "myns" (ClusterIP 10.0.0.8) — the
//     normal-name regression guard
func svcNamedTopology() Topology {
	client := &graph.PodNode{
		IDValue:     "cluster-alpha/abc",
		NameValue:   "checkout",
		LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"},
	}
	back0 := &graph.PodNode{
		IDValue:     "cluster-alpha/b0",
		NameValue:   "svc-0",
		LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "myns"},
	}
	return Topology{
		Pods:      []*graph.PodNode{client, back0},
		PodsByUID: map[string]*graph.PodNode{"abc": client, "b0": back0},
		ServicesByNameNS: map[serviceKey]ServiceObs{
			{"cluster-alpha", "svc", "myservice"}:  {ClusterIP: "10.0.0.7"},
			{"cluster-alpha", "myns", "svc"}:       {ClusterIP: "None"},
			{"cluster-alpha", "myns", "myservice"}: {ClusterIP: "10.0.0.8"},
		},
		EndpointsByService: map[serviceKey][]EndpointObs{
			{"cluster-alpha", "myns", "svc"}: {{Pod: back0}},
		},
	}
}

// Finding 1: NaN never compares true against anything, so the previous
// `s.Value <= 0` guard let NaN-valued series materialise nodes and edges for
// traffic that never happened.
func TestParseServiceGraph_DropsNaNRate(t *testing.T) {
	vec := sampleVec(
		// Would otherwise yield a pod-calls-pod edge between two known pods.
		model.Sample{
			Metric: model.Metric{
				"client":             "checkout",
				"server":             "payments",
				"cluster":            "cluster-alpha",
				"client_k8s_pod_uid": "abc",
				"server_k8s_pod_uid": "def",
			},
			Value: model.SampleValue(math.NaN()),
		},
		// Would otherwise mint a synth pod, an external node, and an edge.
		model.Sample{
			Metric: model.Metric{
				"client":             "ghost",
				"server":             "http://elsewhere.example.com",
				"cluster":            "cluster-alpha",
				"client_k8s_pod_uid": "ghost-uid",
			},
			Value: model.SampleValue(math.NaN()),
		},
	)
	res := parseServiceGraph(vec, sampleTopology())
	assert.Empty(t, res.Edges, "NaN-rate series must not produce edges")
	assert.Empty(t, res.ServiceNodes, "NaN-rate series must not materialise service nodes")
	assert.Empty(t, res.ExternalNodes, "NaN-rate series must not materialise external nodes")
	assert.Empty(t, res.SynthPods, "NaN-rate series must not mint synth pods")
}

// Finding 2 (a): a namespace literally named "svc" —
// "myservice.svc.svc.cluster.local" must strip the LAST ".svc." (the
// cluster-domain suffix) and resolve service "myservice" in namespace "svc",
// not truncate at the first ".svc." and fall back to an external node.
func TestParseServiceGraph_ConnString_NamespaceNamedSvc_ResolvesToServiceNode(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "http://myservice.svc.svc.cluster.local:8080",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
		},
		Value: 4,
	})
	res := parseServiceGraph(vec, svcNamedTopology())

	wantID := graph.ServiceID("cluster-alpha", "svc", "myservice")
	require.Len(t, res.ServiceNodes, 1, "expected the real service node, not an external fallback")
	assert.Equal(t, wantID, res.ServiceNodes[0].ID())
	assert.Empty(t, res.ExternalNodes)

	calls := edgesByType(res, graph.EdgeTypePodCallsService)
	require.Len(t, calls, 1)
	assert.Equal(t, "cluster-alpha/abc", calls[0].Source)
	assert.Equal(t, wantID, calls[0].Target)
}

// Finding 2 (b): a headless per-pod record for a service literally named
// "svc" — "pod-0.svc.myns.svc.cluster.local" must resolve to service "svc" in
// namespace "myns" (with its service-selects-pod fan-out), not truncate at the
// first ".svc." and become external.
func TestParseServiceGraph_ConnString_HeadlessServiceNamedSvc_ResolvesToServiceNode(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "http://pod-0.svc.myns.svc.cluster.local",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
		},
		Value: 4,
	})
	res := parseServiceGraph(vec, svcNamedTopology())

	wantID := graph.ServiceID("cluster-alpha", "myns", "svc")
	require.Len(t, res.ServiceNodes, 1, "expected the real service node, not an external fallback")
	assert.Equal(t, wantID, res.ServiceNodes[0].ID())
	assert.Empty(t, res.ExternalNodes)

	selects := edgesByType(res, graph.EdgeTypeServiceSelectsPod)
	require.Len(t, selects, 1, "headless service fans out to its backing pod")
	assert.Equal(t, wantID, selects[0].Source)
	assert.Equal(t, "cluster-alpha/b0", selects[0].Target)
}

// Finding 2 (c): regression guard — the everyday
// "<service>.<namespace>.svc.cluster.local" form keeps resolving after the
// Index → LastIndex change.
func TestParseServiceGraph_ConnString_NormalServiceName_StillResolves(t *testing.T) {
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "http://myservice.myns.svc.cluster.local:8080",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
		},
		Value: 4,
	})
	res := parseServiceGraph(vec, svcNamedTopology())

	wantID := graph.ServiceID("cluster-alpha", "myns", "myservice")
	require.Len(t, res.ServiceNodes, 1)
	assert.Equal(t, wantID, res.ServiceNodes[0].ID())
	assert.Empty(t, res.ExternalNodes)
}

// Finding 2: direct grammar coverage of the LAST-".svc." suffix rule.
func TestClassifyK8sDNS_SvcLiteralLabels(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		wantSvc string
		wantNS  string
		wantOK  bool
	}{
		{"namespace named svc", "myservice.svc.svc.cluster.local", "myservice", "svc", true},
		{"headless service named svc", "pod-0.svc.myns.svc.cluster.local", "svc", "myns", true},
		{"normal service form", "myservice.myns.svc.cluster.local", "myservice", "myns", true},
		{"normal headless form", "pod-0.myservice.myns.svc.cluster.local", "myservice", "myns", true},
		{"bare .svc suffix", "myservice.myns.svc", "myservice", "myns", true},
		// Bare-suffix forms with a namespace literally named "svc": the bare
		// check must run before the ".svc." cluster-domain strip, or the
		// interior ".svc." truncates the name too early.
		{"bare suffix, namespace named svc", "myservice.svc.svc", "myservice", "svc", true},
		{"bare headless, namespace named svc", "pod-0.myservice.svc.svc", "myservice", "svc", true},
		{"one label is not a k8s name", "localhost", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, ns, ok := classifyK8sDNS(tc.host)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantSvc, svc)
			assert.Equal(t, tc.wantNS, ns)
		})
	}
}

// Finding 3: two series resolving to the same (src, tgt) pair but carrying
// different trace `cluster` labels (one missing → "unknown", the client pod
// recovered via the cluster-agnostic UID index) must yield the same
// labels.cluster regardless of vector arrival order (D6) — an identified
// cluster beats "unknown", then the lexically-smaller name wins.
func TestParseServiceGraph_DupPairClusterTieBreak_OrderIndependent(t *testing.T) {
	withCluster := model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "payments",
			"cluster":            "cluster-alpha",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "def",
		},
		Value: 5,
	}
	// No `cluster` label → bucketed to "unknown"; the client pod is still
	// recovered as cluster-alpha/abc via the global UID index, so both samples
	// dedupe onto the same (src, tgt) pair.
	noCluster := model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "payments",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "def",
		},
		Value: 5,
	}

	orders := map[string]model.Vector{
		"with-cluster-first": sampleVec(withCluster, noCluster),
		"no-cluster-first":   sampleVec(noCluster, withCluster),
	}
	for name, vec := range orders {
		t.Run(name, func(t *testing.T) {
			res := parseServiceGraph(vec, sampleTopology())
			require.Len(t, res.Edges, 1)
			e := res.Edges[0]
			assert.Equal(t, "cluster-alpha/abc", e.Source)
			assert.Equal(t, "cluster-beta/def", e.Target)
			assert.Equal(t, "cluster-alpha", e.Labels["cluster"],
				"labels.cluster must be the identified srcCluster in every arrival order")
		})
	}
}

// The identified cluster must win the duplicate-pair tie-break even when its
// name sorts lexically AFTER "unknown" (AWS-style "us-*" names): a plain
// lexical pick would deterministically degrade labels.cluster to the
// missing-label bucket whenever an unlabelled sibling series exists.
func TestParseServiceGraph_DupPairClusterTieBreak_UnknownNeverBeatsRealCluster(t *testing.T) {
	clientPod := &graph.PodNode{
		IDValue:     "us-east-1/abc",
		NameValue:   "checkout",
		LabelsValue: map[string]string{"cluster": "us-east-1", "namespace": "shop"},
	}
	serverPod := &graph.PodNode{
		IDValue:     "us-west-2/def",
		NameValue:   "payments",
		LabelsValue: map[string]string{"cluster": "us-west-2", "namespace": "billing"},
	}
	topo := Topology{
		Pods:      []*graph.PodNode{clientPod, serverPod},
		PodsByUID: map[string]*graph.PodNode{"abc": clientPod, "def": serverPod},
	}

	withCluster := model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "payments",
			"cluster":            "us-east-1",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "def",
		},
		Value: 5,
	}
	noCluster := model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"server":             "payments",
			"client_k8s_pod_uid": "abc",
			"server_k8s_pod_uid": "def",
		},
		Value: 5,
	}

	orders := map[string]model.Vector{
		"with-cluster-first": sampleVec(withCluster, noCluster),
		"no-cluster-first":   sampleVec(noCluster, withCluster),
	}
	for name, vec := range orders {
		t.Run(name, func(t *testing.T) {
			res := parseServiceGraph(vec, topo)
			require.Len(t, res.Edges, 1)
			e := res.Edges[0]
			assert.Equal(t, "us-east-1", e.Labels["cluster"],
				`"unknown" must never beat an identified srcCluster, whatever the lexical order`)
		})
	}
}
