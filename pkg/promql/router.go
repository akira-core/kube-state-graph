package promql

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/common/model"
)

// ClientFactory builds the Querier one backend's queries are dispatched
// through. Production wiring passes DefaultClientFactory; tests substitute a
// fake so a router can be exercised without a listening socket.
type ClientFactory func(b Backend) (Querier, error)

// DefaultClientFactory returns a ClientFactory producing a *Client per backend,
// carrying that backend's own resolved basic-auth pair. The credentials are
// held on the transport only, scoped to the backend's own host, so a
// cross-host redirect carries no Authorization header.
func DefaultClientFactory(m Metrics) ClientFactory {
	return func(b Backend) (Querier, error) {
		return New(b.URL(), m, BackendClientOptions(b)...)
	}
}

// BackendClientOptions is the option set a backend's client is built from: its
// routing-table name (for span attribution) plus its own basic-auth pair when
// it has one. It is a named function rather than three inline lines so the
// credential wiring between a validated Backend and the outbound transport is
// directly assertable.
func BackendClientOptions(b Backend) []Option {
	user, pass := b.Credentials()
	opts := []Option{WithBackendName(b.Name())}
	if user != "" || pass != "" {
		opts = append(opts, WithBasicAuth(user, pass))
	}
	return opts
}

// clientKey is a backend's client IDENTITY. Two backends sharing a key can
// share a client, and a client survives a reload that did not change its key —
// so a table edit that only added a zone does not churn every connection pool.
type clientKey struct {
	url      string
	username string
	password string
}

func keyOf(b Backend) clientKey {
	user, pass := b.Credentials()
	return clientKey{url: b.URL(), username: user, password: pass}
}

// routerState is one immutable routing snapshot: a validated table plus the
// clients that serve it. It is replaced wholesale, never mutated, which is what
// makes a reload atomic for a reader.
type routerState struct {
	table   *Table
	clients map[string]Querier // backend name → client
	byKey   map[clientKey]Querier
}

// Router dispatches upstream queries across the backends of a live routing
// table, and lets that table be replaced without a restart.
//
// It satisfies BOTH Querier and QuerierSource:
//
//   - as a QuerierSource it hands each build a Querier bound to one immutable
//     snapshot, so a reload cannot change which backends a build in flight
//     reaches;
//   - as a Querier it dispatches with an empty Selector, which is what the
//     readiness and retention `up{}` probes want — the probe family accepts no
//     request dimension, so it reaches every backend serving it.
//
// A deployment with one backend routes through exactly the same code path, so
// the compatibility claim is exercised by the whole existing test suite rather
// than by a separate branch.
type Router struct {
	factory ClientFactory
	metrics Metrics

	state atomic.Pointer[routerState]
	// mu serialises Swap so two concurrent reloads cannot interleave client
	// construction. Readers never take it — they load the state pointer.
	mu sync.Mutex
}

var (
	_ Querier       = (*Router)(nil)
	_ QuerierSource = (*Router)(nil)
)

// NewRouter constructs a Router serving table t. factory may be nil, in which
// case DefaultClientFactory(m) is used.
func NewRouter(t *Table, m Metrics, factory ClientFactory) (*Router, error) {
	if t == nil || t.Len() == 0 {
		return nil, fmt.Errorf("router: nil or empty routing table")
	}
	if factory == nil {
		factory = DefaultClientFactory(m)
	}
	r := &Router{factory: factory, metrics: m}
	st, err := r.buildState(t, nil)
	if err != nil {
		return nil, err
	}
	r.state.Store(st)
	routerMetricsOf(m).SetBackends(backendNames(t.Backends()))
	return r, nil
}

// Table returns the live routing table. It is safe to call concurrently with a
// Swap; the returned table is immutable.
func (r *Router) Table() *Table {
	st := r.state.Load()
	if st == nil {
		return nil
	}
	return st.table
}

