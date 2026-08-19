package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStorageClass_OnlyPVCsCarryIt — the StorageClass() accessor returns the
// resolved value for a PVC and "" for every other node kind (and a class-less
// PVC). It is consumed only by the Cytoscape serialiser for compound grouping
// and is never a label or serialised attribute.
func TestStorageClass_OnlyPVCsCarryIt(t *testing.T) {
	pvc := &PVCNode{IDValue: "c/n/claim", NameValue: "claim", StorageClassValue: "gp3"}
	assert.Equal(t, "gp3", pvc.StorageClass())

	classless := &PVCNode{IDValue: "c/n/claim2", NameValue: "claim2"}
	assert.Empty(t, classless.StorageClass(), "PVC with no resolved StorageClass returns empty")

	others := []GraphNode{
		&PodNode{IDValue: "c/u"},
		&K8sNode{IDValue: "c/w"},
		&ServiceNode{IDValue: "c/n/s"},
		&ExternalNode{IDValue: "external/x"},
	}
	for _, n := range others {
		assert.Emptyf(t, n.StorageClass(), "%T must return empty StorageClass", n)
	}
}

// TestApplication_PodServicePVCCarryIt — the Application() accessor returns the
// resolved ArgoCD Application for pods, services, and PVCs (and "" when
// unresolved), and "" for K8s nodes, externals, and StorageClass nodes. It is a
// typed attribute consumed by the serialiser, never a label.
func TestApplication_PodServicePVCCarryIt(t *testing.T) {
	pod := &PodNode{IDValue: "c/u", NameValue: "checkout", ApplicationValue: "checkout"}
	svc := &ServiceNode{IDValue: "c/n/s", NameValue: "s", ApplicationValue: "checkout"}
	pvc := &PVCNode{IDValue: "c/n/claim", NameValue: "claim", ApplicationValue: "mongo"}
	assert.Equal(t, "checkout", pod.Application())
	assert.Equal(t, "checkout", svc.Application())
	assert.Equal(t, "mongo", pvc.Application())

	// Unresolved pod/service/PVC return "".
	assert.Empty(t, (&PodNode{IDValue: "c/u2"}).Application())
	assert.Empty(t, (&ServiceNode{IDValue: "c/n/s2"}).Application())
	assert.Empty(t, (&PVCNode{IDValue: "c/n/claim2"}).Application())

	// Node kinds that never carry an Application.
	never := []GraphNode{
		&K8sNode{IDValue: "c/w"},
		&ExternalNode{IDValue: "external/x"},
		&NetAppAggrNode{IDValue: NetAppAggrID("oc", "aggr1")},
		&NetAppNode{IDValue: NetAppNodeID("oc", "n1")},
	}
	for _, n := range never {
		assert.Emptyf(t, n.Application(), "%T must return empty Application", n)
	}
}

// TestContainers_OnlyPodsCarryThem — the Containers() accessor returns a pod's
// resolved container list and nil for every other node kind (and an unenriched
// pod). Containers stay pod-only (unlike Application, which widened to
// service/PVC).
func TestContainers_OnlyPodsCarryThem(t *testing.T) {
	pod := &PodNode{
		IDValue:         "c/u",
		NameValue:       "checkout",
		ContainersValue: []Container{{Name: "app", Image: "reg/app:1.2"}},
	}
	assert.Equal(t, []Container{{Name: "app", Image: "reg/app:1.2"}}, pod.Containers())

	bare := &PodNode{IDValue: "c/u2", NameValue: "bare"}
	assert.Nil(t, bare.Containers(), "pod with no containers returns nil")

	others := []GraphNode{
		&K8sNode{IDValue: "c/w"},
		&PVCNode{IDValue: "c/n/claim", ApplicationValue: "mongo"},
		&ServiceNode{IDValue: "c/n/s", ApplicationValue: "checkout"},
		&ExternalNode{IDValue: "external/x"},
		&NetAppAggrNode{IDValue: NetAppAggrID("oc", "aggr1")},
		&NetAppNode{IDValue: NetAppNodeID("oc", "n1")},
	}
	for _, n := range others {
		assert.Nilf(t, n.Containers(), "%T must return nil Containers", n)
	}
}

