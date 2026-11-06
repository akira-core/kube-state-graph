package kubegraph_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// The claim ids of namespaceEstate's aggr1 claims, under the composed cluster
// identity <az>-<env>-<cluster>.
const (
	ordersClaimID = "zone-a-prod-c1/shop/orders-data"
	batchClaimID  = "zone-a-prod-c1/other/batch-data"
)

// ioDeltaEstate is namespaceEstate with a second pod, shop/orders-1, mounting
// shop/orders-data beside orders-0 on the same Kubernetes node. An aggr1 root
// then reaches one claim with two mounters, whose figures the projection
// splits, and one with a single mounter (other/batch-data).
func ioDeltaEstate() map[promql.Query]model.Vector {
	out := namespaceEstate()
	out[promql.QPodInfo] = append(out[promql.QPodInfo],
		nsKSM("namespace", "shop", "pod", "orders-1", "uid", "uid-o1", "node", "worker-1"))
	out[promql.QPodOwner] = append(out[promql.QPodOwner], nsKSM("namespace", "shop", "pod", "orders-1",
		"owner_kind", "StatefulSet", "owner_name", "orders-1", "owner_is_controller", "true"))
	out[promql.QPVCBindings] = append(out[promql.QPVCBindings],
		nsKSM("namespace", "shop", "pod", "orders-1", "persistentvolumeclaim", "orders-data"))
	return out
}

// nsQoS is one qos_* row of FlexVol flexvol on ontap-prod/svm0, label-identical
// to namespaceEstate's QQoSReadOps rows; pairs add labels. A delta row must
// carry exactly the labels of the measured row it continues, or the reader
// treats the workload as relabelled inside the window and drops its delta.
func nsQoS(flexvol string, value model.SampleValue, pairs ...string) *model.Sample {
	s := nsHarvest(append([]string{"cluster", "ontap-prod", "svm", "svm0", "volume", flexvol}, pairs...)...)
	s.Value = value
	return s
}

// readOpsDelta is the promqlfake.Querier.Delta answering the qos_read_ops
// end-minus-start query: orders-data's FlexVol rose by 12.5 requests per
// second over the window, batch-data's fell by 40. Both are exact in binary,
// so every split and sum below is exact too.
func readOpsDelta() map[promql.Query]model.Vector {
	return map[promql.Query]model.Vector{promql.QQoSReadOps: {
		nsQoS("trident_pvc_orders", 12.5),
		nsQoS("trident_pvc_batch", -40),
	}}
}

// claimIO returns the IOMetrics a storage build baked for one claim: the
// storage-flow svm-pvc edge ending at the claim is the one edge a storage
// build weights (pvc-to-netapp-aggr is a /v1/graph edge the storage build
// never emits).
func claimIO(t *testing.T, g *graph.Graph, claimID string) *graph.IOMetrics {
	t.Helper()
	for _, e := range g.Edges {
		if e.Type == graph.EdgeTypeStorageFlow && e.Labels["tier"] == graph.StorageTierSVMPVC && e.Target == claimID {
			require.NotNil(t, e.IO, "svm-pvc edge of %s carries no I/O", claimID)
			return e.IO
		}
	}
	require.Failf(t, "no svm-pvc edge", "claim %s", claimID)
	return nil
}

// fmtReadOps renders read_ops and read_ops_delta for an exact comparison of
// dyadic values; an absent field renders as "-".
func fmtReadOps(readOps, delta *float64) string {
	f := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprintf("%g", *p)
	}
	return "read_ops=" + f(readOps) + " read_ops_delta=" + f(delta)
}

// projectedReadOps keys every storage-flow edge of a projected view by
// "tier source -> target".
func projectedReadOps(edges []*graph.Edge) map[string]string {
	out := make(map[string]string, len(edges))
	for _, e := range edges {
		var readOps, delta *float64
		if e.IO != nil {
			readOps, delta = e.IO.ReadOps, e.IO.ReadOpsDelta
		}
		out[e.Labels["tier"]+" "+e.Source+" -> "+e.Target] = fmtReadOps(readOps, delta)
	}
	return out
}

// bodyReadOps is projectedReadOps over a serialised body's edges.
func bodyReadOps(edges []cytoscape.Edge) map[string]string {
	out := make(map[string]string, len(edges))
	for _, e := range edges {
		var readOps, delta *float64
		if m := e.Data.Metrics; m != nil {
			readOps, delta = m.ReadOps, m.ReadOpsDelta
		}
		out[e.Data.Labels["tier"]+" "+e.Data.Source+" -> "+e.Data.Target] = fmtReadOps(readOps, delta)
	}
	return out
}

