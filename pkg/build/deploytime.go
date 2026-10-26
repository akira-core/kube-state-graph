package build

import (
	"regexp"
	"time"
)

// slashDeployTime is the fixed-width UTC form YYYY/MM/DD hh:mm:ss. The
// pre-check is load-bearing: Go's reference hour "15" accepts a one-digit
// hour, and Parse accepts a fractional second the layout does not show.
var slashDeployTime = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}$`)

// rfc3339DeployTime is the RFC 3339 date-time shape. It is load-bearing for
// the same reason: time.Parse(time.RFC3339, …) falls back to the lenient
// layout parser, which accepts a one-digit hour, a comma before the fraction
// and an offset hour of 24, none of which RFC 3339 allows.
var rfc3339DeployTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$`)

// parseDeployTime accepts the slash form, read as UTC, or RFC 3339 with its
// own offset. The result is UTC, truncated to the whole second. Any other
// value, including an invalid calendar date, is unresolved.
func parseDeployTime(v string) (time.Time, bool) {
	var (
		parsed time.Time
		err    error
	)
	switch {
	case slashDeployTime.MatchString(v):
		parsed, err = time.ParseInLocation("2006/01/02 15:04:05", v, time.UTC)
	case rfc3339DeployTime.MatchString(v):
		parsed, err = time.Parse(time.RFC3339, v)
	default:
		return time.Time{}, false
	}
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC().Truncate(time.Second), true
}
