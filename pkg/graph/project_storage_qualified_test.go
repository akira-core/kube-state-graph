package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// accept-multi-zone-storage-graph: a qualified `aggr=` / `svm=` value
// (`<ontap_cluster>/<name>`) roots exactly one filer's component; the bare form
// still roots that name on every filer.

// filer builds one ONTAP cluster's nodes.
func filer(oc, ctrl, aggr, svm string) (*NetAppNode, *NetAppAggrNode, *NetAppSVMNode) {
	return &NetAppNode{
		IDValue: NetAppNodeID(oc, ctrl), NameValue: ctrl,
		LabelsValue: map[string]string{"ontap_cluster": oc},
	}, &NetAppAggrNode{
		IDValue: NetAppAggrID(oc, aggr), NameValue: aggr,
		LabelsValue: map[string]string{"ontap_cluster": oc, "node": ctrl},
	}, &NetAppSVMNode{
		IDValue: NetAppSVMID(oc, svm), NameValue: svm,
		LabelsValue: map[string]string{"ontap_cluster": oc},
	}
}

// twoFilers: `aggr1` and `svm0` exist on ontap-prod and on ontap-lab, each with
// a mounted claim; ontap-lab additionally serves `svm_shop` on `aggr2`.
//
//	ontap-prod  aggr1 / svm0      orders-data  orders-0
//	ontap-lab   aggr1 / svm0      ledger-data  ledger-0
//	ontap-lab   aggr2 / svm_shop  shop-data    shop-0
func twoFilers() *Graph {
	pCtrl, pAggr, pSVM := filer("ontap-prod", "ontap-prod-01", "aggr1", "svm0")
	lCtrl, lAggr, lSVM := filer("ontap-lab", "ontap-lab-01", "aggr1", "svm0")
	_, lAggr2, lShopSVM := filer("ontap-lab", "ontap-lab-01", "aggr2", "svm_shop")

	orders, ledger, shop := stPVC("shop", "orders-data"), stPVC("shop", "ledger-data"), stPVC("shop", "shop-data")
	podO, podL, podS := stPod("shop", "orders-0", "uid-o", "worker-1"), stPod("shop", "ledger-0", "uid-l", "worker-2"), stPod("shop", "shop-0", "uid-s", "worker-3")
	n1, n2, n3 := stNode("worker-1"), stNode("worker-2"), stNode("worker-3")

	edges := stChain(pCtrl.ID(), pAggr.ID(), pSVM.ID(), orders.ID(), podO.ID(), n1.ID(), stIO(100), 1)
	edges = append(edges, stChain(lCtrl.ID(), lAggr.ID(), lSVM.ID(), ledger.ID(), podL.ID(), n2.ID(), stIO(200), 1)...)
	edges = append(edges, stChain(lCtrl.ID(), lAggr2.ID(), lShopSVM.ID(), shop.ID(), podS.ID(), n3.ID(), stIO(300), 1)...)
	return stGraph([]GraphNode{
		pCtrl, pAggr, pSVM, lCtrl, lAggr, lSVM, lAggr2, lShopSVM,
		orders, ledger, shop, podO, podL, podS, n1, n2, n3,
	}, edges)
}

func TestProjectStorage_BareAggregateRootsEveryFiler(t *testing.T) {
	ids := viewIDs(ProjectStorage(twoFilers(), scopeRoots(StorageRootAggr, "aggr1")))
	assert.True(t, ids[NetAppAggrID("ontap-prod", "aggr1")])
	assert.True(t, ids[NetAppAggrID("ontap-lab", "aggr1")])
	assert.True(t, ids[PVCID(stC, "shop", "orders-data")])
	assert.True(t, ids[PVCID(stC, "shop", "ledger-data")])
	assert.False(t, ids[PVCID(stC, "shop", "shop-data")], "aggr2 is not a root")
}

