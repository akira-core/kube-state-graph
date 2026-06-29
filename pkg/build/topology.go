package build

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	"github.com/marz32one/kube-state-graph/pkg/graph"
	"github.com/marz32one/kube-state-graph/pkg/promql"
)

// PodPVCBinding records that a pod mounts a specific PVC. The reader emits
// these so the edge builder can wire pod-mounts-pvc.
type PodPVCBinding struct {
	PodID string
	PVCID string
}

// podKey groups pod samples by their cluster-scoped namespace/name. Multiple
// UIDs under one key indicate restarts.
type podKey struct{ cluster, namespace, pod string }

// podObs is one parsed kube_pod_info sample.
type podObs struct {
	uid     string
	nodeID  string
	ts      model.Time
	labels  map[string]string
	nodeRaw string
	podIP   string
}

// serviceKey identifies a Service by its cluster-scoped namespace/name (D29).
type serviceKey struct{ cluster, namespace, service string }

// podNameKey identifies a pod by its cluster-scoped namespace/name. Used
// internally to join an endpointslice `targetref_name` to its backing pod when
// building EndpointsByService (D29).
type podNameKey struct{ cluster, namespace, pod string }

// ServiceObs carries the kube_service_info facts needed to materialise a
// ServiceNode on demand. ClusterIP is retained verbatim — the headless
// sentinel "None" distinguishes a headless service from a ClusterIP one.
type ServiceObs struct {
	ClusterIP string
}

// EndpointObs is one resolved backing pod of a Service (from
// kube_endpointslice_endpoints, joined to topology pods by targetref).
type EndpointObs struct {
	Pod *graph.PodNode
}

// Topology is the typed result of reading kube-state-metrics-style series for
// a single time window across all clusters in scope.
type Topology struct {
	Pods           []*graph.PodNode
	Nodes          []*graph.K8sNode
	PVCs           []*graph.PVCNode
	StorageClasses []*graph.StorageClassNode
	PodPVCs        []PodPVCBinding

	// PodsByUID indexes every pod in Pods by its raw Kubernetes UID (without
	// the cluster prefix). K8s pod UIDs are UUIDv4 and unique across clusters
	// in practice, so this is the join key the service-graph reader uses to
	// recover the server-side cluster for `pod-calls-pod` edges (the metric
	// only carries the trace-source / client-side `cluster` label).
	//
	// On duplicate UIDs across clusters (data anomaly), the pod with the
	// lexically-smaller cluster-scoped ID wins. This is a deterministic pure
	// function of the data — NOT first-inserted — so the chosen pod (and hence
	// every `pod-calls-pod` edge target resolved through this index) is stable
	// across rebuilds regardless of map-iteration order (D6 determinism).
	PodsByUID map[string]*graph.PodNode

	// D29 connection-string resolution indexes. Built only when KSM exports
	// services / endpointslices (and, for the slice→service join, allowlists
	// the kubernetes.io/service-name label); empty otherwise. These are
	// INDEXES ONLY — ServiceNodes and service-selects-pod edges are
	// materialised on demand by the service-graph reader for referenced
	// services, not emitted wholesale here.
	//
	//   ServicesByNameNS   — (cluster, namespace, service) → cluster_ip facts
	//   EndpointsByService — (cluster, namespace, service) → backing pods
	ServicesByNameNS   map[serviceKey]ServiceObs
	EndpointsByService map[serviceKey][]EndpointObs

	// ServiceApplications indexes (cluster, namespace, service) → ArgoCD
	// Application name (from kube_service_annotations' tracking-id). ServiceNodes
	// are materialised on demand by the service-graph reader, so it consumes this
	// index to set each node's Application (PVC applications are set directly on
	// the PVCNode at topology assembly instead). Empty when the metric is absent.
	ServiceApplications map[serviceKey]string

	ClustersObserved []string // sorted unique cluster values

	// RawSeriesCount records how many raw upstream series each topology query
	// returned BEFORE parsing/filtering, keyed by query name. Diagnostic only:
	// the build pipeline uses it to enrich the outside-retention error so an
	// operator can tell which upstream metric came back empty (0 raw series)
	// versus returned rows that were all filtered out (raw > 0 but parsed 0,
	// e.g. kube_pod_info samples with an empty uid).
	RawSeriesCount map[string]int
}

