package build

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// routedFake answers every query with the vectors its own fixture declares,
// keyed by query name, and records what it was asked. It stands in for one
// upstream installation.
type routedFake struct {
	mu     sync.Mutex
	byName map[promql.Query]model.Vector
	seen   map[string]int
}

func newRoutedFake(fixtures map[promql.Query]model.Vector) *routedFake {
	return &routedFake{byName: fixtures, seen: map[string]int{}}
}

func (f *routedFake) Instant(_ context.Context, name, _ string, _ time.Time) (model.Vector, error) {
	f.mu.Lock()
	f.seen[name]++
	f.mu.Unlock()
	return f.byName[promql.Query(name)], nil
}

func (f *routedFake) calls(name promql.Query) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[string(name)]
}

func (f *routedFake) legCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

func (f *routedFake) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.seen {
		n += c
	}
	return n
}

// routerOver wires one *promql.Router over the supplied fakes, one per backend
// declared in the table.
func routerOver(t *testing.T, tbl *promql.Table, fakes map[string]*routedFake) *promql.Router {
	t.Helper()
	r, err := promql.NewRouter(tbl, nil, func(b promql.Backend) (promql.Querier, error) {
		f, ok := fakes[b.Name()]
		require.True(t, ok, "no fake for backend %q", b.Name())
		return f, nil
	})
	require.NoError(t, err)
	return r
}

func allFamilies() []promql.Family { return append([]promql.Family(nil), promql.Families...) }

// TestNew_PlainQuerierIsNotUpgraded pins that the routing seam is genuinely
// optional: a mock Querier does not satisfy QuerierSource, so the builder keeps
// dispatching through it directly and every existing test is unaffected.
func TestNew_PlainQuerierIsNotUpgraded(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t)
	b := New(q, Options{}, nil, nil)
	assert.Nil(t, b.src, "a plain Querier must not be treated as a routing source")
	assert.Equal(t, promql.Querier(q), b.querierFor(promql.Selector{}))
}

// TestNew_RouterIsUpgraded is the other half: a *promql.Router does satisfy
// QuerierSource, so the builder resolves a per-build querier from it.
func TestNew_RouterIsUpgraded(t *testing.T) {
	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("a", "http://vm-a:8428", allFamilies(), nil, "", ""),
	})
	require.NoError(t, err)
	r := routerOver(t, tbl, map[string]*routedFake{"a": newRoutedFake(nil)})

	b := New(r, Options{}, nil, nil)
	require.NotNil(t, b.src)
	// The resolved querier is a per-build binding, not the router itself.
	assert.NotEqual(t, promql.Querier(r), b.querierFor(promql.Selector{}))
}

// TestReadTopology_RoutedFanOutReachesEveryBackend pins that each leg is issued
// once per SELECTED backend and that the leg count itself is unchanged — the
// fan-out multiplies calls, never legs.
func TestReadTopology_RoutedFanOutReachesEveryBackend(t *testing.T) {
	// Both backends answer the two legs the scoped QoS read depends on, so the
	// six workload families are issued and the full leg set is exercised.
	joined := map[promql.Query]model.Vector{
		promql.QPVCInfo: {&model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data",
			"volumename": "pvc-9f3a",
		}, Value: 1}},
		promql.QVolumeLabels: {&model.Sample{Metric: model.Metric{
			"volume": "trident_pvc_9f3a", "cluster": "ontap-prod",
			"node": "ontap-prod-01", "aggr": "aggr1", "svm": "svm0",
		}, Value: 1}},
	}
	fa := newRoutedFake(joined)
	fb := newRoutedFake(joined)
	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("zone-a", "http://vm-a:8428", allFamilies(), []string{"zone-a"}, "", ""),
		promql.NewBackend("zone-b", "http://vm-b:8428", allFamilies(), []string{"zone-b"}, "", ""),
	})
	require.NoError(t, err)
	r := routerOver(t, tbl, map[string]*routedFake{"zone-a": fa, "zone-b": fb})

	_, err = ReadTopology(context.Background(), r.QuerierFor(promql.Selector{}),
		time.Minute, time.Unix(1, 0).UTC(), Options{}, promql.Selector{})
	require.NoError(t, err)

	// Task 5.4: the 42-leg fan-out is per BUILD, not per backend.
	assert.Equal(t, 42, fa.legCount(), "each backend sees the full leg set")
	assert.Equal(t, 42, fb.legCount())
	assert.Equal(t, 42, fa.totalCalls(), "each leg issued exactly once per backend")
	assert.Equal(t, 42, fb.totalCalls())
}

