package graph

import "sort"

// NodeType is the canonical type field on every graph node.
type NodeType string

const (
	NodeTypePod        NodeType = "pod"
	NodeTypeK8sNode    NodeType = "node"
	NodeTypePVC        NodeType = "pvc"
	NodeTypeService    NodeType = "service"
	NodeTypeExternal   NodeType = "external"
	NodeTypeNetAppAggr NodeType = "netapp-aggr"
	NodeTypeNetAppNode NodeType = "netapp-node"
	NodeTypeNetAppSVM  NodeType = "netapp-svm"
)

// GraphNode is the sealed interface implemented by every node kind.
//
// Implementations expose the canonical wire fields directly so the API
// serialiser can iterate without type switches. IPAddress carries the
// observed IPv4/IPv6 strings for nodes that have them (pods → pod_ip,
// K8s nodes → ExternalIP, falling back to InternalIP when no ExternalIP
// row exists; services → cluster_ip); other node kinds return nil.
type GraphNode interface {
	ID() string
	Name() string
	Type() NodeType
	Labels() map[string]string
	IPAddress() []string
	// Owner is the node's controller owner, surfaced as a nullable top-level
	// attribute (never inside Labels, which stay strict typological metadata).
	// Only pods carry one — the intermediate ReplicaSet is skipped to its
	// owning Deployment (see build/topology). All other node kinds, and pods
	// with no controller owner, return nil.
	Owner() *Owner
	// Application is the node's ArgoCD Application name (the segment before the
	// first ":" of the tracking-id value), always read from the
	// annotation_argocd_argoproj_io_tracking_id label: for pods off their
	// CONTROLLER's annotation family (kube_deployment_annotations and its five
	// siblings — ArgoCD annotates the managed workload object, never the pods it
	// spawns), for services and PVCs off kube_service_annotations /
	// kube_persistentvolumeclaim_annotations. Surfaced as a top-level attribute,
	// never inside Labels. Only pods, services, and PVCs carry one; K8s nodes,
	// externals, NetApp types, and any pod/service/PVC with no ArgoCD
	// Application, return "".
	Application() string
	// Containers is the pod's container list ({name, image}), resolved from
	// kube_pod_container_info and ordered by (name, image). Surfaced as a
	// top-level attribute, never inside Labels. Only pods carry containers;
	// every other node kind, and a pod with no observed container info, returns
	// nil.
	Containers() []Container
	// ReadyStatus is a K8s node's Kubernetes Ready-condition status, resolved
	// from the active kube_node_status_condition{condition="Ready"} row — one of
	// ReadyStatusReady / ReadyStatusNotReady / ReadyStatusUnknown. Surfaced as a
	// top-level attribute, never inside Labels. Only K8s nodes carry one; every
	// other node kind, and a node with no Ready-condition data, returns "" (the
	// serialiser omits the attribute). The literal "Unknown" is the genuine
	// Kubernetes kubelet-lost state — distinct from "" (no data).
	ReadyStatus() string
	// Health is a NetApp aggregate or controller health string — HealthOnline
	// or HealthDegraded — from aggr_new_status / node_new_status. Surfaced as
	// a top-level attribute, never inside Labels. Only NetApp types carry one;
	// every other node kind, and a NetApp node with no status data, returns
	// "" (the serialiser omits the attribute). Absence is distinct from
	// HealthDegraded.
	Health() string
	// Usage is used/capacity bytes. Present on PVCNode (kubelet volume stats)
	// and NetAppAggrNode (Harvest aggr space); nil for every other type and
	// when neither field resolved. Pointer fields allow per-field omission.
	Usage() *UsageBytes
	// StorageClass is a PVC's resolved StorageClass name, serialised as the
	// PVC's own data.storageclass attribute (omitempty). It is NOT a label
	// and does not materialise a node or edge. Only PVCs carry one; every
	// other node kind, and a PVC with no resolved StorageClass, returns "".
	StorageClass() string
	// Hardware is an ONTAP controller's hardware identity, resolved from the
	// Harvest `node_labels` info series. Surfaced as a top-level attribute,
	// never inside Labels. Only NetAppNode carries one; every other node kind,
	// and a controller no node_labels series matched, returns nil.
	Hardware() *Hardware
	// Perf is an ONTAP controller's raw performance counters, resolved from
	// the Harvest `system_node` family and read VERBATIM. Surfaced as a
	// top-level attribute, never inside Labels. Only NetAppNode carries one;
	// every other node kind, and a controller no counter matched, returns nil.
	//
	// These figures are deliberately NOT turned into a health verdict —
	// Health() stays the ONTAP-reported node_new_status. Thresholds are model-
	// and estate-specific (an A400 at 70 % CPU is idle, a FAS2720 is not) and
	// belong in the operator's alert rules, whose verdicts reach the node
	// through Alerts().
	Perf() *NodePerf
	// Alerts is the set of active alerts the ALERTS overlay matched to this
	// node, sorted by (Name, Severity) and de-duplicated on that pair.
	// Surfaced as a top-level attribute, never inside Labels. Only pods, K8s
	// nodes, PVCs, NetApp controllers and NetApp aggregates can carry alerts;
	// services, externals, SVMs and every unalerted node return nil (the
	// serialiser omits the attribute, so an unalerted estate serialises
	// byte-identically to one built before the overlay existed).
	Alerts() []Alert

	isGraphNode()
}