// topologyVectors groups the raw result vectors of the topology fan-out. It
// lets parseTopology take one named argument instead of ten positional,
// same-typed model.Vectors that were easy to transpose at the call sites.
type topologyVectors struct {
	Pod         model.Vector
	Node        model.Vector
	Addr        model.Vector
	PVC         model.Vector
	NodeLabels  model.Vector
	Service     model.Vector
	EpEndpoints model.Vector
	EpLabels    model.Vector
	// Pod controller-owner resolution (D34).
	PodOwner        model.Vector
	ReplicaSetOwner model.Vector
	// PVC StorageClass resolution.
	PVCInfo model.Vector
	// StorageClass node resolution (provisioner + NetApp/Ceph parameters).
	StorageClassInfo model.Vector
	// Pod container list resolution (name/image per container).
	PodContainerInfo model.Vector
	// K8s node Ready-status resolution (kube_node_status_condition).
	NodeStatus model.Vector
	// Service / PVC ArgoCD Application resolution (annotation tracking-id).
	ServiceAnnotations model.Vector
	PVCAnnotations     model.Vector
}

// ReadTopology runs the topology queries in parallel and assembles the
// result. The Renderer carries the configurable upstream metric-name prefix
// (see design.md D26) so deployments using a fork of kube-state-metrics or a
// custom exporter that re-publishes KSM-shaped series can be supported.
//
// The service / endpointslice queries (D29) are best-effort: an upstream that
// does not export them (older KSM, or KSM started without
// --resources=services,endpointslices) yields empty indexes, and "://"
// connection-string endpoints simply fall back to `external/<label>`.
func ReadTopology(ctx context.Context, q promql.Querier, r promql.Renderer, window time.Duration, end time.Time) (Topology, error) {
	// Each goroutine writes a distinct field, so concurrent writes to v are
	// race-free (no overlapping memory); g.Wait() establishes the happens-before
	// edge to the read below.
	var v topologyVectors

	g, ctx := errgroup.WithContext(ctx)

	// fetch issues one query and stores its result into dst. It captures the
	// errgroup-derived ctx so a failing leg cancels the rest. The closure
	// recovers its own panics: errgroup (x/sync, post-#53757-revert) does NOT
	// propagate goroutine panics to Wait, so an unrecovered panic here would
	// kill the whole process — the HTTP recovery middleware only covers the
	// handler goroutine. Converting to an error keeps the standard
	// build-failure path (sanitised 500, full detail in server logs).
	fetch := func(name promql.Query, dst *model.Vector) func() error {
		return func() (err error) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.ErrorContext(ctx, "panic in topology query",
						"query", string(name),
						"panic", fmt.Sprint(rec),
						"stack", string(debug.Stack()),
					)
					err = fmt.Errorf("panic in %s query: %v", name, rec)
				}
			}()
			out, err := q.Instant(ctx, string(name), r.Render(name, window), end)
			*dst = out
			return err
		}
	}

	g.Go(fetch(promql.QPodInfo, &v.Pod))
	g.Go(fetch(promql.QNodeInfo, &v.Node))
	g.Go(fetch(promql.QNodeAddresses, &v.Addr))
	g.Go(fetch(promql.QPVCBindings, &v.PVC))
	g.Go(fetch(promql.QNodeLabels, &v.NodeLabels))
	g.Go(fetch(promql.QServiceInfo, &v.Service))
	g.Go(fetch(promql.QEndpointSliceEndpoints, &v.EpEndpoints))
	g.Go(fetch(promql.QEndpointSliceLabels, &v.EpLabels))
	g.Go(fetch(promql.QPodOwner, &v.PodOwner))
	g.Go(fetch(promql.QReplicaSetOwner, &v.ReplicaSetOwner))
	g.Go(fetch(promql.QPVCInfo, &v.PVCInfo))
	g.Go(fetch(promql.QStorageClassInfo, &v.StorageClassInfo))
	g.Go(fetch(promql.QPodContainerInfo, &v.PodContainerInfo))
	g.Go(fetch(promql.QNodeStatusCondition, &v.NodeStatus))
	g.Go(fetch(promql.QServiceAnnotations, &v.ServiceAnnotations))
	g.Go(fetch(promql.QPVCAnnotations, &v.PVCAnnotations))
	if err := g.Wait(); err != nil {
		return Topology{}, fmt.Errorf("topology fan-out: %w", err)
	}

	t := parseTopology(v)
	t.RawSeriesCount = map[string]int{
		string(promql.QPodInfo):                len(v.Pod),
		string(promql.QNodeInfo):               len(v.Node),
		string(promql.QNodeAddresses):          len(v.Addr),
		string(promql.QPVCBindings):            len(v.PVC),
		string(promql.QNodeLabels):             len(v.NodeLabels),
		string(promql.QServiceInfo):            len(v.Service),
		string(promql.QEndpointSliceEndpoints): len(v.EpEndpoints),
		string(promql.QEndpointSliceLabels):    len(v.EpLabels),
		string(promql.QPodOwner):               len(v.PodOwner),
		string(promql.QReplicaSetOwner):        len(v.ReplicaSetOwner),
		string(promql.QPVCInfo):                len(v.PVCInfo),
		string(promql.QStorageClassInfo):       len(v.StorageClassInfo),
		string(promql.QPodContainerInfo):       len(v.PodContainerInfo),
		string(promql.QNodeStatusCondition):    len(v.NodeStatus),
		string(promql.QServiceAnnotations):     len(v.ServiceAnnotations),
		string(promql.QPVCAnnotations):         len(v.PVCAnnotations),
	}
	return t, nil
}

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

