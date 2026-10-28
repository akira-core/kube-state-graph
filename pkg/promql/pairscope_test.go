package promql

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderNamesAcrossNamespaces(t *testing.T) {
	t.Parallel()
	render := func(q Query, sel Selector, label string, namespaces, names []string) (string, bool) {
		return RenderNamesAcrossNamespaces(q, time.Minute, LabelKeys{}, sel, label, namespaces, names)
	}

	t.Run("one namespace and one name render two equalities", func(t *testing.T) {
		got, ok := render(QPodInfo, Selector{}, PodLabel, []string{"shop"}, []string{"orders-0"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(kube_pod_info{namespace="shop",pod="orders-0"}[1m])`, got)
	})

	t.Run("several values render sorted, de-duplicated alternations", func(t *testing.T) {
		got, ok := render(QPodOwner, Selector{}, PodLabel,
			[]string{"shop", "platform", "shop"}, []string{"web-0", "orders-0", "redis-0", "orders-0"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(kube_pod_owner{namespace=~"platform|shop",pod=~"orders-0|redis-0|web-0"}[1m])`, got,
			"a pure function of the two value sets")
	})

	t.Run("fixed selector, then request matchers, then namespaces, then names", func(t *testing.T) {
		sel := Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Cluster: []string{"c1"}, Namespace: []string{"shop"}}
		got, ok := render(QPVCBindings, sel, PodLabel, []string{"platform", "shop"}, []string{"redis-0", "orders-0"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info{az="zone-a",env="prod",cluster="c1",namespace="shop",namespace=~"platform|shop",pod=~"orders-0|redis-0"}[1m])`,
			got, "the request namespace matcher is composed ahead of the restriction, never replaced")
	})

	t.Run("a family's fixed selector, where it has one, comes first", func(t *testing.T) {
		for _, q := range slices.Concat(PodNamespaceScopedQueries, ClaimNamespaceScopedQueries) {
			label := PodLabel
			if !slices.Contains(PodNamespaceScopedQueries, q) {
				label = ClaimLabel
			}
			got, ok := render(q, Selector{AZ: []string{"zone-a"}}, label, []string{"shop"}, []string{"a"})
			require.True(t, ok, q)
			if fixed := fixedSelector[q]; fixed != "" {
				assert.Contains(t, got, `{`+fixed+`,az="zone-a",`, "%s: fixed selector, then request matchers", q)
			} else {
				assert.Contains(t, got, `{az="zone-a",namespace="shop",`, "%s: request matchers, then the restriction", q)
			}
		}
	})

	t.Run("regex metacharacters and quotes are escaped", func(t *testing.T) {
		got, ok := render(QPodInfo, Selector{}, PodLabel, []string{"shop.a", "b"}, []string{`web"0`, "z"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(kube_pod_info{namespace=~"b|shop\\.a",pod=~"web\"0|z"}[1m])`, got)
		one, ok := render(QPodInfo, Selector{}, PodLabel, []string{"shop.a"}, []string{`web"0`})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(kube_pod_info{namespace="shop.a",pod="web\"0"}[1m])`, one,
			"a single value is an equality, so only string escapes apply")
	})

	t.Run("the unknown cluster renders both alternatives", func(t *testing.T) {
		got, ok := render(QPodInfo, Selector{Cluster: []string{ClusterUnknownValue}}, PodLabel,
			[]string{"shop"}, []string{"orders-0"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(kube_pod_info{cluster=~"unknown|",namespace="shop",pod="orders-0"}[1m])`, got)
	})

	t.Run("every pod-keyed and claim-keyed family is accepted on its own label", func(t *testing.T) {
		ns, names := []string{"shop"}, []string{"a"}
		for _, q := range PodNamespaceScopedQueries {
			_, ok := render(q, Selector{}, PodLabel, ns, names)
			assert.True(t, ok, "%s on pod", q)
			_, ok = render(q, Selector{}, ClaimLabel, ns, names)
			// The binding family is keyed both ways: by pod for a pod seed, by
			// claim for mounter completion.
			assert.Equal(t, q == QPVCBindings, ok, "%s on claim", q)
		}
		for _, q := range ClaimNamespaceScopedQueries {
			_, ok := render(q, Selector{}, ClaimLabel, ns, names)
			assert.True(t, ok, "%s on claim", q)
		}
		_, ok := render(QNodeInfo, Selector{}, PodLabel, ns, names)
		assert.False(t, ok, "a node family is not namespaced")
	})

	t.Run("restricting only one side renders nothing", func(t *testing.T) {
		for name, tc := range map[string]struct{ namespaces, names []string }{
			"no namespace":     {nil, []string{"a"}},
			"no name":          {[]string{"shop"}, nil},
			"empty namespaces": {[]string{""}, []string{"a"}},
			"empty names":      {[]string{"shop"}, []string{""}},
		} {
			_, ok := render(QPodInfo, Selector{}, PodLabel, tc.namespaces, tc.names)
			assert.False(t, ok, name)
		}
	})

	t.Run("it is the claim-binding renderer generalised", func(t *testing.T) {
		ns, pods := []string{"platform", "shop"}, []string{"redis-0", "orders-0"}
		got, ok := render(QPVCBindings, Selector{}, PodLabel, ns, pods)
		require.True(t, ok)
		want, ok := RenderClaimBindingsByPod(time.Minute, LabelKeys{}, Selector{}, ns, pods)
		require.True(t, ok)
		assert.Equal(t, want, got)
	})
}