// A zone-scoped request narrows the zone-routable families to one backend, but
// leaves the dimensionless service-graph family reaching both.
func TestReadTopology_ZoneScopedRequestNarrowsTheFanOut(t *testing.T) {
	fa := newRoutedFake(nil)
	fb := newRoutedFake(nil)
	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("zone-a", "http://vm-a:8428", allFamilies(), []string{"zone-a"}, "", ""),
		promql.NewBackend("zone-b", "http://vm-b:8428", allFamilies(), []string{"zone-b"}, "", ""),
	})
	require.NoError(t, err)
	r := routerOver(t, tbl, map[string]*routedFake{"zone-a": fa, "zone-b": fb})

	sel := promql.Selector{AZ: []string{"zone-a"}}
	q := r.QuerierFor(sel)
	_, err = ReadTopology(context.Background(), q, time.Minute, time.Unix(1, 0).UTC(), Options{}, sel)
	require.NoError(t, err)

	assert.Equal(t, 1, fa.calls(promql.QPodInfo))
	assert.Zero(t, fb.calls(promql.QPodInfo), "the other zone's store is not asked")
	assert.Equal(t, 1, fa.calls(promql.QVolumeLabels))
	assert.Zero(t, fb.calls(promql.QVolumeLabels), "Harvest is zone-routed like kube-state-metrics")

	// The service-graph family accepts no dimension, so it still reaches both.
	_, err = q.Instant(context.Background(), string(promql.QServiceGraphTotal), "q", time.Unix(1, 0))
	require.NoError(t, err)
	assert.Equal(t, 1, fa.calls(promql.QServiceGraphTotal))
	assert.Equal(t, 1, fb.calls(promql.QServiceGraphTotal))
}

// TestBuild_JoinsAcrossBackends is the change's whole point: a claim read from
// the kube-state-metrics installation joins a volume_labels series read from a
// DIFFERENT (NetApp) installation, and the storage edge is drawn.
func TestBuild_JoinsAcrossBackends(t *testing.T) {
	k8s := newRoutedFake(map[promql.Query]model.Vector{
		promql.QPodInfo: {&model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "pod": "mongo-0",
			"uid": "uid-1", "node": "n1", "pod_ip": "10.0.0.1",
		}, Value: 1}},
		promql.QPVCInfo: {&model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data",
			"storageclass": "netapp-nas", "volumename": "pvc-9f3a",
		}, Value: 1}},
		promql.QPVCBindings: {&model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "pod": "mongo-0",
			"volume": "data", "persistentvolumeclaim": "data",
		}, Value: 1}},
	})
	netapp := newRoutedFake(map[promql.Query]model.Vector{
		promql.QVolumeLabels: {&model.Sample{Metric: model.Metric{
			"volume": "trident_pvc_9f3a", "cluster": "ontap-prod",
			"node": "ontap-lab-01", "aggr": "aggr1", "svm": "svm0",
		}, Value: 1}},
	})

	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("k8s", "http://vm-k8s:8428",
			[]promql.Family{promql.FamilyKSM, promql.FamilyKubelet, promql.FamilyServiceGraph, promql.FamilyProbe},
			nil, "", ""),
		promql.NewBackend("netapp", "http://vm-netapp:8428",
			[]promql.Family{promql.FamilyHarvest}, nil, "", ""),
	})
	require.NoError(t, err)
	r := routerOver(t, tbl, map[string]*routedFake{"k8s": k8s, "netapp": netapp})

	b := New(r, Options{}, nil, nil)
	g, err := b.Build(context.Background(), time.Minute, time.Unix(1000, 0).UTC(), promql.Selector{})
	require.NoError(t, err)

	// Each installation was asked only for the families it serves.
	assert.Equal(t, 1, k8s.calls(promql.QPodInfo))
	assert.Zero(t, netapp.calls(promql.QPodInfo), "the NetApp store is never asked for kube_* series")
	assert.Equal(t, 1, netapp.calls(promql.QVolumeLabels))
	assert.Zero(t, k8s.calls(promql.QVolumeLabels), "the kube store is never asked for Harvest series")

	// The join spans the two stores.
	var storageEdges int
	for _, e := range g.Edges {
		if e.Type == graph.EdgeTypePVCToNetAppAggr {
			storageEdges++
			assert.Equal(t, "c/db/data", e.Source)
			assert.Equal(t, "netapp/ontap-prod/aggr/aggr1", e.Target)
		}
	}
	assert.Equal(t, 1, storageEdges, "the claim read from one backend joined the volume read from the other")
}

