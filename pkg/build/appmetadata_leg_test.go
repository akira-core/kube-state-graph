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

	t.Run("unserved issues no query and leaves no tally", func(t *testing.T) {
		inner := promqlfake.New(map[promql.Query]model.Vector{
			promql.QAppMetadata: {sample},
		})
		tp, err := ReadTopology(t.Context(), appMetaUnserved{inner}, window, end, Options{AppMetadata: &src}, promql.Selector{})
		require.NoError(t, err)
		assert.Empty(t, inner.QueriesFor(promql.QAppMetadata))
		assert.NotContains(t, tp.RawSeriesCount, string(promql.QAppMetadata))
		assert.Nil(t, tp.NamespaceMetadata)
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
		assert.Equal(t, map[graph.NamespaceRef]graph.NamespaceMeta{
			{Cluster: "c", Namespace: "shop"}: {Owner: "team"},
		}, tp.NamespaceMetadata)
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
		assert.Nil(t, g.NamespaceMetadata)
		assert.Nil(t, g.ApplicationMetadata)
	})

	t.Run("a read row is keyed by cluster identity", func(t *testing.T) {
		f := promqlfake.New(map[promql.Query]model.Vector{
			promql.QAppMetadata: {sample},
		})
		g, err := New(f, Options{AppMetadata: &src}, nil, nil).Build(t.Context(), window, end, sel)
		require.NoError(t, err)
		assert.Equal(t, map[graph.NamespaceRef]graph.NamespaceMeta{
			{Cluster: "us-dev-c1", Namespace: "shop"}: {Owner: "team-a"},
		}, g.NamespaceMetadata)
		assert.Equal(t, map[graph.ApplicationRef]graph.ApplicationMeta{
			{Cluster: "us-dev-c1", Namespace: "shop", Application: "checkout"}: {DeployedBy: "alice"},
		}, g.ApplicationMetadata)
	})

	t.Run("an empty read is an empty map", func(t *testing.T) {
		g, err := New(promqlfake.New(nil), Options{AppMetadata: &src}, nil, nil).Build(t.Context(), window, end, sel)
		require.NoError(t, err)
		assert.NotNil(t, g.NamespaceMetadata)
		assert.Empty(t, g.NamespaceMetadata)
		assert.NotNil(t, g.ApplicationMetadata)
		assert.Empty(t, g.ApplicationMetadata)
	})

	t.Run("a query error leaves both maps nil", func(t *testing.T) {
		labels := appMetaBase("web-1", "checkout", "team-a", "alice")
		labels["deploy_time"] = "2025/10/08 13:14:15"
		labels["namespace_group"] = "commerce"
		labels["namespace_group_owner"] = "bob"
		labels["namespace_alias"] = "Shop"
		f := promqlfake.New(map[promql.Query]model.Vector{
			promql.QAppMetadata: {appMetaSample(labels, 1)},
		})
		f.Fail = func(name, _ string) error {
			if name == string(promql.QAppMetadata) {
				return errors.New("app metadata store down")
			}
			return nil
		}
		var g *graph.Graph
		recs := captureDebugRecords(t, func() {
			var err error
			g, err = New(f, Options{AppMetadata: &src}, nil, nil).Build(t.Context(), window, end, sel)
			require.NoError(t, err)
		})
		assert.Nil(t, g.NamespaceMetadata)
		assert.Nil(t, g.ApplicationMetadata)
		logged := false
		for _, r := range recs {
			if r["msg"] == "optional topology query failed; continuing with empty vector" && r["query"] == string(promql.QAppMetadata) {
				logged = true
			}
		}
		assert.True(t, logged, "the degrade is logged naming app_metadata")
	})
}

// Spec: "A row without zone labels is ignored under a zone matcher". The
// pod's kube-state-metrics series carry az / env; the metric row does not. A
// zone filter excludes the row upstream and the controller chain resolves the
// pod. Without the filter the cluster ladder adopts the row.
func TestBuild_AppMetadataRowWithoutZoneLabels(t *testing.T) {
	end := time.Unix(1, 0).UTC()
	src := defaultAppMetadata()
	zoned := func(kv ...string) *model.Sample {
		m := model.Metric{"az": "us", "env": "dev", "cluster": "c1", "namespace": "shop"}
		for i := 0; i < len(kv); i += 2 {
			m[model.LabelName(kv[i])] = model.LabelValue(kv[i+1])
		}
		return &model.Sample{Metric: m, Value: 1}
	}
	fixtures := map[promql.Query]model.Vector{
		promql.QPodInfo:  {zoned("pod", "web-1", "uid", "u1", "node", "w0")},
		promql.QNodeInfo: {zoned("node", "w0")},
		promql.QPodOwner: {zoned("pod", "web-1", "owner_kind", "Deployment", "owner_name", "web", "owner_is_controller", "true")},
		promql.QDeploymentAnnotations: {zoned("deployment", "web",
			"annotation_argocd_argoproj_io_tracking_id", "chain-app:apps/Deployment:shop/web")},
		promql.QAppMetadata: {appMetaSample(map[string]string{
			"cluster": "c1", "namespace": "shop", "pod": "web-1", "argocd_app": "metric-app",
		}, 1)},
	}
	appOf := func(t *testing.T, sel promql.Selector) string {
		t.Helper()
		g, err := New(promqlfake.New(fixtures), Options{AppMetadata: &src}, nil, nil).Build(t.Context(), time.Minute, end, sel)
		require.NoError(t, err)
		pod := g.NodesByID["us-dev-c1/u1"]
		require.NotNil(t, pod)
		return pod.Application()
	}
	assert.Equal(t, "chain-app", appOf(t, promql.Selector{AZ: []string{"us"}}), "the zone matcher excludes the row")
	assert.Equal(t, "metric-app", appOf(t, promql.Selector{}), "an unfiltered build adopts the row")
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
	assert.Nil(t, g.NamespaceMetadata, "the unscoped leg is not the storage read")
	assert.Nil(t, g.ApplicationMetadata)
}
