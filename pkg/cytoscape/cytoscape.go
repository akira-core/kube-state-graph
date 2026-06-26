// Package cytoscape serialises a built graph.Graph (projected to a graph.View)
// into the deterministic Cytoscape.js response body served at /v1/graph. It is
// part of the reusable graph engine: an embedding application calls Serialise
// to obtain the exact same wire shape kube-state-graph emits, with no HTTP or
// JSON round-trip. The serialiser is presentation-only — it synthesises the
// cluster / namespace / application / controller compound group nodes and the
// data.parent nesting (design.md D6, which supersedes the D31 cluster>node>pod
// grouping) without touching the core graph types. StorageClass is a real
// graph node (not synthesised), and the pod→node and pvc→storageclass
// relationships are edges, not nesting.
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

// NodeData is the serialised form of a graph node (plus the synthetic cluster /
// namespace / application / controller group nodes and the presentation-only
// parent field).
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
	Provisioner string            `json:"provisioner,omitempty"`
	Parameters  map[string]string `json:"parameters,omitempty"`
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

// Synthetic compound group-node types. These exist only in the Cytoscape
// presentation (to satisfy data.parent references); they are not
// graph.GraphNodes. See design.md D6.
const (
	nodeTypeCluster     = "cluster"
	nodeTypeNamespace   = "namespace"
	nodeTypeApplication = "application"
	nodeTypeController  = "controller"
)

// Synthetic group-node id constructors. Each id encodes its full ancestry path
// so the compound tree is unambiguous by construction (a node has exactly one
// parent) and no data.parent can dangle.
func clusterParentID(cluster string) string { return "cluster/" + cluster }

func namespaceParentID(cluster, ns string) string {
	return cluster + "/namespace/" + ns
}

func applicationParentID(cluster, ns, app string) string {
	return namespaceParentID(cluster, ns) + "/application/" + app
}

// controllerParentID nests under the application group when the pod has a
// resolved Application, else directly under the namespace group.
func controllerParentID(cluster, ns, app, kind, name string) string {
	base := namespaceParentID(cluster, ns)
	if app != "" {
		base = applicationParentID(cluster, ns, app)
	}
	return base + "/controller/" + kind + "/" + name
}

type nsKey struct{ cluster, ns string }
type appKey struct{ cluster, ns, app string }
type ctrlKey struct{ cluster, ns, app, kind, name string }

