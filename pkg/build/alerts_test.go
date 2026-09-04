package build

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// alertSample builds one ALERTS series. Every case here carries
// alertstate="firing": the query's fixed selector means a `pending` series
// never reaches the reader, so a fixture producing one would be testing a
// state the resolver cannot observe.
func alertSample(name, severity string, labels map[string]string) model.Sample {
	m := model.Metric{
		"alertname":  model.LabelValue(name),
		"alertstate": model.LabelValue(graph.AlertStateFiring),
	}
	if severity != "" {
		m["severity"] = model.LabelValue(severity)
	}
	for k, v := range labels {
		m[model.LabelName(k)] = model.LabelValue(v)
	}
	return model.Sample{Metric: m, Value: 1}
}

// alertEstate is the node set every matching case resolves against: one pod,
// one PVC and one Kubernetes node in identity zone-a-prod-c1, plus an ONTAP
// controller and aggregate on filer ontap-prod.
func alertEstate() []graph.GraphNode {
	return []graph.GraphNode{
		&graph.PodNode{
			IDValue:     graph.PodID("zone-a-prod-c1", "uid-1"),
			NameValue:   "orders-0",
			LabelsValue: map[string]string{"cluster": "zone-a-prod-c1", "namespace": "shop"},
		},
		&graph.PVCNode{
			IDValue:     graph.PVCID("zone-a-prod-c1", "shop", "orders-data"),
			NameValue:   "orders-data",
			LabelsValue: map[string]string{"cluster": "zone-a-prod-c1", "namespace": "shop"},
		},
		&graph.K8sNode{
			IDValue:     graph.K8sNodeID("zone-a-prod-c1", "worker-1"),
			NameValue:   "worker-1",
			LabelsValue: map[string]string{"cluster": "zone-a-prod-c1"},
		},
		&graph.NetAppNode{
			IDValue:     graph.NetAppNodeID("ontap-prod", "ontap-prod-01"),
			NameValue:   "ontap-prod-01",
			LabelsValue: map[string]string{"ontap_cluster": "ontap-prod"},
		},
		&graph.NetAppAggrNode{
			IDValue:     graph.NetAppAggrID("ontap-prod", "aggr1"),
			NameValue:   "aggr1",
			LabelsValue: map[string]string{"ontap_cluster": "ontap-prod", "node": "ontap-prod-01"},
		},
	}
}

// identityResolver returns a resolver that has already composed
// zone-a-prod-c1 from a series stamped az=zone-a, env=prod, cluster=c1 — so a
// later alert carrying only cluster="c1" resolves by ADOPTION, exactly as any
// other join input does.
func identityResolver(t *testing.T) *clusterResolver {
	t.Helper()
	r := newClusterResolver(promql.LabelKeys{})
	r.observe(model.Metric{"az": "zone-a", "env": "prod", "cluster": "c1"})
	return r
}

// resolveOver runs the matcher over the shared estate.
func resolveOver(t *testing.T, nodes []graph.GraphNode, samples ...model.Sample) (
	map[string][]graph.Alert, int, int,
) {
	t.Helper()
	return resolveAlerts(sampleVec(samples...), newAlertIndex(nodes), identityResolver(t))
}

// --- kind 1: pods ---------------------------------------------------------

func TestResolveAlerts_PodAttached(t *testing.T) {
	byNode, unmatched, ambiguous := resolveOver(t, alertEstate(),
		alertSample("KubePodCrashLooping", "warning", map[string]string{
			"cluster": "c1", "namespace": "shop", "pod": "orders-0",
		}))

	assert.Zero(t, unmatched)
	assert.Zero(t, ambiguous)
	assert.Equal(t, map[string][]graph.Alert{
		graph.PodID("zone-a-prod-c1", "uid-1"): {
			{Name: "KubePodCrashLooping", State: graph.AlertStateFiring, Severity: "warning"},
		},
	}, byNode, "the alert attaches to the pod and to no other node")
}

