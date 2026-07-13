package route

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/akira-core/kube-state-graph/pkg/build"
)

func TestParseEnvoyCluster(t *testing.T) {
	cases := []struct {
		name    string
		cluster string
		want    build.RouteDestination
		wantOK  bool
	}{
		{"plain",
			"outbound|8080||reviews.prod.svc.cluster.local",
			build.RouteDestination{Namespace: "prod", Service: "reviews", Port: 8080}, true},
		{"with_subset",
			"outbound|8443|v2|reviews.prod.svc.cluster.local",
			build.RouteDestination{Namespace: "prod", Service: "reviews", Port: 8443, Subset: "v2"}, true},
		{"route_miss_empty", "", build.RouteDestination{}, false},
		{"inbound_direction", "inbound|8080||reviews.prod.svc.cluster.local", build.RouteDestination{}, false},
		{"bad_port", "outbound|http||reviews.prod.svc.cluster.local", build.RouteDestination{}, false},
		{"not_in_cluster_fqdn", "outbound|443||api.example.com", build.RouteDestination{}, false},
		{"too_few_parts", "outbound|8080|reviews.prod.svc.cluster.local", build.RouteDestination{}, false},
		{"bare_service_no_suffix", "outbound|8080||reviews", build.RouteDestination{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseEnvoyCluster(c.cluster)
			assert.Equal(t, c.wantOK, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestOutcomeRank(t *testing.T) {
	// no_route (deepest pipeline progress) must outrank no_listener, which
	// outranks no_gateway — the fold reports the most informative miss.
	assert.Greater(t, outcomeRank(build.RouteNoRoute), outcomeRank(build.RouteNoListenerOnPort))
	assert.Greater(t, outcomeRank(build.RouteNoListenerOnPort), outcomeRank(build.RouteNoGateway))
}
