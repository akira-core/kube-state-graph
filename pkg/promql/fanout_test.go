package promql

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBackend records what it was asked and answers with a fixed vector or a
// fixed error. It stands in for a *Client so the fan-out can be exercised
// without a listening socket — the unit/integration boundary this repository
// draws.
type fakeBackend struct {
	mu      sync.Mutex
	vec     model.Vector
	err     error
	delay   time.Duration
	queries []string
	names   []string
	calls   int
}

func (f *fakeBackend) Instant(ctx context.Context, name, query string, _ time.Time) (model.Vector, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	f.calls++
	f.queries = append(f.queries, query)
	f.names = append(f.names, name)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.vec, nil
}

func (f *fakeBackend) seen() (calls int, queries []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]string(nil), f.queries...)
}

func sample(metric string, labels map[string]string, v float64) *model.Sample {
	m := model.Metric{model.MetricNameLabel: model.LabelValue(metric)}
	for k, val := range labels {
		m[model.LabelName(k)] = model.LabelValue(val)
	}
	return &model.Sample{Metric: m, Value: model.SampleValue(v)}
}

// --- mergeVectors --------------------------------------------------------

func TestMergeVectors_DisjointConcatenatesInOrder(t *testing.T) {
	a := model.Vector{
		sample("kube_pod_info", map[string]string{"pod": "a1"}, 1),
		sample("kube_pod_info", map[string]string{"pod": "a2"}, 1),
	}
	b := model.Vector{
		sample("kube_pod_info", map[string]string{"pod": "b1"}, 1),
	}
	got, conflicts := mergeVectors([]model.Vector{a, b})
	require.Len(t, got, 3)
	assert.Zero(t, conflicts)
	assert.Equal(t, model.LabelValue("a1"), got[0].Metric["pod"])
	assert.Equal(t, model.LabelValue("b1"), got[2].Metric["pod"])
}

func TestMergeVectors_ExactDuplicateCollapses(t *testing.T) {
	dup := map[string]string{"pod": "shared"}
	got, conflicts := mergeVectors([]model.Vector{
		{sample("kube_pod_info", dup, 1)},
		{sample("kube_pod_info", dup, 1)},
	})
	assert.Len(t, got, 1)
	assert.Zero(t, conflicts, "identical values are not a disagreement")
}

// A duplicate carrying a DIFFERENT value is a genuine ambiguity. The first
// contributor (the lexically-smallest backend) wins, deterministically, and
// the disagreement is counted rather than escalated.
func TestMergeVectors_DifferingDuplicateKeepsFirstAndCounts(t *testing.T) {
	dup := map[string]string{"pod": "shared"}
	got, conflicts := mergeVectors([]model.Vector{
		{sample("kube_pod_info", dup, 7)},
		{sample("kube_pod_info", dup, 9)},
	})
	require.Len(t, got, 1)
	assert.InDelta(t, 7.0, float64(got[0].Value), 0)
	assert.Equal(t, 1, conflicts)
}

// The merged result must be a pure function of the value sets, never of which
// backend answered first — that is what keeps the response body deterministic.
// The merge is ordered by backend NAME, so which backend answered first must
// not be observable. This drives the real fan-out twice with the two backends'
// latencies swapped — comparing mergeVectors against itself would prove
// nothing, since the ordering it depends on is the caller's.
func TestFanout_ResponseOrderIndependentOfBackendLatency(t *testing.T) {
	run := func(delayA, delayB time.Duration) string {
		fa := &fakeBackend{
			vec:   model.Vector{sample("kube_pod_info", map[string]string{"pod": "from-a"}, 1)},
			delay: delayA,
		}
		fb := &fakeBackend{
			vec:   model.Vector{sample("kube_pod_info", map[string]string{"pod": "from-b"}, 2)},
			delay: delayB,
		}
		r := routerWithFakes(t, twoZoneTable(t), map[string]*fakeBackend{"zone-a": fa, "zone-b": fb}, nil)
		got, err := r.QuerierFor(Selector{}).Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
		require.NoError(t, err)
		return got.String()
	}

	slowA := run(60*time.Millisecond, 0)
	slowB := run(0, 60*time.Millisecond)

	assert.Equal(t, slowA, slowB, "the merged result must not depend on which backend answered first")
	assert.Contains(t, slowA, "from-a")
	assert.Contains(t, slowA, "from-b")
}

