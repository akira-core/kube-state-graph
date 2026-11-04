package kubegraph_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
