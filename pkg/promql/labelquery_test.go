package promql

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/internal/testlog"
)

func labelQueryAt(fam Family, metric string) LabelQuery {
	return LabelQuery{Metric: metric, Family: fam, At: time.Unix(1, 0)}
}

func fakesFor(tbl *Table) map[string]*fakeBackend {
	out := make(map[string]*fakeBackend, tbl.Len())
	for _, b := range tbl.Backends() {
		out[b.Name()] = &fakeBackend{}
	}
	return out
}

func calledBackends(fakes map[string]*fakeBackend) []string {
	names := make([]string, 0, len(fakes))
	for n, f := range fakes {
		calls, _ := f.seen()
		if calls > 0 {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

func issuedQueries(fakes map[string]*fakeBackend) []string {
	var out []string
	for _, f := range fakes {
		_, qs := f.seen()
		out = append(out, qs...)
	}
	return out
}

func routerForTable(t *testing.T, tbl *Table, fakes map[string]*fakeBackend) *Router {
	t.Helper()
	return routerWithFakes(t, tbl, fakes, nil)
}

func assertNoUpstreamCall(t *testing.T, fakes map[string]*fakeBackend) {
	t.Helper()
	assert.Empty(t, calledBackends(fakes), "a rejected request must not reach any backend")
}

// --- 3. request type and validation --------------------------------------

func TestQueryLabels_UnknownFamilyErrorsWithoutCall(t *testing.T) {
	tbl := twoZoneTable(t)
	fakes := fakesFor(tbl)
	r := routerForTable(t, tbl, fakes)

	_, err := r.QueryLabels(t.Context(), labelQueryAt(Family("metrics"), "kube_pod_info"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown family "metrics"`)
	assertNoUpstreamCall(t, fakes)
}

func TestQueryLabels_MetricNameGrammar(t *testing.T) {
	tbl := twoZoneTable(t)

	t.Run("valid", func(t *testing.T) {
		fakes := fakesFor(tbl)
		r := routerForTable(t, tbl, fakes)
		_, err := r.QueryLabels(t.Context(), labelQueryAt(FamilyKSM, "node_memory_MemAvailable_bytes"))
		require.NoError(t, err)
		assert.NotEmpty(t, calledBackends(fakes))
	})

	t.Run("brace injection", func(t *testing.T) {
		fakes := fakesFor(tbl)
		r := routerForTable(t, tbl, fakes)
		_, err := r.QueryLabels(t.Context(), labelQueryAt(FamilyKSM, "kube_pod_info} or up{"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid metric name")
		assertNoUpstreamCall(t, fakes)
	})
}

func TestQueryLabels_LabelKeyGrammar(t *testing.T) {
	tbl := twoZoneTable(t)

	t.Run("dash in key", func(t *testing.T) {
		fakes := fakesFor(tbl)
		r := routerForTable(t, tbl, fakes)
		q := labelQueryAt(FamilyKSM, "kube_pod_info")
		q.Filters = map[string]string{"foo-bar": "x"}
		_, err := r.QueryLabels(t.Context(), q)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid label filter key")
		assertNoUpstreamCall(t, fakes)
	})

	t.Run("reserved __name__", func(t *testing.T) {
		fakes := fakesFor(tbl)
		r := routerForTable(t, tbl, fakes)
		q := labelQueryAt(FamilyKSM, "kube_pod_info")
		q.Filters = map[string]string{model.MetricNameLabel: "other"}
		_, err := r.QueryLabels(t.Context(), q)
		require.Error(t, err)
		assert.Contains(t, err.Error(), model.MetricNameLabel)
		assert.Contains(t, err.Error(), "reserved")
		assertNoUpstreamCall(t, fakes)
	})
}

func TestQueryLabels_ValueValidation(t *testing.T) {
	tbl := twoZoneTable(t)
	overlong := strings.Repeat("x", MaxLabelQueryValueLen+1)

	cases := []struct {
		name string
		mod  func(*LabelQuery)
		want string
	}{
		{
			name: "newline in AZ",
			mod:  func(q *LabelQuery) { q.AZ = "zone-a\n" },
			want: "control character",
		},
		{
			name: "newline in filter",
			mod: func(q *LabelQuery) {
				q.Filters = map[string]string{"namespace": "shop\n"}
			},
			want: "control character",
		},
		{
			name: "lone surrogate half in AZ",
			mod:  func(q *LabelQuery) { q.AZ = "\xed\xa0\x80" },
			want: "not valid UTF-8",
		},
		{
			name: "raw 0xff in filter",
			mod: func(q *LabelQuery) {
				q.Filters = map[string]string{"namespace": "\xff"}
			},
			want: "not valid UTF-8",
		},
		{
			name: "over-long AZ",
			mod:  func(q *LabelQuery) { q.AZ = overlong },
			want: fmt.Sprintf("exceeds %d bytes", MaxLabelQueryValueLen),
		},
		{
			name: "over-long filter",
			mod: func(q *LabelQuery) {
				q.Filters = map[string]string{"namespace": overlong}
			},
			want: fmt.Sprintf("exceeds %d bytes", MaxLabelQueryValueLen),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakes := fakesFor(tbl)
			r := routerForTable(t, tbl, fakes)
			q := labelQueryAt(FamilyKSM, "kube_pod_info")
			tc.mod(&q)
			_, err := r.QueryLabels(t.Context(), q)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assertNoUpstreamCall(t, fakes)
		})
	}
}

func TestQueryLabels_ZeroInstantIsRequired(t *testing.T) {
	tbl := twoZoneTable(t)
	fakes := fakesFor(tbl)
	r := routerForTable(t, tbl, fakes)

	_, err := r.QueryLabels(t.Context(), LabelQuery{
		Metric: "kube_pod_info",
		Family: FamilyKSM,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evaluation instant is required")
	assertNoUpstreamCall(t, fakes)
}

func TestQueryLabels_AZKeyConflict(t *testing.T) {
	tbl := twoZoneTable(t)

	t.Run("rejected when AZ set, configured key", func(t *testing.T) {
		fakes := fakesFor(tbl)
		r := routerForTable(t, tbl, fakes)
		q := labelQueryAt(FamilyKSM, "kube_pod_info")
		q.AZ = "zone-a"
		q.LabelKeys = LabelKeys{AZ: "zone"}
		q.Filters = map[string]string{"zone": "zone-b"}
		_, err := r.QueryLabels(t.Context(), q)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "conflicts with the AZ field")
		assertNoUpstreamCall(t, fakes)
	})

	t.Run("accepted when AZ empty", func(t *testing.T) {
		fakes := fakesFor(tbl)
		r := routerForTable(t, tbl, fakes)
		q := labelQueryAt(FamilyKSM, "kube_pod_info")
		q.LabelKeys = LabelKeys{AZ: "zone"}
		q.Filters = map[string]string{"zone": "zone-a"}
		_, err := r.QueryLabels(t.Context(), q)
		require.NoError(t, err)
		qs := issuedQueries(fakes)
		require.NotEmpty(t, qs)
		assert.Equal(t, `kube_pod_info{zone="zone-a"}`, qs[0])
	})
}

// --- 4. zone rules -------------------------------------------------------

func TestQueryLabels_AZRejectedOnUnroutedFamilies(t *testing.T) {
	tbl := twoZoneTable(t)
	for _, fam := range []Family{FamilyServiceGraph, FamilyProbe} {
		t.Run(string(fam), func(t *testing.T) {
			fakes := fakesFor(tbl)
			r := routerForTable(t, tbl, fakes)
			q := labelQueryAt(fam, "up")
			if fam == FamilyServiceGraph {
				q.Metric = "traces_service_graph_request_total"
			}
			q.AZ = "zone-a"
			_, err := r.QueryLabels(t.Context(), q)
			require.Error(t, err)
			assert.Contains(t, err.Error(), string(fam))
			assert.Contains(t, err.Error(), "leave AZ empty")
			assert.Contains(t, err.Error(), "filter")
			assertNoUpstreamCall(t, fakes)
		})
	}
}

// Harvest routes by az AND carries the az matcher, like every zone-routed
// family (read-storage-roots-through-volume-hub D13).
func TestQueryLabels_HarvestAZRoutesAndMatches(t *testing.T) {
	tbl := ksmTable(t)
	fakes := fakesFor(tbl)
	r := routerForTable(t, tbl, fakes)

	q := labelQueryAt(FamilyHarvest, "volume_labels")
	q.AZ = "zone-b"
	_, err := r.QueryLabels(t.Context(), q)
	require.NoError(t, err)

	assert.Equal(t, []string{"n-b"}, calledBackends(fakes))
	qs := issuedQueries(fakes)
	require.Len(t, qs, 1)
	assert.Equal(t, `volume_labels{az="zone-b"}`, qs[0])
}

func TestQueryLabels_AZMatcherUsesConfiguredKey(t *testing.T) {
	tbl := twoZoneTable(t)
	for _, fam := range []Family{FamilyKSM, FamilyKubelet, FamilyAlerts} {
		t.Run(string(fam), func(t *testing.T) {
			fakes := fakesFor(tbl)
			r := routerForTable(t, tbl, fakes)
			metric := "kube_pod_info"
			if fam == FamilyKubelet {
				metric = "kubelet_volume_stats_used_bytes"
			}
			if fam == FamilyAlerts {
				metric = "ALERTS"
			}
			q := labelQueryAt(fam, metric)
			q.AZ = "zone-a"
			q.LabelKeys = LabelKeys{AZ: "zone"}
			_, err := r.QueryLabels(t.Context(), q)
			require.NoError(t, err)
			qs := issuedQueries(fakes)
			require.NotEmpty(t, qs)
			assert.Equal(t, metric+`{zone="zone-a"}`, qs[0])
			assert.NotContains(t, qs[0], `az="`)
		})
	}
}

func TestQueryLabels_EmptyAZAppliesNoRestriction(t *testing.T) {
	tbl := twoZoneTable(t)
	metrics := map[Family]string{
		FamilyKSM:          "kube_pod_info",
		FamilyKubelet:      "kubelet_volume_stats_used_bytes",
		FamilyHarvest:      "volume_labels",
		FamilyServiceGraph: "traces_service_graph_request_total",
		FamilyProbe:        "up",
		FamilyAlerts:       "ALERTS",
	}
	for _, fam := range Families {
		t.Run(string(fam), func(t *testing.T) {
			fakes := fakesFor(tbl)
			r := routerForTable(t, tbl, fakes)
			q := labelQueryAt(fam, metrics[fam])
			got, err := r.QueryLabels(t.Context(), q)
			require.NoError(t, err)
			assert.Empty(t, got)
			assert.Equal(t, []string{"zone-a", "zone-b"}, calledBackends(fakes))
			for _, query := range issuedQueries(fakes) {
				assert.Equal(t, metrics[fam], query, "empty AZ must render no matcher")
			}
		})
	}
}

// --- 5. query rendering --------------------------------------------------

func TestLabelQuery_RenderMatcherOrderIsDeterministic(t *testing.T) {
	at := time.Unix(1, 0)
	a := LabelQuery{
		Metric:  "kube_pod_info",
		Family:  FamilyKSM,
		AZ:      "zone-a",
		Filters: map[string]string{"namespace": "shop", "pod": "checkout"},
		At:      at,
	}
	b := LabelQuery{
		Metric:  "kube_pod_info",
		Family:  FamilyKSM,
		AZ:      "zone-a",
		Filters: map[string]string{"pod": "checkout", "namespace": "shop"},
		At:      at,
	}
	want := `kube_pod_info{az="zone-a",namespace="shop",pod="checkout"}`
	assert.Equal(t, want, a.render())
	assert.Equal(t, want, b.render())
}

func TestLabelQuery_RenderWindowed(t *testing.T) {
	q := labelQueryAt(FamilyKSM, "kube_pod_info")
	q.AZ = "zone-a"
	q.Window = 5 * time.Minute
	assert.Equal(t, `last_over_time(kube_pod_info{az="zone-a"}[5m])`, q.render())
}

func TestLabelQuery_RenderEscapesQuoteAndBackslash(t *testing.T) {
	q := labelQueryAt(FamilyKSM, "kube_pod_info")
	q.Filters = map[string]string{"namespace": `shop",other="x`}
	assert.Equal(t, `kube_pod_info{namespace="shop\",other=\"x"}`, q.render())
}

func TestLabelQuery_RenderEmptyFilterValue(t *testing.T) {
	q := labelQueryAt(FamilyHarvest, "qos_read_ops")
	q.Filters = map[string]string{"lun": ""}
	assert.Equal(t, `qos_read_ops{lun=""}`, q.render())
}

func TestLabelQuery_RenderBareMetric(t *testing.T) {
	q := labelQueryAt(FamilyKSM, "kube_pod_info")
	assert.Equal(t, "kube_pod_info", q.render())
	assert.NotContains(t, q.render(), "{}")
}

// --- 6. router method and result assembly --------------------------------

func TestQueryLabels_SelectsZonedAndZonelessBackends(t *testing.T) {
	tbl, err := NewTable([]Backend{
		be("all", "http://vm-all:8428", allFamilies()),
		be("k-a", "http://vm-a:8428", []Family{FamilyKSM}, "zone-a"),
		be("k-b", "http://vm-b:8428", []Family{FamilyKSM}, "zone-b"),
		be("rest", "http://vm-rest:8428", []Family{FamilyKubelet, FamilyHarvest, FamilyServiceGraph, FamilyProbe}),
	})
	require.NoError(t, err)

	t.Run("zoned", func(t *testing.T) {
		fakes := fakesFor(tbl)
		r := routerForTable(t, tbl, fakes)
		q := labelQueryAt(FamilyKSM, "kube_pod_info")
		q.AZ = "zone-a"
		_, err := r.QueryLabels(t.Context(), q)
		require.NoError(t, err)
		assert.Equal(t, []string{"all", "k-a"}, calledBackends(fakes))
	})

	t.Run("zoneless", func(t *testing.T) {
		fakes := fakesFor(tbl)
		r := routerForTable(t, tbl, fakes)
		_, err := r.QueryLabels(t.Context(), labelQueryAt(FamilyKSM, "kube_pod_info"))
		require.NoError(t, err)
		assert.Equal(t, []string{"all", "k-a", "k-b"}, calledBackends(fakes))
	})
}

func TestQueryLabels_StripsMetricNameLabel(t *testing.T) {
	tbl := twoZoneTable(t)
	fakes := fakesFor(tbl)
	fakes["zone-a"].vec = model.Vector{
		sample("kube_pod_info", map[string]string{"pod": "checkout", "namespace": "shop"}, 7),
	}
	r := routerForTable(t, tbl, fakes)

	got, err := r.QueryLabels(t.Context(), labelQueryAt(FamilyKSM, "kube_pod_info"))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, map[string]string{"namespace": "shop", "pod": "checkout"}, got[0])
	_, hasName := got[0][model.MetricNameLabel]
	assert.False(t, hasName)
}

func TestQueryLabels_OrderIndependentOfBackendArrival(t *testing.T) {
	run := func(delayA, delayB time.Duration) []map[string]string {
		tbl := twoZoneTable(t)
		fakes := map[string]*fakeBackend{
			"zone-a": {
				vec:   model.Vector{sample("kube_pod_info", map[string]string{"pod": "z-pod"}, 1)},
				delay: delayA,
			},
			"zone-b": {
				vec:   model.Vector{sample("kube_pod_info", map[string]string{"pod": "a-pod"}, 1)},
				delay: delayB,
			},
		}
		r := routerForTable(t, tbl, fakes)
		got, err := r.QueryLabels(t.Context(), labelQueryAt(FamilyKSM, "kube_pod_info"))
		require.NoError(t, err)
		return got
	}

	slowA := run(60*time.Millisecond, 0)
	slowB := run(0, 60*time.Millisecond)
	want := []map[string]string{
		{"pod": "a-pod"},
		{"pod": "z-pod"},
	}
	assert.Equal(t, want, slowA)
	assert.Equal(t, want, slowB)
}

func TestQueryLabels_ResultBound(t *testing.T) {
	tbl := twoZoneTable(t)

	t.Run("over bound fails", func(t *testing.T) {
		fakes := fakesFor(tbl)
		fakes["zone-a"].vec = model.Vector{
			sample("kube_pod_info", map[string]string{"pod": "a"}, 1),
			sample("kube_pod_info", map[string]string{"pod": "b"}, 1),
			sample("kube_pod_info", map[string]string{"pod": "c"}, 1),
		}
		r := routerForTable(t, tbl, fakes)
		q := labelQueryAt(FamilyKSM, "kube_pod_info")
		q.Limit = 2
		got, err := r.QueryLabels(t.Context(), q)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "result count 3 exceeds limit 2")
		assert.Nil(t, got)
	})

	t.Run("caller-raised bound succeeds", func(t *testing.T) {
		fakes := fakesFor(tbl)
		fakes["zone-a"].vec = model.Vector{
			sample("kube_pod_info", map[string]string{"pod": "a"}, 1),
			sample("kube_pod_info", map[string]string{"pod": "b"}, 1),
		}
		r := routerForTable(t, tbl, fakes)
		q := labelQueryAt(FamilyKSM, "kube_pod_info")
		q.Limit = 2
		got, err := r.QueryLabels(t.Context(), q)
		require.NoError(t, err)
		assert.Len(t, got, 2)
	})

	t.Run("duplicate series consumes one unit", func(t *testing.T) {
		dup := map[string]string{"pod": "shared"}
		fakes := fakesFor(tbl)
		fakes["zone-a"].vec = model.Vector{sample("kube_pod_info", dup, 1)}
		fakes["zone-b"].vec = model.Vector{sample("kube_pod_info", dup, 1)}
		r := routerForTable(t, tbl, fakes)
		q := labelQueryAt(FamilyKSM, "kube_pod_info")
		q.Limit = 1
		got, err := r.QueryLabels(t.Context(), q)
		require.NoError(t, err)
		assert.Equal(t, []map[string]string{{"pod": "shared"}}, got)
	})
}

