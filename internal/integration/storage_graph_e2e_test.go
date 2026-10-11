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

// TestStorageGraph ingests a two-SVM, two-aggregate estate with a shared RWX
// claim and firing ALERTS, then requests /v1/storage-graph from both root
// sides.
func (s *GraphSuite) TestStorageGraph() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	s.IngestExpFmt(fmt.Sprintf(`
kube_pod_info{cluster="c1",namespace="shop",pod="rwx-0",uid="uid-rwx-0",node="worker-1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="shop",pod="rwx-1",uid="uid-rwx-1",node="worker-1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="shop",pod="catalog-0",uid="uid-catalog-0",node="worker-1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="shop",pod="idle-0",uid="uid-idle-0",node="worker-1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_container_info{cluster="c1",namespace="shop",pod="rwx-0",uid="uid-rwx-0",container="app",image="reg/app:1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_node_info{cluster="c1",node="worker-1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="shop",persistentvolumeclaim="shared-data",storageclass="netapp-nas",volumename="pvc-shared",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="shop",persistentvolumeclaim="catalog-data",storageclass="netapp-nas",volumename="pvc-catalog",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="shop",pod="rwx-0",persistentvolumeclaim="shared-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="shop",pod="rwx-1",persistentvolumeclaim="shared-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="shop",pod="catalog-0",persistentvolumeclaim="catalog-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-prod-01",aggr="aggr1",svm="svm_shop",volume="trident_pvc_shared",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-prod-02",aggr="aggr2",svm="svm_shop",volume="trident_pvc_catalog",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-prod-02",aggr="aggr2",svm="svm_other",volume="trident_pvc_other",test=%[1]q} 1 %[2]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-prod",svm="svm_shop",volume="trident_pvc_shared",test=%[1]q} 300 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-prod-02",aggr="aggr9",svm="svm_idle",volume="vol_unclaimed",test=%[1]q} 1 %[2]d
aggr_new_status{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-prod-02",aggr="aggr9",test=%[1]q} 1 %[2]d
node_new_status{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-prod-01",test=%[1]q} 0 %[2]d
node_labels{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-prod-01",model="AFF-A400",version="9.14.1",vendor="NetApp",test=%[1]q} 1 %[2]d
node_cpu_busy{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-prod-01",test=%[1]q} 72.5 %[2]d
node_total_ops{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-prod-01",test=%[1]q} 18500 %[2]d
ALERTS{alertname="KubePodObserved",alertstate="firing",severity="info",cluster="c1",namespace="shop",pod="rwx-0",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
ALERTS{alertname="NetAppAggregateFilling",alertstate="firing",severity="critical",cluster="ontap-prod",aggr="aggr1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
`, disc, t1))
	s.Require().True(
		s.WaitForSeries(`volume_labels{volume="trident_pvc_shared",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the storage-graph volume_labels")
	s.Require().True(
		s.WaitForSeries(`volume_labels{volume="trident_pvc_catalog",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the second SVM-sharing volume_labels series")

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
	s.Equal("critical", ctrl.Status, "node_new_status=0 must fold to critical")
	s.Require().NotNil(ctrl.Perf)
	s.Require().NotNil(ctrl.Perf.CPUBusyPct)
	s.InDelta(72.5, *ctrl.Perf.CPUBusyPct, 1e-9)
	pod := byID[ident+"/uid-rwx-0"]
	s.Require().NotEmpty(pod.Alerts)
	s.Equal("KubePodObserved", pod.Alerts[0].Name)
	s.Equal("normal", pod.Status, "info alerts do not tint")
	s.Equal("critical", byID["netapp/ontap-prod/aggr/aggr1"].Status,
		"the aggregate's firing critical alert must fold to critical")
	for _, id := range []string{
		ident + "/uid-rwx-1",
		ident + "/worker-1",
		ident + "/shop/shared-data",
	} {
		s.Equal("normal", byID[id].Status, "%s has no negative signal", id)
	}
	svm := byID["netapp/ontap-prod/svm/svm_shop"]
	s.Empty(svm.Status, "SVMs do not carry status")
	rawSVM, err := json.Marshal(svm)
	s.Require().NoError(err)
	s.NotContains(string(rawSVM), `"status"`, "SVM status key must be absent")
	s.assertNoStrayStorageFlowLabels(aggrBody)

	// expose-claim-aggregate: svm_shop spans aggr1 (shared-data) and aggr2
	// (catalog-data). `?aggr=aggr1` retains only shared-data; `?svm=svm_shop`
	// retains both claims, each naming its own aggregate.
	shared := byID[graph.PVCID(ident, "shop", "shared-data")]
	s.Equal("netapp/ontap-prod/aggr/aggr1", shared.Labels["aggr"])
	s.NotContains(byID, graph.PVCID(ident, "shop", "catalog-data"),
		"?aggr=aggr1 must not retain svm_shop's aggr2 claim")

	svmBody := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("svm", "svm_shop") })
	s.assertStorageConservation(svmBody)
	s.assertNoStrayStorageFlowLabels(svmBody)
	svmByID := nodesByID(svmBody)
	s.Contains(svmByID, "netapp/ontap-prod/aggr/aggr1")
	s.Contains(svmByID, "netapp/ontap-prod/aggr/aggr2")
	sharedInSVM := svmByID[graph.PVCID(ident, "shop", "shared-data")]
	catalogInSVM := svmByID[graph.PVCID(ident, "shop", "catalog-data")]
	s.Equal("netapp/ontap-prod/aggr/aggr1", sharedInSVM.Labels["aggr"])
	s.Equal("netapp/ontap-prod/aggr/aggr2", catalogInSVM.Labels["aggr"])

	podBody := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("pod", "shop/rwx-0") })
	s.assertStorageConservation(podBody)
	s.assertHasSplit(podBody)
	s.Contains(nodesByID(podBody), ident+"/uid-rwx-0")
	s.NotContains(nodesByID(podBody), ident+"/uid-rwx-1", "the other RWX mounter is not this pod root")

	// harden-topology-read-cardinality: the storage build never reads the
	// container family, and reads pods by reference.
	for _, n := range podBody.Elements.Nodes {
		s.Empty(n.Data.Containers, "a storage-graph node never carries data.containers (%s)", n.Data.ID)
	}
	// A pod root that mounts no claim is drawable only because its name joins
	// the pod scope.
	idle := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("pod", "shop/idle-0") })
	s.Contains(nodesByID(idle), ident+"/uid-idle-0", "a claimless pod root is still read and drawn")
	s.Empty(idle.Elements.Edges)
	s.NotContains(nodesByID(podBody), ident+"/uid-idle-0", "a claimless pod that is not a root is never drawn")
	// Pod-only roots derive the namespace selector; the body must equal the same
	// request with that namespace given explicitly.
	explicit := s.fetchStorageGraph(srv.URL, func(q url.Values) {
		q.Set("pod", "shop/rwx-0")
		q.Set("namespace", "shop")
	})
	s.Equal(podBody, explicit, "the derived namespace changes the queries, never the body")

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

