package build

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// TestParseTopology_PVCQoSAttribute pins the stamp in parseTopology: the
// resolved ceiling lands on PVCNode.QoS() before the graph freezes, whether or
// not the claim drew an edge or carries a measurement
// (add-pvc-qos-ceiling-attribute D1/D2).
func TestParseTopology_PVCQoSAttribute(t *testing.T) {
	t.Run("measured claim's node equals its edge", func(t *testing.T) {
		v := pvcBindingVectors("c", "db", "mongo", "data", "pvc-9f3a")
		v.VolumeLabels = sampleVec(volLabelSample("pvc-9f3a", "oc", "n1", "a1", "svm-prod"))
		v.QoSReadOps = sampleVec(qosSample("pvc-9f3a", "oc", "svm-prod", "gold-tier", 10))
		v.QoSPolicyMaxIOPS = sampleVec(policySample("oc", "svm-prod", "gold-tier", 5000))
		v.QoSPolicyMaxMBps = sampleVec(policySample("oc", "svm-prod", "gold-tier", 250))
		tp := parseTopology(v, promql.LabelKeys{})
		require.Len(t, tp.PVCs, 1)
		require.Len(t, tp.StorageEdges, 1)

		q := tp.PVCs[0].QoS()
		require.NotNil(t, q)
		assert.Equal(t, "gold-tier", q.PolicyGroup)
		io := tp.StorageEdges[0].IO
		require.NotNil(t, io)
		require.NotNil(t, q.MaxIOPS)
		assert.InDelta(t, *io.MaxIOPS, *q.MaxIOPS, 1e-12)
		require.NotNil(t, q.MaxBytesPerSec)
		assert.InDelta(t, *io.MaxBytesPerSec, *q.MaxBytesPerSec, 1e-12)
	})

	t.Run("FlexGroup claim carries a ceiling and no edge", func(t *testing.T) {
		v := pvcBindingVectors("c", "db", "mongo", "data", "pvc-fg")
		v.VolumeLabels = sampleVec(volLabelSample("pvc-fg", "oc", "n1", "", "svm-big"))
		v.QoSReadOps = sampleVec(qosSample("pvc-fg", "oc", "svm-big", "gold-tier", 10))
		v.QoSPolicyMaxIOPS = sampleVec(policySample("oc", "svm-big", "gold-tier", 8000))
		tp := parseTopology(v, promql.LabelKeys{})
		require.Len(t, tp.PVCs, 1)
		assert.Empty(t, tp.StorageEdges)

		q := tp.PVCs[0].QoS()
		require.NotNil(t, q)
		assert.Equal(t, "gold-tier", q.PolicyGroup)
		require.NotNil(t, q.MaxIOPS)
		assert.InDelta(t, 8000.0, *q.MaxIOPS, 1e-12)
		assert.Nil(t, q.MaxBytesPerSec)
	})

	t.Run("LUN-only claim carries a ceiling on an edge without metrics", func(t *testing.T) {
		v := pvcBindingVectors("c", "db", "mongo", "data", "pvc-san")
		v.VolumeLabels = sampleVec(volLabelSample("pvc-san", "oc", "n1", "a1", "svm"))
		v.QoSReadOps = sampleVec(lunQosSample("pvc-san", "oc", "svm", "gold-tier", "lun0", 90))
		v.QoSPolicyMaxIOPS = sampleVec(policySample("oc", "svm", "gold-tier", 5000))
		tp := parseTopology(v, promql.LabelKeys{})
		require.Len(t, tp.PVCs, 1)
		require.Len(t, tp.StorageEdges, 1)
		assert.Nil(t, tp.StorageEdges[0].IO)
		require.NotNil(t, tp.PVCs[0].QoS())
		assert.Equal(t, "gold-tier", tp.PVCs[0].QoS().PolicyGroup)
	})

	t.Run("unresolved ceiling leaves the attribute nil", func(t *testing.T) {
		v := pvcBindingVectors("c", "db", "mongo", "data", "pvc-x")
		v.VolumeLabels = sampleVec(volLabelSample("pvc-x", "oc", "n1", "a1", "svm"))
		v.QoSReadOps = sampleVec(qosSample("pvc-x", "oc", "svm", "", 10))
		v.QoSPolicyMaxIOPS = sampleVec(policySample("oc", "svm", "gold", 5000))
		tp := parseTopology(v, promql.LabelKeys{})
		require.Len(t, tp.PVCs, 1)
		assert.Nil(t, tp.PVCs[0].QoS())
	})

	t.Run("claim with no volumename carries none", func(t *testing.T) {
		v := pvcBindingVectors("c", "db", "mongo", "data", "")
		v.VolumeLabels = sampleVec(volLabelSample("data", "oc", "n1", "a1", "svm"))
		v.QoSReadOps = sampleVec(qosSample("data", "oc", "svm", "gold", 10))
		v.QoSPolicyMaxIOPS = sampleVec(policySample("oc", "svm", "gold", 5000))
		tp := parseTopology(v, promql.LabelKeys{})
		require.Len(t, tp.PVCs, 1)
		assert.Nil(t, tp.PVCs[0].QoS())
	})

	t.Run("no Harvest data leaves the attribute nil", func(t *testing.T) {
		tp := parseTopology(pvcBindingVectors("c", "db", "mongo", "data", "pvc-x"), promql.LabelKeys{})
		require.Len(t, tp.PVCs, 1)
		assert.Nil(t, tp.PVCs[0].QoS())
	})
}