// Serialise renders a projected view into the deterministic Cytoscape body.
// The view supplies the in-scope nodes and edges; the response `clusters` field
// is derived from the clusters actually present in that view (see below).
//
//nolint:unparam // g is retained for the stable reusable-engine signature (D32); the response clusters now derive from the projected view, not the full graph.
func Serialise(g *graph.Graph, view graph.View) Body {
	body := Body{
		APIVersion: APIVersion,
	}

	// Collect the synthetic compound groups to synthesise, derived per node from
	// its own attributes so each pod independently produces its full parent
	// chain — the path-encoded ids then make the tree dangling-free. cluster
	// groups come from every node's cluster; namespace groups from any
	// pod/service/pvc; application/controller groups from pods only.
	clusterSeen := map[string]struct{}{}
	nsSeen := map[nsKey]struct{}{}
	appSeen := map[appKey]struct{}{}
	ctrlSeen := map[ctrlKey]struct{}{}
	for _, n := range view.Nodes {
		labels := n.Labels()
		c := labels["cluster"]
		if c != "" {
			clusterSeen[c] = struct{}{}
		}
		switch n.Type() {
		case graph.NodeTypePod:
			ns := labels["namespace"]
			if c == "" || ns == "" {
				continue // synthesised/cluster-less pod: falls back to cluster group
			}
			nsSeen[nsKey{c, ns}] = struct{}{}
			app := n.Application()
			if app != "" {
				appSeen[appKey{c, ns, app}] = struct{}{}
			}
			if o := n.Owner(); o != nil {
				ctrlSeen[ctrlKey{c, ns, app, o.Kind, o.Name}] = struct{}{}
			}
		case graph.NodeTypeService, graph.NodeTypePVC:
			if ns := labels["namespace"]; c != "" && ns != "" {
				nsSeen[nsKey{c, ns}] = struct{}{}
			}
		default:
			// node / storageclass / external: only the cluster group (collected
			// above) applies; no workload group is derived from them.
		}
	}

	// The top-level `clusters` describes the RESPONSE: it lists the clusters
	// present in the projected view (including cross-cluster partners re-added by
	// projection), not every cluster in upstream VictoriaMetrics.
	sortedClusters := slices.Sorted(maps.Keys(clusterSeen))
	body.Clusters = append(make([]string, 0, len(sortedClusters)), sortedClusters...)

	body.Elements.Nodes = make([]Node, 0,
		len(view.Nodes)+len(clusterSeen)+len(nsSeen)+len(appSeen)+len(ctrlSeen))

	// Synthetic group nodes in tier order (cluster, namespace, application,
	// controller), each tier sorted by id, before the real nodes (determinism, D6).
	for _, c := range sortedClusters {
		body.Elements.Nodes = append(body.Elements.Nodes, groupNode(
			clusterParentID(c), c, nodeTypeCluster, ""))
	}
	nsKeys := slices.SortedFunc(maps.Keys(nsSeen), func(a, b nsKey) int {
		return cmp.Compare(namespaceParentID(a.cluster, a.ns), namespaceParentID(b.cluster, b.ns))
	})
	for _, k := range nsKeys {
		body.Elements.Nodes = append(body.Elements.Nodes, groupNode(
			namespaceParentID(k.cluster, k.ns), k.ns, nodeTypeNamespace, clusterParentID(k.cluster)))
	}
	appKeys := slices.SortedFunc(maps.Keys(appSeen), func(a, b appKey) int {
		return cmp.Compare(applicationParentID(a.cluster, a.ns, a.app), applicationParentID(b.cluster, b.ns, b.app))
	})
	for _, k := range appKeys {
		body.Elements.Nodes = append(body.Elements.Nodes, groupNode(
			applicationParentID(k.cluster, k.ns, k.app), k.app, nodeTypeApplication, namespaceParentID(k.cluster, k.ns)))
	}
	ctrlKeys := slices.SortedFunc(maps.Keys(ctrlSeen), func(a, b ctrlKey) int {
		return cmp.Compare(
			controllerParentID(a.cluster, a.ns, a.app, a.kind, a.name),
			controllerParentID(b.cluster, b.ns, b.app, b.kind, b.name))
	})
	for _, k := range ctrlKeys {
		parent := namespaceParentID(k.cluster, k.ns)
		if k.app != "" {
			parent = applicationParentID(k.cluster, k.ns, k.app)
		}
		body.Elements.Nodes = append(body.Elements.Nodes, groupNode(
			controllerParentID(k.cluster, k.ns, k.app, k.kind, k.name), k.name, nodeTypeController, parent))
	}

	for _, n := range view.Nodes {
		var provisioner string
		var parameters map[string]string
		if info := n.StorageClassInfo(); info != nil {
			provisioner = info.Provisioner
			parameters = info.Parameters
		}
		body.Elements.Nodes = append(body.Elements.Nodes, Node{
			Data: NodeData{
				ID:          n.ID(),
				Name:        n.Name(),
				Type:        string(n.Type()),
				Parent:      compoundParent(n),
				IPAddress:   n.IPAddress(),
				Owner:       n.Owner(),
				Application: n.Application(),
				Containers:  n.Containers(),
				ReadyStatus: n.ReadyStatus(),
				Provisioner: provisioner,
				Parameters:  parameters,
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

// groupNode builds a synthetic compound group node DTO (labels {}, no ipaddress).
func groupNode(id, name, typ, parent string) Node {
	return Node{Data: NodeData{
		ID:     id,
		Name:   name,
		Type:   typ,
		Parent: parent,
		Labels: map[string]string{},
	}}
}

// compoundParent returns the Cytoscape data.parent for a real node, per the
// design D6 workload hierarchy:
//
//	pod          → its controller group when it has a resolved owner,
//	               else its application group when it has a resolved Application,
//	               else its namespace group (skip-absent-levels); a cluster-less
//	               or namespace-less pod falls back to its cluster group, else ""
//	service, pvc → its namespace group (cluster > namespace > {service, pvc})
//	node, sc     → its cluster group (cluster > {node, storageclass})
//	external     → "" (no cluster identity)
//
// The pod→node and pvc→storageclass relationships are edges, not nesting.
func compoundParent(n graph.GraphNode) string {
	labels := n.Labels()
	c := labels["cluster"]
	switch n.Type() {
	case graph.NodeTypePod:
		if ns := labels["namespace"]; c != "" && ns != "" {
			app := n.Application()
			if o := n.Owner(); o != nil {
				return controllerParentID(c, ns, app, o.Kind, o.Name)
			}
			if app != "" {
				return applicationParentID(c, ns, app)
			}
			return namespaceParentID(c, ns)
		}
	case graph.NodeTypeService, graph.NodeTypePVC:
		if ns := labels["namespace"]; c != "" && ns != "" {
			return namespaceParentID(c, ns)
		}
	default:
		// node / storageclass / external: cluster-group fallback below.
	}
	if c != "" {
		return clusterParentID(c)
	}
	return ""
}