// TestStorageGraph_RootedVolumeLabelsMatchTheWholeFilerRead proves, against a
// real VictoriaMetrics, that restricting the volume-label topology read to the
// request's rooted components — and recovering each matched claim's candidate
// set in a second phase — keeps the clone pick (scope-volume-labels-by-storage-root).
//
// The fixture is the hazard the second phase exists for: the claim's token
// (pvc_rv) ends BOTH trident_pvc_rv on rv-aggr9 and a clone
// snap_trident_pvc_rv on the lexically-smaller rv-aggr0. Every name is unique to
// this test because the suite shares one VictoriaMetrics.
func (s *GraphSuite) TestStorageGraph_RootedVolumeLabelsMatchTheWholeFilerRead() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	s.IngestExpFmt(fmt.Sprintf(`
kube_pod_info{cluster="c1",namespace="rvshop",pod="rv-0",uid="uid-rv-0",node="worker-rv",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_node_info{cluster="c1",node="worker-rv",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="rvshop",persistentvolumeclaim="rv-data",storageclass="netapp-nas",volumename="pvc-rv",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="rvshop",pod="rv-0",persistentvolumeclaim="rv-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-rv",node="rv-ctl-09",aggr="rv-aggr9",svm="rv_svm",volume="trident_pvc_rv",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-rv",node="rv-ctl-00",aggr="rv-aggr0",svm="rv_svm",volume="snap_trident_pvc_rv",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-rv",node="rv-ctl-09",aggr="rv-aggr9",svm="rv_svm",volume="rv_unrelated",test=%[1]q} 1 %[2]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-rv",svm="rv_svm",volume="trident_pvc_rv",test=%[1]q} 300 %[2]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-rv",svm="rv_svm",volume="snap_trident_pvc_rv",test=%[1]q} 50 %[2]d
aggr_new_status{az="zone-a",env="prod",cluster="ontap-rv",node="rv-ctl-09",aggr="rv-aggr9",test=%[1]q} 1 %[2]d
aggr_new_status{az="zone-a",env="prod",cluster="ontap-rv",node="rv-ctl-00",aggr="rv-aggr0",test=%[1]q} 1 %[2]d
node_new_status{az="zone-a",env="prod",cluster="ontap-rv",node="rv-ctl-09",test=%[1]q} 1 %[2]d
node_new_status{az="zone-a",env="prod",cluster="ontap-rv",node="rv-ctl-00",test=%[1]q} 1 %[2]d
`, disc, t1))
	for _, series := range []string{
		`volume_labels{volume="snap_trident_pvc_rv",test=` + strconv.Quote(disc) + `}`,
		`volume_labels{volume="trident_pvc_rv",test=` + strconv.Quote(disc) + `}`,
		`kube_pod_spec_volumes_persistentvolumeclaims_info{pod="rv-0",test=` + strconv.Quote(disc) + `}`,
	} {
		s.Require().True(s.WaitForSeries(series, fixedNow, 30*time.Second), "VM did not observe %s", series)
	}

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	const (
		ident   = "zone-a-prod-c1"
		pod     = ident + "/uid-rv-0"
		aggr0   = "netapp/ontap-rv/aggr/rv-aggr0"
		aggr9   = "netapp/ontap-rv/aggr/rv-aggr9"
		claimID = ident + "/rvshop/rv-data"
	)
	fetch := func(configure func(url.Values)) cytoscape.Body {
		body := s.fetchStorageGraph(srv.URL, configure)
		s.Require().NotEmpty(body.Elements.Nodes, "a vacuous body would prove nothing")
		return body
	}

	// Rooted at the LARGER aggregate: the claim's pick is rv-aggr0 (the clone
	// sorts first), so the claim is NOT under this root — which a phase-1-only
	// read, seeing only rv-aggr9, would get wrong.
	larger := fetch(func(q url.Values) { q.Set("aggr", "rv-aggr9") })
	ids := nodesByID(larger)
	s.Contains(ids, aggr9, "the root itself is always drawn")
	s.Contains(ids, "netapp/ontap-rv/rv-ctl-09", "with its owning controller")
	s.NotContains(ids, pod, "the pick is the lexically-smaller aggregate, so no path reaches this root")

	// Rooted at the SMALLER aggregate: the claim IS retained, its I/O sums both
	// volumes (the QoS scope is computed over the merged candidate set), and it
	// names rv-aggr0.
	smaller := fetch(func(q url.Values) { q.Set("aggr", "rv-aggr0") })
	ids = nodesByID(smaller)
	s.Contains(ids, pod)
	s.Equal(aggr0, ids[claimID].Labels["aggr"])

	// A cluster root alone roots every entity of that filer.
	filer := fetch(func(q url.Values) { q.Set("ontap_cluster", "ontap-rv") })
	ids = nodesByID(filer)
	s.Contains(ids, aggr0)
	s.Contains(ids, aggr9)
	s.Contains(ids, pod)

	// A typo roots nothing: never the whole filer.
	typo := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("aggr", "rv-typo") })
	s.Empty(typo.Elements.Nodes)
	s.Empty(typo.Elements.Edges)

	// ontap_node= is the controller. rv-ctl-00 owns rv-aggr0, which is the
	// claim's pick, so the pod is drawn. rv-ctl-09 owns only rv-aggr9.
	owned := fetch(func(q url.Values) { q.Set("ontap_node", "rv-ctl-00") })
	s.Contains(nodesByID(owned), pod, "the controller that owns the picked aggregate draws the pod")
	other := fetch(func(q url.Values) { q.Set("ontap_node", "rv-ctl-09") })
	s.Contains(nodesByID(other), "netapp/ontap-rv/rv-ctl-09")
	s.NotContains(nodesByID(other), pod, "rv-aggr9 is not the claim's pick")

	// node= is a Kubernetes node. The pod scheduled there is drawn; an ONTAP
	// controller name sent as node= draws nothing.
	k8s := fetch(func(q url.Values) { q.Set("node", "worker-rv") })
	s.Contains(nodesByID(k8s), pod)
	asNode := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("node", "rv-ctl-00") })
	s.Empty(asNode.Elements.Nodes, "an ONTAP controller name is not a Kubernetes node")

	// The same claim's aggregate on /v1/graph agrees with the storage pick.
	graphResp := s.httpGet(s.graphURL(srv.URL, func(q url.Values) { q.Set("prune", "false") }))
	defer func() { _ = graphResp.Body.Close() }()
	s.Require().Equal(http.StatusOK, graphResp.StatusCode)
	var graphBody cytoscape.Body
	s.Require().NoError(json.NewDecoder(graphResp.Body).Decode(&graphBody))
	s.Equal(aggr0, nodesByID(graphBody)[claimID].Labels["aggr"],
		"the storage pick and /v1/graph name the same aggregate")

	s.assertStorageRejected(srv.URL, nil, "missing_root")
	s.assertStorageRejected(srv.URL, func(q url.Values) {
		q.Set("aggr", "rv-aggr0")
		q.Set("pod", "rvshop/rv-0")
	}, "invalid_scope")
}