// TestParseTopology_PVCQoSIndependentOfVectorOrder pins D6 for the node
// attribute: every claim's QoS value is a pure function of the upstream data,
// however the QoS and fixed-policy vectors happen to be ordered.
func TestParseTopology_PVCQoSIndependentOfVectorOrder(t *testing.T) {
	build := func(shuffle func(model.Vector) model.Vector) map[string]*graph.QoSCeiling {
		v := topologyVectors{
			PVC: sampleVec(
				model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "m", "claim_name": "a", "volume": "a"}},
				model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "m", "claim_name": "b", "volume": "b"}},
				model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "pod": "m", "claim_name": "fg", "volume": "fg"}},
			),
			Pod: sampleVec(model.Sample{Metric: model.Metric{
				"cluster": "c", "namespace": "db", "pod": "m", "uid": "u1", "node": "w0",
			}}),
			PVCInfo: sampleVec(
				model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "persistentvolumeclaim": "a", "volumename": "pvc-a"}},
				model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "persistentvolumeclaim": "b", "volumename": "pvc-b"}},
				model.Sample{Metric: model.Metric{"cluster": "c", "namespace": "db", "persistentvolumeclaim": "fg", "volumename": "pvc-fg"}},
			),
			VolumeLabels: shuffle(sampleVec(
				volLabelSample("pvc-a", "oc", "n1", "a1", "svm"),
				volLabelSample("pvc-b", "oc", "n1", "a1", "svm"),
				volLabelSample("pvc-fg", "oc", "n1", "", "svm"),
			)),
			QoSReadOps: shuffle(sampleVec(
				qosSample("pvc-a", "oc", "svm", "gold", 1),
				qosSample("pvc-a", "oc", "svm", "bronze", 2),
				lunQosSample("pvc-b", "oc", "svm", "silver", "lun0", 3),
				qosSample("pvc-b", "oc", "svm", "User-Best_effort", 4),
				qosSample("pvc-fg", "oc", "svm", "gold", 5),
			)),
			QoSPolicyMaxIOPS: shuffle(sampleVec(
				policySample("oc", "svm", "gold", 5000),
				policySample("oc", "svm", "bronze", 100),
				policySample("oc", "svm", "silver", 700),
			)),
			QoSPolicyMaxMBps: shuffle(sampleVec(
				policySample("oc", "svm", "gold", 250),
				policySample("oc", "svm", "silver", 70),
			)),
		}
		tp := parseTopology(v, promql.LabelKeys{})
		out := map[string]*graph.QoSCeiling{}
		for _, pvc := range tp.PVCs {
			out[pvc.ID()] = pvc.QoS()
		}
		return out
	}

	want := build(func(v model.Vector) model.Vector { return v })
	require.Len(t, want, 3)
	for id, q := range want {
		require.NotNilf(t, q, "%s resolves a ceiling in the baseline", id)
	}

	rng := rand.New(rand.NewPCG(1, 2))
	for range 25 {
		got := build(func(v model.Vector) model.Vector {
			out := slices.Clone(v)
			rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
			return out
		})
		assert.Equal(t, want, got)
	}
}