// The same property for a DUPLICATED series: the surviving copy is always the
// lexically-smallest backend's, never whichever arrived first.
func TestFanout_DuplicateWinnerIndependentOfArrivalOrder(t *testing.T) {
	dup := map[string]string{"pod": "shared"}
	run := func(delayA, delayB time.Duration) model.Vector {
		fa := &fakeBackend{vec: model.Vector{sample("kube_pod_info", dup, 7)}, delay: delayA}
		fb := &fakeBackend{vec: model.Vector{sample("kube_pod_info", dup, 9)}, delay: delayB}
		r := routerWithFakes(t, twoZoneTable(t), map[string]*fakeBackend{"zone-a": fa, "zone-b": fb}, nil)
		got, err := r.QuerierFor(Selector{}).Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
		require.NoError(t, err)
		return got
	}

	for name, got := range map[string]model.Vector{
		"zone-a slow": run(60*time.Millisecond, 0),
		"zone-b slow": run(0, 60*time.Millisecond),
	} {
		require.Len(t, got, 1, "%s: the duplicate must collapse", name)
		assert.InDelta(t, 7.0, float64(got[0].Value), 0,
			"%s: zone-a wins on name order, not on latency", name)
	}
}

func TestMergeVectors_EmptyInputs(t *testing.T) {
	got, conflicts := mergeVectors(nil)
	assert.Empty(t, got)
	assert.Zero(t, conflicts)

	got, _ = mergeVectors([]model.Vector{{}, nil, {}})
	assert.Empty(t, got)
	assert.NotNil(t, got, "an empty merge is an empty vector, never nil")
}

// --- fan-out dispatch ----------------------------------------------------

// routerWithFakes builds a Router over tbl whose clients are the supplied
// fakes, keyed by backend name.
func routerWithFakes(t *testing.T, tbl *Table, fakes map[string]*fakeBackend, m Metrics) *Router {
	t.Helper()
	r, err := NewRouter(tbl, m, func(b Backend) (Querier, error) {
		f, ok := fakes[b.Name()]
		require.True(t, ok, "no fake registered for backend %q", b.Name())
		return f, nil
	})
	require.NoError(t, err)
	return r
}

func twoZoneTable(t *testing.T) *Table {
	t.Helper()
	tbl, err := NewTable([]Backend{
		be("zone-a", "http://vm-a:8428", allFamilies(), "zone-a"),
		be("zone-b", "http://vm-b:8428", allFamilies(), "zone-b"),
	})
	require.NoError(t, err)
	return tbl
}

func TestFanout_IdenticalQueryStringToEveryBackend(t *testing.T) {
	fa := &fakeBackend{vec: model.Vector{sample("kube_pod_info", map[string]string{"pod": "a"}, 1)}}
	fb := &fakeBackend{vec: model.Vector{sample("kube_pod_info", map[string]string{"pod": "b"}, 1)}}
	r := routerWithFakes(t, twoZoneTable(t), map[string]*fakeBackend{"zone-a": fa, "zone-b": fb}, nil)

	const rendered = `last_over_time(kube_pod_info[5m])`
	got, err := r.QuerierFor(Selector{}).Instant(context.Background(), string(QPodInfo), rendered, time.Unix(0, 0))
	require.NoError(t, err)
	assert.Len(t, got, 2, "both backends contribute")

	for _, f := range []*fakeBackend{fa, fb} {
		calls, queries := f.seen()
		assert.Equal(t, 1, calls)
		assert.Equal(t, []string{rendered}, queries, "the query string is never rewritten per backend")
	}
}

func TestFanout_DeduplicatesAcrossBackends(t *testing.T) {
	shared := map[string]string{"pod": "shared"}
	fa := &fakeBackend{vec: model.Vector{sample("kube_pod_info", shared, 1)}}
	fb := &fakeBackend{vec: model.Vector{sample("kube_pod_info", shared, 1)}}
	r := routerWithFakes(t, twoZoneTable(t), map[string]*fakeBackend{"zone-a": fa, "zone-b": fb}, nil)

	got, err := r.QuerierFor(Selector{}).Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestFanout_ZoneSelectsOneBackend(t *testing.T) {
	fa := &fakeBackend{vec: model.Vector{sample("kube_pod_info", map[string]string{"pod": "a"}, 1)}}
	fb := &fakeBackend{}
	r := routerWithFakes(t, twoZoneTable(t), map[string]*fakeBackend{"zone-a": fa, "zone-b": fb}, nil)

	_, err := r.QuerierFor(Selector{AZ: []string{"zone-a"}}).
		Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
	require.NoError(t, err)

	callsA, _ := fa.seen()
	callsB, _ := fb.seen()
	assert.Equal(t, 1, callsA)
	assert.Equal(t, 0, callsB, "a zone-scoped request never reaches the other zone's store")
}

// The service-graph family accepts no request dimension, so a zone-scoped
// request must still reach every backend serving it — narrowing it would drop
// edges the loaded topology needs.
func TestFanout_ServiceGraphIgnoresZone(t *testing.T) {
	fa := &fakeBackend{}
	fb := &fakeBackend{}
	r := routerWithFakes(t, twoZoneTable(t), map[string]*fakeBackend{"zone-a": fa, "zone-b": fb}, nil)

	_, err := r.QuerierFor(Selector{AZ: []string{"zone-a"}}).
		Instant(context.Background(), string(QServiceGraphTotal), "q", time.Unix(0, 0))
	require.NoError(t, err)

	callsA, _ := fa.seen()
	callsB, _ := fb.seen()
	assert.Equal(t, 1, callsA)
	assert.Equal(t, 1, callsB)
}

func TestFanout_BackendErrorFailsTheQueryAndNamesIt(t *testing.T) {
	fa := &fakeBackend{vec: model.Vector{sample("kube_pod_info", map[string]string{"pod": "a"}, 1)}}
	fb := &fakeBackend{err: errors.New("connection refused")}
	r := routerWithFakes(t, twoZoneTable(t), map[string]*fakeBackend{"zone-a": fa, "zone-b": fb}, nil)

	got, err := r.QuerierFor(Selector{}).Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `backend "zone-b"`)
	assert.Contains(t, err.Error(), "connection refused")
	assert.Nil(t, got, "no partial vector is returned — a partial graph is worse than an error")
}

