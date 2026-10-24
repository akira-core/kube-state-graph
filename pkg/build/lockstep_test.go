package build

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// lockstepQuerier counts the sequential upstream rounds a build takes. Every
// Instant blocks on the current gate; once the build has nothing left to do but
// wait on blocked queries, the test closes the gate and a new round begins. A
// query that depends on an earlier one can therefore only be issued in a later
// round, so the number of gates opened is the length of the build's critical
// path in upstream round trips — measured, not slept.
type lockstepQuerier struct {
	inner promql.Querier

	mu      sync.Mutex
	gate    chan struct{}
	pending []string
}

func newLockstepQuerier(inner promql.Querier) *lockstepQuerier {
	return &lockstepQuerier{inner: inner, gate: make(chan struct{})}
}

func (l *lockstepQuerier) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	l.mu.Lock()
	gate := l.gate
	l.pending = append(l.pending, name)
	l.mu.Unlock()
	select {
	case <-gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return l.inner.Instant(ctx, name, query, ts)
}

// release opens the current gate and returns the names of the queries it let
// through, sorted.
func (l *lockstepQuerier) release() []string {
	l.mu.Lock()
	old, names := l.gate, l.pending
	l.gate, l.pending = make(chan struct{}), nil
	l.mu.Unlock()
	close(old)
	slices.Sort(names)
	return names
}

// lockstepBuild is what runLockstep reports: the rounds the build took, the
// queries each round carried, and the graph it built.
type lockstepBuild struct {
	Rounds [][]string
	Graph  *graph.Graph
}

// maxLockstepRounds bounds the loop, so a build that deadlocks fails the test
// instead of spinning.
const maxLockstepRounds = 40

// runLockstep builds the Kubernetes-only graph for roots over fx inside a
// synctest bubble, one upstream round at a time. A fixture must keep every
// wave under scopeConcurrency queries, or the errgroup's limit splits one
// logical round into two and the count over-reports.
func runLockstep(t *testing.T, fx map[promql.Query]model.Vector, opts Options, roots graph.StorageRoots) lockstepBuild {
	t.Helper()
	var out lockstepBuild
	synctest.Test(t, func(t *testing.T) {
		lq := newLockstepQuerier(promqlfake.New(fx))
		type result struct {
			g   *graph.Graph
			err error
		}
		done := make(chan result, 1)
		go func() {
			g, err := New(lq, opts, nil, nil).BuildK8s(t.Context(), time.Minute, vlrEnd, vlrSel, roots)
			done <- result{g, err}
		}()
		for len(out.Rounds) <= maxLockstepRounds {
			synctest.Wait()
			select {
			case r := <-done:
				require.NoError(t, r.err)
				out.Graph = r.g
				return
			default:
			}
			round := lq.release()
			require.NotEmpty(t, round, "the build is blocked on something other than a query")
			out.Rounds = append(out.Rounds, round)
		}
		t.Fatalf("build did not finish within %d rounds: %v", maxLockstepRounds, out.Rounds)
	})
	return out
}

// lockstepOwner selects how the lockstep fixture's one pod is owned.
type lockstepOwner int

const (
	// ownedByStatefulSet needs no second controller stage.
	ownedByStatefulSet lockstepOwner = iota
	// ownedByDeployment goes pod -> ReplicaSet -> Deployment, the chain whose
	// second hop is the last round of the unoptimised read.
	ownedByDeployment
)

// lockstepEstate is a deliberately tiny estate: one pod, shop/app-0 on
// worker-1, mounting shop/data, which no other pod mounts. It is NOT
// planEstate(): the round count needs every wave small enough that no errgroup
// ever holds more than scopeConcurrency queries.
func lockstepEstate(owner lockstepOwner) map[promql.Query]model.Vector {
	const tracking = "annotation_argocd_argoproj_io_tracking_id"
	fx := map[promql.Query]model.Vector{
		promql.QPodInfo:  {planKSM("namespace", "shop", "pod", "app-0", "uid", "uid-a0", "node", "worker-1")},
		promql.QNodeInfo: {planKSM("node", "worker-1")},
		promql.QNodeAddresses: {
			planKSM("node", "worker-1", "type", "InternalIP", "address", "192.168.0.1"),
		},
		promql.QNodeStatusCondition: {planKSM("node", "worker-1", "condition", "Ready", "status", "true")},
		promql.QPVCBindings: {
			planKSM("namespace", "shop", "pod", "app-0", "persistentvolumeclaim", "data", "volume", "data"),
		},
		promql.QPVCInfo: {
			planKSM("namespace", "shop", "persistentvolumeclaim", "data", "volumename", "pvc-data", "storageclass", "netapp-nas"),
		},
		promql.QKubeletVolumeUsedBytes: {
			withValue(planKSM("namespace", "shop", "persistentvolumeclaim", "data"), 4096),
		},
		promql.QKubeletVolumeCapacityBytes: {
			withValue(planKSM("namespace", "shop", "persistentvolumeclaim", "data"), 8192),
		},
	}
	switch owner {
	case ownedByDeployment:
		fx[promql.QPodOwner] = model.Vector{planKSM("namespace", "shop", "pod", "app-0",
			"owner_kind", "ReplicaSet", "owner_name", "app-7d9", "owner_is_controller", "true")}
		fx[promql.QReplicaSetOwner] = model.Vector{planKSM("namespace", "shop", "replicaset", "app-7d9",
			"owner_kind", "Deployment", "owner_name", "app")}
		fx[promql.QDeploymentAnnotations] = model.Vector{planKSM("namespace", "shop", "deployment", "app",
			tracking, "storefront:apps/Deployment:shop/app")}
	default:
		fx[promql.QPodOwner] = model.Vector{planKSM("namespace", "shop", "pod", "app-0",
			"owner_kind", "StatefulSet", "owner_name", "app", "owner_is_controller", "true")}
		fx[promql.QStatefulSetAnnotations] = model.Vector{planKSM("namespace", "shop", "statefulset", "app",
			tracking, "storefront:apps/StatefulSet:shop/app")}
	}
	return fx
}

// TestLockstep_CountsDependentRounds checks the counter itself, with no build
// behind it: three queries where each needs the one before cost three rounds,
// and queries issued together cost one.
func TestLockstep_CountsDependentRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lq := newLockstepQuerier(promqlfake.New(nil))
		done := make(chan struct{})
		go func() {
			defer close(done)
			ctx := t.Context()
			var wg sync.WaitGroup
			for _, name := range []string{"a", "b"} {
				wg.Go(func() { _, _ = lq.Instant(ctx, name, "x", vlrEnd) })
			}
			wg.Wait()
			_, _ = lq.Instant(ctx, "c", "x", vlrEnd)
			_, _ = lq.Instant(ctx, "d", "x", vlrEnd)
		}()
		var rounds [][]string
		for {
			synctest.Wait()
			select {
			case <-done:
				require.Equal(t, [][]string{{"a", "b"}, {"c"}, {"d"}}, rounds)
				return
			default:
			}
			rounds = append(rounds, lq.release())
		}
	})
}
