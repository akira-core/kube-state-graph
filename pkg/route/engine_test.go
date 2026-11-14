package route

import (
	"context"
	"sync"
	"testing"
	"time"

	envoyroute "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/route/matchcheck"
)

func TestDefaultEngineSlots(t *testing.T) {
	assert.GreaterOrEqual(t, defaultEngineSlots(), 1)
	r := NewResolver(&fakeStore{}, matchcheck.Runner{})
	assert.Equal(t, defaultEngineSlots(), cap(r.engine))
}

// Concurrent builds share ONE ceiling: four builds at four different instants
// (no memo or cache sharing between them) each resolve keys with the
// prescan's per-build concurrency, yet translations and matcher runs in
// progress at once never exceed the process-wide limit.
func TestEngineLimit_BoundsConcurrentBuilds(t *testing.T) {
	for _, d := range buildDrivers {
		t.Run(d.name, func(t *testing.T) {
			const slots = 2
			base := time.Unix(1_700_000_000, 0).UTC()
			gauge := &inflightGauge{}
			st := &fakeStore{snap: sharedIngressSnapshot(t, base)}
			r := newResolver(st, &fakeTranslator{latency: 2 * time.Millisecond, gauge: gauge},
				&fakeMatcher{latency: 2 * time.Millisecond, gauge: gauge})
			r.engine = make(chan struct{}, slots)

			var wg sync.WaitGroup
			for b := range 4 {
				wg.Go(func() {
					// Distinct instants and ports: no memo or cache sharing
					// between builds, two RouteConfigurations per build.
					at := base.Add(time.Duration(b) * time.Millisecond)
					reqs := sharedIngressRequests(8, at)
					for i := range reqs {
						if i%2 == 1 {
							reqs[i].Port = 80
						}
					}
					for _, a := range d.run(t.Context(), r, reqs) {
						assert.Empty(t, a.Err)
					}
				})
			}
			wg.Wait()

			assert.LessOrEqual(t, gauge.max(), slots, "engine work in flight must never exceed the limit")
			assert.Equal(t, slots, gauge.max(), "the limit is reached, so the test really contends")
		})
	}
}

// A key that cannot get a slot before its context ends degrades with an error
// and NO outcome — never a routing miss that could be cached or open the
// RouteNoGateway-gated LB fallback.
func TestEngineLimit_SaturationIsAnErrorWithoutOutcome(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	entered := make(chan struct{})
	release := make(chan struct{})
	mt := &fakeMatcher{}
	mt.hook = func(_ context.Context, _ *envoyroute.RouteConfiguration, qs []matchcheck.Query) ([]string, error) {
		close(entered)
		<-release
		return make([]string, len(qs)), nil
	}
	r := newResolver(&fakeStore{snap: sharedIngressSnapshot(t, at)}, &fakeTranslator{}, mt)
	r.engine = make(chan struct{}, 1)
	reqs := sharedIngressRequests(2, at)

	holder := make(chan struct{})
	go func() {
		defer close(holder)
		_, _, _ = r.ResolveRoute(t.Context(), reqs[0])
	}()
	<-entered // the only slot is now held by the first key's matcher run

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	dest, outcome, err := r.ResolveRoute(ctx, reqs[1])

	require.ErrorIs(t, err, errEngineSaturated)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, outcome)
	assert.Zero(t, dest)

	close(release)
	<-holder
}
