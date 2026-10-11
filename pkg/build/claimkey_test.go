package build

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// Every seed filters claim rows with the key the parse joins on. An absent
// cluster label and a literal "unknown" are one cluster to the parse
// (bucketCluster), so a binding row of either survives a filter built from a
// seed row of the other; another cluster or namespace does not.
func TestClaimKeyOf_BucketsTheClusterAsTheParseDoes(t *testing.T) {
	keys := promql.LabelKeys{}.OrDefault()
	row := func(pairs ...string) *model.Sample {
		m := model.Metric{"az": "zone-a", "env": "prod", "persistentvolumeclaim": "data"}
		for i := 0; i+1 < len(pairs); i += 2 {
			m[model.LabelName(pairs[i])] = model.LabelValue(pairs[i+1])
		}
		return &model.Sample{Metric: m, Value: 1}
	}
	seed := model.Vector{row("namespace", "shop", "pod", "web-0")} // no cluster label
	byClaim := model.Vector{
		row("cluster", "unknown", "namespace", "shop", "pod", "web-0"),
		row("cluster", "c2", "namespace", "shop", "pod", "web-1"),
		row("namespace", "platform", "pod", "api-0"),
	}

	tracked := trackedClaimKeys(seed, keys)
	got := keepRows(byClaim, func(m model.Metric) bool {
		_, ok := tracked[claimKeyOf(m, keys, bindingClaim(m))]
		return ok
	})
	require.Len(t, got, 1)
	assert.Equal(t, model.LabelValue("unknown"), got[0].Metric["cluster"])
}

// The Application seed and the claim expansion read a claim's own tracking-id
// through one function, so both track the same claims for the same roots.
func TestAnnotatedClaimKeys(t *testing.T) {
	keys := promql.LabelKeys{}.OrDefault()
	rows := model.Vector{
		planKSM("namespace", "shop", "persistentvolumeclaim", "ledger-data", trackingLabel, "billing:/PersistentVolumeClaim:shop/ledger-data"),
		planKSM("namespace", "shop", "persistentvolumeclaim", "orders-data", trackingLabel, "checkout:/PersistentVolumeClaim:shop/orders-data"),
		planKSM("namespace", "shop", trackingLabel, "billing:/PersistentVolumeClaim:shop/nameless"),
	}
	got := annotatedClaimKeys(rows, []string{"billing", ""}, keys)
	require.Len(t, got, 1)
	for k := range got {
		assert.Equal(t, "ledger-data", k.claim)
	}
	assert.Nil(t, annotatedClaimKeys(rows, nil, keys), "no root tracks no claim")
}
