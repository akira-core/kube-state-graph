package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
)

var update = flag.Bool("update", false, "update golden files")

// TestGolden_GraphResponses snapshots the Cytoscape responses for several
// canned scenarios so contract drift is caught on PR.
func TestGolden_GraphResponses(t *testing.T) {
	scenarios := map[string]graph.View{
		"single-cluster":       buildSingleCluster(),
		"two-cluster-cross":    buildTwoClusterCross(),
		"with-service":         buildWithService(),
		"family-fanout":        buildFamilyFanout(),
		"with-storageclass":    buildWithStorageClass(),
		"with-netapp-storage":  buildWithNetAppStorage(),
		"prune-false":          buildPruneFalse(),
		"filtered-external":    buildFilteredExternalPartner(),
		"missing-uid-fallback": buildMissingUIDFallback(),
		"link-relation":        buildLinkRelation(),
		"with-red-metrics":     buildWithREDMetrics(),
		"cluster-identity":     buildClusterIdentity(),
	}

	// Scenarios whose clusters were composed from az/env labels also need the
	// identity table the builder attaches, since the serialiser reads it via
	// the graph. Every other scenario models an unstamped estate (nil table),
	// which is what keeps their goldens byte-identical.
	identities := map[string]map[string]graph.ClusterIdentity{
		"cluster-identity": {
			"us-dev-c1":  {AZ: "us", Env: "dev", Name: "c1"},
			"eu-prod-c1": {AZ: "eu", Env: "prod", Name: "c1"},
		},
	}

	for name, view := range scenarios {
		t.Run(name+"-cytoscape", func(t *testing.T) {
			g := &graph.Graph{
				BuiltAt:           time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC),
				NodesByID:         map[string]graph.GraphNode{},
				ClusterIdentities: identities[name],
			}
			for _, n := range view.Nodes {
				g.NodesByID[n.ID()] = n
			}
			body := cytoscape.Serialise(g, view)
			compareGolden(t, name+"-cytoscape.json", body)
		})
	}
}

func TestGolden_StorageGraphResponses(t *testing.T) {
	g := buildStorageGraphEstate()
	aggrScope, err := graph.NewStorageScope(nil, nil, nil, nil, []string{"aggr1"}, nil, nil)
	require.NoError(t, err)
	podScope, err := graph.NewStorageScope(nil, nil, nil, nil, nil, nil, []string{"shop/web-0"})
	require.NoError(t, err)

	scenarios := map[string]graph.View{
		"storage-graph-aggr-root": graph.ProjectStorage(g, aggrScope),
		"storage-graph-pod-root":  graph.ProjectStorage(g, podScope),
	}
	for name, view := range scenarios {
		t.Run(name+"-cytoscape", func(t *testing.T) {
			body := cytoscape.Serialise(g, view)
			compareGolden(t, name+"-cytoscape.json", body)
		})
	}
}

func TestGolden_EdgeTypes(t *testing.T) {
	body := map[string]any{
		"apiVersion": APIVersion,
		"edge_types": graph.EdgeTypes,
	}
	compareGolden(t, "edge-types.json", body)
}

func compareGolden(t *testing.T, file string, body any) {
	t.Helper()
	got, err := json.MarshalIndent(body, "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')
	path := filepath.Join("testdata", "golden", file)

	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o644))
		return
	}

	want, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read golden (run with -update)")
	assert.Truef(t, bytes.Equal(want, got), "golden mismatch for %s\n--- want\n%s\n--- got\n%s", file, want, got)
}

// ----- canned scenarios -----------------------------------------------------

