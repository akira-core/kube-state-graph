// Package cytoscape serialises a built graph.Graph (projected to a graph.View)
// into the deterministic Cytoscape.js response body served at /v1/graph. It is
// part of the reusable graph engine: an embedding application calls Serialise
// to obtain the exact same wire shape kube-state-graph emits, with no HTTP or
// JSON round-trip. The serialiser is presentation-only — it adds synthetic
// cluster group nodes and data.parent compound nesting (design.md D31) without
// touching the core graph types.
package cytoscape

import (
	"cmp"
	"maps"
	"slices"

	"github.com/marz32one/kube-state-graph/pkg/graph"
)

// APIVersion is the value stamped on the body's apiVersion field (design.md D14).
const APIVersion = "v1"

// ----- Cytoscape.js shape ---------------------------------------------------

// Body is the top-level /v1/graph response envelope.
type Body struct {
	APIVersion string   `json:"apiVersion"`
	Clusters   []string `json:"clusters"`
	Elements   Elements `json:"elements"`
}

// Elements holds the node and edge collections.
type Elements struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Node wraps a node's data in the Cytoscape `{ "data": {...} }` shape.
type Node struct {
	Data NodeData `json:"data"`
}

// NodeData is the serialised form of a graph node (plus synthetic cluster
// group nodes and the presentation-only parent field).
type NodeData struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Parent      string            `json:"parent,omitempty"`
	IPAddress   []string          `json:"ipaddress,omitempty"`
	Owner       *graph.Owner      `json:"owner,omitempty"`
	Application string            `json:"application,omitempty"`
	Containers  []graph.Container `json:"containers,omitempty"`
	ReadyStatus string            `json:"ready_status,omitempty"`
	Labels      map[string]string `json:"labels"`
}

// Edge wraps an edge's data in the Cytoscape `{ "data": {...} }` shape.
type Edge struct {
	Data EdgeData `json:"data"`
}

// EdgeData is the serialised form of a graph edge.
type EdgeData struct {
	ID     string            `json:"id"`
	Type   string            `json:"type"`
	Source string            `json:"source"`
	Target string            `json:"target"`
	Labels map[string]string `json:"labels"`
}

// nodeTypeCluster is the synthetic node type used for Cytoscape compound
// grouping. Cluster group nodes exist only in the Cytoscape presentation (to
// satisfy data.parent references); they are not graph.GraphNodes. See
// design.md D31.
const nodeTypeCluster = "cluster"

// nodeTypeStorageClass is the synthetic node type for the StorageClass compound
// group. Like cluster groups these exist only in the Cytoscape presentation (to
// satisfy a PVC's data.parent); they are not graph.GraphNodes. See design.md.
const nodeTypeStorageClass = "storageclass"

// clusterParentID is the synthetic group-node id for a cluster.
func clusterParentID(cluster string) string { return "cluster/" + cluster }

// storageClassParentID is the synthetic group-node id for a (cluster,
// StorageClass) pair. StorageClass names are DNS-1123 subdomains (no "/"), so
// the id is unambiguous against namespace-scoped PVC ids.
func storageClassParentID(cluster, sc string) string {
	return cluster + "/storageclass/" + sc
}

// scKey identifies a synthesised StorageClass group by its (cluster,
// StorageClass) pair.
type scKey struct{ cluster, sc string }

