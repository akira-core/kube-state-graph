package kubegraph_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
)

func storageBase() url.Values {
	return url.Values{
		"start": {"1700000000"},
		"end":   {"1700003600"},
		"az":    {"zone-a"},
		"env":   {"prod"},
	}
}

func TestParseStorageValues_Errors(t *testing.T) {
	cases := []struct {
		name       string
		values     url.Values
		wantReason string
		wantMsg    string
	}{
		{"missing start", url.Values{"end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}}, "missing_start", ""},
		{"missing end", url.Values{"start": {"1700000000"}, "az": {"zone-a"}, "env": {"prod"}}, "missing_end", ""},
		{"invalid start", url.Values{"start": {"nope"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}}, "invalid_start", ""},
		{"invalid end", url.Values{"start": {"1700000000"}, "end": {"nope"}, "az": {"zone-a"}, "env": {"prod"}}, "invalid_end", ""},
		{"end not after start", url.Values{"start": {"1700003600"}, "end": {"1700000000"}, "az": {"zone-a"}, "env": {"prod"}}, "invalid_range", ""},
		{"missing az", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "env": {"prod"}}, "missing_az", ""},
		{"missing env", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}}, "missing_env", ""},
		{"repeated env", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod", "dev"}}, "invalid_scope", "env"},
		{"repeated az", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a", "zone-b"}, "env": {"prod"}}, "invalid_scope", "az"},
		{"malformed pod root", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pod": {"orders-0"}}, "invalid_scope", "orders-0"},
		{"pod empty name", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pod": {"shop/"}}, "invalid_scope", ""},
		{"selector value too long", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {strings.Repeat("a", 254)}}, "invalid_scope", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := kubegraph.ParseStorageValues(tc.values)
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

func TestParseStorageValues_HappyPath(t *testing.T) {
	v := storageBase()
	v["aggr"] = []string{"aggr1", "aggr1"}
	v["pod"] = []string{"shop/orders-0"}
	v["cluster"] = []string{"c1"}
	v["edge_type"] = []string{"storage-flow"}
	v["prune"] = []string{"false"}

	req, err := kubegraph.ParseStorageValues(v)
	require.NoError(t, err)
	assert.Equal(t, []string{"zone-a"}, req.Selector.AZ)
	assert.Equal(t, []string{"prod"}, req.Selector.Env)
	assert.Equal(t, []string{"c1"}, req.Selector.Cluster)
	assert.Equal(t, map[string]struct{}{"aggr1": {}}, req.Scope.Roots.Aggrs)
	assert.Len(t, req.Scope.Roots.Pods, 1)
}

func TestParseStorageValues_IgnoresEdgeTypeAndPrune(t *testing.T) {
	v := storageBase()
	v.Set("edge_type", "not-a-type")
	v.Set("prune", "maybe")
	_, err := kubegraph.ParseStorageValues(v)
	require.NoError(t, err, "edge_type and prune are ignored, even when invalid")
}

// Spec: "Pod-only roots narrow the upstream read" — the derived selector, and
// every rule that suppresses it.
func TestParseStorageValues_DerivesNamespaceFromPodOnlyRoots(t *testing.T) {
	cases := []struct {
		name  string
		extra url.Values
		want  []string
	}{
		{"pod roots push their namespaces", url.Values{"pod": {"shop/orders-0", "platform/redis-0"}}, []string{"platform", "shop"}},
		{"one namespace collapses", url.Values{"pod": {"shop/a", "shop/b"}}, []string{"shop"}},
		{"a storage root suppresses it", url.Values{"pod": {"shop/orders-0"}, "aggr": {"aggr1"}}, nil},
		{"an ontap_cluster root suppresses it", url.Values{"pod": {"shop/orders-0"}, "ontap_cluster": {"ontap-prod"}}, nil},
		{"a node root suppresses it", url.Values{"pod": {"shop/orders-0"}, "node": {"n1"}}, nil},
		{"an explicit namespace wins", url.Values{"pod": {"shop/orders-0"}, "namespace": {"platform"}}, []string{"platform"}},
		{"an empty namespace value is no namespace", url.Values{"pod": {"shop/x"}, "namespace": {""}}, []string{"shop"}},
		{"no root derives nothing", url.Values{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := storageBase()
			for k, vs := range tc.extra {
				v[k] = vs
			}
			req, err := kubegraph.ParseStorageValues(v)
			require.NoError(t, err)
			assert.Equal(t, tc.want, req.Selector.Namespace)
		})
	}
}

// The derivation narrows the upstream read only: the projection's own
// namespace filter stays exactly what the request said.
func TestParseStorageValues_DerivedNamespaceLeavesTheProjectionAlone(t *testing.T) {
	v := storageBase()
	v["pod"] = []string{"shop/orders-0"}
	req, err := kubegraph.ParseStorageValues(v)
	require.NoError(t, err)
	assert.Equal(t, []string{"shop"}, req.Selector.Namespace)
	assert.Empty(t, req.Scope.Namespaces)
}

// Spec: "Root order does not change the queries".
func TestParseStorageValues_DerivedNamespaceIsOrderFree(t *testing.T) {
	a, b := storageBase(), storageBase()
	a["pod"] = []string{"b/x", "a/y"}
	b["pod"] = []string{"a/y", "b/x", "a/y"}
	ra, err := kubegraph.ParseStorageValues(a)
	require.NoError(t, err)
	rb, err := kubegraph.ParseStorageValues(b)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, ra.Selector.Namespace)
	assert.Equal(t, ra.Selector, rb.Selector)
}
