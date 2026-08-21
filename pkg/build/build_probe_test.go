package build

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
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

	g, err := New(q, Options{}, nil, nil).Build(context.Background(), 5*time.Minute, probeTestEnd, promql.Selector{})

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

	g, err := New(q, Options{}, nil, nil).Build(context.Background(), 5*time.Minute, probeTestEnd, promql.Selector{})

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

	g, err := New(q, Options{}, nil, nil).Build(context.Background(), 5*time.Minute, probeTestEnd, promql.Selector{})

	require.Error(t, err)
	assert.Nil(t, g)
	assert.Equal(t, ReasonOutsideRetention, AsReason(err))
}

// TestBuild_FilteredEmptyResultSkipsRetentionClassification pins design D7:
// with any selector-level dimension active, "zero pods and zero nodes" means
// "nothing in scope", not a retention miss. The build returns an empty graph,
// and the up{} probe is never issued — asserted structurally, by registering
// NO up-probe expectation on the mock (an unexpected call fails the test).
func TestBuild_FilteredEmptyResultSkipsRetentionClassification(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name, _ string, _ time.Time) (model.Vector, error) {
			require.NotEqual(t, string(promql.QUpProbe), name,
				"a filtered build must not issue the retention probe")
			return model.Vector{}, nil
		}).
		Maybe()

	sel := promql.Selector{Namespace: []string{"shop"}}
	g, err := New(q, Options{}, nil, nil).Build(context.Background(), 5*time.Minute, probeTestEnd, sel)

	require.NoError(t, err, "an empty filtered result is a valid empty graph, not an error")
	require.NotNil(t, g)
	assert.Empty(t, g.NodesByID)
	assert.Empty(t, g.Edges)
}

// The unfiltered counterpart still classifies: healthy upstream + zero rows is
// outside_retention, exactly as before.
func TestBuild_UnfilteredEmptyResultStillClassifiesRetention(t *testing.T) {
	q := newEmptyTopologyQuerier(t, model.Vector{{Value: 1}}, nil)

	_, err := New(q, Options{}, nil, nil).Build(context.Background(), 5*time.Minute, probeTestEnd, promql.Selector{})

	require.Error(t, err)
	var be *Error
	require.ErrorAs(t, err, &be)
	assert.Equal(t, ReasonOutsideRetention, be.Reason)
}

// The filtered build passes its selector to every topology query and to none of
// the service-graph queries — the push-down contract, asserted on the exact
// query strings the build issues.
func TestBuild_SelectorReachesTopologyQueriesOnly(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name, query string, _ time.Time) (model.Vector, error) {
			mu.Lock()
			seen[name] = query
			mu.Unlock()
			return model.Vector{}, nil
		}).
		Maybe()

	sel := promql.Selector{
		AZ: []string{"zone-a"}, Env: []string{"prod"},
		Cluster: []string{"cluster-alpha"}, Namespace: []string{"shop"},
	}
	_, err := New(q, Options{}, nil, nil).Build(context.Background(), 5*time.Minute, probeTestEnd, sel)
	require.NoError(t, err)

	assert.Equal(t,
		`last_over_time(kube_pod_info{az="zone-a",env="prod",cluster="cluster-alpha",namespace="shop"}[5m])`,
		seen[string(promql.QPodInfo)], "namespaced KSM series carry all four dimensions")
	assert.Equal(t,
		`last_over_time(kube_node_info{az="zone-a",env="prod",cluster="cluster-alpha"}[5m])`,
		seen[string(promql.QNodeInfo)], "node series carry no namespace")
	assert.Equal(t,
		`last_over_time(volume_labels{az="zone-a",env="prod"}[5m])`,
		seen[string(promql.QVolumeLabels)], "Harvest carries az/env only")
	assert.Equal(t,
		`last_over_time(kubelet_volume_stats_used_bytes{az="zone-a",env="prod",cluster="cluster-alpha",namespace="shop"}[5m])`,
		seen[string(promql.QKubeletVolumeUsedBytes)], "kubelet is namespaced")
	assert.Equal(t,
		`rate(traces_service_graph_request_total{client!~"user|unknown",server!~"user"}[5m])`,
		seen[string(promql.QServiceGraphTotal)], "service-graph series are never narrowed")
	assert.NotContains(t, seen, string(promql.QUpProbe), "no retention probe on a filtered build")
}