func buildSingleCluster() graph.View {
	pod := &graph.PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-0"}}
	node := &graph.K8sNode{IDValue: "cluster-alpha/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}
	return graph.View{Nodes: []graph.GraphNode{node, pod}}
}

func buildTwoClusterCross() graph.View {
	a := &graph.PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	b := &graph.PodNode{IDValue: "cluster-beta/p2", NameValue: "payments", LabelsValue: map[string]string{"cluster": "cluster-beta", "namespace": "billing"}}
	er := 0.1
	p90 := 12.5
	cross := graph.NewEdge(graph.EdgeTypePodCallsPod, a.IDValue, b.IDValue, map[string]string{
		"cluster": "cluster-alpha",
	}).WithMetrics(graph.EdgeMetrics{Rate: 5, ErrorRate: &er, P90ServerMs: &p90})
	return graph.View{Nodes: []graph.GraphNode{a, b}, Edges: []*graph.Edge{cross}}
}

// buildWithService snapshots the D29 connection-string service resolution:
// a pod-calls-pod edge whose target is a `type="service"` node (resolved from
// a `<service>.<namespace>.svc...` string, carrying cluster_ip on ipaddress),
// plus a `service-selects-pod` edge fanning out to a backing pod.
func buildWithService() graph.View {
	pod := &graph.PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	svc := &graph.ServiceNode{IDValue: "cluster-alpha/shop/payments", NameValue: "payments", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}, IPAddressValue: []string{"10.0.0.5"}}
	pay0 := &graph.PodNode{IDValue: "cluster-alpha/pay0", NameValue: "payments-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	edges := []*graph.Edge{
		graph.NewEdge(graph.EdgeTypePodCallsService, pod.IDValue, svc.IDValue, map[string]string{"cluster": "cluster-alpha"}),
		graph.NewEdge(graph.EdgeTypeServiceSelectsPod, svc.IDValue, pay0.IDValue, map[string]string{"namespace": "shop"}),
	}
	return graph.View{Nodes: []graph.GraphNode{pod, svc, pay0}, Edges: edges}
}

// buildFamilyFanout snapshots the D29 cluster-family fan-out: a client pod in
// prod-1 dials a `<service>.<namespace>.svc` connection string for a Service
// deployed in BOTH prod-1 and prod-2 (one family: digit runs normalise to
// "prod-0"). Each family cluster's service node materialises, the single
// upstream series yields one pod-calls-service edge per match — the prod-2
// edge is cross-cluster (source labels.cluster=prod-1, target
// labels.cluster=prod-2) — and each service fans out service-selects-pod only
// to its own cluster's backing pod. Both call edges carry
// labels.cluster=prod-1 (the client side is a pod).
func buildFamilyFanout() graph.View {
	pod := &graph.PodNode{IDValue: "prod-1/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "shop"}}
	svc1 := &graph.ServiceNode{IDValue: "prod-1/messaging/nats", NameValue: "nats", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "messaging"}, IPAddressValue: []string{"10.1.0.5"}}
	svc2 := &graph.ServiceNode{IDValue: "prod-2/messaging/nats", NameValue: "nats", LabelsValue: map[string]string{"cluster": "prod-2", "namespace": "messaging"}, IPAddressValue: []string{"10.2.0.5"}}
	nats1 := &graph.PodNode{IDValue: "prod-1/n1", NameValue: "nats-0", LabelsValue: map[string]string{"cluster": "prod-1", "namespace": "messaging"}}
	nats2 := &graph.PodNode{IDValue: "prod-2/n2", NameValue: "nats-0", LabelsValue: map[string]string{"cluster": "prod-2", "namespace": "messaging"}}
	edges := []*graph.Edge{
		graph.NewEdge(graph.EdgeTypePodCallsService, pod.IDValue, svc1.IDValue, map[string]string{"cluster": "prod-1"}),
		graph.NewEdge(graph.EdgeTypePodCallsService, pod.IDValue, svc2.IDValue, map[string]string{"cluster": "prod-1"}),
		graph.NewEdge(graph.EdgeTypeServiceSelectsPod, svc1.IDValue, nats1.IDValue, map[string]string{"namespace": "messaging"}),
		graph.NewEdge(graph.EdgeTypeServiceSelectsPod, svc2.IDValue, nats2.IDValue, map[string]string{"namespace": "messaging"}),
	}
	return graph.View{Nodes: []graph.GraphNode{pod, svc1, svc2, nats1, nats2}, Edges: edges}
}

