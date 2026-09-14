package build

import (
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

func TestAttachStatus_FoldsOnlyEachNodesOwnSignals(t *testing.T) {
	cpu := 99.0
	nodes := []graph.GraphNode{
		&graph.PodNode{IDValue: "pod", AlertsValue: []graph.Alert{{Severity: "warning"}}},
		&graph.K8sNode{IDValue: "worker", ReadyStatusValue: graph.ReadyStatusNotReady},
		&graph.PVCNode{IDValue: "claim"},
		&graph.NetAppNode{
			IDValue:     "controller",
			HealthValue: graph.HealthOnline,
			PerfValue:   &graph.NodePerf{CPUBusyPct: &cpu},
		},
		&graph.NetAppAggrNode{IDValue: "aggregate", HealthValue: graph.HealthDegraded},
		&graph.ServiceNode{IDValue: "service"},
		&graph.ExternalNode{IDValue: "external"},
		&graph.NetAppSVMNode{IDValue: "svm"},
	}

	attachStatus(nodes)

	want := map[string]string{
		"pod":        graph.StatusWarning,
		"worker":     graph.StatusCritical,
		"claim":      graph.StatusNormal,
		"controller": graph.StatusNormal,
		"aggregate":  graph.StatusCritical,
		"service":    "",
		"external":   "",
		"svm":        "",
	}
	for _, n := range nodes {
		assert.Equal(t, want[n.ID()], n.Status(), n.ID())
	}
}

func TestBuildPaths_AttachStatusBeforeGraphFreeze(t *testing.T) {
	fixtures := storageFixtures()
	fixtures[promql.QNodeStatusCondition] = sampleVec(model.Sample{Metric: model.Metric{
		"cluster": "c1", "node": "worker-1", "condition": "Ready", "status": "false",
	}, Value: 1})
	fixtures[promql.QNetAppNodeStatus] = sampleVec(model.Sample{Metric: model.Metric{
		"cluster": "ontap-prod", "node": "ontap-prod-01",
	}, Value: 0})
	fixtures[promql.QNetAppNodeCPUBusy] = sampleVec(model.Sample{Metric: model.Metric{
		"cluster": "ontap-prod", "node": "ontap-prod-01",
	}, Value: 99})
	fixtures[promql.QAlerts] = sampleVec(model.Sample{Metric: model.Metric{
		"alertname": "PodWarning", "alertstate": graph.AlertStateFiring,
		"severity": "warning", "cluster": "c1", "namespace": "shop", "pod": "orders-0",
	}, Value: 1})

	builders := map[string]func(*Builder) (*graph.Graph, error){
		"graph": func(b *Builder) (*graph.Graph, error) {
			return b.Build(t.Context(), time.Minute, time.Unix(1, 0).UTC(),
				promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}})
		},
		"storage": func(b *Builder) (*graph.Graph, error) {
			return b.BuildStorage(t.Context(), time.Minute, time.Unix(1, 0).UTC(),
				promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}}, graph.StorageRoots{})
		},
	}

	for name, run := range builders {
		t.Run(name, func(t *testing.T) {
			q, _ := newRecordingQuerier(t, fixtures)
			g, err := run(newStorageBuilder(t, q))
			require.NoError(t, err)

			assert.Equal(t, graph.StatusWarning, g.NodesByID["c1/uid-1"].Status())
			assert.Equal(t, graph.StatusCritical, g.NodesByID["c1/worker-1"].Status())
			assert.Equal(t, graph.StatusNormal, g.NodesByID["c1/shop/orders-data"].Status())
			assert.Equal(t, graph.StatusCritical,
				g.NodesByID[graph.NetAppNodeID("ontap-prod", "ontap-prod-01")].Status())
			assert.Equal(t, graph.StatusNormal,
				g.NodesByID[graph.NetAppAggrID("ontap-prod", "aggr1")].Status())
		})
	}
}