func parseTopology(v topologyVectors) Topology {
	clusters := map[string]struct{}{}

	// Per-metric tally of samples missing the `cluster` label; surfaced as one
	// aggregated warn per metric at the end of the parse.
	mc := missingClusterCounts{}

	// Pod controller-owner resolution (D34), with the ReplicaSet skipped to its
	// owning Deployment. Built up-front so the per-pod assembly below can set
	// each pod's typed Owner attribute (never a label).
	podOwners := resolvePodOwners(v.PodOwner, v.ReplicaSetOwner, mc)

	// PVC StorageClass resolution. Built up-front so the per-PVC assembly below
	// can set each PVC's StorageClass (consumed by the Cytoscape serialiser for
	// compound grouping — never a label, never serialised).
	pvcStorageClass := resolvePVCStorageClass(v.PVCInfo, mc)

	// StorageClass node attributes (provisioner + NetApp/Ceph parameters) from
	// kube_storageclass_info, keyed (cluster, storageclass). Built up-front; the
	// StorageClass nodes are materialised after the PVC slice exists (so a
	// PVC-referenced-but-absent class can be back-filled bare).
	scInfo := resolveStorageClassInfo(v.StorageClassInfo, mc)

	// Pod container list + ArgoCD Application resolution. Both feed typed pod
	// attributes (never labels) set during the per-pod assembly below. The
	// Application is read from the SAME kube_pod_owner vector as the controller
	// owner but independently of the controller pick (it is a pod-level fact).
	podContainers := resolvePodContainers(v.PodContainerInfo, mc)
	podApplications := resolvePodApplications(v.PodOwner)

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
		cluster := mc.bucket(promql.QNodeAddresses, string(s.Metric["cluster"]))
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
		cluster := mc.bucket(promql.QNodeLabels, string(s.Metric["cluster"]))
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
		cluster := mc.bucket(promql.QNodeInfo, string(s.Metric["cluster"]))
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
		cluster := mc.bucket(promql.QPodInfo, string(s.Metric["cluster"]))
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
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].ts != group[j].ts {
				return group[i].ts > group[j].ts
			}
			return group[i].uid > group[j].uid
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
	for _, s := range v.PVC {
		cluster := mc.bucket(promql.QPVCBindings, string(s.Metric["cluster"]))
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
			node = &graph.PVCNode{
				IDValue:           id,
				NameValue:         claim,
				LabelsValue:       map[string]string{"cluster": cluster, "namespace": ns},
				StorageClassValue: pvcStorageClass[pvcKey{cluster, ns, claim}],
				ApplicationValue:  pvcApplications[pvcKey{cluster, ns, claim}],
			}
			pvcByID[id] = node
			pvcs = append(pvcs, node)
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

	// StorageClass nodes (real, from kube_storageclass_info). Every observed
	// StorageClass becomes a node; a StorageClass referenced by an emitted PVC
	// but absent from the info metric is materialised bare (nil InfoValue, no
	// provisioner/parameters) so its pvc-to-storageclass edge has a target.
	// Attributed nodes win over bare on the same (cluster, name) via scSeen.
	// Sorted by ID so the slice — and hence NewGraph keep-first dedup — is a pure
	// function of the data (D6 determinism).
	storageClasses := make([]*graph.StorageClassNode, 0, len(scInfo))
	scSeen := make(map[storageClassKey]bool, len(scInfo))
	for key, info := range scInfo {
		storageClasses = append(storageClasses, &graph.StorageClassNode{
			IDValue:     graph.StorageClassID(key.cluster, key.name),
			NameValue:   key.name,
			LabelsValue: map[string]string{"cluster": key.cluster},
			InfoValue:   info,
		})
		scSeen[key] = true
		clusters[key.cluster] = struct{}{}
	}
	for _, pv := range pvcs {
		sc := pv.StorageClassValue
		if sc == "" {
			continue
		}
		key := storageClassKey{pv.LabelsValue["cluster"], sc}
		if scSeen[key] {
			continue
		}
		scSeen[key] = true
		storageClasses = append(storageClasses, &graph.StorageClassNode{
			IDValue:     graph.StorageClassID(key.cluster, key.name),
			NameValue:   key.name,
			LabelsValue: map[string]string{"cluster": key.cluster},
			InfoValue:   nil,
		})
		clusters[key.cluster] = struct{}{}
	}
	sort.SliceStable(storageClasses, func(i, j int) bool {
		return storageClasses[i].ID() < storageClasses[j].ID()
	})

	// Services (D29). kube_service_info carries cluster_ip; "None" means headless.
	servicesByNameNS := map[serviceKey]ServiceObs{}
	for _, s := range v.Service {
		cluster := mc.bucket(promql.QServiceInfo, string(s.Metric["cluster"]))
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
		cluster := mc.bucket(promql.QEndpointSliceLabels, string(s.Metric["cluster"]))
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
		cluster := mc.bucket(promql.QEndpointSliceEndpoints, string(s.Metric["cluster"]))
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
	sort.Strings(clusterList)

	mc.warn()

	return Topology{
		Pods:                pods,
		Nodes:               nodes,
		PVCs:                pvcs,
		StorageClasses:      storageClasses,
		PodPVCs:             bindings,
		PodsByUID:           podsByUID,
		ServicesByNameNS:    servicesByNameNS,
		EndpointsByService:  endpointsByService,
		ServiceApplications: serviceApplications,
		ClustersObserved:    clusterList,
	}
}

// ownerRef is a resolved controller owner (kind + name) for a pod.
type ownerRef struct{ kind, name string }

// resolvePodOwners builds the (cluster, namespace, pod) → controller-owner index
// from kube_pod_owner, skipping the intermediate ReplicaSet (D34): when a pod's
// controller owner is a ReplicaSet, it is resolved one level up via
// kube_replicaset_owner to the owning Deployment. A bare ReplicaSet with no
// Deployment owner keeps the ReplicaSet as the owner; any other owner kind is
// surfaced verbatim. Pods with no controller owner are simply absent from the
// returned map (the caller omits the labels rather than emitting empty strings).
//
// The returned map is a deterministic function of the two input vectors — no
// ordering dependence: when a pod reports multiple controller owners, the
// lexically-smallest (kind, name) wins so the emitted entity is stable across
// rebuilds (D6 determinism). The only side effect is tallying missing-cluster
// samples into the caller's mc accumulator.
func resolvePodOwners(ownerVec, rsOwnerVec model.Vector, mc missingClusterCounts) map[podNameKey]ownerRef {
	// ReplicaSet → owning Deployment, keyed by (cluster, namespace, replicaset).
	// Only Deployment owners are retained; a ReplicaSet owned by anything else
	// (or nothing) is left unresolved so the pod keeps the ReplicaSet.
	rsToDeployment := make(map[podNameKey]string, len(rsOwnerVec))
	for _, s := range rsOwnerVec {
		if string(s.Metric["owner_kind"]) != "Deployment" {
			continue
		}
		cluster := mc.bucket(promql.QReplicaSetOwner, string(s.Metric["cluster"]))
		ns := string(s.Metric["namespace"])
		rs := string(s.Metric["replicaset"])
		dep := string(s.Metric["owner_name"])
		if rs == "" || dep == "" {
			continue
		}
		rsToDeployment[podNameKey{cluster, ns, rs}] = dep
	}

	owners := make(map[podNameKey]ownerRef, len(ownerVec))
	for _, s := range ownerVec {
		if string(s.Metric["owner_is_controller"]) != "true" {
			continue
		}
		cluster := mc.bucket(promql.QPodOwner, string(s.Metric["cluster"]))
		ns := string(s.Metric["namespace"])
		pod := string(s.Metric["pod"])
		kind := string(s.Metric["owner_kind"])
		name := string(s.Metric["owner_name"])
		if pod == "" || kind == "" || name == "" {
			continue
		}
		if kind == "ReplicaSet" {
			if dep, ok := rsToDeployment[podNameKey{cluster, ns, name}]; ok {
				kind, name = "Deployment", dep
			}
		}
		key := podNameKey{cluster, ns, pod}
		// Deterministic pick: lexically-smallest (kind, name) wins on collision.
		if cur, ok := owners[key]; ok {
			if kind > cur.kind || (kind == cur.kind && name >= cur.name) {
				continue
			}
		}
		owners[key] = ownerRef{kind, name}
	}
	return owners
}

// resolvePodContainers builds the (cluster, namespace, pod) → sorted container
// list index from kube_pod_container_info. Each series contributes one
// {name=container, image=image} element, deduped per (pod, container-name).
//
// The query is `tlast_over_time(kube_pod_container_info[w])`, so each series'
// VALUE (`s.Value`) is its last-sample timestamp (unix seconds). When a container
// changed image in the window — each image being a DISTINCT series (image is a
// label) — the image SEEN LATEST wins (the current one). Exact-timestamp ties
// (co-scraped images) break by lexically-smallest image so the body stays
// byte-identical across rebuilds (D6). Empty images are skipped so a transient
// image-less series never masks (or, by a later timestamp, beats) a populated
// sibling. The per-pod list is sorted by (name, image).
//
// CAVEAT (documented in design.md D-A4): for query windows far from the real wall
// clock, VictoriaMetrics returns only ONE image-variant series per container
// (dropping the rest) — true for last_over_time, tlast_over_time, AND
// query_range alike. So "latest" is only meaningful for near-now windows (the
// dominant case); for far-past windows the resolver simply surfaces whatever
// single variant VM returns. The pick is never worse than a lexically-smallest
// fallback would be, and degrades gracefully if the query is ever reverted to
// last_over_time (all values equal → the lexical tie-break decides).
//
// OPTIONAL: an absent or empty vector yields an empty map and pods carry no
// containers (graceful degradation). The returned map is a deterministic
// function of the input vector. The only side effect is tallying
// missing-cluster samples into the caller's mc accumulator.
func resolvePodContainers(vec model.Vector, mc missingClusterCounts) map[podNameKey][]graph.Container {
	type containerKey struct {
		pod  podNameKey
		name string
	}
	type pick struct {
		image    string
		lastSeen model.SampleValue
	}
	// (pod, container-name) → the image last seen latest (greatest tlast_over_time
	// value), lexically-smallest image breaking exact-timestamp ties.
	best := make(map[containerKey]pick, len(vec))
	for _, s := range vec {
		cluster := mc.bucket(promql.QPodContainerInfo, string(s.Metric["cluster"]))
		ns := string(s.Metric["namespace"])
		pod := string(s.Metric["pod"])
		name := string(s.Metric["container"])
		image := string(s.Metric["image"])
		if pod == "" || name == "" || image == "" {
			continue
		}
		key := containerKey{podNameKey{cluster, ns, pod}, name}
		if cur, ok := best[key]; ok {
			if s.Value < cur.lastSeen || (s.Value == cur.lastSeen && image >= cur.image) {
				continue
			}
		}
		best[key] = pick{image: image, lastSeen: s.Value}
	}

	out := map[podNameKey][]graph.Container{}
	for key, p := range best {
		out[key.pod] = append(out[key.pod], graph.Container{Name: key.name, Image: p.image})
	}
	for pod := range out {
		list := out[pod]
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Name != list[j].Name {
				return list[i].Name < list[j].Name
			}
			return list[i].Image < list[j].Image
		})
	}
	return out
}

