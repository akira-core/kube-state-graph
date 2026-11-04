package build

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// podSetCluster is the identity planKSM composes for zone-a / prod / c1.
const podSetCluster = "zone-a-prod-c1"

// podSetPod is one kube_pod_info series. at is the sample timestamp. The fake
// returns every matching series, which is the admission last_over_time gives a
// series that has a sample in the window.
type podSetPod struct {
	ns, name, uid, node string
	at                  model.Time
}

// podSetClaim is one claim and the pod names bound to it in the window.
type podSetClaim struct {
	ns, claim, pv string
	mounters      []string
}

// podSetEstate is a storage estate: Harvest from vols, one pod-info series per
// pod, node info for each named node, and the claim info plus bindings.
func podSetEstate(vols []vlrVol, pods []podSetPod, claims []podSetClaim) map[promql.Query]model.Vector {
	out := vlrHarvest(vols)
	nodes := map[string]struct{}{}
	for _, p := range pods {
		s := planKSM("namespace", p.ns, "pod", p.name, "uid", p.uid, "node", p.node)
		s.Timestamp = p.at
		out[promql.QPodInfo] = append(out[promql.QPodInfo], s)
		nodes[p.node] = struct{}{}
	}
	for _, n := range slices.Sorted(maps.Keys(nodes)) {
		out[promql.QNodeInfo] = append(out[promql.QNodeInfo], planKSM("node", n))
	}
	for _, c := range claims {
		out[promql.QPVCInfo] = append(out[promql.QPVCInfo],
			planKSM("namespace", c.ns, "persistentvolumeclaim", c.claim, "volumename", c.pv, "storageclass", "netapp-nas"))
		for _, m := range c.mounters {
			out[promql.QPVCBindings] = append(out[promql.QPVCBindings],
				planKSM("namespace", c.ns, "pod", m, "persistentvolumeclaim", c.claim))
		}
	}
	return out
}

func podSetAt(offset time.Duration) model.Time {
	return model.Time(vlrEnd.Add(offset).UnixMilli())
}

func podSetBody(t *testing.T, fx map[promql.Query]model.Vector, kind graph.StorageRootKind, value string, rollup promql.Rollup) cytoscape.Body {
	t.Helper()
	scope := vlrScope(t, kind, []string{value})
	g, err := New(promqlfake.New(fx), Options{}, nil, nil).BuildStorageWith(
		t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots, StorageOptions{IORollup: rollup})
	require.NoError(t, err)
	return vlrBody(t, g, scope)
}

func podSetFlow(body cytoscape.Body, tier, source, target string) (cytoscape.EdgeData, bool) {
	for _, e := range body.Elements.Edges {
		if e.Data.Labels["tier"] == tier && e.Data.Source == source && e.Data.Target == target {
			return e.Data, true
		}
	}
	return cytoscape.EdgeData{}, false
}