// The kind precedence: an alert naming BOTH a pod and the node it runs on is
// about the pod. Consulting the less specific label too would put a
// crash-loop alert on the host, which is a different claim entirely.
func TestResolveAlerts_PodOutranksNodeLabel(t *testing.T) {
	byNode, _, _ := resolveOver(t, alertEstate(),
		alertSample("KubePodCrashLooping", "warning", map[string]string{
			"cluster": "c1", "namespace": "shop", "pod": "orders-0", "node": "worker-1",
		}))

	require.Len(t, byNode, 1)
	assert.Contains(t, byNode, graph.PodID("zone-a-prod-c1", "uid-1"))
	assert.NotContains(t, byNode, graph.K8sNodeID("zone-a-prod-c1", "worker-1"))
}

// An alert for a pod the build did not load is unmatched — the pod side is
// matched by name against what the window actually returned.
func TestResolveAlerts_PodNotLoadedIsUnmatched(t *testing.T) {
	byNode, unmatched, ambiguous := resolveOver(t, alertEstate(),
		alertSample("KubePodCrashLooping", "warning", map[string]string{
			"cluster": "c1", "namespace": "shop", "pod": "gone-0",
		}))

	assert.Empty(t, byNode)
	assert.Equal(t, 1, unmatched)
	assert.Zero(t, ambiguous)
}

// A cluster-qualified alert naming a cluster the build did not load stays
// unmatched rather than falling back to uniqueness: the label is an assertion
// about WHICH cluster holds the object, so a same-named pod elsewhere must not
// absorb it.
func TestResolveAlerts_WrongClusterDoesNotFallBackToUniqueness(t *testing.T) {
	byNode, unmatched, _ := resolveOver(t, alertEstate(),
		alertSample("KubePodCrashLooping", "warning", map[string]string{
			"cluster": "somewhere-else", "namespace": "shop", "pod": "orders-0",
		}))

	assert.Empty(t, byNode, "an explicit cluster label is honoured, never widened")
	assert.Equal(t, 1, unmatched)
}

// --- kind 2: claims -------------------------------------------------------

func TestResolveAlerts_ClaimAttached(t *testing.T) {
	byNode, _, _ := resolveOver(t, alertEstate(),
		alertSample("VolumeAlmostFull", "critical", map[string]string{
			"cluster": "c1", "namespace": "shop", "persistentvolumeclaim": "orders-data",
		}))

	assert.Equal(t, map[string][]graph.Alert{
		graph.PVCID("zone-a-prod-c1", "shop", "orders-data"): {
			{Name: "VolumeAlmostFull", State: graph.AlertStateFiring, Severity: "critical"},
		},
	}, byNode)
}

// --- kind 3: the shared {cluster, node} shape -----------------------------

func TestResolveAlerts_KubernetesNodeAttached(t *testing.T) {
	byNode, _, ambiguous := resolveOver(t, alertEstate(),
		alertSample("NodeNotReady", "critical", map[string]string{
			"cluster": "c1", "node": "worker-1",
		}))

	assert.Zero(t, ambiguous)
	assert.Equal(t, map[string][]graph.Alert{
		graph.K8sNodeID("zone-a-prod-c1", "worker-1"): {
			{Name: "NodeNotReady", State: graph.AlertStateFiring, Severity: "critical"},
		},
	}, byNode)
}

// The ONTAP side compares the cluster label RAW: a filer name is not part of
// the Kubernetes identity space and composes with no zone or environment.
func TestResolveAlerts_ONTAPControllerAttached(t *testing.T) {
	byNode, _, ambiguous := resolveOver(t, alertEstate(),
		alertSample("NodeHighCPU", "warning", map[string]string{
			"cluster": "ontap-prod", "node": "ontap-prod-01",
		}))

	assert.Zero(t, ambiguous)
	assert.Equal(t, map[string][]graph.Alert{
		graph.NetAppNodeID("ontap-prod", "ontap-prod-01"): {
			{Name: "NodeHighCPU", State: graph.AlertStateFiring, Severity: "warning"},
		},
	}, byNode)
}

