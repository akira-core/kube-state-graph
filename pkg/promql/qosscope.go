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
// set of ONTAP FlexVol names. That scope is the query's ONLY matcher — volume
// granularity is enforced by the reader, not here, because the LUN rows a
// `lun=""` matcher would drop are the only ones naming the QoS policy on a SAN
// backend (design.md D11):
//
//	last_over_time(qos_read_ops{volume=~"trident_pvc_a|trident_pvc_b"}[5m])
//
// The values are FlexVol names the volume-object family already returned, so
// the restriction is EXACT: pkg/build's derive-then-match runs once, in Go,
// during scope computation, and its match modes never reach the query layer.
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
func RenderQoSVolumeScoped(q Query, window time.Duration, volumes []string) (string, bool) {
	if !isQoSWorkloadQuery(q) {
		return "", false
	}
	vals := normaliseValues(volumes)
	if len(vals) == 0 {
		return "", false
	}
	matchers := appendMatcher(nil, HarvestVolumeLabel, vals)
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		q, strings.Join(matchers, ","), FormatDuration(window)), true
}

// ChunkQoSVolumeScope splits a sorted, de-duplicated FlexVol scope into
// chunks whose rendered alternation fits the byte budget. It is ChunkScope
// under the name the QoS workload read was introduced with.
func ChunkQoSVolumeScope(volumes []string, budget int) [][]string {
	return ChunkScope(volumes, budget)
}
