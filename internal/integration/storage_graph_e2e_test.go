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
volume_labels{cluster="ontap-prod",node="ontap-prod-01",aggr="aggr1",svm="svm_shop",volume="trident_pvc_shared",test=%[1]q} 1 %[2]d
volume_labels{cluster="ontap-prod",node="ontap-prod-02",aggr="aggr2",svm="svm_shop",volume="trident_pvc_catalog",test=%[1]q} 1 %[2]d
volume_labels{cluster="ontap-prod",node="ontap-prod-02",aggr="aggr2",svm="svm_other",volume="trident_pvc_other",test=%[1]q} 1 %[2]d
qos_read_ops{cluster="ontap-prod",svm="svm_shop",volume="trident_pvc_shared",test=%[1]q} 300 %[2]d
volume_labels{cluster="ontap-prod",node="ontap-prod-02",aggr="aggr9",svm="svm_idle",volume="vol_unclaimed",test=%[1]q} 1 %[2]d
aggr_new_status{cluster="ontap-prod",node="ontap-prod-02",aggr="aggr9",test=%[1]q} 1 %[2]d
node_new_status{cluster="ontap-prod",node="ontap-prod-01",test=%[1]q} 0 %[2]d
node_labels{cluster="ontap-prod",node="ontap-prod-01",model="AFF-A400",version="9.14.1",vendor="NetApp",test=%[1]q} 1 %[2]d
node_cpu_busy{cluster="ontap-prod",node="ontap-prod-01",test=%[1]q} 72.5 %[2]d
node_total_ops{cluster="ontap-prod",node="ontap-prod-01",test=%[1]q} 18500 %[2]d
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