// The two-way test. A Kubernetes identity and an ONTAP cluster sharing a raw
// name, each holding a node of the alert's name, is genuinely undecidable:
// guessing would point a controller alert at a Kubernetes node or the reverse.
func TestResolveAlerts_NodeShapedAmbiguousAcrossKinds(t *testing.T) {
	nodes := []graph.GraphNode{
		&graph.K8sNode{
			IDValue: graph.K8sNodeID("x", "n1"), NameValue: "n1",
			LabelsValue: map[string]string{"cluster": "x"},
		},
		&graph.NetAppNode{
			IDValue: graph.NetAppNodeID("x", "n1"), NameValue: "n1",
			LabelsValue: map[string]string{"ontap_cluster": "x"},
		},
	}
	// A resolver with no composed identity, so "x" stands verbatim on the
	// Kubernetes side and equals the ONTAP cluster name on the other.
	byNode, unmatched, ambiguous := resolveAlerts(
		sampleVec(alertSample("Something", "warning", map[string]string{"cluster": "x", "node": "n1"})),
		newAlertIndex(nodes), newClusterResolver(promql.LabelKeys{}))

	assert.Empty(t, byNode, "attached to neither kind")
	assert.Zero(t, unmatched, "an ambiguous alert is counted ambiguous, not unmatched")
	assert.Equal(t, 1, ambiguous)
}

// --- kind 4: aggregates ---------------------------------------------------

func TestResolveAlerts_AggregateAttached(t *testing.T) {
	byNode, _, _ := resolveOver(t, alertEstate(),
		alertSample("AggrAlmostFull", "warning", map[string]string{
			"cluster": "ontap-prod", "aggr": "aggr1",
		}))

	assert.Equal(t, map[string][]graph.Alert{
		graph.NetAppAggrID("ontap-prod", "aggr1"): {
			{Name: "AggrAlmostFull", State: graph.AlertStateFiring, Severity: "warning"},
		},
	}, byNode)
}

// The stock Harvest aggr_* series carry the owning controller's `node` beside
// `aggr`, so a rule written over them names both. It is an alert about the
// aggregate: `aggr` outranks `node`, and the controller stays clean.
func TestResolveAlerts_AggregateOutranksNodeLabel(t *testing.T) {
	byNode, unmatched, ambiguous := resolveOver(t, alertEstate(),
		alertSample("AggrSpaceLow", "critical", map[string]string{
			"cluster": "ontap-prod", "node": "ontap-prod-01", "aggr": "aggr1",
		}))

	assert.Equal(t, map[string][]graph.Alert{
		graph.NetAppAggrID("ontap-prod", "aggr1"): {
			{Name: "AggrSpaceLow", State: graph.AlertStateFiring, Severity: "critical"},
		},
	}, byNode)
	assert.Zero(t, unmatched)
	assert.Zero(t, ambiguous)
}

// The reader mirrors the query's fixed alertstate="firing" selector: a pending
// alert reaching it from a hand-built vector is discarded, not attached and
// not counted.
func TestResolveAlerts_NonFiringIsDiscarded(t *testing.T) {
	pending := alertSample("KubePodCrashLooping", "warning", map[string]string{
		"cluster": "c1", "namespace": "shop", "pod": "orders-0",
		"alertstate": "pending",
	})
	byNode, unmatched, ambiguous := resolveOver(t, alertEstate(), pending)

	assert.Empty(t, byNode)
	assert.Zero(t, unmatched)
	assert.Zero(t, ambiguous)
}

// --- kind 5 and the uniqueness fallback -----------------------------------

// With no cluster label the match succeeds only when exactly one node of the
// eligible kind carries the remaining labels.
func TestResolveAlerts_MissingClusterResolvedByUniqueness(t *testing.T) {
	byNode, unmatched, ambiguous := resolveOver(t, alertEstate(),
		alertSample("KubePodCrashLooping", "warning", map[string]string{
			"namespace": "shop", "pod": "orders-0",
		}))

	assert.Zero(t, unmatched)
	assert.Zero(t, ambiguous)
	assert.Contains(t, byNode, graph.PodID("zone-a-prod-c1", "uid-1"))
}