func podSetPodIDs(body cytoscape.Body) []string {
	var ids []string
	for _, n := range body.Elements.Nodes {
		if n.Data.Type == string(graph.NodeTypePod) {
			ids = append(ids, n.Data.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// podSetPVCPods is the pvc-pod edge identity: source, target, attribution.
// Weights are not part of it.
func podSetPVCPods(body cytoscape.Body) []string {
	var keys []string
	for _, e := range body.Elements.Edges {
		if e.Data.Labels["tier"] != graph.StorageTierPVCPod {
			continue
		}
		keys = append(keys, e.Data.Source+"|"+e.Data.Target+"|"+e.Data.Labels["attribution"])
	}
	slices.Sort(keys)
	return keys
}

func TestStoragePodSet_DeletedPodIsDrawn(t *testing.T) {
	fx := podSetEstate(
		[]vlrVol{{"trident_pvc_scratch", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 50}},
		[]podSetPod{{
			ns: "shop", name: "batch-x7k2p", uid: "uid-batch", node: "worker-1",
			at: podSetAt(-30 * time.Second),
		}},
		[]podSetClaim{{ns: "shop", claim: "scratch", pv: "pvc-scratch", mounters: []string{"batch-x7k2p"}}},
	)
	podID := graph.PodID(podSetCluster, "uid-batch")
	pvcID := graph.PVCID(podSetCluster, "shop", "scratch")
	roots := []struct {
		name  string
		kind  graph.StorageRootKind
		value string
	}{
		{"aggr", graph.StorageRootAggr, "aggr1"},
		{"node", graph.StorageRootNode, "worker-1"},
	}
	for _, root := range roots {
		t.Run(root.name, func(t *testing.T) {
			body := podSetBody(t, fx, root.kind, root.value, promql.RollupLast)
			var name string
			for _, n := range body.Elements.Nodes {
				if n.Data.ID == podID {
					name = n.Data.Name
				}
			}
			assert.Equal(t, "batch-x7k2p", name)
			_, ok := podSetFlow(body, graph.StorageTierPVCPod, pvcID, podID)
			assert.True(t, ok, "the deleted pod's pvc-pod edge is drawn")
		})
	}
}

func TestStoragePodSet_RecreatedPodIsTheNewestUID(t *testing.T) {
	fx := podSetEstate(
		[]vlrVol{{"trident_pvc_orders", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 100}},
		[]podSetPod{
			{ns: "shop", name: "orders-0", uid: "uid-old", node: "worker-1", at: podSetAt(-50 * time.Second)},
			{ns: "shop", name: "orders-0", uid: "uid-new", node: "worker-2", at: podSetAt(-10 * time.Second)},
		},
		[]podSetClaim{{ns: "shop", claim: "orders-data", pv: "pvc-orders", mounters: []string{"orders-0"}}},
	)
	newID := graph.PodID(podSetCluster, "uid-new")
	oldID := graph.PodID(podSetCluster, "uid-old")
	nodeID := graph.K8sNodeID(podSetCluster, "worker-2")

	body := podSetBody(t, fx, graph.StorageRootAggr, "aggr1", promql.RollupLast)
	assert.Equal(t, []string{newID}, podSetPodIDs(body))
	_, ok := podSetFlow(body, graph.StorageTierPodNode, newID, nodeID)
	assert.True(t, ok, "the pod sits on the newest incarnation's node")

	onNewest := podSetBody(t, fx, graph.StorageRootNode, "worker-2", promql.RollupLast)
	assert.Equal(t, []string{newID}, podSetPodIDs(onNewest))
	assert.NotContains(t, podSetPodIDs(onNewest), oldID)
}

func TestStoragePodSet_RolloutSplitsTheClaim(t *testing.T) {
	fx := podSetEstate(
		[]vlrVol{{"trident_pvc_webdata", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 200}},
		[]podSetPod{
			{ns: "shop", name: "web-7d9f-abcde", uid: "uid-web-old", node: "worker-1", at: podSetAt(-50 * time.Second)},
			{ns: "shop", name: "web-6c4b-fghij", uid: "uid-web-new", node: "worker-1", at: podSetAt(-10 * time.Second)},
		},
		[]podSetClaim{{
			ns: "shop", claim: "web-data", pv: "pvc-webdata",
			mounters: []string{"web-7d9f-abcde", "web-6c4b-fghij"},
		}},
	)
	pvcID := graph.PVCID(podSetCluster, "shop", "web-data")
	svmID := graph.NetAppSVMID("ontap-prod", "svm_shop")
	pods := []string{
		graph.PodID(podSetCluster, "uid-web-old"),
		graph.PodID(podSetCluster, "uid-web-new"),
	}
	roots := []struct {
		name  string
		kind  graph.StorageRootKind
		value string
	}{
		{"aggr", graph.StorageRootAggr, "aggr1"},
		{"node", graph.StorageRootNode, "worker-1"},
	}
	for _, root := range roots {
		t.Run(root.name, func(t *testing.T) {
			body := podSetBody(t, fx, root.kind, root.value, promql.RollupLast)
			assert.Equal(t, slices.Sorted(slices.Values(pods)), podSetPodIDs(body))
			for _, podID := range pods {
				e, ok := podSetFlow(body, graph.StorageTierPVCPod, pvcID, podID)
				require.True(t, ok, podID)
				assert.Equal(t, graph.AttributionSplit, e.Labels["attribution"])
				require.NotNil(t, e.Metrics)
				require.NotNil(t, e.Metrics.ReadOps)
				assert.InDelta(t, 100.0, *e.Metrics.ReadOps, 1e-9)
			}
			claim, ok := podSetFlow(body, graph.StorageTierSVMPVC, svmID, pvcID)
			require.True(t, ok)
			require.NotNil(t, claim.Metrics)
			require.NotNil(t, claim.Metrics.ReadOps)
			assert.InDelta(t, 200.0, *claim.Metrics.ReadOps, 1e-9)
			assert.NotContains(t, claim.Labels, "attribution")
		})
	}
}

func TestStoragePodSet_RollupDoesNotChangeThePodSet(t *testing.T) {
	fx := podSetEstate(
		[]vlrVol{{"trident_pvc_webdata", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 200}},
		[]podSetPod{
			{ns: "shop", name: "web-7d9f-abcde", uid: "uid-web-old", node: "worker-1", at: podSetAt(-50 * time.Second)},
			{ns: "shop", name: "web-6c4b-fghij", uid: "uid-web-new", node: "worker-1", at: podSetAt(-10 * time.Second)},
		},
		[]podSetClaim{{
			ns: "shop", claim: "web-data", pv: "pvc-webdata",
			mounters: []string{"web-7d9f-abcde", "web-6c4b-fghij"},
		}},
	)
	last := podSetBody(t, fx, graph.StorageRootAggr, "aggr1", promql.RollupLast)
	avg := podSetBody(t, fx, graph.StorageRootAggr, "aggr1", promql.RollupAvg)
	assert.Equal(t, podSetPodIDs(last), podSetPodIDs(avg))
	assert.Equal(t, podSetPVCPods(last), podSetPVCPods(avg))
	require.Len(t, podSetPVCPods(last), 2)
}