// Swap replaces the live routing table.
//
// The new table is fully realised — every client constructed — BEFORE the
// pointer moves, so a failure leaves the previous table serving unchanged and a
// partially applied table is never observable. Clients whose identity survived
// the swap are carried over; those whose identity disappeared have their
// connection pools released.
func (r *Router) Swap(t *Table) error {
	if t == nil || t.Len() == 0 {
		return fmt.Errorf("router: refusing to swap in a nil or empty routing table")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	prev := r.state.Load()
	next, err := r.buildState(t, prev)
	if err != nil {
		return err
	}
	r.state.Store(next)
	routerMetricsOf(r.metrics).SetBackends(backendNames(t.Backends()))

	if prev != nil {
		for key, q := range prev.byKey {
			if _, kept := next.byKey[key]; kept {
				continue
			}
			closeIdle(q)
		}
	}
	return nil
}

// buildState realises every client the table needs, reusing prev's clients
// wherever the identity is unchanged.
func (r *Router) buildState(t *Table, prev *routerState) (*routerState, error) {
	backends := t.Backends()
	st := &routerState{
		table:   t,
		clients: make(map[string]Querier, len(backends)),
		byKey:   make(map[clientKey]Querier, len(backends)),
	}
	// Clients created during this build, so a later failure can release them
	// rather than leaking a pool for a table that never went live.
	var fresh []Querier

	for _, b := range backends {
		key := keyOf(b)
		if q, ok := st.byKey[key]; ok {
			// Two backends addressing the same endpoint with the same
			// credentials — legitimate when they split families or zones.
			st.clients[b.Name()] = q
			continue
		}
		if prev != nil {
			if q, ok := prev.byKey[key]; ok {
				st.byKey[key] = q
				st.clients[b.Name()] = q
				continue
			}
		}
		q, err := r.factory(b)
		if err != nil {
			for _, c := range fresh {
				closeIdle(c)
			}
			return nil, fmt.Errorf("backend %q: %w", b.Name(), err)
		}
		fresh = append(fresh, q)
		st.byKey[key] = q
		st.clients[b.Name()] = q
	}
	return st, nil
}

// closeIdle releases a client's connection pool when it offers one. A Querier
// that does not (a mock, an embedder's own implementation) is left alone.
func closeIdle(q Querier) {
	if c, ok := q.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// QuerierFor returns the Querier this build must dispatch through. The routing
// snapshot is read ONCE here and closed over, which is what makes "a reload
// does not disturb a build in flight" structural rather than best-effort.
func (r *Router) QuerierFor(sel Selector) Querier {
	st := r.state.Load()
	return &fanoutQuerier{
		table:   st.table,
		clients: st.clients,
		az:      normaliseValues(sel.AZ),
		metrics: r.metrics,
	}
}

// Instant dispatches with an empty Selector — the unfiltered fan-out. It is
// what the readiness and retention probes use: the `probe` family accepts no
// request dimension, so the query reaches every backend serving it and fails
// naming any that did not answer.
func (r *Router) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	return r.QuerierFor(Selector{}).Instant(ctx, name, query, ts)
}

// Prober is the OPTIONAL upgrade a Querier may satisfy to expose a readiness
// probe across every backend it dispatches to. A *Router does; a plain
// *Client (or a mock) does not, so a consumer that type-asserts for it falls
// back to a single up{} query and behaves exactly as it does today.
//
// It exists because a readiness probe wants a DIFFERENT failure mode from an
// ordinary query. A query fails closed on the first backend error and cancels
// the rest (a partial graph is worse than an error), which means it can only
// ever name one backend. A probe's whole purpose is telling the operator WHICH
// store is down, so it must ask every backend and report all the failures.
type Prober interface {
	ProbeAll(ctx context.Context, ts time.Time) error
}

var _ Prober = (*Router)(nil)

// ProbeError names the backends that did not answer a readiness probe.
//
// It carries names ONLY — never a URL, host, IP, or upstream error string. The
// readiness endpoint is unauthenticated, so its body must not disclose the
// internal topology; a backend name is an operator-chosen label in the
// operator's own routing file, which is exactly the identifier they need to
// act on. The underlying errors are logged server-side by the client.
type ProbeError struct {
	// Failed is the sorted set of backend names that did not answer.
	Failed []string
}

func (e *ProbeError) Error() string {
	return "upstream backends did not answer: " + strings.Join(e.Failed, ", ")
}

// ProbeAll issues the up{} probe to every backend serving the probe family and
// reports which of them did not answer.
//
// Every probe runs concurrently under the caller's context, so the probe
// latency is that of the slowest backend rather than the sum, and a single
// deadline covers them all. Unlike a query fan-out this does NOT cancel the
// remaining probes on the first failure: a probe that stopped at the first
// error could only ever name one backend, and "one of your six upstreams is
// down" is not actionable.
func (r *Router) ProbeAll(ctx context.Context, ts time.Time) error {
	st := r.state.Load()
	selected := st.table.Select(FamilyProbe, nil)
	if len(selected) == 0 {
		// Unreachable through NewTable, which requires every family to be
		// served, but a probe that silently reports ready because it asked
		// nothing would be the worst possible failure of this endpoint.
		return &ProbeError{Failed: []string{"<none configured>"}}
	}

	query := Render(QUpProbe, 0, LabelKeys{}, Selector{})
	failed := make([]string, len(selected))
	var wg sync.WaitGroup
	for i, b := range selected {
		wg.Add(1)
		go func(i int, b Backend) {
			defer wg.Done()
			q, ok := st.clients[b.Name()]
			if !ok {
				failed[i] = b.Name()
				return
			}
			if _, err := q.Instant(ctx, string(QUpProbe), query, ts); err != nil {
				routerMetricsOf(r.metrics).IncBackendQueryFailure(b.Name())
				failed[i] = b.Name()
			}
		}(i, b)
	}
	wg.Wait()

	// selected is already in ascending name order, so the reported set is
	// deterministic without a further sort.
	names := make([]string, 0, len(failed))
	for _, n := range failed {
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return &ProbeError{Failed: names}
}
