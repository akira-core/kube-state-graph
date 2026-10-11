package build

import (
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

// parityRoot is one root kind the harness projects both builds with.
type parityRoot struct {
	kind   graph.StorageRootKind
	values []string
}

// TestStorageParityHarness is the oracle for the by-reference storage read.
// Each corpus fixture is built twice — once under fullPlan (the unrestricted
// read of the same zone) and once under the storage plan — then projected and
// serialised. The bodies must match. Containers are the one attribute fullPlan
// still resolves and the storage body does not carry, so they are stripped
// from the control before the comparison, and only when the projected body
// actually drew a pod.
//
// The corpus is the set a completion bug would move: takeover, clone,
// cross-filer FlexVol collision, an RWX claim, a pod rescheduled inside the
// window, an inherited Application, a FlexGroup claim, and a flowless root of
// every kind. It runs against whatever storagePlan currently is, so a seed
// that changes a body fails here before any golden moves.
func TestStorageParityHarness(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		fx    map[promql.Query]model.Vector
		roots []parityRoot
		check func(t *testing.T, root parityRoot, body cytoscape.Body)
	}{
		{
			name: "takeover",
			fx: vlrEstate(vlrHarvest([]vlrVol{
				{"trident_pvc_orders", "ontap-prod", "ontap-prod-02", "aggr1", "svm_shop", 300},
				{"prod_unrelated_1", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 7},
				{"trident_pvc_redis", "ontap-prod", "ontap-prod-02", "aggr2", "svm_platform", 100},
			})),
			roots: parityEveryKind(map[graph.StorageRootKind]string{
				graph.StorageRootONTAPCluster: "ontap-prod",
				graph.StorageRootONTAPNode:    "ontap-prod-01",
				graph.StorageRootAggr:         "aggr1",
				graph.StorageRootSVM:          "svm_shop",
				graph.StorageRootNode:         "worker-1",
				graph.StorageRootPod:          "shop/orders-0",
				graph.StorageRootPVC:          "shop/orders-data",
				graph.StorageRootPV:           "pvc-orders",
				graph.StorageRootApplication:  "shop-orders",
			}),
			check: func(t *testing.T, root parityRoot, body cytoscape.Body) {
				if root.kind != graph.StorageRootAggr {
					return
				}
				assert.True(t, parityHasID(body, "netapp/ontap-prod/ontap-prod-01"),
					"the owner vote is the lexically-smallest controller of the whole aggregate")
				assert.False(t, parityHasID(body, "netapp/ontap-prod/ontap-prod-02"))
			},
		},
		{
			name: "clone",
			fx:   vlrClone(),
			roots: parityEveryKind(map[graph.StorageRootKind]string{
				graph.StorageRootONTAPCluster: "ontap-prod",
				graph.StorageRootONTAPNode:    "ontap-prod-00",
				graph.StorageRootAggr:         "aggr0",
				graph.StorageRootSVM:          "svm_shop",
				graph.StorageRootNode:         "worker-1",
				graph.StorageRootPod:          "shop/orders-0",
				graph.StorageRootPVC:          "shop/orders-data",
				graph.StorageRootPV:           "pvc-orders",
				graph.StorageRootApplication:  "shop-orders",
			}),
			check: func(t *testing.T, root parityRoot, body cytoscape.Body) {
				if root.kind != graph.StorageRootAggr {
					return
				}
				assert.True(t, parityHasID(body, "netapp/ontap-prod/aggr/aggr0"),
					"the full candidate set picks the lexically-smaller aggregate")
				assert.True(t, parityHasID(body, "zone-a-prod-c1/uid-o0"))
			},
		},
		{
			name: "cross-filer FlexVol collision",
			fx: vlrEstate(vlrHarvest([]vlrVol{
				{"trident_pvc_orders", "ontap-prod", "ontap-prod-01", "aggr1", "svm_shop", 300},
				{"trident_pvc_orders", "ontap-lab", "ontap-lab-07", "aggr7", "svm_lab", 90},
				{"trident_pvc_redis", "ontap-prod", "ontap-prod-02", "aggr2", "svm_platform", 100},
			})),
			roots: parityEveryKind(map[graph.StorageRootKind]string{
				graph.StorageRootONTAPCluster: "ontap-lab",
				graph.StorageRootONTAPNode:    "ontap-lab-07",
				graph.StorageRootAggr:         "aggr7",
				graph.StorageRootSVM:          "svm_lab",
				graph.StorageRootNode:         "worker-1",
				graph.StorageRootPod:          "shop/orders-0",
				graph.StorageRootPVC:          "shop/orders-data",
				graph.StorageRootPV:           "pvc-orders",
				graph.StorageRootApplication:  "shop-orders",
			}),
			check: func(t *testing.T, root parityRoot, body cytoscape.Body) {
				if root.kind != graph.StorageRootONTAPCluster {
					return
				}
				assert.True(t, parityHasID(body, "zone-a-prod-c1/uid-o0"),
					"the lexically-smallest filer is ontap-lab, so the claim is retained there")
				assert.False(t, parityHasID(body, "netapp/ontap-prod/aggr/aggr1"))
			},
		},
		{
			name: "RWX claim across nodes",
			fx:   planEstate(),
			roots: parityEveryKind(map[graph.StorageRootKind]string{
				graph.StorageRootONTAPCluster: "ontap-prod",
				graph.StorageRootONTAPNode:    "ontap-prod-01",
				graph.StorageRootAggr:         "aggr1",
				graph.StorageRootSVM:          "svm_shop",
				graph.StorageRootNode:         "worker-1",
				graph.StorageRootPod:          "shop/beta-rwx-0",
				graph.StorageRootPVC:          "shop/shared-beta",
				graph.StorageRootPV:           "pvc-sharedbeta",
				graph.StorageRootApplication:  "beta",
			}),
			check: func(t *testing.T, root parityRoot, body cytoscape.Body) {
				if root.kind != graph.StorageRootAggr {
					return
				}
				assert.True(t, parityHasID(body, "zone-a-prod-c1/uid-br"), "shop/beta-rwx-0")
				assert.True(t, parityHasID(body, "zone-a-prod-c1/uid-as"), "shop/alpha-share-0, the other mounter")
			},
		},
		{
			name:  "pod rescheduled inside the window",
			fx:    parityRescheduledOrders(),
			roots: parityEveryKind(parityOrdersRoots()),
			check: func(t *testing.T, root parityRoot, body cytoscape.Body) {
				if root.kind != graph.StorageRootPod {
					return
				}
				assert.True(t, parityHasID(body, "zone-a-prod-c1/uid-orders-new"),
					"the newest incarnation is the pod the body draws")
				assert.False(t, parityHasID(body, "zone-a-prod-c1/uid-o0"))
			},
		},
		{
			name: "inherited Application",
			fx:   parityInheritedAAA(),
			// shop-orders covers the application kind; beta is the root the
			// inheritance oracle reads. Both are the same kind.
			roots: append(parityEveryKind(parityOrdersRoots()), parityRoot{graph.StorageRootApplication, []string{"beta"}}),
			check: func(t *testing.T, root parityRoot, body cytoscape.Body) {
				if root.kind != graph.StorageRootApplication || root.values[0] != "beta" {
					return
				}
				pvc := parityNode(body, "zone-a-prod-c1/shop/inherit-beta")
				require.NotNil(t, pvc, "the claim mounted by a beta pod is retained")
				assert.Equal(t, "aaa", pvc.Application, "inherited from the lexically-smallest mounter")
			},
		},
		{
			name: "FlexGroup claim",
			fx:   parityFlexGroup(),
			roots: parityEveryKind(map[graph.StorageRootKind]string{
				graph.StorageRootONTAPCluster: "ontap-prod",
				graph.StorageRootONTAPNode:    "ontap-prod-01",
				graph.StorageRootAggr:         "aggr1",
				graph.StorageRootSVM:          "svm_fg",
				graph.StorageRootNode:         "worker-1",
				graph.StorageRootPod:          "shop/web-0",
				graph.StorageRootPVC:          "shop/fg-data",
				graph.StorageRootPV:           "pvc-fg",
				graph.StorageRootApplication:  "storefront",
			}),
			check: func(t *testing.T, root parityRoot, body cytoscape.Body) {
				if root.kind != graph.StorageRootSVM {
					return
				}
				assert.True(t, parityHasID(body, "netapp/ontap-prod/svm/svm_fg"))
				assert.True(t, parityHasID(body, "zone-a-prod-c1/uid-w0"))
				pvc := parityNode(body, "zone-a-prod-c1/shop/fg-data")
				require.NotNil(t, pvc)
				assert.Empty(t, pvc.Labels["aggr"], "a FlexGroup claim has an SVM and no aggregate")
				assert.Equal(t, "svm_fg", pvc.Labels["svm"])
			},
		},
		{
			name:  "flowless root",
			fx:    parityFlowless(),
			roots: parityEveryKind(parityFlowlessRoots()),
			check: func(t *testing.T, root parityRoot, body cytoscape.Body) {
				want := map[graph.StorageRootKind]string{
					graph.StorageRootONTAPCluster: "netapp/ontap-empty/aggr/aggr-empty",
					graph.StorageRootONTAPNode:    "netapp/ontap-empty/ontap-empty-01",
					graph.StorageRootAggr:         "netapp/ontap-empty/aggr/aggr-empty",
					graph.StorageRootSVM:          "netapp/ontap-prod/svm/svm_lonely",
					graph.StorageRootNode:         "zone-a-prod-c1/worker-3",
					graph.StorageRootPod:          "zone-a-prod-c1/uid-w0",
					graph.StorageRootPVC:          "zone-a-prod-c1/platform/cache-data",
					graph.StorageRootPV:           "zone-a-prod-c1/platform/cache-data",
					graph.StorageRootApplication:  "zone-a-prod-c1/uid-w0",
				}
				assert.True(t, parityHasID(body, want[root.kind]), "kind %s draws its flowless root", root.kind)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seen := map[graph.StorageRootKind]bool{}
			for _, root := range tc.roots {
				seen[root.kind] = true
			}
			require.Len(t, seen, len(graph.StorageRootKinds), "every root kind is compared")
			for _, root := range tc.roots {
				t.Run(string(root.kind), func(t *testing.T) {
					t.Parallel()
					body := assertStorageParity(t, tc.fx, root)
					if tc.check != nil {
						tc.check(t, root, body)
					}
				})
			}
		})
	}
}