// Owner is a pod's resolved controller owner. Kind/Name are the owning
// workload after the intermediate ReplicaSet has been collapsed to its
// Deployment; a bare ReplicaSet keeps Kind="ReplicaSet". Emitted as the
// `owner` object on a pod's Cytoscape data, omitted when nil.
type Owner struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Container is one container of a pod — its name and image, resolved from
// kube_pod_container_info. Emitted as an element of a pod's `containers` array
// on its Cytoscape data; only pods carry containers.
type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// UsageBytes is used/capacity storage usage in bytes. Either field may be
// nil (per-field OPTIONAL); the object itself is omitted when both are
// unresolved. Shared by PVCNode (kubelet) and NetAppAggrNode (Harvest).
type UsageBytes struct {
	UsedBytes     *float64
	CapacityBytes *float64
}

// Hardware is an ONTAP controller's hardware identity, read verbatim from the
// like-named labels of the Harvest `node_labels` info series. Every field is
// independently optional and omitted by the serialiser when empty; the object
// itself is omitted when no field resolved. It is a typed attribute, never a
// label — the ipaddress / owner / ready_status precedent.
type Hardware struct {
	Model    string `json:"model,omitempty"`
	Serial   string `json:"serial,omitempty"`
	Version  string `json:"version,omitempty"`
	Vendor   string `json:"vendor,omitempty"`
	Location string `json:"location,omitempty"`
}

// Empty reports whether no field of h resolved. A Hardware that is Empty must
// not be attached: the attribute is omitted entirely rather than serialised as
// an object of absent keys.
func (h Hardware) Empty() bool {
	return h.Model == "" && h.Serial == "" && h.Version == "" &&
		h.Vendor == "" && h.Location == ""
}

// NodePerf is an ONTAP controller's raw performance counters, read VERBATIM
// from the Harvest `system_node` family — Harvest has already resolved the
// ONTAP base counters, so no rate() is applied and no unit is converted.
//
// Each field is a pointer so an absent counter is omitted rather than
// serialised as 0, and the object itself is omitted when none resolved. The
// builder never derives a health verdict from these figures; see the Perf()
// doc comment on GraphNode.
type NodePerf struct {
	// CPUBusyPct is `node_cpu_busy` — a percentage.
	CPUBusyPct *float64
	// TotalOps is `node_total_ops` — operations per second.
	TotalOps *float64
	// TotalLatencyUs is `node_total_latency` — an average in microseconds.
	TotalLatencyUs *float64
	// TotalBytesPerSec is `node_total_data` — bytes per second.
	TotalBytesPerSec *float64
}

// Empty reports whether no counter of p resolved.
func (p NodePerf) Empty() bool {
	return p.CPUBusyPct == nil && p.TotalOps == nil &&
		p.TotalLatencyUs == nil && p.TotalBytesPerSec == nil
}

// Alert is one active alert the ALERTS overlay matched to a node. Name is the
// `alertname` label, State the `alertstate` label (always "firing" — pending
// alerts are excluded at the query layer), Severity the `severity` label,
// omitted when empty.
//
// The set on a node is sorted by (Name, Severity) and de-duplicated on that
// pair, so two ALERTS series differing only in a label the matcher does not
// read collapse to one entry and the wire form is order-independent.
type Alert struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Severity string `json:"severity,omitempty"`
}

