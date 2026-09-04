package build

import (
	"context"
	"log/slog"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// Label names the ALERTS reader consumes. `alertstate` is re-tested against
// graph.AlertStateFiring even though the query's fixed selector already
// restricts the read to firing alerts: every fixed selector mirrors a discard
// its reader performs, so the pushdown stays output-preserving for a vector
// that did not come through the query (a hand-built Topology, an embedder).
const (
	alertNameLabel      = "alertname"
	alertStateLabel     = "alertstate"
	alertSeverityLabel  = "severity"
	alertNamespaceLabel = "namespace"
	alertPodLabel       = "pod"
	alertPVCLabel       = "persistentvolumeclaim"
	alertNodeLabel      = "node"
	alertAggrLabel      = "aggr"
	alertClusterLabel   = "cluster"
)

// Index keys. Each kind is indexed twice — once cluster-qualified and once by
// bare name — because an alert may or may not carry a usable `cluster` label
// and the two cases resolve by different rules (identity match vs. uniqueness
// in the loaded estate).
type (
	alertNSKey   struct{ cluster, namespace, name string }
	alertNameKey struct{ namespace, name string }
	alertOCKey   struct{ oc, name string }
)

// alertIndex is the assembled node set, keyed every way the matcher looks a
// node up. It is built ONCE per build from the final node slice — after the
// service-graph read has contributed its synth pods — so "the pods loaded in
// the window" means exactly what the response will contain.
//
// The bare-name maps hold SLICES rather than single nodes on purpose: an alert
// with no `cluster` label resolves only when exactly one node of the eligible
// kind carries the remaining labels, so the matcher has to be able to see that
// there were two.
type alertIndex struct {
	podsByCluster map[alertNSKey]string
	podsByName    map[alertNameKey][]string

	pvcsByCluster map[alertNSKey]string
	pvcsByName    map[alertNameKey][]string

	k8sNodesByCluster map[alertOCKey]string
	k8sNodesByName    map[string][]string

	ctrlsByCluster map[alertOCKey]string
	ctrlsByName    map[string][]string

	aggrsByCluster map[alertOCKey]string
	aggrsByName    map[string][]string
}

// newAlertIndex builds the lookup structure from the assembled node set.
//
// Only the five alert-carrying kinds are indexed. Services, externals and SVMs
// are deliberately absent: no alert label set names them, and indexing them
// would let a `{namespace, pod}` alert collide with a same-named Service (whose
// id mirrors PVC keying) and attach to the wrong node.
//
// A node whose own identifying label is missing — a synth pod with no cluster
// or namespace — contributes nothing, so it can neither match nor make a
// bare-name lookup look ambiguous.
func newAlertIndex(nodes []graph.GraphNode) alertIndex {
	idx := alertIndex{
		podsByCluster:     map[alertNSKey]string{},
		podsByName:        map[alertNameKey][]string{},
		pvcsByCluster:     map[alertNSKey]string{},
		pvcsByName:        map[alertNameKey][]string{},
		k8sNodesByCluster: map[alertOCKey]string{},
		k8sNodesByName:    map[string][]string{},
		ctrlsByCluster:    map[alertOCKey]string{},
		ctrlsByName:       map[string][]string{},
		aggrsByCluster:    map[alertOCKey]string{},
		aggrsByName:       map[string][]string{},
	}

	// addNamespaced / addClusterScoped keep the cluster-qualified map
	// deterministic on the (defensive) duplicate: the lexically-smallest id
	// wins, so the pick is a pure function of the node set rather than of slice
	// order. A duplicate cannot arise from a well-formed estate — graph.NewGraph
	// dedupes by id — but the matcher must not acquire an order dependency.
	addNamespaced := func(byCluster map[alertNSKey]string, byName map[alertNameKey][]string,
		cluster, namespace, name, id string,
	) {
		if namespace == "" || name == "" {
			return
		}
		if cluster != "" {
			k := alertNSKey{cluster, namespace, name}
			if prev, dup := byCluster[k]; !dup || id < prev {
				byCluster[k] = id
			}
		}
		nk := alertNameKey{namespace, name}
		byName[nk] = append(byName[nk], id)
	}
	addClusterScoped := func(byCluster map[alertOCKey]string, byName map[string][]string,
		cluster, name, id string,
	) {
		if name == "" {
			return
		}
		if cluster != "" {
			k := alertOCKey{cluster, name}
			if prev, dup := byCluster[k]; !dup || id < prev {
				byCluster[k] = id
			}
		}
		byName[name] = append(byName[name], id)
	}

	for _, n := range nodes {
		labels := n.Labels()
		switch n.Type() {
		case graph.NodeTypePod:
			addNamespaced(idx.podsByCluster, idx.podsByName,
				labels["cluster"], labels[alertNamespaceLabel], n.Name(), n.ID())
		case graph.NodeTypePVC:
			addNamespaced(idx.pvcsByCluster, idx.pvcsByName,
				labels["cluster"], labels[alertNamespaceLabel], n.Name(), n.ID())
		case graph.NodeTypeK8sNode:
			addClusterScoped(idx.k8sNodesByCluster, idx.k8sNodesByName,
				labels["cluster"], n.Name(), n.ID())
		case graph.NodeTypeNetAppNode:
			addClusterScoped(idx.ctrlsByCluster, idx.ctrlsByName,
				labels["ontap_cluster"], n.Name(), n.ID())
		case graph.NodeTypeNetAppAggr:
			addClusterScoped(idx.aggrsByCluster, idx.aggrsByName,
				labels["ontap_cluster"], n.Name(), n.ID())
		case graph.NodeTypeService, graph.NodeTypeExternal, graph.NodeTypeNetAppSVM:
			// Never alert-carrying. Listed so the switch stays exhaustive and a
			// new node kind has to decide deliberately.
		}
	}
	return idx
}

// matchUnique resolves the no-cluster-label fallback over a candidate set:
// exactly one id matches, several are the ambiguous case the caller counts
// (an alert naming a pod two clusters both hold, with nothing to disambiguate
// them, attaches to neither rather than to whichever the slice happened to
// hold first), and none is unmatched.
func matchUnique(ids []string) alertMatch {
	switch len(ids) {
	case 0:
		return alertMatch{}
	case 1:
		return alertMatch{id: ids[0]}
	}
	return alertMatch{ambiguous: true}
}

// alertMatch is one resolution outcome. Exactly one of the three states holds:
// a non-empty id (matched), ambiguous, or neither (unmatched).
type alertMatch struct {
	id        string
	ambiguous bool
}

// resolveAlerts matches each active alert to at most one graph node and returns
// the per-node alert sets, plus the two aggregated counts.
//
// It is pure: the index and the resolver are inputs, and the returned map is
// the whole effect. The caller attaches the sets and emits the warnings, so a
// test can assert the matching without capturing logs.
//
// The alert sets come back sorted by (name, severity) and de-duplicated on that
// pair, so two ALERTS series differing only in a label the matcher never reads
// collapse to one entry whatever order the vector held them in.
func resolveAlerts(vec model.Vector, idx alertIndex, clusters *clusterResolver) (
	byNode map[string][]graph.Alert, unmatched, ambiguous int,
) {
	if len(vec) == 0 {
		return nil, 0, 0
	}
	if clusters == nil {
		// A hand-built Topology (a test, an embedder) carries no resolver. The
		// ladder then has nothing to compose or adopt from, which is exactly
		// the unstamped-estate behaviour: every name stands verbatim.
		clusters = newClusterResolver(promql.LabelKeys{})
	}
	acc := map[string][]graph.Alert{}
	for _, s := range vec {
		// Mirrors the query's fixed alertstate="firing" selector, so the
		// pushdown is output-preserving: a hand-built Topology or an embedder's
		// vector carrying a pending alert is discarded here exactly as the
		// upstream matcher would have discarded it.
		if string(s.Metric[alertStateLabel]) != graph.AlertStateFiring {
			continue
		}
		m := resolveOneAlert(s.Metric, idx, clusters)
		switch {
		case m.ambiguous:
			ambiguous++
		case m.id == "":
			unmatched++
		default:
			acc[m.id] = append(acc[m.id], graph.Alert{
				Name:     string(s.Metric[alertNameLabel]),
				State:    string(s.Metric[alertStateLabel]),
				Severity: string(s.Metric[alertSeverityLabel]),
			})
		}
	}
	if len(acc) == 0 {
		return nil, unmatched, ambiguous
	}
	byNode = make(map[string][]graph.Alert, len(acc))
	for id, alerts := range acc {
		byNode[id] = graph.SortAlerts(alerts)
	}
	return byNode, unmatched, ambiguous
}

// resolveOneAlert applies the kind precedence: the target kind is chosen by the
// MOST SPECIFIC label present, and a less specific label on the same series is
// then never consulted. That is what makes a `{namespace, pod, node}` alert
// attach to the pod alone rather than to both the pod and the node it runs on —
// an alert about a crash-looping pod is not an alert about its host.
func resolveOneAlert(m model.Metric, idx alertIndex, clusters *clusterResolver) alertMatch {
	ns := string(m[alertNamespaceLabel])
	pod := string(m[alertPodLabel])
	pvc := string(m[alertPVCLabel])
	node := string(m[alertNodeLabel])
	aggr := string(m[alertAggrLabel])
	rawCluster := string(m[alertClusterLabel])

	// `aggr` outranks `node`: the stock Harvest aggr_* series carry the owning
	// controller's `node` beside `aggr`, so an alert written over them names
	// both, and it is an alert about the aggregate — attaching it to the
	// controller would make every aggregate alert land one tier up.
	switch {
	case ns != "" && pod != "":
		return matchNamespaced(idx.podsByCluster, idx.podsByName, m, rawCluster, ns, pod, clusters)
	case ns != "" && pvc != "":
		return matchNamespaced(idx.pvcsByCluster, idx.pvcsByName, m, rawCluster, ns, pvc, clusters)
	case aggr != "":
		return matchAggr(idx, rawCluster, aggr)
	case node != "":
		return matchNodeShaped(idx, m, rawCluster, node, clusters)
	}
	return alertMatch{}
}

// matchNamespaced resolves the pod and PVC kinds. A present `cluster` label
// walks the SAME identity ladder every Kubernetes series walks (compose, else
// adopt, else verbatim), so an alert stamped `cluster="c1"` finds the pod the
// build stored under `zone-a-prod-c1`. With no label the match must be unique
// across the loaded estate.
func matchNamespaced(
	byCluster map[alertNSKey]string, byName map[alertNameKey][]string,
	m model.Metric, rawCluster, namespace, name string, clusters *clusterResolver,
) alertMatch {
	if rawCluster != "" {
		identity := clusters.identify(m)
		if id, ok := byCluster[alertNSKey{identity, namespace, name}]; ok {
			return alertMatch{id: id}
		}
		// A cluster-qualified alert that names no loaded node is UNMATCHED, not
		// a candidate for the uniqueness fallback: the label is an assertion
		// about which cluster the object lives in, and honouring it means a
		// same-named pod in a different cluster must not absorb the alert.
		return alertMatch{}
	}
	return matchUnique(byName[alertNameKey{namespace, name}])
}

// matchNodeShaped resolves the `{cluster, node}` shape, which the Kubernetes
// and ONTAP node kinds SHARE. This is the one place two entirely different
// entities answer to the same label set, so it is also the only place a
// two-way test is needed.
//
// With a `cluster` label the two sides are tested differently, because they
// mean different things: the Kubernetes side resolves the label through the
// identity ladder (it is a Kubernetes cluster name), while the ONTAP side
// compares it RAW (a filer name is not part of the Kubernetes identity space
// and composes with no zone or environment). An alert satisfying BOTH is
// counted ambiguous and attached to neither — guessing would silently point a
// controller alert at a Kubernetes node, or the reverse.
func matchNodeShaped(idx alertIndex, m model.Metric, rawCluster, node string, clusters *clusterResolver) alertMatch {
	if rawCluster != "" {
		identity := clusters.identify(m)
		k8sID, isK8s := idx.k8sNodesByCluster[alertOCKey{identity, node}]
		ctrlID, isCtrl := idx.ctrlsByCluster[alertOCKey{rawCluster, node}]
		switch {
		case isK8s && isCtrl:
			return alertMatch{ambiguous: true}
		case isK8s:
			return alertMatch{id: k8sID}
		case isCtrl:
			return alertMatch{id: ctrlID}
		}
		return alertMatch{}
	}
	// No cluster label: the eligible kinds are BOTH, so uniqueness is tested
	// over their union. Two candidates of the same kind are as ambiguous as one
	// of each.
	return matchUnique(append(append([]string(nil), idx.k8sNodesByName[node]...), idx.ctrlsByName[node]...))
}

// matchAggr resolves the aggregate kind. Its `cluster` label is an ONTAP
// cluster and is compared RAW — an aggregate belongs to no Kubernetes cluster,
// so the identity ladder has nothing to say about it.
func matchAggr(idx alertIndex, rawCluster, aggr string) alertMatch {
	if rawCluster != "" {
		if id, ok := idx.aggrsByCluster[alertOCKey{rawCluster, aggr}]; ok {
			return alertMatch{id: id}
		}
		return alertMatch{}
	}
	return matchUnique(idx.aggrsByName[aggr])
}

// attachAlerts resolves the build's ALERTS vector against the assembled node
// set and stamps each node's matched alerts onto it.
//
// It runs after assemble and BEFORE graph.NewGraph — the same "bake before
// freeze" point PVC Application inheritance uses. Mutating the node structs is
// safe there and only there: once NewGraph has taken them the graph is
// immutable and shared.
//
// Both warnings are aggregated to at most one per build and fire only when
// their count is non-zero AND the vector was non-empty, so a healthy estate —
// or one whose alerts family is served by no backend — stays silent.
func attachAlerts(ctx context.Context, nodes []graph.GraphNode, topology Topology) {
	if len(topology.Alerts) == 0 {
		return
	}
	byNode, unmatched, ambiguous := resolveAlerts(
		topology.Alerts, newAlertIndex(nodes), topology.clusters)

	for _, n := range nodes {
		alerts := byNode[n.ID()]
		if len(alerts) == 0 {
			continue
		}
		switch t := n.(type) {
		case *graph.PodNode:
			t.AlertsValue = alerts
		case *graph.K8sNode:
			t.AlertsValue = alerts
		case *graph.PVCNode:
			t.AlertsValue = alerts
		case *graph.NetAppNode:
			t.AlertsValue = alerts
		case *graph.NetAppAggrNode:
			t.AlertsValue = alerts
		}
	}

	if unmatched > 0 {
		slog.WarnContext(ctx, "alerts_unmatched",
			"count", unmatched,
			"detail", "active alerts whose label set named no loaded node; check that the alerting store stamps the same az/env labels as kube-state-metrics",
		)
	}
	if ambiguous > 0 {
		slog.WarnContext(ctx, "alerts_ambiguous",
			"count", ambiguous,
			"detail", "active alerts matching several nodes, or matching both a Kubernetes node and an ONTAP controller; attached to none",
		)
	}
}
