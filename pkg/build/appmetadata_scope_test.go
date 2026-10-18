package build

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// appMetaUnserved reports every family served except appmeta. A storage build
// that holds one skips the metric the way a table with no appmeta backend does.
// The skip has to be visible on the querier the build calls: a Router fan-out
// already returns an empty vector for an unserved optional family, so a client
// behind it cannot tell "the build skipped" from "the fan-out short-circuited".
type appMetaUnserved struct{ promql.Querier }

func (appMetaUnserved) Serves(f promql.Family) bool { return f != promql.FamilyAppMeta }

func appMetaRow(ns, pod, app, owner, user string) *model.Sample {
	return appMetaSample(map[string]string{
		"az": "zone-a", "env": "prod", "cluster": "c1",
		"namespace": ns, "pod": pod,
		"argocd_app": app, "owner": owner, "user": user,
	}, 100)
}

// appMetaClaimEstate is the claim-seed estate plus an unmounted root claim in
// archive and metric rows for the loaded shop pod and for a shop pod the build
// does not load. The ghost row is how a test sees that the namespace read
// returned more than the loaded pods.
func appMetaClaimEstate() map[promql.Query]model.Vector {
	fx := claimSeedEstate()
	info := slices.Clone(fx[promql.QPVCInfo])
	fx[promql.QPVCInfo] = append(info, claimInfoRow("archive", "old", "pvc-old"))
	fx[promql.QAppMetadata] = model.Vector{
		appMetaRow("shop", "orders-0", "shop-v2", "team-a", "alice"),
		appMetaRow("shop", "ghost", "ghost-app", "team-a", "ghost-user"),
	}
	return fx
}

func appMetaClaimScope(t *testing.T) graph.StorageScope {
	t.Helper()
	return vlrScope(t, graph.StorageRootPVC, []string{"shop/orders-data", "archive/old"})
}

func TestBuildStorage_AppMetadataReadsPodAndClaimNamespaces(t *testing.T) {
	src := defaultAppMetadata()
	scope := appMetaClaimScope(t)
	q := promqlfake.New(appMetaClaimEstate())
	g, err := New(q, Options{AppMetadata: &src}, nil, nil).BuildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
	require.NoError(t, err)

	got := q.QueriesFor(promql.QAppMetadata)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "tlast_over_time("+src.Metric)
	assert.NotContains(t, got[0], "tlast_over_time("+string(promql.QAppMetadata))
	assert.NotContains(t, got[0], "pod=")
	assert.Contains(t, got[0], `cluster="c1"`, "the namespaces are paired with the cluster that loaded them")
	assert.Equal(t, [][]string{{"archive", "shop"}}, q.ScopeValues(promql.QAppMetadata, src.Namespace))

	const cluster = "zone-a-prod-c1"
	pod := g.NodesByID[cluster+"/uid-o0"]
	require.NotNil(t, pod)
	assert.Equal(t, "shop-v2", pod.Application())
	require.NotNil(t, pod.Owner())
	assert.Equal(t, graph.Owner{Kind: "StatefulSet", Name: "orders"}, *pod.Owner())

	assert.Equal(t, "team-a", g.NamespaceOwners[graph.NamespaceRef{Cluster: cluster, Namespace: "shop"}])
	assert.Equal(t, "alice", g.ApplicationDeployers[graph.ApplicationRef{Cluster: cluster, Namespace: "shop", Application: "shop-v2"}])
	assert.Equal(t, "ghost-user", g.ApplicationDeployers[graph.ApplicationRef{Cluster: cluster, Namespace: "shop", Application: "ghost-app"}])
	for _, n := range g.NodesByID {
		assert.NotEqual(t, "ghost-app", n.Application())
	}

	body := cytoscape.Serialise(g, graph.ProjectStorage(g, scope))
	byID := make(map[string]cytoscape.NodeData, len(body.Elements.Nodes))
	for _, n := range body.Elements.Nodes {
		byID[n.Data.ID] = n.Data
		if n.Data.Type == "pod" {
			assert.Empty(t, n.Data.NamespaceOwner)
			assert.Empty(t, n.Data.DeployedBy)
		}
	}
	assert.Equal(t, "team-a", byID[cluster+"/namespace/shop"].NamespaceOwner)
	assert.Equal(t, "alice", byID[cluster+"/namespace/shop/application/shop-v2"].DeployedBy)
}

func TestBuildStorage_AppMetadataNamespaceScopeChunks(t *testing.T) {
	src := defaultAppMetadata()
	q := promqlfake.New(appMetaClaimEstate())
	_, err := New(q, Options{AppMetadata: &src, QoSScopeBatchBytes: 1}, nil, nil).BuildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel, appMetaClaimScope(t).Roots)
	require.NoError(t, err)

	got := q.ScopeValues(promql.QAppMetadata, src.Namespace)
	require.Len(t, got, 2, "a data-derived namespace scope is chunked, never rejected")
	flat := slices.Concat(got...)
	slices.Sort(flat)
	assert.Equal(t, []string{"archive", "shop"}, flat)
	for _, query := range q.QueriesFor(promql.QAppMetadata) {
		assert.NotContains(t, query, "pod=")
	}
}

