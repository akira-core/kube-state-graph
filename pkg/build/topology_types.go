package build

import (
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
	"github.com/prometheus/common/model"
)

// PodPVCBinding records that a pod mounts a specific PVC. The reader emits
// these so the edge builder can wire pod-mounts-pvc.
type PodPVCBinding struct {
	PodID string
	PVCID string
}

// SVMRef is a claim's resolved SVM, qualified by the ONTAP cluster it lives on.
// SVM names are unique within a cluster but not across filers, so the pair is
// the identity — and the pair is what NetAppSVMID needs.
type SVMRef struct {
	ONTAPCluster string
	SVM          string
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
	Pods         []*graph.PodNode
	Nodes        []*graph.K8sNode
	PVCs         []*graph.PVCNode
	NetAppAggrs  []*graph.NetAppAggrNode
	NetAppNodes  []*graph.NetAppNode
	StorageEdges []*graph.Edge
	PodPVCs      []PodPVCBinding

	// ClaimSeeded records that the build was claim-seeded (a pvc or pv root),
	// copied from topologyVectors.MaterialiseUnboundClaims by the parse. It is
	// the ONE statement of that fact downstream of the read: the parse has
	// materialised every root claim no pod mounts, and assembleStorageFlow
	// draws their chains as sinks. Carrying it here rather than as a second
	// argument keeps the two from disagreeing.
	ClaimSeeded bool

	// NetAppInventory is every NetApp entity the Harvest read NAMED, whether or
	// not a claim joined it. It is deliberately wider than NetAppAggrs /
	// NetAppNodes above, which stay join-only so GET /v1/graph is unchanged:
	// the storage-flow graph needs flowless roots (a degraded aggregate serving
	// no claim is a valid answer to "what is on this filer?"), and a storage
	// root must stay drawable when no claim reaches it. An aggr, ontap_node or
	// ontap_cluster root therefore reads its aggregate and controller gauges
	// for that root (readFlowlessHarvest). A pod, node, svm or application
	// root still reads those gauges across the zone. Roots reach the pod scope
	// (readScopedPods), the node scope (readScopedNodes) and — for an
	// ontap_cluster= / aggr= / svm= / ontap_node= root — the volume_labels leg
	// itself (readRootedVolumeLabels), which is safe because that leg is
	// restricted to exactly the components the roots name, every aggregate an
	// svm= root touches is re-read whole for its owner vote, and each matched
	// claim's whole candidate set is recovered in a second phase.
	//
	// Its size is bounded by the FILER (tens of aggregates, hundreds of SVMs),
	// not by the Kubernetes estate, and a flowless entity costs nothing at
	// projection — it is dropped unless it is a root.
	NetAppInventory NetAppInventory

	// Alerts is the RAW ALERTS vector, carried unparsed. Matching runs against
	// the ASSEMBLED node set — which the service-graph read contributes synth
	// pods to — so it cannot happen at parse time, and holding the vector is
	// what lets Build resolve it at the one point every node exists but the
	// graph is not yet frozen.
	Alerts model.Vector

	// SVMByPVC maps a PVC node id to the SVM its FlexVol lives in, as resolved
	// by hop A. The PVC's `svm` LABEL carries only the name; this carries the
	// ONTAP cluster with it, which the storage-flow assembler needs because an
	// SVM node id is cluster-qualified and a FlexGroup claim — entering the
	// chain AT the SVM — has no aggregate to borrow the cluster from.
	SVMByPVC map[string]SVMRef

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

	// RawSeriesCount records how many series each topology query returned,
	// keyed by query name. Diagnostic only: the build pipeline uses it to
	// enrich the outside-retention error so an operator can tell which
	// upstream metric came back empty (0 series) versus returned rows that
	// were all discarded in Go (count > 0 but parsed 0, e.g. kube_pod_info
	// samples with an empty uid).
	//
	// It is a POST-selector count, not a raw object count. Seven legs carry a
	// fixed selector that pre-filters what the reader would have discarded
	// anyway — kube_job_owner (CronJob controllers only) and the six
	// controller-annotation families (annotated objects only) — so for those a
	// 0 means "nothing matched the fixed selector", never "the collector is
	// off". Every leg is also narrowed by the request's own az/env/cluster/
	// namespace matchers per promql.queryDims. And because
	// kube_replicaset_annotations, kube_job_annotations and
	// kube_pod_container_info are fetchOptional, a 0 for those three ALSO
	// covers "the query errored and the leg degraded" — the accompanying
	// `optional topology query failed` Warn is the only thing that separates
	// the two.
	//
	// A family the read did not ISSUE has no key at all: a leg the endpoint's
	// plan skips (the storage build never reads the container or service-side
	// families) and a second-wave family whose scope came out empty (no claim
	// matched a FlexVol; no pod, node or controller name reached its scope).
	// An absent key means "never read"; 0 means "read, matched nothing". This
	// covers the twelve by-reference families under a storage build
	// (scope-controller-legs-by-reference) exactly as it already covers the
	// QoS workload six: a StatefulSet-only estate, for example, carries a
	// `kube_statefulset_annotations` entry but no key at all for
	// `kube_job_owner`, `kube_deployment_annotations`, or any other kind no
	// loaded pod is owned by.
	RawSeriesCount map[string]int

	// ClusterIdentities is the identity table the reader composed, handed to
	// the built graph so the projection-level `cluster` filter can recover each
	// identity's RAW component. Nil for an estate that stamps no az/env pair.
	ClusterIdentities map[string]graph.ClusterIdentity

	// clusters is the resolver that composed those identities. Carried so the
	// service-graph reader resolves the trace `cluster` label and the route
	// store's cluster names through the SAME table — a second resolver could
	// hold a different one and the two would silently disagree.
	clusters *clusterResolver

	// ontapZones is the `az` / `env` zone set of every ONTAP cluster the
	// Harvest read named, for the zone-agreeing alert match
	// (read-storage-roots-through-volume-hub D11). Nil when no Harvest series
	// carried the pair, which leaves NetApp alerts matching by label alone.
	ontapZones map[string][]zone
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
	// Job → CronJob resolution, for pod ArgoCD Application resolution ONLY:
	// resolvePodOwners never reads it, so the pod `owner` attribute cannot move.
	JobOwner model.Vector
	// PVC StorageClass name + bound PV name.
	PVCInfo model.Vector
	// Pod container list resolution (name/image per container).
	PodContainerInfo model.Vector
	// K8s node Ready-status resolution (kube_node_status_condition).
	NodeStatus model.Vector
	// Service / PVC ArgoCD Application resolution (annotation tracking-id).
	ServiceAnnotations model.Vector
	PVCAnnotations     model.Vector
	// Pod ArgoCD Application resolution — one annotation family per controller
	// kind kube-state-metrics can describe. ArgoCD stamps its tracking-id on the
	// managed controller, never on the pods it spawns.
	DeploymentAnnotations  model.Vector
	StatefulSetAnnotations model.Vector
	DaemonSetAnnotations   model.Vector
	ReplicaSetAnnotations  model.Vector
	JobAnnotations         model.Vector
	CronJobAnnotations     model.Vector
	// JobAnnotationsDegraded records that the kube_job_annotations leg came back
	// empty BECAUSE THE QUERY FAILED, as opposed to genuinely matching nothing.
	// resolvePodApplications needs the two told apart: its Job → CronJob hop is
	// gated on "this Job carries no annotation of its own", which only a leg that
	// was actually read can establish.
	//
	// kube_replicaset_annotations — the other degrading family — needs no such
	// flag: a bare ReplicaSet has no further ancestor to consult, so a miss
	// resolves no Application either way and the degrade stays subtractive on
	// its own.
	JobAnnotationsDegraded bool

	// ScopeIssued records, per query, that a second wave issued at least one
	// chunk for that family (QoS workload, by-reference pod, by-reference
	// node, by-reference controller). tallySeries reads it after g.Wait() so a
	// family is counted iff it was actually read — an entry the map does not
	// carry means the family's scope was empty and nothing was issued.
	//
	// Several second-wave goroutines (QoS, pods, nodes, controllers) run
	// CONCURRENTLY and write into this ONE map, so every write goes through
	// the package-level markScopeIssued under a caller-supplied *sync.Mutex —
	// Go maps are not safe for concurrent writes even to disjoint keys. The
	// mutex is deliberately NOT a field of this struct: topologyVectors is
	// passed BY VALUE into a couple of dozen existing resolver functions
	// (parseTopology, resolveControllerApplications, resolveNetAppStorage,
	// volumeKey, ...), and embedding a sync.Mutex here would make every one of
	// those calls a lock copy — go vet's copylocks check flags it, correctly.
	// Reads happen only after g.Wait(), when every writer has returned, so
	// they need no lock.
	ScopeIssued map[promql.Query]bool

	// ExtraSeriesCount is the series a wave read WITHOUT landing them in a
	// slot above — the application recovery, whose vectors are local because
	// the by-reference controller wave writes the same families later.
	// tallySeries adds the count into the family's entry (creating it when
	// the by-reference wave did not issue the family). Writes go through
	// addExtraSeries under the same mutex as ScopeIssued.
	ExtraSeriesCount map[promql.Query]int

	// VolumeKey derives each claim's Harvest match token from its bound PV
	// name and decides how that token is compared against the stock `volume`
	// label. Like JobAnnotationsDegraded it is a build-scoped FACT rather than
	// a vector, carried here so parseTopology keeps its two-argument shape.
	// Nil means the defaults (`-` → `_`, suffix match).
	VolumeKey *VolumeKeyRewriter
	// VolumeLabelsRestricted records that VolumeLabels was read restricted to
	// the request's rooted components (scope-volume-labels-by-storage-root)
	// rather than whole. Like VolumeKey and JobAnnotationsDegraded it is a
	// build-scoped FACT the parse needs: under a restriction every claim outside
	// the rooted components resolves no aggregate BY CONSTRUCTION, so the
	// join-coverage signal that counts such claims no longer distinguishes a
	// derivation that does not fit the estate's naming from a claim the request
	// did not ask about, and is suppressed.
	VolumeLabelsRestricted bool
	// MaterialiseUnboundClaims records that the build is claim-seeded (a pvc or
	// pv root): every claim its claim-info read returned is a root claim, and a
	// root claim no pod mounts has no binding to materialise it, so the parse
	// builds its PVC node — and runs the Harvest join over it — from the
	// claim-info row. Off everywhere else, which keeps /v1/graph and every other
	// root kind from drawing an unmounted claim. A build-scoped FACT like
	// VolumeLabelsRestricted.
	MaterialiseUnboundClaims bool
	// NetApp Harvest storage series, in join order (design.md D3):
	// hop A the volume label series (topology), hop B the QoS workload
	// families (I/O), hop C the QoS fixed-policy ceilings.
	VolumeLabels     model.Vector
	QoSReadOps       model.Vector
	QoSWriteOps      model.Vector
	QoSReadLatency   model.Vector
	QoSWriteLatency  model.Vector
	QoSReadData      model.Vector
	QoSWriteData     model.Vector
	QoSPolicyMaxIOPS model.Vector
	QoSPolicyMaxMBps model.Vector
	AggrStatus       model.Vector
	AggrSpaceUsed    model.Vector
	AggrSpaceTotal   model.Vector
	NetAppNodeStatus model.Vector
	// Controller hardware identity (an info series — labels only) and the four
	// system_node performance counters, all matched on (ontap cluster, node)
	// and all read VERBATIM. They resolve the netapp-node data.hardware and
	// data.perf attributes; none of them feeds data.health, which stays the
	// ONTAP-reported NetAppNodeStatus above.
	NetAppNodeLabels       model.Vector
	NetAppNodeCPUBusy      model.Vector
	NetAppNodeTotalOps     model.Vector
	NetAppNodeTotalLatency model.Vector
	NetAppNodeTotalData    model.Vector
	// Kubelet PVC usage.
	KubeletVolumeUsed     model.Vector
	KubeletVolumeCapacity model.Vector
	// Active alerts (the alert overlay). Unlike every other vector here it is
	// NOT consumed by parseTopology: matching needs the ASSEMBLED node set,
	// which only exists after the service-graph read has contributed its synth
	// pods, so the raw vector is carried through to Build untouched.
	Alerts model.Vector
}