// TestReadyStatus_OnlyK8sNodesCarryIt — the ReadyStatus() accessor returns the
// node's resolved Ready-condition status for a K8sNode and "" for every other
// node kind (and a node with no observed Ready-condition data). It is a typed
// attribute consumed by the serialiser, never a label.
func TestReadyStatus_OnlyK8sNodesCarryIt(t *testing.T) {
	ready := &K8sNode{IDValue: "c/w", NameValue: "w", ReadyStatusValue: ReadyStatusReady}
	assert.Equal(t, ReadyStatusReady, ready.ReadyStatus())

	for _, want := range []string{ReadyStatusReady, ReadyStatusNotReady, ReadyStatusUnknown} {
		n := &K8sNode{IDValue: "c/w", ReadyStatusValue: want}
		assert.Equal(t, want, n.ReadyStatus())
	}

	bare := &K8sNode{IDValue: "c/w2", NameValue: "w2"}
	assert.Empty(t, bare.ReadyStatus(), "node with no Ready-condition data returns empty")

	others := []GraphNode{
		&PodNode{IDValue: "c/u"},
		&PVCNode{IDValue: "c/n/claim"},
		&ServiceNode{IDValue: "c/n/s"},
		&ExternalNode{IDValue: "external/x"},
	}
	for _, n := range others {
		assert.Emptyf(t, n.ReadyStatus(), "%T must return empty ReadyStatus", n)
	}
}

// TestHealthAndUsage_ZeroValuesPerType — Health/Usage are empty/nil on
// every type except the NetApp types (Health) and PVC + NetApp aggr (Usage).
func TestHealthAndUsage_ZeroValuesPerType(t *testing.T) {
	used, cap := 10.0, 20.0
	pvc := &PVCNode{IDValue: "c/n/claim", UsageValue: &UsageBytes{UsedBytes: &used, CapacityBytes: &cap}}
	aggr := &NetAppAggrNode{
		IDValue: NetAppAggrID("oc", "aggr1"), NameValue: "aggr1",
		LabelsValue: map[string]string{"ontap_cluster": "oc", "node": "n1"},
		HealthValue: HealthOnline, UsageValue: &UsageBytes{UsedBytes: &used},
	}
	ctrl := &NetAppNode{
		IDValue: NetAppNodeID("oc", "n1"), NameValue: "n1",
		LabelsValue: map[string]string{"ontap_cluster": "oc"},
		HealthValue: HealthDegraded,
	}
	assert.Equal(t, HealthOnline, aggr.Health())
	assert.Equal(t, &used, aggr.Usage().UsedBytes)
	assert.Equal(t, HealthDegraded, ctrl.Health())
	assert.Nil(t, ctrl.Usage())
	assert.Empty(t, pvc.Health())
	require := func(n GraphNode) {
		t.Helper()
		if n.Type() != NodeTypeNetAppAggr && n.Type() != NodeTypeNetAppNode {
			assert.Emptyf(t, n.Health(), "%T Health must be empty", n)
		}
		if n.Type() != NodeTypePVC && n.Type() != NodeTypeNetAppAggr {
			assert.Nilf(t, n.Usage(), "%T Usage must be nil", n)
		}
	}
	for _, n := range []GraphNode{
		&PodNode{IDValue: "c/u"},
		&K8sNode{IDValue: "c/w"},
		pvc,
		&ServiceNode{IDValue: "c/n/s"},
		&ExternalNode{IDValue: "external/x"},
		aggr, ctrl,
	} {
		require(n)
	}
}

func TestNetAppIDs(t *testing.T) {
	assert.Equal(t, "netapp/ontap-prod/aggr/aggr1", NetAppAggrID("ontap-prod", "aggr1"))
	assert.Equal(t, "netapp/ontap-prod/ontap-prod-01", NetAppNodeID("ontap-prod", "ontap-prod-01"))
}