// resolvePodApplications builds the (cluster, namespace, pod) → ArgoCD
// Application index from the argocd_tracking_id label on kube_pod_owner. The
// Application is the segment of the tracking-id value before the first ":"
// (ArgoCD annotation-based form <app>:<group>/<kind>:<ns>/<name>); a value with
// no ":" is surfaced verbatim. The label is read independently of the
// controller-owner pick — it is a pod-level fact that must survive even when no
// kube_pod_owner row is a controller.
//
// OPTIONAL: pods with no non-empty argocd_tracking_id label are absent from the
// returned map (the caller omits the attribute rather than emitting ""). The
// returned map is a deterministic function of the input vector — on a per-pod
// collision the lexically-smallest non-empty tracking-id wins. It uses the pure
// bucketCluster helper (NOT mc.bucket): resolvePodOwners already tallies this
// vector's missing-cluster samples for its controller rows, so using mc.bucket
// here would double-count those. (A tracking-id carried only on a non-controller
// row with a missing cluster label is therefore bucketed silently — an
// acceptable diagnostic gap, not a wrong-output one.)
func resolvePodApplications(ownerVec model.Vector) map[podNameKey]string {
	// kube_pod_owner's missing-cluster tally is owned by resolvePodOwners (the
	// controller pick reads the SAME vector), so this pass buckets without
	// re-tallying — hence bucketCluster, not mc.bucket.
	return resolveApplications(ownerVec, "argocd_tracking_id", func(m model.Metric) (podNameKey, bool) {
		pod := string(m["pod"])
		if pod == "" {
			return podNameKey{}, false
		}
		return podNameKey{bucketCluster(string(m["cluster"])), string(m["namespace"]), pod}, true
	})
}

