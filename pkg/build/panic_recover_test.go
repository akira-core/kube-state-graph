package build

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/promql"
	promqlmocks "github.com/marz32one/kube-state-graph/pkg/promql/mocks"
)

// A panic on one of ReadTopology's errgroup goroutines must surface as a
// build error, not kill the process: x/sync's errgroup (post-#53757-revert)
// does not propagate goroutine panics to Wait, and the HTTP recovery
// middleware only covers the handler goroutine — without the in-closure
// recover this test would crash the whole test binary.
func TestReadTopology_QueryPanicBecomesError(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t)
	// Specific expectation first: testify matches in registration order.
	q.EXPECT().
		Instant(mock.Anything, string(promql.QPodInfo), mock.Anything, mock.Anything).
		Run(func(context.Context, string, string, time.Time) { panic("decoder exploded") }).
		Return(nil, nil).
		Maybe()
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Vector{}, nil).
		Maybe()

	_, err := ReadTopology(context.Background(), q, promql.Renderer{}, 5*time.Minute, probeTestEnd)

	require.Error(t, err, "a goroutine panic must convert to a build error")
	assert.Contains(t, err.Error(), "panic in kube_pod_info query")
	assert.Contains(t, err.Error(), "decoder exploded")
}