// buildWithStorageClass snapshots a PVC that still carries its StorageClass
// *name* as data.storageclass (the StorageClass node and pvc-to-storageclass
// edge are gone). The pod nests under its controller > application >
// namespace hierarchy and links to its host node via a `pod-to-node` edge.
func buildWithStorageClass() graph.View {
	pod := &graph.PodNode{
		IDValue:          "cluster-alpha/p1",
		NameValue:        "mongo-0",
		LabelsValue:      map[string]string{"cluster": "cluster-alpha", "namespace": "db", "node": "cluster-alpha/worker-0"},
		ApplicationValue: "mongo",
		OwnerValue:       &graph.Owner{Kind: "StatefulSet", Name: "mongo"},
	}
	node := &graph.K8sNode{IDValue: "cluster-alpha/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}
	used, cap := 5368709120.0, 10737418240.0
	pvcGP3 := &graph.PVCNode{
		IDValue: "cluster-alpha/db/data-mongo-0", NameValue: "data-mongo-0",
		LabelsValue:       map[string]string{"cluster": "cluster-alpha", "namespace": "db", "volume": "data"},
		StorageClassValue: "gp3",
		UsageValue:        &graph.UsageBytes{UsedBytes: &used, CapacityBytes: &cap},
	}
	pvcNone := &graph.PVCNode{IDValue: "cluster-alpha/db/legacy", NameValue: "legacy", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "db"}}
	edges := []*graph.Edge{
		graph.NewEdge(graph.EdgeTypePodToNode, pod.IDValue, node.IDValue, nil),
		graph.NewEdge(graph.EdgeTypePodMountsPVC, pod.IDValue, pvcGP3.IDValue, map[string]string{"claim_name": "data-mongo-0"}),
		graph.NewEdge(graph.EdgeTypePodMountsPVC, pod.IDValue, pvcNone.IDValue, map[string]string{"claim_name": "legacy"}),
	}
	return graph.View{Nodes: []graph.GraphNode{node, pod, pvcGP3, pvcNone}, Edges: edges}
}

// buildWithNetAppStorage snapshots the Harvest-joined storage graph: a PVC
// with volumename+svm labels, a pvc-to-netapp-aggr edge carrying I/O metrics,
// an aggregate with health+usage nested under its real owning controller,
// which nests under a storage-cluster group.
func buildWithNetAppStorage() graph.View {
	pod := &graph.PodNode{
		IDValue: "cluster-alpha/p1", NameValue: "mongo-0",
		LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "db"},
		AlertsValue: []graph.Alert{{Name: "KubePodCrashLooping", State: graph.AlertStateFiring, Severity: "warning"}},
	}
	k8sNode := &graph.K8sNode{
		IDValue: "cluster-alpha/worker-0", NameValue: "worker-0",
		LabelsValue: map[string]string{"cluster": "cluster-alpha"},
		AlertsValue: []graph.Alert{{Name: "KubeNodeNotReady", State: graph.AlertStateFiring, Severity: "critical"}},
	}
	used, cap := 700000000000.0, 1000000000000.0
	readOps, writeOps, readLat, writeLat, readBps, writeBps := 150.0, 40.0, 830.0, 1200.0, 5242880.0, 1000000.0
	cpu, ops, lat, data := 72.5, 18500.0, 830.0, 1.2e9
	pvc := &graph.PVCNode{
		IDValue:   "cluster-alpha/db/data-mongo-0",
		NameValue: "data-mongo-0",
		LabelsValue: map[string]string{
			"cluster": "cluster-alpha", "namespace": "db",
			"volume": "data", "volumename": "pvc-9f3a", "svm": "svm-prod",
		},
		StorageClassValue: "netapp-nas",
		AlertsValue:       []graph.Alert{{Name: "PVCAlmostFull", State: graph.AlertStateFiring, Severity: "warning"}},
	}
	pvcPlain := &graph.PVCNode{IDValue: "cluster-alpha/db/scratch", NameValue: "scratch", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "db"}}
	aggr := &graph.NetAppAggrNode{
		IDValue:     graph.NetAppAggrID("ontap-prod", "aggr1"),
		NameValue:   "aggr1",
		LabelsValue: map[string]string{"ontap_cluster": "ontap-prod", "node": "ontap-prod-01"},
		HealthValue: graph.HealthOnline,
		UsageValue:  &graph.UsageBytes{UsedBytes: &used, CapacityBytes: &cap},
		AlertsValue: []graph.Alert{{Name: "AggrSpaceLow", State: graph.AlertStateFiring, Severity: "warning"}},
	}
	ctrl := &graph.NetAppNode{
		IDValue:     graph.NetAppNodeID("ontap-prod", "ontap-prod-01"),
		NameValue:   "ontap-prod-01",
		LabelsValue: map[string]string{"ontap_cluster": "ontap-prod"},
		HealthValue: graph.HealthOnline,
		HardwareValue: &graph.Hardware{
			Model: "AFF-A400", Serial: "721234000123", Version: "9.14.1", Vendor: "NetApp",
		},
		PerfValue:   &graph.NodePerf{CPUBusyPct: &cpu, TotalOps: &ops, TotalLatencyUs: &lat, TotalBytesPerSec: &data},
		AlertsValue: []graph.Alert{{Name: "NodeCPUBusy", State: graph.AlertStateFiring, Severity: "critical"}},
	}
	maxIOPS, maxBps := 5000.0, 262144000.0
	ioEdge := graph.NewEdge(graph.EdgeTypePVCToNetAppAggr, pvc.IDValue, aggr.IDValue, nil).WithIO(graph.IOMetrics{
		ReadOps: &readOps, WriteOps: &writeOps, ReadLatencyUs: &readLat, WriteLatencyUs: &writeLat,
		ReadBytesPerSec: &readBps, WriteBytesPerSec: &writeBps,
		MaxIOPS: &maxIOPS, MaxBytesPerSec: &maxBps,
	})
	edges := []*graph.Edge{
		graph.NewEdge(graph.EdgeTypePodMountsPVC, pod.IDValue, pvc.IDValue, map[string]string{"claim_name": "data-mongo-0"}),
		graph.NewEdge(graph.EdgeTypePodMountsPVC, pod.IDValue, pvcPlain.IDValue, map[string]string{"claim_name": "scratch"}),
		ioEdge,
	}
	return graph.View{Nodes: []graph.GraphNode{pod, k8sNode, pvc, pvcPlain, aggr, ctrl}, Edges: edges}
}

