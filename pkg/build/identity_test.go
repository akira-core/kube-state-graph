package build

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// sample is a terse fixture constructor: one series with the given labels.
func sample(labels map[string]string) model.Sample {
	m := model.Metric{}
	for k, v := range labels {
		m[model.LabelName(k)] = model.LabelValue(v)
	}
	return model.Sample{Metric: m, Value: 1}
}

func podSeries(cluster, az, env, pod, uid string) model.Sample {
	l := map[string]string{"namespace": "shop", "pod": pod, "uid": uid, "node": "worker-0"}
	if cluster != "" {
		l["cluster"] = cluster
	}
	if az != "" {
		l["az"] = az
	}
	if env != "" {
		l["env"] = env
	}
	return sample(l)
}

// TestParseTopology_ClusterIdentity pins the identity ladder end to end through
// the parse: composition, adoption, the verbatim failure, and the structures
// that must inherit the identity.
func TestParseTopology_ClusterIdentity(t *testing.T) {
	t.Run("same raw name in two zones yields two clusters", func(t *testing.T) {
		tp := parseTopology(topologyVectors{
			Node: sampleVec(
				sample(map[string]string{"cluster": "c1", "node": "worker-0", "az": "us", "env": "dev"}),
				sample(map[string]string{"cluster": "c1", "node": "worker-0", "az": "eu", "env": "prod"}),
			),
		}, promql.LabelKeys{})

		ids := map[string]bool{}
		for _, n := range tp.Nodes {
			ids[n.ID()] = true
		}
		assert.True(t, ids["us-dev-c1/worker-0"])
		assert.True(t, ids["eu-prod-c1/worker-0"])
		assert.False(t, ids["c1/worker-0"], "the raw name must not survive as a cluster")
		assert.Equal(t, []string{"eu-prod-c1", "us-dev-c1"}, tp.ClustersObserved)
	})

	t.Run("every cluster-scoped structure carries the identity", func(t *testing.T) {
		tp := parseTopology(topologyVectors{
			Pod:  sampleVec(podSeries("cluster-alpha", "zone-a", "prod", "checkout", "uid-1")),
			Node: sampleVec(sample(map[string]string{"cluster": "cluster-alpha", "node": "worker-0", "az": "zone-a", "env": "prod"})),
			Service: sampleVec(sample(map[string]string{
				"cluster": "cluster-alpha", "namespace": "shop", "service": "checkout", "az": "zone-a", "env": "prod",
			})),
			PVC: sampleVec(sample(map[string]string{
				"cluster": "cluster-alpha", "namespace": "shop", "pod": "checkout",
				"volume": "data", "claim_name": "checkout-data", "az": "zone-a", "env": "prod",
			})),
		}, promql.LabelKeys{})

		const id = "zone-a-prod-cluster-alpha"
		require.Len(t, tp.Pods, 1)
		assert.Equal(t, id+"/uid-1", tp.Pods[0].ID())
		assert.Equal(t, id, tp.Pods[0].Labels()["cluster"])
		assert.Equal(t, id+"/worker-0", tp.Pods[0].Labels()["node"])
		require.Len(t, tp.PVCs, 1)
		assert.Equal(t, id+"/shop/checkout-data", tp.PVCs[0].ID())
		assert.Contains(t, tp.ServicesByNameNS, serviceKey{id, "shop", "checkout"})
		assert.Equal(t, []string{id}, tp.ClustersObserved)
		assert.Equal(t, graph.ClusterIdentity{AZ: "zone-a", Env: "prod", Name: "cluster-alpha"}, tp.ClusterIdentities[id])
	})

	t.Run("unambiguous raw name is adopted by a joining family", func(t *testing.T) {
		buf := captureLogs(t)
		tp := parseTopology(topologyVectors{
			Pod: sampleVec(podSeries("c1", "us", "dev", "checkout", "uid-1")),
			PVC: sampleVec(sample(map[string]string{
				"cluster": "c1", "namespace": "shop", "pod": "checkout",
				"volume": "data", "claim_name": "data", "az": "us", "env": "dev",
			})),
			// The kubelet leg carries NO az/env pair — it must still join.
			KubeletVolumeUsed: sampleVec(sample(map[string]string{
				"cluster": "c1", "namespace": "shop", "persistentvolumeclaim": "data",
			})),
		}, promql.LabelKeys{})

		require.Len(t, tp.PVCs, 1)
		assert.Equal(t, "us-dev-c1/shop/data", tp.PVCs[0].ID())
		require.NotNil(t, tp.PVCs[0].Usage(), "the unstamped kubelet series must adopt us-dev-c1 and join")
		assert.NotContains(t, buf.String(), "cluster_identity_unresolved")
	})

	t.Run("ambiguous raw name stays verbatim, joins nothing, and warns once", func(t *testing.T) {
		vectors := func(order ...model.Sample) topologyVectors {
			return topologyVectors{
				Pod: sampleVec(
					podSeries("c1", "us", "dev", "checkout", "uid-1"),
					podSeries("c1", "eu", "prod", "checkout", "uid-2"),
				),
				PodOwner: sampleVec(order...),
			}
		}
		owner := sample(map[string]string{
			"cluster": "c1", "namespace": "shop", "pod": "checkout",
			"owner_kind": "Deployment", "owner_name": "checkout", "owner_is_controller": "true",
		})

		buf := captureLogs(t)
		fwd := parseTopology(vectors(owner), promql.LabelKeys{})
		out := buf.String()

		for _, p := range fwd.Pods {
			assert.Nil(t, p.Owner(), "an unresolvable owner series must join no identity")
		}
		assert.Contains(t, out, "cluster_identity_unresolved")
		assert.Contains(t, out, "metric=kube_pod_owner")
		assert.Equal(t, 1, strings.Count(out, "cluster_identity_unresolved"), "one aggregated warn per metric")

		// Order-free: the same input in the other order produces the same clusters.
		rev := parseTopology(vectors(owner), promql.LabelKeys{})
		assert.Equal(t, fwd.ClustersObserved, rev.ClustersObserved)
	})

	t.Run("partially stamped series never composes", func(t *testing.T) {
		tp := parseTopology(topologyVectors{
			Pod: sampleVec(podSeries("c1", "us", "", "checkout", "uid-1")),
		}, promql.LabelKeys{})

		require.Len(t, tp.Pods, 1)
		assert.Equal(t, "c1", tp.Pods[0].Labels()["cluster"], "a half pair must never compose")
		assert.NotContains(t, tp.Pods[0].ID(), "us-")
		assert.Empty(t, tp.ClusterIdentities)
	})

	t.Run("identity table is built from entity families only", func(t *testing.T) {
		tp := parseTopology(topologyVectors{
			Pod: sampleVec(podSeries("c1", "us", "dev", "checkout", "uid-1")),
			PodOwner: sampleVec(sample(map[string]string{
				"cluster": "c1", "namespace": "shop", "pod": "checkout", "az": "eu", "env": "prod",
				"owner_kind": "Deployment", "owner_name": "checkout", "owner_is_controller": "true",
			})),
		}, promql.LabelKeys{})

		assert.Len(t, tp.ClusterIdentities, 1, "an owner series must not add a cluster")
		assert.Contains(t, tp.ClusterIdentities, "us-dev-c1")
		assert.Equal(t, []string{"us-dev-c1"}, tp.ClustersObserved)
		// It composed eu-prod-c1 by step 1 and therefore joined the us-dev pod's
		// owner index under a cluster that holds no entity: no owner resolves.
		require.Len(t, tp.Pods, 1)
		assert.Nil(t, tp.Pods[0].Owner())
	})

	t.Run("cluster-less series composes into the unknown bucket", func(t *testing.T) {
		tp := parseTopology(topologyVectors{
			Pod: sampleVec(podSeries("", "us", "dev", "stray", "uid-9")),
		}, promql.LabelKeys{})

		require.Len(t, tp.Pods, 1)
		assert.Equal(t, "us-dev-unknown/uid-9", tp.Pods[0].ID())
		assert.Equal(t, graph.ClusterIdentity{AZ: "us", Env: "dev", Name: promql.ClusterUnknownValue},
			tp.ClusterIdentities["us-dev-unknown"],
			"the bucket keeps its raw component so ?cluster=unknown still addresses it")
	})

	t.Run("rebound key drives the identity read", func(t *testing.T) {
		tp := parseTopology(topologyVectors{
			Pod: sampleVec(sample(map[string]string{
				"cluster": "c1", "namespace": "shop", "pod": "checkout", "uid": "uid-1",
				"az": "us", "deployment_tier": "dev", "env": "ignored",
			})),
		}, promql.LabelKeys{Env: "deployment_tier"})

		require.Len(t, tp.Pods, 1)
		assert.Equal(t, "us-dev-c1/uid-1", tp.Pods[0].ID())
	})

	t.Run("unstamped estate composes nothing and warns nothing", func(t *testing.T) {
		buf := captureLogs(t)
		tp := parseTopology(topologyVectors{
			Pod:      sampleVec(podSeries("cluster-alpha", "", "", "checkout", "uid-1")),
			PodOwner: sampleVec(sample(map[string]string{"cluster": "cluster-alpha", "namespace": "shop", "pod": "checkout", "owner_kind": "Deployment", "owner_name": "checkout", "owner_is_controller": "true"})),
		}, promql.LabelKeys{})

		require.Len(t, tp.Pods, 1)
		assert.Equal(t, "cluster-alpha/uid-1", tp.Pods[0].ID())
		assert.Nil(t, tp.ClusterIdentities)
		require.NotNil(t, tp.Pods[0].Owner(), "an unstamped estate still joins exactly as before")
		assert.NotContains(t, buf.String(), "cluster_identity_unresolved")
	})
}

