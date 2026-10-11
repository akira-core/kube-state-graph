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
		{"missing start", url.Values{"end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {"aggr1"}}, "missing_start", ""},
		{"missing end", url.Values{"start": {"1700000000"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {"aggr1"}}, "missing_end", ""},
		{"invalid start", url.Values{"start": {"nope"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {"aggr1"}}, "invalid_start", ""},
		{"invalid end", url.Values{"start": {"1700000000"}, "end": {"nope"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {"aggr1"}}, "invalid_end", ""},
		{"end not after start", url.Values{"start": {"1700003600"}, "end": {"1700000000"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {"aggr1"}}, "invalid_range", ""},
		{"missing az", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "env": {"prod"}}, "missing_az", ""},
		{"missing az before the root", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "env": {"prod"}}, "missing_az", ""},
		{"missing env", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}}, "missing_env", ""},
		{"missing root", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}}, "missing_root", ""},
		{"bare root is missing", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {""}}, "missing_root", ""},
		{"two root kinds", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {"aggr1"}, "pod": {"shop/orders-0"}}, "invalid_scope", "aggr and pod"},
		{"ontap_cluster no longer qualifies an aggregate", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "ontap_cluster": {"ontap-prod"}, "aggr": {"aggr1"}}, "invalid_scope", "ontap_cluster and aggr"},
		{"malformed pod root", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pod": {"orders-0"}}, "invalid_scope", "orders-0"},
		{"pod empty name", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pod": {"shop/"}}, "invalid_scope", ""},
		{"malformed pvc root", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pvc": {"orders-data"}}, "invalid_scope", "orders-data"},
		{"pvc root with two slashes", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pvc": {"shop/orders/data"}}, "invalid_scope", "shop/orders/data"},
		{"pvc empty name", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pvc": {"shop/"}}, "invalid_scope", ""},
		{"pvc empty namespace", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pvc": {"/orders-data"}}, "invalid_scope", ""},
		{"pvc and pv are two root kinds", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pvc": {"shop/orders-data"}, "pv": {"pvc-ab12-cd34"}}, "invalid_scope", "pvc and pv"},
		{"pod and pvc are two root kinds", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pod": {"shop/orders-0"}, "pvc": {"shop/orders-data"}}, "invalid_scope", "pod and pvc"},
		{"pv and application are two root kinds", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pv": {"pvc-ab12-cd34"}, "application": {"checkout"}}, "invalid_scope", "pv and application"},
		{"bare pvc is missing", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pvc": {""}}, "missing_root", ""},
		{"bare pv is missing", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pv": {""}}, "missing_root", ""},
		{"pvc value too long", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pvc": {"shop/" + strings.Repeat("a", 260)}}, "invalid_scope", "pvc"},
		{"pv value too long", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pv": {strings.Repeat("a", 254)}}, "invalid_scope", "pv"},
		{"pv control character", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "pv": {"pvc-\nab"}}, "invalid_scope", "pv"},
		{"selector value too long", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "aggr": {strings.Repeat("a", 254)}}, "invalid_scope", ""},
		{"application value too long", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "application": {strings.Repeat("a", 300)}}, "invalid_scope", "application"},
		{"application control character", url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"zone-a"}, "env": {"prod"}, "application": {"check\nout"}}, "invalid_scope", "application"},
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
	v["aggr"] = []string{"aggr1", "aggr1", ""}
	v["cluster"] = []string{"c1"}
	v["namespace"] = []string{"shop"}
	v["edge_type"] = []string{"storage-flow"}
	v["prune"] = []string{"false"}

	req, err := kubegraph.ParseStorageValues(v)
	require.NoError(t, err)
	assert.Equal(t, []string{"zone-a"}, req.Selector.AZ)
	assert.Equal(t, []string{"prod"}, req.Selector.Env)
	assert.Equal(t, []string{"c1"}, req.Selector.Cluster)
	assert.Equal(t, []string{"shop"}, req.Selector.Namespace)
	assert.Equal(t, graph.StorageRootAggr, req.Scope.Roots.Kind)
	assert.Equal(t, []string{"aggr1"}, req.Scope.Roots.Names)
	assert.Equal(t, map[string]struct{}{"shop": {}}, req.Scope.Namespaces)

	apps := storageBase()
	apps["application"] = []string{"checkout", "a"}
	req, err = kubegraph.ParseStorageValues(apps)
	require.NoError(t, err)
	assert.Equal(t, graph.StorageRootApplication, req.Scope.Roots.Kind)
	assert.Equal(t, []string{"a", "checkout"}, req.Scope.Roots.Names)
	assert.Empty(t, req.Selector.Namespace, "a pod root's namespace is not derived")

	pods := storageBase()
	pods["pod"] = []string{"shop/orders-0", "platform/redis-0"}
	req, err = kubegraph.ParseStorageValues(pods)
	require.NoError(t, err)
	assert.Equal(t, graph.StorageRootPod, req.Scope.Roots.Kind)
	assert.Equal(t, []graph.PodRef{
		{Namespace: "platform", Name: "redis-0"},
		{Namespace: "shop", Name: "orders-0"},
	}, req.Scope.Roots.Pods)
	assert.Empty(t, req.Selector.Namespace)

	claims := storageBase()
	claims["pvc"] = []string{"shop/orders-data", "platform/queue", "shop/orders-data", ""}
	req, err = kubegraph.ParseStorageValues(claims)
	require.NoError(t, err)
	assert.Equal(t, graph.StorageRootPVC, req.Scope.Roots.Kind)
	assert.Equal(t, []graph.ClaimRef{
		{Namespace: "platform", Name: "queue"},
		{Namespace: "shop", Name: "orders-data"},
	}, req.Scope.Roots.Claims)
	assert.Empty(t, req.Selector.Namespace, "a claim root's namespace is not derived")

	volumes := storageBase()
	volumes["pv"] = []string{"pvc-b", "pvc-a", "pvc-b"}
	req, err = kubegraph.ParseStorageValues(volumes)
	require.NoError(t, err)
	assert.Equal(t, graph.StorageRootPV, req.Scope.Roots.Kind)
	assert.Equal(t, []string{"pvc-a", "pvc-b"}, req.Scope.Roots.Names)
	assert.Empty(t, req.Scope.Roots.Claims)

	node := storageBase()
	node["ontap_node"] = []string{"ontap-prod-01"}
	req, err = kubegraph.ParseStorageValues(node)
	require.NoError(t, err)
	assert.Equal(t, graph.StorageRootONTAPNode, req.Scope.Roots.Kind)
	assert.Equal(t, []string{"ontap-prod-01"}, req.Scope.Roots.Names)
}

func TestParseStorageValues_IgnoresEdgeTypeAndPrune(t *testing.T) {
	v := storageBase()
	v.Set("aggr", "aggr1")
	v.Set("edge_type", "not-a-type")
	v.Set("prune", "maybe")
	got, err := kubegraph.ParseStorageValues(v)
	require.NoError(t, err, "edge_type and prune are ignored, even when invalid")

	bare := storageBase()
	bare.Set("aggr", "aggr1")
	want, err := kubegraph.ParseStorageValues(bare)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestParseStorageValues_FiltersCombineWithAnyRootKind(t *testing.T) {
	v := storageBase()
	v["aggr"] = []string{"aggr1"}
	v["namespace"] = []string{"shop"}
	v["cluster"] = []string{"c1"}
	req, err := kubegraph.ParseStorageValues(v)
	require.NoError(t, err)
	assert.Equal(t, graph.StorageRootAggr, req.Scope.Roots.Kind)
	assert.Equal(t, map[string]struct{}{"shop": {}}, req.Scope.Namespaces)
	assert.Equal(t, map[string]struct{}{"c1": {}}, req.Scope.Clusters)
}

func TestParseStorageValues_RepeatedAZAndEnv(t *testing.T) {
	v := url.Values{
		"start": {"1700000000"},
		"end":   {"1700003600"},
		"az":    {"zone-b", "zone-a", "zone-b", ""},
		"env":   {"prod", "dev"},
		"aggr":  {"aggr1"},
	}
	req, err := kubegraph.ParseStorageValues(v)
	require.NoError(t, err, "az and env are repeatable")
	assert.Equal(t, []string{"zone-a", "zone-b"}, req.Selector.AZ, "sorted, de-duplicated, empties dropped")
	assert.Equal(t, []string{"dev", "prod"}, req.Selector.Env)

	swapped := url.Values{
		"start": {"1700000000"},
		"end":   {"1700003600"},
		"az":    {"zone-a", "zone-b"},
		"env":   {"dev", "prod"},
		"aggr":  {"aggr1"},
	}
	want, err := kubegraph.ParseStorageValues(swapped)
	require.NoError(t, err)
	assert.Equal(t, want, req, "value order never changes the parsed request")
}

func TestParseStorageValues_EmptyZoneValuesAreMissing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		az, env    []string
		wantReason string
	}{
		{"only empty az", []string{"", ""}, []string{"prod"}, "missing_az"},
		{"only empty env", []string{"zone-a"}, []string{""}, "missing_env"},
		{"az is checked before env", nil, nil, "missing_az"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "aggr": {"aggr1"}}
			if tc.az != nil {
				v["az"] = tc.az
			}
			if tc.env != nil {
				v["env"] = tc.env
			}
			_, err := kubegraph.ParseStorageValues(v)
			var pe *kubegraph.ParseError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, tc.wantReason, pe.Reason)
		})
	}
}

func TestParseStorageValues_ZoneIsCheckedBeforeRootAndAfterWindow(t *testing.T) {
	// window first
	_, err := kubegraph.ParseStorageValues(url.Values{"end": {"1700003600"}})
	var pe *kubegraph.ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "missing_start", pe.Reason)

	// az before the root
	_, err = kubegraph.ParseStorageValues(url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "env": {"prod"}})
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "missing_az", pe.Reason)

	// a repeated pair is accepted, so the root check is what fires
	_, err = kubegraph.ParseStorageValues(url.Values{"start": {"1700000000"}, "end": {"1700003600"}, "az": {"a", "b"}, "env": {"x", "y"}})
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "missing_root", pe.Reason)
}

