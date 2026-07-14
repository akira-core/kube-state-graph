package graph

import "sort"

// NodeType is the canonical type field on every graph node.
type NodeType string

const (
	NodeTypePod          NodeType = "pod"
	NodeTypeK8sNode      NodeType = "node"
	NodeTypePVC          NodeType = "pvc"
	NodeTypeService      NodeType = "service"
	NodeTypeExternal     NodeType = "external"
	NodeTypeStorageClass NodeType = "storageclass"
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
	// first ":" of the tracking-id value). For pods it is resolved from the
	// argocd_tracking_id label on kube_pod_owner; for services and PVCs from the
	// annotation_argocd_argoproj_io_tracking_id label on kube_service_annotations /
	// kube_persistentvolumeclaim_annotations. Surfaced as a top-level attribute,
	// never inside Labels. Only pods, services, and PVCs carry one; K8s nodes,
	// externals, StorageClass nodes, and any pod/service/PVC with no ArgoCD
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
	// StorageClass is a PVC's resolved StorageClass name. It is NOT serialised
	// as a node attribute and NOT a label — the topology reader consumes it to
	// build the PVC's pvc-to-storageclass edge to the real StorageClass node
	// (see design.md). Only PVCs carry one; every other node kind, and a PVC
	// with no resolved StorageClass, returns "".
	StorageClass() string
	// StorageClassInfo is a StorageClass node's provisioner + backing-storage
	// parameters, surfaced as the top-level data.provisioner / data.parameters
	// attributes (never inside Labels, which stay {cluster}). Only attributed
	// StorageClass nodes carry one; every other node kind, and a bare
	// StorageClass node (referenced by a PVC but absent from
	// kube_storageclass_info), return nil.
	StorageClassInfo() *StorageClassInfo

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

// StorageClassInfo is a StorageClass node's typed attributes: the provisioner
// (native kube_storageclass_info `provisioner` label) and the NetApp/Ceph
// backing-storage parameters (pool/fs/cluster_id/selector, resolved with
// source-label fallback). Emitted as data.provisioner / data.parameters on a
// StorageClass node's Cytoscape data — never inside Labels (which stay
// {cluster}). Parameters keys are present only when their source label
// resolved non-empty; Parameters is nil/empty when no parameter resolved.
type StorageClassInfo struct {
	Provisioner string            `json:"provisioner,omitempty"`
	Parameters  map[string]string `json:"parameters,omitempty"`
}

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
}

func (p *PodNode) ID() string                          { return p.IDValue }
func (p *PodNode) Name() string                        { return p.NameValue }
func (p *PodNode) Type() NodeType                      { return NodeTypePod }
func (p *PodNode) Labels() map[string]string           { return p.LabelsValue }
func (p *PodNode) IPAddress() []string                 { return p.IPAddressValue }
func (p *PodNode) Owner() *Owner                       { return p.OwnerValue }
func (p *PodNode) Application() string                 { return p.ApplicationValue }
func (p *PodNode) Containers() []Container             { return p.ContainersValue }
func (p *PodNode) ReadyStatus() string                 { return "" }
func (p *PodNode) StorageClass() string                { return "" }
func (p *PodNode) StorageClassInfo() *StorageClassInfo { return nil }
func (p *PodNode) isGraphNode()                        {}

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
}

func (n *K8sNode) ID() string                          { return n.IDValue }
func (n *K8sNode) Name() string                        { return n.NameValue }
func (n *K8sNode) Type() NodeType                      { return NodeTypeK8sNode }
func (n *K8sNode) Labels() map[string]string           { return n.LabelsValue }
func (n *K8sNode) IPAddress() []string                 { return n.IPAddressValue }
func (n *K8sNode) Owner() *Owner                       { return nil }
func (n *K8sNode) Application() string                 { return "" }
func (n *K8sNode) Containers() []Container             { return nil }
func (n *K8sNode) ReadyStatus() string                 { return n.ReadyStatusValue }
func (n *K8sNode) StorageClass() string                { return "" }
func (n *K8sNode) StorageClassInfo() *StorageClassInfo { return nil }
func (n *K8sNode) isGraphNode()                        {}

// PVCNode represents a PersistentVolumeClaim entity. StorageClassValue is the
// PVC's resolved StorageClass name (from kube_persistentvolumeclaim_info); the
// topology reader consumes it to build the PVC's pvc-to-storageclass edge and
// it is never serialised as a node attribute or label. Empty when unresolved.
type PVCNode struct {
	IDValue           string
	NameValue         string
	LabelsValue       map[string]string
	StorageClassValue string
	ApplicationValue  string
}

