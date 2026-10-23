package kubegraph_test

import (
	"net/url"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

func k8sSample(pairs ...string) *model.Sample {
	m := model.Metric{"cluster": "c1", "az": "zone-a", "env": "prod"}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[model.LabelName(pairs[i])] = model.LabelValue(pairs[i+1])
	}
	return &model.Sample{Metric: m, Value: 1}
}

// k8sFixtures is shop/web-0 and shop/api-0 on worker-1, web-0 mounting
// shop/data, plus a Harvest row for that claim's volume.
func k8sFixtures() map[promql.Query]model.Vector {
	return map[promql.Query]model.Vector{
		promql.QPodInfo: {
			k8sSample("namespace", "shop", "pod", "web-0", "uid", "uid-web", "node", "worker-1"),
			k8sSample("namespace", "shop", "pod", "api-0", "uid", "uid-api", "node", "worker-1"),
		},
		promql.QNodeInfo:    {k8sSample("node", "worker-1")},
		promql.QPVCBindings: {k8sSample("namespace", "shop", "pod", "web-0", "persistentvolumeclaim", "data", "volume", "v")},
		promql.QPVCInfo:     {k8sSample("namespace", "shop", "persistentvolumeclaim", "data", "volumename", "pvc-1", "storageclass", "netapp-nas")},
		promql.QVolumeLabels: {{Metric: model.Metric{
			"volume": "trident_pvc_1", "cluster": "ontap", "node": "n1", "aggr": "aggr1", "svm": "svm0",
		}, Value: 1}},
	}
}

func k8sParams() url.Values {
	return url.Values{
		"start":   {"1700000000"},
		"end":     {"1700003600"},
		"cluster": {"c1"},
		"az":      {"zone-a"},
		"env":     {"prod"},
		"node":    {"worker-1"},
	}
}

func TestBuildK8sFromValues(t *testing.T) {
	q := promqlfake.New(k8sFixtures())
	eng := kubegraph.New(q, kubegraph.Options{APITimeout: 5 * time.Second})

	body, err := eng.BuildK8sFromValues(t.Context(), k8sParams())
	require.NoError(t, err)

	assert.Equal(t, "v1", body.APIVersion)
	assert.Equal(t, []string{"zone-a-prod-c1"}, body.Clusters)
	types := map[string]int{}
	for _, n := range body.Elements.Nodes {
		types[n.Data.Type]++
	}
	assert.Equal(t, 2, types["pod"], "every pod on the root node, mounting or not")
	assert.Equal(t, 1, types["node"])
	assert.Equal(t, 1, types["pvc"])
	for _, n := range body.Elements.Nodes {
		assert.NotContains(t, []string{"netapp-node", "netapp-aggr", "netapp-svm", "storage-cluster"}, n.Data.Type)
	}
	for _, e := range body.Elements.Edges {
		assert.Contains(t, []string{"pod-to-node", "pod-mounts-pvc"}, e.Data.Type)
	}
	for _, is := range q.Issued() {
		fam, _ := promql.FamilyOf(promql.Query(is.Name))
		assert.NotEqual(t, promql.FamilyHarvest, fam, "no Harvest query: %s", is.Name)
	}
}

