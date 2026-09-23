package promql

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderVolumeLabelsRooted(t *testing.T) {
	t.Parallel()

	t.Run("aggregate alone renders an aggr matcher only", func(t *testing.T) {
		got, ok := RenderVolumeLabelsRooted(5*time.Minute, nil, []string{"aggr00"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{aggr="aggr00"}[5m])`, got)
	})

	t.Run("cluster alone renders a cluster matcher only", func(t *testing.T) {
		got, ok := RenderVolumeLabelsRooted(5*time.Minute, []string{"ontap-prod"}, nil)
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{cluster="ontap-prod"}[5m])`, got)
	})

	t.Run("both are AND-combined in one selector, cluster first", func(t *testing.T) {
		// The projection combines these two as a narrowing, so a single selector
		// carrying both is exactly its rule — not two merged queries.
		got, ok := RenderVolumeLabelsRooted(5*time.Minute,
			[]string{"ontap-prod"}, []string{"aggr00"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(volume_labels{cluster="ontap-prod",aggr="aggr00"}[5m])`, got)
	})

	t.Run("several values render one anchored alternation each, sorted", func(t *testing.T) {
		got, ok := RenderVolumeLabelsRooted(time.Minute,
			[]string{"ontap-b", "ontap-a"}, []string{"aggr2", "aggr1"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(volume_labels{cluster=~"ontap-a|ontap-b",aggr=~"aggr1|aggr2"}[1m])`, got,
			"sorted, so the string is a pure function of the two value sets")
	})

	t.Run("duplicates and empties are normalised away", func(t *testing.T) {
		got, ok := RenderVolumeLabelsRooted(time.Minute, []string{"", "c1", "c1"}, []string{""})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{cluster="c1"}[1m])`, got,
			"an all-empty set contributes no matcher rather than an empty one")
	})

	t.Run("a metacharacter matches itself literally", func(t *testing.T) {
		got, ok := RenderVolumeLabelsRooted(time.Minute, nil, []string{"aggr.a", "aggr_b"})
		require.True(t, ok)
		assert.Contains(t, got, `aggr=~"aggr\\.a|aggr_b"`,
			"QuoteMeta then string-escape, so the parser unquotes into the regex aggr\\.a")
	})

	t.Run("no value at all is not renderable", func(t *testing.T) {
		// Never an unrestricted fallback: the caller decides that, because an
		// empty restriction means "no root", not "match nothing".
		_, ok := RenderVolumeLabelsRooted(time.Minute, nil, nil)
		assert.False(t, ok)
		_, ok = RenderVolumeLabelsRooted(time.Minute, []string{""}, []string{"", ""})
		assert.False(t, ok)
	})
}

func TestRenderVolumeLabelsSVMRooted(t *testing.T) {
	t.Parallel()

	t.Run("svm alone renders an svm matcher only", func(t *testing.T) {
		got, ok := RenderVolumeLabelsSVMRooted(5*time.Minute, nil, []string{"svm_shop"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{svm="svm_shop"}[5m])`, got)
	})

	t.Run("cluster and svm are AND-combined in one selector, cluster first", func(t *testing.T) {
		got, ok := RenderVolumeLabelsSVMRooted(time.Minute,
			[]string{"ontap-b", "ontap-a"}, []string{"svm_b", "svm_a", "svm_a"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(volume_labels{cluster=~"ontap-a|ontap-b",svm=~"svm_a|svm_b"}[1m])`, got,
			"sorted and de-duplicated, so the string is a pure function of the two value sets")
	})

	t.Run("a metacharacter matches itself literally", func(t *testing.T) {
		got, ok := RenderVolumeLabelsSVMRooted(time.Minute, []string{"ontap.prod"}, []string{"svm.a", "svm_b"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(volume_labels{cluster="ontap.prod",svm=~"svm\\.a|svm_b"}[1m])`, got,
			"an equality needs no regex escape; an alternation is QuoteMeta'd and string-escaped")
	})

	t.Run("an empty svm set is not renderable, whatever the clusters", func(t *testing.T) {
		_, ok := RenderVolumeLabelsSVMRooted(time.Minute, []string{"ontap-prod"}, nil)
		assert.False(t, ok, "a cluster set alone is the cluster group's shape, not this one's")
		_, ok = RenderVolumeLabelsSVMRooted(time.Minute, nil, []string{"", ""})
		assert.False(t, ok)
	})
}

