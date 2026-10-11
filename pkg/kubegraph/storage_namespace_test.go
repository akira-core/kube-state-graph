package kubegraph_test

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/kubegraph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

func nsKSM(pairs ...string) *model.Sample {
	m := model.Metric{"cluster": "c1", "az": "zone-a", "env": "prod"}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[model.LabelName(pairs[i])] = model.LabelValue(pairs[i+1])
	}
	return &model.Sample{Metric: m, Value: 1}
}

// nsHarvest is one Harvest series stamped with the fixture zone every Harvest
// series must carry; a pair overrides it.
func nsHarvest(pairs ...string) *model.Sample {
	m := model.Metric{"az": "zone-a", "env": "prod"}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[model.LabelName(pairs[i])] = model.LabelValue(pairs[i+1])
	}
	return &model.Sample{Metric: m, Value: 1}
}

// namespaceEstate spreads NetApp-backed claims over three namespaces. Two of
// them hold the pod roots; the third, `other`, holds a claim on the SAME
// aggregate as a root's claim, plus a firing pod alert, so a derived namespace
// that wrongly dropped or kept something would move a weight, an alert or a
// node.
func namespaceEstate() map[promql.Query]model.Vector {
	type pod struct{ ns, name, uid, claim, volume, flexvol string }
	pods := []pod{
		{"shop", "orders-0", "uid-o0", "orders-data", "pvc-orders", "trident_pvc_orders"},
		{"platform", "redis-0", "uid-r0", "redis-data", "pvc-redis", "trident_pvc_redis"},
		{"other", "batch-0", "uid-b0", "batch-data", "pvc-batch", "trident_pvc_batch"},
	}
	out := map[promql.Query]model.Vector{
		promql.QNodeInfo: {nsKSM("node", "worker-1")},
		promql.QVolumeLabels: {
			nsHarvest("cluster", "ontap-prod", "node", "ontap-prod-01", "aggr", "aggr1", "svm", "svm0", "volume", "trident_pvc_orders"),
			nsHarvest("cluster", "ontap-prod", "node", "ontap-prod-02", "aggr", "aggr2", "svm", "svm0", "volume", "trident_pvc_redis"),
			nsHarvest("cluster", "ontap-prod", "node", "ontap-prod-01", "aggr", "aggr1", "svm", "svm0", "volume", "trident_pvc_batch"),
		},
		promql.QAlerts: {
			nsKSM("alertname", "PodWarning", "alertstate", "firing", "severity", "warning", "namespace", "other", "pod", "batch-0"),
			nsHarvest("alertname", "AggrFilling", "alertstate", "firing", "severity", "warning",
				"cluster", "ontap-prod", "aggr", "aggr1", "az", "zone-a", "env", "prod"),
		},
	}
	for _, p := range pods {
		out[promql.QPodInfo] = append(out[promql.QPodInfo], nsKSM("namespace", p.ns, "pod", p.name, "uid", p.uid, "node", "worker-1"))
		out[promql.QPodOwner] = append(out[promql.QPodOwner], nsKSM("namespace", p.ns, "pod", p.name,
			"owner_kind", "StatefulSet", "owner_name", p.name, "owner_is_controller", "true"))
		out[promql.QPVCBindings] = append(out[promql.QPVCBindings], nsKSM("namespace", p.ns, "pod", p.name, "persistentvolumeclaim", p.claim))
		out[promql.QPVCInfo] = append(out[promql.QPVCInfo], nsKSM("namespace", p.ns, "persistentvolumeclaim", p.claim, "volumename", p.volume))
		used := nsKSM("namespace", p.ns, "persistentvolumeclaim", p.claim)
		used.Value = 1024
		out[promql.QKubeletVolumeUsedBytes] = append(out[promql.QKubeletVolumeUsedBytes], used)
		qos := nsHarvest("cluster", "ontap-prod", "svm", "svm0", "volume", p.flexvol)
		qos.Value = 100
		out[promql.QQoSReadOps] = append(out[promql.QQoSReadOps], qos)
	}
	return out
}

// A pod root is keyed by the pod ref. It does not push the roots' namespaces
// into the selector.
func TestParseStorageValues_PodRootDoesNotDeriveNamespace(t *testing.T) {
	vals := storageBase()
	vals["pod"] = []string{"shop/orders-0", "platform/redis-0"}

	q := promqlfake.New(namespaceEstate())
	body, err := kubegraph.New(q, kubegraph.Options{}).BuildStorageFromValues(t.Context(), vals)
	require.NoError(t, err)
	require.NotEmpty(t, body.Elements.Edges, "a vacuous body would prove nothing")

	req, err := kubegraph.ParseStorageValues(vals)
	require.NoError(t, err)
	assert.Empty(t, req.Selector.Namespace)
	// The seed read comes first; mounter completion then reads the tracked
	// claims one namespace at a time, concurrently, so only its set is pinned.
	bindings := q.QueriesFor(promql.QPVCBindings)
	require.Len(t, bindings, 3)
	assert.Equal(t,
		`last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info{az="zone-a",env="prod",namespace=~"platform|shop",pod=~"orders-0|redis-0"}[1h])`,
		bindings[0], "bindings are the pod seed first")
	assert.ElementsMatch(t, []string{
		`last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info{az="zone-a",env="prod",namespace="platform",persistentvolumeclaim="redis-data"}[1h])`,
		`last_over_time(kube_pod_spec_volumes_persistentvolumeclaims_info{az="zone-a",env="prod",namespace="shop",persistentvolumeclaim="orders-data"}[1h])`,
	}, bindings[1:], "then mounter completion by (namespace, claim)")
	assert.ElementsMatch(t, []string{
		`last_over_time(kube_persistentvolumeclaim_info{az="zone-a",env="prod",namespace="platform",persistentvolumeclaim="redis-data"}[1h])`,
		`last_over_time(kube_persistentvolumeclaim_info{az="zone-a",env="prod",namespace="shop",persistentvolumeclaim="orders-data"}[1h])`,
	}, q.QueriesFor(promql.QPVCInfo), "claim info is read by the tracked (namespace, claim) pairs")
	assert.Equal(t, []string{
		`last_over_time(ALERTS{alertstate="firing",az="zone-a",env="prod"}[1h])`,
	}, q.QueriesFor(promql.QAlerts))
}