// AlertStateFiring is the only `alertstate` value that reaches the graph. A
// `pending` alert is a threshold crossed for less than its `for:` duration and
// is excluded upstream by the query's fixed selector.
const AlertStateFiring = "firing"

// SortAlerts orders alerts by (Name, Severity) and removes duplicates on that
// pair, in place semantics aside — it returns the normalised slice. Callers
// attach the RESULT; the ordering is what makes the serialised attribute a
// pure function of the matched set rather than of series arrival order.
func SortAlerts(alerts []Alert) []Alert {
	if len(alerts) == 0 {
		return nil
	}
	sort.SliceStable(alerts, func(i, j int) bool {
		if alerts[i].Name != alerts[j].Name {
			return alerts[i].Name < alerts[j].Name
		}
		return alerts[i].Severity < alerts[j].Severity
	})
	out := alerts[:0]
	type key struct{ name, severity string }
	seen := make(map[key]struct{}, len(alerts))
	for _, a := range alerts {
		k := key{a.Name, a.Severity}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, a)
	}
	return out
}

// Health values for NetAppAggrNode / NetAppNode Health(). The empty string
// is the absent/no-data state and is omitted by the serialiser — never
// confuse it with HealthDegraded, which is a reported unhealthy state.
const (
	HealthOnline   = "online"
	HealthDegraded = "degraded"
)

// Ready-status values for a K8s node's ReadyStatus() (the Kubernetes Ready
// condition). The empty string is the absent/no-data state and is omitted by
// the serialiser — never confuse it with ReadyStatusUnknown, which is the
// genuine Kubernetes state where the kubelet has stopped reporting.
const (
	ReadyStatusReady    = "Ready"
	ReadyStatusNotReady = "NotReady"
	ReadyStatusUnknown  = "Unknown"
)

// PodNode represents a Kubernetes pod entity (or a synthesised pod when the
// service-graph reader observes a pod UID with no topology).
type PodNode struct {
	IDValue          string
	NameValue        string
	LabelsValue      map[string]string
	IPAddressValue   []string
	OwnerValue       *Owner
	ApplicationValue string
	ContainersValue  []Container
	AlertsValue      []Alert
}

func (p *PodNode) ID() string                { return p.IDValue }
func (p *PodNode) Name() string              { return p.NameValue }
func (p *PodNode) Type() NodeType            { return NodeTypePod }
func (p *PodNode) Labels() map[string]string { return p.LabelsValue }
func (p *PodNode) IPAddress() []string       { return p.IPAddressValue }
func (p *PodNode) Owner() *Owner             { return p.OwnerValue }
func (p *PodNode) Application() string       { return p.ApplicationValue }
func (p *PodNode) Containers() []Container   { return p.ContainersValue }
func (p *PodNode) ReadyStatus() string       { return "" }
func (p *PodNode) Health() string            { return "" }
func (p *PodNode) Usage() *UsageBytes        { return nil }
func (p *PodNode) StorageClass() string      { return "" }
func (p *PodNode) Hardware() *Hardware       { return nil }
func (p *PodNode) Perf() *NodePerf           { return nil }
func (p *PodNode) Alerts() []Alert           { return p.AlertsValue }
func (p *PodNode) isGraphNode()              {}

// K8sNode represents a Kubernetes node entity. ReadyStatusValue carries the
// node's Kubernetes Ready-condition status (from kube_node_status_condition) —
// one of ReadyStatusReady / ReadyStatusNotReady / ReadyStatusUnknown, or "" when
// no Ready-condition data was observed (the serialiser omits the attribute).
type K8sNode struct {
	IDValue          string
	NameValue        string
	LabelsValue      map[string]string
	IPAddressValue   []string
	ReadyStatusValue string
	AlertsValue      []Alert
}