func (s *GraphSuite) assertStorageRejected(base string, configure func(url.Values), reason string) {
	s.T().Helper()
	resp := s.httpGet(s.storageGraphURL(base, configure))
	defer func() { _ = resp.Body.Close() }()
	s.Require().Equal(http.StatusBadRequest, resp.StatusCode)
	var body struct {
		Error struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	s.Equal(reason, body.Error.Reason)
}

// TestStorageGraph_ByReferenceControllersAndNodes proves the
// scope-controller-legs-by-reference waves against a real VictoriaMetrics: a
// `node=` root naming a Kubernetes node no pod is scheduled on is still
// drawn, and a Job-owned pod with no annotation of its own resolves its
// ArgoCD Application through kube_job_owner -> kube_cronjob_annotations.
func (s *GraphSuite) TestStorageGraph_ByReferenceControllersAndNodes() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	s.IngestExpFmt(fmt.Sprintf(`
kube_pod_info{cluster="c1",namespace="batch",pod="nightly-28901-x",uid="uid-nightly",node="worker-1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_owner{cluster="c1",namespace="batch",pod="nightly-28901-x",owner_kind="Job",owner_name="nightly-28901",owner_is_controller="true",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_job_owner{cluster="c1",namespace="batch",job_name="nightly-28901",owner_kind="CronJob",owner_name="nightly",owner_is_controller="true",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_cronjob_annotations{cluster="c1",namespace="batch",cronjob="nightly",annotation_argocd_argoproj_io_tracking_id="reports:batch/CronJob:batch/nightly",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_node_info{cluster="c1",node="worker-1",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_node_info{cluster="c1",node="worker-empty",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
`, disc, t1))
	s.Require().True(
		s.WaitForSeries(`kube_cronjob_annotations{cronjob="nightly",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the CronJob annotation series")
	s.Require().True(
		s.WaitForSeries(`kube_node_info{node="worker-empty",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the pod-less node")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	const ident = "zone-a-prod-c1"

	// A node= root naming a Kubernetes node no pod is scheduled on is still
	// drawn, restricted through the by-reference node wave.
	nodeBody := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("node", "worker-empty") })
	byID := nodesByID(nodeBody)
	s.Contains(byID, ident+"/worker-empty", "the node root is drawn with no flow through it")
	s.Empty(nodeBody.Elements.Edges)

	// A Job-owned pod root resolves its Application through the two-stage
	// controller wave: kube_pod_owner names the Job, kube_job_owner (required,
	// scoped to that Job's name) resolves its CronJob, and
	// kube_cronjob_annotations (stage B, scoped to that CronJob's name)
	// supplies the tracking-id.
	podBody := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("pod", "batch/nightly-28901-x") })
	podByID := nodesByID(podBody)
	s.Require().Contains(podByID, ident+"/uid-nightly")
	pod := podByID[ident+"/uid-nightly"]
	s.Equal("reports", pod.Application,
		"resolved through the CronJob, since the Job itself carries no annotation")
}

