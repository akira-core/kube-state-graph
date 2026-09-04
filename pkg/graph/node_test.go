package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Equal(t, "netapp/ontap-prod/svm/svm_shop", NetAppSVMID("ontap-prod", "svm_shop"))

	// The three id spaces must stay disjoint. A controller sits directly under
	// its cluster, so a controller literally named "svm" would otherwise
	// collide with the SVM prefix; the "/svm/" infix carries a trailing
	// segment that a controller id never has.
	assert.NotEqual(t, NetAppNodeID("oc", "svm"), NetAppSVMID("oc", ""))
	assert.NotEqual(t, NetAppAggrID("oc", "x"), NetAppSVMID("oc", "x"))
}

// --- the SVM node type ----------------------------------------------------

// The SVM is an IDENTITY only in this change: no SVM-level Harvest series is
// resolved, so every attribute accessor is nil / empty — alerts included, since
// an alert's label set names no SVM. Its labels are exactly {ontap_cluster},
// with no `cluster` key, which is what keeps it out of clusters[] and out of
// ?cluster= addressing.
func TestNetAppSVMNode_IdentityOnly(t *testing.T) {
	svm := &NetAppSVMNode{
		IDValue:     NetAppSVMID("ontap-prod", "svm_shop"),
		NameValue:   "svm_shop",
		LabelsValue: map[string]string{"ontap_cluster": "ontap-prod"},
	}

	assert.Equal(t, "netapp/ontap-prod/svm/svm_shop", svm.ID())
	assert.Equal(t, "svm_shop", svm.Name())
	assert.Equal(t, NodeTypeNetAppSVM, svm.Type())
	assert.Equal(t, map[string]string{"ontap_cluster": "ontap-prod"}, svm.Labels())
	assert.NotContains(t, svm.Labels(), "cluster",
		"an SVM belongs to no Kubernetes cluster")

	assert.Nil(t, svm.IPAddress())
	assert.Nil(t, svm.Owner())
	assert.Empty(t, svm.Application())
	assert.Nil(t, svm.Containers())
	assert.Empty(t, svm.ReadyStatus())
	assert.Empty(t, svm.Health())
	assert.Nil(t, svm.Usage())
	assert.Empty(t, svm.StorageClass())
	assert.Nil(t, svm.Hardware())
	assert.Nil(t, svm.Perf())
	assert.Nil(t, svm.Alerts())
}

// SortNodes is id-ordered and type-blind, so the SVM takes its place among the
// other NetApp types by id alone. Within one ONTAP cluster the shared
// "netapp/<oc>/" prefix leaves the segment after it to decide, which orders
// aggregate ("aggr/…") before controller ("n1") before SVM ("svm/…"). Pinning
// it here is what keeps the body byte-deterministic.
func TestSortNodes_OrdersTheSVMByID(t *testing.T) {
	nodes := []GraphNode{
		&NetAppSVMNode{IDValue: NetAppSVMID("oc", "svm_shop")},
		&NetAppNode{IDValue: NetAppNodeID("oc", "n1")},
		&NetAppAggrNode{IDValue: NetAppAggrID("oc", "aggr1")},
		&PodNode{IDValue: "cluster-a/uid-1"},
	}
	SortNodes(nodes)

	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID()
	}
	assert.Equal(t, []string{
		"cluster-a/uid-1",
		"netapp/oc/aggr/aggr1",
		"netapp/oc/n1",
		"netapp/oc/svm/svm_shop",
	}, ids)
}

// --- hardware / perf ------------------------------------------------------

// Only a controller carries hardware and perf. Both are typed attributes,
// never labels, and both are omitted (nil) rather than zero-valued when the
// Harvest legs matched nothing.
func TestHardwareAndPerf_OnlyNetAppNodesCarryThem(t *testing.T) {
	cpu, ops := 72.5, 18500.0
	ctrl := &NetAppNode{
		IDValue:     NetAppNodeID("oc", "n1"),
		NameValue:   "n1",
		LabelsValue: map[string]string{"ontap_cluster": "oc"},
		HealthValue: HealthOnline,
		HardwareValue: &Hardware{
			Model: "AFF-A400", Serial: "721234000123", Version: "9.14.1", Vendor: "NetApp",
		},
		PerfValue: &NodePerf{CPUBusyPct: &cpu, TotalOps: &ops},
	}

	require.NotNil(t, ctrl.Hardware())
	assert.Equal(t, "AFF-A400", ctrl.Hardware().Model)
	assert.Empty(t, ctrl.Hardware().Location, "an unresolved field stays empty, never defaulted")
	require.NotNil(t, ctrl.Perf())
	assert.Equal(t, &cpu, ctrl.Perf().CPUBusyPct)
	assert.Nil(t, ctrl.Perf().TotalLatencyUs, "an unresolved counter is nil, never 0")

	// A high CPU figure must not touch the reported health.
	assert.Equal(t, HealthOnline, ctrl.Health())

	// A controller no leg matched carries neither object.
	bare := &NetAppNode{IDValue: NetAppNodeID("oc", "n2")}
	assert.Nil(t, bare.Hardware())
	assert.Nil(t, bare.Perf())

	for _, n := range []GraphNode{
		&PodNode{IDValue: "c/u"},
		&K8sNode{IDValue: "c/w"},
		&PVCNode{IDValue: "c/n/claim"},
		&ServiceNode{IDValue: "c/n/s"},
		&ExternalNode{IDValue: "external/x"},
		&NetAppAggrNode{IDValue: NetAppAggrID("oc", "aggr1")},
		&NetAppSVMNode{IDValue: NetAppSVMID("oc", "svm1")},
	} {
		assert.Nilf(t, n.Hardware(), "%T must return nil Hardware", n)
		assert.Nilf(t, n.Perf(), "%T must return nil Perf", n)
	}
}

