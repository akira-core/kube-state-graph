package promql

import (
	"errors"
	"fmt"
)

// Rollup chooses the PromQL range function that reduces each Harvest QoS
// workload series over the request window. The reduction is per series and
// happens upstream, before the reader discards LUN rows, picks one zone per
// workload, or sums. last is the sample at the window end. max is that
// series' own peak, so a later sum of peaks is an upper bound on the peak of
// the summed series. avg is the unweighted mean of the samples the series
// actually has: a gap is not counted as zero, and a latency mean is not
// weighted by ops. Ceilings, gauges and every family other than the six QoS
// workload queries stay on last. A /v1/graph build always uses last; only a
// /v1/storage-graph build carries the caller's choice.
//
// The zero value means last, the same reduction as RollupLast.
type Rollup string

const (
	// RollupLast reduces each series to its value at the window end
	// (last_over_time).
	RollupLast Rollup = "last"
	// RollupMax reduces each series to its own peak in the window
	// (max_over_time).
	RollupMax Rollup = "max"
	// RollupAvg reduces each series to the unweighted mean of its own
	// samples (avg_over_time).
	RollupAvg Rollup = "avg"
)

// rollupChoices lists the accepted values in the order every rejection names
// them. Adding a rollup means adding it here, to ParseRollup, to Valid and to
// rangeFunc.
const rollupChoices = "last, max, avg"

// InvalidRollupMessage is the one wording every layer uses to reject a rollup
// value. param names the caller's surface — the wire parameter `io_rollup` for
// the request parser and the build, `rollup` for ParseRollup — and got is the
// rejected value as the caller saw it.
func InvalidRollupMessage(param, got string) string {
	return fmt.Sprintf("%s must be one of %s, got %q", param, rollupChoices, got)
}

// ParseRollup accepts the literals last, max and avg, and the empty string.
// The empty string returns RollupLast. Every other value, including a
// different case or surrounding space, is rejected.
func ParseRollup(s string) (Rollup, error) {
	switch Rollup(s) {
	case "", RollupLast:
		return RollupLast, nil
	case RollupMax, RollupAvg:
		return Rollup(s), nil
	default:
		return "", errors.New(InvalidRollupMessage("rollup", s))
	}
}

// Valid reports whether r is RollupLast, RollupMax, RollupAvg, or the zero
// value. The zero value is valid and means last. ParseRollup turns a string
// into a Rollup; Valid is the check a typed caller makes before any query.
func (r Rollup) Valid() bool {
	switch r {
	case "", RollupLast, RollupMax, RollupAvg:
		return true
	default:
		return false
	}
}

// rangeFunc is the one mapping from a rollup to the PromQL range function
// that reduces each QoS series. The zero value maps to last_over_time, the
// same as RollupLast. Callers pass a value Valid accepts; the build rejects
// any other value before a query is rendered.
func (r Rollup) rangeFunc() string {
	switch r {
	case RollupMax:
		return "max_over_time"
	case RollupAvg:
		return "avg_over_time"
	default:
		return "last_over_time"
	}
}
