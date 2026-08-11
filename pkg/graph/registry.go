package graph

// EdgeTypeLabel describes a single label key an edge of this type may emit.
type EdgeTypeLabel struct {
	Name        string `json:"name"`
	ValueType   string `json:"value_type"`
	Description string `json:"description,omitempty"`
}

// EdgeTypeDefinition is one entry in the static catalogue served by
// GET /v1/edge-types.
type EdgeTypeDefinition struct {
	Type            EdgeType        `json:"type"`
	Description     string          `json:"description"`
	SourceType      []NodeType      `json:"source_type"`
	TargetType      []NodeType      `json:"target_type"`
	Directed        bool            `json:"directed"`
	MayCrossCluster bool            `json:"may_cross_cluster"`
	Labels          []EdgeTypeLabel `json:"labels"`
}

// validEdgeTypes is the lookup set derived from EdgeTypes at init. Because it
// is built from the registry itself, it can never drift from what the builder
// produces and /v1/edge-types advertises.
var validEdgeTypes = func() map[EdgeType]struct{} {
	out := make(map[EdgeType]struct{}, len(EdgeTypes))
	for _, def := range EdgeTypes {
		out[def.Type] = struct{}{}
	}
	return out
}()

// ValidEdgeType reports whether t is a registered edge type — i.e. present in
// the EdgeTypes registry served by /v1/edge-types. Request parsers use it to
// reject unknown ?edge_type= filter values instead of silently matching no
// edges.
func ValidEdgeType(t EdgeType) bool {
	_, ok := validEdgeTypes[t]
	return ok
}

