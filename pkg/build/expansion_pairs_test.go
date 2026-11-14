package build

import (
	"fmt"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
)

// uncoveredAggrPairs and uncoveredControllerPairs must answer exactly what the
// root lists say, whatever their representation: a bare cluster covers every
// aggregate and controller of that filer, a bare aggregate covers that name on
// every filer, and a qualified aggregate covers that filer's alone.
func TestUncoveredPairs_RootCoverage(t *testing.T) {
	row := func(oc, aggr, node string) *model.Sample {
		return &model.Sample{Metric: model.Metric{
			"cluster": model.LabelValue(oc), "aggr": model.LabelValue(aggr), "node": model.LabelValue(node),
		}}
	}
	rows := model.Vector{
		row("ontap-a", "aggr1", "a-01"), row("ontap-a", "aggr2", "a-02"),
		row("ontap-b", "aggr1", "b-01"), row("ontap-b", "aggr3", "b-01"),
		row("ontap-c", "aggr3", "c-01"), row("ontap-c", "aggr4", "c-02"),
		row("", "aggr9", "x-01"), row("ontap-d", "", ""),
	}
	plan := topologyPlan{
		volumeClusters:  []string{"ontap-a"},
		volumeAggrs:     []string{"aggr1"},
		volumeAggrPairs: map[string][]string{"ontap-c": {"aggr3"}},
	}

	assert.Equal(t, map[string][]string{
		"ontap-b": {"aggr3"},
		"ontap-c": {"aggr4"},
	}, uncoveredAggrPairs(rows, plan))
	assert.Equal(t, map[string][]string{
		"ontap-b": {"b-01"},
		"ontap-c": {"c-01", "c-02"},
	}, uncoveredControllerPairs([]model.Vector{rows}, plan, map[string]struct{}{"x-01": {}}))
}

// BenchmarkUncoveredAggrPairs measures the reached-component scope over a
// large rooted read: every row is tested against every root list.
func BenchmarkUncoveredAggrPairs(b *testing.B) {
	var rows model.Vector
	for i := range 20000 {
		rows = append(rows, &model.Sample{Metric: model.Metric{
			"cluster": model.LabelValue(fmt.Sprintf("ontap-%03d", i%200)),
			"aggr":    model.LabelValue(fmt.Sprintf("aggr%03d", i%500)),
		}})
	}
	plan := topologyPlan{volumeAggrPairs: map[string][]string{}}
	for i := range 400 {
		plan.volumeAggrs = append(plan.volumeAggrs, fmt.Sprintf("root-aggr%03d", i))
		oc := fmt.Sprintf("ontap-%03d", i%200)
		plan.volumeAggrPairs[oc] = append(plan.volumeAggrPairs[oc], fmt.Sprintf("q-aggr%03d", i))
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = uncoveredAggrPairs(rows, plan)
	}
}