// resolveServiceApplications builds the (cluster, namespace, service) → ArgoCD
// Application index from kube_service_annotations'
// annotation_argocd_argoproj_io_tracking_id label (KSM's sanitised form of the
// argocd.argoproj.io/tracking-id annotation). OPTIONAL: an absent/empty vector
// yields an empty map (services carry no Application). Deterministic per
// "absent when empty" (lexically-smallest raw tracking-id wins on collision).
func resolveServiceApplications(vec model.Vector, mc missingClusterCounts) map[serviceKey]string {
	return resolveApplications(vec, "annotation_argocd_argoproj_io_tracking_id", func(m model.Metric) (serviceKey, bool) {
		svc := string(m["service"])
		if svc == "" {
			return serviceKey{}, false
		}
		return serviceKey{mc.bucket(promql.QServiceAnnotations, string(m["cluster"])), string(m["namespace"]), svc}, true
	})
}

// resolvePVCApplications builds the (cluster, namespace, claim) → ArgoCD
// Application index from kube_persistentvolumeclaim_annotations' tracking-id
// label, keyed identically to resolvePVCStorageClass so the per-PVC assembly
// can join it. OPTIONAL/graceful and deterministic like the service variant.
func resolvePVCApplications(vec model.Vector, mc missingClusterCounts) map[pvcKey]string {
	return resolveApplications(vec, "annotation_argocd_argoproj_io_tracking_id", func(m model.Metric) (pvcKey, bool) {
		claim := string(m["persistentvolumeclaim"])
		if claim == "" {
			return pvcKey{}, false
		}
		return pvcKey{mc.bucket(promql.QPVCAnnotations, string(m["cluster"])), string(m["namespace"]), claim}, true
	})
}

