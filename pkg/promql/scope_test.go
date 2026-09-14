package promql

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderScoped(t *testing.T) {
	t.Parallel()

	t.Run("an empty selector leaves the scope as the only matcher", func(t *testing.T) {
		got, ok := RenderScoped(QPodInfo, 5*time.Minute, LabelKeys{}, Selector{}, []string{"b", "a"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(kube_pod_info{pod=~"a|b"}[5m])`, got,
			"values are sorted, so the rendered string is a pure function of the set")
	})

	t.Run("one value renders an exact matcher", func(t *testing.T) {
		got, ok := RenderScoped(QPodOwner, time.Minute, LabelKeys{}, Selector{}, []string{"a", "", "a"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(kube_pod_owner{pod="a"}[1m])`, got)
	})

	t.Run("request matchers precede the scope", func(t *testing.T) {
		sel := Selector{
			AZ: []string{"zone-a"}, Env: []string{"prod"},
			Cluster: []string{"c1"}, Namespace: []string{"shop"},
		}
		for _, q := range PodScopedQueries {
			got, ok := RenderScoped(q, time.Minute, LabelKeys{}, sel, []string{"web-0", "orders-0"})
			require.True(t, ok)
			base := Render(q, time.Minute, LabelKeys{}, sel)
			want := strings.TrimSuffix(base, "}[1m])") + `,pod=~"orders-0|web-0"}[1m])`
			assert.Equal(t, want, got, "%s: Render's output with exactly one matcher appended", q)
		}
	})

	t.Run("configured label keys are honoured", func(t *testing.T) {
		got, ok := RenderScoped(QPodInfo, time.Minute, LabelKeys{AZ: "zone", Env: "stage"},
			Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}, []string{"a"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(kube_pod_info{zone="zone-a",stage="prod",pod="a"}[1m])`, got)
	})

	t.Run("a metacharacter in a name matches itself literally", func(t *testing.T) {
		got, ok := RenderScoped(QPodInfo, time.Minute, LabelKeys{}, Selector{}, []string{"web.0", "web-1"})
		require.True(t, ok)
		assert.Contains(t, got, `pod=~"web-1|web\\.0"`, "sorted: - sorts before .")
	})

	t.Run("an empty scope renders nothing at all", func(t *testing.T) {
		_, ok := RenderScoped(QPodInfo, time.Minute, LabelKeys{}, Selector{}, nil)
		assert.False(t, ok)
		_, ok = RenderScoped(QPodInfo, time.Minute, LabelKeys{}, Selector{}, []string{"", ""})
		assert.False(t, ok)
	})

	t.Run("only the pod-scoped families are scopeable", func(t *testing.T) {
		for _, q := range PodScopedQueries {
			_, ok := RenderScoped(q, time.Minute, LabelKeys{}, Selector{}, []string{"a"})
			assert.True(t, ok, string(q))
		}
		for _, q := range []Query{QPodContainerInfo, QPVCBindings, QNodeInfo, QQoSReadOps, QAlerts} {
			_, ok := RenderScoped(q, time.Minute, LabelKeys{}, Selector{}, []string{"a"})
			assert.False(t, ok, string(q))
		}
	})
}

// A data-derived scope is not a request dimension: the pod families keep the
// dimension set they had, so Selector.Reaches and every unscoped render of them
// are unchanged.
func TestQueryDims_ScopedPodLegsUnchanged(t *testing.T) {
	t.Parallel()
	for _, q := range PodScopedQueries {
		assert.Equal(t, dimsNamespaced, queryDims[q], string(q))
		assert.Equal(t, `last_over_time(`+string(q)+`[1m])`,
			Render(q, time.Minute, LabelKeys{}, Selector{}), "Render itself is untouched")
	}
}

func TestChunkScope_IsTheQoSChunker(t *testing.T) {
	t.Parallel()
	for _, in := range [][]string{nil, {"a"}, {"aaaa", "bbbb", "cccc", "dddd"}, {"short", "an_extremely_long_name"}} {
		for _, budget := range []int{0, 3, 9, 100} {
			assert.Equal(t, ChunkQoSVolumeScope(in, budget), ChunkScope(in, budget))
		}
	}
}
