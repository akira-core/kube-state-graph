package kubegraph_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// An embedder passes the same query parameters as GET /v1/storage-graph.
// io_rollup=max changes the function on the six QoS workload queries; an
// unrecognised value is a *ParseError before any query.
func ExampleEngine_BuildStorageFromValues() {
	q := promqlfake.New(namespaceEstate())
	eng := kubegraph.New(q, kubegraph.Options{})
	params := url.Values{
		"start":     {"1700000000"},
		"end":       {"1700003600"},
		"az":        {"zone-a"},
		"env":       {"prod"},
		"aggr":      {"aggr1"},
		"io_rollup": {"max"},
	}
	_, err := eng.BuildStorageFromValues(context.Background(), params)
	prefix, _, _ := strings.Cut(q.QueriesFor(promql.QQoSReadOps)[0], "(")
	fmt.Println(err, prefix)

	params.Set("io_rollup", "p99")
	_, err = eng.BuildStorageFromValues(context.Background(), params)
	pe, _ := errors.AsType[*kubegraph.ParseError](err)
	fmt.Println(pe.Reason)

	// Output:
	// <nil> max_over_time
	// invalid_scope
}

// BuildStorageWith takes the rollup as a typed option. RollupAvg renders
// avg_over_time on each QoS workload series.
func ExampleEngine_BuildStorageWith() {
	q := promqlfake.New(namespaceEstate())
	eng := kubegraph.New(q, kubegraph.Options{})
	end := time.Unix(1700003600, 0).UTC()
	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootAggr, []string{"aggr1"})
	if err != nil {
		fmt.Println(err)
		return
	}
	_, err = eng.BuildStorageWith(context.Background(), time.Hour, end,
		promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}},
		scope.Roots, build.StorageOptions{IORollup: promql.RollupAvg})
	prefix, _, _ := strings.Cut(q.QueriesFor(promql.QQoSReadOps)[0], "(")
	fmt.Println(err, prefix)

	// Output:
	// <nil> avg_over_time
}

// StorageOptions.IODelta puts each claim's end-minus-start change beside its
// measurement: graph.IOMetrics.ReadOpsDelta on the claim's svm-pvc
// storage-flow edge, nil (never 0) when the start value is unknown.
// graph.ProjectStorage splits a change across the claim's mounters and sums it
// up the chain like the flow weights, over the claims that carry one, and
// cytoscape.Serialise writes it as data.metrics.read_ops_delta, the field
// GET /v1/storage-graph?io_delta=true returns. Here shop/orders-data (two
// mounters) rose by 12.5 requests per second and other/batch-data fell by 40,
// both on aggr1.
func ExampleEngine_BuildStorageWith_ioDelta() {
	q := promqlfake.New(ioDeltaEstate())
	q.Delta = readOpsDelta()
	eng := kubegraph.New(q, kubegraph.Options{})
	scope, err := graph.NewStorageScope(nil, nil, graph.StorageRootAggr, []string{"aggr1"})
	if err != nil {
		fmt.Println(err)
		return
	}
	g, err := eng.BuildStorageWith(context.Background(), 5*time.Minute, time.Unix(1700000300, 0).UTC(),
		promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}},
		scope.Roots, build.StorageOptions{IODelta: true})
	if err != nil {
		fmt.Println(err)
		return
	}

	lines := map[string][]string{}
	for _, e := range graph.ProjectStorage(g, scope).Edges {
		tier := e.Labels["tier"]
		// Print orders-data's pod shares; skip the rest of the workload side.
		if (tier == graph.StorageTierPVCPod && e.Source != ordersClaimID) || tier == graph.StorageTierPodNode {
			continue
		}
		if e.IO == nil || e.IO.ReadOps == nil || e.IO.ReadOpsDelta == nil {
			continue
		}
		lines[tier] = append(lines[tier], fmt.Sprintf("%s %s -> %s read_ops=%g read_ops_delta=%+g",
			tier, path.Base(e.Source), path.Base(e.Target), *e.IO.ReadOps, *e.IO.ReadOpsDelta))
	}
	for _, tier := range graph.StorageTiers {
		slices.Sort(lines[tier])
		for _, line := range lines[tier] {
			fmt.Println(line)
		}
	}

	// Output:
	// node-aggr ontap-prod-01 -> aggr1 read_ops=200 read_ops_delta=-27.5
	// aggr-svm aggr1 -> svm0 read_ops=200 read_ops_delta=-27.5
	// svm-pvc svm0 -> batch-data read_ops=100 read_ops_delta=-40
	// svm-pvc svm0 -> orders-data read_ops=100 read_ops_delta=+12.5
	// pvc-pod orders-data -> uid-o0 read_ops=50 read_ops_delta=+6.25
	// pvc-pod orders-data -> uid-o1 read_ops=50 read_ops_delta=+6.25
}

// io_delta=true adds, per QoS family and chunk, one query returning each
// series' end-minus-start change; an unrecognised value is a *ParseError before
// any query.
func ExampleEngine_BuildStorageFromValues_ioDelta() {
	q := promqlfake.New(namespaceEstate())
	eng := kubegraph.New(q, kubegraph.Options{})
	params := url.Values{
		"start":    {"1700000000"},
		"end":      {"1700000300"},
		"az":       {"zone-a"},
		"env":      {"prod"},
		"aggr":     {"aggr1"},
		"io_delta": {"true"},
	}
	_, err := eng.BuildStorageFromValues(context.Background(), params)
	fmt.Println(err)
	for _, query := range q.QueriesFor(promql.QQoSReadOps) {
		if strings.Contains(query, "] offset ") {
			fmt.Println(query)
		}
	}

	params.Set("io_delta", "yes")
	_, err = eng.BuildStorageFromValues(context.Background(), params)
	pe, _ := errors.AsType[*kubegraph.ParseError](err)
	fmt.Println(pe.Reason)

	// Output:
	// <nil>
	// last_over_time(qos_read_ops{volume=~"trident_pvc_batch|trident_pvc_orders"}[5m]) - last_over_time(qos_read_ops{volume=~"trident_pvc_batch|trident_pvc_orders"}[5m] offset 5m)
	// invalid_scope
}