// probeFailingFake answers topology queries with nothing and fails only the
// up{} probe, which is what an unreachable backend looks like to a build that
// loaded no rows.
type probeFailingFake struct{ routedFake }

func (f *probeFailingFake) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	if name == string(promql.QUpProbe) {
		f.mu.Lock()
		f.seen[name]++
		f.mu.Unlock()
		return nil, errors.New("connection refused")
	}
	return f.routedFake.Instant(ctx, name, query, ts)
}

// An empty graph must be reported as an empty graph when the upstream cannot
// confirm it is healthy: outside_retention is a claim about the data, and a
// backend that did not answer cannot support it.
func TestBuild_RetentionClassificationSkippedWhenABackendIsDown(t *testing.T) {
	healthy := newRoutedFake(map[promql.Query]model.Vector{
		promql.QUpProbe: {&model.Sample{Metric: model.Metric{"__name__": "up"}, Value: 1}},
	})
	down := &probeFailingFake{routedFake: *newRoutedFake(nil)}

	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("zone-a", "http://vm-a:8428", allFamilies(), nil, "", ""),
		promql.NewBackend("zone-b", "http://vm-b:8428", allFamilies(), nil, "", ""),
	})
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, nil, func(b promql.Backend) (promql.Querier, error) {
		if b.Name() == "zone-a" {
			return healthy, nil
		}
		return down, nil
	})
	require.NoError(t, err)

	g, err := New(r, Options{}, nil, nil).
		Build(context.Background(), time.Minute, time.Unix(1000, 0).UTC(), promql.Selector{})
	require.NoError(t, err, "an unconfirmable empty graph is an empty graph, not a retention error")
	assert.Empty(t, g.NodesByID)
	assert.Equal(t, 1, down.calls(promql.QUpProbe), "the probe still reaches every backend")
}

// The classification still fires when every backend confirms health — the
// skip above is caused by the failure, not by routing itself.
func TestBuild_RetentionClassificationStillFiresWhenAllBackendsAnswer(t *testing.T) {
	up := model.Vector{&model.Sample{Metric: model.Metric{"__name__": "up"}, Value: 1}}
	a := newRoutedFake(map[promql.Query]model.Vector{promql.QUpProbe: up})
	b := newRoutedFake(map[promql.Query]model.Vector{promql.QUpProbe: up})

	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("zone-a", "http://vm-a:8428", allFamilies(), nil, "", ""),
		promql.NewBackend("zone-b", "http://vm-b:8428", allFamilies(), nil, "", ""),
	})
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, nil, func(bk promql.Backend) (promql.Querier, error) {
		if bk.Name() == "zone-a" {
			return a, nil
		}
		return b, nil
	})
	require.NoError(t, err)

	_, err = New(r, Options{}, nil, nil).
		Build(context.Background(), time.Minute, time.Unix(1000, 0).UTC(), promql.Selector{})
	require.Error(t, err)
	var be *Error
	require.ErrorAs(t, err, &be)
	assert.Equal(t, ReasonOutsideRetention, be.Reason)
}

// queryRecordingFake records the rendered query STRING per leg, so a test can
// assert that routing composed with the PromQL matchers rather than replacing
// them.
type queryRecordingFake struct {
	mu       sync.Mutex
	queries  map[string][]string
	fixtures map[promql.Query]model.Vector
}

func newQueryRecordingFake() *queryRecordingFake {
	return &queryRecordingFake{queries: map[string][]string{}}
}

// newQueryRecordingFakeWith answers the named legs with fixtures instead of an
// empty vector — needed whenever a test must reach the scoped QoS read, which
// is issued only for FlexVol names a loaded claim already matched.
func newQueryRecordingFakeWith(fixtures map[promql.Query]model.Vector) *queryRecordingFake {
	return &queryRecordingFake{queries: map[string][]string{}, fixtures: fixtures}
}

func (f *queryRecordingFake) Instant(_ context.Context, name, query string, _ time.Time) (model.Vector, error) {
	f.mu.Lock()
	f.queries[name] = append(f.queries[name], query)
	out := f.fixtures[promql.Query(name)]
	f.mu.Unlock()
	if out == nil {
		return model.Vector{}, nil
	}
	return out, nil
}

