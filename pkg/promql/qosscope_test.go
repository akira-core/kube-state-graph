package promql

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderQoSVolumeScoped(t *testing.T) {
	t.Parallel()

	t.Run("one name renders an exact matcher beside lun", func(t *testing.T) {
		got, ok := RenderQoSVolumeScoped(QQoSReadOps, time.Minute, LabelKeys{}, Selector{}, []string{"trident_pvc_a"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(qos_read_ops{volume="trident_pvc_a"}[1m])`, got)
	})

	t.Run("several names render one anchored alternation", func(t *testing.T) {
		got, ok := RenderQoSVolumeScoped(QQoSWriteData, 5*time.Minute, LabelKeys{}, Selector{}, []string{"trident_pvc_b", "trident_pvc_a"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(qos_write_data{volume=~"trident_pvc_a|trident_pvc_b"}[5m])`, got,
			"values are sorted, so the rendered string is a pure function of the set")
	})

	t.Run("duplicates and empties are normalised away", func(t *testing.T) {
		got, ok := RenderQoSVolumeScoped(QQoSReadOps, time.Minute, LabelKeys{}, Selector{}, []string{"v_a", "", "v_a"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(qos_read_ops{volume="v_a"}[1m])`, got)
	})

	t.Run("a metacharacter in a name matches itself literally", func(t *testing.T) {
		got, ok := RenderQoSVolumeScoped(QQoSReadOps, time.Minute, LabelKeys{}, Selector{}, []string{"vol.a", "vol_b"})
		require.True(t, ok)
		assert.Contains(t, got, `volume=~"vol\\.a|vol_b"`,
			"QuoteMeta then string-escape, so the parser unquotes into the regex vol\\.a")
	})

	t.Run("an empty scope renders nothing at all", func(t *testing.T) {
		// Never an unscoped fallback: an empty scope means no claim matched, so
		// the unrestricted read could only fetch series the reader discards.
		_, ok := RenderQoSVolumeScoped(QQoSReadOps, time.Minute, LabelKeys{}, Selector{}, nil)
		assert.False(t, ok)
		_, ok = RenderQoSVolumeScoped(QQoSReadOps, time.Minute, LabelKeys{}, Selector{}, []string{"", ""})
		assert.False(t, ok)
	})

	t.Run("only the six workload families are scopeable", func(t *testing.T) {
		for _, q := range QoSWorkloadQueries {
			_, ok := RenderQoSVolumeScoped(q, time.Minute, LabelKeys{}, Selector{}, []string{"v"})
			assert.True(t, ok, string(q))
		}
		for _, q := range []Query{QVolumeLabels, QQoSPolicyFixedMaxIOPS, QAggrStatus, QPodInfo} {
			_, ok := RenderQoSVolumeScoped(q, time.Minute, LabelKeys{}, Selector{}, []string{"v"})
			assert.False(t, ok, string(q))
		}
	})

	t.Run("the volume scope is the only matcher", func(t *testing.T) {
		sel := Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Cluster: []string{"c1"}, Namespace: []string{"shop"}}
		for _, q := range QoSWorkloadQueries {
			got, ok := RenderQoSVolumeScoped(q, time.Minute, LabelKeys{}, sel, []string{"v"})
			require.True(t, ok)
			assert.NotContains(t, got, "lun",
				"volume granularity is the reader's rule, never a matcher")
			assert.NotContains(t, got, `az=`)
			assert.NotContains(t, got, `env=`)
			assert.Equal(t, `last_over_time(`+string(q)+`{volume="v"}[1m])`, got)
		}
	})
}

func TestChunkQoSVolumeScope(t *testing.T) {
	t.Parallel()

	t.Run("a scope within budget is one chunk", func(t *testing.T) {
		assert.Equal(t, [][]string{{"a", "b", "c"}},
			ChunkQoSVolumeScope([]string{"a", "b", "c"}, 100))
	})

	t.Run("splits deterministically on the byte budget", func(t *testing.T) {
		in := []string{"aaaa", "bbbb", "cccc", "dddd"}
		// 4 bytes each, +1 separator once a chunk is non-empty: 4+5=9 fits, 14 does not.
		got := ChunkQoSVolumeScope(in, 9)
		assert.Equal(t, [][]string{{"aaaa", "bbbb"}, {"cccc", "dddd"}}, got)
		assert.Equal(t, got, ChunkQoSVolumeScope(in, 9), "pure function of input and budget")
	})

	t.Run("no name is ever dropped", func(t *testing.T) {
		in := []string{"a", "bb", "ccc", "dddd", "eeeee"}
		var flat []string
		for _, c := range ChunkQoSVolumeScope(in, 3) {
			require.NotEmpty(t, c)
			flat = append(flat, c...)
		}
		assert.Equal(t, in, flat)
	})

	t.Run("a single over-budget name gets its own chunk", func(t *testing.T) {
		got := ChunkQoSVolumeScope([]string{"short", "an_extremely_long_flexvol_name"}, 6)
		assert.Equal(t, [][]string{{"short"}, {"an_extremely_long_flexvol_name"}}, got,
			"dropping it would silently cost a claim its measurements")
	})

	t.Run("empty scope chunks to nothing", func(t *testing.T) {
		assert.Nil(t, ChunkQoSVolumeScope(nil, 100))
	})

	t.Run("a non-positive budget is one chunk, never zero chunks", func(t *testing.T) {
		assert.Equal(t, [][]string{{"a", "b"}}, ChunkQoSVolumeScope([]string{"a", "b"}, 0))
	})
}

func TestRenderQoSVolumeScopedRollup(t *testing.T) {
	t.Parallel()

	const matcher = `{volume=~"v_a|v_b"}[5m]`
	volumes := []string{"v_b", "v_a"}
	window := 5 * time.Minute

	t.Run("each rollup changes only the function name", func(t *testing.T) {
		t.Parallel()
		last, ok := RenderQoSVolumeScopedRollup(QQoSReadOps, window, LabelKeys{}, Selector{}, volumes, RollupLast)
		require.True(t, ok)
		maxR, ok := RenderQoSVolumeScopedRollup(QQoSReadOps, window, LabelKeys{}, Selector{}, volumes, RollupMax)
		require.True(t, ok)
		avg, ok := RenderQoSVolumeScopedRollup(QQoSReadOps, window, LabelKeys{}, Selector{}, volumes, RollupAvg)
		require.True(t, ok)
		zero, ok := RenderQoSVolumeScopedRollup(QQoSReadOps, window, LabelKeys{}, Selector{}, volumes, "")
		require.True(t, ok)

		assert.Equal(t, `last_over_time(qos_read_ops`+matcher+`)`, last)
		assert.Equal(t, `max_over_time(qos_read_ops`+matcher+`)`, maxR)
		assert.Equal(t, `avg_over_time(qos_read_ops`+matcher+`)`, avg)
		assert.Equal(t, last, zero, "the zero rollup renders last")
		assert.Equal(t, trimRollupFunc(last), trimRollupFunc(maxR))
		assert.Equal(t, trimRollupFunc(last), trimRollupFunc(avg))
	})

	t.Run("every workload family keeps the same matcher and window", func(t *testing.T) {
		t.Parallel()
		for _, q := range QoSWorkloadQueries {
			for _, roll := range []Rollup{"", RollupLast, RollupMax, RollupAvg} {
				got, ok := RenderQoSVolumeScopedRollup(q, time.Minute, LabelKeys{}, Selector{}, []string{"v"}, roll)
				require.True(t, ok, "%s %q", q, roll)
				assert.Equal(t, roll.rangeFunc()+`(`+string(q)+`{volume="v"}[1m])`, got)
			}
		}
	})

	t.Run("a non-QoS query renders nothing", func(t *testing.T) {
		t.Parallel()
		for _, q := range []Query{QVolumeLabels, QQoSPolicyFixedMaxIOPS, QAggrStatus, QPodInfo} {
			for _, roll := range []Rollup{"", RollupLast, RollupMax, RollupAvg} {
				got, ok := RenderQoSVolumeScopedRollup(q, time.Minute, LabelKeys{}, Selector{}, []string{"v"}, roll)
				assert.False(t, ok, string(q))
				assert.Empty(t, got)
			}
		}
	})

	t.Run("an empty scope renders nothing", func(t *testing.T) {
		t.Parallel()
		for _, roll := range []Rollup{"", RollupMax, RollupAvg} {
			got, ok := RenderQoSVolumeScopedRollup(QQoSReadOps, time.Minute, LabelKeys{}, Selector{}, nil, roll)
			assert.False(t, ok)
			assert.Empty(t, got)
			got, ok = RenderQoSVolumeScopedRollup(QQoSReadOps, time.Minute, LabelKeys{}, Selector{}, []string{"", ""}, roll)
			assert.False(t, ok)
			assert.Empty(t, got)
		}
	})

	t.Run("RenderQoSVolumeScoped matches the last rollup byte for byte", func(t *testing.T) {
		t.Parallel()
		sel := Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Cluster: []string{"c1"}, Namespace: []string{"shop"}}
		keys := LabelKeys{AZ: "zone", Env: "stage"}
		sets := [][]string{
			{"trident_pvc_b", "trident_pvc_a"},
			{"v_a", "", "v_a"},
			nil,
			{"", ""},
		}
		for _, q := range append(append([]Query{}, QoSWorkloadQueries...), QVolumeLabels, QPodInfo) {
			for _, vols := range sets {
				want, wantOK := RenderQoSVolumeScoped(q, window, keys, sel, vols)
				for _, roll := range []Rollup{"", RollupLast} {
					got, ok := RenderQoSVolumeScopedRollup(q, window, keys, sel, vols, roll)
					assert.Equal(t, wantOK, ok, "%s %q", q, roll)
					assert.Equal(t, want, got, "%s %q", q, roll)
				}
			}
		}
	})
}

// trimRollupFunc drops the range-function name so two renders can be compared
// on matcher and window alone.
func trimRollupFunc(q string) string {
	_, rest, _ := strings.Cut(q, "(")
	return rest
}

// The scoped renderer is an ADDITIONAL entry point: Render's own output for the
// QoS families — and therefore render-baseline.txt — is untouched.
func TestRenderQoSVolumeScoped_DoesNotDisturbRender(t *testing.T) {
	t.Parallel()
	for _, q := range QoSWorkloadQueries {
		assert.Equal(t,
			`last_over_time(`+string(q)+`[1m])`,
			Render(q, time.Minute, LabelKeys{}, Selector{}))
	}
}

func TestRenderQoSVolumeScopedDelta(t *testing.T) {
	t.Parallel()

	const matcher = `{volume=~"v_a|v_b"}[5m]`
	volumes := []string{"v_b", "v_a"}
	window := 5 * time.Minute

	t.Run("end minus start per series, one selector twice", func(t *testing.T) {
		t.Parallel()
		for _, q := range QoSWorkloadQueries {
			got, ok := RenderQoSVolumeScopedDelta(q, window, LabelKeys{}, Selector{}, volumes)
			require.True(t, ok, string(q))
			assert.Equal(t,
				`last_over_time(`+string(q)+matcher+`) - last_over_time(`+string(q)+matcher+` offset 5m)`, got)
		}
	})

	t.Run("window and offset both go through FormatDuration", func(t *testing.T) {
		t.Parallel()
		got, ok := RenderQoSVolumeScopedDelta(QQoSReadOps, 90*time.Second, LabelKeys{}, Selector{}, []string{"v"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(qos_read_ops{volume="v"}[90s]) - last_over_time(qos_read_ops{volume="v"}[90s] offset 90s)`, got)
	})

	t.Run("both operands carry the rollup render's matcher set", func(t *testing.T) {
		t.Parallel()
		sel := Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Cluster: []string{"c1"}, Namespace: []string{"shop"}}
		keys := LabelKeys{AZ: "zone", Env: "stage"}
		for _, q := range QoSWorkloadQueries {
			last, ok := RenderQoSVolumeScopedRollup(q, window, keys, sel, volumes, RollupLast)
			require.True(t, ok)
			got, ok := RenderQoSVolumeScopedDelta(q, window, keys, sel, volumes)
			require.True(t, ok)
			assert.Equal(t, last+` - `+strings.TrimSuffix(last, `)`)+` offset 5m)`, got)
			assert.NotContains(t, got, "lun", "volume granularity is the reader's rule, never a matcher")
			// Harvest is routed, not matched: neither the configured zone keys
			// nor the cluster / namespace values reach either operand.
			assert.NotContains(t, got, `zone=`)
			assert.NotContains(t, got, `stage=`)
			assert.NotContains(t, got, `cluster=`)
			assert.NotContains(t, got, `namespace=`)
			assert.NotContains(t, got, "delta(")
			assert.NotContains(t, got, "rate(")
		}
	})

	t.Run("a non-QoS query renders nothing", func(t *testing.T) {
		t.Parallel()
		for _, q := range []Query{QVolumeLabels, QQoSPolicyFixedMaxIOPS, QAggrStatus, QPodInfo} {
			got, ok := RenderQoSVolumeScopedDelta(q, window, LabelKeys{}, Selector{}, volumes)
			assert.False(t, ok, string(q))
			assert.Empty(t, got)
		}
	})

	t.Run("an empty scope renders nothing", func(t *testing.T) {
		t.Parallel()
		for _, vols := range [][]string{nil, {"", ""}} {
			got, ok := RenderQoSVolumeScopedDelta(QQoSReadOps, window, LabelKeys{}, Selector{}, vols)
			assert.False(t, ok)
			assert.Empty(t, got)
		}
	})
}