// Spec: "Several values form a product". Two zones each running a cluster c1
// with a node worker-1: az=zone-a&az=zone-b selects both identities, each node
// is drawn under its own cluster group, and a third zone and another
// environment stay out.
func TestBuildK8sFromValues_ZonesFormAProduct(t *testing.T) {
	in := func(az, env string, pairs ...string) *model.Sample {
		s := k8sSample(pairs...)
		s.Metric["az"], s.Metric["env"] = model.LabelValue(az), model.LabelValue(env)
		return s
	}
	zones := [][2]string{{"zone-a", "prod"}, {"zone-b", "prod"}, {"zone-c", "prod"}, {"zone-a", "dev"}}
	podInfo := make(model.Vector, 0, len(zones))
	nodeInfo := make(model.Vector, 0, len(zones))
	for _, z := range zones {
		podInfo = append(podInfo, in(z[0], z[1], "namespace", "shop", "pod", "web-0", "uid", "uid-"+z[0]+"-"+z[1], "node", "worker-1"))
		nodeInfo = append(nodeInfo, in(z[0], z[1], "node", "worker-1"))
	}
	eng := kubegraph.New(promqlfake.New(map[promql.Query]model.Vector{
		promql.QPodInfo:  podInfo,
		promql.QNodeInfo: nodeInfo,
	}), kubegraph.Options{})
	vals := k8sParams()
	vals["az"] = []string{"zone-b", "zone-a"}

	body, err := eng.BuildK8sFromValues(t.Context(), vals)
	require.NoError(t, err)

	want := []string{"zone-a-prod-c1", "zone-b-prod-c1"}
	assert.Equal(t, want, body.Clusters)
	parents := map[string]string{}
	for _, n := range body.Elements.Nodes {
		if n.Data.Type == "node" || n.Data.Type == "pod" {
			parents[n.Data.ID] = n.Data.Parent
		}
	}
	for _, ident := range want {
		assert.Equal(t, "cluster/"+ident, parents[graph.K8sNodeID(ident, "worker-1")], "each node sits under its own cluster group")
		assert.Contains(t, parents, graph.PodID(ident, "uid-"+ident[:6]+"-prod"))
	}
	assert.Len(t, parents, 4, "zone-c and env dev are not selected")
}

// A request lacking a root is a parse error before any upstream query.
func TestBuildK8sFromValues_MissingRootIssuesNoQuery(t *testing.T) {
	q := promqlfake.New(k8sFixtures())
	eng := kubegraph.New(q, kubegraph.Options{})
	vals := k8sParams()
	vals.Del("node")

	_, err := eng.BuildK8sFromValues(t.Context(), vals)
	var pe *kubegraph.ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "missing_root", pe.Reason)
	assert.Empty(t, q.Issued())
}

// `clusters[]` carries the composed identity, which is not a cluster name: sent
// back as `cluster=` it selects nothing and the body is an empty 200.
func TestBuildK8sFromValues_IdentityAsClusterIsEmpty(t *testing.T) {
	eng := kubegraph.New(promqlfake.New(k8sFixtures()), kubegraph.Options{})
	vals := k8sParams()
	vals.Set("cluster", "zone-a-prod-c1")

	body, err := eng.BuildK8sFromValues(t.Context(), vals)
	require.NoError(t, err)
	assert.Empty(t, body.Clusters)
	assert.Empty(t, body.Elements.Nodes)
	assert.Empty(t, body.Elements.Edges)
}

func TestBuildK8sFromValues_AgreesWithComposedPipeline(t *testing.T) {
	eng := kubegraph.New(promqlfake.New(k8sFixtures()), kubegraph.Options{})
	vals := k8sParams()

	facade, err := eng.BuildK8sFromValues(t.Context(), vals)
	require.NoError(t, err)

	req, err := kubegraph.ParseK8sValues(vals)
	require.NoError(t, err)
	start, end := kubegraph.AlignWindow(req.Start, req.End, kubegraph.DefaultEndAlign)
	g, err := eng.BuildK8s(t.Context(), end.Sub(start), end, req.Selector, req.Scope.Roots)
	require.NoError(t, err)
	assert.Equal(t, cytoscape.Serialise(g, graph.ProjectK8s(g, req.Scope)), facade)
}

func TestBuildK8s_RejectsAStorageRoot(t *testing.T) {
	q := promqlfake.New(k8sFixtures())
	eng := kubegraph.New(q, kubegraph.Options{})
	_, err := eng.BuildK8s(t.Context(), time.Minute, time.Unix(1700003600, 0), promql.Selector{AZ: []string{"zone-a"}},
		graph.StorageRoots{Kind: graph.StorageRootAggr, Names: []string{"aggr1"}})
	require.Error(t, err)
	assert.Empty(t, q.Issued())
}
