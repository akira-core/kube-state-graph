package promql

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// AggrPairQueries are the three aggregate-gauge families keyed by
// (ONTAP cluster, aggregate).
var AggrPairQueries = []Query{QAggrStatus, QAggrSpaceUsed, QAggrSpaceTotal}

// NetAppNodePairQueries are the six controller families keyed by
// (ONTAP cluster, controller).
var NetAppNodePairQueries = []Query{
	QNetAppNodeStatus, QNetAppNodeLabels, QNetAppNodeCPUBusy,
	QNetAppNodeTotalOps, QNetAppNodeTotalLatency, QNetAppNodeTotalData,
}

// PolicyPairQueries are the two fixed-policy families keyed by
// (ONTAP cluster, SVM).
var PolicyPairQueries = []Query{QQoSPolicyFixedMaxIOPS, QQoSPolicyFixedMaxMBps}

// HarvestPairQueries is every family RenderHarvestPair accepts.
var HarvestPairQueries = slices.Concat(AggrPairQueries, NetAppNodePairQueries, PolicyPairQueries)

// harvestPairLabel is the second label of a pair-keyed family. The first is
// always the ONTAP cluster, rendered as an equality by RenderHarvestPair.
func harvestPairLabel(q Query) (string, bool) {
	switch q {
	case QAggrStatus, QAggrSpaceUsed, QAggrSpaceTotal:
		return volumeLabelsAggrLabel, true
	case QNetAppNodeStatus, QNetAppNodeLabels, QNetAppNodeCPUBusy,
		QNetAppNodeTotalOps, QNetAppNodeTotalLatency, QNetAppNodeTotalData:
		return NodeLabel, true
	case QQoSPolicyFixedMaxIOPS, QQoSPolicyFixedMaxMBps:
		return volumeLabelsSVMLabel, true
	default:
		return "", false
	}
}

// RenderHarvestPair renders one pair-keyed Harvest family restricted to names
// of a single ONTAP cluster:
//
//	last_over_time(aggr_space_used{az="zone-a",cluster="ontap-prod",aggr=~"aggr1|aggr2"}[5m])
//
// The cluster equality is always rendered, including when cluster is empty, so
// a query never spans filers and a series carrying no cluster label still
// matches its own bucket. Request matchers (az / env) precede the pair. These
// families have no fixed selector.
//
// ok is false when q is not a pair-keyed family or names holds no non-empty
// value. The caller MUST skip the query rather than fall back to an unscoped
// read.
func RenderHarvestPair(q Query, window time.Duration, keys LabelKeys, sel Selector, cluster string, names []string) (string, bool) {
	label, ok := harvestPairLabel(q)
	if !ok {
		return "", false
	}
	vals := normaliseValues(names)
	if len(vals) == 0 {
		return "", false
	}
	matchers := append(requestMatchers(q, keys, sel),
		VolumeLabelsClusterLabel+`="`+escapeLiteral(cluster)+`"`)
	matchers = appendMatcher(matchers, label, vals)
	return renderHarvest(q, window, matchers), true
}

// RenderHarvestByName renders a pair-keyed family restricted on its name
// label alone, with no cluster matcher:
//
//	last_over_time(aggr_new_status{az="zone-a",aggr=~"aggr9"}[5m])
//
// A bare aggr= or ontap_node= root names a component on every filer, and the
// cluster is not known until the rows come back. ok is false when q is not
// pair-keyed or names is empty.
func RenderHarvestByName(q Query, window time.Duration, keys LabelKeys, sel Selector, names []string) (string, bool) {
	label, ok := harvestPairLabel(q)
	if !ok {
		return "", false
	}
	return renderHarvestRestricted(q, window, keys, sel, label, names)
}

// RenderHarvestByCluster renders a pair-keyed family restricted on the ONTAP
// cluster alone, so one filer's whole aggregate or controller inventory is
// read:
//
//	last_over_time(node_new_status{az="zone-a",cluster=~"ontap-a|ontap-b"}[5m])
//
// ok is false when q is not pair-keyed or clusters is empty.
func RenderHarvestByCluster(q Query, window time.Duration, keys LabelKeys, sel Selector, clusters []string) (string, bool) {
	if _, ok := harvestPairLabel(q); !ok {
		return "", false
	}
	return renderHarvestRestricted(q, window, keys, sel, VolumeLabelsClusterLabel, clusters)
}

func renderHarvestRestricted(q Query, window time.Duration, keys LabelKeys, sel Selector, label string, values []string) (string, bool) {
	if len(normaliseValues(values)) == 0 {
		return "", false
	}
	return renderHarvest(q, window, appendMatcher(requestMatchers(q, keys, sel), label, values)), true
}

func renderHarvest(q Query, window time.Duration, matchers []string) string {
	return fmt.Sprintf(`last_over_time(%s{%s}[%s])`,
		q, strings.Join(matchers, ","), FormatDuration(window))
}

// HarvestNameChunk is one ONTAP cluster's slice of a pair-keyed scope: the
// names that share one rendered query.
type HarvestNameChunk struct {
	Cluster string
	Names   []string
}

// ChunkHarvestPairs groups names by ONTAP cluster and splits each cluster so
// the rendered query fits budget, after the repeated request matchers and the
// repeated cluster equality are charged at their rendered length (the
// owner-completion precedent). Clusters are visited in sorted order. Names are
// de-duplicated and sorted before the split, so the chunks are a pure function
// of the name set.
//
// Every chunk names exactly one cluster. budget <= 0 yields one chunk per
// cluster holding all of its names. A positive budget that the repeated
// matchers already exhaust still issues each name (the remainder passed to
// ChunkScope is at least 1), and a single name longer than that remainder is
// issued alone, never dropped. A cluster whose names normalise to nothing is
// omitted.
//
// q must be a pair-keyed family. Anything else, and an empty byCluster,
// returns nil.
func ChunkHarvestPairs(q Query, keys LabelKeys, sel Selector, byCluster map[string][]string, budget int) []HarvestNameChunk {
	if _, ok := harvestPairLabel(q); !ok || len(byCluster) == 0 {
		return nil
	}
	request := RequestMatcherCost(q, keys, sel)
	var out []HarvestNameChunk
	for _, cluster := range slices.Sorted(maps.Keys(byCluster)) {
		names := normaliseValues(byCluster[cluster])
		if len(names) == 0 {
			continue
		}
		remain := budget
		if budget > 0 {
			remain = max(budget-request-OwnerCompletionClusterCost(cluster), 1)
		}
		for _, chunk := range ChunkScope(names, remain) {
			out = append(out, HarvestNameChunk{Cluster: cluster, Names: chunk})
		}
	}
	return out
}
