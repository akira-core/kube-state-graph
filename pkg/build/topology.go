package build

import (
	"cmp"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	"github.com/prometheus/common/model"
)

// nodeAddrs holds the best (lexically-smallest) address seen per type for one
// (cluster, node). ExternalIP wins over InternalIP regardless of sample order.
type nodeAddrs struct {
	external string
	internal string
}

func (a nodeAddrs) pick() string {
	if a.external != "" {
		return a.external
	}
	return a.internal
}

// volumeKey resolves the derivation this parse uses, adopting the defaults for
// a zero topologyVectors — which is what every hand-built test fixture and
// every embedder that configures nothing passes.
//
// POINTER receiver, deliberately. topologyVectors is the struct every leg of
// the fan-out writes its own slot of, so a value receiver would make
// `v.volumeKey()` on the shared struct copy every one of those slots while
// siblings are still writing them — a data race that only shows up when the
// timing happens to overlap. A pointer receiver reads the one field instead,
// so the method is safe to call from inside the fan-out as well as after it.
func (v *topologyVectors) volumeKey() *VolumeKeyRewriter {
	if v.VolumeKey != nil {
		return v.VolumeKey
	}
	return defaultVolumeKeyRewriter()
}

func parseTopology(v topologyVectors, keys promql.LabelKeys) Topology {
	clusters := map[string]struct{}{}

	// Resolver for the cluster identity every structure below is keyed on, and
	// the per-metric tallies of samples missing the `cluster` label or naming
	// no single identity; both surfaced as one aggregated warn per metric at
	// the end of the parse.
	mc := newClusterResolver(keys)

	// FIRST PASS: build the identity table from the four families that mint
	// cluster-labelled entities. It must complete before ANY bucket call —
	// including the resolve* helpers below — because step 2 of the ladder
	// (adopt) reads it. The other families resolve THROUGH the table and never
	// add to it, so a join input cannot invent a cluster that holds no entity.
	for _, vec := range []model.Vector{v.Pod, v.Node, v.Service, v.PVC} {
		for _, s := range vec {
			mc.observe(s.Metric)
		}
	}
	// A claim-seeded build materialises its unbound root claims, so their
	// claim-info rows mint cluster-labelled entities too.
	if v.MaterialiseUnboundClaims {
		for _, s := range v.PVCInfo {
			mc.observe(s.Metric)
		}
	}

	// Pod controller-owner resolution (D34), with the ReplicaSet skipped to its
	// owning Deployment. Built up-front so the per-pod assembly below can set
	// each pod's typed Owner attribute (never a label).
	podOwners := resolvePodOwners(v.PodOwner, v.ReplicaSetOwner, mc)

	// PVC info resolution (StorageClass name + bound PV name). Built up-front
	// so the per-PVC assembly below can set each PVC's StorageClass (typed
	// data.storageclass, never a label or node) and its `volumename` label
	// (the bound PV name, rooting the Harvest volume join below).
	pvcInfo := resolvePVCInfo(v.PVCInfo, mc)

	// Kubelet PVC usage (used/capacity bytes). Built up-front so assembly
	// can set PVCNode.UsageValue. OPTIONAL — absent series leave usage nil.
	pvcUsage := resolvePVCUsage(v.KubeletVolumeUsed, v.KubeletVolumeCapacity, mc)

	// Pod container list + ArgoCD Application resolution. Both feed typed pod
	// attributes (never labels) set during the per-pod assembly below. The
	// Application is joined from the pod's controller — ArgoCD annotates the
	// managed workload object, never the pods it spawns — reusing the controller
	// owner resolved above, so the Deployment case needs no extra owner hop.
	podContainers := resolvePodContainers(v.PodContainerInfo, mc)
	podApplications := resolvePodApplications(
		podOwners,
		resolveControllerApplications(v, mc),
		resolveJobCronJobOwners(v.JobOwner, mc),
		v.JobAnnotationsDegraded,
	)

	// Service / PVC ArgoCD Application resolution (annotation tracking-id). Built
	// up-front: the PVC index enriches each PVC at the per-PVC assembly below; the
	// service index is threaded into the service-graph reader (service nodes are
	// materialised there, on demand). Both reuse the pod's segment-before-":" parse.
	pvcApplications := resolvePVCApplications(v.PVCAnnotations, mc)
	serviceApplications := resolveServiceApplications(v.ServiceAnnotations, mc)

	// K8s node Ready-status resolution. Built up-front so the per-node assembly
	// below can set each node's typed ReadyStatus attribute (never a label).
	// Keyed (cluster, node) — the same key the node IP / label joins use.
	nodeReady := resolveNodeReadyStatus(v.NodeStatus, mc)

	// Node IP map: (cluster, node-name) -> {ExternalIP, InternalIP}.
	// ExternalIP is preferred at assembly; InternalIP is the fallback for
	// nodes without one (private / NATed node pools). Other address types
	// are ignored even if a wider selector ever leaks them — hostnames must
	// never reach `ipaddress`.
	nodeIPs := map[[2]string]nodeAddrs{}
	for _, s := range v.Addr {
		cluster := mc.bucket(promql.QNodeAddresses, s.Metric)
		nodeName := string(s.Metric["node"])
		typ := string(s.Metric["type"])
		addr := string(s.Metric["address"])
		if addr != "" && (typ == "ExternalIP" || typ == "InternalIP") {
			key := [2]string{cluster, nodeName}
			cur := nodeIPs[key]
			// Deterministic pick: lexically-smallest address wins on duplicate
			// (cluster, node) samples WITHIN each address type, so the emitted
			// IP is a pure function of the data, not upstream vector order
			// (D6 determinism). The external-over-internal preference is
			// applied at node assembly.
			switch typ {
			case "ExternalIP":
				if cur.external == "" || addr < cur.external {
					cur.external = addr
				}
			case "InternalIP":
				if cur.internal == "" || addr < cur.internal {
					cur.internal = addr
				}
			}
			nodeIPs[key] = cur
		}
		clusters[cluster] = struct{}{}
	}

	// K8s node label map: (cluster, node-name) -> labels (with `label_` prefix removed).
	nodeLabels := map[[2]string]map[string]string{}
	for _, s := range v.NodeLabels {
		cluster := mc.bucket(promql.QNodeLabels, s.Metric)
		nodeName := string(s.Metric["node"])
		key := [2]string{cluster, nodeName}
		if _, ok := nodeLabels[key]; !ok {
			nodeLabels[key] = map[string]string{}
		}
		for ln, lv := range s.Metric {
			name := string(ln)
			if !strings.HasPrefix(name, "label_") {
				continue
			}
			lk, val := unflattenLabel(name), string(lv)
			// Deterministic merge: when two series disagree on a key, the
			// lexically-smaller value wins so the emitted label set is a pure
			// function of the data, not upstream vector order (D6 determinism).
			if cur, ok := nodeLabels[key][lk]; !ok || val < cur {
				nodeLabels[key][lk] = val
			}
		}
		clusters[cluster] = struct{}{}
	}

	// K8s nodes. Deduped by (cluster, node): kube_node_info can return multiple
	// series for one node — two KSM scrape targets (HA, or a rollout still inside
	// the last_over_time window) carry different instance/pod target labels, and a
	// kubelet / OS upgrade churns kubelet_version / os_image within the window.
	// Every node attribute below is sourced from (cluster, node)-keyed join maps
	// (nodeLabels / nodeIPs / nodeReady), so duplicate series describe the
	// identical node; collapsing them here (first occurrence wins — the node is a
	// pure function of the order-free join maps, so the winner is deterministic
	// regardless of vector order, D6) keeps same-ID K8sNodes from flooding
	// NewGraph with "duplicate node ID" warnings.
	nodes := make([]*graph.K8sNode, 0, len(v.Node))
	seenNodes := make(map[[2]string]struct{}, len(v.Node))
	for _, s := range v.Node {
		cluster := mc.bucket(promql.QNodeInfo, s.Metric)
		nodeName := string(s.Metric["node"])
		if nodeName == "" {
			continue
		}
		key := [2]string{cluster, nodeName}
		if _, dup := seenNodes[key]; dup {
			continue
		}
		seenNodes[key] = struct{}{}
		labels := map[string]string{}
		for k, v := range nodeLabels[key] {
			labels[k] = v
		}
		// Contract keys win: set AFTER the KSM-derived merge. An operator node
		// label `cluster=...` flattens to label_cluster, and
		// unflattenLabel("label_cluster") == "cluster" — copying it over the
		// contract value would clobber the cluster-scoping every consumer
		// relies on.
		labels["cluster"] = cluster
		var ips []string
		if ip := nodeIPs[key].pick(); ip != "" {
			ips = []string{ip}
		}
		nodes = append(nodes, &graph.K8sNode{
			IDValue:          graph.K8sNodeID(cluster, nodeName),
			NameValue:        nodeName,
			LabelsValue:      labels,
			IPAddressValue:   ips,
			ReadyStatusValue: nodeReady[key],
		})
		clusters[cluster] = struct{}{}
	}

	// Pods (group by (cluster, namespace, pod) for restart handling).
	podGroups := map[podKey][]podObs{}
	for _, s := range v.Pod {
		cluster := mc.bucket(promql.QPodInfo, s.Metric)
		ns := string(s.Metric["namespace"])
		name := string(s.Metric["pod"])
		uid := string(s.Metric["uid"])
		nodeName := string(s.Metric["node"])
		if uid == "" {
			continue
		}
		labels := map[string]string{
			"cluster":   cluster,
			"namespace": ns,
		}
		if nodeName != "" {
			labels["node"] = graph.K8sNodeID(cluster, nodeName)
		}
		podIP := string(s.Metric["pod_ip"])
		k := podKey{cluster, ns, name}
		podGroups[k] = append(podGroups[k], podObs{
			uid:     uid,
			nodeID:  graph.K8sNodeID(cluster, nodeName),
			ts:      s.Timestamp,
			labels:  labels,
			nodeRaw: nodeName,
			podIP:   podIP,
		})
		clusters[cluster] = struct{}{}
	}

	pods := make([]*graph.PodNode, 0, len(v.Pod))
	podsByUID := map[string]*graph.PodNode{}
	podsByNameNS := map[podNameKey]*graph.PodNode{}
	addPodToIndex := func(uid string, pod *graph.PodNode) {
		if uid == "" {
			return
		}
		if existing, dup := podsByUID[uid]; dup {
			slog.Warn("duplicate pod UID across clusters",
				"uid", uid,
				"existing_id", existing.ID(),
				"new_id", pod.ID(),
			)
			// Deterministic dedupe: the lexically-smaller cluster-scoped ID
			// wins so the winner is a pure function of the data, independent of
			// the randomised map-iteration order this runs in (D6 determinism).
			if existing.ID() <= pod.ID() {
				return
			}
		}
		podsByUID[uid] = pod
	}
	for k, group := range podGroups {
		// Newest sample first; pods that churned UIDs within the window collapse
		// to the most recent observation since there is no reliable cross-UID
		// identity link (deleted pods do not back-fill metrics). On equal
		// timestamps (two distinct UIDs scraped at the same step) the
		// lexically-larger UID is the deterministic tie-break, so the canonical
		// pick is a pure function of the data, not vector arrival order (D6).
		slices.SortStableFunc(group, func(a, b podObs) int {
			return cmp.Or(cmp.Compare(b.ts, a.ts), cmp.Compare(b.uid, a.uid))
		})
		// kube-state-metrics emits multiple series per pod-UID as labels evolve
		// during scheduling (e.g. node arrives after the first scrape). Merge
		// labels across same-UID samples — newer values win — so the emitted
		// PodNode reflects the most informative observation. The pod IP lives
		// outside labels and is selected separately below.
		merged := mergeSameUIDLabels(group)
		canonical := group[0]
		// Pod IP is sourced from kube_pod_info.pod_ip. Newest sample wins; if
		// the newest is empty (e.g. arrived before scheduling completed) we
		// fall back to the most recent non-empty observation OF THE CANONICAL
		// UID only — like the label merge above, this is strictly per-UID. A
		// recreated pod (same name, new UID) must not inherit the dead
		// predecessor UID's stale pod_ip.
		var podIP string
		for _, obs := range group {
			if obs.uid == canonical.uid && obs.podIP != "" {
				podIP = obs.podIP
				break
			}
		}
		var ips []string
		if podIP != "" {
			ips = []string{podIP}
		}
		// Resolve the controller owner (ReplicaSet skipped to its Deployment)
		// onto the typed Owner attribute — never into labels. nil when the pod
		// has no controller owner. nk is the pod's (cluster, namespace, name) key
		// shared by the owner / application / container indexes.
		nk := podNameKey(k)
		var owner *graph.Owner
		if o, ok := podOwners[nk]; ok {
			owner = &graph.Owner{Kind: o.kind, Name: o.name}
		}
		canonicalPod := &graph.PodNode{
			IDValue:          graph.PodID(k.cluster, canonical.uid),
			NameValue:        k.pod,
			LabelsValue:      merged[canonical.uid],
			IPAddressValue:   ips,
			OwnerValue:       owner,
			ApplicationValue: podApplications[nk],
			ContainersValue:  podContainers[nk],
		}
		pods = append(pods, canonicalPod)
		addPodToIndex(canonical.uid, canonicalPod)
		podsByNameNS[nk] = canonicalPod
	}

	// PVCs + pod-PVC bindings.
	// Each kube_pod_spec_volumes_persistentvolumeclaims_info series wires one
	// pod to one PVC via (cluster, namespace, pod, persistentvolumeclaim).
	pvcByID := map[string]*graph.PVCNode{}
	pvcs := make([]*graph.PVCNode, 0, len(v.PVC))
	bindingSeen := map[PodPVCBinding]bool{}
	bindings := make([]PodPVCBinding, 0, len(v.PVC))
	canonicalPodUID := map[[3]string]string{}
	for k, group := range podGroups {
		canonicalPodUID[[3]string{k.cluster, k.namespace, k.pod}] = group[0].uid
	}
	// newPVC builds a claim's node from what kube_persistentvolumeclaim_info,
	// the annotation families and the kubelet stats say about it, and registers
	// it. Bindings mint a claim through it; so does a claim-seeded build for a
	// root claim no pod mounts.
	newPVC := func(id, cluster, ns, claim string) *graph.PVCNode {
		attrs := pvcInfo[pvcKey{cluster, ns, claim}]
		labels := map[string]string{"cluster": cluster, "namespace": ns}
		// Bound PV name and NetApp Trident SVM, as additive labels. Each
		// key is set only when its value resolved non-empty — never an
		// empty-string label — and svm is impossible without volumename
		// (the chain is rooted at the PV name). `volumename` (the bound PV)
		// is distinct from the `volume` key below (the pod-spec volume
		// name); both may coexist on one PVC.
		if attrs.volumeName != "" {
			labels["volumename"] = attrs.volumeName
		}
		node := &graph.PVCNode{
			IDValue:           id,
			NameValue:         claim,
			LabelsValue:       labels,
			StorageClassValue: attrs.storageClass,
			ApplicationValue:  pvcApplications[pvcKey{cluster, ns, claim}],
			UsageValue:        pvcUsage[pvcKey{cluster, ns, claim}],
		}
		pvcByID[id] = node
		pvcs = append(pvcs, node)
		clusters[cluster] = struct{}{}
		return node
	}
	for _, s := range v.PVC {
		cluster := mc.bucket(promql.QPVCBindings, s.Metric)
		ns := string(s.Metric["namespace"])
		podName := string(s.Metric["pod"])
		claim := string(s.Metric["persistentvolumeclaim"])
		if claim == "" {
			claim = string(s.Metric["claim_name"])
		}
		if claim == "" {
			continue
		}
		id := graph.PVCID(cluster, ns, claim)
		node, seen := pvcByID[id]
		if !seen {
			node = newPVC(id, cluster, ns, claim)
		}
		// Deterministic pick: the lexically-smallest non-empty volume wins
		// across all samples for this PVC, so the emitted label is a pure
		// function of the data, not upstream vector order (D6 determinism).
		if vol := string(s.Metric["volume"]); vol != "" {
			if cur, ok := node.LabelsValue["volume"]; !ok || vol < cur {
				node.LabelsValue["volume"] = vol
			}
		}
		if podName != "" {
			if uid, ok := canonicalPodUID[[3]string{cluster, ns, podName}]; ok {
				// Dedupe by (PodID, PVCID): one claim mounted via two volume
				// names, a restarted pod, or HA-KSM duplicate series would
				// otherwise emit duplicate pod-mounts-pvc edges sharing one
				// UUIDv5 edge ID.
				b := PodPVCBinding{PodID: graph.PodID(cluster, uid), PVCID: id}
				if !bindingSeen[b] {
					bindingSeen[b] = true
					bindings = append(bindings, b)
				}
			}
		}
		clusters[cluster] = struct{}{}
	}

	// Root claims no pod mounts (claim-seeded builds only). Sorted, so the node
	// order — and everything derived from it — never depends on vector order.
	if v.MaterialiseUnboundClaims {
		infoKeys := slices.SortedFunc(maps.Keys(pvcInfo), func(a, b pvcKey) int {
			return cmp.Or(cmp.Compare(a.cluster, b.cluster), cmp.Compare(a.namespace, b.namespace), cmp.Compare(a.claim, b.claim))
		})
		for _, k := range infoKeys {
			id := graph.PVCID(k.cluster, k.namespace, k.claim)
			if _, ok := pvcByID[id]; !ok {
				newPVC(id, k.cluster, k.namespace, k.claim)
			}
		}
	}

	// PVC ArgoCD Application inheritance (D13): a PVC with no Application of its
	// own inherits the lexically-smallest Application among the pods that mount
	// it, so an unannotated PVC still nests under its workload's application
	// group. The PVC's own annotation (set above from pvcApplications) ALWAYS
	// wins — this pass only fills app-less PVCs — and runs before graph.NewGraph
	// freezes the nodes. Pure function of the (binding set, pod Applications), so
	// order-independent and byte-stable (D6).
	podAppByID := make(map[string]string, len(pods))
	for _, p := range pods {
		if p.ApplicationValue == "" {
			continue
		}
		// Lexically-smallest wins on the (improbable) same-ID pod collision —
		// the same tie-break as addPodToIndex, so the join key is order-free (D6).
		if cur, ok := podAppByID[p.IDValue]; !ok || p.ApplicationValue < cur {
			podAppByID[p.IDValue] = p.ApplicationValue
		}
	}
	inherited := pvcInheritedApps(bindings, podAppByID)
	for _, pvc := range pvcs {
		if pvc.ApplicationValue == "" {
			if app := inherited[pvc.IDValue]; app != "" {
				pvc.ApplicationValue = app
			}
		}
	}

	// Harvest volume join: SVM label, pvc-to-netapp-aggr edges, demand-driven
	// NetApp aggregate + controller nodes. Rooted at each PVC's volumename.
	claims := make([]pvcVolume, 0, len(pvcs))
	for _, pv := range pvcs {
		if vn := pv.LabelsValue["volumename"]; vn != "" {
			attrs := pvcInfo[pvcKey{pv.LabelsValue["cluster"], pv.LabelsValue["namespace"], pv.NameValue}]
			claims = append(claims, pvcVolume{id: pv.IDValue, volumeName: vn, zone: attrs.zone})
		}
	}
	netapp := resolveNetAppStorage(claims, v, mc.keys)
	aggrByPVC := make(map[string]string, len(netapp.edges))
	for _, e := range netapp.edges {
		if e.Type == graph.EdgeTypePVCToNetAppAggr {
			aggrByPVC[e.Source] = e.Target
		}
	}
	for _, pv := range pvcs {
		if ref, ok := netapp.svmByPVC[pv.IDValue]; ok && ref.SVM != "" {
			pv.LabelsValue["svm"] = ref.SVM
		}
		if aggr, ok := aggrByPVC[pv.IDValue]; ok {
			pv.LabelsValue["aggr"] = aggr
		}
		// The declared ceiling rides on the node as a typed attribute, never a
		// label, and independently of the edge: a FlexGroup claim has none, and
		// an unmeasured claim's edge carries no ceiling, yet the node does.
		if q, ok := netapp.qosByPVC[pv.IDValue]; ok {
			pv.QoSValue = q
		}
	}

	// Services (D29). kube_service_info carries cluster_ip; "None" means headless.
	servicesByNameNS := map[serviceKey]ServiceObs{}
	for _, s := range v.Service {
		cluster := mc.bucket(promql.QServiceInfo, s.Metric)
		ns := string(s.Metric["namespace"])
		svc := string(s.Metric["service"])
		if svc == "" {
			continue
		}
		servicesByNameNS[serviceKey{cluster, ns, svc}] = ServiceObs{
			ClusterIP: string(s.Metric["cluster_ip"]),
		}
		clusters[cluster] = struct{}{}
	}

	// EndpointSlice -> owning Service name, via the kubernetes.io/service-name
	// label kube-state-metrics flattens to label_kubernetes_io_service_name
	// (requires the operator to allowlist it; absent -> the slice's endpoints
	// stay unmapped and the service falls back to external/<label> downstream).
	type sliceKey struct{ cluster, namespace, slice string }
	sliceToService := map[sliceKey]string{}
	for _, s := range v.EpLabels {
		cluster := mc.bucket(promql.QEndpointSliceLabels, s.Metric)
		ns := string(s.Metric["namespace"])
		slice := string(s.Metric["endpointslice"])
		svc := string(s.Metric["label_kubernetes_io_service_name"])
		if slice == "" || svc == "" {
			continue
		}
		sliceToService[sliceKey{cluster, ns, slice}] = svc
		clusters[cluster] = struct{}{}
	}

	// EndpointsByService: resolve each endpoint's backing pod via
	// (cluster, targetref_namespace, targetref_name) against the loaded pods,
	// keyed by the owning service recovered from the slice->service map. This is
	// the source of the Service → backing-pod fan-out (service-selects-pod edges).
	endpointsByService := map[serviceKey][]EndpointObs{}
	for _, s := range v.EpEndpoints {
		cluster := mc.bucket(promql.QEndpointSliceEndpoints, s.Metric)
		ns := string(s.Metric["namespace"])
		slice := string(s.Metric["endpointslice"])
		svc, ok := sliceToService[sliceKey{cluster, ns, slice}]
		if !ok {
			continue
		}
		if kind := string(s.Metric["targetref_kind"]); kind != "" && kind != "Pod" {
			continue
		}
		targetNS := string(s.Metric["targetref_namespace"])
		if targetNS == "" {
			targetNS = ns
		}
		targetName := string(s.Metric["targetref_name"])
		if targetName == "" {
			continue
		}
		pod, ok := podsByNameNS[podNameKey{cluster, targetNS, targetName}]
		if !ok {
			continue
		}
		key := serviceKey{cluster, ns, svc}
		endpointsByService[key] = append(endpointsByService[key], EndpointObs{Pod: pod})
		clusters[cluster] = struct{}{}
	}

	clusterList := make([]string, 0, len(clusters))
	for c := range clusters {
		clusterList = append(clusterList, c)
	}
	slices.Sort(clusterList)

	mc.warn()

	return Topology{
		Pods:                pods,
		Nodes:               nodes,
		PVCs:                pvcs,
		NetAppAggrs:         netapp.aggrs,
		NetAppNodes:         netapp.nodes,
		StorageEdges:        netapp.edges,
		NetAppInventory:     netapp.inventory,
		SVMByPVC:            netapp.svmByPVC,
		Alerts:              v.Alerts,
		PodPVCs:             bindings,
		PodsByUID:           podsByUID,
		ServicesByNameNS:    servicesByNameNS,
		EndpointsByService:  endpointsByService,
		ServiceApplications: serviceApplications,
		ClustersObserved:    clusterList,
		ClusterIdentities:   mc.snapshot(),
		clusters:            mc,
		ontapZones:          ontapZonesOf(v, mc.keys),
	}
}