func parityEveryKind(values map[graph.StorageRootKind]string) []parityRoot {
	out := make([]parityRoot, 0, len(graph.StorageRootKinds))
	for _, kind := range graph.StorageRootKinds {
		out = append(out, parityRoot{kind, []string{values[kind]}})
	}
	return out
}

func parityOrdersRoots() map[graph.StorageRootKind]string {
	return map[graph.StorageRootKind]string{
		graph.StorageRootONTAPCluster: "ontap-prod",
		graph.StorageRootONTAPNode:    "ontap-prod-01",
		graph.StorageRootAggr:         "aggr1",
		graph.StorageRootSVM:          "svm_shop",
		graph.StorageRootNode:         "worker-2",
		graph.StorageRootPod:          "shop/orders-0",
		graph.StorageRootPVC:          "shop/orders-data",
		graph.StorageRootPV:           "pvc-orders",
		graph.StorageRootApplication:  "shop-orders",
	}
}

func parityFlowlessRoots() map[graph.StorageRootKind]string {
	return map[graph.StorageRootKind]string{
		graph.StorageRootONTAPCluster: "ontap-empty",
		graph.StorageRootONTAPNode:    "ontap-empty-01",
		graph.StorageRootAggr:         "aggr-empty",
		graph.StorageRootSVM:          "svm_lonely",
		graph.StorageRootNode:         "worker-3",
		graph.StorageRootPod:          "shop/web-0",
		graph.StorageRootPVC:          "platform/cache-data",
		graph.StorageRootPV:           "pvc-cache",
		graph.StorageRootApplication:  "storefront",
	}
}

