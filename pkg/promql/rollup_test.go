package promql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRollup(t *testing.T) {
	t.Parallel()

	t.Run("accepted values", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			in   string
			want Rollup
		}{
			{in: "last", want: RollupLast},
			{in: "max", want: RollupMax},
			{in: "avg", want: RollupAvg},
			{in: "", want: RollupLast},
		}
		for _, tc := range cases {
			got, err := ParseRollup(tc.in)
			require.NoError(t, err, "input %q", tc.in)
			assert.Equal(t, tc.want, got, "input %q", tc.in)
		}
	})

	t.Run("rejected values", func(t *testing.T) {
		t.Parallel()
		for _, in := range []string{"p99", "MAX", "sum", " max"} {
			got, err := ParseRollup(in)
			require.Error(t, err, "input %q", in)
			assert.Equal(t, Rollup(""), got, "input %q", in)
			assert.EqualError(t, err, InvalidRollupMessage("rollup", in), "input %q", in)
		}
	})
}

// Every layer that rejects a rollup words it through InvalidRollupMessage, so
// the accepted values are listed in one place.
func TestInvalidRollupMessage(t *testing.T) {
	t.Parallel()

	assert.Equal(t, `io_rollup must be one of last, max, avg, got "p99"`,
		InvalidRollupMessage("io_rollup", "p99"))
	assert.Equal(t, `rollup must be one of last, max, avg, got "max,avg"`,
		InvalidRollupMessage("rollup", "max,avg"))
}

func TestRollupValid(t *testing.T) {
	t.Parallel()

	for _, r := range []Rollup{RollupLast, RollupMax, RollupAvg, ""} {
		assert.True(t, r.Valid(), "rollup %q", r)
	}
	for _, r := range []Rollup{"p99", "MAX", "sum", " max", "Last", "last "} {
		assert.False(t, r.Valid(), "rollup %q", r)
	}
}

func TestRollupRangeFunc(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "last_over_time", RollupLast.rangeFunc())
	assert.Equal(t, "max_over_time", RollupMax.rangeFunc())
	assert.Equal(t, "avg_over_time", RollupAvg.rangeFunc())
	assert.Equal(t, "last_over_time", Rollup("").rangeFunc())
}