func TestResolveAlerts_MissingClusterWithSeveralCandidates(t *testing.T) {
	nodes := append(alertEstate(), &graph.PodNode{
		IDValue:     graph.PodID("zone-b-prod-c2", "uid-2"),
		NameValue:   "orders-0",
		LabelsValue: map[string]string{"cluster": "zone-b-prod-c2", "namespace": "shop"},
	})
	byNode, unmatched, ambiguous := resolveOver(t, nodes,
		alertSample("KubePodCrashLooping", "warning", map[string]string{
			"namespace": "shop", "pod": "orders-0",
		}))

	assert.Empty(t, byNode)
	assert.Zero(t, unmatched)
	assert.Equal(t, 1, ambiguous)
}

// The node-shaped uniqueness fallback tests the UNION of both kinds: two
// candidates of one kind are as ambiguous as one of each.
func TestResolveAlerts_MissingClusterNodeShapedUnion(t *testing.T) {
	t.Run("unique across both kinds resolves", func(t *testing.T) {
		byNode, _, ambiguous := resolveOver(t, alertEstate(),
			alertSample("NodeHighCPU", "warning", map[string]string{"node": "ontap-prod-01"}))
		assert.Zero(t, ambiguous)
		assert.Contains(t, byNode, graph.NetAppNodeID("ontap-prod", "ontap-prod-01"))
	})

	t.Run("one of each kind is ambiguous", func(t *testing.T) {
		nodes := append(alertEstate(), &graph.NetAppNode{
			IDValue: graph.NetAppNodeID("ontap-prod", "worker-1"), NameValue: "worker-1",
			LabelsValue: map[string]string{"ontap_cluster": "ontap-prod"},
		})
		byNode, _, ambiguous := resolveOver(t, nodes,
			alertSample("Something", "warning", map[string]string{"node": "worker-1"}))
		assert.Empty(t, byNode)
		assert.Equal(t, 1, ambiguous)
	})
}

// An alert whose label set names none of the four kinds is unmatched.
func TestResolveAlerts_UnmatchableLabelSet(t *testing.T) {
	byNode, unmatched, ambiguous := resolveOver(t, alertEstate(),
		alertSample("TargetDown", "critical", map[string]string{"cluster": "c1"}))

	assert.Empty(t, byNode)
	assert.Equal(t, 1, unmatched)
	assert.Zero(t, ambiguous)
}

// A `namespace` with no pod and no claim names no kind either — the precedence
// requires BOTH halves of the pair.
func TestResolveAlerts_NamespaceAloneIsUnmatched(t *testing.T) {
	_, unmatched, _ := resolveOver(t, alertEstate(),
		alertSample("QuotaExceeded", "warning", map[string]string{
			"cluster": "c1", "namespace": "shop",
		}))
	assert.Equal(t, 1, unmatched)
}

// --- attribute shape ------------------------------------------------------

// The set on a node is sorted by (name, severity) and de-duplicated on that
// pair, so two series differing only in a label the matcher never reads
// collapse to one entry — and the result does not depend on vector order.
func TestResolveAlerts_SortedAndDeduplicated(t *testing.T) {
	podLabels := map[string]string{"cluster": "c1", "namespace": "shop", "pod": "orders-0"}
	crash := alertSample("KubePodCrashLooping", "warning", podLabels)
	mem := alertSample("HighMemory", "critical", podLabels)
	// Same (name, severity), differing only in a label the matcher never reads.
	dup := alertSample("KubePodCrashLooping", "warning", podLabels)
	dup.Metric["container"] = "sidecar"

	want := []graph.Alert{
		{Name: "HighMemory", State: graph.AlertStateFiring, Severity: "critical"},
		{Name: "KubePodCrashLooping", State: graph.AlertStateFiring, Severity: "warning"},
	}

	fwd, _, _ := resolveOver(t, alertEstate(), crash, mem, dup)
	rev, _, _ := resolveOver(t, alertEstate(), dup, mem, crash)
	assert.Equal(t, want, fwd[graph.PodID("zone-a-prod-c1", "uid-1")])
	assert.Equal(t, fwd, rev, "upstream vector order must not reach the output")
}

