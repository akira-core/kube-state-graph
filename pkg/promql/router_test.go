package promql

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closableFake is a fakeBackend that records CloseIdleConnections, so a test
// can assert a retired backend's pool is released.
type closableFake struct {
	fakeBackend
	mu     sync.Mutex
	closed int
}

func (c *closableFake) CloseIdleConnections() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
}

func (c *closableFake) closes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func singleTable(t *testing.T, name, rawURL string) *Table {
	t.Helper()
	tbl, err := NewTable([]Backend{be(name, rawURL, allFamilies())})
	require.NoError(t, err)
	return tbl
}

func TestRouter_SatisfiesBothInterfaces(t *testing.T) {
	var _ Querier = (*Router)(nil)
	var _ QuerierSource = (*Router)(nil)
}

func TestNewRouter_RejectsNilTable(t *testing.T) {
	_, err := NewRouter(nil, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil or empty routing table")
}

func TestNewRouter_FactoryErrorFailsConstruction(t *testing.T) {
	_, err := NewRouter(singleTable(t, "a", "http://vm-a:8428"), nil,
		func(Backend) (Querier, error) { return nil, errors.New("bad url") })
	require.Error(t, err)
	assert.Contains(t, err.Error(), `backend "a"`)
	assert.Contains(t, err.Error(), "bad url")
}

// Router.Instant is the unfiltered fan-out — what the readiness and retention
// probes use.
func TestRouter_InstantRoutesAsUnfiltered(t *testing.T) {
	fa := &fakeBackend{}
	fb := &fakeBackend{}
	r := routerWithFakes(t, twoZoneTable(t), map[string]*fakeBackend{"zone-a": fa, "zone-b": fb}, nil)

	_, err := r.Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
	require.NoError(t, err)
	callsA, _ := fa.seen()
	callsB, _ := fb.seen()
	assert.Equal(t, 1, callsA)
	assert.Equal(t, 1, callsB)
}

// The probe family accepts no request dimension, so the up{} probe reaches
// every backend serving it and fails naming any that did not answer.
func TestRouter_ProbeReachesEveryBackendAndNamesTheFailure(t *testing.T) {
	fa := &fakeBackend{vec: model.Vector{sample("up", nil, 1)}}
	fb := &fakeBackend{err: errors.New("connection refused")}
	r := routerWithFakes(t, twoZoneTable(t), map[string]*fakeBackend{"zone-a": fa, "zone-b": fb}, nil)

	_, err := r.Instant(context.Background(), string(QUpProbe), "up", time.Unix(0, 0))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `backend "zone-b"`)

	callsA, _ := fa.seen()
	assert.Equal(t, 1, callsA, "every probe-serving backend is asked")
}

func TestRouter_SwapChangesRouting(t *testing.T) {
	fakes := map[string]*fakeBackend{"a": {}, "b": {}}
	r, err := NewRouter(singleTable(t, "a", "http://vm-a:8428"), nil, func(bk Backend) (Querier, error) {
		f, ok := fakes[bk.Name()]
		if !ok {
			return nil, fmt.Errorf("no fake for %q", bk.Name())
		}
		return f, nil
	})
	require.NoError(t, err)

	next, err := NewTable([]Backend{
		be("a", "http://vm-a:8428", allFamilies()),
		be("b", "http://vm-b:8428", allFamilies()),
	})
	require.NoError(t, err)
	require.NoError(t, r.Swap(next))

	_, err = r.Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
	require.NoError(t, err)
	callsB, _ := fakes["b"].seen()
	assert.Equal(t, 1, callsB, "the new backend serves after the swap")
	assert.Equal(t, 2, r.Table().Len())
}

// A rejected swap must leave the live table serving unchanged — a partially
// applied table is never observable.
func TestRouter_RejectedSwapKeepsPreviousTable(t *testing.T) {
	fa := &fakeBackend{}
	r, err := NewRouter(singleTable(t, "a", "http://vm-a:8428"), nil,
		func(bk Backend) (Querier, error) {
			if bk.Name() == "a" {
				return fa, nil
			}
			return nil, errors.New("cannot dial")
		})
	require.NoError(t, err)

	next, err := NewTable([]Backend{
		be("a", "http://vm-a:8428", allFamilies()),
		be("broken", "http://vm-x:8428", allFamilies()),
	})
	require.NoError(t, err)

	require.Error(t, r.Swap(next))
	assert.Equal(t, 1, r.Table().Len(), "the previous table still serves")

	_, err = r.Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
	require.NoError(t, err)
	calls, _ := fa.seen()
	assert.Equal(t, 1, calls)
}

func TestRouter_SwapRejectsNilTable(t *testing.T) {
	r := routerWithFakes(t, twoZoneTable(t),
		map[string]*fakeBackend{"zone-a": {}, "zone-b": {}}, nil)
	require.Error(t, r.Swap(nil))
	assert.Equal(t, 2, r.Table().Len())
}

