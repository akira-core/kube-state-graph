package build

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// TestParseTopology_DuplicateUIDAcrossClusters_DeterministicWinner guards N2:
// when two pods in different clusters share a raw UID (data anomaly), the pod
// indexed in PodsByUID must be a pure function of the data — the
// lexically-smaller cluster-scoped ID — not the randomised map-iteration order
// addPodToIndex runs in. The service-graph reader resolves pod-calls-pod edge
// targets through this index, so an unstable winner would break the D6
// byte-identical response contract.
func TestParseTopology_DuplicateUIDAcrossClusters_DeterministicWinner(t *testing.T) {
	pod := func(cluster string) model.Sample {
		return model.Sample{
			Metric: model.Metric{
				"cluster":   model.LabelValue(cluster),
				"namespace": "shop",
				"pod":       "checkout",
				"uid":       "shared-uid",
				"node":      "worker-0",
			},
			Value:     1,
			Timestamp: 100,
		}
	}
	vec := sampleVec(pod("cluster-b"), pod("cluster-a"))

	// Run many times: with only two map entries, randomised iteration order
	// exercises both insertion orders within a handful of runs.
	const runs = 64
	for i := range runs {
		tp := parseTopology(topologyVectors{Pod: vec}, promql.LabelKeys{})
		require.Len(t, tp.Pods, 2, "both colliding pods remain as distinct nodes")
		winner := tp.PodsByUID["shared-uid"]
		require.NotNil(t, winner)
		assert.Equalf(t, "cluster-a/shared-uid", winner.ID(),
			"PodsByUID winner must be the lexically-smaller ID on every run (run %d)", i)
	}
}

// TestParseTopology_EqualTimestampUIDChurn_DeterministicCanonical guards N8:
// when a (cluster, namespace, pod) churns UIDs at the SAME timestamp, the
// canonical pod must be a pure function of the data (lexically-larger UID),
// independent of vector arrival order — VictoriaMetrics does not guarantee a
// stable vector order across identical queries.
func TestParseTopology_EqualTimestampUIDChurn_DeterministicCanonical(t *testing.T) {
	pod := func(uid string) model.Sample {
		return model.Sample{
			Metric: model.Metric{
				"cluster":   "cluster-alpha",
				"namespace": "shop",
				"pod":       "checkout",
				"uid":       model.LabelValue(uid),
				"node":      "worker-0",
			},
			Value:     1,
			Timestamp: 200, // identical timestamp for both UIDs
		}
	}
	// Both arrival orders must collapse to the same canonical UID.
	for _, order := range [][]model.Sample{
		{pod("uid-a"), pod("uid-b")},
		{pod("uid-b"), pod("uid-a")},
	} {
		tp := parseTopology(topologyVectors{Pod: sampleVec(order...)}, promql.LabelKeys{})
		require.Len(t, tp.Pods, 1, "UID churn collapses to one canonical pod")
		assert.Equal(t, "cluster-alpha/uid-b", tp.Pods[0].ID(),
			"equal-timestamp tie-break must pick the lexically-larger UID regardless of arrival order")
	}
}

// TestParseServiceGraph_SynthPodNamespace_DeterministicOnConflict guards N7:
// when the same unknown server UID arrives in two series with conflicting
// namespace labels, the synthesised pod's namespace must be a pure function of
// the data (lexically-smaller), not vector arrival order.
func TestParseServiceGraph_SynthPodNamespace_DeterministicOnConflict(t *testing.T) {
	series := func(serverNS string) model.Sample {
		return model.Sample{
			Metric: model.Metric{
				"cluster":                   "cluster-alpha",
				"client":                    "checkout",
				"client_k8s_pod_uid":        "abc", // known pod in sampleTopology
				"server":                    "ghost-workload",
				"server_k8s_pod_uid":        "ghost-uid", // unknown → synth pod, cluster ""
				"server_k8s_namespace_name": model.LabelValue(serverNS),
			},
			Value: 1,
		}
	}
	for _, order := range [][]model.Sample{
		{series("billing"), series("apps")},
		{series("apps"), series("billing")},
	} {
		res := parseServiceGraph(sampleVec(order...), sampleTopology())
		require.Len(t, res.SynthPods, 1, "one synth pod for the unknown server UID")
		assert.Equal(t, "apps", res.SynthPods[0].Labels()["namespace"],
			"conflicting-namespace synth pod must keep the lexically-smaller namespace regardless of arrival order")
	}
}

// TestParseTopology_UnstampedIsByteIdentical is the in-package half of the
// compatibility claim: an estate that stamps no az/env pair must parse to the
// SAME Topology it did before cluster identities existed. The identity ladder
// composes nothing, adopts nothing (the table is empty), and every name stands
// verbatim — so ids, labels, join keys and the observed-cluster list are the
// raw-name shapes, and no identity table reaches the graph.
func TestParseTopology_UnstampedIsByteIdentical(t *testing.T) {
	buf := captureLogs(t)

	v := topologyVectors{
		Pod: sampleVec(
			sample(map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "pod": "checkout", "uid": "uid-1", "node": "worker-0"}),
			sample(map[string]string{"cluster": "cluster-beta", "namespace": "billing", "pod": "payments", "uid": "uid-2", "node": "worker-1"}),
		),
		Node: sampleVec(
			sample(map[string]string{"cluster": "cluster-alpha", "node": "worker-0"}),
			sample(map[string]string{"cluster": "cluster-beta", "node": "worker-1"}),
		),
		Service: sampleVec(sample(map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "service": "checkout", "cluster_ip": "10.0.0.1"})),
		PVC: sampleVec(sample(map[string]string{
			"cluster": "cluster-alpha", "namespace": "shop", "pod": "checkout", "volume": "data", "claim_name": "checkout-data",
		})),
		PodOwner: sampleVec(sample(map[string]string{
			"cluster": "cluster-alpha", "namespace": "shop", "pod": "checkout",
			"owner_kind": "Deployment", "owner_name": "checkout", "owner_is_controller": "true",
		})),
		KubeletVolumeUsed: sampleVec(sample(map[string]string{
			"cluster": "cluster-alpha", "namespace": "shop", "persistentvolumeclaim": "checkout-data",
		})),
	}

	tp := parseTopology(v, promql.LabelKeys{})

	assert.Nil(t, tp.ClusterIdentities, "an unstamped estate composes no identity table")
	assert.Equal(t, []string{"cluster-alpha", "cluster-beta"}, tp.ClustersObserved)

	ids := map[string]bool{}
	for _, p := range tp.Pods {
		ids[p.ID()] = true
		assert.NotContains(t, p.Labels()["cluster"], "-unknown")
	}
	assert.True(t, ids["cluster-alpha/uid-1"])
	assert.True(t, ids["cluster-beta/uid-2"])

	require.Len(t, tp.Nodes, 2)
	require.Len(t, tp.PVCs, 1)
	assert.Equal(t, "cluster-alpha/shop/checkout-data", tp.PVCs[0].ID())
	assert.NotNil(t, tp.PVCs[0].Usage(), "the kubelet join is unaffected")
	assert.Contains(t, tp.ServicesByNameNS, serviceKey{"cluster-alpha", "shop", "checkout"})

	// Every join input resolved against the raw name, so the owner is attached
	// and nothing was reported as unplaceable.
	for _, p := range tp.Pods {
		if p.ID() == "cluster-alpha/uid-1" {
			require.NotNil(t, p.Owner())
			assert.Equal(t, "Deployment", p.Owner().Kind)
		}
	}
	assert.NotContains(t, buf.String(), "cluster_identity_unresolved")
}