func TestRenderVolumeLabelsOwnerCompletion(t *testing.T) {
	t.Parallel()

	t.Run("one cluster equality and the aggregate alternation", func(t *testing.T) {
		got, ok := RenderVolumeLabelsOwnerCompletion(5*time.Minute, "ontap-prod", []string{"aggr07", "aggr03", "aggr03"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{cluster="ontap-prod",aggr=~"aggr03|aggr07"}[5m])`, got)
	})

	t.Run("one aggregate renders an equality", func(t *testing.T) {
		got, ok := RenderVolumeLabelsOwnerCompletion(time.Minute, "ontap-prod", []string{"aggr03"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{cluster="ontap-prod",aggr="aggr03"}[1m])`, got)
	})

	t.Run("escaping", func(t *testing.T) {
		got, ok := RenderVolumeLabelsOwnerCompletion(time.Minute, `on"tap`, []string{"aggr.1", "aggr_2"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{cluster="on\"tap",aggr=~"aggr\\.1|aggr_2"}[1m])`, got)
	})

	t.Run("an empty cluster is still an equality, never dropped", func(t *testing.T) {
		// An aggregate name is unique only within its filer: dropping the matcher
		// would read every filer's aggregate of that name.
		got, ok := RenderVolumeLabelsOwnerCompletion(time.Minute, "", []string{"aggr1"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{cluster="",aggr="aggr1"}[1m])`, got)
	})

	t.Run("an empty aggregate set is not renderable", func(t *testing.T) {
		_, ok := RenderVolumeLabelsOwnerCompletion(time.Minute, "ontap-prod", nil)
		assert.False(t, ok)
		_, ok = RenderVolumeLabelsOwnerCompletion(time.Minute, "ontap-prod", []string{""})
		assert.False(t, ok)
	})

	t.Run("the repeated cluster matcher's cost is its rendered length plus the comma", func(t *testing.T) {
		assert.Equal(t, len(`cluster="ontap-prod"`)+1, OwnerCompletionClusterCost("ontap-prod"))
		assert.Equal(t, len(`cluster="on\"tap"`)+1, OwnerCompletionClusterCost(`on"tap`))
	})
}

