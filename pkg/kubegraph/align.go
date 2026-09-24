package kubegraph

import "time"

// DefaultEndAlign is the end-time alignment grid applied when Options.EndAlign
// is zero. The server's --end-align default reads this constant.
const DefaultEndAlign = 30 * time.Second

// AlignWindow floors end to the largest multiple of grid, counted from the Unix
// epoch, that is not after it, and shifts start earlier by the same amount, so
// the window length end-start is unchanged. Requests issued within one grid
// step therefore render identical upstream queries evaluated at one instant,
// which is what lets the upstream query-result cache serve them. grid <= 0
// returns start and end unchanged.
//
// Callers align AFTER validating start/end, so validation errors always refer
// to the caller's own values.
func AlignWindow(start, end time.Time, grid time.Duration) (time.Time, time.Time) {
	if grid <= 0 {
		return start, end
	}
	ns := end.UnixNano()
	g := int64(grid)
	rem := ns % g
	if rem < 0 {
		rem += g // floor, not truncation toward zero, for pre-epoch instants
	}
	delta := time.Duration(rem)
	return start.Add(-delta), end.Add(-delta)
}

// resolveDuration applies the Options convention: zero ⇒ def, negative ⇒
// disabled (0), positive ⇒ the value.
func resolveDuration(v, def time.Duration) time.Duration {
	switch {
	case v == 0:
		return def
	case v < 0:
		return 0
	default:
		return v
	}
}

// resolveInt is resolveDuration for integer knobs.
func resolveInt(v, def int) int {
	switch {
	case v == 0:
		return def
	case v < 0:
		return 0
	default:
		return v
	}
}