func TestQueryLabels_DuplicateAcrossCatchAllAndZoneAppearsOnce(t *testing.T) {
	tbl, err := NewTable([]Backend{
		be("all", "http://vm-all:8428", allFamilies()),
		be("k-a", "http://vm-a:8428", []Family{FamilyKSM}, "zone-a"),
		be("rest", "http://vm-rest:8428", []Family{FamilyKubelet, FamilyHarvest, FamilyServiceGraph, FamilyProbe}),
	})
	require.NoError(t, err)
	dup := map[string]string{"pod": "shared"}
	fakes := fakesFor(tbl)
	fakes["all"].vec = model.Vector{sample("kube_pod_info", dup, 1)}
	fakes["k-a"].vec = model.Vector{sample("kube_pod_info", dup, 9)}
	r := routerForTable(t, tbl, fakes)

	q := labelQueryAt(FamilyKSM, "kube_pod_info")
	q.AZ = "zone-a"
	got, err := r.QueryLabels(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, []map[string]string{{"pod": "shared"}}, got)
	assert.Equal(t, []string{"all", "k-a"}, calledBackends(fakes))
}

// --- 7. degradation and observability ------------------------------------

func TestQueryLabels_UnservedFamilyErrorsWithoutCall(t *testing.T) {
	tbl := ksmTable(t)
	require.Equal(t, []Family{FamilyAlerts}, tbl.Unserved())
	fakes := fakesFor(tbl)
	r := routerForTable(t, tbl, fakes)

	_, err := r.QueryLabels(t.Context(), labelQueryAt(FamilyAlerts, "ALERTS"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `family "alerts"`)
	assert.Contains(t, err.Error(), "served by no backend")
	assertNoUpstreamCall(t, fakes)
}

func TestQueryLabels_UnmatchedZoneReturnsEmptyWithWarn(t *testing.T) {
	buf := testlog.Capture(t)
	tbl := ksmTable(t)
	fakes := fakesFor(tbl)
	r := routerForTable(t, tbl, fakes)

	q := labelQueryAt(FamilyKSM, "kube_pod_info")
	q.AZ = "zone-z"
	got, err := r.QueryLabels(t.Context(), q)
	require.NoError(t, err)
	assert.Empty(t, got)
	assertNoUpstreamCall(t, fakes)

	out := buf.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "ksm")
	assert.Contains(t, out, "zone-z")
}

