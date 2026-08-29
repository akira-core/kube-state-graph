package kubegraph

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// recordingBackend records the queries one backend was asked, so a routed
// build can be attributed to the backend the zone selected.
type recordingBackend struct {
	name  string
	asked chan string
}

func (b *recordingBackend) Instant(_ context.Context, name, _ string, _ time.Time) (model.Vector, error) {
	select {
	case b.asked <- name:
	default:
	}
	return model.Vector{}, nil
}

// An engine built over a two-backend table dispatches an ?az=-scoped build to
// that zone's backend only — the routing seam is live through the facade.
func TestNewRouted_ScopedBuildReachesOneZonesBackend(t *testing.T) {
	zoneA := &recordingBackend{name: "zone-a", asked: make(chan string, 256)}
	zoneB := &recordingBackend{name: "zone-b", asked: make(chan string, 256)}

	k8sFamilies := []promql.Family{promql.FamilyKSM, promql.FamilyKubelet, promql.FamilyServiceGraph, promql.FamilyProbe, promql.FamilyHarvest}
	table, err := promql.NewTable([]promql.Backend{
		promql.NewBackend("zone-a", "http://vm-a:8428", k8sFamilies, []string{"zone-a"}, "", ""),
		promql.NewBackend("zone-b", "http://vm-b:8428", k8sFamilies, []string{"zone-b"}, "", ""),
	})
	require.NoError(t, err)

	byName := map[string]promql.Querier{"zone-a": zoneA, "zone-b": zoneB}
	router, err := promql.NewRouter(table, nil, func(b promql.Backend) (promql.Querier, error) {
		return byName[b.Name()], nil
	})
	require.NoError(t, err)

	engine := NewRouted(router, Options{APITimeout: time.Second})

	end := time.Unix(1_700_000_000, 0).UTC()
	_, err = engine.Build(context.Background(), 5*time.Minute, end, promql.Selector{AZ: []string{"zone-a"}})
	require.NoError(t, err)

	assert.NotEmpty(t, zoneA.asked, "the selected zone's backend answers the build")

	// The zone-scoped families never reach zone-b; the families that accept no
	// zone dimension (service-graph, probe) still fan out to it.
	for len(zoneB.asked) > 0 {
		name := <-zoneB.asked
		fam, ok := promql.FamilyOf(promql.Query(name))
		require.True(t, ok, "query %q has no family", name)
		assert.False(t, fam.AcceptsAZ(), "zone-routed query %q must not reach the other zone's backend", name)
	}
}