func (f *queryRecordingFake) queryFor(name promql.Query) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	qs := f.queries[string(name)]
	if len(qs) == 0 {
		return ""
	}
	return qs[0]
}

// Routing narrows WHICH store is asked; the matcher narrows WHAT it returns.
// The two compose — a zone that selected a backend must still be rendered.
func TestRoutedBuild_ZoneMatcherStillRendered(t *testing.T) {
	fa := newQueryRecordingFake()
	fb := newQueryRecordingFake()
	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("zone-a", "http://vm-a:8428", allFamilies(), []string{"zone-a"}, "", ""),
		promql.NewBackend("zone-b", "http://vm-b:8428", allFamilies(), []string{"zone-b"}, "", ""),
	})
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, nil, func(b promql.Backend) (promql.Querier, error) {
		if b.Name() == "zone-a" {
			return fa, nil
		}
		return fb, nil
	})
	require.NoError(t, err)

	sel := promql.Selector{AZ: []string{"zone-a"}}
	_, err = ReadTopology(context.Background(), r.QuerierFor(sel),
		time.Minute, time.Unix(1, 0).UTC(), Options{}, sel)
	require.NoError(t, err)

	got := fa.queryFor(promql.QPodInfo)
	require.NotEmpty(t, got, "the zone-a store must have been asked")
	assert.Contains(t, got, `az="zone-a"`,
		"selecting a backend must not replace the pushed-down matcher")
	assert.Empty(t, fb.queryFor(promql.QPodInfo), "the other zone's store is not asked")

	// The Harvest leg is zone-routed the same way but carries NO matcher: for
	// that family the selected store is the zone boundary.
	hv := fa.queryFor(promql.QVolumeLabels)
	require.NotEmpty(t, hv, "the zone-a store must have been asked for Harvest")
	assert.Equal(t, `last_over_time(volume_labels[1m])`, hv,
		"Harvest is routed by zone, never narrowed by matcher")
	assert.Empty(t, fb.queryFor(promql.QVolumeLabels), "the other zone's Harvest store is not asked")
}

// harvestQueries is the thirteen-leg Harvest family, listed here so the
// routing test below cannot silently cover a subset.
var harvestQueries = []promql.Query{
	promql.QVolumeLabels,
	promql.QQoSReadOps, promql.QQoSWriteOps, promql.QQoSReadLatency, promql.QQoSWriteLatency,
	promql.QQoSReadData, promql.QQoSWriteData,
	promql.QQoSPolicyFixedMaxIOPS, promql.QQoSPolicyFixedMaxMBps,
	promql.QAggrStatus, promql.QAggrSpaceUsed, promql.QAggrSpaceTotal, promql.QNetAppNodeStatus,
}

// Under a zone-scoped request every Harvest leg reaches the zone's backend AND
// any catch-all backend, and each receives the byte-identical UNFILTERED
// string — the same one an unscoped build renders. Routing is the only effect
// `az` has on the family.
func TestRoutedBuild_HarvestLegsAreUnfilteredOnZoneAndCatchAllBackends(t *testing.T) {
	joined := map[promql.Query]model.Vector{
		promql.QPVCInfo: {&model.Sample{Metric: model.Metric{
			"cluster": "c", "namespace": "db", "persistentvolumeclaim": "data",
			"volumename": "pvc-9f3a", "az": "zone-a", "env": "prod",
		}, Value: 1}},
		promql.QVolumeLabels: {&model.Sample{Metric: model.Metric{
			"volume": "trident_pvc_9f3a", "cluster": "ontap-prod",
			"node": "ontap-prod-01", "aggr": "aggr1", "svm": "svm0",
		}, Value: 1}},
	}
	zone := newQueryRecordingFakeWith(joined)
	catchAll := newQueryRecordingFakeWith(joined)
	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("zone-a", "http://vm-a:8428", allFamilies(), []string{"zone-a"}, "", ""),
		promql.NewBackend("netapp-all", "http://vm-netapp:8428", []promql.Family{promql.FamilyHarvest}, nil, "", ""),
	})
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, nil, func(b promql.Backend) (promql.Querier, error) {
		if b.Name() == "zone-a" {
			return zone, nil
		}
		return catchAll, nil
	})
	require.NoError(t, err)

	sel := promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}
	_, err = ReadTopology(context.Background(), r.QuerierFor(sel),
		time.Minute, time.Unix(1, 0).UTC(), Options{}, sel)
	require.NoError(t, err)

	require.Len(t, harvestQueries, 13)
	scoped := map[promql.Query]bool{}
	for _, q := range promql.QoSWorkloadQueries {
		scoped[q] = true
	}
	for _, q := range harvestQueries {
		want := promql.Render(q, time.Minute, promql.LabelKeys{}, promql.Selector{})
		if scoped[q] {
			// The six workload families carry ONE extra matcher, and it is
			// derived from upstream data (the FlexVol names the loaded claims
			// matched), never from the request. Still no az / env.
			want, _ = promql.RenderQoSVolumeScoped(q, time.Minute, []string{"trident_pvc_9f3a"})
		}
		assert.Equal(t, want, zone.queryFor(q), "%s on the zone backend must carry no request matcher", q)
		assert.Equal(t, want, catchAll.queryFor(q), "%s on the catch-all backend must carry no request matcher", q)
		assert.NotContains(t, want, `az=`, "%s must render no az matcher", q)
		assert.NotContains(t, want, `env=`, "%s must render no env matcher", q)
	}
	// The kube-state-metrics leg on the same backend keeps its matcher, which
	// is what makes this a per-family contract rather than a router quirk.
	assert.Contains(t, zone.queryFor(promql.QPodInfo), `az="zone-a",env="prod"`)
}

