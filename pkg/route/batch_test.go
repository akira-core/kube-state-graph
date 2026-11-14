package route

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	envoyroute "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/matchcheck"
)

var batchAt = time.Unix(1_700_000_000, 0).UTC()

const noIngressIP = "203.0.113.9"

// mixedWorld builds a fresh resolver over the shared-ingress fixture whose
// matcher misses "miss.example.com" and whose probe finds no ingress for
// noIngressIP. A fresh world per call is the "resolved alone" reference.
func mixedWorld(t *testing.T) (*Resolver, *fakeStore, *fakeTranslator, *fakeMatcher) {
	t.Helper()
	st := &fakeStore{snap: sharedIngressSnapshot(t, batchAt)}
	st.probeHook = func(ip string) ([]string, error) {
		if ip == noIngressIP {
			return nil, nil
		}
		return []string{sharedCluster}, nil
	}
	tr := &fakeTranslator{}
	mt := &fakeMatcher{miss: map[string]bool{"miss.example.com": true}}
	return newResolver(st, tr, mt), st, tr, mt
}

// mixedRequests covers every outcome the engine produces from one ingress:
// routed hits on two RouteConfigurations (https.443 and http.80), a route
// miss, a listener miss, an LB-Service fallback, a no-ingress miss, and two
// callers posing the same host (one query, two keys).
func mixedRequests() []build.RouteRequest {
	req := func(caller, host string, port int, ip string) build.RouteRequest {
		return build.RouteRequest{CallerCluster: caller, Host: host, Path: "/", Port: port, IPs: []string{ip}, At: batchAt}
	}
	return []build.RouteRequest{
		req(sharedCluster, "h00.example.com", 443, sharedIP),
		req(sharedCluster, "h01.example.com", 443, sharedIP),
		req("prod-02", "h01.example.com", 443, sharedIP),
		req(sharedCluster, "h02.example.com", 80, sharedIP),
		req(sharedCluster, "h03.example.com", 80, sharedIP),
		req(sharedCluster, "miss.example.com", 443, sharedIP),
		req(sharedCluster, "h04.example.com", 8443, sharedIP),
		req(sharedCluster, "api.other.org", 443, sharedIP),
		req(sharedCluster, "h05.example.com", 443, noIngressIP),
	}
}

func aloneAnswers(t *testing.T, reqs []build.RouteRequest) []routeAnswer {
	t.Helper()
	out := make([]routeAnswer, len(reqs))
	for i, req := range reqs {
		r, _, _, _ := mixedWorld(t)
		dest, outcome, err := r.ResolveRoute(t.Context(), req)
		out[i] = routeAnswer{Dest: dest, Outcome: outcome}
		if err != nil {
			out[i].Err = err.Error()
		}
	}
	return out
}

// The mixed key set really does cover every outcome — otherwise the parity
// test below would prove less than it claims.
func TestMixedRequests_CoverEveryOutcome(t *testing.T) {
	var got []build.RouteOutcome
	for _, a := range aloneAnswers(t, mixedRequests()) {
		require.Empty(t, a.Err)
		got = append(got, a.Outcome)
	}
	for _, want := range []build.RouteOutcome{
		build.RouteHit, build.RouteNoRoute, build.RouteNoListenerOnPort,
		build.RouteIngressLBService, build.RouteNoIngress,
	} {
		assert.Contains(t, got, want)
	}
}

// C9 step 2 parity: one batch answers every key exactly as each key resolved
// alone does, and as the per-key path through one scope does.
func TestResolveRoutes_ParityWithPerKeyResolution(t *testing.T) {
	reqs := mixedRequests()
	want := aloneAnswers(t, reqs)

	batchR, _, _, mt := mixedWorld(t)
	assert.Equal(t, want, resolveBuild(t.Context(), batchR, reqs), "batch")

	perKeyR, _, _, _ := mixedWorld(t)
	assert.Equal(t, want, resolveBuildPerKey(t.Context(), perKeyR, reqs), "per key")

	// Two RouteConfigurations reached the matcher (https.443, http.80): two
	// runs, the duplicate h01 query posed once.
	assert.Equal(t, int64(2), mt.execs.Load())
	assert.Equal(t, int64(5), mt.queries.Load(),
		"h00, h01 (posed once for both callers), miss on https.443; h02, h03 on http.80")
}

// 32 keys on ONE RouteConfiguration: one router_check_tool run with all 32
// queries.
func TestResolveRoutes_OneMatcherRunPerRouteConfiguration(t *testing.T) {
	st := &fakeStore{snap: sharedIngressSnapshot(t, batchAt)}
	tr, mt := &fakeTranslator{}, &fakeMatcher{}
	r := newResolver(st, tr, mt)

	got := resolveBuild(t.Context(), r, sharedIngressRequests(32, batchAt))

	for _, a := range got {
		require.Empty(t, a.Err)
		require.Equal(t, build.RouteHit, a.Outcome)
	}
	assert.Equal(t, int64(1), mt.execs.Load())
	assert.Equal(t, int64(32), mt.queries.Load())
	assert.Equal(t, int64(1), tr.calls.Load())
	assert.Equal(t, int64(1), st.loads.Load())
}

