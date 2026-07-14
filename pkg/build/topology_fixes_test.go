package build

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// TestParseTopology_PVCBindingDeduped — a pod mounting one claim through two
// volume names (or a restarted pod / HA-KSM duplicate series) emits multiple
// kube_pod_spec_volumes_persistentvolumeclaims_info samples for the same
// (pod, claim) pair. Bindings must be deduped by (PodID, PVCID): duplicate
// bindings would yield duplicate pod-mounts-pvc edges sharing one UUIDv5 ID.
func TestParseTopology_PVCBindingDeduped(t *testing.T) {
	podVec := sampleVec(model.Sample{Metric: model.Metric{
		"cluster": "c-a", "namespace": "db", "pod": "mongo-0", "uid": "u1", "node": "w0",
	}})
	binding := func(volume string) model.Sample {
		return model.Sample{Metric: model.Metric{
			"cluster": "c-a", "namespace": "db", "pod": "mongo-0",
			"persistentvolumeclaim": "data-mongo-0",
			"volume":                model.LabelValue(volume),
		}}
	}
	tp := parseTopology(topologyVectors{
		Pod: podVec,
		PVC: sampleVec(binding("data"), binding("data-again")),
	})
	require.Len(t, tp.PVCs, 1, "one PVC node per claim")
	require.Len(t, tp.PodPVCs, 1,
		"two samples for the same (pod, claim) must collapse to exactly one binding")
	assert.Equal(t, "c-a/u1", tp.PodPVCs[0].PodID)
	assert.Equal(t, "c-a/db/data-mongo-0", tp.PodPVCs[0].PVCID)
}

// TestParseTopology_K8sNodeClusterLabelNotClobbered — an operator node label
// `cluster=staging` flattens to kube_node_labels{label_cluster="staging"} and
// unflattenLabel("label_cluster") == "cluster", colliding with the contract
// key. The contract value (the upstream `cluster` series label) must win.
func TestParseTopology_K8sNodeClusterLabelNotClobbered(t *testing.T) {
	nodeVec := sampleVec(model.Sample{Metric: model.Metric{
		"cluster": "prod", "node": "worker-0",
	}})
	labelVec := sampleVec(model.Sample{Metric: model.Metric{
		"cluster":       "prod",
		"node":          "worker-0",
		"label_cluster": "staging", // operator label colliding with the contract key
		"label_app":     "ingress",
	}})
	tp := parseTopology(topologyVectors{Node: nodeVec, NodeLabels: labelVec})
	require.Len(t, tp.Nodes, 1)
	labels := tp.Nodes[0].Labels()
	assert.Equal(t, "prod", labels["cluster"],
		"contract labels.cluster must not be clobbered by an operator node label")
	assert.Equal(t, "ingress", labels["app"], "non-colliding KSM labels still merge")
}

// TestParseTopology_K8sNodeDeduped — kube_node_info can return more than one
// series for the same (cluster, node): two KSM scrape targets (HA, or a rollout
// still inside the last_over_time window) carry different instance/pod target
// labels, and a kubelet / OS upgrade churns kubelet_version / os_image within
// the window. Every node attribute (labels, IP, ready_status) is sourced from
// (cluster, node)-keyed join maps, so duplicate series describe the identical
// node — they MUST collapse to one K8sNode here rather than minting same-ID
// duplicates that flood NewGraph with "duplicate node ID" warnings. Distinct
// nodes must still survive.
func TestParseTopology_K8sNodeDeduped(t *testing.T) {
	nodeVec := sampleVec(
		model.Sample{Metric: model.Metric{
			"cluster": "prod", "node": "worker-0",
			"kubelet_version": "v1.29.4", "instance": "10.0.0.1:8080",
		}, Value: 1},
		model.Sample{Metric: model.Metric{
			"cluster": "prod", "node": "worker-0",
			"kubelet_version": "v1.29.5", "instance": "10.0.0.2:8080",
		}, Value: 1},
		model.Sample{Metric: model.Metric{
			"cluster": "prod", "node": "worker-1",
		}, Value: 1},
	)
	condVec := sampleVec(model.Sample{Metric: model.Metric{
		"cluster": "prod", "node": "worker-0", "condition": "Ready", "status": "true",
	}, Value: 1})

	tp := parseTopology(topologyVectors{Node: nodeVec, NodeStatus: condVec})

	require.Len(t, tp.Nodes, 2,
		"duplicate (cluster,node) series must collapse to one node; distinct nodes survive")

	byID := map[string]*graph.K8sNode{}
	for _, n := range tp.Nodes {
		_, dup := byID[n.ID()]
		require.False(t, dup, "no duplicate node IDs may leave parseTopology: %s", n.ID())
		byID[n.ID()] = n
	}
	w0 := byID["prod/worker-0"]
	require.NotNil(t, w0, "the deduped node survives")
	assert.Equal(t, graph.ReadyStatusReady, w0.ReadyStatus(),
		"dedup must not drop the (cluster,node)-keyed ready_status")
	assert.Equal(t, "prod", w0.Labels()["cluster"])
	require.NotNil(t, byID["prod/worker-1"], "the distinct node is not collapsed")
}

