package route

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	envoyroute "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/build"
	"github.com/akira-core/kube-state-graph/pkg/route/store"
	"github.com/akira-core/kube-state-graph/pkg/route/translate"
)

var memoAt = time.Unix(1_700_000_000, 0).UTC()

// referenceAnswers resolves every request ALONE, each through its own fresh
// resolver — the pre-memo behaviour every shared path must reproduce.
func referenceAnswers(t *testing.T, snap store.TrafficSnapshot, reqs []build.RouteRequest) []routeAnswer {
	t.Helper()
	out := make([]routeAnswer, len(reqs))
	for i, req := range reqs {
		r := newResolver(&fakeStore{snap: snap}, &fakeTranslator{}, &fakeMatcher{})
		dest, outcome, err := r.ResolveRoute(t.Context(), req)
		out[i] = routeAnswer{Dest: dest, Outcome: outcome}
		if err != nil {
			out[i].Err = err.Error()
		}
	}
	return out
}

// C8 + C9 step 1: 32 FQDNs behind ONE ingress IP and ONE gateway listener
// probe the store once, load the IP's configuration once and translate the
// RouteConfiguration once per build — under concurrent keys (-race) — and
// every key's answer equals its stand-alone answer.
func TestBuildScoped_SharedIngressDoesEachStageOnce(t *testing.T) {
	for _, d := range buildDrivers {
		t.Run(d.name, func(t *testing.T) {
			snap := sharedIngressSnapshot(t, memoAt)
			reqs := sharedIngressRequests(32, memoAt)
			st := &fakeStore{snap: snap, latency: time.Millisecond}
			tr := &fakeTranslator{latency: time.Millisecond}
			r := newResolver(st, tr, &fakeMatcher{})

			got := d.run(t.Context(), r, reqs)

			assert.Equal(t, referenceAnswers(t, snap, reqs), got)
			assert.Equal(t, int64(1), st.probes.Load(), "one probe per (ip, at)")
			assert.Equal(t, int64(1), st.loads.Load(), "one configuration load per (cluster, ip, at)")
			assert.Equal(t, int64(1), tr.calls.Load(), "one translation per RouteConfiguration")
		})
	}
}

// buildDrivers are the two ways a build reaches the engine: the batch the
// prescan uses when the scope offers it, and the per-key path every other
// resolver (and a batch-less scope) takes. Per-build sharing must hold on both.
var buildDrivers = []struct {
	name string
	run  func(context.Context, build.RouteResolver, []build.RouteRequest) []routeAnswer
}{
	{"batch", resolveBuild},
	{"per_key", resolveBuildPerKey},
}

// Different ports select different listeners, so different
// RouteConfigurations: each is translated once, never shared across names.
func TestBuildScoped_TranslationKeyedByRouteConfiguration(t *testing.T) {
	for _, d := range buildDrivers {
		t.Run(d.name, func(t *testing.T) {
			snap := sharedIngressSnapshot(t, memoAt)
			reqs := sharedIngressRequests(6, memoAt)
			for i := range reqs {
				if i%2 == 1 {
					reqs[i].Port = 80
				}
			}
			tr := &fakeTranslator{}
			r := newResolver(&fakeStore{snap: snap}, tr, &fakeMatcher{})

			got := d.run(t.Context(), r, reqs)

			assert.Equal(t, referenceAnswers(t, snap, reqs), got)
			assert.Equal(t, int64(2), tr.calls.Load(), "https.443 and http.80 are two RouteConfigurations")
		})
	}
}

// A failed configuration load is not remembered: the next key behind the same
// IP loads again and succeeds.
func TestBuildScoped_LoadErrorIsRetried(t *testing.T) {
	snap := sharedIngressSnapshot(t, memoAt)
	loadErr := errors.New("clickhouse down")
	var n atomic.Int64
	st := &fakeStore{snap: snap}
	st.loadHook = func(string, string) (store.TrafficSnapshot, error) {
		if n.Add(1) == 1 {
			return store.TrafficSnapshot{}, loadErr
		}
		return snap, nil
	}
	scope := newResolver(st, &fakeTranslator{}, &fakeMatcher{}).BuildScoped()
	reqs := sharedIngressRequests(2, memoAt)

	_, outcome, err := scope.ResolveRoute(t.Context(), reqs[0])
	require.ErrorIs(t, err, loadErr)
	assert.Empty(t, outcome, "an infrastructure failure carries no outcome")

	_, outcome, err = scope.ResolveRoute(t.Context(), reqs[1])
	require.NoError(t, err)
	assert.Equal(t, build.RouteHit, outcome)
	assert.Equal(t, int64(2), st.loads.Load())
}

// A failed translation is not remembered either.
func TestBuildScoped_TranslationErrorIsRetried(t *testing.T) {
	snap := sharedIngressSnapshot(t, memoAt)
	trErr := errors.New("istiod rejected the config")
	var n atomic.Int64
	tr := &fakeTranslator{}
	tr.hook = func(in translate.ScopedInput) (*envoyroute.RouteConfiguration, error) {
		if n.Add(1) == 1 {
			return nil, trErr
		}
		name, _ := translate.ListenerFor(in)
		return &envoyroute.RouteConfiguration{Name: name}, nil
	}
	scope := newResolver(&fakeStore{snap: snap}, tr, &fakeMatcher{}).BuildScoped()
	reqs := sharedIngressRequests(2, memoAt)

	_, _, err := scope.ResolveRoute(t.Context(), reqs[0])
	require.ErrorIs(t, err, trErr)
	_, outcome, err := scope.ResolveRoute(t.Context(), reqs[1])
	require.NoError(t, err)
	assert.Equal(t, build.RouteHit, outcome)
	assert.Equal(t, int64(2), tr.calls.Load())
}

// One key's per-call timeout must not become another key's error: the load
// leader times out while a second key with a live context waits on it, and
// the second key loads for itself.
func TestBuildScoped_LeaderTimeoutIsNotShared(t *testing.T) {
	snap := sharedIngressSnapshot(t, memoAt)
	st := &fakeStore{snap: snap, latency: 50 * time.Millisecond}
	scope := newResolver(st, &fakeTranslator{}, &fakeMatcher{}).BuildScoped()
	reqs := sharedIngressRequests(2, memoAt)

	shortCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	leaderErr := make(chan error, 1)
	go func() {
		_, _, err := scope.ResolveRoute(shortCtx, reqs[0])
		leaderErr <- err
	}()
	time.Sleep(5 * time.Millisecond) // the second key joins the in-flight probe/load

	_, outcome, err := scope.ResolveRoute(t.Context(), reqs[1])
	require.NoError(t, err, "the leader's deadline must not reach this key")
	assert.Equal(t, build.RouteHit, outcome)
	assert.ErrorIs(t, <-leaderErr, context.DeadlineExceeded)
}
