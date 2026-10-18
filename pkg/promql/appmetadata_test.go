package promql

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testAppMetadataSource() AppMetadataSource {
	return AppMetadataSource{
		Metric:    "argocd_app_pod_info",
		App:       "argocd_app",
		User:      "user",
		NSOwner:   "owner",
		Namespace: "k8s_namespace",
		Pod:       "pod",
	}
}

func TestRenderAppMetadata(t *testing.T) {
	t.Parallel()
	src := testAppMetadataSource()
	window := 5 * time.Minute

	t.Run("configured metric name, no request matcher", func(t *testing.T) {
		t.Parallel()
		got := RenderAppMetadata(src, window, LabelKeys{}, Selector{})
		assert.Equal(t, `tlast_over_time(argocd_app_pod_info[5m])`, got)
		assert.NotContains(t, got, "app_metadata")
		assert.NotContains(t, got, "{")
		assert.Zero(t, AppMetadataMatcherCost(src, LabelKeys{}, Selector{}))
	})

	t.Run("namespace filter uses the configured label", func(t *testing.T) {
		t.Parallel()
		sel := Selector{Namespace: []string{"shop"}}
		got := RenderAppMetadata(src, window, LabelKeys{}, sel)
		assert.Equal(t, `tlast_over_time(argocd_app_pod_info{k8s_namespace="shop"}[5m])`, got)
		assert.Equal(t, len(`k8s_namespace="shop"`), AppMetadataMatcherCost(src, LabelKeys{}, sel))
	})

	t.Run("request matchers keep az env cluster namespace order", func(t *testing.T) {
		t.Parallel()
		keys := LabelKeys{AZ: "zone", Env: "environment"}
		sel := Selector{
			AZ:        []string{"us"},
			Env:       []string{"dev"},
			Cluster:   []string{"c1"},
			Namespace: []string{"shop"},
		}
		got := RenderAppMetadata(src, window, keys, sel)
		const fragment = `zone="us",environment="dev",cluster="c1",k8s_namespace="shop"`
		assert.Equal(t, `tlast_over_time(argocd_app_pod_info{`+fragment+`}[5m])`, got)
		assert.Equal(t, len(fragment), AppMetadataMatcherCost(src, keys, sel))
	})

	t.Run("cluster unknown keeps the absent alternative", func(t *testing.T) {
		t.Parallel()
		sel := Selector{Cluster: []string{"unknown"}}
		got := RenderAppMetadata(src, window, LabelKeys{}, sel)
		assert.Equal(t, `tlast_over_time(argocd_app_pod_info{cluster=~"unknown|"}[5m])`, got)
		assert.Equal(t, len(`cluster=~"unknown|"`), AppMetadataMatcherCost(src, LabelKeys{}, sel))
	})
}

func TestRenderAppMetadataByNamespace(t *testing.T) {
	t.Parallel()
	src := testAppMetadataSource()
	window := 5 * time.Minute

	t.Run("cluster restriction precedes the sorted, de-duplicated, escaped alternation", func(t *testing.T) {
		t.Parallel()
		got, ok := RenderAppMetadataByNamespace(src, window, LabelKeys{}, Selector{}, "c1",
			[]string{"shop.b", "", "pay", "shop.b", `a"b`})
		require.True(t, ok)
		assert.Equal(t,
			`tlast_over_time(argocd_app_pod_info{cluster="c1",k8s_namespace=~"a\"b|pay|shop\\.b"}[5m])`,
			got)
		assert.Equal(t, len(`cluster="c1"`), AppMetadataClusterMatcherCost(src, LabelKeys{}, Selector{}, "c1"))
	})

	t.Run("unknown cluster also matches an absent label", func(t *testing.T) {
		t.Parallel()
		got, ok := RenderAppMetadataByNamespace(src, window, LabelKeys{}, Selector{}, ClusterUnknownValue, []string{"shop"})
		require.True(t, ok)
		assert.Equal(t, `tlast_over_time(argocd_app_pod_info{cluster=~"unknown|",k8s_namespace="shop"}[5m])`, got)
	})

	t.Run("request matchers precede the restrictions", func(t *testing.T) {
		t.Parallel()
		sel := Selector{Cluster: []string{"c1"}, Namespace: []string{"shop"}}
		got, ok := RenderAppMetadataByNamespace(src, window, LabelKeys{}, sel, "c1", []string{"pay", "shop"})
		require.True(t, ok)
		assert.Equal(t,
			`tlast_over_time(argocd_app_pod_info{cluster="c1",k8s_namespace="shop",cluster="c1",k8s_namespace=~"pay|shop"}[5m])`,
			got)
		assert.Equal(t, len(`cluster="c1",k8s_namespace="shop",cluster="c1"`),
			AppMetadataClusterMatcherCost(src, LabelKeys{}, sel, "c1"),
			"the chunk cost is the fixed fragment, not the namespace restriction")
	})

	t.Run("empty scope renders nothing", func(t *testing.T) {
		t.Parallel()
		_, ok := RenderAppMetadataByNamespace(src, window, LabelKeys{}, Selector{}, "c1", nil)
		assert.False(t, ok)
		_, ok = RenderAppMetadataByNamespace(src, window, LabelKeys{}, Selector{}, "c1", []string{"", ""})
		assert.False(t, ok)
		_, ok = RenderAppMetadataByNamespace(src, window, LabelKeys{}, Selector{}, "", []string{"shop"})
		assert.False(t, ok)
	})
}

