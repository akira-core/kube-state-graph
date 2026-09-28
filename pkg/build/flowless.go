package build

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// readFlowlessHarvest reads the gauge families that materialise a storage
// root no claim reaches. An aggr root reads aggregate gauges by aggregate
// name, then controller families for the owners those rows and the rooted
// volume-label rows name. An ontap_node root reads controller families by
// controller name. An ontap_cluster root reads both families for the filers
// it names. The read is skipped when the plan carries no such scope, which
// is how a caller keeps the zone-wide gauge read.
func readFlowlessHarvest(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	plan topologyPlan,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	volumeLabelsDone <-chan struct{},
) (err error) {
	defer recoverScopedPanic(ctx, promql.QAggrStatus, &err)
	switch plan.kind {
	case graph.StorageRootAggr:
		if err := issueHarvestByName(ctx, q, window, end, opts, sel, v, scopeMu, promql.AggrPairQueries, plan.volumeAggrs); err != nil {
			return err
		}
		if !waitSignal(ctx, volumeLabelsDone) {
			return nil
		}
		return issueHarvestByName(ctx, q, window, end, opts, sel, v, scopeMu, promql.NetAppNodePairQueries,
			controllerNames(v.AggrStatus, v.AggrSpaceUsed, v.AggrSpaceTotal, v.VolumeLabels))
	case graph.StorageRootONTAPNode:
		return issueHarvestByName(ctx, q, window, end, opts, sel, v, scopeMu, promql.NetAppNodePairQueries, plan.volumeNodes)
	case graph.StorageRootONTAPCluster:
		return issueHarvestByCluster(ctx, q, window, end, opts, sel, v, scopeMu,
			slices.Concat(promql.AggrPairQueries, promql.NetAppNodePairQueries), plan.volumeClusters)
	default:
		return nil
	}
}

func waitSignal(ctx context.Context, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func issueHarvestByName(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	families []promql.Query,
	names []string,
) error {
	return issueHarvestScoped(ctx, q, window, end, opts, sel, v, scopeMu, families, names, promql.RenderHarvestByName)
}

func issueHarvestByCluster(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	families []promql.Query,
	clusters []string,
) error {
	return issueHarvestScoped(ctx, q, window, end, opts, sel, v, scopeMu, families, clusters, promql.RenderHarvestByCluster)
}

func issueHarvestScoped(
	ctx context.Context,
	q promql.Querier,
	window time.Duration,
	end time.Time,
	opts Options,
	sel promql.Selector,
	v *topologyVectors,
	scopeMu *sync.Mutex,
	families []promql.Query,
	values []string,
	render func(promql.Query, time.Duration, promql.LabelKeys, promql.Selector, []string) (string, bool),
) error {
	if len(values) == 0 {
		return nil
	}
	reserve := promql.RequestMatcherCost(promql.QAggrStatus, opts.LabelKeys, sel)
	fams := make([]scopedFamily, 0, len(families))
	for _, fam := range families {
		dst := harvestDst(v, fam)
		if dst == nil {
			continue
		}
		fams = append(fams, scopedFamily{
			query:         fam,
			dst:           dst,
			scope:         values,
			budgetReserve: reserve,
			render: func(chunk []string) (string, bool) {
				return render(fam, window, opts.LabelKeys, sel, chunk)
			},
		})
	}
	return issueScopedFamilies(ctx, q, window, end, opts, sel, v, scopeMu, fams)
}

func harvestDst(v *topologyVectors, q promql.Query) *model.Vector {
	switch q {
	case promql.QAggrStatus:
		return &v.AggrStatus
	case promql.QAggrSpaceUsed:
		return &v.AggrSpaceUsed
	case promql.QAggrSpaceTotal:
		return &v.AggrSpaceTotal
	case promql.QNetAppNodeStatus:
		return &v.NetAppNodeStatus
	case promql.QNetAppNodeLabels:
		return &v.NetAppNodeLabels
	case promql.QNetAppNodeCPUBusy:
		return &v.NetAppNodeCPUBusy
	case promql.QNetAppNodeTotalOps:
		return &v.NetAppNodeTotalOps
	case promql.QNetAppNodeTotalLatency:
		return &v.NetAppNodeTotalLatency
	case promql.QNetAppNodeTotalData:
		return &v.NetAppNodeTotalData
	case promql.QQoSPolicyFixedMaxIOPS:
		return &v.QoSPolicyMaxIOPS
	case promql.QQoSPolicyFixedMaxMBps:
		return &v.QoSPolicyMaxMBps
	default:
		return nil
	}
}

func controllerNames(vecs ...model.Vector) []string {
	seen := make(map[string]struct{})
	for _, vec := range vecs {
		for _, s := range vec {
			if node := string(s.Metric["node"]); node != "" {
				seen[node] = struct{}{}
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
