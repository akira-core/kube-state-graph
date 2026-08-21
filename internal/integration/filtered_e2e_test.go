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

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
)

// FilterSuite exercises the request-scoped upstream selectors against a REAL
// VictoriaMetrics: the mock-based tests can only prove which query string was
// issued, so this is where the store actually applies the matchers and the
// resulting narrowed topology drives the filtered-build rules.
type FilterSuite struct {
	VMSuite
}

func TestFilterSuite(t *testing.T) {
	// Each suite owns its own container, so the suites are independent; go
	// test otherwise runs them one after another and the wall clock is their
	// sum. Tests INSIDE a suite stay sequential (testify shares suite state).
	t.Parallel()
	suite.Run(t, new(FilterSuite))
}

// SetupTest seeds one estate spanning two zones, two clusters, two namespaces
// and one NetApp aggregate. Every series is stamped `az="zone-a",env="prod"`
// by ExtraLabels unless it spells the key out itself, which models the
// scrape-time external labels a real deployment applies.
//
//   - cluster-alpha (zone-a/prod): checkout + cart in `shop`, ledger in
//     `finance`, idle in `shop` (no traffic, mounts a NetApp-backed claim),
//     nodes worker-0 (hosts pods) and worker-9 (hosts none).
//   - cluster-beta  (zone-b/prod): payments in `billing`.
//   - one pod carrying NO cluster label at all (the `unknown` bucket).
//
// Traffic: checkout→cart (same namespace), checkout→payments (cross-cluster,
// cross-zone), checkout→ledger (cross-namespace).
// SetupSuite seeds the filtered-build fixture ONCE — see GraphSuite.SetupSuite
// for why this is not SetupTest (VM charges ~10s to make a brand-new series
// queryable, and a per-test discriminator label re-registered the whole set on
// every test).
func (s *FilterSuite) SetupSuite() {
	s.VMSuite.SetupSuite()

	s.ExtraLabels = `az="zone-a",env="prod"`

	const disc = "base"
	t1 := fixedNow.Unix() * 1000
	t0 := fixedNow.Add(-time.Minute).Unix() * 1000
	const step = 60.0

	exposition := fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="checkout",uid="alpha-1",node="worker-0",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="cart",uid="alpha-2",node="worker-0",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="cluster-alpha",namespace="finance",pod="ledger",uid="alpha-3",node="worker-0",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="idle",uid="alpha-4",node="worker-9",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="cluster-beta",namespace="billing",pod="payments",uid="beta-1",node="worker-0",az="zone-b",test=%[1]q} 1 %[2]d
kube_pod_info{namespace="orphan",pod="stray",uid="nocluster-1",node="worker-0",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="unknown",namespace="orphan",pod="literal",uid="nocluster-2",node="worker-0",test=%[1]q} 1 %[2]d
# HELP kube_node_info dummy
kube_node_info{cluster="cluster-alpha",node="worker-0",test=%[1]q} 1 %[2]d
kube_node_info{cluster="cluster-alpha",node="worker-9",test=%[1]q} 1 %[2]d
kube_node_info{cluster="cluster-beta",node="worker-0",az="zone-b",test=%[1]q} 1 %[2]d
# HELP kube_pod_spec_volumes_persistentvolumeclaims_info dummy
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="cluster-alpha",namespace="shop",pod="idle",persistentvolumeclaim="idle-data",volume="data",test=%[1]q} 1 %[2]d
# HELP kube_persistentvolumeclaim_info dummy
kube_persistentvolumeclaim_info{cluster="cluster-alpha",namespace="shop",persistentvolumeclaim="idle-data",storageclass="netapp-nas",volumename="pvc-idle",test=%[1]q} 1 %[2]d
# HELP volume_labels dummy
volume_labels{cluster="ontap-prod",node="ontap-prod-01",aggr="aggr1",svm="svm-prod",volume_name="pvc-idle",test=%[1]q} 1 %[2]d
# HELP aggr_new_status dummy
aggr_new_status{cluster="ontap-prod",node="ontap-prod-01",aggr="aggr1",test=%[1]q} 1 %[2]d
# HELP node_new_status dummy
node_new_status{cluster="ontap-prod",node="ontap-prod-01",test=%[1]q} 1 %[2]d
# HELP traces_service_graph_request_total dummy
traces_service_graph_request_total{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="alpha-2",test=%[1]q} 0 %[3]d
traces_service_graph_request_total{client="checkout",server="cart",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="alpha-2",test=%[1]q} %[4]g %[2]d
traces_service_graph_request_total{client="checkout",server="payments",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="beta-1",test=%[1]q} 0 %[3]d
traces_service_graph_request_total{client="checkout",server="payments",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="beta-1",test=%[1]q} %[4]g %[2]d
traces_service_graph_request_total{client="checkout",server="ledger",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="alpha-3",test=%[1]q} 0 %[3]d
traces_service_graph_request_total{client="checkout",server="ledger",cluster="cluster-alpha",client_k8s_pod_uid="alpha-1",server_k8s_pod_uid="alpha-3",test=%[1]q} %[4]g %[2]d
`, disc, t1, t0, 5.0*step)

	s.IngestExpFmt(exposition)
	s.Require().True(s.WaitForSeries(`kube_pod_info{test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe ingested kube_pod_info")
	s.Require().True(
		s.WaitForSeries(`rate(traces_service_graph_request_total{test=`+strconv.Quote(disc)+`}[5m]) > 0`, fixedNow, 30*time.Second),
		"VM did not observe non-zero service-graph rate")
}

