package build

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/marz32one/kube-state-graph/pkg/promql"
	promqlmocks "github.com/marz32one/kube-state-graph/pkg/promql/mocks"
)

// probeTestEnd is an arbitrary fixed build end time so the tests are
// independent of wall-clock time.
var probeTestEnd = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// newEmptyTopologyQuerier returns a MockQuerier whose up probe behaves as
// configured by upVec/upErr while every other query (topology +
// service-graph) returns an empty vector — the "zero pods, zero nodes" build
// that triggers the outside-retention classification path.
//
// The up-probe expectation MUST be registered before the catch-all: testify
// matches expectations in registration order, so the specific
// promql.QUpProbe name has to win over mock.Anything.
func newEmptyTopologyQuerier(t *testing.T, upVec model.Vector, upErr error) *promqlmocks.MockQuerier {
	t.Helper()
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, string(promql.QUpProbe), mock.Anything, mock.Anything).
		Return(upVec, upErr)
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Vector{}, nil)
	return q
}

// TestBuild_UpProbeError_WarnsAndProceeds guards the fix for the silent
// up-probe failure: when topology is empty and the up probe errors (upstream
// flake / timeout), the build must still succeed with an empty graph (control
// flow unchanged — classification as outside-retention is simply skipped) AND
// leave a server-side WARN, instead of degrading to a 200 empty body with
// zero signal.
func TestBuild_UpProbeError_WarnsAndProceeds(t *testing.T) {
	buf := captureLogs(t)
	q := newEmptyTopologyQuerier(t, nil, errors.New("probe exploded"))

	g, err := New(q, Options{}, nil, nil).Build(context.Background(), 5*time.Minute, probeTestEnd)

	require.NoError(t, err, "a failed probe must not fail the build")
	require.NotNil(t, g)
	assert.Empty(t, g.Edges)
	assert.Empty(t, g.NodesByID)

	out := buf.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "up probe failed; outside-retention classification skipped")
	assert.Contains(t, out, "probe exploded")
}

// TestBuild_UpProbeEmpty_NoWarnProceeds: an empty up{} vector with no error is
// not a probe failure — the build proceeds to an empty graph exactly as
// before, with no probe-failure warn line.
func TestBuild_UpProbeEmpty_NoWarnProceeds(t *testing.T) {
	buf := captureLogs(t)
	q := newEmptyTopologyQuerier(t, model.Vector{}, nil)

	g, err := New(q, Options{}, nil, nil).Build(context.Background(), 5*time.Minute, probeTestEnd)

	require.NoError(t, err)
	require.NotNil(t, g)
	assert.NotContains(t, buf.String(), "up probe failed")
}

// TestBuild_UpProbeHealthy_OutsideRetentionUnchanged: regression guard that
// the warn-on-error fix did not alter the existing control flow — a healthy
// upstream (non-empty up{}) with zero topology still classifies as
// outside_retention.
func TestBuild_UpProbeHealthy_OutsideRetentionUnchanged(t *testing.T) {
	captureLogs(t) // keep the outside_retention warn out of test output
	up := sampleVec(model.Sample{
		Metric: model.Metric{"__name__": "up", "job": "ksm"},
		Value:  1,
	})
	q := newEmptyTopologyQuerier(t, up, nil)

	g, err := New(q, Options{}, nil, nil).Build(context.Background(), 5*time.Minute, probeTestEnd)

	require.Error(t, err)
	assert.Nil(t, g)
	assert.Equal(t, ReasonOutsideRetention, AsReason(err))
}
