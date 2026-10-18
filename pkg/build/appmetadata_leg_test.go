package build

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

func TestReadTopology_ConfiguredAppMetadataIssuesOneLogicalQuery(t *testing.T) {
	end := time.Unix(1, 0).UTC()
	window := time.Minute
	src := defaultAppMetadata()
	sample := appMetaSample(map[string]string{
		"cluster": "c", "namespace": "shop", "pod": "web-1",
		"argocd_app": "shop", "owner": "team", "user": "alice",
	}, 1)

	t.Run("unset issues no query and leaves no tally", func(t *testing.T) {
		f := promqlfake.New(nil)
		tp, err := ReadTopology(t.Context(), f, window, end, Options{}, promql.Selector{})
		require.NoError(t, err)
		assert.Empty(t, f.QueriesFor(promql.QAppMetadata))
		assert.NotContains(t, tp.RawSeriesCount, string(promql.QAppMetadata))
		assert.NotContains(t, tp.RawSeriesCount, src.Metric)
	})

	t.Run("configured issues exactly one logical query", func(t *testing.T) {
		f := promqlfake.New(map[promql.Query]model.Vector{
			promql.QAppMetadata: {sample},
		})
		tp, err := ReadTopology(t.Context(), f, window, end, Options{AppMetadata: &src}, promql.Selector{})
		require.NoError(t, err)

		got := f.QueriesFor(promql.QAppMetadata)
		require.Len(t, got, 1)
		assert.Contains(t, got[0], "tlast_over_time("+src.Metric)
		assert.NotContains(t, got[0], "tlast_over_time("+string(promql.QAppMetadata))
		assert.Equal(t, 1, tp.RawSeriesCount[string(promql.QAppMetadata)])
		assert.NotContains(t, tp.RawSeriesCount, src.Metric, "the tally key is the logical query name")
		for _, issued := range f.Issued() {
			assert.NotEqual(t, src.Metric, issued.Name)
		}
	})

	t.Run("empty labels read their defaults", func(t *testing.T) {
		f := promqlfake.New(map[promql.Query]model.Vector{
			promql.QAppMetadata: {sample},
		})
		partial := promql.AppMetadataSource{Metric: src.Metric}
		tp, err := ReadTopology(t.Context(), f, window, end, Options{AppMetadata: &partial}, promql.Selector{})
		require.NoError(t, err)
		require.Len(t, f.QueriesFor(promql.QAppMetadata), 1)
		assert.Equal(t, 1, tp.RawSeriesCount[string(promql.QAppMetadata)])
		assert.Equal(t, map[graph.NamespaceRef]string{{Cluster: "c", Namespace: "shop"}: "team"}, tp.NamespaceOwners)
		assert.Empty(t, partial.App, "the caller's source is not mutated")
	})

	t.Run("query error does not fail the build and tallies zero", func(t *testing.T) {
		f := promqlfake.New(nil)
		f.Fail = func(name, _ string) error {
			if name == string(promql.QAppMetadata) {
				return errors.New("app metadata store down")
			}
			return nil
		}
		tp, err := ReadTopology(t.Context(), f, window, end, Options{AppMetadata: &src}, promql.Selector{})
		require.NoError(t, err)
		require.Len(t, f.QueriesFor(promql.QAppMetadata), 1)
		assert.Equal(t, 0, tp.RawSeriesCount[string(promql.QAppMetadata)], "a swallowed error is present-and-zero")
		assert.NotContains(t, tp.RawSeriesCount, src.Metric)
	})
}

func TestBuild_AppMetadataGroupIndexes(t *testing.T) {
	end := time.Unix(1, 0).UTC()
	window := time.Minute
	sel := promql.Selector{Namespace: []string{"shop"}}
	src := defaultAppMetadata()
	sample := appMetaSample(appMetaBase("web-1", "checkout", "team-a", "alice"), 1)

	t.Run("unset leaves both maps nil", func(t *testing.T) {
		g, err := New(promqlfake.New(nil), Options{}, nil, nil).Build(t.Context(), window, end, sel)
		require.NoError(t, err)
		assert.Nil(t, g.NamespaceOwners)
		assert.Nil(t, g.ApplicationDeployers)
	})

	t.Run("a read row is keyed by cluster identity", func(t *testing.T) {
		f := promqlfake.New(map[promql.Query]model.Vector{
			promql.QAppMetadata: {sample},
		})
		g, err := New(f, Options{AppMetadata: &src}, nil, nil).Build(t.Context(), window, end, sel)
		require.NoError(t, err)
		assert.Equal(t, map[graph.NamespaceRef]string{
			{Cluster: "us-dev-c1", Namespace: "shop"}: "team-a",
		}, g.NamespaceOwners)
		assert.Equal(t, map[graph.ApplicationRef]string{
			{Cluster: "us-dev-c1", Namespace: "shop", Application: "checkout"}: "alice",
		}, g.ApplicationDeployers)
	})

	t.Run("an empty read is an empty map", func(t *testing.T) {
		g, err := New(promqlfake.New(nil), Options{AppMetadata: &src}, nil, nil).Build(t.Context(), window, end, sel)
		require.NoError(t, err)
		assert.NotNil(t, g.NamespaceOwners)
		assert.Empty(t, g.NamespaceOwners)
		assert.NotNil(t, g.ApplicationDeployers)
		assert.Empty(t, g.ApplicationDeployers)
	})

	t.Run("a query error leaves both maps nil", func(t *testing.T) {
		f := promqlfake.New(map[promql.Query]model.Vector{
			promql.QAppMetadata: {sample},
		})
		f.Fail = func(name, _ string) error {
			if name == string(promql.QAppMetadata) {
				return errors.New("app metadata store down")
			}
			return nil
		}
		g, err := New(f, Options{AppMetadata: &src}, nil, nil).Build(t.Context(), window, end, sel)
		require.NoError(t, err)
		assert.Nil(t, g.NamespaceOwners)
		assert.Nil(t, g.ApplicationDeployers)
	})
}

func TestBuildStorage_ConfiguredAppMetadataSkipsUnscopedLeg(t *testing.T) {
	src := defaultAppMetadata()
	f := promqlfake.New(nil)
	f.Fail = func(name, _ string) error {
		if name == string(promql.QAppMetadata) {
			return errors.New("unscoped app_metadata must not be issued")
		}
		return nil
	}
	g, err := New(f, Options{AppMetadata: &src}, nil, nil).BuildStorage(
		t.Context(), time.Minute, time.Unix(1, 0).UTC(), storageSel, graph.StorageRoots{})
	require.NoError(t, err)
	assert.Empty(t, f.QueriesFor(promql.QAppMetadata))
	assert.Nil(t, g.NamespaceOwners, "the unscoped leg is not the storage read")
	assert.Nil(t, g.ApplicationDeployers)
}