func TestRenderVolumeLabelsTokenScoped(t *testing.T) {
	t.Parallel()

	t.Run("suffix renders a dot-star prefix on every branch, always as a regex", func(t *testing.T) {
		got, ok := RenderVolumeLabelsTokenScoped(5*time.Minute, []string{"pvc_b", "pvc_a"}, true, nil)
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{volume=~".*pvc_a|.*pvc_b"}[5m])`, got)
	})

	t.Run("suffix with one token is still the regex form", func(t *testing.T) {
		// The prefix needs a regex, so the one-value equality shortcut appendMatcher
		// takes elsewhere would be WRONG here: volume="pvc_a" would demand equality.
		got, ok := RenderVolumeLabelsTokenScoped(time.Minute, []string{"pvc_a"}, true, nil)
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{volume=~".*pvc_a"}[1m])`, got)
	})

	t.Run("exact renders the token alone", func(t *testing.T) {
		one, ok := RenderVolumeLabelsTokenScoped(time.Minute, []string{"pvc_a"}, false, nil)
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{volume="pvc_a"}[1m])`, one)

		many, ok := RenderVolumeLabelsTokenScoped(time.Minute, []string{"pvc_b", "pvc_a"}, false, nil)
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{volume=~"pvc_a|pvc_b"}[1m])`, many)
	})

	t.Run("escaping is applied to the token and never to the prefix", func(t *testing.T) {
		got, ok := RenderVolumeLabelsTokenScoped(time.Minute, []string{"pvc.a", "pvc_b"}, true, nil)
		require.True(t, ok)
		assert.Contains(t, got, `volume=~".*pvc\\.a|.*pvc_b"`,
			"the token's dot is escaped; the prefix's dot-star is left as the wildcard it is")
		assert.NotContains(t, got, `\\.*`, "the prefix must never be escaped")
	})

	t.Run("an empty scope renders nothing at all", func(t *testing.T) {
		_, ok := RenderVolumeLabelsTokenScoped(time.Minute, nil, true, nil)
		assert.False(t, ok)
		_, ok = RenderVolumeLabelsTokenScoped(time.Minute, []string{"", ""}, false, nil)
		assert.False(t, ok)
	})

	t.Run("it never carries an aggr matcher", func(t *testing.T) {
		// Phase 2 must reach volumes on ANY aggregate — a clone on a
		// lexically-smaller one is exactly what it exists to recover.
		got, ok := RenderVolumeLabelsTokenScoped(time.Minute, []string{"pvc_a"}, true, nil)
		require.True(t, ok)
		assert.NotContains(t, got, "aggr")
		assert.NotContains(t, got, "cluster")
	})

	t.Run("excluded clusters render a negative matcher before the tokens", func(t *testing.T) {
		// A restriction by ONTAP cluster alone read those filers whole, so the
		// only candidates left are elsewhere. Without this, phase 2 re-scans
		// the family phase 1 already returned.
		got, ok := RenderVolumeLabelsTokenScoped(time.Minute, []string{"pvc_a"}, true, []string{"ontap-prod"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(volume_labels{cluster!~"ontap-prod",volume=~".*pvc_a"}[1m])`, got)

		many, ok := RenderVolumeLabelsTokenScoped(time.Minute, []string{"pvc_a"}, false, []string{"b", "a", "", "a"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(volume_labels{cluster!~"a|b",volume="pvc_a"}[1m])`, many,
			"sorted and de-duplicated like every other alternation; always the regex form")

		esc, ok := RenderVolumeLabelsTokenScoped(time.Minute, []string{"pvc_a"}, true, []string{"ontap.a"})
		require.True(t, ok)
		assert.Contains(t, esc, `cluster!~"ontap\\.a"`, "an excluded name matches itself literally")

		none, ok := RenderVolumeLabelsTokenScoped(time.Minute, []string{"pvc_a"}, true, []string{"", ""})
		require.True(t, ok)
		assert.NotContains(t, none, "cluster", "an all-empty exclusion adds no matcher")
	})
}