func TestFanout_UnmatchedZoneReturnsEmptyWithoutQuerying(t *testing.T) {
	fa := &fakeBackend{}
	fb := &fakeBackend{}
	r := routerWithFakes(t, twoZoneTable(t), map[string]*fakeBackend{"zone-a": fa, "zone-b": fb}, nil)

	got, err := r.QuerierFor(Selector{AZ: []string{"zone-z"}}).
		Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
	require.NoError(t, err, "an empty filtered result is a legitimate empty graph, not an error")
	assert.Empty(t, got)

	callsA, _ := fa.seen()
	callsB, _ := fb.seen()
	assert.Zero(t, callsA)
	assert.Zero(t, callsB, "no upstream call is made for a zone nothing serves")
}

func TestFanout_UnknownQueryNameFailsLoudly(t *testing.T) {
	r := routerWithFakes(t, twoZoneTable(t),
		map[string]*fakeBackend{"zone-a": {}, "zone-b": {}}, nil)

	_, err := r.QuerierFor(Selector{}).Instant(context.Background(), "not_a_metric", "q", time.Unix(0, 0))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no upstream family declared")
}

// --- metrics upgrade -----------------------------------------------------

// plainMetrics implements only the required Metrics methods, not the optional
// RouterMetrics upgrade — the shape an external embedder may supply.
type plainMetrics struct{ failures int }

func (p *plainMetrics) ObserveQueryDuration(string, float64) {}
func (p *plainMetrics) IncQueryFailure(string)               { p.failures++ }

type routerMetricsRecorder struct {
	plainMetrics
	backends       []string
	backendFails   []string
	reloadOutcomes []string
}

func (r *routerMetricsRecorder) SetBackends(names []string) { r.backends = names }
func (r *routerMetricsRecorder) IncBackendQueryFailure(b string) {
	r.backendFails = append(r.backendFails, b)
}
func (r *routerMetricsRecorder) IncBackendConfigReload(result string) {
	r.reloadOutcomes = append(r.reloadOutcomes, result)
}

func TestRouterMetrics_PlainMetricsIsANoop(t *testing.T) {
	m := &plainMetrics{}
	fb := &fakeBackend{err: errors.New("boom")}
	r := routerWithFakes(t, twoZoneTable(t),
		map[string]*fakeBackend{"zone-a": {}, "zone-b": fb}, m)

	// Must not panic, and must record nothing routing-specific.
	_, err := r.QuerierFor(Selector{}).Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
	require.Error(t, err)
	assert.Zero(t, m.failures, "the fan-out records backend failures on the optional interface only")
}

func TestRouterMetrics_UpgradeRecorded(t *testing.T) {
	m := &routerMetricsRecorder{}
	fb := &fakeBackend{err: errors.New("boom")}
	r := routerWithFakes(t, twoZoneTable(t),
		map[string]*fakeBackend{"zone-a": {}, "zone-b": fb}, m)

	assert.Equal(t, []string{"zone-a", "zone-b"}, m.backends, "the backends are recorded at construction")

	_, err := r.QuerierFor(Selector{}).Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
	require.Error(t, err)
	assert.Equal(t, []string{"zone-b"}, m.backendFails)
}

func TestRouterMetricsOf_NilMetrics(t *testing.T) {
	rm := routerMetricsOf(nil)
	require.NotNil(t, rm)
	rm.SetBackends([]string{"a", "b", "c"})
	rm.IncBackendQueryFailure("x")
	rm.IncBackendConfigReload(ReloadResultOK)
}
