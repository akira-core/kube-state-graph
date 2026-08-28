package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
)

// IdentitySuite exercises the cluster identity against a REAL VictoriaMetrics.
// It owns its own container and — unlike FilterSuite — sets NO ExtraLabels, so
// a series written without an az/env pair genuinely arrives without one. That
// is what makes the ladder's ADOPT step observable end to end; a suite that
// stamps scrape-time defaults can only ever exercise COMPOSE.
type IdentitySuite struct {
	VMSuite
}

func TestIdentitySuite(t *testing.T) {
	t.Parallel()
	suite.Run(t, new(IdentitySuite))
}

// SetupSuite seeds one raw cluster name (`c1`) stamped in two zone/environment
// pairs, a second unambiguous raw name (`c2`) whose kubelet leg carries no pair
// at all, and a call from the us/dev c1 pod to the eu/prod one.
func (s *IdentitySuite) SetupSuite() {
	s.VMSuite.SetupSuite()

	const disc = "identity"
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	const step = 60.0

	s.IngestExpFmt(fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="c1",namespace="shop",pod="checkout",uid="id-us-1",node="worker-0",az="us",env="dev",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="shop",pod="payments",uid="id-eu-1",node="worker-0",az="eu",env="prod",test=%[1]q} 1 %[2]d
kube_node_info{cluster="c1",node="worker-0",az="us",env="dev",test=%[1]q} 1 %[2]d
kube_node_info{cluster="c1",node="worker-0",az="eu",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c2",namespace="shop",pod="ledger",uid="id-us-2",node="worker-0",az="us",env="dev",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c2",namespace="shop",pod="ledger",persistentvolumeclaim="ledger-data",volume="data",az="us",env="dev",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c2",namespace="shop",persistentvolumeclaim="ledger-data",storageclass="netapp-nas",volumename="pvc-c2",az="us",env="dev",test=%[1]q} 1 %[2]d
kubelet_volume_stats_used_bytes{cluster="c2",namespace="shop",persistentvolumeclaim="ledger-data",test=%[1]q} 4096 %[2]d
kube_pod_owner{cluster="c1",namespace="shop",pod="checkout",owner_kind="Deployment",owner_name="checkout",owner_is_controller="true",test=%[1]q} 1 %[2]d
traces_service_graph_request_total{client="checkout",server="payments",cluster="c1",client_k8s_pod_uid="id-us-1",server_k8s_pod_uid="id-eu-1",test=%[1]q} 0 %[3]d
traces_service_graph_request_total{client="checkout",server="payments",cluster="c1",client_k8s_pod_uid="id-us-1",server_k8s_pod_uid="id-eu-1",test=%[1]q} %[4]g %[2]d
`, disc, t1, t0, step))

	s.Require().True(
		s.WaitForSeries(`kube_pod_info{cluster="c1",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the identity fixture")
	s.Require().True(
		s.WaitForSeries(`kubelet_volume_stats_used_bytes{cluster="c2",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the unstamped kubelet leg")
	s.Require().True(
		s.WaitForSeries(`rate(traces_service_graph_request_total{test=`+strconv.Quote(disc)+`}[5m]) > 0`, fixedNow, 30*time.Second),
		"VM did not observe a non-zero service-graph rate")
}

func (s *IdentitySuite) fetch(srv string, configure func(url.Values)) cytoscape.Body {
	s.T().Helper()
	resp := s.httpGet(s.graphURL(srv, configure))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// One raw name stamped in two pairs is TWO clusters: two id spaces, two
// compound groups, and a cross-cluster edge between them — while `?cluster=`
// keeps addressing the RAW name at both the query and the projection layer.
func (s *IdentitySuite) TestSameRawNameSplitsIntoTwoClusters() {
	srv := s.StartAPIServer(nil)
	body := s.fetch(srv.URL, func(q url.Values) {
		q.Set("cluster", "c1")
		q.Set("prune", "false")
	})
	ids := nodeIDs(body)

	s.Contains(ids, "us-dev-c1/id-us-1")
	s.Contains(ids, "eu-prod-c1/id-eu-1")
	s.Contains(ids, "us-dev-c1/worker-0")
	s.Contains(ids, "eu-prod-c1/worker-0", "one node name in two identities is two nodes")
	s.ElementsMatch([]string{"eu-prod-c1", "us-dev-c1"}, body.Clusters)

	for id, n := range ids {
		s.NotEqual("c1", n.Labels["cluster"], "the raw name must appear on no element (%s)", id)
	}

	groups := map[string]bool{}
	for _, n := range body.Elements.Nodes {
		if n.Data.Type == "cluster" {
			groups[n.Data.ID] = true
		}
	}
	s.True(groups["cluster/us-dev-c1"])
	s.True(groups["cluster/eu-prod-c1"])
	s.False(groups["cluster/c1"], "the raw name is not a compound group")

	// The call between them is cross-cluster, and the edge names the CLIENT
	// POD's identity rather than the raw trace label the series carried.
	found := false
	for _, e := range body.Elements.Edges {
		if e.Data.Source == "us-dev-c1/id-us-1" && e.Data.Target == "eu-prod-c1/id-eu-1" {
			found = true
			s.Equal("us-dev-c1", e.Data.Labels["cluster"])
		}
	}
	s.True(found, "the cross-identity call is present")
}

// Ladder step 3: a joining family with no pair whose raw name maps to TWO
// identities cannot be placed, so it joins nothing rather than guessing. The
// aggregated cluster_identity_unresolved warning it raises is pinned by the
// unit test TestParseTopology_ClusterIdentity; here the observable outcome is
// that the owner never lands on either pod.
func (s *IdentitySuite) TestAmbiguousRawNameJoinsNothing() {
	srv := s.StartAPIServer(nil)
	ids := nodeIDs(s.fetch(srv.URL, func(q url.Values) {
		q.Set("cluster", "c1")
		q.Set("prune", "false")
	}))

	pod, ok := ids["us-dev-c1/id-us-1"]
	s.Require().True(ok)
	s.Nil(pod.Owner, "kube_pod_owner carries no pair and c1 is ambiguous: it places nowhere")
}

// az + env + cluster are the identity's three components, so together they pin
// exactly one of the two clusters behind the raw name.
func (s *IdentitySuite) TestZoneAndEnvPinOneIdentity() {
	srv := s.StartAPIServer(nil)
	body := s.fetch(srv.URL, func(q url.Values) {
		q.Set("az", "us")
		q.Set("env", "dev")
		q.Set("cluster", "c1")
		q.Set("prune", "false")
	})
	ids := nodeIDs(body)

	s.Equal([]string{"us-dev-c1"}, body.Clusters)
	s.Contains(ids, "us-dev-c1/id-us-1")
	s.NotContains(ids, "eu-prod-c1/id-eu-1")
}

// Ladder step 2: a joining family that carries NO pair adopts the sole identity
// its raw name maps to, so a partially-stamped estate still joins.
func (s *IdentitySuite) TestUnstampedFamilyAdoptsUnambiguousIdentity() {
	srv := s.StartAPIServer(nil)
	ids := nodeIDs(s.fetch(srv.URL, func(q url.Values) {
		q.Set("cluster", "c2")
		q.Set("prune", "false")
	}))

	pvc, ok := ids["us-dev-c2/shop/ledger-data"]
	s.Require().True(ok, "the claim is loaded under its composed identity")
	s.Require().NotNil(pvc.Usage, "the unstamped kubelet series adopts us-dev-c2 and joins")
	s.Require().NotNil(pvc.Usage.UsedBytes)
	s.InDelta(4096, *pvc.Usage.UsedBytes, 0.5)
}

// `clusters[]` lists identities, which are response values — NOT valid
// `?cluster=` values, since that parameter is matched upstream against the raw
// label.
func (s *IdentitySuite) TestListedIdentityIsNotAClusterFilterValue() {
	srv := s.StartAPIServer(nil)
	body := s.fetch(srv.URL, func(q url.Values) {
		q.Set("cluster", "us-dev-c1")
		q.Set("prune", "false")
	})

	s.Empty(nodeIDs(body), "the identity matches no upstream cluster label")
	s.Empty(body.Clusters)
}