// buildStorageGraphEstate is the shared /v1/storage-graph golden fixture: two
// Kubernetes clusters sharing one filer, a FlexGroup claim, an unmounted
// claim, an unmeasured claim, and an RWX claim whose equal split is visible
// from the pod-root projection.
func buildStorageGraphEstate() *graph.Graph {
	const oc, a, b = "ontap-prod", "cluster-alpha", "cluster-beta"
	ctrl1 := &graph.NetAppNode{IDValue: graph.NetAppNodeID(oc, "ontap-prod-01"), NameValue: "ontap-prod-01", LabelsValue: map[string]string{"ontap_cluster": oc}}
	ctrl2 := &graph.NetAppNode{IDValue: graph.NetAppNodeID(oc, "ontap-prod-02"), NameValue: "ontap-prod-02", LabelsValue: map[string]string{"ontap_cluster": oc}}
	aggr1 := &graph.NetAppAggrNode{IDValue: graph.NetAppAggrID(oc, "aggr1"), NameValue: "aggr1", LabelsValue: map[string]string{"ontap_cluster": oc, "node": "ontap-prod-01"}}
	aggr7 := &graph.NetAppAggrNode{IDValue: graph.NetAppAggrID(oc, "aggr7"), NameValue: "aggr7", LabelsValue: map[string]string{"ontap_cluster": oc, "node": "ontap-prod-02"}}
	aggr9 := &graph.NetAppAggrNode{IDValue: graph.NetAppAggrID(oc, "aggr9"), NameValue: "aggr9", LabelsValue: map[string]string{"ontap_cluster": oc, "node": "ontap-prod-02"}}
	svmShop := &graph.NetAppSVMNode{IDValue: graph.NetAppSVMID(oc, "svm_shop"), NameValue: "svm_shop", LabelsValue: map[string]string{"ontap_cluster": oc}}
	svmBig := &graph.NetAppSVMNode{IDValue: graph.NetAppSVMID(oc, "svm_big"), NameValue: "svm_big", LabelsValue: map[string]string{"ontap_cluster": oc}}
	svmPlat := &graph.NetAppSVMNode{IDValue: graph.NetAppSVMID(oc, "svm_plat"), NameValue: "svm_plat", LabelsValue: map[string]string{"ontap_cluster": oc}}

	orders := &graph.PVCNode{IDValue: graph.PVCID(a, "shop", "orders-data"), NameValue: "orders-data", LabelsValue: map[string]string{"cluster": a, "namespace": "shop"}}
	shared := &graph.PVCNode{IDValue: graph.PVCID(a, "shop", "shared-data"), NameValue: "shared-data", LabelsValue: map[string]string{"cluster": a, "namespace": "shop"}}
	plain := &graph.PVCNode{IDValue: graph.PVCID(a, "shop", "plain-data"), NameValue: "plain-data", LabelsValue: map[string]string{"cluster": a, "namespace": "shop"}}
	big := &graph.PVCNode{IDValue: graph.PVCID(a, "shop", "big-data"), NameValue: "big-data", LabelsValue: map[string]string{"cluster": a, "namespace": "shop"}}
	idle := &graph.PVCNode{IDValue: graph.PVCID(a, "shop", "idle-data"), NameValue: "idle-data", LabelsValue: map[string]string{"cluster": a, "namespace": "shop"}}
	db := &graph.PVCNode{IDValue: graph.PVCID(b, "db", "db-data"), NameValue: "db-data", LabelsValue: map[string]string{"cluster": b, "namespace": "db"}}

	pod := func(cluster, ns, name, uid, node string) *graph.PodNode {
		labels := map[string]string{"cluster": cluster, "namespace": ns, "node": graph.K8sNodeID(cluster, node)}
		return &graph.PodNode{IDValue: graph.PodID(cluster, uid), NameValue: name, LabelsValue: labels}
	}
	orders0 := pod(a, "shop", "orders-0", "uid-orders", "worker-1")
	web0 := pod(a, "shop", "web-0", "uid-web-0", "worker-1")
	web1 := pod(a, "shop", "web-1", "uid-web-1", "worker-1")
	web2 := pod(a, "shop", "web-2", "uid-web-2", "worker-2")
	plain0 := pod(a, "shop", "plain-0", "uid-plain", "worker-1")
	big0 := pod(a, "shop", "big-0", "uid-big", "worker-1")
	db0 := pod(b, "db", "db-0", "uid-db", "worker-b")
	w1 := &graph.K8sNode{IDValue: graph.K8sNodeID(a, "worker-1"), NameValue: "worker-1", LabelsValue: map[string]string{"cluster": a}}
	w2 := &graph.K8sNode{IDValue: graph.K8sNodeID(a, "worker-2"), NameValue: "worker-2", LabelsValue: map[string]string{"cluster": a}}
	wb := &graph.K8sNode{IDValue: graph.K8sNodeID(b, "worker-b"), NameValue: "worker-b", LabelsValue: map[string]string{"cluster": b}}

	f64 := func(v float64) *float64 { return &v }
	io := func(ops float64) *graph.IOMetrics {
		return &graph.IOMetrics{ReadOps: f64(ops), WriteOps: f64(ops / 2), ReadLatencyUs: f64(450), MaxIOPS: f64(5000)}
	}
	hop := func(tier, src, tgt string, extra map[string]string, m *graph.IOMetrics) *graph.Edge {
		l := map[string]string{"tier": tier}
		for k, v := range extra {
			l[k] = v
		}
		e := graph.NewEdge(graph.EdgeTypeStorageFlow, src, tgt, l)
		if m != nil {
			e = e.WithIO(*m)
		}
		return e
	}
	chain := func(ctrl, aggr, svm, pvc, p, node string, m *graph.IOMetrics, n int) []*graph.Edge {
		var out []*graph.Edge
		if ctrl != "" && aggr != "" {
			out = append(out, hop(graph.StorageTierNodeAggr, ctrl, aggr, nil, nil))
		}
		if aggr != "" && svm != "" {
			out = append(out, hop(graph.StorageTierAggrSVM, aggr, svm, nil, nil))
		}
		cl := map[string]string{}
		if aggr != "" {
			cl[graph.ClaimAggrLabel] = aggr
		}
		out = append(out, hop(graph.StorageTierSVMPVC, svm, pvc, cl, m))
		pl := map[string]string{}
		if n > 1 {
			pl["attribution"] = graph.AttributionSplit
		}
		out = append(out, hop(graph.StorageTierPVCPod, pvc, p, pl, nil))
		if node != "" {
			out = append(out, hop(graph.StorageTierPodNode, p, node, nil, nil))
		}
		return out
	}

	edges := make([]*graph.Edge, 0, 32)
	edges = append(edges, chain(ctrl1.ID(), aggr1.ID(), svmShop.ID(), orders.ID(), orders0.ID(), w1.ID(), io(100), 1)...)
	edges = append(edges, hop(graph.StorageTierNodeAggr, ctrl1.ID(), aggr1.ID(), nil, nil))
	edges = append(edges, hop(graph.StorageTierAggrSVM, aggr1.ID(), svmShop.ID(), nil, nil))
	edges = append(edges, hop(graph.StorageTierSVMPVC, svmShop.ID(), shared.ID(), map[string]string{graph.ClaimAggrLabel: aggr1.ID()}, io(300)))
	for _, p := range []*graph.PodNode{web0, web1, web2} {
		edges = append(edges, hop(graph.StorageTierPVCPod, shared.ID(), p.ID(), map[string]string{"attribution": graph.AttributionSplit}, nil))
		edges = append(edges, hop(graph.StorageTierPodNode, p.ID(), p.Labels()["node"], nil, nil))
	}
	edges = append(edges, chain(ctrl1.ID(), aggr1.ID(), svmShop.ID(), plain.ID(), plain0.ID(), w1.ID(), nil, 1)...)
	edges = append(edges, chain("", "", svmBig.ID(), big.ID(), big0.ID(), w1.ID(), nil, 1)...)
	edges = append(edges, chain(ctrl1.ID(), aggr1.ID(), svmPlat.ID(), db.ID(), db0.ID(), wb.ID(), io(50), 1)...)
	// idle-data / aggr7 / aggr9 are inventory only: no edges.

	nodes := []graph.GraphNode{
		ctrl1, ctrl2, aggr1, aggr7, aggr9, svmShop, svmBig, svmPlat,
		orders, shared, plain, big, idle, db,
		orders0, web0, web1, web2, plain0, big0, db0, w1, w2, wb,
	}
	return graph.NewGraph(nodes, edges, time.Time{})
}

