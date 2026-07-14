package cytoscape

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// A real StorageClass node serialises with its provisioner + parameters as typed
// data attributes (never labels, which stay {cluster}) and parents to its
// cluster group. The PVC parents to its namespace group, not the storageclass.
func TestSerialiseCytoscape_StorageClassNodePayload(t *testing.T) {
	sc := &graph.StorageClassNode{
		IDValue:     "c1/storageclass/netapp-nas",
		NameValue:   "netapp-nas",
		LabelsValue: map[string]string{"cluster": "c1"},
		InfoValue: &graph.StorageClassInfo{
			Provisioner: "csi.trident.netapp.io",
			Parameters:  map[string]string{"pool": "aggr1", "fs": "nfs"},
		},
	}
	pvc := &graph.PVCNode{IDValue: "c1/db/data", NameValue: "data", LabelsValue: map[string]string{"cluster": "c1", "namespace": "db"}, StorageClassValue: "netapp-nas"}
	nodes := cyNodesByID(cy(t, []graph.GraphNode{sc, pvc}, nil))

	scData := nodes["c1/storageclass/netapp-nas"]
	assert.Equal(t, "storageclass", scData.Type)
	assert.Equal(t, "cluster/c1", scData.Parent, "storageclass parents to its cluster group")
	assert.Equal(t, map[string]string{"cluster": "c1"}, scData.Labels)
	assert.Equal(t, "csi.trident.netapp.io", scData.Provisioner)
	assert.Equal(t, map[string]string{"pool": "aggr1", "fs": "nfs"}, scData.Parameters)
	_, hasProvLabel := scData.Labels["provisioner"]
	assert.False(t, hasProvLabel, "provisioner must not appear in labels")

	assert.Equal(t, "c1/namespace/db", nodes["c1/db/data"].Parent,
		"pvc parents to its namespace group, not the storageclass (pvc→sc is an edge)")
}

// A bare StorageClass node (no info) omits provisioner and parameters entirely.
func TestSerialiseCytoscape_BareStorageClassOmitsTypedAttrs(t *testing.T) {
	sc := &graph.StorageClassNode{IDValue: "c1/storageclass/gp3", NameValue: "gp3", LabelsValue: map[string]string{"cluster": "c1"}}
	scData := cyNodesByID(cy(t, []graph.GraphNode{sc}, nil))["c1/storageclass/gp3"]
	assert.Empty(t, scData.Provisioner, "bare node has no provisioner")
	assert.Nil(t, scData.Parameters, "bare node has no parameters")
	assert.Equal(t, "cluster/c1", scData.Parent)
}

// The serialiser no longer fabricates a synthetic storageclass GROUP node —
// StorageClass is a real graph node now, so a lone PVC produces no group.
func TestSerialiseCytoscape_NoSyntheticStorageClassGroup(t *testing.T) {
	pvc := &graph.PVCNode{IDValue: "c1/db/data", NameValue: "data", LabelsValue: map[string]string{"cluster": "c1", "namespace": "db"}, StorageClassValue: "gp3"}
	nodes := cyNodesByID(cy(t, []graph.GraphNode{pvc}, nil))
	_, hasGroup := nodes["c1/storageclass/gp3"]
	assert.False(t, hasGroup, "serialiser must not fabricate a storageclass group node")
	assert.Equal(t, "c1/namespace/db", nodes["c1/db/data"].Parent)
}