func TestChunkScopeWithOverhead(t *testing.T) {
	t.Parallel()

	tokens := []string{"pvc_aaaa", "pvc_bbbb", "pvc_cccc", "pvc_dddd"} // 8 bytes each

	t.Run("zero overhead is ChunkScope", func(t *testing.T) {
		assert.Equal(t, ChunkScope(tokens, 20), ChunkScopeWithOverhead(tokens, 20, 0))
	})

	t.Run("the branch overhead is counted against the budget", func(t *testing.T) {
		// Without overhead: 8 + (8+1) = 17 fits in 20, a third would not.
		assert.Equal(t, [][]string{tokens[:2], tokens[2:]}, ChunkScope(tokens, 20))
		// With the two-byte prefix each branch costs 10: 10 + (10+1) = 21 > 20,
		// so each token needs its own chunk.
		got := ChunkScopeWithOverhead(tokens, 20, VolumeTokenBranchOverhead)
		assert.Equal(t, [][]string{{"pvc_aaaa"}, {"pvc_bbbb"}, {"pvc_cccc"}, {"pvc_dddd"}}, got)
	})

	t.Run("every chunk's rendered alternation stays inside the budget", func(t *testing.T) {
		const budget = 64
		var many []string
		for i := range 40 {
			many = append(many, "pvc_"+strings.Repeat("x", i%7)+string(rune('a'+i%26)))
		}
		many = normaliseValues(many)
		for _, chunk := range ChunkScopeWithOverhead(many, budget, VolumeTokenBranchOverhead) {
			q, ok := RenderVolumeLabelsTokenScoped(time.Minute, chunk, true, nil)
			require.True(t, ok)
			alt := q[strings.Index(q, `~"`)+2 : strings.LastIndex(q, `"}`)]
			assert.LessOrEqual(t, len(alt), budget, "chunk %v rendered %q", chunk, alt)
		}
	})

	t.Run("a single over-budget token still gets its own chunk", func(t *testing.T) {
		// Dropping it would be a silently missing volume.
		long := strings.Repeat("v", 100)
		got := ChunkScopeWithOverhead([]string{"a", long, "b"}, 16, VolumeTokenBranchOverhead)
		require.Len(t, got, 3)
		assert.Equal(t, []string{long}, got[1])
	})

	t.Run("a negative overhead is treated as zero", func(t *testing.T) {
		assert.Equal(t, ChunkScope(tokens, 20), ChunkScopeWithOverhead(tokens, 20, -5))
	})

	t.Run("no values and a non-positive budget", func(t *testing.T) {
		assert.Nil(t, ChunkScopeWithOverhead(nil, 10, 2))
		assert.Equal(t, [][]string{tokens}, ChunkScopeWithOverhead(tokens, 0, 2))
	})
}

// TestVolumeLabelsRenderers_LeaveTheDimensionTableAlone pins the contract that
// makes a root-derived restriction a different mechanism from a selector-level
// dimension: Harvest keeps its routing-only bit, and no request dimension
// reaches the family.
func TestVolumeLabelsRenderers_LeaveTheDimensionTableAlone(t *testing.T) {
	t.Parallel()

	assert.Equal(t, dimsHarvest, queryDims[QVolumeLabels])
	assert.Equal(t, dimAZRoute, queryDims[QVolumeLabels])

	full := Selector{
		AZ: []string{"zone-a"}, Env: []string{"prod"},
		Cluster: []string{"c1"}, Namespace: []string{"shop"},
	}
	for _, dim := range []string{"az", "env", "namespace"} {
		assert.False(t, full.Reaches(QVolumeLabels), "dimension %s must not reach Harvest", dim)
	}
	assert.Equal(t, `last_over_time(volume_labels[5m])`,
		Render(QVolumeLabels, 5*time.Minute, LabelKeys{}, full),
		"the unrestricted rendering is still bare under every request dimension")
}

func TestMatcherCost(t *testing.T) {
	t.Parallel()

	t.Run("it measures the rendered matcher, not the raw values", func(t *testing.T) {
		dotted := []string{"ontap.a", "ontap.b"}
		assert.Equal(t, 30, MatcherCost("cluster", dotted))
		assert.Len(t, strings.Join(dotted, "|"), 15,
			"a raw join under-counts by the escaping and the wrapper")
	})

	t.Run("it agrees with what the renderer emits", func(t *testing.T) {
		for _, values := range [][]string{{"a"}, {"a", "b"}, {"ontap.a", "ontap.b"}, {`quo"te`}} {
			q, ok := RenderVolumeLabelsRooted(time.Minute, values, nil)
			require.True(t, ok)
			sel := q[strings.Index(q, "{")+1 : strings.LastIndex(q, "}")]
			assert.Equal(t, len(sel), MatcherCost("cluster", values), "for %q", values)
		}
	})

	t.Run("an empty set costs nothing", func(t *testing.T) {
		assert.Equal(t, 0, MatcherCost("cluster", nil))
		assert.Equal(t, 0, MatcherCost("cluster", []string{"", ""}))
	})
}