// buildMissingUIDFallback snapshots the D27 fallback shape: a service-graph
// series whose client_k8s_pod_uid is empty surfaces as `external/<label>`
// with an empty labels map (distinguishable from the pattern-matched
// external case, which carries labels.pattern). The edge omits
// labels.cluster because the client side is external.
func buildMissingUIDFallback() graph.View {
	pod := &graph.PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	ext := &graph.ExternalNode{IDValue: "external/admin", NameValue: "admin", LabelsValue: map[string]string{}}
	edge := graph.NewEdge(graph.EdgeTypePodCallsPod, ext.IDValue, pod.IDValue, map[string]string{})
	return graph.View{Nodes: []graph.GraphNode{pod, ext}, Edges: []*graph.Edge{edge}}
}

// buildLinkRelation snapshots the span-link relation marking
// (add-span-link-logical-edges): a logical producer→consumer pod-calls-pod
// edge carries labels.relation="link", the producer's pod→broker
// pod-calls-service edge carries labels.relation="transport", and the broker's
// service-selects-pod fan-out carries no relation key (shared edge, never
// marked). Ordinary labels (cluster / namespace) are unaffected.
func buildLinkRelation() graph.View {
	producer := &graph.PodNode{IDValue: "cluster-alpha/p1", NameValue: "producer", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	consumer := &graph.PodNode{IDValue: "cluster-alpha/c1", NameValue: "consumer", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	broker := &graph.ServiceNode{IDValue: "cluster-alpha/messaging/nats", NameValue: "nats", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "messaging"}, IPAddressValue: []string{"10.0.0.9"}}
	brokerPod := &graph.PodNode{IDValue: "cluster-alpha/n1", NameValue: "nats-0", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "messaging"}}
	edges := []*graph.Edge{
		graph.NewEdge(graph.EdgeTypePodCallsPod, producer.IDValue, consumer.IDValue, map[string]string{"cluster": "cluster-alpha", "relation": "link"}),
		graph.NewEdge(graph.EdgeTypePodCallsService, producer.IDValue, broker.IDValue, map[string]string{"cluster": "cluster-alpha", "relation": "transport"}),
		graph.NewEdge(graph.EdgeTypeServiceSelectsPod, broker.IDValue, brokerPod.IDValue, map[string]string{"namespace": "messaging"}),
	}
	return graph.View{Nodes: []graph.GraphNode{producer, consumer, broker, brokerPod}, Edges: edges}
}

