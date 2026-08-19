package cytoscape

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

func TestSerialise_NetAppNestingAndPayloads(t *testing.T) {
	used, cap := 700.0, 1000.0
	pvcUsed, pvcCap := 5.0, 10.0
	aggr := &graph.NetAppAggrNode{
		IDValue:     graph.NetAppAggrID("ontap-prod", "aggr1"),
		NameValue:   "aggr1",
		LabelsValue: map[string]string{"ontap_cluster": "ontap-prod", "node": "ontap-prod-01"},
		HealthValue: graph.HealthOnline,
		UsageValue:  &graph.UsageBytes{UsedBytes: &used, CapacityBytes: &cap},
	}
	ctrl := &graph.NetAppNode{
		IDValue:     graph.NetAppNodeID("ontap-prod", "ontap-prod-01"),
		NameValue:   "ontap-prod-01",
		LabelsValue: map[string]string{"ontap_cluster": "ontap-prod"},
		HealthValue: graph.HealthOnline,
	}
	pvc := &graph.PVCNode{
		IDValue:           "cluster-alpha/db/data",
		NameValue:         "data",
		LabelsValue:       map[string]string{"cluster": "cluster-alpha", "namespace": "db", "volumename": "pvc-9f3a", "svm": "svm-prod"},
		StorageClassValue: "netapp-nas",
		UsageValue:        &graph.UsageBytes{UsedBytes: &pvcUsed, CapacityBytes: &pvcCap},
	}
	body := cy(t, []graph.GraphNode{aggr, ctrl, pvc}, nil)
	nodes := cyNodesByID(body)

	require.Contains(t, nodes, "storage-cluster/ontap-prod")
	assert.Equal(t, "storage-cluster", nodes["storage-cluster/ontap-prod"].Type)
	assert.Empty(t, nodes["storage-cluster/ontap-prod"].Parent)
	assert.Equal(t, "storage-cluster/ontap-prod", nodes[ctrl.ID()].Parent)
	assert.Equal(t, ctrl.ID(), nodes[aggr.ID()].Parent)
	assert.Equal(t, graph.HealthOnline, nodes[aggr.ID()].Health)
	require.NotNil(t, nodes[aggr.ID()].Usage)
	assert.InDelta(t, 700.0, *nodes[aggr.ID()].Usage.UsedBytes, 1e-12)
	assert.Equal(t, "netapp-nas", nodes[pvc.ID()].StorageClass)
	require.NotNil(t, nodes[pvc.ID()].Usage)

	for _, c := range body.Clusters {
		assert.NotEqual(t, "ontap-prod", c, "ONTAP cluster names must not appear in clusters[]")
	}

	// storage-cluster group sits after cluster groups, before namespace groups.
	types := make([]string, 0, len(body.Elements.Nodes))
	for _, n := range body.Elements.Nodes {
		types = append(types, n.Data.Type)
	}
	assert.Equal(t, "cluster", types[0])
	assert.Equal(t, "storage-cluster", types[1])
	assert.Equal(t, "namespace", types[2])
}

func TestSerialise_HealthAndUsageOmittedWhenEmpty(t *testing.T) {
	aggr := &graph.NetAppAggrNode{
		IDValue: graph.NetAppAggrID("oc", "a1"), NameValue: "a1",
		LabelsValue: map[string]string{"ontap_cluster": "oc", "node": "n1"},
	}
	ctrl := &graph.NetAppNode{
		IDValue: graph.NetAppNodeID("oc", "n1"), NameValue: "n1",
		LabelsValue: map[string]string{"ontap_cluster": "oc"},
	}
	pvc := &graph.PVCNode{IDValue: "c/ns/claim", NameValue: "claim", LabelsValue: map[string]string{"cluster": "c", "namespace": "ns"}}
	body := cy(t, []graph.GraphNode{aggr, ctrl, pvc}, nil)
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"health"`)
	assert.NotContains(t, string(raw), `"usage"`)
	assert.NotContains(t, string(raw), `"storageclass"`)
}

// An owner-less aggregate has no controller node to parent it; it must still
// nest under its storage-cluster group rather than dangle at top level.
func TestSerialise_OwnerlessAggrNestsUnderStorageCluster(t *testing.T) {
	aggr := &graph.NetAppAggrNode{
		IDValue:     graph.NetAppAggrID("ontap-prod", "aggr1"),
		NameValue:   "aggr1",
		LabelsValue: map[string]string{"ontap_cluster": "ontap-prod"},
	}
	body := Serialise(nil, graph.View{Nodes: []graph.GraphNode{aggr}})
	var got string
	for _, n := range body.Elements.Nodes {
		if n.Data.ID == aggr.IDValue {
			got = n.Data.Parent
		}
	}
	require.Equal(t, "storage-cluster/ontap-prod", got)
}