// An alert with no severity label keeps the field empty, so the serialiser
// omits it rather than emitting "".
func TestResolveAlerts_SeverityOptional(t *testing.T) {
	byNode, _, _ := resolveOver(t, alertEstate(),
		alertSample("NodeNotReady", "", map[string]string{"cluster": "c1", "node": "worker-1"}))

	assert.Equal(t, []graph.Alert{{Name: "NodeNotReady", State: graph.AlertStateFiring}},
		byNode[graph.K8sNodeID("zone-a-prod-c1", "worker-1")])
}

// Services, externals and SVMs never carry alerts, and are never indexed — so
// they cannot absorb an alert nor make a bare-name lookup look ambiguous. The
// Service is the load-bearing one: ServiceID mirrors PVCID keying, so an
// indexed Service would collide with the PVC of the same (cluster, ns, name).
func TestResolveAlerts_NonCarryingKindsAreNotIndexed(t *testing.T) {
	nodes := append(alertEstate(),
		&graph.ServiceNode{
			IDValue:     graph.ServiceID("zone-a-prod-c1", "shop", "orders-data"),
			NameValue:   "orders-data",
			LabelsValue: map[string]string{"cluster": "zone-a-prod-c1", "namespace": "shop"},
		},
		&graph.ExternalNode{IDValue: graph.ExternalID("api.example.com"), NameValue: "api.example.com"},
		&graph.NetAppSVMNode{
			IDValue:     graph.NetAppSVMID("ontap-prod", "svm_shop"),
			NameValue:   "svm_shop",
			LabelsValue: map[string]string{"ontap_cluster": "ontap-prod"},
		},
	)

	byNode, _, ambiguous := resolveOver(t, nodes,
		alertSample("VolumeAlmostFull", "critical", map[string]string{
			"namespace": "shop", "persistentvolumeclaim": "orders-data",
		}))

	assert.Zero(t, ambiguous, "the same-named Service must not make the claim look ambiguous")
	assert.Equal(t, map[string][]graph.Alert{
		graph.PVCID("zone-a-prod-c1", "shop", "orders-data"): {
			{Name: "VolumeAlmostFull", State: graph.AlertStateFiring, Severity: "critical"},
		},
	}, byNode)
}

// A synth pod carries no cluster or namespace label, so it is neither matchable
// nor a source of false ambiguity.
func TestResolveAlerts_SynthPodContributesNothing(t *testing.T) {
	nodes := append(alertEstate(), &graph.PodNode{
		IDValue: graph.PodID("", "uid-synth"), NameValue: "orders-0",
	})
	byNode, _, ambiguous := resolveOver(t, nodes,
		alertSample("KubePodCrashLooping", "warning", map[string]string{
			"namespace": "shop", "pod": "orders-0",
		}))

	assert.Zero(t, ambiguous)
	assert.Contains(t, byNode, graph.PodID("zone-a-prod-c1", "uid-1"))
}

func TestResolveAlerts_EmptyVector(t *testing.T) {
	byNode, unmatched, ambiguous := resolveAlerts(nil, newAlertIndex(alertEstate()), nil)
	assert.Nil(t, byNode)
	assert.Zero(t, unmatched)
	assert.Zero(t, ambiguous)
}

// --- attachment and observability -----------------------------------------

