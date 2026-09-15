package promqlfake

import (
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

func TestQuerier_AppliesRenderedMatchers(t *testing.T) {
	pod := func(ns, name string) *model.Sample {
		return &model.Sample{Metric: model.Metric{
			"az": "zone-a", "namespace": model.LabelValue(ns), "pod": model.LabelValue(name),
		}, Value: 1}
	}
	f := New(map[promql.Query]model.Vector{
		promql.QPodInfo: {pod("shop", "orders-0"), pod("shop", "web.0"), pod("platform", "orders-0")},
	})
	sel := promql.Selector{AZ: []string{"zone-a"}, Namespace: []string{"shop"}}

	cases := map[string]struct {
		query string
		want  int
	}{
		"request matchers": {promql.Render(promql.QPodInfo, time.Minute, promql.LabelKeys{}, sel), 2},
		"no matcher":       {promql.Render(promql.QPodInfo, time.Minute, promql.LabelKeys{}, promql.Selector{}), 3},
		"quoted-meta alternation matches literally": {
			`last_over_time(kube_pod_info{pod=~"web\\.0|nope"}[1m])`, 1},
		"regex is fully anchored": {`last_over_time(kube_pod_info{pod=~"orders"}[1m])`, 0},
		"negative matchers":       {`last_over_time(kube_pod_info{namespace!="shop",pod!~"web.*"}[1m])`, 1},
		"absent label is empty":   {`last_over_time(kube_pod_info{node=""}[1m])`, 3},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := f.Instant(t.Context(), string(promql.QPodInfo), tc.query, time.Unix(1, 0))
			require.NoError(t, err)
			assert.Len(t, got, tc.want)
		})
	}
	assert.Len(t, f.QueriesFor(promql.QPodInfo), len(cases))
}

func TestQuerier_ScopeValues(t *testing.T) {
	f := New(nil)
	_, err := f.Instant(t.Context(), string(promql.QPodInfo),
		`last_over_time(kube_pod_info{pod=~"a|b"}[1m])`, time.Unix(1, 0))
	require.NoError(t, err)
	_, err = f.Instant(t.Context(), string(promql.QJobOwner),
		`last_over_time(kube_job_owner{owner_kind="CronJob",owner_is_controller="true",job_name="x"}[1m])`, time.Unix(1, 0))
	require.NoError(t, err)

	_, err = f.Instant(t.Context(), string(promql.QNodeInfo),
		`last_over_time(kube_node_info{node=~"ip-1\\.ec2|ip-2\\.ec2"}[1m])`, time.Unix(1, 0))
	require.NoError(t, err)

	assert.Equal(t, [][]string{{"a", "b"}}, f.ScopeValues(promql.QPodInfo, "pod"))
	assert.Equal(t, [][]string{{"ip-1.ec2", "ip-2.ec2"}}, f.ScopeValues(promql.QNodeInfo, "node"),
		"QuoteMeta escapes are removed: the entry is the literal value set")
	assert.Equal(t, [][]string{{"x"}}, f.ScopeValues(promql.QJobOwner, "job_name"))
	assert.Nil(t, f.ScopeValues(promql.QJobOwner, "no_such_label"))
}
