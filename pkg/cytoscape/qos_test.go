package cytoscape

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

func pvcWithQoS(q *graph.QoSCeiling) *graph.PVCNode {
	return &graph.PVCNode{
		IDValue:           "cluster-alpha/db/data",
		NameValue:         "data",
		LabelsValue:       map[string]string{"cluster": "cluster-alpha", "namespace": "db"},
		StorageClassValue: "netapp-nas",
		QoSValue:          q,
	}
}

// pvcNodeJSON serialises one PVC through the full Serialise path and returns
// the raw JSON of its node data, so the tests pin the wire form (key names,
// order, omission) rather than the Go struct.
func pvcNodeJSON(t *testing.T, q *graph.QoSCeiling) string {
	t.Helper()
	pvc := pvcWithQoS(q)
	body := cy(t, []graph.GraphNode{pvc}, nil)
	raw, err := json.Marshal(cyNodesByID(body)[pvc.ID()])
	require.NoError(t, err)
	return string(raw)
}

func TestSerialise_PVCQoS_FullCeiling(t *testing.T) {
	iops, bps := 5000.0, 250.0*1048576
	got := pvcNodeJSON(t, &graph.QoSCeiling{PolicyGroup: "gold-tier", MaxIOPS: &iops, MaxBytesPerSec: &bps})
	assert.Contains(t, got, `"qos":{"policy_group":"gold-tier","max_iops":5000,"max_bytes_per_sec":262144000}`)
	assert.NotContains(t, got, `"labels":{"cluster":"cluster-alpha","namespace":"db","policy_group"`,
		"the ceiling never leaks into labels")
}

// The attribute sits directly after storageclass, so the JSON key order of a
// PVC is stable for the goldens.
func TestSerialise_PVCQoS_KeyOrder(t *testing.T) {
	iops := 5000.0
	got := pvcNodeJSON(t, &graph.QoSCeiling{PolicyGroup: "gold-tier", MaxIOPS: &iops})
	sc, qos, labels := strings.Index(got, `"storageclass"`), strings.Index(got, `"qos"`), strings.Index(got, `"labels"`)
	require.True(t, sc >= 0 && qos >= 0 && labels >= 0, got)
	assert.Less(t, sc, qos)
	assert.Less(t, qos, labels)
}

func TestSerialise_PVCQoS_PartialCeilingKeepsOnlyResolvedField(t *testing.T) {
	iops := 5000.0
	got := pvcNodeJSON(t, &graph.QoSCeiling{PolicyGroup: "gold-tier", MaxIOPS: &iops})
	assert.Contains(t, got, `"qos":{"policy_group":"gold-tier","max_iops":5000}`)

	bps := 2621440.0
	got = pvcNodeJSON(t, &graph.QoSCeiling{PolicyGroup: "gold-tier", MaxBytesPerSec: &bps})
	assert.Contains(t, got, `"qos":{"policy_group":"gold-tier","max_bytes_per_sec":2621440}`)
	assert.NotContains(t, got, "max_iops")
}

// Both figures go through round6, the edge's rounding, so a node and its edge
// serialise identical digits.
func TestSerialise_PVCQoS_FiguresRoundedToSixSignificantDigits(t *testing.T) {
	iops, bps := 1234567.891, 0.00012345678
	got := pvcNodeJSON(t, &graph.QoSCeiling{PolicyGroup: "g", MaxIOPS: &iops, MaxBytesPerSec: &bps})
	assert.Contains(t, got, `"max_iops":1234570`)
	assert.Contains(t, got, `"max_bytes_per_sec":0.000123457`)

	// Rounding never reaches back into pkg/graph values.
	assert.InDelta(t, 1234567.891, iops, 1e-9)
}

func TestSerialise_PVCQoS_AbsentCeilingOmitsKey(t *testing.T) {
	assert.NotContains(t, pvcNodeJSON(t, nil), `"qos"`)
}

// A ceiling with no figure is not a ceiling: the object is present iff at least
// one field resolved, never an empty object, a 0 or an "unlimited" sentinel.
func TestSerialise_PVCQoS_NoFigureOmitsKey(t *testing.T) {
	assert.NotContains(t, pvcNodeJSON(t, &graph.QoSCeiling{PolicyGroup: "gold-tier"}), `"qos"`)
}

func TestSerialise_QoS_OnlyPVCNodesCarryIt(t *testing.T) {
	nodes := []graph.GraphNode{
		&graph.PodNode{IDValue: "c/u", NameValue: "p", LabelsValue: map[string]string{"cluster": "c"}},
		&graph.K8sNode{IDValue: "c/w", NameValue: "w", LabelsValue: map[string]string{"cluster": "c"}},
		&graph.ServiceNode{IDValue: "c/n/s", NameValue: "s", LabelsValue: map[string]string{"cluster": "c", "namespace": "n"}},
		&graph.ExternalNode{IDValue: "external/x", NameValue: "x", LabelsValue: map[string]string{}},
		&graph.NetAppAggrNode{IDValue: graph.NetAppAggrID("oc", "a1"), NameValue: "a1", LabelsValue: map[string]string{"ontap_cluster": "oc", "node": "n1"}},
		&graph.NetAppNode{IDValue: graph.NetAppNodeID("oc", "n1"), NameValue: "n1", LabelsValue: map[string]string{"ontap_cluster": "oc"}},
		&graph.NetAppSVMNode{IDValue: graph.NetAppSVMID("oc", "svm"), NameValue: "svm", LabelsValue: map[string]string{"ontap_cluster": "oc"}},
	}
	raw, err := json.Marshal(cy(t, nodes, nil))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"qos"`)
}