// Only `az` routes. A namespace-scoped request must reach every backend
// exactly as an unscoped one does — while still rendering its matcher.
func TestRoutedBuild_NamespaceFilterDoesNotRoute(t *testing.T) {
	fa := newQueryRecordingFake()
	fb := newQueryRecordingFake()
	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("zone-a", "http://vm-a:8428", allFamilies(), []string{"zone-a"}, "", ""),
		promql.NewBackend("zone-b", "http://vm-b:8428", allFamilies(), []string{"zone-b"}, "", ""),
	})
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, nil, func(b promql.Backend) (promql.Querier, error) {
		if b.Name() == "zone-a" {
			return fa, nil
		}
		return fb, nil
	})
	require.NoError(t, err)

	sel := promql.Selector{Namespace: []string{"shop"}, Cluster: []string{"alpha"}, Env: []string{"prod"}}
	_, err = ReadTopology(context.Background(), r.QuerierFor(sel),
		time.Minute, time.Unix(1, 0).UTC(), Options{}, sel)
	require.NoError(t, err)

	for name, f := range map[string]*queryRecordingFake{"zone-a": fa, "zone-b": fb} {
		got := f.queryFor(promql.QPodInfo)
		require.NotEmpty(t, got, "%s must be asked: env/cluster/namespace never route", name)
		assert.Contains(t, got, `namespace="shop"`)
		assert.Contains(t, got, `env="prod"`)
	}
	assert.Equal(t, fa.queryFor(promql.QPodInfo), fb.queryFor(promql.QPodInfo),
		"every selected backend receives a byte-identical query string")
}

// optionalLegFake fails exactly one leg and answers the rest, which is what an
// unreachable backend looks like to a leg the builder already treats as
// optional.
type optionalLegFake struct {
	routedFake
	failing promql.Query
}

func (f *optionalLegFake) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	if name == string(f.failing) {
		f.mu.Lock()
		f.seen[name]++
		f.mu.Unlock()
		return nil, errors.New("connection refused")
	}
	return f.routedFake.Instant(ctx, name, query, ts)
}

// A backend error on a leg the builder already treats as OPTIONAL degrades
// that leg only — the routed fan-out must not turn a documented degrade into a
// failed build.
func TestRoutedBuild_OptionalLegDegradesOnBackendError(t *testing.T) {
	healthy := newRoutedFake(nil)
	broken := &optionalLegFake{routedFake: *newRoutedFake(nil), failing: promql.QKubeletVolumeUsedBytes}

	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("zone-a", "http://vm-a:8428", allFamilies(), nil, "", ""),
		promql.NewBackend("zone-b", "http://vm-b:8428", allFamilies(), nil, "", ""),
	})
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, nil, func(b promql.Backend) (promql.Querier, error) {
		if b.Name() == "zone-a" {
			return healthy, nil
		}
		return broken, nil
	})
	require.NoError(t, err)

	tp, err := ReadTopology(context.Background(), r.QuerierFor(promql.Selector{}),
		time.Minute, time.Unix(1, 0).UTC(), Options{}, promql.Selector{})
	require.NoError(t, err, "an optional leg's backend failure must not fail the build")
	assert.NotNil(t, tp)
	assert.Equal(t, 1, broken.calls(promql.QKubeletVolumeUsedBytes))
}