func TestBuildStorage_UnsetAppMetadataIssuesNoRead(t *testing.T) {
	q := promqlfake.New(appMetaClaimEstate())
	g, err := New(q, Options{}, nil, nil).BuildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel, appMetaClaimScope(t).Roots)
	require.NoError(t, err)
	assert.Empty(t, q.QueriesFor(promql.QAppMetadata))
	assert.Equal(t, "shop-orders", g.NodesByID["zone-a-prod-c1/uid-o0"].Application())
	assert.Nil(t, g.NamespaceOwners)
	assert.Nil(t, g.ApplicationDeployers)
}

func TestBuildStorage_UnservedAppMetadataIssuesNoRead(t *testing.T) {
	src := defaultAppMetadata()
	inner := promqlfake.New(appMetaClaimEstate())
	g, err := New(appMetaUnserved{inner}, Options{AppMetadata: &src}, nil, nil).BuildStorage(
		t.Context(), time.Minute, vlrEnd, vlrSel, appMetaClaimScope(t).Roots)
	require.NoError(t, err)
	assert.Empty(t, inner.QueriesFor(promql.QAppMetadata))
	assert.Equal(t, "shop-orders", g.NodesByID["zone-a-prod-c1/uid-o0"].Application())
	assert.Nil(t, g.NamespaceOwners)
	assert.Nil(t, g.ApplicationDeployers)
}

func TestBuildStorage_AppMetadataChunkErrorFailsClosed(t *testing.T) {
	src := defaultAppMetadata()
	q := promqlfake.New(appMetaClaimEstate())
	q.Fail = func(name, _ string) error {
		if name == string(promql.QAppMetadata) {
			return assert.AnError
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := New(q, Options{AppMetadata: &src}, nil, nil).BuildStorage(
		ctx, time.Minute, vlrEnd, vlrSel, appMetaClaimScope(t).Roots)
	require.Error(t, err)
	require.NoError(t, ctx.Err(), "the build returned on the chunk error instead of running into the deadline")
	be, ok := errors.AsType[*Error](err)
	require.True(t, ok)
	assert.Equal(t, ReasonUpstream, be.Reason)
	assert.Equal(t, string(promql.QAppMetadata), be.Query)
}

func appMetaScopeSample(cluster, ns string) *model.Sample {
	m := model.Metric{"namespace": model.LabelValue(ns)}
	if cluster != "" {
		m["cluster"] = model.LabelValue(cluster)
	}
	return &model.Sample{Metric: m, Value: 1}
}

func TestAppMetadataScopes_PairsNamespacesWithTheirCluster(t *testing.T) {
	pods := model.Vector{
		appMetaScopeSample("c2", "pay"),
		appMetaScopeSample("c1", "shop"),
		appMetaScopeSample("c1", "shop"),
		appMetaScopeSample("", "legacy"),
		appMetaScopeSample("c1", ""),
	}
	claims := model.Vector{appMetaScopeSample("c1", "archive")}
	assert.Equal(t, []appMetadataClusterScope{
		{cluster: "c1", namespaces: []string{"archive", "shop"}},
		{cluster: "c2", namespaces: []string{"pay"}},
		{cluster: promql.ClusterUnknownValue, namespaces: []string{"legacy"}},
	}, appMetadataScopes(pods, claims))
	assert.Empty(t, appMetadataScopes(nil, nil))
}

// A namespace loaded in one cluster is never read in another: two clusters
// issue two restrictions, each carrying only its own namespaces, and the
// rows of both land in v.AppMetadata.
func TestReadScopedAppMetadata_OneRestrictionPerCluster(t *testing.T) {
	src := defaultAppMetadata()
	row := func(cluster, ns, pod string) *model.Sample {
		return appMetaSample(map[string]string{
			"az": "zone-a", "env": "prod", "cluster": cluster,
			"namespace": ns, "pod": pod, "argocd_app": ns,
		}, 100)
	}
	q := promqlfake.New(map[promql.Query]model.Vector{
		promql.QAppMetadata: {row("c1", "shop", "web-1"), row("c2", "pay", "api-1"), row("c2", "shop", "ghost")},
	})
	v := &topologyVectors{
		Pod: model.Vector{appMetaScopeSample("c1", "shop"), appMetaScopeSample("c2", "pay")},
	}
	done := make(chan struct{})
	close(done)
	var mu sync.Mutex
	err := readScopedAppMetadata(t.Context(), q, time.Minute, vlrEnd, Options{AppMetadata: &src}, vlrSel, v, &mu, done, done)
	require.NoError(t, err)

	got := q.QueriesFor(promql.QAppMetadata)
	require.Len(t, got, 2)
	slices.Sort(got)
	assert.Contains(t, got[0], `cluster="c1",namespace="shop"`)
	assert.NotContains(t, got[0], "pay")
	assert.Contains(t, got[1], `cluster="c2",namespace="pay"`)
	assert.NotContains(t, got[1], "shop")
	require.Len(t, v.AppMetadata, 2, "each restriction's rows are kept; c2/shop is never read")
	for _, r := range v.AppMetadata {
		assert.NotEqual(t, model.LabelValue("ghost"), r.Metric["pod"])
	}
	assert.True(t, v.ScopeIssued[promql.QAppMetadata])
}