func (n *K8sNode) ID() string                { return n.IDValue }
func (n *K8sNode) Name() string              { return n.NameValue }
func (n *K8sNode) Type() NodeType            { return NodeTypeK8sNode }
func (n *K8sNode) Labels() map[string]string { return n.LabelsValue }
func (n *K8sNode) IPAddress() []string       { return n.IPAddressValue }
func (n *K8sNode) Owner() *Owner             { return nil }
func (n *K8sNode) Application() string       { return "" }
func (n *K8sNode) Containers() []Container   { return nil }
func (n *K8sNode) ReadyStatus() string       { return n.ReadyStatusValue }
func (n *K8sNode) Health() string            { return "" }
func (n *K8sNode) Usage() *UsageBytes        { return nil }
func (n *K8sNode) StorageClass() string      { return "" }
func (n *K8sNode) Hardware() *Hardware       { return nil }
func (n *K8sNode) Perf() *NodePerf           { return nil }
func (n *K8sNode) Alerts() []Alert           { return n.AlertsValue }
func (n *K8sNode) isGraphNode()              {}

// PVCNode represents a PersistentVolumeClaim entity. StorageClassValue is the
// PVC's resolved StorageClass name (from kube_persistentvolumeclaim_info),
// serialised as data.storageclass. UsageValue is kubelet volume-stats usage
// (used/capacity bytes). Empty / nil when unresolved.
type PVCNode struct {
	IDValue           string
	NameValue         string
	LabelsValue       map[string]string
	StorageClassValue string
	ApplicationValue  string
	UsageValue        *UsageBytes
	AlertsValue       []Alert
}

func (p *PVCNode) ID() string                { return p.IDValue }
func (p *PVCNode) Name() string              { return p.NameValue }
func (p *PVCNode) Type() NodeType            { return NodeTypePVC }
func (p *PVCNode) Labels() map[string]string { return p.LabelsValue }
func (p *PVCNode) IPAddress() []string       { return nil }
func (p *PVCNode) Owner() *Owner             { return nil }
func (p *PVCNode) Application() string       { return p.ApplicationValue }
func (p *PVCNode) Containers() []Container   { return nil }
func (p *PVCNode) ReadyStatus() string       { return "" }
func (p *PVCNode) Health() string            { return "" }
func (p *PVCNode) Usage() *UsageBytes        { return p.UsageValue }
func (p *PVCNode) StorageClass() string      { return p.StorageClassValue }
func (p *PVCNode) Hardware() *Hardware       { return nil }
func (p *PVCNode) Perf() *NodePerf           { return nil }
func (p *PVCNode) Alerts() []Alert           { return p.AlertsValue }
func (p *PVCNode) isGraphNode()              {}

// ServiceNode represents a Kubernetes Service surfaced when a service-graph
// connection string (`<service>.<namespace>.svc.<domain>`) resolves to an
// in-cluster Service via `kube_service_info` (see design.md D29). Its backing
// pods are wired with `service-selects-pod` edges. IPAddressValue carries the
// service's `cluster_ip` (single-element slice) when it is not the headless
// sentinel `"None"`; headless services carry nil.
type ServiceNode struct {
	IDValue          string
	NameValue        string
	LabelsValue      map[string]string
	IPAddressValue   []string
	ApplicationValue string
}

func (s *ServiceNode) ID() string                { return s.IDValue }
func (s *ServiceNode) Name() string              { return s.NameValue }
func (s *ServiceNode) Type() NodeType            { return NodeTypeService }
func (s *ServiceNode) Labels() map[string]string { return s.LabelsValue }
func (s *ServiceNode) IPAddress() []string       { return s.IPAddressValue }
func (s *ServiceNode) Owner() *Owner             { return nil }
func (s *ServiceNode) Application() string       { return s.ApplicationValue }
func (s *ServiceNode) Containers() []Container   { return nil }
func (s *ServiceNode) ReadyStatus() string       { return "" }
func (s *ServiceNode) Health() string            { return "" }
func (s *ServiceNode) Usage() *UsageBytes        { return nil }
func (s *ServiceNode) StorageClass() string      { return "" }
func (s *ServiceNode) Hardware() *Hardware       { return nil }
func (s *ServiceNode) Perf() *NodePerf           { return nil }
func (s *ServiceNode) Alerts() []Alert           { return nil }
func (s *ServiceNode) isGraphNode()              {}

