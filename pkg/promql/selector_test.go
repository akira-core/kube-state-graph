package promql

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelector_Active(t *testing.T) {
	assert.False(t, Selector{}.Active(), "zero Selector is the unfiltered build")
	assert.False(t, Selector{AZ: []string{}, Namespace: []string{}}.Active(),
		"empty slices are not a filter")
	assert.False(t, Selector{Namespace: []string{"", ""}}.Active(),
		"a bare ?namespace= renders no matcher, so it must not switch the build into filtered mode")
	assert.True(t, Selector{AZ: []string{"zone-a"}}.Active())
	assert.True(t, Selector{Env: []string{"prod"}}.Active())
	assert.True(t, Selector{Cluster: []string{"c"}}.Active())
	assert.True(t, Selector{Namespace: []string{"shop"}}.Active())
}

func TestSelector_Render(t *testing.T) {
	cases := []struct {
		name string
		sel  Selector
		keys LabelKeys
		d    dims
		want string
	}{
		{"empty selector renders nothing", Selector{}, LabelKeys{}, dimsNamespaced, ""},
		{"no dimensions granted", Selector{AZ: []string{"zone-a"}}, LabelKeys{}, dimsNone, ""},
		{"single az", Selector{AZ: []string{"zone-a"}}, LabelKeys{}, dimsNamespaced, `az="zone-a"`},
		{
			"multi az sorted and de-duplicated",
			Selector{AZ: []string{"zone-b", "zone-a", "zone-a"}}, LabelKeys{}, dimsNamespaced,
			`az=~"zone-a|zone-b"`,
		},
		{
			"fixed dimension order",
			Selector{
				Namespace: []string{"shop"}, Cluster: []string{"c1"},
				Env: []string{"prod"}, AZ: []string{"zone-a"},
			},
			LabelKeys{}, dimsNamespaced,
			`az="zone-a",env="prod",cluster="c1",namespace="shop"`,
		},
		{
			"cluster-scoped query drops the namespace dimension",
			Selector{Namespace: []string{"shop"}, Cluster: []string{"c1"}}, LabelKeys{}, dimsClusterScoped,
			`cluster="c1"`,
		},
		{
			"harvest query drops cluster and namespace",
			Selector{
				AZ: []string{"zone-a"}, Env: []string{"prod"},
				Cluster: []string{"c1"}, Namespace: []string{"shop"},
			},
			LabelKeys{}, dimsHarvest,
			`az="zone-a",env="prod"`,
		},
		{
			"unknown cluster alone renders the empty-string matcher",
			Selector{Cluster: []string{"unknown"}}, LabelKeys{}, dimsNamespaced,
			`cluster=""`,
		},
		{
			"unknown cluster mixed keeps raw sort order",
			Selector{Cluster: []string{"unknown", "alpha"}}, LabelKeys{}, dimsNamespaced,
			`cluster=~"alpha|"`,
		},
		{
			"regex metacharacters are quoted for the literal AND the regex",
			Selector{Env: []string{"prod.eu", "prod-us"}}, LabelKeys{}, dimsNamespaced,
			`env=~"prod-us|prod\\.eu"`,
		},
		{
			"single value is exact, so a metacharacter needs no regex quoting",
			Selector{Env: []string{"prod.eu"}}, LabelKeys{}, dimsNamespaced,
			`env="prod.eu"`,
		},
		{
			"quote and backslash are escaped for the string literal",
			Selector{Namespace: []string{`we"ird\ns`}}, LabelKeys{}, dimsNamespaced,
			`namespace="we\"ird\\ns"`,
		},
		{
			"custom keys rebind az and env only",
			Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Cluster: []string{"c1"}},
			LabelKeys{AZ: "topology_zone", Env: "deployment_tier"}, dimsNamespaced,
			`topology_zone="zone-a",deployment_tier="prod",cluster="c1"`,
		},
		{
			"empty values are skipped",
			Selector{Namespace: []string{"", "shop", ""}}, LabelKeys{}, dimsNamespaced,
			`namespace="shop"`,
		},
		{
			"a dimension of only empty values renders nothing",
			Selector{Namespace: []string{"", ""}}, LabelKeys{}, dimsNamespaced,
			``,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.sel.render(tc.d, tc.keys))
		})
	}
}

// TestSelector_RenderIsOrderFree pins the determinism contract: the rendered
// matcher depends on the value SET, never on the order the caller supplied it.
func TestSelector_RenderIsOrderFree(t *testing.T) {
	a := Selector{AZ: []string{"b", "a"}, Namespace: []string{"y", "x"}}
	b := Selector{AZ: []string{"a", "b", "a"}, Namespace: []string{"x", "y"}}
	assert.Equal(t, a.render(dimsNamespaced, LabelKeys{}), b.render(dimsNamespaced, LabelKeys{}))
}

func TestLabelKeys_Defaults(t *testing.T) {
	assert.Equal(t, LabelKeys{AZ: "az", Env: "env"}, DefaultLabelKeys())
	assert.Equal(t, DefaultLabelKeys(), LabelKeys{}.OrDefault())
	assert.Equal(t, LabelKeys{AZ: "zone", Env: "env"}, LabelKeys{AZ: "zone"}.OrDefault())
}