// TestBuild_ClusterIdentitiesReachTheGraph pins the one wiring step: the table
// the reader composed must reach the built graph, or the projection-level
// `cluster` filter cannot recover a raw name and every filtered request empties.
func TestBuild_ClusterIdentitiesReachTheGraph(t *testing.T) {
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().
		Instant(mock.Anything, string(promql.QPodInfo), mock.Anything, mock.Anything).
		Return(sampleVec(
			podSeries("c1", "us", "dev", "checkout", "uid-1"),
			podSeries("c1", "eu", "prod", "payments", "uid-2"),
			podSeries("cluster-beta", "", "", "ledger", "uid-3"),
		), nil)
	q.EXPECT().
		Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Vector{}, nil)

	g, err := New(q, Options{}, nil, nil).
		Build(context.Background(), 5*time.Minute, probeTestEnd, promql.Selector{})
	require.NoError(t, err)

	assert.Equal(t, []string{"cluster-beta", "eu-prod-c1", "us-dev-c1"}, g.ClusterNames())
	assert.Equal(t, "c1", g.ClusterRawName("us-dev-c1"))
	assert.Equal(t, "c1", g.ClusterRawName("eu-prod-c1"))
	assert.Equal(t, "cluster-beta", g.ClusterRawName("cluster-beta"),
		"an unstamped cluster has no table entry and stands for itself")
}