// Spec: "A qualified aggregate roots one filer".
func TestProjectStorage_QualifiedAggregateRootsOneFiler(t *testing.T) {
	v := ProjectStorage(twoFilers(), scopeRoots(StorageRootAggr, "ontap-prod/aggr1"))
	ids := viewIDs(v)
	assert.True(t, ids[NetAppAggrID("ontap-prod", "aggr1")])
	assert.True(t, ids[NetAppNodeID("ontap-prod", "ontap-prod-01")])
	assert.True(t, ids[NetAppSVMID("ontap-prod", "svm0")])
	assert.True(t, ids[PVCID(stC, "shop", "orders-data")])
	assert.True(t, ids[PodID(stC, "uid-o")])
	assert.True(t, ids[K8sNodeID(stC, "worker-1")])

	assert.False(t, ids[NetAppAggrID("ontap-lab", "aggr1")], "the same name on another filer is not a root")
	assert.False(t, ids[PVCID(stC, "shop", "ledger-data")], "and neither is a claim reachable only through it")
	assert.False(t, ids[PodID(stC, "uid-l")])
}

// Spec: "Bare and qualified values combine" — the qualified `svm0` roots
// ontap-prod's only; the bare `svm_shop` roots every filer's.
func TestProjectStorage_BareAndQualifiedSVMsCombine(t *testing.T) {
	ids := viewIDs(ProjectStorage(twoFilers(), scopeRoots(StorageRootSVM, "ontap-prod/svm0", "svm_shop")))
	assert.True(t, ids[NetAppSVMID("ontap-prod", "svm0")])
	assert.True(t, ids[NetAppSVMID("ontap-lab", "svm_shop")])
	assert.True(t, ids[PVCID(stC, "shop", "orders-data")])
	assert.True(t, ids[PVCID(stC, "shop", "shop-data")])
	assert.False(t, ids[NetAppSVMID("ontap-lab", "svm0")], "lab's svm0 is not a root")
	assert.False(t, ids[PVCID(stC, "shop", "ledger-data")])
}

// A qualified value alone is a root; one naming a filer that does not hold the
// component roots nothing (the typo case), never every filer's.
func TestProjectStorage_QualifiedValueNamingNoComponentRootsNothing(t *testing.T) {
	v := ProjectStorage(twoFilers(), scopeRoots(StorageRootAggr, "ontap-elsewhere/aggr1"))
	assert.Empty(t, v.Nodes)
	assert.Empty(t, v.Edges)
}

// Spec: "Qualified aggregate with no claims still shows on its filer only".
func TestProjectStorage_QualifiedAggregateWithNoClaimsStillShowsOnItsFilerOnly(t *testing.T) {
	pCtrl, pAggr, _ := filer("ontap-prod", "ontap-prod-03", "aggr9", "svm0")
	lCtrl, lAggr, _ := filer("ontap-lab", "ontap-lab-03", "aggr9", "svm0")
	g := stGraph([]GraphNode{pCtrl, pAggr, lCtrl, lAggr}, nil)

	ids := viewIDs(ProjectStorage(g, scopeRoots(StorageRootAggr, "ontap-prod/aggr9")))
	assert.True(t, ids[NetAppAggrID("ontap-prod", "aggr9")])
	assert.True(t, ids[NetAppNodeID("ontap-prod", "ontap-prod-03")], "its owning controller is drawn")
	assert.False(t, ids[NetAppAggrID("ontap-lab", "aggr9")])
	assert.False(t, ids[NetAppNodeID("ontap-lab", "ontap-lab-03")])
}

// The projection of a request is a pure function of its value SET.
func TestProjectStorage_QualifiedRootOrderIsIrrelevant(t *testing.T) {
	a := ProjectStorage(twoFilers(), scopeRoots(StorageRootSVM, "ontap-prod/svm0", "svm_shop", "ontap-lab/svm0"))
	b := ProjectStorage(twoFilers(), scopeRoots(StorageRootSVM, "ontap-lab/svm0", "ontap-prod/svm0", "svm_shop"))
	assert.Equal(t, viewIDs(a), viewIDs(b))
	assert.Equal(t, sortedTiers(a), sortedTiers(b))
}