func parityRescheduledOrders() map[promql.Query]model.Vector {
	fx := planEstate()
	newer := planKSM("namespace", "shop", "pod", "orders-0", "uid", "uid-orders-new", "node", "worker-2")
	newer.Timestamp = model.Time(5_000)
	fx[promql.QPodInfo] = append(fx[promql.QPodInfo], newer)
	return fx
}

func parityInheritedAAA() map[promql.Query]model.Vector {
	fx := planEstate()
	for _, s := range fx[promql.QStatefulSetAnnotations] {
		if s.Metric["statefulset"] == "zeta" {
			s.Metric[model.LabelName(trackingLabel)] = "aaa:apps/StatefulSet:shop/zeta"
		}
	}
	return fx
}

func parityFlexGroup() map[promql.Query]model.Vector {
	fx := planEstate()
	fx[promql.QPVCBindings] = append(fx[promql.QPVCBindings],
		planKSM("namespace", "shop", "pod", "web-0", "persistentvolumeclaim", "fg-data", "volume", "data"))
	fx[promql.QPVCInfo] = append(fx[promql.QPVCInfo],
		planKSM("namespace", "shop", "persistentvolumeclaim", "fg-data", "volumename", "pvc-fg", "storageclass", "netapp-nas"))
	fx[promql.QVolumeLabels] = append(fx[promql.QVolumeLabels],
		planHarvest("cluster", "ontap-prod", "node", "ontap-prod-01", "svm", "svm_fg", "volume", "trident_pvc_fg"))
	return fx
}