// Serialise renders a projected view into the deterministic Cytoscape body.
// The view supplies the in-scope nodes and edges; the response `clusters` field
// is derived from the clusters actually present in that view (see below).
//
//nolint:unparam // g is retained for the stable reusable-engine signature (D32); the response clusters now derive from the projected view, not the full graph.
func Serialise(g *graph.Graph, view graph.View) Body {
	body := Body{
		APIVersion: APIVersion,
	}

	// Index emitted node ids so a pod's parent (its K8s node) is referenced
	// only when that node is actually present in elements.nodes — a data.parent
	// pointing at an absent node would dangle in Cytoscape. Collect the
	// distinct clusters to synthesise group nodes for.
	present := make(map[string]struct{}, len(view.Nodes))
	clusterSeen := map[string]struct{}{}
	// StorageClass compound groups: one synthetic group node per (cluster,
	// StorageClass) pair carried by an emitted PVC with a resolved StorageClass.
	// Derived from the same emitted node set as the cluster groups, so a PVC's
	// data.parent can never dangle.
	scSeen := map[scKey]struct{}{}
	for _, n := range view.Nodes {
		present[n.ID()] = struct{}{}
		c := n.Labels()["cluster"]
		if c != "" {
			clusterSeen[c] = struct{}{}
		}
		if n.Type() == graph.NodeTypePVC {
			if sc := n.StorageClass(); sc != "" && c != "" {
				scSeen[scKey{c, sc}] = struct{}{}
			}
		}
	}

	// The top-level `clusters` describes the RESPONSE: it lists the clusters
	// present in the projected view (including cross-cluster partners re-added by
	// projection), not every cluster in upstream VictoriaMetrics. Under a
	// `?cluster=` / `?name=` filter this keeps `clusters` consistent with
	// `elements` instead of advertising clusters with zero nodes in the body.
	sortedClusters := slices.Sorted(maps.Keys(clusterSeen))
	body.Clusters = append(make([]string, 0, len(sortedClusters)), sortedClusters...)

	body.Elements.Nodes = make([]Node, 0, len(view.Nodes)+len(clusterSeen)+len(scSeen))

	// Synthetic cluster group nodes first, sorted by name (determinism, D6).
	for _, c := range sortedClusters {
		body.Elements.Nodes = append(body.Elements.Nodes, Node{
			Data: NodeData{
				ID:     clusterParentID(c),
				Name:   c,
				Type:   nodeTypeCluster,
				Labels: map[string]string{},
			},
		})
	}

	// StorageClass group nodes next, after the cluster groups and before the
	// real nodes, ordered by (cluster, storageclass) (determinism). Each nests
	// under its cluster group and carries no labels — its cluster identity lives
	// in its id and parent.
	sortedSC := slices.SortedFunc(maps.Keys(scSeen), func(a, b scKey) int {
		return cmp.Or(cmp.Compare(a.cluster, b.cluster), cmp.Compare(a.sc, b.sc))
	})
	for _, k := range sortedSC {
		body.Elements.Nodes = append(body.Elements.Nodes, Node{
			Data: NodeData{
				ID:     storageClassParentID(k.cluster, k.sc),
				Name:   k.sc,
				Type:   nodeTypeStorageClass,
				Parent: clusterParentID(k.cluster),
				Labels: map[string]string{},
			},
		})
	}

	for _, n := range view.Nodes {
		body.Elements.Nodes = append(body.Elements.Nodes, Node{
			Data: NodeData{
				ID:          n.ID(),
				Name:        n.Name(),
				Type:        string(n.Type()),
				Parent:      compoundParent(n, present),
				IPAddress:   n.IPAddress(),
				Owner:       n.Owner(),
				Application: n.Application(),
				Containers:  n.Containers(),
				ReadyStatus: n.ReadyStatus(),
				Labels:      n.Labels(),
			},
		})
	}

	body.Elements.Edges = make([]Edge, 0, len(view.Edges))
	for _, e := range view.Edges {
		body.Elements.Edges = append(body.Elements.Edges, Edge{
			Data: EdgeData{
				ID:     e.ID,
				Type:   string(e.Type),
				Source: e.Source,
				Target: e.Target,
				Labels: e.Labels,
			},
		})
	}
	return body
}

// compoundParent returns the Cytoscape data.parent for a node, per design D31
// and the StorageClass grouping design:
//
//	pod          → its K8s node (labels.node) when present in the view,
//	               else its cluster group, else "" (unknown cluster)
//	pvc          → its StorageClass group (<cluster>/storageclass/<sc>) when it
//	               has a resolved StorageClass, else its cluster group
//	node/service → its cluster group (cluster/<cluster>)
//	external     → "" (no cluster identity)
func compoundParent(n graph.GraphNode, present map[string]struct{}) string {
	labels := n.Labels()
	switch n.Type() {
	case graph.NodeTypePod:
		// A pod nests under its scheduling K8s node (labels.node) when that node
		// is present in the view; otherwise it falls through to its cluster group.
		if node := labels["node"]; node != "" {
			if _, ok := present[node]; ok {
				return node
			}
		}
	case graph.NodeTypePVC:
		// A PVC with a resolved StorageClass nests under its StorageClass group
		// (cluster > storageclass > pvc); otherwise it falls through to its
		// cluster group (cluster > pvc). The group node is synthesised from the
		// same emitted PVC set, so it is guaranteed present — mirroring the
		// cluster-group invariant.
		if sc := n.StorageClass(); sc != "" {
			if c := labels["cluster"]; c != "" {
				return storageClassParentID(c, sc)
			}
		}
	default:
		// node / service / external: fall through to the cluster-group fallback
		// below (external carries no cluster label, so it gets no parent).
	}
	// node / service — and any pod whose node is out of scope, or any PVC with no
	// StorageClass — nest under their cluster group. external nodes carry no
	// cluster label (labels={}), so they fall through to no parent.
	if c := labels["cluster"]; c != "" {
		return clusterParentID(c)
	}
	return ""
}
