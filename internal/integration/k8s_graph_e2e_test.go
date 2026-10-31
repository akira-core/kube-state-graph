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
	"github.com/akira-core/kube-state-graph/pkg/graph"
)

func (s *GraphSuite) fetchK8sGraph(base string, configure func(url.Values)) cytoscape.Body {
	s.T().Helper()
	resp := s.httpGet(s.k8sGraphURL(base, configure))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var body cytoscape.Body
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// TestK8sGraph ingests a Kubernetes estate beside a full NetApp estate for the
// same claims — Harvest topology, QoS, aggregate and controller series, and
// service-graph edges between the pods — then requests /v1/k8s-graph rooted at
// a node and at a pod. The body must carry the pods, the node, the mounted
// claims and the Application groups, and nothing of the NetApp or service
// side, and the server must never have asked for it.
func (s *GraphSuite) TestK8sGraph() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	s.IngestExpFmt(fmt.Sprintf(`
kube_pod_info{cluster="c1",namespace="kgshop",pod="kg-orders-0",uid="uid-kg-orders-0",node="kg-worker-1",pod_ip="10.1.0.1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="kgshop",pod="kg-orders-1",uid="uid-kg-orders-1",node="kg-worker-2",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="kgshop",pod="kg-web-0",uid="uid-kg-web-0",node="kg-worker-1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="kgshop",pod="kg-other-0",uid="uid-kg-other-0",node="kg-worker-2",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_container_info{cluster="c1",namespace="kgshop",pod="kg-web-0",uid="uid-kg-web-0",container="app",image="reg/app:1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_node_info{cluster="c1",node="kg-worker-1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_node_info{cluster="c1",node="kg-worker-2",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_node_status_condition{cluster="c1",node="kg-worker-1",condition="Ready",status="true",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_owner{cluster="c1",namespace="kgshop",pod="kg-orders-0",owner_kind="ReplicaSet",owner_name="kg-orders-7d9",owner_is_controller="true",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_owner{cluster="c1",namespace="kgshop",pod="kg-orders-1",owner_kind="ReplicaSet",owner_name="kg-orders-7d9",owner_is_controller="true",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_replicaset_owner{cluster="c1",namespace="kgshop",replicaset="kg-orders-7d9",owner_kind="Deployment",owner_name="kg-orders",owner_is_controller="true",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_deployment_annotations{cluster="c1",namespace="kgshop",deployment="kg-orders",annotation_argocd_argoproj_io_tracking_id="kg-checkout:apps/Deployment:kgshop/kg-orders",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="kgshop",persistentvolumeclaim="kg-shared",storageclass="netapp-nas",volumename="pvc-kg-shared",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="kgshop",persistentvolumeclaim="kg-unmounted",storageclass="netapp-nas",volumename="pvc-kg-unmounted",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="kgshop",pod="kg-orders-0",persistentvolumeclaim="kg-shared",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="kgshop",pod="kg-orders-1",persistentvolumeclaim="kg-shared",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kubelet_volume_stats_used_bytes{cluster="c1",namespace="kgshop",persistentvolumeclaim="kg-shared",az="zone-a",env="prod",test=%[1]q} 2048 %[2]d
kubelet_volume_stats_capacity_bytes{cluster="c1",namespace="kgshop",persistentvolumeclaim="kg-shared",az="zone-a",env="prod",test=%[1]q} 8192 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-kg",node="kg-ctl-01",aggr="kg-aggr1",svm="kg_svm",volume="trident_pvc_kg_shared",test=%[1]q} 1 %[2]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-kg",svm="kg_svm",volume="trident_pvc_kg_shared",test=%[1]q} 300 %[2]d
aggr_new_status{az="zone-a",env="prod",cluster="ontap-kg",node="kg-ctl-01",aggr="kg-aggr1",test=%[1]q} 1 %[2]d
node_new_status{az="zone-a",env="prod",cluster="ontap-kg",node="kg-ctl-01",test=%[1]q} 1 %[2]d
traces_service_graph_request_total{client="kg-orders-0",server="kg-web-0",client_k8s_pod_uid="uid-kg-orders-0",server_k8s_pod_uid="uid-kg-web-0",connection_type="virtual_node",cluster="c1",az="zone-a",env="prod",test=%[1]q} 100 %[2]d
ALERTS{alertname="KgPodWarning",alertstate="firing",severity="warning",cluster="c1",namespace="kgshop",pod="kg-orders-0",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
`, disc, t1))
	for _, series := range []string{
		`volume_labels{volume="trident_pvc_kg_shared",test=` + strconv.Quote(disc) + `}`,
		`kube_pod_info{pod="kg-web-0",test=` + strconv.Quote(disc) + `}`,
		`kube_deployment_annotations{deployment="kg-orders",test=` + strconv.Quote(disc) + `}`,
		`kubelet_volume_stats_used_bytes{persistentvolumeclaim="kg-shared",test=` + strconv.Quote(disc) + `}`,
	} {
		s.Require().True(s.WaitForSeries(series, fixedNow, 30*time.Second), "VM did not observe %s", series)
	}

	queries := &queryLog{}
	srv := s.StartAPIServer(func(cfg *config.Config) {}, WithQueryLog(queries))
	const ident = "zone-a-prod-c1"

	assertKubernetesOnly := func(body cytoscape.Body) {
		s.T().Helper()
		for _, n := range body.Elements.Nodes {
			s.Contains([]string{"cluster", "namespace", "application", "controller", "pod", "node", "pvc"}, n.Data.Type,
				"%s is a %s", n.Data.ID, n.Data.Type)
			s.Empty(n.Data.Containers, "%s", n.Data.ID)
			s.Nil(n.Data.QoS, "%s", n.Data.ID)
			s.NotContains(n.Data.Labels, "aggr", "%s", n.Data.ID)
			s.NotContains(n.Data.Labels, "svm", "%s", n.Data.ID)
		}
		for _, e := range body.Elements.Edges {
			s.Contains([]string{"pod-to-node", "pod-mounts-pvc"}, e.Data.Type)
		}
	}

	// A node root: every pod on kg-worker-1, claimless or not, the claim the
	// mounting one holds, and nothing of the other mounter on kg-worker-2.
	nodeBody := s.fetchK8sGraph(srv.URL, func(q url.Values) { q.Set("node", "kg-worker-1") })
	assertKubernetesOnly(nodeBody)
	byID := nodesByID(nodeBody)
	s.Equal([]string{ident}, nodeBody.Clusters)
	s.Contains(byID, ident+"/kg-worker-1")
	s.Contains(byID, ident+"/uid-kg-orders-0")
	s.Contains(byID, ident+"/uid-kg-web-0", "a pod that mounts nothing runs on the root node")
	s.Contains(byID, graph.PVCID(ident, "kgshop", "kg-shared"))
	s.NotContains(byID, ident+"/uid-kg-orders-1", "the other mounter is read, not drawn")
	s.NotContains(byID, ident+"/kg-worker-2")
	s.NotContains(byID, ident+"/uid-kg-other-0")
	s.NotContains(byID, graph.PVCID(ident, "kgshop", "kg-unmounted"), "an unmounted claim is never drawn")
	s.Equal("Ready", byID[ident+"/kg-worker-1"].ReadyStatus)
	s.Equal("kg-checkout", byID[ident+"/uid-kg-orders-0"].Application)
	s.Contains(byID, ident+"/namespace/kgshop/application/kg-checkout", "the Application group is synthesised")
	claim := byID[graph.PVCID(ident, "kgshop", "kg-shared")]
	s.Equal("kg-checkout", claim.Application, "the claim inherits its mounters' Application")
	s.Equal("netapp-nas", claim.StorageClass)
	s.Require().NotNil(claim.Usage)
	pod := byID[ident+"/uid-kg-orders-0"]
	s.Require().NotEmpty(pod.Alerts)
	s.Equal("KgPodWarning", pod.Alerts[0].Name)
	// tlast_over_time returns the fixture sample's own timestamp (whole seconds)
	// as the series value, so the entry's time is when it was last seen firing.
	s.Equal(fixedNow.Unix(), pod.Alerts[0].Time, "alert time = the firing sample's timestamp")
	s.Equal("warning", pod.Status)

	// A pod root: the pod, its node and its claim.
	podBody := s.fetchK8sGraph(srv.URL, func(q url.Values) { q.Set("pod", "kgshop/kg-orders-0") })
	assertKubernetesOnly(podBody)
	podIDs := nodesByID(podBody)
	s.Contains(podIDs, ident+"/uid-kg-orders-0")
	s.Contains(podIDs, ident+"/kg-worker-1")
	s.Contains(podIDs, graph.PVCID(ident, "kgshop", "kg-shared"))
	s.NotContains(podIDs, ident+"/uid-kg-web-0", "web-0 shares the node but is not a root")
	s.NotContains(podIDs, ident+"/uid-kg-orders-1")

	// A root nothing names is an empty 200.
	unknown := s.fetchK8sGraph(srv.URL, func(q url.Values) { q.Set("node", "kg-typo") })
	s.Empty(unknown.Elements.Nodes)
	s.Empty(unknown.Clusters)

	// An identity is not a cluster name.
	identity := s.fetchK8sGraph(srv.URL, func(q url.Values) { q.Set("cluster", ident); q.Set("node", "kg-worker-1") })
	s.Empty(identity.Elements.Nodes)

	// The server never asked for the NetApp or service side.
	s.Empty(queries.harvestQueries(), "no Harvest query is issued")
	queries.mu.Lock()
	defer queries.mu.Unlock()
	for _, q := range queries.items {
		s.NotContains([]string{"traces_service_graph_request_total", "kube_pod_container_info", "kube_service_info", "up"}, q.name)
	}
}
