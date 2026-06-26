package build

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/graph"
)

// labelSample builds a model.Sample (value 1) from a flat label map.
func labelSample(labels map[string]string) model.Sample {
	m := model.Metric{}
	for k, v := range labels {
		m[model.LabelName(k)] = model.LabelValue(v)
	}
	return model.Sample{Metric: m, Value: 1}
}

func TestResolveStorageClassInfo_ProvisionerAndParameters(t *testing.T) {
	t.Parallel()
	vec := sampleVec(labelSample(map[string]string{
		"cluster": "cluster-alpha", "storageclass": "netapp-nas",
		"provisioner":  "csi.trident.netapp.io",
		"storagePools": "aggr1", "fsType": "nfs",
		"ClusterID": "ceph-uuid", "selector": "region=eu",
	}))
	out := resolveStorageClassInfo(vec, missingClusterCounts{})
	info := out[storageClassKey{"cluster-alpha", "netapp-nas"}]
	require.NotNil(t, info)
	assert.Equal(t, "csi.trident.netapp.io", info.Provisioner)
	assert.Equal(t, map[string]string{
		"pool": "aggr1", "fs": "nfs", "cluster_id": "ceph-uuid", "selector": "region=eu",
	}, info.Parameters)
}

func TestResolveStorageClassInfo_SourceLabelFallback(t *testing.T) {
	t.Parallel()
	// pool (not storagePools) and fsName (not fsType) are the fallback sources.
	vec := sampleVec(labelSample(map[string]string{
		"cluster": "c", "storageclass": "ceph",
		"pool": "ceph-pool", "fsName": "cephfs",
	}))
	out := resolveStorageClassInfo(vec, missingClusterCounts{})
	info := out[storageClassKey{"c", "ceph"}]
	require.NotNil(t, info)
	assert.Equal(t, "ceph-pool", info.Parameters["pool"])
	assert.Equal(t, "cephfs", info.Parameters["fs"])
}

func TestResolveStorageClassInfo_UnsetOmitted(t *testing.T) {
	t.Parallel()
	vec := sampleVec(labelSample(map[string]string{"cluster": "c", "storageclass": "gp3"}))
	out := resolveStorageClassInfo(vec, missingClusterCounts{})
	info := out[storageClassKey{"c", "gp3"}]
	require.NotNil(t, info)
	assert.Empty(t, info.Provisioner, "no provisioner label → empty")
	assert.Nil(t, info.Parameters, "no parameter labels → nil parameters")
}

func TestResolveStorageClassInfo_DeterministicCollision(t *testing.T) {
	t.Parallel()
	vec := sampleVec(
		labelSample(map[string]string{"cluster": "c", "storageclass": "gp3", "pool": "b-pool"}),
		labelSample(map[string]string{"cluster": "c", "storageclass": "gp3", "pool": "a-pool"}),
	)
	out := resolveStorageClassInfo(vec, missingClusterCounts{})
	assert.Equal(t, "a-pool", out[storageClassKey{"c", "gp3"}].Parameters["pool"],
		"lexically-smallest non-empty value wins across series")
}

func TestParseTopology_StorageClassNodeAndEdge(t *testing.T) {
	t.Parallel()
	v := topologyVectors{
		PVC: sampleVec(labelSample(map[string]string{
			"cluster": "cluster-alpha", "namespace": "db", "claim_name": "data-mongo-0",
		})),
		PVCInfo: sampleVec(labelSample(map[string]string{
			"cluster": "cluster-alpha", "namespace": "db",
			"persistentvolumeclaim": "data-mongo-0", "storageclass": "gp3",
		})),
		StorageClassInfo: sampleVec(labelSample(map[string]string{
			"cluster": "cluster-alpha", "storageclass": "gp3",
			"provisioner": "ebs.csi.aws.com", "fsType": "ext4",
		})),
	}
	tp := parseTopology(v)
	require.Len(t, tp.StorageClasses, 1)
	sc := tp.StorageClasses[0]
	assert.Equal(t, "cluster-alpha/storageclass/gp3", sc.ID())
	assert.Equal(t, graph.NodeTypeStorageClass, sc.Type())
	assert.Equal(t, map[string]string{"cluster": "cluster-alpha"}, sc.Labels())
	require.NotNil(t, sc.StorageClassInfo())
	assert.Equal(t, "ebs.csi.aws.com", sc.StorageClassInfo().Provisioner)
	assert.Equal(t, "ext4", sc.StorageClassInfo().Parameters["fs"])

	var scEdge *graph.Edge
	for _, e := range TopologyEdges(tp) {
		if e.Type == graph.EdgeTypePVCToStorageClass {
			scEdge = e
		}
	}
	require.NotNil(t, scEdge, "expected a pvc-to-storageclass edge")
	assert.Equal(t, "cluster-alpha/db/data-mongo-0", scEdge.Source)
	assert.Equal(t, "cluster-alpha/storageclass/gp3", scEdge.Target)
	assert.Empty(t, scEdge.Labels, "pvc-to-storageclass edge carries no labels")
}