// TestStorageGraphApplicationRoot recovers a Deployment-managed claimless pod
// and a mounting pod from one tracking-id, draws both, leaves the claimless
// pod edgeless, and returns an empty body for an Application no controller names.
func (s *GraphSuite) TestStorageGraphApplicationRoot() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	const app = "ksg-app-root"
	s.IngestExpFmt(fmt.Sprintf(`
kube_deployment_annotations{cluster="c1",namespace="shop",deployment="approot-web",annotation_argocd_argoproj_io_tracking_id="%[3]s:apps/Deployment:shop/approot-web",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_replicaset_owner{cluster="c1",namespace="shop",replicaset="approot-web-7d9f",owner_kind="Deployment",owner_name="approot-web",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_owner{cluster="c1",namespace="shop",pod="approot-web-abc",owner_kind="ReplicaSet",owner_name="approot-web-7d9f",owner_is_controller="true",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="shop",pod="approot-web-abc",uid="uid-approot-web",node="worker-approot",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_statefulset_annotations{cluster="c1",namespace="shop",statefulset="approot-orders",annotation_argocd_argoproj_io_tracking_id="%[3]s:apps/StatefulSet:shop/approot-orders",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_owner{cluster="c1",namespace="shop",pod="approot-orders-0",owner_kind="StatefulSet",owner_name="approot-orders",owner_is_controller="true",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="shop",pod="approot-orders-0",uid="uid-approot-orders",node="worker-approot",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="shop",pod="approot-orders-0",persistentvolumeclaim="approot-data",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="shop",persistentvolumeclaim="approot-data",volumename="pvc-approot",storageclass="netapp-nas",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-prod-approot",aggr="aggr-approot",svm="svm_approot",volume="trident_pvc_approot",test=%[1]q} 1 %[2]d
kube_node_info{cluster="c1",node="worker-approot",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
`, disc, t1, app))
	s.Require().True(
		s.WaitForSeries(`kube_deployment_annotations{deployment="approot-web",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the application-root deployment annotation")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	const ident = "zone-a-prod-c1"
	body := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("application", app) })
	byID := nodesByID(body)
	claimless := ident + "/uid-approot-web"
	mounting := ident + "/uid-approot-orders"
	s.Require().Contains(byID, claimless)
	s.Require().Contains(byID, mounting)
	s.Equal(app, byID[claimless].Application)
	s.Equal(app, byID[mounting].Application)
	for _, e := range body.Elements.Edges {
		s.NotEqual(claimless, e.Data.Source, "the claimless pod has no edge")
		s.NotEqual(claimless, e.Data.Target, "the claimless pod has no edge")
	}
	mounted := false
	for _, e := range body.Elements.Edges {
		if e.Data.Source == mounting || e.Data.Target == mounting {
			mounted = true
		}
	}
	s.True(mounted, "the mounting pod sits on its storage path")

	typo := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("application", "ksg-app-root-typo") })
	s.Empty(typo.Elements.Nodes)
	s.Empty(typo.Elements.Edges)
}

// TestStorageGraphClaimRoots proves the pvc= / pv= roots against a real
// VictoriaMetrics: a mounted claim, an unmounted claim that keeps its
// storage-side path as a Sankey sink, a claim bound to a statically provisioned
// PV that only a claim root reaches, and a claim on no filer that is drawn alone.
func (s *GraphSuite) TestStorageGraphClaimRoots() {
	disc := s.T().Name()
	t1 := fixedNow.Unix() * 1000
	s.IngestExpFmt(fmt.Sprintf(`
kube_pod_info{cluster="c1",namespace="shop",pod="cr-web-0",uid="uid-cr-web",node="worker-cr",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="shop",pod="cr-static-0",uid="uid-cr-static",node="worker-cr",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_info{cluster="c1",namespace="shop",pod="cr-plain-0",uid="uid-cr-plain",node="worker-cr",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_node_info{cluster="c1",node="worker-cr",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="shop",persistentvolumeclaim="cr-mounted",storageclass="netapp-nas",volumename="pvc-cr-mounted",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="shop",persistentvolumeclaim="cr-orphan",storageclass="netapp-nas",volumename="pvc-cr-orphan",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="shop",persistentvolumeclaim="cr-static",storageclass="netapp-nas",volumename="cr-static-pv",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_persistentvolumeclaim_info{cluster="c1",namespace="shop",persistentvolumeclaim="cr-plain",storageclass="standard",volumename="pvc-cr-plain",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="shop",pod="cr-web-0",persistentvolumeclaim="cr-mounted",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="shop",pod="cr-static-0",persistentvolumeclaim="cr-static",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster="c1",namespace="shop",pod="cr-plain-0",persistentvolumeclaim="cr-plain",volume="data",az="zone-a",env="prod",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-cr-01",aggr="aggr-cr",svm="svm_cr",volume="trident_pvc_cr_mounted",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-cr-01",aggr="aggr-cr",svm="svm_cr",volume="trident_pvc_cr_orphan",test=%[1]q} 1 %[2]d
volume_labels{az="zone-a",env="prod",cluster="ontap-prod",node="ontap-cr-01",aggr="aggr-cr",svm="svm_cr",volume="cr_static_pv",test=%[1]q} 1 %[2]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-prod",svm="svm_cr",volume="trident_pvc_cr_mounted",test=%[1]q} 200 %[2]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-prod",svm="svm_cr",volume="trident_pvc_cr_orphan",test=%[1]q} 50 %[2]d
qos_read_ops{az="zone-a",env="prod",cluster="ontap-prod",svm="svm_cr",volume="cr_static_pv",test=%[1]q} 30 %[2]d
`, disc, t1))
	s.Require().True(
		s.WaitForSeries(`volume_labels{volume="cr_static_pv",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the claim-root volume_labels")
	s.Require().True(
		s.WaitForSeries(`kube_persistentvolumeclaim_info{persistentvolumeclaim="cr-plain",test=`+strconv.Quote(disc)+`}`, fixedNow, 30*time.Second),
		"VM did not observe the claim-root claims")

	srv := s.StartAPIServer(func(cfg *config.Config) {})
	const (
		ident = "zone-a-prod-c1"
		ctrl  = "netapp/ontap-prod/ontap-cr-01"
		aggr  = "netapp/ontap-prod/aggr/aggr-cr"
		svm   = "netapp/ontap-prod/svm/svm_cr"
	)
	claim := func(name string) string { return graph.PVCID(ident, "shop", name) }
	readOps := func(body cytoscape.Body, source, target string) float64 {
		s.T().Helper()
		for _, e := range body.Elements.Edges {
			if e.Data.Source == source && e.Data.Target == target {
				s.Require().NotNil(e.Data.Metrics, "%s -> %s carries a measurement", source, target)
				s.Require().NotNil(e.Data.Metrics.ReadOps)
				return *e.Data.Metrics.ReadOps
			}
		}
		s.Failf("edge not found", "%s -> %s", source, target)
		return 0
	}
	tiers := func(body cytoscape.Body) map[string]int {
		out := map[string]int{}
		for _, e := range body.Elements.Edges {
			out[e.Data.Labels["tier"]]++
		}
		return out
	}

	// A mounted claim: the whole chain to its pod and node, and no other claim
	// sharing the aggregate or SVM.
	mounted := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("pvc", "shop/cr-mounted") })
	byID := nodesByID(mounted)
	for _, id := range []string{ctrl, aggr, svm, claim("cr-mounted"), ident + "/uid-cr-web", ident + "/worker-cr"} {
		s.Contains(byID, id, "%s is on the root claim's path", id)
	}
	for _, name := range []string{"cr-orphan", "cr-static", "cr-plain"} {
		s.NotContains(byID, claim(name), "%s shares the filer but is not the root claim", name)
	}
	s.NotContains(byID, ident+"/uid-cr-static")
	s.assertStorageConservation(mounted)
	s.assertNoStrayStorageFlowLabels(mounted)
	s.InDelta(200.0, readOps(mounted, aggr, svm), 1e-9)

	// A volume root resolves to the same claim and returns the same body.
	byVolume := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("pv", "pvc-cr-mounted") })
	s.Equal(mounted, byVolume, "?pv= is ?pvc= for the claim bound to that volume")

	// An unmounted claim keeps its storage-side path and ends at the claim: its
	// whole measurement rides every hop above it, and no edge leaves it.
	sink := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("pvc", "shop/cr-orphan") })
	sinkByID := nodesByID(sink)
	for _, id := range []string{ctrl, aggr, svm, claim("cr-orphan")} {
		s.Contains(sinkByID, id)
	}
	s.NotContains(sinkByID, claim("cr-mounted"))
	s.Equal(map[string]int{"node-aggr": 1, "aggr-svm": 1, "svm-pvc": 1}, tiers(sink), "no pvc-pod or pod-node edge")
	s.InDelta(50.0, readOps(sink, ctrl, aggr), 1e-9)
	s.InDelta(50.0, readOps(sink, aggr, svm), 1e-9)
	s.InDelta(50.0, readOps(sink, svm, claim("cr-orphan")), 1e-9)
	s.assertNoStrayStorageFlowLabels(sink)

	// Both together: the sink and the mounted claim sum on the shared hops.
	both := s.fetchStorageGraph(srv.URL, func(q url.Values) {
		q.Add("pvc", "shop/cr-mounted")
		q.Add("pvc", "shop/cr-orphan")
	})
	s.InDelta(250.0, readOps(both, ctrl, aggr), 1e-9)
	s.InDelta(250.0, readOps(both, aggr, svm), 1e-9)
	s.InDelta(200.0, readOps(both, svm, claim("cr-mounted")), 1e-9)
	s.InDelta(50.0, readOps(both, svm, claim("cr-orphan")), 1e-9)

	// The unmounted claim is not a node under any other root kind.
	byAggr := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("aggr", "aggr-cr") })
	s.NotContains(nodesByID(byAggr), claim("cr-orphan"), "an unmounted claim that is not a root stays dropped")
	s.Contains(nodesByID(byAggr), claim("cr-mounted"))

	// A claim bound to a statically provisioned PV embeds no `pvc_`, so no
	// storage root finds it; a volume root does, and draws the chain the
	// forward join resolves.
	s.NotContains(nodesByID(byAggr), claim("cr-static"), "no candidate names cr-static-pv")
	static := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("pv", "cr-static-pv") })
	staticByID := nodesByID(static)
	for _, id := range []string{ctrl, aggr, svm, claim("cr-static"), ident + "/uid-cr-static"} {
		s.Contains(staticByID, id, "%s is on the static claim's path", id)
	}
	s.InDelta(30.0, readOps(static, svm, claim("cr-static")), 1e-9)

	// A claim on no filer is drawn alone; its mounting pod is not.
	plain := s.fetchStorageGraph(srv.URL, func(q url.Values) { q.Set("pvc", "shop/cr-plain") })
	plainByID := nodesByID(plain)
	s.Contains(plainByID, claim("cr-plain"))
	s.NotContains(plainByID, ident+"/uid-cr-plain")
	s.Empty(plain.Elements.Edges)

	// A root the upstream does not name is an empty 200.
	for name, configure := range map[string]func(url.Values){
		"pvc": func(q url.Values) { q.Set("pvc", "shop/cr-typo") },
		"pv":  func(q url.Values) { q.Set("pv", "pvc-cr-typo") },
	} {
		typo := s.fetchStorageGraph(srv.URL, configure)
		s.Empty(typo.Elements.Nodes, name)
		s.Empty(typo.Elements.Edges, name)
	}

	// The request contract.
	s.assertStorageRejected(srv.URL, func(q url.Values) { q.Set("pvc", "cr-mounted") }, "invalid_scope")
	s.assertStorageRejected(srv.URL, func(q url.Values) { q.Set("pvc", "shop/cr-mounted"); q.Set("pv", "pvc-cr-mounted") }, "invalid_scope")
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

// assertNoStrayStorageFlowLabels pins the storage-graph-api spec's "No
// storage-flow edge names a claim's aggregate": every storage-flow edge's
// labels hold only `tier` and, on a split pvc-pod edge, `attribution` — never
// the internal `claim_aggr` key the assembler stamps and ProjectStorage
// strips.
func (s *GraphSuite) assertNoStrayStorageFlowLabels(body cytoscape.Body) {
	s.T().Helper()
	for _, e := range body.Elements.Edges {
		if e.Data.Type != "storage-flow" {
			continue
		}
		for k := range e.Data.Labels {
			s.Contains([]string{"tier", "attribution"}, k,
				"storage-flow edge %s -> %s carries an unexpected label %q", e.Data.Source, e.Data.Target, k)
		}
	}
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