func TestRenderAppMetadataByApp(t *testing.T) {
	t.Parallel()
	src := testAppMetadataSource()
	window := 5 * time.Minute

	t.Run("alternation is sorted, de-duplicated and escaped", func(t *testing.T) {
		t.Parallel()
		got, ok := RenderAppMetadataByApp(src, window, LabelKeys{}, Selector{},
			[]string{"shop.b", "checkout", "", "checkout"})
		require.True(t, ok)
		assert.Equal(t,
			`tlast_over_time(argocd_app_pod_info{argocd_app=~"checkout|shop\\.b"}[5m])`,
			got)
	})

	t.Run("request matchers precede the app restriction", func(t *testing.T) {
		t.Parallel()
		keys := LabelKeys{AZ: "zone"}
		sel := Selector{AZ: []string{"us"}, Namespace: []string{"shop"}}
		got, ok := RenderAppMetadataByApp(src, window, keys, sel, []string{"checkout"})
		require.True(t, ok)
		const request = `zone="us",k8s_namespace="shop"`
		assert.Equal(t,
			`tlast_over_time(argocd_app_pod_info{`+request+`,argocd_app="checkout"}[5m])`,
			got)
		assert.Equal(t, len(request), AppMetadataMatcherCost(src, keys, sel))
	})

	t.Run("empty values render nothing", func(t *testing.T) {
		t.Parallel()
		_, ok := RenderAppMetadataByApp(src, window, LabelKeys{}, Selector{}, nil)
		assert.False(t, ok)
		_, ok = RenderAppMetadataByApp(src, window, LabelKeys{}, Selector{}, []string{""})
		assert.False(t, ok)
	})
}

func TestAppMetadataSource_OrDefault(t *testing.T) {
	t.Parallel()
	got := AppMetadataSource{Metric: "m", User: "deployer"}.OrDefault()
	assert.Equal(t, AppMetadataSource{
		Metric:    "m",
		App:       DefaultAppMetadataAppLabel,
		User:      "deployer",
		NSOwner:   DefaultAppMetadataNSOwnerLabel,
		Namespace: DefaultAppMetadataNamespaceLabel,
		Pod:       DefaultAppMetadataPodLabel,
	}, got)
	assert.Empty(t, AppMetadataSource{}.OrDefault().Metric, "the metric name is never defaulted")
}

func TestAppMetadataSource_Validate(t *testing.T) {
	t.Parallel()
	require.NoError(t, AppMetadataSource{Metric: "argocd_app_pod_info"}.Validate(LabelKeys{}),
		"empty labels validate as their defaults")

	tests := map[string]struct {
		src     AppMetadataSource
		keys    LabelKeys
		wantSub []string
	}{
		"empty metric":      {AppMetadataSource{}, LabelKeys{}, []string{"metric"}},
		"illegal metric":    {AppMetadataSource{Metric: "a-b"}, LabelKeys{}, []string{`"a-b"`}},
		"illegal label":     {AppMetadataSource{Metric: "m", App: `x",y="`}, LabelKeys{}, []string{`"app"`}},
		"colon in label":    {AppMetadataSource{Metric: "m", Pod: "a:b"}, LabelKeys{}, []string{`"pod"`}},
		"cluster label":     {AppMetadataSource{Metric: "m", User: "cluster"}, LabelKeys{}, []string{`"user"`, `"cluster"`}},
		"default az key":    {AppMetadataSource{Metric: "m", NSOwner: "az"}, LabelKeys{}, []string{`"ns_owner"`, `"az"`}},
		"configured az key": {AppMetadataSource{Metric: "m", NSOwner: "zone"}, LabelKeys{AZ: "zone"}, []string{`"ns_owner"`, `"zone"`}},
		"duplicate labels":  {AppMetadataSource{Metric: "m", App: "x", User: "x"}, LabelKeys{}, []string{`"x"`, `"app"`, `"user"`}},
		"default collision": {AppMetadataSource{Metric: "m", App: "pod"}, LabelKeys{}, []string{`"pod"`, `"app"`}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tc.src.Validate(tc.keys)
			require.Error(t, err)
			for _, sub := range tc.wantSub {
				assert.Contains(t, err.Error(), sub)
			}
		})
	}
}
