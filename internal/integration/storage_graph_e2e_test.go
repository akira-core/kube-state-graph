package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/akira-core/kube-state-graph/internal/config"
	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
)

// TestStorageGraph ingests a two-SVM, two-aggregate estate with a shared RWX
// claim and firing ALERTS, then requests /v1/storage-graph from both root
// sides.
func (s *GraphSuite) TestStorageGraph() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	s.IngestExpFmt(fmt.Sprintf(`
kube_pod_info{cluster="c1",namespace="shop",pod="rwx-0",uid="uid-rwx-0",node="worker-1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="shop",pod="rwx-1",uid="uid-rwx-1",node="worker-1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_node_info{cluster="c1",node="worker-1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="shop",persistentvolumeclaim="shared-data",storageclass="netapp-nas",volumename="pvc-shared",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="shop",pod="rwx-0",persistentvolumeclaim="shared-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="shop",pod="rwx-1",persistentvolumeclaim="shared-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
volume_labels{cluster="ontap-prod",node="ontap-prod-01",aggr="aggr1",svm="svm_shop",volume="trident_pvc_shared",test=%[1]q} 1 %[2]d
volume_labels{cluster="ontap-prod",node="ontap-prod-02",aggr="aggr2",svm="svm_other",volume="trident_pvc_other",test=%[1]q} 1 %[2]d
qos_read_ops{cluster="ontap-prod",svm="svm_shop",volume="trident_pvc_shared",test=%[1]q} 300 %[2]d
volume_labels{cluster="ontap-prod",node="ontap-prod-02",aggr="aggr9",svm="svm_idle",volume="vol_unclaimed",test=%[1]q} 1 %[2]d
aggr_new_status{cluster="ontap-prod",node="ontap-prod-02",aggr="aggr9",test=%[1]q} 1 %[2]d
node_new_status{cluster="ontap-prod",node="ontap-prod-01",test=%[1]q} 1 %[2]d
node_labels{cluster="ontap-prod",node="ontap-prod-01",model="AFF-A400",version="9.14.1",vendor="NetApp",test=%[1]q} 1 %[2]d
node_cpu_busy{cluster="ontap-prod",node="ontap-prod-01",test=%[1]q} 72.5 %[2]d
node_total_ops{cluster="ontap-prod",node="ontap-prod-01",test=%[1]q} 18500 %[2]d
ALERTS{alertname="KubePodCrashLooping",alertstate="firing",severity="warning",cluster="c1",namespace="shop",pod="rwx-0",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
`, disc, t1))
	s.Require().True(
		s.WaitForSeries(`volume_labels{volume="trident_pvc_shared",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the storage-graph volume_labels")

	srv := s.StartAPIServer(func(cfg *config.Config) {})

	aggrBody := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("aggr", "aggr1") })
	s.assertStorageConservation(aggrBody)
	s.assertHasSplit(aggrBody)
	byID := nodesByID(aggrBody)
	s.Contains(byID, "netapp/ontap-prod/aggr/aggr1")
	s.Contains(byID, "netapp/ontap-prod/ontap-prod-01")
	// az=zone-a env=prod compose the cluster identity zone-a-prod-c1.
	const ident = "zone-a-prod-c1"
	s.Contains(byID, ident+"/uid-rwx-0")
	s.Contains(byID, ident+"/uid-rwx-1")
	ctrl := byID["netapp/ontap-prod/ontap-prod-01"]
	s.Require().NotNil(ctrl.Hardware)
	s.Equal("AFF-A400", ctrl.Hardware.Model)
	s.Require().NotNil(ctrl.Perf)
	s.Require().NotNil(ctrl.Perf.CPUBusyPct)
	s.InDelta(72.5, *ctrl.Perf.CPUBusyPct, 1e-9)
	pod := byID[ident+"/uid-rwx-0"]
	s.Require().NotEmpty(pod.Alerts)
	s.Equal("KubePodCrashLooping", pod.Alerts[0].Name)

	podBody := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("pod", "shop/rwx-0") })
	s.assertStorageConservation(podBody)
	s.assertHasSplit(podBody)
	s.Contains(nodesByID(podBody), ident+"/uid-rwx-0")
	s.NotContains(nodesByID(podBody), ident+"/uid-rwx-1", "the other RWX mounter is not this pod root")

	claimless := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("aggr", "aggr9") })
	ids := nodesByID(claimless)
	s.Contains(ids, "netapp/ontap-prod/aggr/aggr9")
	s.Contains(ids, "netapp/ontap-prod/ontap-prod-02", "owning controller always pulled")
	s.Empty(claimless.Elements.Edges)

	unknown := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("aggr", "typo") })
	s.Empty(unknown.Elements.Nodes)
	s.Empty(unknown.Elements.Edges)
	s.Empty(unknown.Clusters)
}

func (s *GraphSuite) fetchStorageGraph(base string, configure func(url.Values)) cytoscape.Body {
	s.T().Helper()
	resp := s.httpGet(s.storageGraphURL(base, configure))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	return body
}

func nodesByID(body cytoscape.Body) map[string]cytoscape.NodeData {
	out := map[string]cytoscape.NodeData{}
	for _, n := range body.Elements.Nodes {
		out[n.Data.ID] = n.Data
	}
	return out
}

func (s *GraphSuite) assertHasSplit(body cytoscape.Body) {
	s.T().Helper()
	found := false
	for _, e := range body.Elements.Edges {
		if e.Data.Labels["attribution"] == "split" {
			found = true
			s.Equal("pvc-pod", e.Data.Labels["tier"])
		}
	}
	s.True(found, "RWX pvc-pod edges must carry attribution=split")
}

func (s *GraphSuite) assertStorageConservation(body cytoscape.Body) {
	s.T().Helper()
	type bucket struct{ in, out float64 }
	by := map[string]*bucket{}
	ensure := func(id string) *bucket {
		b, ok := by[id]
		if !ok {
			b = &bucket{}
			by[id] = b
		}
		return b
	}
	for _, e := range body.Elements.Edges {
		if e.Data.Type != "storage-flow" || e.Data.Metrics == nil || e.Data.Metrics.ReadOps == nil {
			continue
		}
		v := *e.Data.Metrics.ReadOps
		ensure(e.Data.Source).out += v
		ensure(e.Data.Target).in += v
	}
	kind := map[string]string{}
	for _, n := range body.Elements.Nodes {
		kind[n.Data.ID] = n.Data.Type
	}
	for id, b := range by {
		switch kind[id] {
		case "pvc", "pod", "netapp-aggr":
			if b.in == 0 && b.out == 0 {
				continue
			}
			s.InDelta(b.in, b.out, 1e-6, "conservation at %s (%s)", id, kind[id])
		}
	}
}
