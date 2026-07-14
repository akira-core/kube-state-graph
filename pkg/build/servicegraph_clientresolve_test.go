package build

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// TestParseServiceGraph_ClientUID_RecoveredViaIndexWhenClusterLabelMissing
// guards N3: when a service-graph series omits the `cluster` label (common with
// Beyla/Alloy/Tempo exporters), the client-side cluster-scoped podByID lookup
// misses. The resolver must fall back to the global UID index — symmetric with
// the server side — and return the REAL pod instead of minting a duplicate
// "unknown/<uid>" ghost.
func TestParseServiceGraph_ClientUID_RecoveredViaIndexWhenClusterLabelMissing(t *testing.T) {
	// No `cluster` label → traceCluster buckets to "unknown"; client UID "abc"
	// and server UID "def" both live in sampleTopology's global UID index.
	vec := sampleVec(model.Sample{
		Metric: model.Metric{
			"client":             "checkout",
			"client_k8s_pod_uid": "abc",
			"server":             "payments",
			"server_k8s_pod_uid": "def",
		},
		Value: 1,
	})

	res := parseServiceGraph(vec, sampleTopology())

	assert.Empty(t, res.SynthPods, "client pod must be recovered via the UID index, not synthesised as a ghost")

	pp := edgesByType(res, graph.EdgeTypePodCallsPod)
	require.Len(t, pp, 1)
	assert.Equal(t, "cluster-alpha/abc", pp[0].Source,
		"client endpoint must resolve to its real cluster-scoped pod ID")
	assert.Equal(t, "cluster-beta/def", pp[0].Target,
		"server endpoint resolves via the same global UID index")
}
