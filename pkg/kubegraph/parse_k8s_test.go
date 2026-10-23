package kubegraph_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
)

func k8sBase() url.Values {
	return url.Values{
		"start":   {"1700000000"},
		"end":     {"1700003600"},
		"cluster": {"c1"},
		"az":      {"zone-a"},
		"env":     {"prod"},
	}
}

// with returns base plus the given parameters, replacing any it already holds.
func with(base url.Values, kv map[string][]string) url.Values {
	out := url.Values{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range kv {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

func TestParseK8sValues_Errors(t *testing.T) {
	cases := []struct {
		name       string
		values     url.Values
		wantReason string
		wantMsg    string
	}{
		{"missing start", with(k8sBase(), map[string][]string{"start": nil, "node": {"w"}}), "missing_start", ""},
		{"end not after start", with(k8sBase(), map[string][]string{"start": {"1700003600"}, "end": {"1700000000"}, "node": {"w"}}), "invalid_range", ""},
		{"missing cluster", with(k8sBase(), map[string][]string{"cluster": nil, "node": {"w"}}), "missing_cluster", ""},
		{"empty cluster", with(k8sBase(), map[string][]string{"cluster": {""}, "node": {"w"}}), "missing_cluster", ""},
		{"missing az", with(k8sBase(), map[string][]string{"az": nil, "node": {"w"}}), "missing_az", ""},
		{"empty env", with(k8sBase(), map[string][]string{"env": {""}, "node": {"w"}}), "missing_env", ""},
		{"cluster is checked before az and env", with(k8sBase(), map[string][]string{"cluster": nil, "az": nil, "env": nil, "node": {"w"}}), "missing_cluster", ""},
		{"az is checked before env", with(k8sBase(), map[string][]string{"az": nil, "env": nil, "node": {"w"}}), "missing_az", ""},
		{"missing root", k8sBase(), "missing_root", ""},
		{"empty root value", with(k8sBase(), map[string][]string{"node": {""}}), "missing_root", ""},
		{"pod and node", with(k8sBase(), map[string][]string{"pod": {"shop/web-0"}, "node": {"worker-1"}}), "invalid_scope", "node and pod"},
		{"malformed pod", with(k8sBase(), map[string][]string{"pod": {"web-0"}}), "invalid_scope", "web-0"},
		{"pod with empty name", with(k8sBase(), map[string][]string{"pod": {"shop/"}}), "invalid_scope", ""},
		{"aggr alone is not a root", with(k8sBase(), map[string][]string{"aggr": {"aggr1"}}), "missing_root", ""},
		{"pvc alone is not a root", with(k8sBase(), map[string][]string{"pvc": {"shop/data"}}), "missing_root", ""},
		{"application alone is not a root", with(k8sBase(), map[string][]string{"application": {"shop"}}), "missing_root", ""},
		{"node value too long", with(k8sBase(), map[string][]string{"node": {strings.Repeat("n", 254)}}), "invalid_scope", "node"},
		{"pod control character", with(k8sBase(), map[string][]string{"pod": {"shop/web\n0"}}), "invalid_scope", "pod"},
		{"cluster control character", with(k8sBase(), map[string][]string{"cluster": {"c\n1"}, "node": {"w"}}), "invalid_scope", "cluster"},
		{"invalid UTF-8", with(k8sBase(), map[string][]string{"namespace": {"sh\xffop"}, "node": {"w"}}), "invalid_scope", "namespace"},
		{"namespace too long", with(k8sBase(), map[string][]string{"namespace": {strings.Repeat("a", 254)}, "node": {"w"}}), "invalid_scope", "namespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := kubegraph.ParseK8sValues(tc.values)
			require.Error(t, err)
			var pe *kubegraph.ParseError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, tc.wantReason, pe.Reason)
			if tc.wantMsg != "" {
				assert.Contains(t, pe.Message, tc.wantMsg)
			}
		})
	}
}

func TestParseK8sValues_HappyPath(t *testing.T) {
	t.Run("node root, sorted de-duplicated selector sets", func(t *testing.T) {
		v := with(k8sBase(), map[string][]string{
			"cluster": {"c2", "c1", "c1", ""}, "az": {"b", "a"}, "env": {"prod", "dev"},
			"namespace": {"shop"}, "node": {"worker-2", "worker-1", "worker-1"},
		})
		req, err := kubegraph.ParseK8sValues(v)
		require.NoError(t, err)
		assert.Equal(t, []string{"c1", "c2"}, req.Selector.Cluster)
		assert.Equal(t, []string{"a", "b"}, req.Selector.AZ)
		assert.Equal(t, []string{"dev", "prod"}, req.Selector.Env)
		assert.Equal(t, []string{"shop"}, req.Selector.Namespace)
		assert.Equal(t, graph.StorageRootNode, req.Scope.Roots.Kind)
		assert.Equal(t, []string{"worker-1", "worker-2"}, req.Scope.Roots.Names)
		assert.Equal(t, map[string]struct{}{"c1": {}, "c2": {}}, req.Scope.Clusters)
		assert.Equal(t, map[string]struct{}{"shop": {}}, req.Scope.Namespaces)
		assert.True(t, req.End.After(req.Start))
	})

	t.Run("pod root", func(t *testing.T) {
		req, err := kubegraph.ParseK8sValues(with(k8sBase(), map[string][]string{"pod": {"shop/web-0", "platform/redis-0"}}))
		require.NoError(t, err)
		assert.Equal(t, graph.StorageRootPod, req.Scope.Roots.Kind)
		assert.Equal(t, []graph.PodRef{
			{Namespace: "platform", Name: "redis-0"},
			{Namespace: "shop", Name: "web-0"},
		}, req.Scope.Roots.Pods)
		assert.Empty(t, req.Selector.Namespace, "a pod root's namespace is not derived")
	})

	t.Run("prune and withdrawn parameters are ignored", func(t *testing.T) {
		plain, err := kubegraph.ParseK8sValues(with(k8sBase(), map[string][]string{"node": {"worker-1"}}))
		require.NoError(t, err)
		noisy, err := kubegraph.ParseK8sValues(with(k8sBase(), map[string][]string{
			"node": {"worker-1"}, "prune": {"false"}, "edge_type": {"nope"}, "aggr": {"aggr1"}, "depth": {"3"},
		}))
		require.NoError(t, err)
		assert.Equal(t, plain, noisy)
		_, err = kubegraph.ParseK8sValues(with(k8sBase(), map[string][]string{"node": {"worker-1"}, "prune": {"maybe"}}))
		require.NoError(t, err, "an invalid prune is not even read")
	})

	t.Run("value order is irrelevant", func(t *testing.T) {
		a, err := kubegraph.ParseK8sValues(with(k8sBase(), map[string][]string{"cluster": {"c2", "c1"}, "node": {"x"}}))
		require.NoError(t, err)
		b, err := kubegraph.ParseK8sValues(with(k8sBase(), map[string][]string{"cluster": {"c1", "c2"}, "node": {"x"}}))
		require.NoError(t, err)
		assert.Equal(t, a, b)
	})
}