// pvcInheritedApps computes, per PVC ID, the ArgoCD Application a PVC may
// inherit from the pods that mount it (D13): the lexically-smallest non-empty
// Application across all its mounting pods (from the pod-PVC bindings, joined to
// each pod's already-resolved Application via podApp keyed by pod ID). A PVC
// whose mounting pods all carry no Application is absent from the result. The
// accumulation is a pure min over the binding set, so it is independent of
// binding order (D6 determinism). The caller applies this only to PVCs that have
// no Application of their own, so an own annotation always wins.
func pvcInheritedApps(bindings []PodPVCBinding, podApp map[string]string) map[string]string {
	out := make(map[string]string)
	for _, b := range bindings {
		app := podApp[b.PodID]
		if app == "" {
			continue
		}
		if cur, ok := out[b.PVCID]; !ok || app < cur {
			out[b.PVCID] = app
		}
	}
	return out
}

// argoAppName extracts the ArgoCD Application from a tracking-id value: the
// segment before the first ":" (ArgoCD <app>:<group>/<kind>:<ns>/<name> form);
// a value with no ":" is verbatim, an empty leading segment yields "".
func argoAppName(raw string) string {
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		return raw[:i]
	}
	return raw
}

// resolveApplications builds a key → ArgoCD Application index from a vector
// carrying a tracking-id under `label`. For each key it keeps the
// lexically-smallest non-empty raw tracking-id (the tie-break is on the raw
// value, so one map suffices — deterministic), then derives the Application in
// place (argoAppName), dropping keys whose Application is empty so the map stays
// "absent when empty" (never present-but-""). keyOf returns (key, false) to skip
// a series (e.g. missing name label). Shared by the pod / service / PVC
// resolvers, which differ only in their key type, name label, and tracking
// label.
func resolveApplications[K comparable](vec model.Vector, label string, keyOf func(model.Metric) (K, bool)) map[K]string {
	out := make(map[K]string, len(vec))
	for _, s := range vec {
		raw := string(s.Metric[model.LabelName(label)])
		// Skip a value whose derived Application would be empty — an empty
		// tracking-id, or an empty leading segment like ":apps/..." — BEFORE the
		// min-pick. Otherwise a malformed sibling could win the lexically-smallest
		// race (':' = 0x3A sorts below every letter/digit) and suppress a valid
		// Application for the same key. Among the surviving (non-empty-app) series
		// the smallest raw tracking-id still wins (the documented tie-break).
		if raw == "" || argoAppName(raw) == "" {
			continue
		}
		key, ok := keyOf(s.Metric)
		if !ok {
			continue
		}
		if cur, ok := out[key]; !ok || raw < cur {
			out[key] = raw
		}
	}
	// Every surviving raw has a non-empty Application; derive it in place.
	for key, raw := range out {
		out[key] = argoAppName(raw)
	}
	return out
}