// TestParseTopology_PodIPNotInheritedFromOldUID — when a pod is recreated
// (same name, new UID), the IP fallback must only consider samples of the
// canonical (newest) UID. A pod_ip observed on the dead predecessor UID must
// NOT leak onto the new pod.
func TestParseTopology_PodIPNotInheritedFromOldUID(t *testing.T) {
	sample := func(uid, podIP string, ts model.Time) model.Sample {
		m := model.Metric{
			"cluster": "c-a", "namespace": "shop", "pod": "checkout",
			"uid": model.LabelValue(uid), "node": "w0",
		}
		if podIP != "" {
			m["pod_ip"] = model.LabelValue(podIP)
		}
		return model.Sample{Metric: m, Value: 1, Timestamp: ts}
	}
	tp := parseTopology(topologyVectors{Pod: sampleVec(
		sample("uid-old", "10.244.0.1", 100), // dead predecessor, has an IP
		sample("uid-new", "", 200),           // canonical UID, no IP yet
	)})
	require.Len(t, tp.Pods, 1)
	assert.Equal(t, "c-a/uid-new", tp.Pods[0].ID())
	assert.Nil(t, tp.Pods[0].IPAddress(),
		"the predecessor UID's pod_ip must not leak onto the recreated pod")

	// The fallback must still work WITHIN the canonical UID: an older
	// same-UID sample carrying the IP fills in for a newer empty one.
	tp2 := parseTopology(topologyVectors{Pod: sampleVec(
		sample("uid-new", "10.244.0.9", 150),
		sample("uid-new", "", 200),
	)})
	require.Len(t, tp2.Pods, 1)
	assert.Equal(t, []string{"10.244.0.9"}, tp2.Pods[0].IPAddress(),
		"same-UID fallback to the most recent non-empty pod_ip must survive")
}

// TestParseTopology_PVCVolumeLabelDeterministic — two binding samples for the
// same PVC id carrying different `volume` labels must resolve to the
// lexically-smallest volume regardless of upstream vector order (D6).
func TestParseTopology_PVCVolumeLabelDeterministic(t *testing.T) {
	binding := func(volume string) model.Sample {
		return model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "n", "pod": "p",
			"persistentvolumeclaim": "claim",
			"volume":                model.LabelValue(volume),
		}}
	}
	fwd := parseTopology(topologyVectors{PVC: sampleVec(binding("vol-b"), binding("vol-a"))})
	rev := parseTopology(topologyVectors{PVC: sampleVec(binding("vol-a"), binding("vol-b"))})
	require.Len(t, fwd.PVCs, 1)
	require.Len(t, rev.PVCs, 1)
	assert.Equal(t, "vol-a", fwd.PVCs[0].Labels()["volume"], "lexically-smallest volume wins")
	assert.Equal(t, fwd.PVCs[0].Labels()["volume"], rev.PVCs[0].Labels()["volume"], "order-independent")
}

// TestParseTopology_ExternalIPDeterministic — two ExternalIP address samples
// for the same (cluster, node) must resolve to the lexically-smallest address
// regardless of upstream vector order (D6).
func TestParseTopology_ExternalIPDeterministic(t *testing.T) {
	addr := func(a string) model.Sample {
		return model.Sample{Metric: model.Metric{
			"cluster": "c", "node": "w0", "type": "ExternalIP",
			"address": model.LabelValue(a),
		}}
	}
	nodeVec := sampleVec(model.Sample{Metric: model.Metric{"cluster": "c", "node": "w0"}})
	fwd := parseTopology(topologyVectors{Node: nodeVec, Addr: sampleVec(addr("203.0.113.9"), addr("203.0.113.10"))})
	rev := parseTopology(topologyVectors{Node: nodeVec, Addr: sampleVec(addr("203.0.113.10"), addr("203.0.113.9"))})
	require.Len(t, fwd.Nodes, 1)
	require.Len(t, rev.Nodes, 1)
	assert.Equal(t, []string{"203.0.113.10"}, fwd.Nodes[0].IPAddress(),
		"lexically-smallest address wins") // "203.0.113.10" < "203.0.113.9" lexically
	assert.Equal(t, fwd.Nodes[0].IPAddress(), rev.Nodes[0].IPAddress(), "order-independent")
}

