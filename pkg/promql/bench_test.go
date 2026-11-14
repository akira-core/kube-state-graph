package promql

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
)

// benchVector builds n distinct series of `labels` labels each. offset shifts
// the series identity so two vectors built with different offsets are
// disjoint and ones built with the same offset are identical.
func benchVector(n, labels, offset int) model.Vector {
	out := make(model.Vector, n)
	for i := range n {
		m := make(model.Metric, labels)
		m[model.MetricNameLabel] = "kube_pod_info"
		m["uid"] = model.LabelValue("pod-" + strconv.Itoa(i+offset))
		for l := 2; l < labels; l++ {
			m[model.LabelName("label_"+strconv.Itoa(l))] = model.LabelValue("value-" + strconv.Itoa(l))
		}
		out[i] = &model.Sample{Metric: m, Value: 1}
	}
	return out
}

// BenchmarkMergeVectors measures the fan-out merge every query passes through,
// cache hit or miss. One part is the single-backend deployment (the default);
// two parts overlap by half, the catch-all-beside-a-zone shape.
func BenchmarkMergeVectors(b *testing.B) {
	for _, parts := range []int{1, 2} {
		for _, n := range []int{10_000, 100_000} {
			for _, labels := range []int{12, 30} {
				in := make([]model.Vector, parts)
				for p := range parts {
					in[p] = benchVector(n, labels, p*n/2)
				}
				b.Run(fmt.Sprintf("parts=%d/series=%d/labels=%d", parts, n, labels), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						mergeVectors(in, false)
					}
				})
			}
		}
	}
}

// BenchmarkQueryCache_PutGet measures one insert plus one hit at a realistic
// key length, with the cache at steady state (every put evicts).
func BenchmarkQueryCache_PutGet(b *testing.B) {
	for _, qlen := range []int{256, 16 << 10} {
		b.Run(fmt.Sprintf("query_bytes=%d", qlen), func(b *testing.B) {
			c := newQueryCache(1000, time.Minute, nil, nil)
			vec := benchVector(10, 12, 0)
			// More distinct queries than the budget holds, built up front so
			// the loop measures the cache rather than string construction.
			base := strings.Repeat("x", qlen)
			queries := make([]string, 1024)
			for i := range queries {
				queries[i] = base + strconv.Itoa(i)
			}
			ts := time.Unix(1_700_000_000, 0)
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				q := queries[i%len(queries)]
				c.put(cacheKeyFor(1, q, ts), vec)
				if _, ok := c.get(cacheKeyFor(1, q, ts)); !ok {
					b.Fatal("miss after put")
				}
				i++
			}
		})
	}
}