func TestParseTopology_BareStorageClassWhenInfoAbsent(t *testing.T) {
	t.Parallel()
	v := topologyVectors{
		PVC: sampleVec(labelSample(map[string]string{
			"cluster": "c", "namespace": "db", "claim_name": "data-x",
		})),
		PVCInfo: sampleVec(labelSample(map[string]string{
			"cluster": "c", "namespace": "db",
			"persistentvolumeclaim": "data-x", "storageclass": "unknown-sc",
		})),
		// no StorageClassInfo → the referenced class must still materialise bare.
	}
	tp := parseTopology(v)
	require.Len(t, tp.StorageClasses, 1)
	sc := tp.StorageClasses[0]
	assert.Equal(t, "c/storageclass/unknown-sc", sc.ID())
	assert.Nil(t, sc.StorageClassInfo(), "bare node carries no provisioner/parameters")

	var found bool
	for _, e := range TopologyEdges(tp) {
		if e.Type == graph.EdgeTypePVCToStorageClass && e.Target == "c/storageclass/unknown-sc" {
			found = true
		}
	}
	assert.True(t, found, "pvc-to-storageclass edge to the bare node is still emitted")
}

func TestParseTopology_AttributedStorageClassWinsOverBare(t *testing.T) {
	t.Parallel()
	v := topologyVectors{
		PVC: sampleVec(labelSample(map[string]string{"cluster": "c", "namespace": "db", "claim_name": "x"})),
		PVCInfo: sampleVec(labelSample(map[string]string{
			"cluster": "c", "namespace": "db", "persistentvolumeclaim": "x", "storageclass": "gp3",
		})),
		StorageClassInfo: sampleVec(labelSample(map[string]string{
			"cluster": "c", "storageclass": "gp3", "provisioner": "p",
		})),
	}
	tp := parseTopology(v)
	require.Len(t, tp.StorageClasses, 1, "an attributed class referenced by a PVC must not also yield a bare dup")
	require.NotNil(t, tp.StorageClasses[0].StorageClassInfo())
	assert.Equal(t, "p", tp.StorageClasses[0].StorageClassInfo().Provisioner)
}

func TestTopologyEdges_PodToNode_ScheduledOnly(t *testing.T) {
	t.Parallel()
	scheduled := &graph.PodNode{IDValue: "c/uid-1", NameValue: "a", LabelsValue: map[string]string{"cluster": "c", "node": "c/worker-0"}}
	unscheduled := &graph.PodNode{IDValue: "c/uid-2", NameValue: "b", LabelsValue: map[string]string{"cluster": "c"}}
	tp := Topology{Pods: []*graph.PodNode{scheduled, unscheduled}}

	var p2n []*graph.Edge
	for _, e := range TopologyEdges(tp) {
		if e.Type == graph.EdgeTypePodToNode {
			p2n = append(p2n, e)
		}
	}
	require.Len(t, p2n, 1, "only the scheduled pod emits a pod-to-node edge")
	assert.Equal(t, "c/uid-1", p2n[0].Source)
	assert.Equal(t, "c/worker-0", p2n[0].Target)
	assert.Empty(t, p2n[0].Labels, "pod-to-node edge carries no labels")
}