// pvcKey identifies a PVC by its cluster-scoped namespace/name for the
// StorageClass join. The claim component matches the binding metric's
// persistentvolumeclaim / claim_name and the info metric's
// persistentvolumeclaim.
type pvcKey struct{ cluster, namespace, claim string }

// resolvePVCStorageClass builds the (cluster, namespace, persistentvolumeclaim) →
// storageclass index from kube_persistentvolumeclaim_info. The result enriches
// PVC nodes that already exist (from the pod→PVC binding metric); it never
// materialises a PVC on its own.
//
// OPTIONAL: an absent or empty vector yields an empty map and PVCs carry no
// StorageClass (graceful degradation). The returned map is a deterministic
// function of the input vector — on a duplicate (cluster, namespace, claim)
// the lexically-smallest storageclass wins, so the emitted grouping is stable
// across rebuilds (D6 determinism). The only side effect is tallying
// missing-cluster samples into the caller's mc accumulator.
func resolvePVCStorageClass(vec model.Vector, mc missingClusterCounts) map[pvcKey]string {
	out := make(map[pvcKey]string, len(vec))
	for _, s := range vec {
		cluster := mc.bucket(promql.QPVCInfo, string(s.Metric["cluster"]))
		ns := string(s.Metric["namespace"])
		claim := string(s.Metric["persistentvolumeclaim"])
		sc := string(s.Metric["storageclass"])
		if claim == "" || sc == "" {
			continue
		}
		key := pvcKey{cluster, ns, claim}
		if cur, ok := out[key]; ok && cur <= sc {
			continue
		}
		out[key] = sc
	}
	return out
}

// storageClassKey identifies a StorageClass by its cluster-scoped name. The
// name is cluster-scoped (StorageClass names are not globally unique).
type storageClassKey struct{ cluster, name string }

// resolveStorageClassInfo builds the (cluster, storageclass) → typed
// provisioner/parameters index from kube_storageclass_info. The result
// materialises real StorageClass nodes; it does not enrich any existing node.
//
// The `provisioner` parameter comes from the native KSM `provisioner` label.
// The `parameters` object carries the operator-allowlisted NetApp/Ceph values,
// each resolved "first non-empty source label wins" PER SERIES
// (storagePools→pool, fsType→fsName, ClusterID, selector), then the
// lexically-smallest non-empty value ACROSS series so the emitted node is a
// pure function of the data (D6 determinism). The native reclaim_policy /
// volume_binding_mode fields are out of scope and never read.
//
// OPTIONAL: an absent or empty vector yields an empty map (all referenced
// StorageClass nodes are then materialised bare by the caller). The only side
// effect is tallying missing-cluster samples into the caller's mc accumulator.
func resolveStorageClassInfo(vec model.Vector, mc missingClusterCounts) map[storageClassKey]*graph.StorageClassInfo {
	type acc struct {
		provisioner, pool, fs, clusterID, selector string
	}
	pick := func(cur *string, val string) {
		if val == "" {
			return
		}
		if *cur == "" || val < *cur {
			*cur = val
		}
	}
	firstNonEmpty := func(primary, fallback string) string {
		if primary != "" {
			return primary
		}
		return fallback
	}

	raw := make(map[storageClassKey]*acc, len(vec))
	for _, s := range vec {
		name := string(s.Metric["storageclass"])
		if name == "" {
			continue
		}
		cluster := mc.bucket(promql.QStorageClassInfo, string(s.Metric["cluster"]))
		key := storageClassKey{cluster, name}
		a := raw[key]
		if a == nil {
			a = &acc{}
			raw[key] = a
		}
		pick(&a.provisioner, string(s.Metric["provisioner"]))
		pick(&a.pool, firstNonEmpty(string(s.Metric["storagePools"]), string(s.Metric["pool"])))
		pick(&a.fs, firstNonEmpty(string(s.Metric["fsType"]), string(s.Metric["fsName"])))
		pick(&a.clusterID, string(s.Metric["ClusterID"]))
		pick(&a.selector, string(s.Metric["selector"]))
	}

	out := make(map[storageClassKey]*graph.StorageClassInfo, len(raw))
	for key, a := range raw {
		params := map[string]string{}
		if a.pool != "" {
			params["pool"] = a.pool
		}
		if a.fs != "" {
			params["fs"] = a.fs
		}
		if a.clusterID != "" {
			params["cluster_id"] = a.clusterID
		}
		if a.selector != "" {
			params["selector"] = a.selector
		}
		info := &graph.StorageClassInfo{Provisioner: a.provisioner}
		if len(params) > 0 {
			info.Parameters = params
		}
		out[key] = info
	}
	return out
}