// ExternalNode represents a non-pod endpoint surfaced by the missing-UID
// human-label fallback (D27): the service-graph producer dropped
// client_k8s_pod_uid or server_k8s_pod_uid, but the human-readable
// client/server label survived.
type ExternalNode struct {
	IDValue     string
	NameValue   string
	LabelsValue map[string]string
}

func (e *ExternalNode) ID() string                { return e.IDValue }
func (e *ExternalNode) Name() string              { return e.NameValue }
func (e *ExternalNode) Type() NodeType            { return NodeTypeExternal }
func (e *ExternalNode) Labels() map[string]string { return e.LabelsValue }
func (e *ExternalNode) IPAddress() []string       { return nil }
func (e *ExternalNode) Owner() *Owner             { return nil }
func (e *ExternalNode) Application() string       { return "" }
func (e *ExternalNode) Containers() []Container   { return nil }
func (e *ExternalNode) ReadyStatus() string       { return "" }
func (e *ExternalNode) Health() string            { return "" }
func (e *ExternalNode) Usage() *UsageBytes        { return nil }
func (e *ExternalNode) StorageClass() string      { return "" }
func (e *ExternalNode) Hardware() *Hardware       { return nil }
func (e *ExternalNode) Perf() *NodePerf           { return nil }
func (e *ExternalNode) Alerts() []Alert           { return nil }
func (e *ExternalNode) isGraphNode()              {}

// NetAppAggrNode is one ONTAP aggregate. Id excludes the owning node so an
// HA takeover moves labels.node (and the compound parent) without changing
// identity. Labels are exactly {ontap_cluster, node} — no cluster key.
type NetAppAggrNode struct {
	IDValue     string
	NameValue   string
	LabelsValue map[string]string
	HealthValue string
	UsageValue  *UsageBytes
	AlertsValue []Alert
}

func (n *NetAppAggrNode) ID() string                { return n.IDValue }
func (n *NetAppAggrNode) Name() string              { return n.NameValue }
func (n *NetAppAggrNode) Type() NodeType            { return NodeTypeNetAppAggr }
func (n *NetAppAggrNode) Labels() map[string]string { return n.LabelsValue }
func (n *NetAppAggrNode) IPAddress() []string       { return nil }
func (n *NetAppAggrNode) Owner() *Owner             { return nil }
func (n *NetAppAggrNode) Application() string       { return "" }
func (n *NetAppAggrNode) Containers() []Container   { return nil }
func (n *NetAppAggrNode) ReadyStatus() string       { return "" }
func (n *NetAppAggrNode) Health() string            { return n.HealthValue }
func (n *NetAppAggrNode) Usage() *UsageBytes        { return n.UsageValue }
func (n *NetAppAggrNode) StorageClass() string      { return "" }
func (n *NetAppAggrNode) Hardware() *Hardware       { return nil }
func (n *NetAppAggrNode) Perf() *NodePerf           { return nil }
func (n *NetAppAggrNode) Alerts() []Alert           { return n.AlertsValue }
func (n *NetAppAggrNode) isGraphNode()              {}

// NetAppNode is one physical ONTAP controller. Materialised only when
// referenced by an emitted aggregate. Labels are exactly {ontap_cluster} —
// no cluster key. It is the compound parent of its aggregates and the
// target of no edge.
type NetAppNode struct {
	IDValue       string
	NameValue     string
	LabelsValue   map[string]string
	HealthValue   string
	HardwareValue *Hardware
	PerfValue     *NodePerf
	AlertsValue   []Alert
}

func (n *NetAppNode) ID() string                { return n.IDValue }
func (n *NetAppNode) Name() string              { return n.NameValue }
func (n *NetAppNode) Type() NodeType            { return NodeTypeNetAppNode }
func (n *NetAppNode) Labels() map[string]string { return n.LabelsValue }
func (n *NetAppNode) IPAddress() []string       { return nil }
func (n *NetAppNode) Owner() *Owner             { return nil }
func (n *NetAppNode) Application() string       { return "" }
func (n *NetAppNode) Containers() []Container   { return nil }
func (n *NetAppNode) ReadyStatus() string       { return "" }
func (n *NetAppNode) Health() string            { return n.HealthValue }
func (n *NetAppNode) Usage() *UsageBytes        { return nil }
func (n *NetAppNode) StorageClass() string      { return "" }
func (n *NetAppNode) Hardware() *Hardware       { return n.HardwareValue }
func (n *NetAppNode) Perf() *NodePerf           { return n.PerfValue }
func (n *NetAppNode) Alerts() []Alert           { return n.AlertsValue }
func (n *NetAppNode) isGraphNode()              {}

