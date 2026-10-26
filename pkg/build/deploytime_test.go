package build

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseDeployTime(t *testing.T) {
	t.Parallel()
	utc := func(y int, m time.Month, d, hh, mm, ss int) time.Time {
		return time.Date(y, m, d, hh, mm, ss, 0, time.UTC)
	}
	tests := []struct {
		name string
		raw  string
		want time.Time
		ok   bool
	}{
		{name: "slash form is UTC", raw: "2025/10/08 13:14:15", want: utc(2025, 10, 8, 13, 14, 15), ok: true},
		{name: "RFC 3339 with offset and a fraction", raw: "2025-10-08T13:14:15.987+08:00", want: utc(2025, 10, 8, 5, 14, 15), ok: true},
		{name: "one-digit hour", raw: "2025/10/08 9:14:15"},
		{name: "slash form with a fraction", raw: "2025/10/08 13:14:15.5"},
		{name: "leading space", raw: " 2025/10/08 13:14:15"},
		{name: "unix seconds", raw: "1759929255"},
		{name: "invalid calendar date", raw: "2025/02/30 00:00:00"},
		{name: "RFC 3339 with a one-digit hour", raw: "2025-10-08T1:14:15Z"},
		{name: "RFC 3339 with a comma fraction", raw: "2025-10-08T13:14:15,5Z"},
		{name: "RFC 3339 with an offset hour of 24", raw: "2025-10-08T13:14:15+24:00"},
		{name: "RFC 3339 with an invalid calendar date", raw: "2025-02-30T00:00:00Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseDeployTime(tc.raw)
			assert.Equal(t, tc.ok, ok)
			assert.True(t, got.Equal(tc.want))
		})
	}
}