// queryConstants parses queries.go and returns every constant declared with
// the `Query` type, mapped name → value. Parsing the source (rather than
// hardcoding a list) is what makes the completeness test below a real guard:
// a new constant is picked up automatically and must be given a queryDims
// entry in the same change.
func queryConstants(t *testing.T) map[string]Query {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "queries.go", nil, 0)
	require.NoError(t, err)

	out := map[string]Query{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var lastType string
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); ok {
				lastType = id.Name
			}
			if lastType != "Query" || len(vs.Values) == 0 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v, err := strconv.Unquote(lit.Value)
			require.NoError(t, err)
			out[vs.Names[0].Name] = Query(v)
		}
	}
	require.NotEmpty(t, out, "parsed no Query constants — the parser or the file layout changed")
	return out
}

// TestQueryDims_EveryQueryListed is the completeness guard for the
// series × dimension contract: every Query constant must have an explicit
// queryDims entry (dimsNone is an entry — an omission is not), and queryDims
// must not name a constant that no longer exists.
func TestQueryDims_EveryQueryListed(t *testing.T) {
	consts := queryConstants(t)
	for name, q := range consts {
		if _, ok := queryDims[q]; !ok {
			t.Errorf("Query constant %s (%q) has no queryDims entry — add one (dimsNone if it must stay unfiltered)", name, q)
		}
	}
	values := map[Query]struct{}{}
	for _, q := range consts {
		values[q] = struct{}{}
	}
	for q := range queryDims {
		if _, ok := values[q]; !ok {
			t.Errorf("queryDims names %q, which is not a Query constant", q)
		}
	}
}

// TestQueryDims_UnfilteredFamilies pins the two deliberate dimsNone groups.
// The service-graph series are read in full so a narrowed topology can still
// resolve (or externalise) every peer; the up{} probe measures the store.
func TestQueryDims_UnfilteredFamilies(t *testing.T) {
	for _, q := range []Query{
		QServiceGraphTotal, QServiceGraphFailedTotal, QServiceGraphServerSecondsBucket, QUpProbe,
	} {
		assert.Equal(t, dimsNone, queryDims[q], "%s must accept no request dimension", q)
	}
}

// TestQueryDims_HarvestNeverCarriesClusterOrNamespace pins the Harvest rule:
// its `cluster` label is the ONTAP cluster name, so a Kubernetes cluster value
// pushed into it would match nothing.
func TestQueryDims_HarvestNeverCarriesClusterOrNamespace(t *testing.T) {
	for _, q := range []Query{
		QVolumeLabels, QQoSReadOps, QQoSWriteOps, QQoSReadLatency, QQoSWriteLatency,
		QQoSReadData, QQoSWriteData, QQoSPolicyFixedMaxIOPS, QQoSPolicyFixedMaxMBps,
		QAggrStatus, QAggrSpaceUsed, QAggrSpaceTotal, QNetAppNodeStatus,
	} {
		assert.Equal(t, dimsHarvest, queryDims[q], "%s must accept az/env only", q)
	}
	sel := Selector{
		AZ: []string{"zone-a"}, Env: []string{"prod"},
		Cluster: []string{"c1"}, Namespace: []string{"shop"},
	}
	got := Render(QVolumeLabels, time.Minute, LabelKeys{}, sel)
	assert.Equal(t, `last_over_time(volume_labels{az="zone-a",env="prod"}[1m])`, got)
}

// TestRender_ComposesFixedSelectorFirst pins that a query's request-invariant
// selector is never reordered or replaced by the request matchers.
func TestRender_ComposesFixedSelectorFirst(t *testing.T) {
	sel := Selector{AZ: []string{"zone-a"}, Cluster: []string{"cluster-alpha"}, Namespace: []string{"shop"}}
	cases := map[string]struct {
		q    Query
		want string
	}{
		"node addresses": {QNodeAddresses,
			`last_over_time(kube_node_status_addresses{type=~"ExternalIP|InternalIP",az="zone-a",cluster="cluster-alpha"}[1m])`},
		"node condition": {QNodeStatusCondition,
			`last_over_time(kube_node_status_condition{condition="Ready",az="zone-a",cluster="cluster-alpha"}[1m])`},
		"qos volume granularity": {QQoSReadOps,
			`last_over_time(qos_read_ops{lun="",az="zone-a"}[1m])`},
		"pod info (no fixed selector)": {QPodInfo,
			`last_over_time(kube_pod_info{az="zone-a",cluster="cluster-alpha",namespace="shop"}[1m])`},
		"service graph total (unfiltered)": {QServiceGraphTotal,
			`rate(traces_service_graph_request_total{client!~"user|unknown",server!~"user"}[1m])`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, Render(tc.q, time.Minute, LabelKeys{}, sel))
		})
	}
}

// TestRender_EmptySelectorMatchesBaseline is the byte-identity proof for the
// unfiltered build: every query renders exactly the string it rendered before
// request-scoped selectors existed. testdata/render-baseline.txt was captured
// from the pre-change tree; the only expected difference is the deleted
// cluster_discovery query.
func TestRender_EmptySelectorMatchesBaseline(t *testing.T) {
	const removed = "cluster_discovery"

	raw, err := os.ReadFile("testdata/render-baseline.txt")
	require.NoError(t, err)

	want := map[Query]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rendered, ok := strings.Cut(line, "\t")
		require.True(t, ok, "malformed baseline line %q", line)
		if name == removed {
			continue
		}
		want[Query(name)] = rendered
	}
	require.NotEmpty(t, want)

	got := map[Query]string{}
	for q := range queryDims {
		got[q] = Render(q, 5*time.Minute, LabelKeys{}, Selector{})
	}
	assert.Equal(t, want, got,
		"unfiltered rendering drifted from the pre-change baseline (testdata/render-baseline.txt)")
}