// wantAggr1ReadOps is the aggr1-rooted storage-flow view of ioDeltaEstate with
// readOpsDelta: each claim's svm-pvc edge carries its own figure and change,
// orders-data's two mounters each take half of both, and the shared aggr-svm
// and node-aggr hops carry the sum over the three flow units.
var wantAggr1ReadOps = map[string]string{
	"node-aggr netapp/ontap-prod/ontap-prod-01 -> netapp/ontap-prod/aggr/aggr1": "read_ops=200 read_ops_delta=-27.5",
	"aggr-svm netapp/ontap-prod/aggr/aggr1 -> netapp/ontap-prod/svm/svm0":       "read_ops=200 read_ops_delta=-27.5",
	"svm-pvc netapp/ontap-prod/svm/svm0 -> " + ordersClaimID:                    "read_ops=100 read_ops_delta=12.5",
	"svm-pvc netapp/ontap-prod/svm/svm0 -> " + batchClaimID:                     "read_ops=100 read_ops_delta=-40",
	"pvc-pod " + ordersClaimID + " -> zone-a-prod-c1/uid-o0":                    "read_ops=50 read_ops_delta=6.25",
	"pvc-pod " + ordersClaimID + " -> zone-a-prod-c1/uid-o1":                    "read_ops=50 read_ops_delta=6.25",
	"pvc-pod " + batchClaimID + " -> zone-a-prod-c1/uid-b0":                     "read_ops=100 read_ops_delta=-40",
	"pod-node zone-a-prod-c1/uid-o0 -> zone-a-prod-c1/worker-1":                 "read_ops=50 read_ops_delta=6.25",
	"pod-node zone-a-prod-c1/uid-o1 -> zone-a-prod-c1/worker-1":                 "read_ops=50 read_ops_delta=6.25",
	"pod-node zone-a-prod-c1/uid-b0 -> zone-a-prod-c1/worker-1":                 "read_ops=100 read_ops_delta=-40",
}

// An embedder reaches the change through both entry points: BuildStorageWith
// bakes it on the typed graph, graph.ProjectStorage sums and splits it across
// the tiers, and BuildStorageFromValues with io_delta=true serialises it as
// data.metrics.read_ops_delta.
func TestBuildStorageWith_IODeltaReachesTheEmbedder(t *testing.T) {
	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootAggr, []string{"aggr1"})
	require.NoError(t, err)
	q := promqlfake.New(ioDeltaEstate())
	q.Delta = readOpsDelta()
	// A write_ops change with no write_ops measurement: a delta never creates
	// a measurement, so it must not surface.
	q.Delta[promql.QQoSWriteOps] = model.Vector{nsQoS("trident_pvc_orders", 3), nsQoS("trident_pvc_batch", 3)}
	g, err := kubegraph.New(q, kubegraph.Options{}).BuildStorageWith(t.Context(),
		5*time.Minute, time.Unix(1700000300, 0).UTC(),
		promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}},
		scope.Roots, build.StorageOptions{IODelta: true})
	require.NoError(t, err)

	orders := claimIO(t, g, ordersClaimID)
	assert.Equal(t, "read_ops=100 read_ops_delta=12.5", fmtReadOps(orders.ReadOps, orders.ReadOpsDelta))
	batch := claimIO(t, g, batchClaimID)
	assert.Equal(t, "read_ops=100 read_ops_delta=-40", fmtReadOps(batch.ReadOps, batch.ReadOpsDelta))
	for _, io := range []*graph.IOMetrics{orders, batch} {
		assert.Nil(t, io.WriteOps)
		assert.Nil(t, io.WriteOpsDelta, "no write_ops measurement, so no write_ops change")
	}

	assert.Equal(t, wantAggr1ReadOps, projectedReadOps(graph.ProjectStorage(g, scope).Edges))
}

// The gate on a workload whose series changed inside the window, seen from the
// Engine: when shop/orders-data's FlexVol is measured under a second series
// (its policy group moved from silver to gold) that has no start value, the
// upstream still answers the OLD silver series' change, and the claim carries
// its measurement with no change at all. other/batch-data keeps its change.
func TestBuildStorageWith_IODeltaDropsARelabelledWorkload(t *testing.T) {
	silver := nsQoS("trident_pvc_orders", 60, "policy_group", "silver")
	gold := nsQoS("trident_pvc_orders", 40, "policy_group", "gold")
	cases := []struct {
		name        string
		measured    model.Vector
		ordersDelta bool
	}{
		// The control: the same delta row against an unchanged series set.
		{"the series set did not change", model.Vector{silver}, true},
		{"a series appeared inside the window", model.Vector{silver, gold}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			estate := namespaceEstate()
			readOps := slices.Clone(tc.measured)
			for _, s := range estate[promql.QQoSReadOps] {
				if s.Metric["volume"] != "trident_pvc_orders" {
					readOps = append(readOps, s)
				}
			}
			estate[promql.QQoSReadOps] = readOps
			q := promqlfake.New(estate)
			q.Delta = map[promql.Query]model.Vector{promql.QQoSReadOps: {
				nsQoS("trident_pvc_orders", 5, "policy_group", "silver"),
				nsQoS("trident_pvc_batch", -40),
			}}

			scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootAggr, []string{"aggr1"})
			require.NoError(t, err)
			g, err := kubegraph.New(q, kubegraph.Options{}).BuildStorageWith(t.Context(),
				5*time.Minute, time.Unix(1700000300, 0).UTC(),
				promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}},
				scope.Roots, build.StorageOptions{IODelta: true})
			require.NoError(t, err)

			orders := claimIO(t, g, ordersClaimID)
			require.NotNil(t, orders.ReadOps, "the claim keeps its measurement")
			if tc.ordersDelta {
				require.NotNil(t, orders.ReadOpsDelta)
				assert.Equal(t, "5", fmt.Sprintf("%g", *orders.ReadOpsDelta))
			} else {
				assert.Nil(t, orders.ReadOpsDelta, "a stale change of the old series is never reported")
			}
			batch := claimIO(t, g, batchClaimID)
			assert.Equal(t, "read_ops=100 read_ops_delta=-40", fmtReadOps(batch.ReadOps, batch.ReadOpsDelta),
				"the gate is per workload: another claim keeps its change")
		})
	}
}