// The mirror case: the SAME backend error on a REQUIRED leg fails the build and
// names the backend. A partial fan-out would render as a smaller, wrong graph.
func TestRoutedBuild_RequiredLegFailsOnBackendError(t *testing.T) {
	healthy := newRoutedFake(nil)
	broken := &optionalLegFake{routedFake: *newRoutedFake(nil), failing: promql.QPodInfo}

	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("zone-a", "http://vm-a:8428", allFamilies(), nil, "", ""),
		promql.NewBackend("zone-b", "http://vm-b:8428", allFamilies(), nil, "", ""),
	})
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, nil, func(b promql.Backend) (promql.Querier, error) {
		if b.Name() == "zone-a" {
			return healthy, nil
		}
		return broken, nil
	})
	require.NoError(t, err)

	_, err = ReadTopology(context.Background(), r.QuerierFor(promql.Selector{}),
		time.Minute, time.Unix(1, 0).UTC(), Options{}, promql.Selector{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `backend "zone-b"`)
}

// An unfiltered build must MERGE Harvest across the per-zone stores, so
// aggregates from both zones can join their claims in one graph. Zone routing
// narrows the fan-out only when a zone was actually requested.
func TestRoutedBuild_UnfilteredHarvestMergesAcrossBackends(t *testing.T) {
	claims := map[promql.Query]model.Vector{
		promql.QPodInfo: {
			&model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "pod": "p-a", "uid": "u-a", "node": "n1",
			}, Value: 1},
			&model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "pod": "p-b", "uid": "u-b", "node": "n1",
			}, Value: 1},
		},
		promql.QPVCBindings: {
			&model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "pod": "p-a", "volume": "v", "persistentvolumeclaim": "a",
			}, Value: 1},
			&model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "pod": "p-b", "volume": "v", "persistentvolumeclaim": "b",
			}, Value: 1},
		},
		promql.QPVCInfo: {
			&model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "persistentvolumeclaim": "a",
				"storageclass": "netapp-nas", "volumename": "pv-a",
			}, Value: 1},
			&model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "persistentvolumeclaim": "b",
				"storageclass": "netapp-nas", "volumename": "pv-b",
			}, Value: 1},
		},
	}
	zoneA := newRoutedFake(mergeFixtures(claims, map[promql.Query]model.Vector{
		promql.QVolumeLabels: {&model.Sample{Metric: model.Metric{
			"volume": "trident_pv_a", "cluster": "ontap-a", "node": "n-a", "aggr": "aggr-a",
		}, Value: 1}},
	}))
	zoneB := newRoutedFake(map[promql.Query]model.Vector{
		promql.QVolumeLabels: {&model.Sample{Metric: model.Metric{
			"volume": "trident_pv_b", "cluster": "ontap-b", "node": "n-b", "aggr": "aggr-b",
		}, Value: 1}},
	})

	tbl, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("zone-a", "http://vm-a:8428", allFamilies(), []string{"zone-a"}, "", ""),
		promql.NewBackend("zone-b", "http://vm-b:8428", allFamilies(), []string{"zone-b"}, "", ""),
	})
	require.NoError(t, err)
	r, err := promql.NewRouter(tbl, nil, func(b promql.Backend) (promql.Querier, error) {
		if b.Name() == "zone-a" {
			return zoneA, nil
		}
		return zoneB, nil
	})
	require.NoError(t, err)

	tp, err := ReadTopology(context.Background(), r.QuerierFor(promql.Selector{}),
		time.Minute, time.Unix(1, 0).UTC(), Options{}, promql.Selector{})
	require.NoError(t, err)

	aggrs := map[string]bool{}
	for _, a := range tp.NetAppAggrs {
		aggrs[a.ID()] = true
	}
	assert.True(t, aggrs["netapp/ontap-a/aggr/aggr-a"], "the zone-a filer joined")
	assert.True(t, aggrs["netapp/ontap-b/aggr/aggr-b"],
		"an unfiltered build merges Harvest from every backend, not just the first")
}

// mergeFixtures combines two per-query fixture maps into one.
func mergeFixtures(a, b map[promql.Query]model.Vector) map[promql.Query]model.Vector {
	out := make(map[promql.Query]model.Vector, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
