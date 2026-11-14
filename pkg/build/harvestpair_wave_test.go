package build

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// inflightQuerier counts how many queries it is answering at once, holding each
// for hold so concurrently launched queries overlap.
type inflightQuerier struct {
	hold     time.Duration
	cur, max atomic.Int32
	calls    atomic.Int32
}

func (q *inflightQuerier) Instant(_ context.Context, name, _ string, _ time.Time) (model.Vector, error) {
	n := q.cur.Add(1)
	for {
		m := q.max.Load()
		if n <= m || q.max.CompareAndSwap(m, n) {
			break
		}
	}
	q.calls.Add(1)
	time.Sleep(q.hold)
	q.cur.Add(-1)
	return model.Vector{{Metric: model.Metric{"__name__": model.LabelValue(name), "cluster": "c"}}}, nil
}

// issueHarvestPairMap reads several families, each in several chunks. Every
// query it issues shares ONE scopeConcurrency bound: a nested per-family
// errgroup would multiply the bound by the number of families and let a
// single build hold that many per-store slots at once.
func TestIssueHarvestPairMap_OneConcurrencyBoundAcrossFamilies(t *testing.T) {
	byCluster := map[string][]string{}
	for i := range 2 * scopeConcurrency {
		oc := fmt.Sprintf("ontap-%02d", i)
		byCluster[oc] = []string{"n1"}
	}
	q := &inflightQuerier{hold: 20 * time.Millisecond}
	var v topologyVectors
	var mu sync.Mutex

	require.NoError(t, issueHarvestPairMap(t.Context(), q, time.Minute, time.Unix(1, 0).UTC(),
		Options{}, promql.Selector{}, &v, &mu, promql.NetAppNodePairQueries, byCluster))

	assert.Equal(t, int32(len(promql.NetAppNodePairQueries)*2*scopeConcurrency), q.calls.Load(),
		"one query per (family, ONTAP cluster)")
	assert.LessOrEqual(t, q.max.Load(), int32(scopeConcurrency),
		"every family's chunks share one bound")
}

// The merge keeps the flowless read's rows first, then each chunk's rows in
// chunk order, de-duplicated by label set across all of them.
func TestMergeVolumeLabels_VariadicKeepsOrderAndDeduplicates(t *testing.T) {
	s := func(v string) *model.Sample {
		return &model.Sample{Metric: model.Metric{"volume": model.LabelValue(v)}}
	}
	base := model.Vector{s("a"), s("b")}
	got := mergeVolumeLabels(base, model.Vector{s("b"), s("c")}, nil, model.Vector{s("c"), s("d"), s("a")})

	names := make([]string, 0, len(got))
	for _, x := range got {
		names = append(names, string(x.Metric["volume"]))
	}
	assert.Equal(t, []string{"a", "b", "c", "d"}, names)
	assert.Equal(t, base, mergeVolumeLabels(base), "no extra returns the base unchanged")
	assert.Len(t, base, 2, "the base is never appended to in place")
}

// BenchmarkMergeVolumeLabels compares the merge issueHarvestPairMap used to
// run — chunk parts folded pairwise, then folded into the destination, which
// re-fingerprints the accumulated vector on every step — with the single
// variadic pass.
func BenchmarkMergeVolumeLabels(b *testing.B) {
	const parts, perPart = 16, 500
	chunks := make([]model.Vector, parts)
	for p := range parts {
		for i := range perPart {
			chunks[p] = append(chunks[p], &model.Sample{Metric: model.Metric{
				"cluster": model.LabelValue(fmt.Sprintf("ontap-%02d", p)),
				"node":    model.LabelValue(fmt.Sprintf("node-%04d", i)),
			}})
		}
	}
	b.Run("pairwise", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var merged model.Vector
			for _, part := range chunks {
				merged = mergeVolumeLabels(merged, part)
			}
			_ = mergeVolumeLabels(nil, merged)
		}
	})
	b.Run("variadic", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = mergeVolumeLabels(nil, chunks...)
		}
	})
}
