package route

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/build"
)

// The benchmark fixture must exercise the whole pipeline: every key reaches
// the matcher and comes back a routed hit on its own Service, stamped with the
// selected ingress cluster and the ingress LB identity.
func TestSharedIngressFixture_EveryKeyIsARoutedHit(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	st := &fakeStore{snap: sharedIngressSnapshot(t, at)}
	r := newResolver(st, &fakeTranslator{}, &fakeMatcher{})

	answers := resolveBuild(t.Context(), r, sharedIngressRequests(8, at))
	for i, a := range answers {
		require.Empty(t, a.Err)
		require.Equal(t, build.RouteHit, a.Outcome)
		require.Equal(t, build.RouteDestination{
			Cluster: sharedCluster, Namespace: "shop", Service: fmt.Sprintf("h%02d", i), Port: 8080,
			IngressNamespace: "istio-system", IngressService: "ingress-lb",
		}, a.Dest)
	}
}

// Stage latencies for the route benchmarks, scaled down ~10x from production
// (a LoadTrafficAt is ~5 ClickHouse round-trips, a translate builds an istiod
// world, a router_check_tool exec is a ~50-60 ms fork/exec) so a run finishes
// in seconds while keeping the stages' relative cost.
const (
	benchStoreLatency     = 2 * time.Millisecond // LoadTrafficAt; the probe is a quarter of it
	benchTranslateLatency = 2 * time.Millisecond
	benchExecLatency      = 5 * time.Millisecond
	benchKeys             = 32
)

func benchResolver(b *testing.B) (*Resolver, *fakeStore, *fakeTranslator, *fakeMatcher) {
	b.Helper()
	st := &fakeStore{snap: sharedIngressSnapshot(b, time.Unix(1_700_000_000, 0).UTC()), latency: benchStoreLatency}
	// The fixture's versions span ±1h around its own instant; every benchmark
	// instant below stays inside that span.
	tr := &fakeTranslator{latency: benchTranslateLatency}
	mt := &fakeMatcher{latency: benchExecLatency}
	return newResolver(st, tr, mt), st, tr, mt
}

// reportStageCounts records the expensive-stage calls per build alongside
// ns/op: with sleep-based fakes the wall time says little on its own, and the
// counts are what C8 (loads), C9 (translates, execs) and C10 (all three on a
// repeat build) are meant to change.
func reportStageCounts(b *testing.B, st *fakeStore, tr *fakeTranslator, mt *fakeMatcher) {
	n := float64(b.N)
	b.ReportMetric(float64(st.probes.Load())/n, "probes/op")
	b.ReportMetric(float64(st.loads.Load())/n, "loads/op")
	b.ReportMetric(float64(tr.calls.Load())/n, "translates/op")
	b.ReportMetric(float64(mt.execs.Load())/n, "execs/op")
}

// BenchmarkResolveRoute_SharedIngress is one COLD build: benchKeys distinct
// FQDNs behind a single ingress IP, all routed by one gateway's one
// RouteConfiguration. Each iteration uses a fresh resolution instant, so no
// answer carries over from a previous build.
func BenchmarkResolveRoute_SharedIngress(b *testing.B) {
	r, st, tr, mt := benchResolver(b)
	base := time.Unix(1_700_000_000, 0).UTC()
	b.ResetTimer()
	for i := range b.N {
		at := base.Add(time.Duration(i) * time.Millisecond)
		_ = resolveBuild(b.Context(), r, sharedIngressRequests(benchKeys, at))
	}
	b.StopTimer()
	reportStageCounts(b, st, tr, mt)
}

// BenchmarkResolveRoute_SharedIngressRepeat is the same build REPEATED at one
// resolution instant — what a dashboard refreshing inside one end-alignment
// step sends.
func BenchmarkResolveRoute_SharedIngressRepeat(b *testing.B) {
	r, st, tr, mt := benchResolver(b)
	at := time.Unix(1_700_000_000, 0).UTC()
	reqs := sharedIngressRequests(benchKeys, at)
	b.ResetTimer()
	for range b.N {
		_ = resolveBuild(b.Context(), r, reqs)
	}
	b.StopTimer()
	reportStageCounts(b, st, tr, mt)
}