// fetch runs one /v1/graph request and returns its decoded body.
func (s *FilterSuite) fetch(srv string, configure func(url.Values)) cytoscape.Body {
	s.T().Helper()
	resp := s.httpGet(s.graphURL(srv, configure))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// nodeIDs returns the set of node ids in a body, restricted to real graph
// types (the synthesised compound groups are presentation-only).
func nodeIDs(body cytoscape.Body) map[string]cytoscape.NodeData {
	out := map[string]cytoscape.NodeData{}
	for _, n := range body.Elements.Nodes {
		switch n.Data.Type {
		case "cluster", "namespace", "application", "controller", "storage-cluster":
			continue
		}
		out[n.Data.ID] = n.Data
	}
	return out
}

// A zone filter narrows the estate at the source: only zone-a workload is
// loaded, `clusters` lists only its cluster, and the cross-zone peer renders
// as an external node rather than a real pod.
func (s *FilterSuite) TestAZFilterNarrowsEstate() {
	srv := s.StartAPIServer(nil)
	body := s.fetch(srv.URL, func(q url.Values) { q.Set("az", "zone-a") })

	ids := nodeIDs(body)
	s.Contains(ids, "cluster-alpha/alpha-1", "zone-a caller present")
	s.Contains(ids, "cluster-alpha/alpha-2", "zone-a callee present")
	s.NotContains(ids, "cluster-beta/beta-1", "zone-b pod is not loaded")
	s.Equal([]string{"cluster-alpha"}, body.Clusters)

	ext, ok := ids["external/payments"]
	s.Require().True(ok, "the out-of-zone peer renders as an external node")
	s.Equal("external", ext.Type)
	s.Empty(ext.Labels, "an external node carries no labels")
}

// A selector that matches nothing is an empty 200 — never outside_retention.
func (s *FilterSuite) TestEnvFilterMissReturnsEmpty200() {
	srv := s.StartAPIServer(nil)
	body := s.fetch(srv.URL, func(q url.Values) { q.Set("env", "staging") })

	s.Empty(body.Elements.Nodes)
	s.Empty(body.Elements.Edges)
	s.Empty(body.Clusters)
}

// The namespace push-down loads only the requested namespace's pods; the
// cross-namespace peer becomes an external, and the host node follows by
// reference (it carries no namespace of its own).
func (s *FilterSuite) TestNamespacePushDownNarrowsTopology() {
	srv := s.StartAPIServer(nil)
	body := s.fetch(srv.URL, func(q url.Values) { q.Set("namespace", "shop") })

	ids := nodeIDs(body)
	s.Contains(ids, "cluster-alpha/alpha-1")
	s.Contains(ids, "cluster-alpha/alpha-2")
	s.NotContains(ids, "cluster-alpha/alpha-3", "the finance pod is not loaded")
	s.Contains(ids, "external/ledger", "the out-of-namespace peer renders as an external")
	s.Contains(ids, "cluster-alpha/worker-0", "the host node follows the in-scope pods by reference")
	s.NotContains(ids, "cluster-alpha/worker-9", "a node hosting no in-scope pod is not admitted")

	for id, n := range ids {
		if n.Type == "pod" {
			s.Equal("shop", n.Labels["namespace"], "no out-of-namespace pod survived: %s", id)
		}
	}
}

// A cluster filter renders the out-of-cluster partner as an external node —
// the partner's topology was never loaded, so it cannot be a real pod.
func (s *FilterSuite) TestClusterFilterRendersExternalPartner() {
	srv := s.StartAPIServer(nil)
	body := s.fetch(srv.URL, func(q url.Values) { q.Set("cluster", "cluster-alpha") })

	ids := nodeIDs(body)
	s.Contains(ids, "cluster-alpha/alpha-1")
	s.NotContains(ids, "cluster-beta/beta-1")
	s.Contains(ids, "external/payments")
	s.Equal([]string{"cluster-alpha"}, body.Clusters)

	var found bool
	for _, e := range body.Elements.Edges {
		if e.Data.Source == "cluster-alpha/alpha-1" && e.Data.Target == "external/payments" {
			found = true
			s.Equal("pod-calls-pod", e.Data.Type)
			s.Equal("cluster-alpha", e.Data.Labels["cluster"], "the client side is still a loaded pod")
		}
	}
	s.True(found, "the edge to the externalised partner is emitted")
}

// `cluster=unknown` addresses the bucket of series carrying no cluster label,
// via the empty-string matcher.
func (s *FilterSuite) TestClusterUnknownMatchesUnlabelledSeries() {
	srv := s.StartAPIServer(nil)
	body := s.fetch(srv.URL, func(q url.Values) {
		q.Set("cluster", "unknown")
		q.Set("prune", "false")
	})

	ids := nodeIDs(body)
	s.Contains(ids, "unknown/nocluster-1", "the unlabelled pod is addressable as cluster=unknown")
	// Both spellings of the bucket must come back: bucketCluster maps an absent
	// label to "unknown", so a series LABELLED "unknown" is indistinguishable
	// from it downstream and the rendered matcher must accept both.
	s.Contains(ids, "unknown/nocluster-2", "a literal cluster=\"unknown\" series is in the same bucket")
	s.NotContains(ids, "cluster-alpha/alpha-1", "labelled series are excluded")
	s.Equal([]string{"unknown"}, body.Clusters)
}

// A mixed `cluster` set including `unknown` renders one anchored alternation
// whose empty alternative still matches the unlabelled series.
func (s *FilterSuite) TestClusterUnknownMixedWithRealCluster() {
	srv := s.StartAPIServer(nil)
	body := s.fetch(srv.URL, func(q url.Values) {
		q.Add("cluster", "cluster-alpha")
		q.Add("cluster", "unknown")
		q.Set("prune", "false")
	})

	ids := nodeIDs(body)
	s.Contains(ids, "unknown/nocluster-1")
	s.Contains(ids, "unknown/nocluster-2")
	s.Contains(ids, "cluster-alpha/alpha-1")
	s.NotContains(ids, "cluster-beta/beta-1")
}

// prune=false surfaces the connectivity-disconnected pod with its whole
// storage chain, plus the podless node — the inventory view.
func (s *FilterSuite) TestPruneFalseSurfacesIdleWorkloadAndPodlessNode() {
	srv := s.StartAPIServer(nil)

	pruned := nodeIDs(s.fetch(srv.URL, nil))
	s.NotContains(pruned, "cluster-alpha/alpha-4", "the idle pod is pruned by default")
	s.NotContains(pruned, "cluster-alpha/worker-9")

	all := nodeIDs(s.fetch(srv.URL, func(q url.Values) { q.Set("prune", "false") }))
	s.Contains(all, "cluster-alpha/alpha-4", "prune=false surfaces the idle pod")
	s.Contains(all, "cluster-alpha/shop/idle-data", "with its claim")
	s.Contains(all, "netapp/ontap-prod/aggr/aggr1", "and the aggregate behind it")
	s.Contains(all, "netapp/ontap-prod/ontap-prod-01", "and the owning controller")
	s.Contains(all, "cluster-alpha/worker-9", "and the podless node")
}

// Under a namespace filter, prune=false stays reference-driven for
// infrastructure: it surfaces the namespace's own idle workload but no node
// that hosts none of it.
func (s *FilterSuite) TestPruneFalseUnderNamespaceFilterStaysScoped() {
	srv := s.StartAPIServer(nil)
	ids := nodeIDs(s.fetch(srv.URL, func(q url.Values) {
		q.Set("namespace", "shop")
		q.Set("prune", "false")
	}))

	s.Contains(ids, "cluster-alpha/alpha-4", "the namespace's idle pod is surfaced")
	s.Contains(ids, "cluster-alpha/worker-9", "its host node follows by reference")
	s.NotContains(ids, "cluster-alpha/alpha-3", "the finance pod is still out of scope")
}

// The unfiltered request is unaffected by any of this: every pod of every
// cluster that carries traffic is present, and the cross-cluster partner is a
// REAL pod, not an external.
func (s *FilterSuite) TestUnfilteredRequestKeepsRealCrossClusterPartner() {
	srv := s.StartAPIServer(nil)
	ids := nodeIDs(s.fetch(srv.URL, nil))

	s.Contains(ids, "cluster-alpha/alpha-1")
	s.Contains(ids, "cluster-beta/beta-1", "both clusters are loaded, so the partner is real")
	s.NotContains(ids, "external/payments")
	s.ElementsMatch([]string{"cluster-alpha", "cluster-beta"}, s.fetch(srv.URL, nil).Clusters)
}

// A metric family that does NOT carry the configured env label vanishes from a
// filtered request. The Kubernetes topology still resolves; only the NetApp
// chain behind the mislabelled series disappears — the operator precondition
// the specs record.
func (s *FilterSuite) TestHarvestWithoutEnvLabelDegrades() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	// A second claim whose ONLY Harvest series carries a different env value,
	// so `?env=prod` cannot match it. Its pod and claim keep the suite's
	// stamped labels and are loaded normally.
	s.IngestExpFmt(fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="cluster-alpha",namespace="shop",pod="lonely",uid="alpha-9",node="worker-0",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="cluster-alpha",namespace="shop",pod="lonely",persistentvolumeclaim="lonely-data",volume="data",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="cluster-alpha",namespace="shop",persistentvolumeclaim="lonely-data",storageclass="netapp-nas",volumename="pvc-lonely",test=%[1]q} 1 %[2]d
volume_labels{cluster="ontap-prod",node="ontap-prod-02",aggr="aggr9",svm="svm-prod",volume_name="pvc-lonely",env="other",test=%[1]q} 1 %[2]d
`, disc, t1))
	s.Require().True(
		s.WaitForSeries(`volume_labels{volume_name="pvc-lonely",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the mislabelled volume_labels series")

	srv := s.StartAPIServer(nil)
	body := s.fetch(srv.URL, func(q url.Values) {
		q.Set("env", "prod")
		q.Set("prune", "false")
	})
	ids := nodeIDs(body)

	s.Contains(ids, "cluster-alpha/shop/lonely-data", "the claim itself is still loaded")
	s.NotContains(ids, "netapp/ontap-prod/aggr/aggr9", "the mislabelled aggregate never matched the env filter")
	s.NotContains(ids, "netapp/ontap-prod/ontap-prod-02", "nor its controller")

	for _, e := range body.Elements.Edges {
		s.NotEqual("cluster-alpha/shop/lonely-data", e.Data.Source,
			"the claim joins no aggregate when its Harvest series is filtered out")
	}
}

// A rebound label key changes the MATCHER, never the request parameter name.
func (s *FilterSuite) TestConfiguredLabelKeyIsUsed() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	s.IngestExpFmt(fmt.Sprintf(`# HELP kube_pod_info dummy
kube_pod_info{cluster="cluster-gamma",namespace="ops",pod="tool",uid="gamma-1",node="worker-0",topology_zone="zone-x",test=%[1]q} 1 %[2]d
kube_node_info{cluster="cluster-gamma",node="worker-0",topology_zone="zone-x",test=%[1]q} 1 %[2]d
`, disc, t1))
	s.Require().True(
		s.WaitForSeries(`kube_pod_info{topology_zone="zone-x",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the rebound-key fixture")

	srv := s.StartAPIServer(func(cfg *config.Config) { cfg.AZLabel = "topology_zone" })
	ids := nodeIDs(s.fetch(srv.URL, func(q url.Values) {
		q.Set("az", "zone-x")
		q.Set("prune", "false")
	}))

	s.Contains(ids, "cluster-gamma/gamma-1", "matched through the rebound key")
	s.NotContains(ids, "cluster-alpha/alpha-1", "series carrying only the default key do not match")
}
