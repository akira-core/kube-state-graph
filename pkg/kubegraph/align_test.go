package kubegraph

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAlignWindow(t *testing.T) {
	cases := []struct {
		name               string
		start, end         string
		grid               time.Duration
		wantStart, wantEnd string
	}{
		{"floors end keeping window", "2026-05-02T12:04:17Z", "2026-05-02T12:19:47Z", 30 * time.Second, "2026-05-02T12:04:00Z", "2026-05-02T12:19:30Z"},
		{"sub-grid window keeps its length", "2026-05-02T12:19:40Z", "2026-05-02T12:19:50Z", 30 * time.Second, "2026-05-02T12:19:20Z", "2026-05-02T12:19:30Z"},
		{"already aligned is identity", "2026-05-02T12:00:00Z", "2026-05-02T12:19:30Z", 30 * time.Second, "2026-05-02T12:00:00Z", "2026-05-02T12:19:30Z"},
		{"zero grid is identity", "2026-05-02T12:04:17Z", "2026-05-02T12:19:47Z", 0, "2026-05-02T12:04:17Z", "2026-05-02T12:19:47Z"},
		{"negative grid is identity", "2026-05-02T12:04:17Z", "2026-05-02T12:19:47Z", -time.Second, "2026-05-02T12:04:17Z", "2026-05-02T12:19:47Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, e := AlignWindow(mustTime(t, tc.start), mustTime(t, tc.end), tc.grid)
			assert.Equal(t, mustTime(t, tc.wantStart), s)
			assert.Equal(t, mustTime(t, tc.wantEnd), e)
		})
	}
}

// A grid that does not divide the zero-time → epoch offset proves the floor is
// counted from the Unix epoch (time.Truncate would count from year 1).
func TestAlignWindow_EpochAnchored(t *testing.T) {
	end := time.Unix(1000, 0).UTC()
	_, got := AlignWindow(end.Add(-time.Minute), end, 7*time.Second)
	assert.Equal(t, int64(994), got.Unix(), "1000 floored to a multiple of 7 from the epoch")
}

func TestAlignWindow_TwoEndsInOneStepAgree(t *testing.T) {
	_, a := AlignWindow(mustTime(t, "2026-05-02T12:04:31Z"), mustTime(t, "2026-05-02T12:19:31Z"), 30*time.Second)
	_, b := AlignWindow(mustTime(t, "2026-05-02T12:04:59Z"), mustTime(t, "2026-05-02T12:19:59Z"), 30*time.Second)
	assert.Equal(t, a, b)
}