// chunkCost is the rendered byte length of the two alternations a chunk of
// pairs carries, the comma joining them included.
func chunkCost(label string, chunk []NamePair) int {
	namespaces := make([]string, 0, len(chunk))
	names := make([]string, 0, len(chunk))
	for _, p := range chunk {
		namespaces = append(namespaces, p.Namespace)
		names = append(names, p.Name)
	}
	return MatcherCost(NamespaceLabel, namespaces) + 1 + MatcherCost(label, names)
}

func TestChunkNamePairs(t *testing.T) {
	t.Parallel()

	t.Run("nothing to chunk", func(t *testing.T) {
		assert.Nil(t, ChunkNamePairs(nil, PodLabel, 100))
		assert.Nil(t, ChunkNamePairs([]NamePair{{"", "a"}, {"shop", ""}}, PodLabel, 100))
	})

	t.Run("the budget decides where a chunk ends, to the byte", func(t *testing.T) {
		// namespace=~"" + , + pod=~"" is 21 bytes; the first pair adds 1 + 1, the
		// second 2 + 2 (a value and its separator each).
		pairs := []NamePair{{"b", "y"}, {"a", "x"}}
		assert.Equal(t, [][]NamePair{{{"a", "x"}, {"b", "y"}}}, ChunkNamePairs(pairs, PodLabel, 27))
		assert.Equal(t, [][]NamePair{{{"a", "x"}}, {{"b", "y"}}}, ChunkNamePairs(pairs, PodLabel, 26))
	})

	t.Run("a repeated namespace or name is paid once", func(t *testing.T) {
		// Same namespace: only the second name's 2 bytes are added.
		pairs := []NamePair{{"a", "x"}, {"a", "y"}}
		assert.Len(t, ChunkNamePairs(pairs, PodLabel, 25), 1)
		assert.Len(t, ChunkNamePairs(pairs, PodLabel, 24), 2)
	})

	t.Run("a pair larger than the budget still gets its own chunk", func(t *testing.T) {
		huge := NamePair{"shop", "a-very-long-pod-name-that-does-not-fit-anything"}
		got := ChunkNamePairs([]NamePair{{"shop", "a"}, huge, {"zzz", "b"}}, PodLabel, 10)
		require.Len(t, got, 3, "no pair is dropped and none is merged past the budget")
		assert.Contains(t, got, []NamePair{huge})
	})

	t.Run("a zero budget is one chunk", func(t *testing.T) {
		got := ChunkNamePairs([]NamePair{{"b", "y"}, {"a", "x"}}, PodLabel, 0)
		assert.Equal(t, [][]NamePair{{{"a", "x"}, {"b", "y"}}}, got)
	})

	t.Run("chunks are namespace-local, sorted and cover every pair once", func(t *testing.T) {
		pairs := make([]NamePair, 0, 30)
		for _, ns := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"} {
			for i := range 5 {
				pairs = append(pairs, NamePair{ns, fmt.Sprintf("pod-%d", i)})
			}
		}
		slices.Reverse(pairs)
		const budget = 60
		chunks := ChunkNamePairs(pairs, PodLabel, budget)
		require.Greater(t, len(chunks), 1)

		var flat []NamePair
		for _, c := range chunks {
			assert.LessOrEqual(t, chunkCost(PodLabel, c), budget, "chunk %v", c)
			flat = append(flat, c...)
		}
		assert.Equal(t, SortedNamePairs(pairs), flat, "the chunks are consecutive runs of the sorted set")

		// Sorting by namespace first means a namespace only straddles a chunk
		// boundary, never several chunks.
		seen := map[string]int{}
		for i, c := range chunks {
			last := ""
			for _, p := range c {
				if p.Namespace != last {
					last = p.Namespace
					if prev, ok := seen[p.Namespace]; ok {
						assert.Equal(t, i-1, prev, "namespace %s reappears only in the next chunk", p.Namespace)
					}
					seen[p.Namespace] = i
				}
			}
		}
	})

	t.Run("the split is a pure function of the set", func(t *testing.T) {
		a := []NamePair{{"b", "y"}, {"a", "x"}, {"a", "z"}, {"c", "w"}}
		b := []NamePair{{"c", "w"}, {"a", "z"}, {"b", "y"}, {"a", "x"}, {"a", "x"}}
		assert.Equal(t, ChunkNamePairs(a, ClaimLabel, 40), ChunkNamePairs(b, ClaimLabel, 40))
	})
}