// unflattenLabel inverts kube-state-metrics' `label_*` flattening.
//
// Examples:
//
//	"label_topology_kubernetes_io_zone" -> "topology.kubernetes.io/zone"
//	"label_kubernetes_io_arch"          -> "kubernetes.io/arch"
//	"label_app"                          -> "app"
//
// Heuristic: strip the `label_` prefix, then convert underscores to dots
// except the underscore preceding the LAST segment, which becomes a slash if
// the label key contains a domain prefix.
func unflattenLabel(flattened string) string {
	s := strings.TrimPrefix(flattened, "label_")
	// kube-state-metrics replaces invalid label-name characters with `_`.
	// We can't perfectly invert that, but the dominant case is
	// `<dns-prefix>/<segment>` where the prefix uses dots. We approximate:
	// replace all `_` with `.`, then turn the last `.` into `/` if any prior
	// `.` exists.
	withDots := strings.ReplaceAll(s, "_", ".")
	if i := strings.LastIndex(withDots, "."); i > 0 && strings.Contains(withDots[:i], ".") {
		return withDots[:i] + "/" + withDots[i+1:]
	}
	return withDots
}

// mergeSameUIDLabels returns one label map per UID, formed by merging labels
// from every sample with that UID. group is assumed sorted newest-first; older
// samples fill in keys the newer ones omit. This handles kube-state-metrics
// emitting multiple kube_pod_info series per UID as state evolves (e.g. node
// arrives on a later scrape).
func mergeSameUIDLabels(group []podObs) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, obs := range group {
		merged, ok := out[obs.uid]
		if !ok {
			merged = map[string]string{}
			out[obs.uid] = merged
		}
		for k, v := range obs.labels {
			if v == "" {
				continue
			}
			if _, present := merged[k]; !present {
				merged[k] = v
			}
		}
	}
	return out
}
