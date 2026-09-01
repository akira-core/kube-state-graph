package promql

import (
	"fmt"
	"regexp"
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
// set of ONTAP FlexVol names, composed with — never replacing — the family's
// fixed `lun=""` volume-granularity contract:
//
//	last_over_time(qos_read_ops{lun="",volume=~"trident_pvc_a|trident_pvc_b"}[5m])
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
	matchers := appendMatcher([]string{qosVolumeGranularitySelector}, HarvestVolumeLabel, vals)
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		q, strings.Join(matchers, ","), FormatDuration(window)), true
}

// ChunkQoSVolumeScope splits a sorted, de-duplicated scope into chunks whose
// rendered alternation fits the byte budget, so a large estate is read through
// several narrow queries instead of one query the upstream would reject for
// length (`-search.maxQueryLen`).
//
// The split is a pure function of the input slice and the budget, so which
// claims share a chunk — and therefore which claims a failing chunk costs their
// measurements — is deterministic across rebuilds.
//
// A single name longer than the budget still gets its own chunk rather than
// being dropped: a silently missing claim is precisely the failure mode this
// capability exists to remove, and an over-length query that upstream rejects
// degrades one chunk, visibly.
func ChunkQoSVolumeScope(volumes []string, budget int) [][]string {
	if len(volumes) == 0 {
		return nil
	}
	if budget <= 0 {
		return [][]string{volumes}
	}
	var (
		out  [][]string
		cur  []string
		used int
	)
	for _, v := range volumes {
		// The rendered cost of this value: its escaped form plus the `|`
		// separator it needs once it is not first in the chunk.
		cost := len(escapeLiteral(regexp.QuoteMeta(v)))
		if len(cur) > 0 {
			cost++
		}
		if len(cur) > 0 && used+cost > budget {
			out = append(out, cur)
			cur, used = nil, 0
			cost = len(escapeLiteral(regexp.QuoteMeta(v)))
		}
		cur = append(cur, v)
		used += cost
	}
	return append(out, cur)
}