// attachAlerts stamps the resolved sets onto the node structs and emits at most
// one Warn per build per axis, only when the count is non-zero.
func TestAttachAlerts_StampsAndWarns(t *testing.T) {
	buf := captureLogs(t)
	nodes := alertEstate()

	attachAlerts(context.Background(), nodes, Topology{
		clusters: identityResolver(t),
		Alerts: sampleVec(
			alertSample("KubePodCrashLooping", "warning", map[string]string{
				"cluster": "c1", "namespace": "shop", "pod": "orders-0",
			}),
			// Unmatched: three alerts naming pods the build did not load.
			alertSample("A", "warning", map[string]string{"cluster": "c1", "namespace": "shop", "pod": "gone-1"}),
			alertSample("B", "warning", map[string]string{"cluster": "c1", "namespace": "shop", "pod": "gone-2"}),
			alertSample("C", "warning", map[string]string{"cluster": "c1", "namespace": "shop", "pod": "gone-3"}),
		),
	})

	assert.Equal(t,
		[]graph.Alert{{Name: "KubePodCrashLooping", State: graph.AlertStateFiring, Severity: "warning"}},
		nodes[0].Alerts(), "the matched alert is baked onto the pod struct")
	for _, n := range nodes[1:] {
		assert.Nilf(t, n.Alerts(), "%s matched no alert and must stay nil", n.ID())
	}

	out := buf.String()
	assert.Contains(t, out, "alerts_unmatched")
	assert.Contains(t, out, "count=3")
	assert.NotContains(t, out, "alerts_ambiguous", "a zero count emits no Warn")
}

// A fully-matched estate is silent on both axes.
func TestAttachAlerts_FullMatchIsSilent(t *testing.T) {
	buf := captureLogs(t)
	nodes := alertEstate()

	attachAlerts(context.Background(), nodes, Topology{
		clusters: identityResolver(t),
		Alerts: sampleVec(alertSample("NodeNotReady", "critical", map[string]string{
			"cluster": "c1", "node": "worker-1",
		})),
	})

	assert.NotContains(t, buf.String(), "alerts_unmatched")
	assert.NotContains(t, buf.String(), "alerts_ambiguous")
}

// An empty vector — the healthy estate, or an alerts family served by no
// backend — attaches nothing and warns about nothing.
func TestAttachAlerts_EmptyVectorIsSilent(t *testing.T) {
	buf := captureLogs(t)
	nodes := alertEstate()

	attachAlerts(context.Background(), nodes, Topology{clusters: identityResolver(t)})

	for _, n := range nodes {
		assert.Nil(t, n.Alerts())
	}
	assert.Empty(t, buf.String())
}

// --- end to end through ReadTopology / Build ------------------------------

// The ALERTS leg is OPTIONAL: a query error degrades log-and-continue, leaving
// every node alert-less rather than failing the build.
func TestReadTopology_AlertsLegFailureDoesNotFailBuild(t *testing.T) {
	legs := failingLegs(nil, errors.New("no such metric"), promql.QAlerts)
	legs[promql.QPodInfo] = legFixture{sampleVec(model.Sample{Metric: model.Metric{
		"cluster": "c", "namespace": "shop", "pod": "orders-0", "uid": "u1", "node": "w0",
	}}), nil}

	tp, err := readTopologyDefaults(context.Background(), legQuerier(t, legs))
	require.NoError(t, err, "the alert overlay must never fail a build")
	assert.Empty(t, tp.Alerts)
	require.Contains(t, tp.RawSeriesCount, string(promql.QAlerts))
	assert.Zero(t, tp.RawSeriesCount[string(promql.QAlerts)])
	require.Len(t, tp.Pods, 1)
	assert.Nil(t, tp.Pods[0].Alerts())
}

// The raw vector reaches Topology unparsed — matching cannot run at parse time
// because it needs the assembled node set.
func TestReadTopology_AlertsVectorCarriedRaw(t *testing.T) {
	vec := sampleVec(alertSample("KubePodCrashLooping", "warning", map[string]string{
		"cluster": "c", "namespace": "shop", "pod": "orders-0",
	}))
	legs := map[promql.Query]legFixture{promql.QAlerts: {vec, nil}}

	tp, err := readTopologyDefaults(context.Background(), legQuerier(t, legs))
	require.NoError(t, err)
	assert.Equal(t, vec, tp.Alerts)
	assert.Equal(t, 1, tp.RawSeriesCount[string(promql.QAlerts)])
}