func TestParseStorageValues_ZoneValuesAreValidated(t *testing.T) {
	v := storageBase()
	v["aggr"] = []string{"aggr1"}
	v["az"] = []string{"zone-a", strings.Repeat("z", 260)}
	_, err := kubegraph.ParseStorageValues(v)
	var pe *kubegraph.ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "invalid_scope", pe.Reason)
	assert.Contains(t, pe.Message, "az")
}

func TestParseStorageValues_QualifiedAggrAndSVMRoots(t *testing.T) {
	v := storageBase()
	v["aggr"] = []string{"ontap-prod/aggr1", "aggr9", "ontap-prod/aggr1", "ontap-lab/aggr1", ""}
	req, err := kubegraph.ParseStorageValues(v)
	require.NoError(t, err)
	assert.Equal(t, graph.StorageRootAggr, req.Scope.Roots.Kind)
	assert.Equal(t, []string{"aggr9"}, req.Scope.Roots.Names)
	assert.Equal(t, []graph.ONTAPRef{
		{ONTAPCluster: "ontap-lab", Name: "aggr1"},
		{ONTAPCluster: "ontap-prod", Name: "aggr1"},
	}, req.Scope.Roots.Qualified)

	svm := storageBase()
	svm["svm"] = []string{"ontap-prod/svm0", "svm0", "ontap-lab/svm_shop"}
	req, err = kubegraph.ParseStorageValues(svm)
	require.NoError(t, err)
	assert.Equal(t, []string{"svm0"}, req.Scope.Roots.Names, "the bare value subsumes ontap-prod/svm0")
	assert.Equal(t, []graph.ONTAPRef{{ONTAPCluster: "ontap-lab", Name: "svm_shop"}}, req.Scope.Roots.Qualified)

	only := storageBase()
	only["aggr"] = []string{"ontap-prod/aggr9"}
	req, err = kubegraph.ParseStorageValues(only)
	require.NoError(t, err, "a qualified value alone is a root")
	assert.True(t, req.Scope.Roots.Any())
}

func TestParseStorageValues_MalformedQualifiedRoot(t *testing.T) {
	for _, tc := range []struct{ param, value string }{
		{"aggr", "ontap-prod/"},
		{"aggr", "/aggr1"},
		{"svm", "a/b/c"},
		{"svm", "/"},
	} {
		t.Run(tc.param+" "+tc.value, func(t *testing.T) {
			v := storageBase()
			v[tc.param] = []string{tc.value}
			_, err := kubegraph.ParseStorageValues(v)
			var pe *kubegraph.ParseError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, "invalid_scope", pe.Reason)
			assert.Contains(t, pe.Message, tc.param)
		})
	}
}

// Only aggr and svm split on "/": a slash in any other root kind's value is
// part of a bare name, exactly as before.
func TestParseStorageValues_SlashIsNotQualifyingOtherKinds(t *testing.T) {
	v := storageBase()
	v["ontap_cluster"] = []string{"a/b"}
	req, err := kubegraph.ParseStorageValues(v)
	require.NoError(t, err)
	assert.Equal(t, []string{"a/b"}, req.Scope.Roots.Names)
	assert.Empty(t, req.Scope.Roots.Qualified)
}
