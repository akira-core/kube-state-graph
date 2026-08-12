package build

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Standard OTel servicegraph-ish bucket set used by the interpolation fixture.
// Boundaries in seconds; cumulative counts chosen so q=0.90 lands mid-bucket.
func otelBuckets() []bucket {
	// Cumulative: 10 @ 0.01, 30 @ 0.05, 50 @ 0.1, 80 @ 0.5, 100 @ +Inf
	// total=100 → rank for q=0.90 is 90 → lands in (0.1, 0.5] between count 50 and 80.
	return []bucket{
		{le: 0.01, count: 10},
		{le: 0.05, count: 30},
		{le: 0.1, count: 50},
		{le: 0.5, count: 80},
		{le: 1.0, count: 95},
		{le: math.Inf(1), count: 100},
	}
}

func TestClassicQuantile_InterpolationAtP90(t *testing.T) {
	// rank = 0.90 * 100 = 90; bucket (0.1, 0.5] has counts 50→80.
	// 90 is past 80, so next bucket is (0.5, 1.0] with counts 80→95.
	// frac = (90-80)/(95-80) = 10/15 = 2/3
	// value = 0.5 + (2/3)*(1.0-0.5) = 0.5 + 1/3 ≈ 0.8333...
	got, ok := classicQuantile(0.90, otelBuckets())
	require.True(t, ok)
	assert.InDelta(t, 0.5+(2.0/3.0)*0.5, got, 1e-9)
}

func TestClassicQuantile_InfClamp(t *testing.T) {
	// All mass in the last finite bucket → p90 still finite.
	// Put enough mass so p90 lands in +Inf: count at highest finite < rank.
	bs := []bucket{
		{le: 0.1, count: 10},
		{le: 1.0, count: 50}, // highest finite
		{le: math.Inf(1), count: 100},
	}
	// rank = 90; 50 < 90 so lands in +Inf → clamp to 1.0
	got, ok := classicQuantile(0.90, bs)
	require.True(t, ok)
	assert.InDelta(t, 1.0, got, 1e-12)
}

func TestClassicQuantile_Median(t *testing.T) {
	// Keep the q parameter live for unparam; p50 lands mid-bucket.
	bs := []bucket{
		{le: 0.1, count: 20},
		{le: 0.5, count: 80},
		{le: math.Inf(1), count: 100},
	}
	// rank = 0.5 * 100 = 50; bucket (0.1, 0.5] counts 20→80
	// frac = (50-20)/(80-20) = 0.5 → 0.1 + 0.5*0.4 = 0.3
	got, ok := classicQuantile(0.50, bs)
	require.True(t, ok)
	assert.InDelta(t, 0.3, got, 1e-12)
}

func TestClassicQuantile_SingleBucket(t *testing.T) {
	// Fewer than two boundaries → ok=false.
	_, ok := classicQuantile(0.90, []bucket{{le: math.Inf(1), count: 10}})
	assert.False(t, ok)
}

func TestClassicQuantile_EmptyInput(t *testing.T) {
	_, ok := classicQuantile(0.90, nil)
	assert.False(t, ok)
	_, ok = classicQuantile(0.90, []bucket{})
	assert.False(t, ok)
}

func TestClassicQuantile_MissingInf(t *testing.T) {
	_, ok := classicQuantile(0.90, []bucket{
		{le: 0.1, count: 5},
		{le: 1.0, count: 10},
	})
	assert.False(t, ok)
}

func TestClassicQuantile_ZeroTotal(t *testing.T) {
	_, ok := classicQuantile(0.90, []bucket{
		{le: 0.1, count: 0},
		{le: math.Inf(1), count: 0},
	})
	assert.False(t, ok)
}

func TestClassicQuantile_UnsortedInput(t *testing.T) {
	// Same multiset as otelBuckets but shuffled — result must match.
	shuffled := []bucket{
		{le: 0.5, count: 80},
		{le: math.Inf(1), count: 100},
		{le: 0.01, count: 10},
		{le: 1.0, count: 95},
		{le: 0.1, count: 50},
		{le: 0.05, count: 30},
	}
	want, ok1 := classicQuantile(0.90, otelBuckets())
	got, ok2 := classicQuantile(0.90, shuffled)
	require.True(t, ok1)
	require.True(t, ok2)
	assert.InDelta(t, want, got, 1e-12, "quantile must be sort-order independent")
}

func TestParseLe(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"0.1", 0.1, true},
		{"+Inf", math.Inf(1), true},
		{"Inf", math.Inf(1), true},
		{"-Inf", math.Inf(-1), true},
		{"", 0, false},
		{"vmrange", 0, false},
		{"abc", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseLe(tc.in)
		assert.Equal(t, tc.ok, ok, "parseLe(%q)", tc.in)
		if tc.ok {
			if math.IsInf(tc.want, 0) {
				assert.True(t, math.IsInf(got, int(math.Copysign(1, tc.want))))
			} else {
				assert.InDelta(t, tc.want, got, 1e-12)
			}
		}
	}
}