// router_check_tool reports all-or-nothing, so one query it cannot answer
// fails the whole run. The batch retries query by query: only that key fails,
// every sibling answers as it would alone.
func TestResolveRoutes_FailedRunIsRetriedQueryByQuery(t *testing.T) {
	r, _, _, mt := mixedWorld(t)
	poison := errors.New("router_check_tool: parsed 2 of 3 per-case results")
	mt.hook = func(_ context.Context, _ *envoyroute.RouteConfiguration, qs []matchcheck.Query) ([]string, error) {
		if slices.ContainsFunc(qs, func(q matchcheck.Query) bool { return q.Host == "h01.example.com" }) {
			return nil, poison
		}
		out := make([]string, len(qs))
		for i, q := range qs {
			out[i] = "outbound|8080||" + q.Host[:3] + ".shop.svc.cluster.local"
		}
		return out, nil
	}
	reqs := sharedIngressRequests(3, batchAt)

	got := resolveBuild(t.Context(), r, reqs)

	assert.Equal(t, build.RouteHit, got[0].Outcome)
	assert.Contains(t, got[1].Err, poison.Error())
	assert.Empty(t, got[1].Outcome)
	assert.Equal(t, build.RouteHit, got[2].Outcome)
	assert.Equal(t, int64(1+3), mt.execs.Load(), "one batched run, then one run per query")
}

// A batch whose context has already ended answers nothing: every request
// keeps Answered=false and gets no index entry upstream.
func TestResolveRoutes_EndedContextAnswersNothing(t *testing.T) {
	r, st, _, _ := mixedWorld(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	answers := r.newScope().ResolveRoutes(ctx, mixedRequests(), build.RouteBatchLimits{Concurrency: 4})

	for _, a := range answers {
		assert.False(t, a.Answered)
	}
	assert.Zero(t, st.probes.Load())
}

// A panic in the matcher fails that group's keys with an error and no
// outcome, the process survives, and the cache leases are released: a later
// resolution of the same key runs instead of blocking.
func TestResolveRoutes_PanicFailsTheGroupAndReleasesLeases(t *testing.T) {
	r, _, _, mt := mixedWorld(t)
	var once sync.Once
	mt.hook = func(_ context.Context, _ *envoyroute.RouteConfiguration, qs []matchcheck.Query) (out []string, err error) {
		once.Do(func() { panic("router_check_tool wrapper blew up") })
		out = make([]string, len(qs))
		for i, q := range qs {
			out[i] = "outbound|8080||" + q.Host[:3] + ".shop.svc.cluster.local"
		}
		return out, nil
	}
	reqs := sharedIngressRequests(3, batchAt)

	got := resolveBuild(t.Context(), r, reqs)
	for _, a := range got {
		assert.Contains(t, a.Err, "panicked")
		assert.Empty(t, a.Outcome)
	}

	done := make(chan routeAnswer, 1)
	go func() { done <- resolveBuild(t.Context(), r, reqs[:1])[0] }()
	select {
	case a := <-done:
		assert.Equal(t, build.RouteHit, a.Outcome, "the failed answer was not cached and the lease was released")
	case <-time.After(5 * time.Second):
		t.Fatal("a later resolution blocked on a lease the panicking batch never released")
	}
}

// Two concurrent builds at the same instant over the same keys, in opposite
// order, each wait on the other's in-flight keys. Waits are deferred until a
// batch has released its own leases, so neither can deadlock, and both see
// the answers each key gets alone.
func TestResolveRoutes_ConcurrentBuildsDoNotDeadlock(t *testing.T) {
	r, _, _, mt := mixedWorld(t)
	mt.latency = 5 * time.Millisecond
	reqs := sharedIngressRequests(16, batchAt)
	reversed := slices.Clone(reqs)
	slices.Reverse(reversed)
	want := aloneAnswers(t, reqs)

	var wg sync.WaitGroup
	results := make([][]routeAnswer, 2)
	wg.Go(func() { results[0] = resolveBuild(t.Context(), r, reqs) })
	wg.Go(func() { results[1] = resolveBuild(t.Context(), r, reversed) })
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("two concurrent builds deadlocked on each other's keys")
	}

	assert.Equal(t, want, results[0])
	got := slices.Clone(results[1])
	slices.Reverse(got)
	assert.Equal(t, want, got)
}

// Duplicate keys inside one batch share one resolution and one answer.
func TestResolveRoutes_DuplicateKeysShareAnAnswer(t *testing.T) {
	r, _, _, mt := mixedWorld(t)
	reqs := sharedIngressRequests(2, batchAt)
	reqs = append(reqs, reqs[0])

	got := resolveBuild(t.Context(), r, reqs)

	assert.Equal(t, got[0], got[2])
	assert.Equal(t, build.RouteHit, got[2].Outcome)
	assert.Equal(t, int64(2), mt.queries.Load())
}