// readyStatusFromLabel maps a kube_node_status_condition `status` label to the
// graph ReadyStatus value. The caller canonicalises casing to lowercase first
// (the contract does not pin status-label casing — stock KSM lowercases, a
// raw-enum exporter emits "True"/"False"/"Unknown"), so this matches the
// lowercase forms. Any other value yields "" so a malformed status never
// surfaces as a non-enum attribute — the caller drops it (omit ready_status).
func readyStatusFromLabel(status string) string {
	switch status {
	case "true":
		return graph.ReadyStatusReady
	case "false":
		return graph.ReadyStatusNotReady
	case "unknown":
		return graph.ReadyStatusUnknown
	}
	return ""
}

// resolveNodeReadyStatus builds the (cluster, node) → Ready-status index from
// kube_node_status_condition. For condition="Ready", kube-state-metrics emits
// one series per status (true/false/unknown) with value 1 for the active one;
// the reader reads the active row's `status` label and maps it to
// ReadyStatusReady / ReadyStatusNotReady / ReadyStatusUnknown.
//
// The condition="Ready" selector is applied at the query layer; the condition
// guard here is defensive against a wider selector. Only rows with value == 1
// AND a recognised status are considered, so a 0-valued (inactive) row, or a
// malformed status, never wins. On the defensive case where more than one row
// is active for the same (cluster, node) — which correct KSM never emits — the
// lexically-smallest `status` label wins, so the result is order-free (D6).
//
// OPTIONAL: an absent or empty vector yields an empty map and nodes carry no
// ready_status (graceful degradation). "" (absent) is intentionally distinct
// from ReadyStatusUnknown (kubelet lost contact). The returned map is a
// deterministic function of the input vector; the only side effect is tallying
// missing-cluster samples into the caller's mc accumulator.
func resolveNodeReadyStatus(vec model.Vector, mc missingClusterCounts) map[[2]string]string {
	// (cluster, node) → lexically-smallest active, recognised `status` label.
	raw := make(map[[2]string]string, len(vec))
	for _, s := range vec {
		if string(s.Metric["condition"]) != "Ready" {
			continue
		}
		if s.Value != 1 {
			continue
		}
		// Canonicalise casing at the read site so the guard, the lexical
		// tie-break below, and the final mapping all operate on one casing. The
		// `status` value casing is NOT pinned by the KSM-shaped contract: stock
		// kube-state-metrics lowercases it (addConditionMetrics → strings.ToLower),
		// but an exporter that re-publishes the raw Kubernetes v1.ConditionStatus
		// enum verbatim emits "True"/"False"/"Unknown" — both must resolve.
		status := strings.ToLower(string(s.Metric["status"]))
		if readyStatusFromLabel(status) == "" {
			continue
		}
		cluster := mc.bucket(promql.QNodeStatusCondition, string(s.Metric["cluster"]))
		node := string(s.Metric["node"])
		if node == "" {
			continue
		}
		key := [2]string{cluster, node}
		if cur, ok := raw[key]; ok && cur <= status {
			continue
		}
		raw[key] = status
	}

	out := make(map[[2]string]string, len(raw))
	for key, status := range raw {
		out[key] = readyStatusFromLabel(status)
	}
	return out
}

// bucketCluster returns "unknown" when the upstream cluster label is missing.
func bucketCluster(c string) string {
	if c == "" {
		return "unknown"
	}
	return c
}

// missingClusterCounts tallies, per upstream metric, samples whose `cluster`
// label is absent and were therefore bucketed into the "unknown" cluster.
type missingClusterCounts map[promql.Query]int

// bucket records a missing cluster label against metric and returns the
// bucketed cluster name (see bucketCluster).
func (m missingClusterCounts) bucket(metric promql.Query, c string) string {
	if c == "" {
		m[metric]++
	}
	return bucketCluster(c)
}

// warn emits one aggregated warning per affected metric — not one per sample,
// so a whole cluster missing the label cannot flood the log — letting
// operators spot which exporter feeds the "unknown" cluster. Sorted iteration
// keeps the log order deterministic.
func (m missingClusterCounts) warn() {
	for _, q := range slices.Sorted(maps.Keys(m)) {
		slog.Warn(`samples missing cluster label; bucketed into "unknown" cluster`,
			"metric", string(q),
			"samples", m[q],
		)
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