// TestParseTopology_InternalIPFallback — a node with only InternalIP rows
// surfaces the InternalIP on IPAddress; ExternalIP wins whenever present,
// regardless of upstream vector order; with neither type no IP is emitted;
// duplicate samples within a type resolve lexically-smallest (D6).
func TestParseTopology_InternalIPFallback(t *testing.T) {
	addr := func(typ, a string) model.Sample {
		return model.Sample{Metric: model.Metric{
			"cluster": "c", "node": "w0",
			"type":    model.LabelValue(typ),
			"address": model.LabelValue(a),
		}}
	}
	nodeVec := sampleVec(model.Sample{Metric: model.Metric{"cluster": "c", "node": "w0"}})

	t.Run("internal-only falls back", func(t *testing.T) {
		tp := parseTopology(topologyVectors{Node: nodeVec, Addr: sampleVec(addr("InternalIP", "10.0.0.7"))})
		require.Len(t, tp.Nodes, 1)
		assert.Equal(t, []string{"10.0.0.7"}, tp.Nodes[0].IPAddress())
		_, hasInternalIP := tp.Nodes[0].Labels()["internal_ip"]
		assert.False(t, hasInternalIP, "internal_ip must not appear in labels")
	})

	t.Run("external wins order-independently", func(t *testing.T) {
		fwd := parseTopology(topologyVectors{Node: nodeVec, Addr: sampleVec(addr("InternalIP", "10.0.0.7"), addr("ExternalIP", "203.0.113.10"))})
		rev := parseTopology(topologyVectors{Node: nodeVec, Addr: sampleVec(addr("ExternalIP", "203.0.113.10"), addr("InternalIP", "10.0.0.7"))})
		require.Len(t, fwd.Nodes, 1)
		require.Len(t, rev.Nodes, 1)
		assert.Equal(t, []string{"203.0.113.10"}, fwd.Nodes[0].IPAddress())
		assert.Equal(t, fwd.Nodes[0].IPAddress(), rev.Nodes[0].IPAddress(), "order-independent")
	})

	t.Run("duplicate internal samples pick lexically-smallest", func(t *testing.T) {
		fwd := parseTopology(topologyVectors{Node: nodeVec, Addr: sampleVec(addr("InternalIP", "10.0.0.9"), addr("InternalIP", "10.0.0.10"))})
		rev := parseTopology(topologyVectors{Node: nodeVec, Addr: sampleVec(addr("InternalIP", "10.0.0.10"), addr("InternalIP", "10.0.0.9"))})
		require.Len(t, fwd.Nodes, 1)
		assert.Equal(t, []string{"10.0.0.10"}, fwd.Nodes[0].IPAddress(),
			"lexically-smallest address wins") // "10.0.0.10" < "10.0.0.9" lexically
		assert.Equal(t, fwd.Nodes[0].IPAddress(), rev.Nodes[0].IPAddress(), "order-independent")
	})

	t.Run("non-IP address types ignored", func(t *testing.T) {
		tp := parseTopology(topologyVectors{Node: nodeVec, Addr: sampleVec(addr("Hostname", "w0.example.internal"))})
		require.Len(t, tp.Nodes, 1)
		assert.Nil(t, tp.Nodes[0].IPAddress(), "Hostname rows must never reach ipaddress")
	})

	t.Run("no address rows omits IP", func(t *testing.T) {
		tp := parseTopology(topologyVectors{Node: nodeVec})
		require.Len(t, tp.Nodes, 1)
		assert.Nil(t, tp.Nodes[0].IPAddress())
	})
}

// TestParseTopology_NodeLabelMergeDeterministic — two kube_node_labels series
// for the same node disagreeing on a label key must resolve to the
// lexically-smallest value regardless of upstream vector order (D6).
func TestParseTopology_NodeLabelMergeDeterministic(t *testing.T) {
	series := func(zone string) model.Sample {
		return model.Sample{Metric: model.Metric{
			"cluster": "c", "node": "w0",
			"label_topology_kubernetes_io_zone": model.LabelValue(zone),
		}}
	}
	nodeVec := sampleVec(model.Sample{Metric: model.Metric{"cluster": "c", "node": "w0"}})
	fwd := parseTopology(topologyVectors{Node: nodeVec, NodeLabels: sampleVec(series("us-east-1a"), series("us-east-1b"))})
	rev := parseTopology(topologyVectors{Node: nodeVec, NodeLabels: sampleVec(series("us-east-1b"), series("us-east-1a"))})
	require.Len(t, fwd.Nodes, 1)
	require.Len(t, rev.Nodes, 1)
	assert.Equal(t, "us-east-1a", fwd.Nodes[0].Labels()["topology.kubernetes.io/zone"],
		"lexically-smallest value wins on conflict")
	assert.Equal(t,
		fwd.Nodes[0].Labels()["topology.kubernetes.io/zone"],
		rev.Nodes[0].Labels()["topology.kubernetes.io/zone"],
		"order-independent")
}