// EdgeTypes is the in-code registry consumed by both the graph builder and
// the /v1/edge-types HTTP handler.
var EdgeTypes = []EdgeTypeDefinition{
	{
		Type:            EdgeTypePodMountsPVC,
		Description:     "Pod mounts a PVC bound on the pod's host node. Always intra-cluster.",
		SourceType:      []NodeType{NodeTypePod},
		TargetType:      []NodeType{NodeTypePVC},
		Directed:        true,
		MayCrossCluster: false,
		Labels: []EdgeTypeLabel{
			{Name: "claim_name", ValueType: "string"},
		},
	},
	{
		Type:            EdgeTypePodCallsPod,
		Description:     "Pod-UID-resolved RPC edge from service-graph metrics. May cross clusters when the resolved source and target pods live in different clusters (recovered from the topology pod-UID index since the metric only carries the trace-source cluster). An endpoint that resolves to a Kubernetes Service produces a 'pod-calls-service' edge instead (see that type for the paths that do so). An endpoint whose resolution finds no such Service — a '://' connection string the caller's own cluster does not hold, an unknown-server peer address no lookup matched, or a missing pod UID with a non-URL label (D27) — falls back to an 'external' node.",
		SourceType:      []NodeType{NodeTypePod, NodeTypeService, NodeTypeExternal},
		TargetType:      []NodeType{NodeTypePod, NodeTypeExternal},
		Directed:        true,
		MayCrossCluster: true,
		Labels: []EdgeTypeLabel{
			{Name: "cluster", ValueType: "string"},
			{Name: "relation", ValueType: "string", Description: "Span-link relation marker: 'link' for a logical producer→consumer edge derived from cross-trace span links through a broker, 'transport' for a pod→broker network hop backing such a link. Absent on ordinary edges."},
		},
	},
	{
		Type:            EdgeTypePodCallsService,
		Description:     "Service-graph call edge whose target resolves to an in-cluster Kubernetes Service node. Four sources. (1) A '://' connection string (D29): the addressed (namespace, service) resolves to a SINGLE Service node in the caller's OWN (anchor) cluster — the UID-recovered client-pod cluster when available, else the trace-source label — and ONLY when that cluster holds the same-named Service object (a family sibling holding it is not enough; otherwise the endpoint falls back to 'external'). This path is always intra-cluster. (2) Unknown-server peer-label enrichment: a server='unknown' endpoint whose client resolved to a real pod is classified from its peer address — in-cluster DNS name, bare short Service name resolved in the client pod's namespace, or a ClusterIP literal looked up in the client's own cluster — and resolves under the same anchor-cluster rule as (1). (3) The Istio route-resolution engine (translate-global-fqdn-to-k8s-service): a global/ingress FQDN peer resolves to the Service the selected ingress cluster's Gateway + VirtualService config routed it to. The ingress cluster is picked from the destination IPs (family-first, caller tie-break) and may be a family sibling of the caller's, so this path MAY cross clusters. (4) The ingress chain of such a routed hit: the caller also gets an edge to the ingress entry-point Service (whose node carries labels.role), and each of that cluster's ingress gateway pods gets a synthesized edge to the routed backend Service. Per-edge cross-cluster status is derived by comparing the resolved endpoints' labels.cluster (D9). The resolved Service fans out service-selects-pod edges that MAY cross clusters (see that type). Carries labels.cluster when the client side is a pod (D9).",
		SourceType:      []NodeType{NodeTypePod, NodeTypeService, NodeTypeExternal},
		TargetType:      []NodeType{NodeTypeService},
		Directed:        true,
		MayCrossCluster: true,
		Labels: []EdgeTypeLabel{
			{Name: "cluster", ValueType: "string"},
			{Name: "relation", ValueType: "string", Description: "Span-link relation marker: 'link' for a logical producer→consumer edge derived from cross-trace span links through a broker, 'transport' for a pod→broker network hop backing such a link. Absent on ordinary edges."},
		},
	},
	{
		Type:            EdgeTypeServiceSelectsPod,
		Description:     "A Kubernetes Service routes to a backing pod, derived from kube_endpointslice_endpoints joined to topology pods (D29). Materialised on demand, only for Service nodes some endpoint actually resolved to — a '://' connection string, an unknown-server peer address (DNS name, bare short name, or ClusterIP literal), or an Istio route-engine resolution. The fan-out has two forms. (1) Family-wide (the default): one edge per backing pod across EVERY same-family cluster that holds the same-named Service object — the union of each such cluster's endpoints — reflecting service-mesh endpoint aggregation. (2) Locked-cluster: for an ingress entry-point Service (a routed hit's gateway Service, or the ingress LB fallback destination) the fan-out uses ONLY the selected ingress cluster's own endpoints, because an ingress address is a per-cluster address and a family sibling's same-named Service is not behind it. Either form MAY cross clusters — a Service node selecting a backing pod that runs in another cluster — so cross-cluster status is derived by comparing the source service node's and target pod node's labels.cluster.",
		SourceType:      []NodeType{NodeTypeService},
		TargetType:      []NodeType{NodeTypePod},
		Directed:        true,
		MayCrossCluster: true,
		Labels: []EdgeTypeLabel{
			{Name: "namespace", ValueType: "string", Description: "Namespace of the service and its backing pod (optional)."},
		},
	},
	{
		Type:            EdgeTypePodToNode,
		Description:     "Pod is scheduled on a Kubernetes node, derived from the pod's `node` label (kube_pod_info). Emitted for every scheduled pod. Always intra-cluster (the node is in the pod's own cluster).",
		SourceType:      []NodeType{NodeTypePod},
		TargetType:      []NodeType{NodeTypeK8sNode},
		Directed:        true,
		MayCrossCluster: false,
		Labels:          nil,
	},
	{
		Type:            EdgeTypePVCToStorageClass,
		Description:     "PVC is provisioned by a StorageClass, derived from the PVC's resolved `storageclass` (kube_persistentvolumeclaim_info) joined to the StorageClass node (kube_storageclass_info, bare-synthesised when absent). Emitted for every PVC with a resolved StorageClass. Always intra-cluster.",
		SourceType:      []NodeType{NodeTypePVC},
		TargetType:      []NodeType{NodeTypeStorageClass},
		Directed:        true,
		MayCrossCluster: false,
		Labels:          nil,
	},
}
