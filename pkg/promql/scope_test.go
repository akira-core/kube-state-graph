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

	t.Run("only the reference-scoped families are scopeable", func(t *testing.T) {
		for _, q := range ReferenceScopedQueries {
			_, ok := RenderScoped(q, time.Minute, LabelKeys{}, Selector{}, []string{"a"})
			assert.True(t, ok, string(q))
		}
		for _, q := range []Query{QPodContainerInfo, QPVCBindings, QQoSReadOps, QAlerts} {
			_, ok := RenderScoped(q, time.Minute, LabelKeys{}, Selector{}, []string{"a"})
			assert.False(t, ok, string(q))
		}
	})

	t.Run("a fixed selector is rendered ahead of the scope", func(t *testing.T) {
		got, ok := RenderScoped(QJobOwner, time.Minute, LabelKeys{}, Selector{}, []string{"b", "a"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(kube_job_owner{owner_kind="CronJob",owner_is_controller="true",job_name=~"a|b"}[1m])`,
			got, "the CronJob-controller selector must survive a scoped render")
	})

	t.Run("the node type alternation precedes the node scope", func(t *testing.T) {
		got, ok := RenderScoped(QNodeAddresses, time.Minute, LabelKeys{}, Selector{}, []string{"n2", "n1"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(kube_node_status_addresses{type=~"ExternalIP|InternalIP",node=~"n1|n2"}[1m])`,
			got)
	})

	t.Run("the tracking-id selector survives a scoped controller-annotation render", func(t *testing.T) {
		got, ok := RenderScoped(QDeploymentAnnotations, time.Minute, LabelKeys{}, Selector{}, []string{"web"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(kube_deployment_annotations{annotation_argocd_argoproj_io_tracking_id!="",deployment="web"}[1m])`,
			got)
	})

	t.Run("a non-scopeable query still returns ok=false", func(t *testing.T) {
		for _, q := range []Query{QPodContainerInfo, QPVCBindings, QQoSReadOps, QAlerts, QUpProbe} {
			_, ok := RenderScoped(q, time.Minute, LabelKeys{}, Selector{}, []string{"a"})
			assert.False(t, ok, string(q))
		}
	})
}

// TestRenderScoped_IsRenderPlusOneMatcher pins, for EVERY scopeable query and
// with and without a request-scoped selector, that the scoped string is
// exactly Render's output with one `<label>=~"…"` (or `<label>="…"`) matcher
// appended as the last element inside the braces — so a family's fixed
// selector can never drift between its unscoped and scoped renderings
// (scope-controller-legs-by-reference D2).
func TestRenderScoped_IsRenderPlusOneMatcher(t *testing.T) {
	t.Parallel()
	sel := Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Cluster: []string{"c1"}, Namespace: []string{"shop"}}
	for _, q := range ReferenceScopedQueries {
		label := scopedLabel[q]
		for _, s := range []Selector{{}, sel} {
			t.Run(string(q), func(t *testing.T) {
				got, ok := RenderScoped(q, time.Minute, LabelKeys{}, s, []string{"b", "a"})
				require.True(t, ok)
				base := Render(q, time.Minute, LabelKeys{}, s)
				want := strings.TrimSuffix(base, "}[1m])") + `,` + label + `=~"a|b"}[1m])`
				if !strings.Contains(base, "{") {
					// No fixed selector and no request matcher: Render emits no
					// braces at all, so the scoped form opens its own.
					want = strings.TrimSuffix(base, "[1m])") + `{` + label + `=~"a|b"}[1m])`
				}
				assert.Equal(t, want, got)
			})
		}
	}
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

// A by-reference plan changes WHICH BUILD issues a family, never what
// dimension that family accepts once it is issued: every ReferenceScopedQueries
// member keeps the queryDims entry it had before this capability existed, and
// TestQueryDims_EveryQueryListed still holds.
func TestQueryDims_ReferenceScopedLegsUnchanged(t *testing.T) {
	t.Parallel()
	want := map[Query]dims{
		QPodInfo:  dimsNamespaced,
		QPodOwner: dimsNamespaced,

		QNodeInfo:            dimsClusterScoped,
		QNodeAddresses:       dimsClusterScoped,
		QNodeLabels:          dimsClusterScoped,
		QNodeStatusCondition: dimsClusterScoped,

		QReplicaSetOwner:        dimsNamespaced,
		QReplicaSetAnnotations:  dimsNamespaced,
		QJobOwner:               dimsNamespaced,
		QJobAnnotations:         dimsNamespaced,
		QDeploymentAnnotations:  dimsNamespaced,
		QStatefulSetAnnotations: dimsNamespaced,
		QDaemonSetAnnotations:   dimsNamespaced,
		QCronJobAnnotations:     dimsNamespaced,
	}
	assert.Len(t, want, len(ReferenceScopedQueries), "the pin must cover every scoped query")
	for _, q := range ReferenceScopedQueries {
		assert.Equal(t, want[q], queryDims[q], string(q))
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