// buildPruneFalse snapshots `?prune=false`: the inventory view. A
// connectivity-disconnected pod keeps its whole storage chain (host node,
// claim, NetApp aggregate, owning controller) and an unreferenced podless node
// is admitted too — the shape that replaces the withdrawn ?name= / ?root=
// escape hatches.
func buildPruneFalse() graph.View {
	idle := &graph.PodNode{IDValue: "cluster-alpha/p9", NameValue: "idle", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-1"}}
	worker1 := &graph.K8sNode{IDValue: "cluster-alpha/worker-1", NameValue: "worker-1", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}
	worker9 := &graph.K8sNode{IDValue: "cluster-alpha/worker-9", NameValue: "worker-9", LabelsValue: map[string]string{"cluster": "cluster-alpha"}, ReadyStatusValue: "NotReady"}
	pvc := &graph.PVCNode{IDValue: "cluster-alpha/shop/idle-data", NameValue: "idle-data", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "volume": "data"}}
	aggr := &graph.NetAppAggrNode{IDValue: graph.NetAppAggrID("ontap-prod", "aggr2"), NameValue: "aggr2", LabelsValue: map[string]string{"ontap_cluster": "ontap-prod", "node": "ontap-prod-02"}}
	ctrl := &graph.NetAppNode{IDValue: graph.NetAppNodeID("ontap-prod", "ontap-prod-02"), NameValue: "ontap-prod-02", LabelsValue: map[string]string{"ontap_cluster": "ontap-prod"}}
	edges := []*graph.Edge{
		graph.NewEdge(graph.EdgeTypePodToNode, idle.IDValue, worker1.IDValue, nil),
		graph.NewEdge(graph.EdgeTypePodMountsPVC, idle.IDValue, pvc.IDValue, map[string]string{"claim_name": "idle-data"}),
		graph.NewEdge(graph.EdgeTypePVCToNetAppAggr, pvc.IDValue, aggr.IDValue, nil),
	}
	g := graph.NewGraph([]graph.GraphNode{idle, worker1, worker9, pvc, aggr, ctrl}, edges, time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC))
	return graph.Project(g, graph.Scope{Inventory: true})
}