// NetAppSVMNode is one ONTAP Storage Virtual Machine. An SVM spans aggregates
// and controllers, so it is NOT a compound child of a controller and is NOT
// the compound parent of anything — it nests directly under its
// storage-cluster group. Labels are exactly {ontap_cluster} — no cluster key,
// so an SVM never appears in clusters[] and is never addressed by ?cluster=.
//
// It is an IDENTITY only: this change resolves no SVM-level Harvest series, so
// every attribute accessor is nil / empty, alerts included. It is materialised
// exclusively by the storage-flow graph (as a tier of a retained claim's path,
// or as a root); GET /v1/graph never emits one.
type NetAppSVMNode struct {
	IDValue     string
	NameValue   string
	LabelsValue map[string]string
}

func (n *NetAppSVMNode) ID() string                { return n.IDValue }
func (n *NetAppSVMNode) Name() string              { return n.NameValue }
func (n *NetAppSVMNode) Type() NodeType            { return NodeTypeNetAppSVM }
func (n *NetAppSVMNode) Labels() map[string]string { return n.LabelsValue }
func (n *NetAppSVMNode) IPAddress() []string       { return nil }
func (n *NetAppSVMNode) Owner() *Owner             { return nil }
func (n *NetAppSVMNode) Application() string       { return "" }
func (n *NetAppSVMNode) Containers() []Container   { return nil }
func (n *NetAppSVMNode) ReadyStatus() string       { return "" }
func (n *NetAppSVMNode) Health() string            { return "" }
func (n *NetAppSVMNode) Usage() *UsageBytes        { return nil }
func (n *NetAppSVMNode) StorageClass() string      { return "" }
func (n *NetAppSVMNode) Hardware() *Hardware       { return nil }
func (n *NetAppSVMNode) Perf() *NodePerf           { return nil }
func (n *NetAppSVMNode) Alerts() []Alert           { return nil }
func (n *NetAppSVMNode) isGraphNode()              {}

// SortNodes orders nodes deterministically by ID for stable output.
func SortNodes(nodes []GraphNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		return nodes[i].ID() < nodes[j].ID()
	})
}

// PodID returns the cluster-scoped pod ID.
func PodID(cluster, uid string) string { return cluster + "/" + uid }

// K8sNodeID returns the cluster-scoped node ID.
func K8sNodeID(cluster, name string) string { return cluster + "/" + name }

// PVCID returns the cluster-scoped PVC ID.
func PVCID(cluster, namespace, claim string) string {
	return cluster + "/" + namespace + "/" + claim
}

// ServiceID returns the cluster-scoped Service ID (mirrors PVC keying).
func ServiceID(cluster, namespace, service string) string {
	return cluster + "/" + namespace + "/" + service
}

// NetAppAggrID returns the ONTAP-cluster-scoped aggregate node ID. The
// owning node is deliberately excluded so an HA takeover does not change
// identity.
func NetAppAggrID(ontapCluster, aggr string) string {
	return "netapp/" + ontapCluster + "/aggr/" + aggr
}

// NetAppNodeID returns the ONTAP-cluster-scoped controller node ID.
func NetAppNodeID(ontapCluster, node string) string {
	return "netapp/" + ontapCluster + "/" + node
}

// NetAppSVMID returns the ONTAP-cluster-scoped SVM node ID. SVM names are
// unique within an ONTAP cluster, so the pair is the identity. The "/svm/"
// infix keeps the id space disjoint from NetAppNodeID's, whose controller
// segment sits directly under the cluster.
func NetAppSVMID(ontapCluster, svm string) string {
	return "netapp/" + ontapCluster + "/svm/" + svm
}

// ExternalID returns the missing-UID-fallback external node ID.
func ExternalID(value string) string { return "external/" + value }

// Reserved: the "cluster/" ID prefix is owned by the Cytoscape presentation
// layer (api.clusterParentID, design.md D31) for synthetic compound group
// nodes. Those are NOT GraphNodes and are never minted here — do not reuse the
// prefix for a real node kind.