func TestHardwareAndPerf_Empty(t *testing.T) {
	assert.True(t, Hardware{}.Empty())
	assert.False(t, Hardware{Location: "rack-3"}.Empty(),
		"a lone location still resolves the object")
	assert.True(t, NodePerf{}.Empty())

	zero := 0.0
	assert.False(t, NodePerf{CPUBusyPct: &zero}.Empty(),
		"a counter reporting 0 resolved — absent and zero are different")
}

// --- alerts ---------------------------------------------------------------

// Only pods, K8s nodes, PVCs, controllers and aggregates can carry alerts.
// Services, externals and SVMs never do, and an unalerted node of ANY kind
// returns nil so the serialiser omits the attribute — which is what keeps an
// unalerted estate byte-identical to one built before the overlay existed.
func TestAlerts_OnlyFiveKindsCarryThem(t *testing.T) {
	a := []Alert{{Name: "HighMemory", State: AlertStateFiring, Severity: "critical"}}

	carriers := []GraphNode{
		&PodNode{IDValue: "c/u", AlertsValue: a},
		&K8sNode{IDValue: "c/w", AlertsValue: a},
		&PVCNode{IDValue: "c/n/claim", AlertsValue: a},
		&NetAppNode{IDValue: NetAppNodeID("oc", "n1"), AlertsValue: a},
		&NetAppAggrNode{IDValue: NetAppAggrID("oc", "aggr1"), AlertsValue: a},
	}
	for _, n := range carriers {
		assert.Equalf(t, a, n.Alerts(), "%T must carry its matched alerts", n)
	}

	never := []GraphNode{
		&ServiceNode{IDValue: "c/n/s"},
		&ExternalNode{IDValue: "external/x"},
		&NetAppSVMNode{IDValue: NetAppSVMID("oc", "svm1")},
	}
	for _, n := range never {
		assert.Nilf(t, n.Alerts(), "%T must never carry alerts", n)
	}

	// Unalerted nodes of the carrying kinds return nil, not an empty slice.
	for _, n := range []GraphNode{
		&PodNode{IDValue: "c/u2"},
		&K8sNode{IDValue: "c/w2"},
		&PVCNode{IDValue: "c/n/claim2"},
		&NetAppNode{IDValue: NetAppNodeID("oc", "n2")},
		&NetAppAggrNode{IDValue: NetAppAggrID("oc", "aggr2")},
	} {
		assert.Nilf(t, n.Alerts(), "%T with no matched alert must return nil", n)
	}
}

// SortAlerts is what makes the attribute a pure function of the matched SET:
// sorted by (name, severity) and de-duplicated on that pair, so two ALERTS
// series differing only in a label the matcher never reads collapse to one
// entry whatever order they arrived in.
func TestSortAlerts_SortsAndDeduplicates(t *testing.T) {
	got := SortAlerts([]Alert{
		{Name: "KubePodCrashLooping", State: AlertStateFiring, Severity: "warning"},
		{Name: "HighMemory", State: AlertStateFiring, Severity: "critical"},
		{Name: "KubePodCrashLooping", State: AlertStateFiring, Severity: "warning"},
	})
	assert.Equal(t, []Alert{
		{Name: "HighMemory", State: AlertStateFiring, Severity: "critical"},
		{Name: "KubePodCrashLooping", State: AlertStateFiring, Severity: "warning"},
	}, got)

	// Same (name, severity) pair arriving in the opposite order yields the
	// same slice — the order-freedom the determinism rule needs.
	reversed := SortAlerts([]Alert{
		{Name: "KubePodCrashLooping", State: AlertStateFiring, Severity: "warning"},
		{Name: "HighMemory", State: AlertStateFiring, Severity: "critical"},
	})
	assert.Equal(t, got, reversed)

	// Same name, different severity is NOT a duplicate.
	assert.Len(t, SortAlerts([]Alert{
		{Name: "A", State: AlertStateFiring, Severity: "warning"},
		{Name: "A", State: AlertStateFiring, Severity: "critical"},
	}), 2)

	assert.Nil(t, SortAlerts(nil))
}