func (p *PVCNode) ID() string                          { return p.IDValue }
func (p *PVCNode) Name() string                        { return p.NameValue }
func (p *PVCNode) Type() NodeType                      { return NodeTypePVC }
func (p *PVCNode) Labels() map[string]string           { return p.LabelsValue }
func (p *PVCNode) IPAddress() []string                 { return nil }
func (p *PVCNode) Owner() *Owner                       { return nil }
func (p *PVCNode) Application() string                 { return p.ApplicationValue }
func (p *PVCNode) Containers() []Container             { return nil }
func (p *PVCNode) ReadyStatus() string                 { return "" }
func (p *PVCNode) StorageClass() string                { return p.StorageClassValue }
func (p *PVCNode) StorageClassInfo() *StorageClassInfo { return nil }
func (p *PVCNode) isGraphNode()                        {}

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

func (s *ServiceNode) ID() string                          { return s.IDValue }
func (s *ServiceNode) Name() string                        { return s.NameValue }
func (s *ServiceNode) Type() NodeType                      { return NodeTypeService }
func (s *ServiceNode) Labels() map[string]string           { return s.LabelsValue }
func (s *ServiceNode) IPAddress() []string                 { return s.IPAddressValue }
func (s *ServiceNode) Owner() *Owner                       { return nil }
func (s *ServiceNode) Application() string                 { return s.ApplicationValue }
func (s *ServiceNode) Containers() []Container             { return nil }
func (s *ServiceNode) ReadyStatus() string                 { return "" }
func (s *ServiceNode) StorageClass() string                { return "" }
func (s *ServiceNode) StorageClassInfo() *StorageClassInfo { return nil }
func (s *ServiceNode) isGraphNode()                        {}

// ExternalNode represents a non-pod endpoint surfaced by the missing-UID
// human-label fallback (D27): the service-graph producer dropped
// client_k8s_pod_uid or server_k8s_pod_uid, but the human-readable
// client/server label survived.
type ExternalNode struct {
	IDValue     string
	NameValue   string
	LabelsValue map[string]string
}

func (e *ExternalNode) ID() string                          { return e.IDValue }
func (e *ExternalNode) Name() string                        { return e.NameValue }
func (e *ExternalNode) Type() NodeType                      { return NodeTypeExternal }
func (e *ExternalNode) Labels() map[string]string           { return e.LabelsValue }
func (e *ExternalNode) IPAddress() []string                 { return nil }
func (e *ExternalNode) Owner() *Owner                       { return nil }
func (e *ExternalNode) Application() string                 { return "" }
func (e *ExternalNode) Containers() []Container             { return nil }
func (e *ExternalNode) ReadyStatus() string                 { return "" }
func (e *ExternalNode) StorageClass() string                { return "" }
func (e *ExternalNode) StorageClassInfo() *StorageClassInfo { return nil }
func (e *ExternalNode) isGraphNode()                        {}

// StorageClassNode represents a Kubernetes StorageClass entity, sourced from
// kube_storageclass_info. It is cluster-scoped (names are not globally unique).
// InfoValue carries the provisioner + backing-storage parameters when the info
// metric surfaced them, and is nil for a bare node (one referenced by a PVC but
// absent from kube_storageclass_info). LabelsValue is {cluster} only — the
// provisioner/parameters live on the typed StorageClassInfo, never in Labels.
type StorageClassNode struct {
	IDValue     string
	NameValue   string
	LabelsValue map[string]string
	InfoValue   *StorageClassInfo
}

func (s *StorageClassNode) ID() string                          { return s.IDValue }
func (s *StorageClassNode) Name() string                        { return s.NameValue }
func (s *StorageClassNode) Type() NodeType                      { return NodeTypeStorageClass }
func (s *StorageClassNode) Labels() map[string]string           { return s.LabelsValue }
func (s *StorageClassNode) IPAddress() []string                 { return nil }
func (s *StorageClassNode) Owner() *Owner                       { return nil }
func (s *StorageClassNode) Application() string                 { return "" }
func (s *StorageClassNode) Containers() []Container             { return nil }
func (s *StorageClassNode) ReadyStatus() string                 { return "" }
func (s *StorageClassNode) StorageClass() string                { return "" }
func (s *StorageClassNode) StorageClassInfo() *StorageClassInfo { return s.InfoValue }
func (s *StorageClassNode) isGraphNode()                        {}

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

// StorageClassID returns the cluster-scoped StorageClass node ID. The literal
// "storageclass" segment disambiguates it from a PVC/Service ID, since a real
// namespace named "storageclass" is pathological.
func StorageClassID(cluster, name string) string {
	return cluster + "/storageclass/" + name
}

// ExternalID returns the missing-UID-fallback external node ID.
func ExternalID(value string) string { return "external/" + value }

// Reserved: the "cluster/" ID prefix is owned by the Cytoscape presentation
// layer (api.clusterParentID, design.md D31) for synthetic compound group
// nodes. Those are NOT GraphNodes and are never minted here — do not reuse the
// prefix for a real node kind.
