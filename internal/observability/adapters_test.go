package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seriesByBackend returns, per backend label value, the gauge value of an
// in-flight series, or the sample count of a slot-wait series.
func seriesByBackend(t *testing.T, c prometheus.Collector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	families, err := reg.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			var backend string
			for _, l := range m.GetLabel() {
				if l.GetName() == "backend" {
					backend = l.GetValue()
				}
			}
			switch {
			case m.GetGauge() != nil:
				out[backend] = m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				out[backend] = float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return out
}

// A reload that removes a backend removes its limiter series, keeps a kept
// backend's in-flight count intact, and a query still in flight on the
// removed backend does not resurrect its series when it finishes.
func TestSetBackends_RemovesStaleLimiterSeries(t *testing.T) {
	m := NewMetrics()
	m.SetBackends([]string{"zone-a", "zone-b"})
	m.AddInflight("zone-a", 1)
	m.AddInflight("zone-b", 1)
	m.ObserveSlotWait("zone-a", 0.1)
	m.ObserveSlotWait("zone-b", 0.1)

	m.SetBackends([]string{"zone-a", "zone-c"})

	assert.Equal(t, map[string]float64{"zone-a": 1}, seriesByBackend(t, m.UpstreamInflight),
		"zone-b's gauge is gone; zone-a keeps its in-flight query")
	assert.Equal(t, map[string]float64{"zone-a": 1}, seriesByBackend(t, m.UpstreamSlotWait))

	// The zone-b query bound to the previous table finishes.
	m.AddInflight("zone-b", -1)
	m.ObserveSlotWait("zone-b", 0.2)
	m.AddInflight("zone-a", -1)
	assert.Equal(t, map[string]float64{"zone-a": 0}, seriesByBackend(t, m.UpstreamInflight),
		"no zone-b series at -1, and zone-a's decrement lands on its kept count")
	assert.NotContains(t, seriesByBackend(t, m.UpstreamSlotWait), "zone-b")

	m.AddInflight("zone-c", 1)
	assert.InDelta(t, 1.0, seriesByBackend(t, m.UpstreamInflight)["zone-c"], 0, "a new backend is observed")
}

// Before any table is recorded every backend is observed, so a recorder used
// without a Router (an embedder's Guard) still sees its series.
func TestAddInflight_BeforeSetBackendsObservesEveryName(t *testing.T) {
	m := NewMetrics()
	m.AddInflight("", 1)
	m.ObserveSlotWait("", 0.1)
	assert.InDelta(t, 1.0, seriesByBackend(t, m.UpstreamInflight)[""], 0)
	assert.InDelta(t, 1.0, seriesByBackend(t, m.UpstreamSlotWait)[""], 0)
}

// The swap and the observations may race; run under -race.
func TestSetBackends_ConcurrentWithObservations(t *testing.T) {
	m := NewMetrics()
	m.SetBackends([]string{"zone-a"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			m.AddInflight("zone-a", 1)
			m.ObserveSlotWait("zone-a", 0)
			m.AddInflight("zone-a", -1)
		}
	}()
	for i := range 100 {
		if i%2 == 0 {
			m.SetBackends([]string{"zone-a", "zone-b"})
		} else {
			m.SetBackends([]string{"zone-a"})
		}
	}
	<-done
	assert.InDelta(t, 0.0, seriesByBackend(t, m.UpstreamInflight)["zone-a"], 0)
}