func parityFlowless() map[promql.Query]model.Vector {
	fx := planEstate()
	fx[promql.QAggrStatus] = append(fx[promql.QAggrStatus],
		planHarvest("cluster", "ontap-empty", "node", "ontap-empty-01", "aggr", "aggr-empty"))
	fx[promql.QNetAppNodeStatus] = append(fx[promql.QNetAppNodeStatus],
		planHarvest("cluster", "ontap-empty", "node", "ontap-empty-01"))
	fx[promql.QVolumeLabels] = append(fx[promql.QVolumeLabels],
		planHarvest("cluster", "ontap-prod", "node", "ontap-prod-09", "aggr", "aggr9", "svm", "svm_lonely", "volume", "lonely_no_claim"))
	return fx
}

// assertStorageParity builds fx under fullPlan and under the storage plan for
// root, projects both with that root, and requires byte-identical bodies once
// containers are removed from the control. It returns the agreed body.
func assertStorageParity(t *testing.T, fx map[promql.Query]model.Vector, root parityRoot) cytoscape.Body {
	t.Helper()
	scope, err := graph.NewStorageScope(nil, nil, root.kind, root.values)
	require.NoError(t, err)
	sel := promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}
	end := time.Unix(1, 0).UTC()

	fullQ := promqlfake.New(fx)
	full, err := New(fullQ, Options{}, nil, nil).buildStorage(t.Context(), time.Minute, end, sel, fullPlan)
	require.NoError(t, err)
	scopedQ := promqlfake.New(fx)
	scoped, err := New(scopedQ, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, end, sel, scope.Roots)
	require.NoError(t, err)

	fullBody := cytoscape.Serialise(full, graph.ProjectStorage(full, scope))
	scopedBody := cytoscape.Serialise(scoped, graph.ProjectStorage(scoped, scope))
	require.NotEmpty(t, scopedBody.Elements.Nodes, "a vacuous body would prove nothing")

	if parityHasType(fullBody, "pod") {
		require.NotZero(t, stripContainers(&fullBody), "the control must have resolved containers")
	}
	assert.Zero(t, stripContainers(&scopedBody), "the storage body carries no containers")
	assert.Empty(t, scopedQ.QueriesFor(promql.QPodContainerInfo))
	assert.NotEmpty(t, fullQ.QueriesFor(promql.QPodContainerInfo))
	assert.JSONEq(t, planBodyJSON(t, fullBody), planBodyJSON(t, scopedBody))
	return scopedBody
}

func parityHasID(body cytoscape.Body, id string) bool {
	return parityNode(body, id) != nil
}

func parityNode(body cytoscape.Body, id string) *cytoscape.NodeData {
	for i := range body.Elements.Nodes {
		if body.Elements.Nodes[i].Data.ID == id {
			return &body.Elements.Nodes[i].Data
		}
	}
	return nil
}

func parityHasType(body cytoscape.Body, typ string) bool {
	for _, n := range body.Elements.Nodes {
		if n.Data.Type == typ {
			return true
		}
	}
	return false
}