func TestKeepNamePairs(t *testing.T) {
	t.Parallel()
	row := func(ns, pod string) *model.Sample {
		return &model.Sample{Metric: model.Metric{"namespace": model.LabelValue(ns), PodLabel: model.LabelValue(pod)}}
	}
	rows := model.Vector{
		row("shop", "orders-0"),
		row("platform", "orders-0"), // a cross pair: both halves were requested, just not together
		row("platform", "redis-0"),
		row("shop", "redis-0"), // another
		row("shop", "orders-0"),
		{Metric: model.Metric{"namespace": "shop"}}, // names no pod
	}
	got := KeepNamePairs(rows, PodLabel, []NamePair{{"shop", "orders-0"}, {"platform", "redis-0"}})
	require.Len(t, got, 3)
	assert.Same(t, rows[0], got[0], "input order is kept and the samples are shared, not copied")
	assert.Same(t, rows[2], got[1])
	assert.Same(t, rows[4], got[2])
	assert.Len(t, rows, 6, "the input is not mutated")

	assert.Empty(t, KeepNamePairs(rows, PodLabel, nil))
}

// The upstream judges each label filter on its own, so a chunk closes when
// either alternation — distinct namespaces or distinct names — would pass
// MaxAlternationValues, never on the pair count.
func TestChunkNamePairs_BoundsEachAlternation(t *testing.T) {
	t.Parallel()

	sides := func(chunks [][]NamePair) []string {
		out := make([]string, len(chunks))
		for i, c := range chunks {
			ns, names := map[string]struct{}{}, map[string]struct{}{}
			for _, p := range c {
				ns[p.Namespace] = struct{}{}
				names[p.Name] = struct{}{}
			}
			out[i] = fmt.Sprintf("ns=%d names=%d", len(ns), len(names))
		}
		return out
	}

	for _, budget := range []int{0, 1 << 20} {
		t.Run(fmt.Sprintf("150 pods in one namespace, budget %d", budget), func(t *testing.T) {
			pairs := make([]NamePair, 0, 150)
			for _, n := range numberedValues("pod-", 150) {
				pairs = append(pairs, NamePair{"shop", n})
			}
			got := ChunkNamePairs(pairs, PodLabel, budget)
			assert.Equal(t, []string{"ns=1 names=100", "ns=1 names=50"}, sides(got))
		})

		t.Run(fmt.Sprintf("120 namespaces sharing one pod name, budget %d", budget), func(t *testing.T) {
			pairs := make([]NamePair, 0, 120)
			for _, ns := range numberedValues("ns-", 120) {
				pairs = append(pairs, NamePair{ns, "web-0"})
			}
			got := ChunkNamePairs(pairs, PodLabel, budget)
			assert.Equal(t, []string{"ns=100 names=1", "ns=20 names=1"}, sides(got))
		})

		t.Run(fmt.Sprintf("more pairs than the bound but each side within it, budget %d", budget), func(t *testing.T) {
			pairs := make([]NamePair, 0, 225)
			for _, ns := range []string{"a", "b", "c"} {
				for _, n := range numberedValues("pod-", 75) {
					pairs = append(pairs, NamePair{ns, n})
				}
			}
			got := ChunkNamePairs(pairs, PodLabel, budget)
			assert.Equal(t, []string{"ns=3 names=75"}, sides(got), "225 pairs, but 3 namespaces and 75 names")
		})
	}

	t.Run("exactly 100 names in 3 namespaces stay one chunk", func(t *testing.T) {
		pairs := make([]NamePair, 0, 100)
		for i, n := range numberedValues("pod-", 100) {
			pairs = append(pairs, NamePair{[]string{"a", "b", "c"}[i%3], n})
		}
		assert.Len(t, ChunkNamePairs(pairs, PodLabel, 1<<20), 1)
	})
}
