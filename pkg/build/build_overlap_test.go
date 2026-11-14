package build

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// latencyQuerier answers every query from fixtures after a fixed delay, so the
// wall time of a build is a function of how many query ROUNDS it runs in
// sequence rather than of how many queries it issues.
type latencyQuerier struct {
	delay    time.Duration
	fixtures map[promql.Query]model.Vector
}

func (q *latencyQuerier) Instant(ctx context.Context, name, _ string, _ time.Time) (model.Vector, error) {
	t := time.NewTimer(q.delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
	}
	return q.fixtures[promql.Query(name)], nil
}

// syntheticEstate is a /v1/graph estate of pods pods, every one of them
// mounting a claim on a NetApp FlexVol, and a service-graph series from each
// pod to the next. The claims make the build run its second (QoS) wave, so a
// cold build is three rounds: topology, QoS, service graph.
func syntheticEstate(pods int) map[promql.Query]model.Vector {
	fx := map[promql.Query]model.Vector{}
	add := func(q promql.Query, m model.Metric, v float64) {
		fx[q] = append(fx[q], &model.Sample{Metric: m, Value: model.SampleValue(v)})
	}
	add(promql.QNodeInfo, model.Metric{"cluster": "c1", "node": "worker-0"}, 1)
	for i := range pods {
		pod := model.LabelValue(fmt.Sprintf("app-%04d", i))
		uid := model.LabelValue(fmt.Sprintf("uid-%04d", i))
		claim := model.LabelValue(fmt.Sprintf("data-%04d", i))
		pv := model.LabelValue(fmt.Sprintf("pvc-%08d-0000", i))
		add(promql.QPodInfo, model.Metric{"cluster": "c1", "namespace": "shop", "pod": pod, "uid": uid, "node": "worker-0"}, 1)
		add(promql.QPVCBindings, model.Metric{"cluster": "c1", "namespace": "shop", "pod": pod, "persistentvolumeclaim": claim}, 1)
		add(promql.QPVCInfo, model.Metric{"cluster": "c1", "namespace": "shop", "persistentvolumeclaim": claim, "volumename": pv, "storageclass": "netapp-nas"}, 1)
		add(promql.QVolumeLabels, model.Metric{
			"cluster": "ontap-prod", "node": "ontap-prod-01", "aggr": "aggr1", "svm": "svm0",
			"volume": model.LabelValue("trident_" + strings.ReplaceAll(string(pv), "-", "_")),
		}, 1)
		next := model.LabelValue(fmt.Sprintf("uid-%04d", (i+1)%pods))
		add(promql.QServiceGraphTotal, model.Metric{
			"cluster": "c1", "client": pod, "server": "next",
			"client_k8s_pod_uid": uid, "server_k8s_pod_uid": next,
		}, 1)
	}
	return fx
}

// BenchmarkBuilder_Build is a cold /v1/graph build against an upstream that
// takes delay per query. Its wall time counts the sequential query rounds the
// build runs, which is what a store's latency multiplies.
func BenchmarkBuilder_Build(b *testing.B) {
	const delay = 5 * time.Millisecond
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	b.Cleanup(func() { slog.SetDefault(prev) })
	for _, pods := range []int{50, 500} {
		b.Run(fmt.Sprintf("pods=%d", pods), func(b *testing.B) {
			q := &latencyQuerier{delay: delay, fixtures: syntheticEstate(pods)}
			builder := New(q, Options{}, nil, nil)
			b.ReportAllocs()
			for b.Loop() {
				g, err := builder.Build(context.Background(), 5*time.Minute, probeTestEnd, promql.Selector{})
				if err != nil || len(g.Edges) == 0 {
					b.Fatalf("build: %v (%d edges)", err, len(g.Edges))
				}
			}
		})
	}
}

// overlapQuerier holds kube_pod_info until a traces_service_graph_* query has
// arrived — or until fallback elapses, which a build that issues the service
// graph only after topology runs into. Every other query answers at once.
type overlapQuerier struct {
	fixtures map[promql.Query]model.Vector
	fallback time.Duration

	once     sync.Once
	sgSeen   chan struct{}
	mu       sync.Mutex
	timedOut bool
}