// A table edit that leaves a backend's identity untouched must keep its client
// — otherwise every reload would churn every connection pool.
func TestRouter_SwapReusesUnchangedClientsAndClosesRetired(t *testing.T) {
	keep := &closableFake{}
	retire := &closableFake{}
	added := &closableFake{}
	byName := map[string]Querier{"keep": keep, "retire": retire, "added": added}

	built := 0
	factory := func(bk Backend) (Querier, error) {
		built++
		q, ok := byName[bk.Name()]
		if !ok {
			return nil, fmt.Errorf("no fake for %q", bk.Name())
		}
		return q, nil
	}

	first, err := NewTable([]Backend{
		be("keep", "http://vm-keep:8428", allFamilies(), "zone-a"),
		be("retire", "http://vm-retire:8428", allFamilies(), "zone-b"),
	})
	require.NoError(t, err)
	r, err := NewRouter(first, nil, factory)
	require.NoError(t, err)
	require.Equal(t, 2, built)

	// `keep` gains a zone — its identity (url + credentials) is unchanged.
	second, err := NewTable([]Backend{
		be("keep", "http://vm-keep:8428", allFamilies(), "zone-a", "zone-c"),
		be("added", "http://vm-added:8428", allFamilies(), "zone-d"),
	})
	require.NoError(t, err)
	require.NoError(t, r.Swap(second))

	assert.Equal(t, 3, built, "only the genuinely new backend is constructed")
	assert.Zero(t, keep.closes(), "an unchanged identity keeps its pool")
	assert.Equal(t, 1, retire.closes(), "a retired backend's pool is released")
	assert.Zero(t, added.closes())
}

// Changing a backend's credentials changes its identity, so it must get a new
// client rather than silently keep authenticating with the old pair.
func TestRouter_CredentialChangeRebuildsClient(t *testing.T) {
	old := &closableFake{}
	fresh := &closableFake{}
	seq := []Querier{old, fresh}
	i := 0
	factory := func(Backend) (Querier, error) {
		q := seq[i]
		i++
		return q, nil
	}

	first, err := NewTable([]Backend{NewBackend("a", "http://vm-a:8428", allFamilies(), nil, "u", "p1")})
	require.NoError(t, err)
	r, err := NewRouter(first, nil, factory)
	require.NoError(t, err)

	second, err := NewTable([]Backend{NewBackend("a", "http://vm-a:8428", allFamilies(), nil, "u", "p2")})
	require.NoError(t, err)
	require.NoError(t, r.Swap(second))

	assert.Equal(t, 2, i, "a credential change rebuilds the client")
	assert.Equal(t, 1, old.closes(), "the superseded client's pool is released")
}

// Two backends addressing the same endpoint with the same credentials — a
// legitimate split by family or zone — share one client.
func TestRouter_SameIdentityBackendsShareAClient(t *testing.T) {
	built := 0
	tbl, err := NewTable([]Backend{
		be("k8s", "http://vm:8428", []Family{FamilyKSM, FamilyKubelet, FamilyServiceGraph, FamilyProbe}),
		be("netapp", "http://vm:8428", []Family{FamilyHarvest}),
	})
	require.NoError(t, err)
	_, err = NewRouter(tbl, nil, func(Backend) (Querier, error) {
		built++
		return &fakeBackend{}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, built)
}

// The snapshot is read ONCE in QuerierFor, so a reload mid-build cannot change
// which backends that build's remaining queries reach.
func TestRouter_BoundQuerierIgnoresLaterSwap(t *testing.T) {
	fakes := map[string]*fakeBackend{"a": {}, "b": {}}
	r, err := NewRouter(singleTable(t, "a", "http://vm-a:8428"), nil, func(bk Backend) (Querier, error) {
		f, ok := fakes[bk.Name()]
		if !ok {
			return nil, fmt.Errorf("no fake for %q", bk.Name())
		}
		return f, nil
	})
	require.NoError(t, err)

	bound := r.QuerierFor(Selector{}) // build starts here

	next, err := NewTable([]Backend{
		be("a", "http://vm-a:8428", allFamilies()),
		be("b", "http://vm-b:8428", allFamilies()),
	})
	require.NoError(t, err)
	require.NoError(t, r.Swap(next))

	_, err = bound.Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
	require.NoError(t, err)

	callsB, _ := fakes["b"].seen()
	assert.Zero(t, callsB, "a build in flight keeps the table it started with")

	// A build starting after the swap does see the new backend.
	_, err = r.QuerierFor(Selector{}).Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
	require.NoError(t, err)
	callsB, _ = fakes["b"].seen()
	assert.Equal(t, 1, callsB)
}

// Concurrent swaps and dispatches must not race. Run under -race.
func TestRouter_ConcurrentSwapAndDispatch(t *testing.T) {
	r := routerWithFakes(t, twoZoneTable(t),
		map[string]*fakeBackend{"zone-a": {}, "zone-b": {}}, nil)

	tables := []*Table{twoZoneTable(t), twoZoneTable(t)}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = r.Swap(tables[i%len(tables)])
		}(i)
		go func() {
			defer wg.Done()
			_, _ = r.Instant(context.Background(), string(QPodInfo), "q", time.Unix(0, 0))
		}()
	}
	wg.Wait()
	assert.Equal(t, 2, r.Table().Len())
}

// Static is the single-upstream case expressed in the routed vocabulary.
func TestStatic_IgnoresSelector(t *testing.T) {
	f := &fakeBackend{}
	src := Static(f)
	assert.Same(t, Querier(f), src.QuerierFor(Selector{}))
	assert.Same(t, Querier(f), src.QuerierFor(Selector{AZ: []string{"zone-a"}}))
}
