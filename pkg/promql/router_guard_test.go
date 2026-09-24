package promql

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouter_DefaultsOnWithNoOptions(t *testing.T) {
	r := routerWithFakes(t, singleTable(t, "a", "http://vm-a:8428"), map[string]*fakeBackend{"a": {vec: vecN(1)}}, nil)
	g, ok := r.state.Load().clients["a"].(*guard)
	require.True(t, ok, "every store is guarded")
	require.NotNil(t, g.sem)
	require.NotNil(t, r.cache)
	assert.Equal(t, DefaultQueryCacheMaxSeries, r.cache.max)

	f := &fakeBackend{vec: vecN(1)}
	r2, err := NewRouter(singleTable(t, "a", "http://vm-a:8428"), nil, func(Backend) (Querier, error) { return f, nil })
	require.NoError(t, err)
	for range 3 {
		_, err := r2.QuerierFor(Selector{}).Instant(t.Context(), string(QPodInfo), "q", ts0)
		require.NoError(t, err)
	}
	calls, _ := f.seen()
	assert.Equal(t, 1, calls, "repeat served from the default cache")
}

func TestRouter_ExplicitDisable(t *testing.T) {
	f := &fakeBackend{vec: vecN(1)}
	r, err := NewRouter(singleTable(t, "a", "http://vm-a:8428"), nil,
		func(Backend) (Querier, error) { return f, nil },
		WithMaxConcurrency(0), WithQueryCache(0, 0))
	require.NoError(t, err)
	assert.Nil(t, r.cache)
	assert.Nil(t, r.state.Load().clients["a"].(*guard).sem)
	for range 2 {
		_, err := r.QuerierFor(Selector{}).Instant(t.Context(), string(QPodInfo), "q", ts0)
		require.NoError(t, err)
	}
	calls, _ := f.seen()
	assert.Equal(t, 2, calls)
}

// A reload that keeps a store keeps its guard: the in-flight bound is not reset
// and its cache entries stay addressable.
func TestRouter_SwapPreservesGuard(t *testing.T) {
	f := &fakeBackend{vec: vecN(1)}
	r, err := NewRouter(singleTable(t, "a", "http://vm-a:8428"), nil, func(Backend) (Querier, error) { return f, nil })
	require.NoError(t, err)
	before := r.state.Load().clients["a"].(*guard)
	_, err = r.QuerierFor(Selector{}).Instant(t.Context(), string(QPodInfo), "q", ts0)
	require.NoError(t, err)

	// Same store under a new backend name.
	require.NoError(t, r.Swap(singleTable(t, "renamed", "http://vm-a:8428")))
	after := r.state.Load().clients["renamed"].(*guard)
	assert.Same(t, before, after)

	_, err = r.QuerierFor(Selector{}).Instant(t.Context(), string(QPodInfo), "q", ts0)
	require.NoError(t, err)
	calls, _ := f.seen()
	assert.Equal(t, 1, calls, "cache entry survived the reload")
}

// Two backends naming one store share its bound.
func TestRouter_TwoBackendsOneStoreShareBound(t *testing.T) {
	tbl, err := NewTable([]Backend{
		be("ksm", "http://vm-shared:8428", []Family{FamilyKSM, FamilyKubelet, FamilyServiceGraph, FamilyProbe}),
		be("harvest", "http://vm-shared:8428", []Family{FamilyHarvest}),
	})
	require.NoError(t, err)
	gate := newGate()
	r, err := NewRouter(tbl, nil, func(Backend) (Querier, error) { return gate, nil },
		WithMaxConcurrency(4), WithQueryCache(0, 0))
	require.NoError(t, err)
	st := r.state.Load()
	require.Same(t, st.clients["ksm"], st.clients["harvest"])

	q := r.QuerierFor(Selector{})
	var wg sync.WaitGroup
	for i := range 10 {
		name := QPodInfo
		if i%2 == 0 {
			name = QVolumeLabels
		}
		wg.Go(func() { _, _ = q.Instant(t.Context(), string(name), fmt.Sprintf("q%d", i), ts0) })
	}
	for range 4 {
		<-gate.started
	}
	time.Sleep(20 * time.Millisecond)
	assert.EqualValues(t, 4, gate.inflight.Load())
	close(gate.release)
	wg.Wait()
	assert.LessOrEqual(t, gate.peak.Load(), int64(4))
}

// A saturated store does not delay a query routed only to another store.
func TestRouter_SaturatedStoreDoesNotStallAnother(t *testing.T) {
	gate := newGate()
	other := &fakeBackend{vec: vecN(1)}
	byName := map[string]Querier{"zone-a": gate, "zone-b": other}
	r, err := NewRouter(twoZoneTable(t), nil,
		func(b Backend) (Querier, error) { return byName[b.Name()], nil }, WithMaxConcurrency(1))
	require.NoError(t, err)

	go func() {
		_, _ = r.QuerierFor(Selector{AZ: []string{"zone-a"}}).Instant(t.Context(), string(QPodInfo), "hold", ts0)
	}()
	<-gate.started

	done := make(chan error, 1)
	go func() {
		_, err := r.QuerierFor(Selector{AZ: []string{"zone-b"}}).Instant(t.Context(), string(QPodInfo), "q", ts0)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("zone-b query waited on saturated zone-a")
	}
	close(gate.release)
}

// Probes bypass the router's guards too, via ProbeAll and the probe family.
func TestRouter_ProbeAllBypassesSaturatedStore(t *testing.T) {
	gate := newGate()
	probe := &fakeBackend{vec: vecN(1)}
	split := &splitBackend{probe: probe, other: gate}
	r, err := NewRouter(singleTable(t, "a", "http://vm-a:8428"), nil,
		func(Backend) (Querier, error) { return split, nil }, WithMaxConcurrency(1))
	require.NoError(t, err)
	go func() { _, _ = r.QuerierFor(Selector{}).Instant(t.Context(), string(QPodInfo), "hold", ts0) }()
	<-gate.started

	require.NoError(t, r.ProbeAll(t.Context(), ts0))
	_, err = r.Instant(t.Context(), string(QUpProbe), string(QUpProbe), ts0)
	require.NoError(t, err)
	calls, _ := probe.seen()
	assert.Equal(t, 2, calls, "neither probe queued nor was cached")
	close(gate.release)
}
