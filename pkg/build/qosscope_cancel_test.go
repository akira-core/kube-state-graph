package build

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// blockingQoSFake fails every QoS chunk naming failVolume at once and holds
// every other QoS chunk until its context is cancelled — or until a generous
// fallback elapses, which a sibling that was never cancelled runs into.
type blockingQoSFake struct {
	failVolume string
	fallback   time.Duration

	cancelled, timedOut atomic.Int32
}

func (f *blockingQoSFake) Instant(ctx context.Context, name, query string, _ time.Time) (model.Vector, error) {
	if !isQoSWorkload(promql.Query(name)) {
		return model.Vector{}, nil
	}
	if strings.Contains(query, f.failVolume) {
		return nil, errors.New("upstream 5xx")
	}
	select {
	case <-ctx.Done():
		f.cancelled.Add(1)
		return nil, ctx.Err()
	case <-time.After(f.fallback):
		f.timedOut.Add(1)
		return model.Vector{}, nil
	}
}

// Under a fail-closed plan one failed QoS chunk decides the build, so its
// siblings must be cancelled rather than left to run to completion: the build
// cannot use their answers, and an uncancelled sibling holds a per-store slot
// and an upstream query for as long as the slowest store takes.
func TestReadScopedQoS_FailClosedChunkCancelsItsSiblings(t *testing.T) {
	var v topologyVectors
	for _, id := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc"} {
		v.PVCInfo = append(v.PVCInfo, claimSample("db", id, "pvc-"+id))
		v.VolumeLabels = append(v.VolumeLabels, volSample("trident_pvc_"+id, "aggr1"))
	}
	f := &blockingQoSFake{failVolume: "trident_pvc_bbbbbbbb", fallback: 5 * time.Second}

	done := make(chan struct{})
	close(done)
	var mu sync.Mutex
	start := time.Now()
	// A 21-byte budget puts each volume in a chunk of its own.
	err := readScopedQoS(t.Context(), t.Context(), f, time.Minute, time.Unix(1, 0).UTC(),
		Options{QoSScopeBatchBytes: 21}, promql.Selector{}, &v, &mu, true, "", false, done, done)

	require.Error(t, err)
	qe, ok := errors.AsType[*QueryError](err)
	require.True(t, ok, "the first chunk error is the one reported, got %v", err)
	assert.NotErrorIs(t, qe, context.Canceled, "a cancelled sibling's error must not displace the real failure")
	assert.Zero(t, f.timedOut.Load(), "no sibling may run to completion after the read already failed")
	assert.Less(t, time.Since(start), f.fallback, "the read returns as soon as the failure is known")
}
