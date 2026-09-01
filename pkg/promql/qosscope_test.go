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
		got, ok := RenderQoSVolumeScoped(QQoSReadOps, time.Minute, []string{"trident_pvc_a"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(qos_read_ops{lun="",volume="trident_pvc_a"}[1m])`, got)
	})

	t.Run("several names render one anchored alternation", func(t *testing.T) {
		got, ok := RenderQoSVolumeScoped(QQoSWriteData, 5*time.Minute,
			[]string{"trident_pvc_b", "trident_pvc_a"})
		require.True(t, ok)
		assert.Equal(t,
			`last_over_time(qos_write_data{lun="",volume=~"trident_pvc_a|trident_pvc_b"}[5m])`, got,
			"values are sorted, so the rendered string is a pure function of the set")
	})

	t.Run("duplicates and empties are normalised away", func(t *testing.T) {
		got, ok := RenderQoSVolumeScoped(QQoSReadOps, time.Minute,
			[]string{"v_a", "", "v_a"})
		require.True(t, ok)
		assert.Equal(t, `last_over_time(qos_read_ops{lun="",volume="v_a"}[1m])`, got)
	})

	t.Run("a metacharacter in a name matches itself literally", func(t *testing.T) {
		got, ok := RenderQoSVolumeScoped(QQoSReadOps, time.Minute, []string{"vol.a", "vol_b"})
		require.True(t, ok)
		assert.Contains(t, got, `volume=~"vol\\.a|vol_b"`,
			"QuoteMeta then string-escape, so the parser unquotes into the regex vol\\.a")
	})

	t.Run("an empty scope renders nothing at all", func(t *testing.T) {
		// Never an unscoped fallback: an empty scope means no claim matched, so
		// the unrestricted read could only fetch series the reader discards.
		_, ok := RenderQoSVolumeScoped(QQoSReadOps, time.Minute, nil)
		assert.False(t, ok)
		_, ok = RenderQoSVolumeScoped(QQoSReadOps, time.Minute, []string{"", ""})
		assert.False(t, ok)
	})

	t.Run("only the six workload families are scopeable", func(t *testing.T) {
		for _, q := range QoSWorkloadQueries {
			_, ok := RenderQoSVolumeScoped(q, time.Minute, []string{"v"})
			assert.True(t, ok, string(q))
		}
		for _, q := range []Query{QVolumeLabels, QQoSPolicyFixedMaxIOPS, QAggrStatus, QPodInfo} {
			_, ok := RenderQoSVolumeScoped(q, time.Minute, []string{"v"})
			assert.False(t, ok, string(q))
		}
	})

	t.Run("the fixed lun contract is composed, never replaced", func(t *testing.T) {
		for _, q := range QoSWorkloadQueries {
			got, ok := RenderQoSVolumeScoped(q, time.Minute, []string{"v"})
			require.True(t, ok)
			assert.Contains(t, got, `lun=""`)
			assert.Less(t, strings.Index(got, `lun=""`), strings.Index(got, "volume"),
				"the request-invariant selector is rendered first")
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

// The scoped renderer is an ADDITIONAL entry point: Render's own output for the
// QoS families — and therefore render-baseline.txt — is untouched.
func TestRenderQoSVolumeScoped_DoesNotDisturbRender(t *testing.T) {
	t.Parallel()
	for _, q := range QoSWorkloadQueries {
		assert.Equal(t,
			`last_over_time(`+string(q)+`{lun=""}[1m])`,
			Render(q, time.Minute, LabelKeys{}, Selector{}))
	}
}