func TestQueryLabels_BackendErrorFailsClosed(t *testing.T) {
	tbl := twoZoneTable(t)
	fakes := fakesFor(tbl)
	fakes["zone-a"].vec = model.Vector{sample("kube_pod_info", map[string]string{"pod": "a"}, 1)}
	fakes["zone-b"].err = fmt.Errorf("connection refused")
	r := routerForTable(t, tbl, fakes)

	got, err := r.QueryLabels(t.Context(), labelQueryAt(FamilyKSM, "kube_pod_info"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `backend "zone-b"`)
	assert.Contains(t, err.Error(), "connection refused")
	assert.Nil(t, got)
}

type queryNameMetrics struct {
	mu    sync.Mutex
	names []string
}

func (m *queryNameMetrics) ObserveQueryDuration(name string, _ float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.names = append(m.names, name)
}

func (m *queryNameMetrics) IncQueryFailure(string) {}

type nameObservingQuerier struct {
	inner *fakeBackend
	m     Metrics
}

func (q *nameObservingQuerier) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	q.m.ObserveQueryDuration(name, 0)
	return q.inner.Instant(ctx, name, query, ts)
}

func TestQueryLabels_FixedQueryNameOnMetrics(t *testing.T) {
	m := &queryNameMetrics{}
	tbl := twoZoneTable(t)
	fakes := fakesFor(tbl)
	r, err := NewRouter(tbl, m, func(b Backend) (Querier, error) {
		return &nameObservingQuerier{inner: fakes[b.Name()], m: m}, nil
	})
	require.NoError(t, err)

	const n = 1000
	for i := range n {
		_, err := r.QueryLabels(t.Context(), labelQueryAt(FamilyKSM, fmt.Sprintf("metric_%d", i)))
		require.NoError(t, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	uniq := make(map[string]struct{}, 1)
	for _, name := range m.names {
		uniq[name] = struct{}{}
	}
	assert.Equal(t, map[string]struct{}{QueryNameLabelQuery: {}}, uniq)
	assert.Len(t, m.names, n*tbl.Len())

	for _, f := range fakes {
		for _, name := range f.seenNames() {
			assert.Equal(t, QueryNameLabelQuery, name)
		}
	}
}

func TestQueryLabels_NoMatchIsEmptyNotError(t *testing.T) {
	tbl := twoZoneTable(t)
	fakes := fakesFor(tbl)
	r := routerForTable(t, tbl, fakes)
	got, err := r.QueryLabels(t.Context(), labelQueryAt(FamilyKSM, "kube_pod_info"))
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestQueryLabels_DocumentedExample is the snippet docs/upstream-backend-routing.md
// copies. Changing the request shape here means changing the docs.
func TestQueryLabels_DocumentedExample(t *testing.T) {
	tbl := twoZoneTable(t)
	fakes := fakesFor(tbl)
	fakes["zone-a"].vec = model.Vector{
		sample("kube_pod_info", map[string]string{"namespace": "shop", "pod": "checkout"}, 1),
	}
	r := routerForTable(t, tbl, fakes)

	sets, err := r.QueryLabels(t.Context(), LabelQuery{
		Metric: "kube_pod_info",
		Family: FamilyKSM,
		AZ:     "zone-a",
		Filters: map[string]string{
			"namespace": "shop",
			"pod":       "checkout",
		},
		At:     time.Unix(1, 0),
		Window: 5 * time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, []map[string]string{{"namespace": "shop", "pod": "checkout"}}, sets)
	qs := issuedQueries(fakes)
	require.NotEmpty(t, qs)
	assert.Equal(t, `last_over_time(kube_pod_info{az="zone-a",namespace="shop",pod="checkout"}[5m])`, qs[0])
}

// TestQueryLabels_InvalidAZLabelKeyRejected pins that the caller-supplied az
// label key is validated like a filter key: it is rendered into the query
// string, so a binding carrying a quote could otherwise introduce a second
// matcher.
func TestQueryLabels_InvalidAZLabelKeyRejected(t *testing.T) {
	tbl := twoZoneTable(t)
	fakes := fakesFor(tbl)
	r := routerForTable(t, tbl, fakes)

	q := labelQueryAt(FamilyKSM, "kube_pod_info")
	q.AZ = "zone-a"
	q.LabelKeys = LabelKeys{AZ: `az",foo="bar`}
	_, err := r.QueryLabels(t.Context(), q)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid az label key")
	assertNoUpstreamCall(t, fakes)
}

// TestQueryLabels_DeduplicatesByLabelSetNotFingerprint covers two backends of
// different engines holding one series: one preserves __name__ through the
// rolling function and the other does not, so the vector merge (which keys on
// the full label set) cannot collapse them. De-duplication by the STRIPPED
// label set must, or the series appears twice and consumes two units of the
// result bound.
func TestQueryLabels_DeduplicatesByLabelSetNotFingerprint(t *testing.T) {
	tbl := twoZoneTable(t)
	fakes := fakesFor(tbl)
	fakes["zone-a"].vec = model.Vector{
		sample("kube_pod_info", map[string]string{"pod": "checkout"}, 1),
	}
	fakes["zone-b"].vec = model.Vector{
		{Metric: model.Metric{"pod": "checkout"}, Value: 1},
	}
	r := routerForTable(t, tbl, fakes)

	q := labelQueryAt(FamilyKSM, "kube_pod_info")
	q.Limit = 1
	got, err := r.QueryLabels(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, []map[string]string{{"pod": "checkout"}}, got)
}