func newOverlapQuerier(fx map[promql.Query]model.Vector) *overlapQuerier {
	return &overlapQuerier{fixtures: fx, fallback: 3 * time.Second, sgSeen: make(chan struct{})}
}

func (q *overlapQuerier) Instant(ctx context.Context, name, _ string, _ time.Time) (model.Vector, error) {
	if strings.HasPrefix(name, "traces_service_graph_") {
		q.once.Do(func() { close(q.sgSeen) })
	}
	if promql.Query(name) == promql.QPodInfo {
		select {
		case <-q.sgSeen:
		case <-time.After(q.fallback):
			q.mu.Lock()
			q.timedOut = true
			q.mu.Unlock()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return q.fixtures[promql.Query(name)], nil
}

// C6: an unfiltered build issues the three service-graph queries while the
// topology fan-out is still in flight — they take no request matcher and do
// not depend on topology, only the parse does. The topology leg here cannot
// return until a service-graph query has arrived, so a build that waits for
// topology first runs into the fallback.
func TestBuild_UnfilteredServiceGraphOverlapsTopology(t *testing.T) {
	q := newOverlapQuerier(syntheticEstate(3))

	g, err := New(q, Options{}, nil, nil).Build(t.Context(), 5*time.Minute, probeTestEnd, promql.Selector{})

	require.NoError(t, err)
	q.mu.Lock()
	defer q.mu.Unlock()
	assert.False(t, q.timedOut, "kube_pod_info had to wait for the fallback: the service graph was issued after topology")
	assert.NotEmpty(t, g.Edges, "the prefetched series are still joined against topology")
}

// failingTopologyQuerier fails kube_pod_info and answers the service graph
// only once its context is cancelled (or after fallback), recording which.
// With sgSeen set, kube_pod_info fails only after a service-graph query has
// arrived, so the failure lands while the prefetch is provably in flight.
type failingTopologyQuerier struct {
	topoErr  error
	fallback time.Duration
	sgSeen   chan struct{}

	once               sync.Once
	mu                 sync.Mutex
	sgCancelled, sgRan int
}

func (q *failingTopologyQuerier) Instant(ctx context.Context, name, _ string, _ time.Time) (model.Vector, error) {
	if strings.HasPrefix(name, "traces_service_graph_") {
		if q.sgSeen != nil {
			q.once.Do(func() { close(q.sgSeen) })
		}
		select {
		case <-ctx.Done():
			q.mu.Lock()
			q.sgCancelled++
			q.mu.Unlock()
			return nil, ctx.Err()
		case <-time.After(q.fallback):
			q.mu.Lock()
			q.sgRan++
			q.mu.Unlock()
			return model.Vector{}, nil
		}
	}
	if promql.Query(name) == promql.QPodInfo {
		if q.sgSeen != nil {
			select {
			case <-q.sgSeen:
			case <-time.After(q.fallback):
			}
		}
		return nil, q.topoErr
	}
	return model.Vector{}, nil
}

// A topology failure decides the build: the service-graph prefetch is
// cancelled rather than left running, and its context.Canceled never reaches
// the classification — the build is the upstream failure (502), not a
// client cancellation (499).
func TestBuild_TopologyFailureCancelsServiceGraphPrefetch(t *testing.T) {
	q := &failingTopologyQuerier{
		topoErr: errors.New("upstream 5xx"), fallback: 3 * time.Second, sgSeen: make(chan struct{}),
	}

	start := time.Now()
	_, err := New(q, Options{}, nil, nil).Build(t.Context(), 5*time.Minute, probeTestEnd, promql.Selector{})

	require.Error(t, err)
	be, ok := errors.AsType[*Error](err)
	require.True(t, ok, "a typed build error, got %v", err)
	assert.Equal(t, ReasonUpstream, be.Reason, "the topology failure, not the cancelled prefetch")
	assert.Equal(t, string(promql.QPodInfo), be.Query)
	assert.Less(t, time.Since(start), q.fallback, "the build does not wait for the prefetch")
	q.mu.Lock()
	defer q.mu.Unlock()
	assert.Zero(t, q.sgRan, "no service-graph query runs to completion after topology failed")
	assert.Positive(t, q.sgCancelled, "the prefetch was in flight and was cancelled, not merely never started")
}

// The outside-retention classification is the other path that discards the
// prefetch. It stays a 400 outside_retention, never a cancellation, and the
// prefetch that answers within outsideRetentionPrefetchGrace is left to finish
// rather than cancelled: a cancelled in-flight
// query would count as an upstream failure and log at Error on a request
// that is the client's to fix.
func TestBuild_OutsideRetentionDiscardsServiceGraphPrefetch(t *testing.T) {
	q := &failingTopologyQuerier{fallback: 20 * time.Millisecond}
	upQ := &upAnsweringQuerier{inner: q}

	_, err := New(upQ, Options{}, nil, nil).Build(t.Context(), 5*time.Minute, probeTestEnd, promql.Selector{})

	require.Error(t, err)
	be, ok := errors.AsType[*Error](err)
	require.True(t, ok, "a typed build error, got %v", err)
	assert.Equal(t, ReasonOutsideRetention, be.Reason)
	q.mu.Lock()
	defer q.mu.Unlock()
	assert.Zero(t, q.sgCancelled, "the prefetch is not cancelled on outside_retention")
	assert.Equal(t, 3, q.sgRan, "all three service-graph queries ran to completion")
}

// The wait is bounded: a split routing table can leave the service-graph store
// holding the window the kube_pod_info store has aged out, and the 400 must
// not take as long as that scan. Past outsideRetentionPrefetchGrace the build
// cancels the prefetch and returns.
func TestBuild_OutsideRetentionBoundsTheServiceGraphPrefetchWait(t *testing.T) {
	q := &failingTopologyQuerier{fallback: 10 * time.Second}
	upQ := &upAnsweringQuerier{inner: q}

	start := time.Now()
	_, err := New(upQ, Options{}, nil, nil).Build(t.Context(), 5*time.Minute, probeTestEnd, promql.Selector{})
	took := time.Since(start)

	require.Equal(t, ReasonOutsideRetention, AsReason(err))
	assert.GreaterOrEqual(t, took, outsideRetentionPrefetchGrace, "a slow prefetch is given the grace")
	assert.Less(t, took, q.fallback, "but the 400 does not wait for the slow store")
	q.mu.Lock()
	defer q.mu.Unlock()
	assert.Equal(t, 3, q.sgCancelled, "the prefetch still in flight past the grace is cancelled")
	assert.Zero(t, q.sgRan)
}

// upAnsweringQuerier answers the retention probe as healthy and everything
// else through inner, with kube_pod_info returning nothing.
type upAnsweringQuerier struct{ inner *failingTopologyQuerier }

func (q *upAnsweringQuerier) Instant(ctx context.Context, name, query string, at time.Time) (model.Vector, error) {
	switch promql.Query(name) {
	case promql.QUpProbe:
		return model.Vector{{Metric: model.Metric{"job": "ksm"}, Value: 1}}, nil
	case promql.QPodInfo:
		return model.Vector{}, nil
	default:
		return q.inner.Instant(ctx, name, query, at)
	}
}

// A service-graph failure on an otherwise healthy build still fails it as
// before: the prefetch's own error is the build's error once topology lands.
func TestBuild_PrefetchedServiceGraphFailureFailsTheBuild(t *testing.T) {
	fx := syntheticEstate(2)
	q := &sgFailingQuerier{fixtures: fx}

	_, err := New(q, Options{}, nil, nil).Build(t.Context(), 5*time.Minute, probeTestEnd, promql.Selector{})

	require.Error(t, err)
	be, ok := errors.AsType[*Error](err)
	require.True(t, ok)
	assert.Equal(t, ReasonUpstream, be.Reason)
	assert.Equal(t, "service-graph read failed", be.Message)
}

type sgFailingQuerier struct {
	fixtures map[promql.Query]model.Vector
}

func (q *sgFailingQuerier) Instant(_ context.Context, name, _ string, _ time.Time) (model.Vector, error) {
	if promql.Query(name) == promql.QServiceGraphTotal {
		return nil, errors.New("vmselect 503")
	}
	return q.fixtures[promql.Query(name)], nil
}
