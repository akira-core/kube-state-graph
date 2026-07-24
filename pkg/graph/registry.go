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
		Description:     "Pod-UID-resolved RPC edge from service-graph metrics. May cross clusters when the resolved source and target pods live in different clusters (recovered from the topology pod-UID index since the metric only carries the trace-source cluster). An endpoint whose client/server label is a '://' connection string resolving to a Kubernetes Service in the caller's OWN cluster produces a 'pod-calls-service' edge instead (see that type); a '://' string whose service resolution finds no such service in the caller's own cluster falls back to an 'external' node, and endpoints with a missing pod UID and a non-URL label become 'external' nodes via the human-label fallback (D27).",
		SourceType:      []NodeType{NodeTypePod, NodeTypeService, NodeTypeExternal},
		TargetType:      []NodeType{NodeTypePod, NodeTypeExternal},
		Directed:        true,
		MayCrossCluster: true,
		Labels: []EdgeTypeLabel{
			{Name: "cluster", ValueType: "string"},
		},
	},
	{
		Type:            EdgeTypePodCallsService,
		Description:     "Service-graph call edge whose target resolves to an in-cluster Kubernetes Service node. Two sources. (1) A '://' connection string (D29): the addressed (namespace, service) resolves to a SINGLE Service node in the caller's OWN (anchor) cluster — the UID-recovered client-pod cluster when available, else the trace-source label — and ONLY when that cluster holds the same-named Service object (a family sibling holding it is not enough; otherwise the endpoint falls back to 'external'). There is no per-family service-node fan-out and no cross-family fallback; this path is always intra-cluster. (2) The Istio route-resolution engine (translate-global-fqdn-to-k8s-service): a global/ingress FQDN peer resolves to the Service the selected ingress cluster's Gateway + VirtualService config routed it to — the ingress cluster is picked from the destination IPs (family-first, caller tie-break) and may be a family sibling of the caller's, so a route-engine-sourced pod-calls-service edge MAY cross clusters. Per-edge cross-cluster status is derived by comparing the resolved endpoints' labels.cluster (D9). The resolved Service fans out service-selects-pod edges that MAY cross clusters (see that type). Carries labels.cluster when the client side is a pod (D9).",
		SourceType:      []NodeType{NodeTypePod, NodeTypeService, NodeTypeExternal},
		TargetType:      []NodeType{NodeTypeService},
		Directed:        true,
		MayCrossCluster: true,
		Labels: []EdgeTypeLabel{
			{Name: "cluster", ValueType: "string"},
		},
	},
	{
		Type:            EdgeTypeServiceSelectsPod,
		Description:     "A Kubernetes Service routes to a backing pod, derived from kube_endpointslice_endpoints joined to topology pods (D29). Materialised on demand only for the single local Service node a '://' connection-string endpoint resolves to (in the caller's own cluster). The Service node fans out one edge per backing pod across EVERY same-family cluster that holds the same-named Service object — the union of each such cluster's endpoints — so the edge MAY cross clusters (a local Service node selecting a backing pod that runs in a family-sibling cluster, reflecting service-mesh endpoint aggregation). Cross-cluster status is derived by comparing the source service node's and target pod node's labels.cluster.",
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
