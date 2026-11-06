package promql

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// QoSWorkloadQueries are the six Harvest QoS workload families the storage
// join reads its I/O measurements from. They are the ONLY queries issued
// scope-restricted: every other Harvest leg is read unfiltered, because the set
// of interesting FlexVol names is not known until the volume-object family has
// been read.
var QoSWorkloadQueries = []Query{
	QQoSReadOps, QQoSWriteOps, QQoSReadLatency, QQoSWriteLatency,
	QQoSReadData, QQoSWriteData,
}

func isQoSWorkloadQuery(q Query) bool {
	return slices.Contains(QoSWorkloadQueries, q)
}

// RenderQoSVolumeScoped renders one QoS workload query restricted to a known
// set of ONTAP FlexVol names. keys and sel render no matcher on a Harvest
// query — az routes the family and is not a matcher — so the volume scope is
// the query's ONLY matcher. Volume granularity is enforced by the reader, not
// here, because the LUN rows a `lun=""` matcher would drop are the only ones
// naming the QoS policy on a SAN backend:
//
//	last_over_time(qos_read_ops{volume=~"trident_pvc_a|trident_pvc_b"}[5m])
//
// This function always renders last_over_time. RenderQoSVolumeScopedRollup
// takes the rollup explicitly; this one delegates to it with the zero value.
//
// The values are FlexVol names the volume-object family already returned, so
// the restriction is EXACT: pkg/build's derive-then-suffix runs once, in Go,
// during scope computation, and the comparison never reaches the query layer.
// PromQL anchors `=~` as ^(?:...)$, and each value is QuoteMeta-escaped, so a
// name containing a regex metacharacter still matches itself and nothing else.
//
// The `volume` restriction is derived from UPSTREAM DATA, not from the request:
// it is not a request-scoped matcher and queryDims is unchanged. It is
// nonetheless influenced by the request, because the claims whose tokens
// produced those names are themselves loaded under the request's selectors —
// this capability's "narrowed by reference" rule realised at the query layer.
//
// ok is false when volumes is empty or q is not a QoS workload family. The
// caller MUST skip the query rather than fall back to an unscoped read: an
// empty scope means no claim matched anything, so the unrestricted read could
// only fetch series the reader would discard.
func RenderQoSVolumeScoped(q Query, window time.Duration, keys LabelKeys, sel Selector, volumes []string) (string, bool) {
	return RenderQoSVolumeScopedRollup(q, window, keys, sel, volumes, "")
}

// RenderQoSVolumeScopedRollup is RenderQoSVolumeScoped with the range
// function chosen by rollup. The matcher set and the window are identical
// for every rollup; only the function name changes. rollup's zero value and
// RollupLast both render last_over_time. Each series is reduced by that
// function over the whole window before any per-claim sum.
//
// ok is false on the same inputs as RenderQoSVolumeScoped: q is not a QoS
// workload family, or volumes normalises to nothing.
func RenderQoSVolumeScopedRollup(q Query, window time.Duration, keys LabelKeys, sel Selector, volumes []string, rollup Rollup) (string, bool) {
	selector, ok := qosVolumeSelector(q, window, keys, sel, volumes)
	if !ok {
		return "", false
	}
	return fmt.Sprintf(`%s(%s)`, rollup.rangeFunc(), selector), true
}

// RenderQoSVolumeScopedDelta renders the end-minus-start change of one QoS
// workload query over the same FlexVol scope and matcher set as the rollup
// render:
//
//	last_over_time(qos_read_ops{volume=~"v_a|v_b"}[5m]) - last_over_time(qos_read_ops{volume=~"v_a|v_b"}[5m] offset 5m)
//
// The left operand is each series' last sample in (end-w, end]; the right is
// its last sample in (start-w, start], the value a plain last-rollup request
// for the window ending at start would read. The two operands carry identical
// label sets apart from __name__, which arithmetic matching ignores, so the
// subtraction is one-to-one and a series with no start sample drops out: its
// delta is absent, never zero. It is deliberately not PromQL delta(), which
// extrapolates in Prometheus and reads the sample before the window only
// within the staleness interval in VictoriaMetrics, and not rate-of-a-rate.
// It stays standard PromQL — two rollups of ONE selector, never a join of two
// families — so any Prometheus-compatible store serves it. The window is
// rendered twice, so the result scans twice the window.
//
// ok is false on the same inputs as RenderQoSVolumeScopedRollup.
func RenderQoSVolumeScopedDelta(q Query, window time.Duration, keys LabelKeys, sel Selector, volumes []string) (string, bool) {
	selector, ok := qosVolumeSelector(q, window, keys, sel, volumes)
	if !ok {
		return "", false
	}
	return fmt.Sprintf(`last_over_time(%s) - last_over_time(%s offset %s)`,
		selector, selector, FormatDuration(window)), true
}

// qosVolumeSelector renders `<family>{<matchers>}[<window>]`, the one body both
// the rollup and the delta render share. ok is false when q is not a QoS
// workload family or volumes normalises to nothing.
func qosVolumeSelector(q Query, window time.Duration, keys LabelKeys, sel Selector, volumes []string) (string, bool) {
	if !isQoSWorkloadQuery(q) {
		return "", false
	}
	vals := normaliseValues(volumes)
	if len(vals) == 0 {
		return "", false
	}
	matchers := appendMatcher(requestMatchers(q, keys, sel), HarvestVolumeLabel, vals)
	return fmt.Sprintf(`%s{%s}[%s]`, q, strings.Join(matchers, ","), FormatDuration(window)), true
}

// ChunkQoSVolumeScope splits a sorted, de-duplicated FlexVol scope into
// chunks whose rendered alternation fits the byte budget and holds at most
// MaxAlternationValues names. It is ChunkScope
// under the name the QoS workload read was introduced with.
func ChunkQoSVolumeScope(volumes []string, budget int) [][]string {
	return ChunkScope(volumes, budget)
}