// buildFilteredExternalPartner snapshots the filtered-build wire shape: a
// caller loaded by the request talks to a peer the request's selector did NOT
// load, so the peer renders as `external/<label>` with empty labels rather
// than as a synthesised pod. The edge keeps labels.cluster (the client side is
// a real pod) and carries no metrics (an external endpoint is never measured).
func buildFilteredExternalPartner() graph.View {
	caller := &graph.PodNode{IDValue: "cluster-alpha/p1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "node": "cluster-alpha/worker-0"}}
	node := &graph.K8sNode{IDValue: "cluster-alpha/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}
	outbound := &graph.ExternalNode{IDValue: graph.ExternalID("cart"), NameValue: "cart", LabelsValue: map[string]string{}}
	inbound := &graph.ExternalNode{IDValue: graph.ExternalID("frontend"), NameValue: "frontend", LabelsValue: map[string]string{}}
	edges := []*graph.Edge{
		graph.NewEdge(graph.EdgeTypePodCallsPod, caller.IDValue, outbound.IDValue, map[string]string{"cluster": "cluster-alpha"}),
		graph.NewEdge(graph.EdgeTypePodCallsPod, inbound.IDValue, caller.IDValue, map[string]string{}),
		graph.NewEdge(graph.EdgeTypePodToNode, caller.IDValue, node.IDValue, nil),
	}
	g := graph.NewGraph([]graph.GraphNode{caller, node, outbound, inbound}, edges, time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC))
	return graph.Project(g, graph.Scope{Namespaces: map[string]struct{}{"shop": {}}})
}

// buildWithREDMetrics exercises every wire shape of data.metrics in one body:
// full RED, partial RED (rate + error_rate, no p90), peer-resolved pod-to-pod
// with no metrics, a metric-less topology edge, and a tiny rate that serialises
// in exponent form (one request over a long window).
func buildWithREDMetrics() graph.View {
	client := &graph.PodNode{IDValue: "cluster-alpha/c1", NameValue: "checkout", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	server := &graph.PodNode{IDValue: "cluster-alpha/s1", NameValue: "cart", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	peer := &graph.PodNode{IDValue: "cluster-alpha/s2", NameValue: "direct", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	slow := &graph.PodNode{IDValue: "cluster-alpha/s3", NameValue: "rare", LabelsValue: map[string]string{"cluster": "cluster-alpha", "namespace": "shop"}}
	node := &graph.K8sNode{IDValue: "cluster-alpha/worker-0", NameValue: "worker-0", LabelsValue: map[string]string{"cluster": "cluster-alpha"}}

	erFull := 0.2
	p90 := 45.0
	erPartial := 0.0
	tiny := 3.86e-7
	erTiny := 6.7e-8

	edges := []*graph.Edge{
		// Full RED.
		graph.NewEdge(graph.EdgeTypePodCallsPod, client.IDValue, server.IDValue, map[string]string{"cluster": "cluster-alpha"}).
			WithMetrics(graph.EdgeMetrics{Rate: 5, ErrorRate: &erFull, P90ServerMs: &p90}),
		// Partial RED (no p90).
		graph.NewEdge(graph.EdgeTypePodCallsPod, client.IDValue, slow.IDValue, map[string]string{"cluster": "cluster-alpha"}).
			WithMetrics(graph.EdgeMetrics{Rate: 1, ErrorRate: &erPartial}),
		// Peer-resolved pod-to-pod — no metrics (half-observed call).
		graph.NewEdge(graph.EdgeTypePodCallsPod, client.IDValue, peer.IDValue, map[string]string{"cluster": "cluster-alpha"}),
		// Tiny rate (exponent form on the wire).
		graph.NewEdge(graph.EdgeTypePodCallsPod, server.IDValue, slow.IDValue, map[string]string{"cluster": "cluster-alpha"}).
			WithMetrics(graph.EdgeMetrics{Rate: tiny, ErrorRate: &erTiny}),
		// Topology edge — never carries metrics.
		graph.NewEdge(graph.EdgeTypePodToNode, client.IDValue, node.IDValue, nil),
	}
	return graph.View{
		Nodes: []graph.GraphNode{client, server, peer, slow, node},
		Edges: edges,
	}
}

// buildClusterIdentity snapshots the cluster-identity wire shape: two clusters
// composed from ONE raw name (`c1`) in different zone/environment pairs, plus an
// unstamped cluster that composed nothing. The identity is what every id prefix,
// `labels.cluster`, compound-group id and `clusters[]` entry carries; the raw
// name appears nowhere. The cross-identity edge is cross-cluster even though
// both sides are "c1".
func buildClusterIdentity() graph.View {
	usPod := &graph.PodNode{IDValue: "us-dev-c1/p1", NameValue: "checkout",
		LabelsValue: map[string]string{"cluster": "us-dev-c1", "namespace": "shop", "node": "us-dev-c1/worker-0"}}
	usNode := &graph.K8sNode{IDValue: "us-dev-c1/worker-0", NameValue: "worker-0",
		LabelsValue: map[string]string{"cluster": "us-dev-c1"}}
	usPVC := &graph.PVCNode{IDValue: "us-dev-c1/shop/checkout-data", NameValue: "checkout-data",
		LabelsValue: map[string]string{"cluster": "us-dev-c1", "namespace": "shop"}}
	usSvc := &graph.ServiceNode{IDValue: "us-dev-c1/shop/payments", NameValue: "payments",
		LabelsValue: map[string]string{"cluster": "us-dev-c1", "namespace": "shop"}}
	euPod := &graph.PodNode{IDValue: "eu-prod-c1/p2", NameValue: "payments",
		LabelsValue: map[string]string{"cluster": "eu-prod-c1", "namespace": "shop"}}
	// Unstamped: no az/env pair upstream, so its cluster stayed the raw name.
	plainPod := &graph.PodNode{IDValue: "cluster-beta/p3", NameValue: "ledger",
		LabelsValue: map[string]string{"cluster": "cluster-beta", "namespace": "billing"}}

	edges := []*graph.Edge{
		// Intra-identity call.
		graph.NewEdge(graph.EdgeTypePodCallsService, usPod.IDValue, usSvc.IDValue,
			map[string]string{"cluster": "us-dev-c1"}),
		// Same raw name, different identity → cross-cluster.
		graph.NewEdge(graph.EdgeTypePodCallsPod, usPod.IDValue, euPod.IDValue,
			map[string]string{"cluster": "us-dev-c1"}),
		graph.NewEdge(graph.EdgeTypePodToNode, usPod.IDValue, usNode.IDValue, nil),
		graph.NewEdge(graph.EdgeTypePodMountsPVC, usPod.IDValue, usPVC.IDValue, nil),
	}
	return graph.View{
		Nodes: []graph.GraphNode{usPod, usNode, usPVC, usSvc, euPod, plainPod},
		Edges: edges,
	}
}
