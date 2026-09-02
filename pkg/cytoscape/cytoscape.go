// Package cytoscape serialises a built graph.Graph (projected to a graph.View)
// into the deterministic Cytoscape.js response body served at /v1/graph. It is
// part of the reusable graph engine: an embedding application calls Serialise
// to obtain the exact same wire shape kube-state-graph emits, with no HTTP or
// JSON round-trip. The serialiser is presentation-only — it synthesises the
// cluster / namespace / application / controller compound group nodes and the
// data.parent nesting (design.md D6, which supersedes the D31 cluster>node>pod
// grouping) without touching the core graph types. NetApp aggregates nest
// under the real netapp-node (the first real-node compound parent); the
// pvc→aggregate relationship is an edge, not nesting.
package cytoscape

import (
	"cmp"
	"maps"
	"slices"
	"strconv"

	"github.com/akira-core/kube-state-graph/pkg/graph"
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
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	Parent       string            `json:"parent,omitempty"`
	IPAddress    []string          `json:"ipaddress,omitempty"`
	Owner        *graph.Owner      `json:"owner,omitempty"`
	Application  string            `json:"application,omitempty"`
	Containers   []graph.Container `json:"containers,omitempty"`
	ReadyStatus  string            `json:"ready_status,omitempty"`
	Health       string            `json:"health,omitempty"`
	Usage        *UsageDTO         `json:"usage,omitempty"`
	StorageClass string            `json:"storageclass,omitempty"`
	Labels       map[string]string `json:"labels"`
}

// UsageDTO is the wire form of graph.UsageBytes. Either field may be omitted.
type UsageDTO struct {
	UsedBytes     *float64 `json:"used_bytes,omitempty"`
	CapacityBytes *float64 `json:"capacity_bytes,omitempty"`
}

// Edge wraps an edge's data in the Cytoscape `{ "data": {...} }` shape.
type Edge struct {
	Data EdgeData `json:"data"`
}

// EdgeMetricsDTO is the serialised form of graph.EdgeMetrics. Rounding to 6
// significant digits is applied here and only here (design D9) so pkg/graph
// values stay un-rounded and the policy lives in one place.
//
// Absent-vs-zero: ErrorRate is omitted when the failure counter was unreadable;
// a present 0 means "read successfully, no failures". P90ServerMs is omitted
// when no usable classic histogram was available. All three values are JSON
// numbers (never strings) and MAY appear in exponent form for small values.
//
//	@Description	Union of two disjoint measurement families. RED family (trace-derived call edges): rate (required within the family, > 0), error_rate, p90_server_ms. I/O family (pvc-to-netapp-aggr edges): read_ops, write_ops, read_latency_us, write_latency_us, read_bytes_per_sec, write_bytes_per_sec, plus the volume's own QoS policy group's declared ceiling, max_iops and max_bytes_per_sec (absent means no declared ceiling, never 0; neither appears without at least one measurement). A single edge carries fields from exactly one family. All values are JSON numbers rounded to 6 significant digits and may appear in exponent form (e.g. 3.86e-7).
type EdgeMetricsDTO struct {
	// Rate is requests per second over the window (always > 0 when the RED family is present). Schema-optional because the object is a union.
	Rate *float64 `json:"rate,omitempty" example:"5"`
	// ErrorRate is the failed fraction in [0,1]. Omitted when the failure counter was unreadable; 0 means read successfully with no failures.
	ErrorRate *float64 `json:"error_rate,omitempty" example:"0.1"`
	// P90ServerMs is the 90th percentile server-observed request duration in milliseconds.
	P90ServerMs      *float64 `json:"p90_server_ms,omitempty" example:"12.5"`
	ReadOps          *float64 `json:"read_ops,omitempty"`
	WriteOps         *float64 `json:"write_ops,omitempty"`
	ReadLatencyUs    *float64 `json:"read_latency_us,omitempty"`
	WriteLatencyUs   *float64 `json:"write_latency_us,omitempty"`
	ReadBytesPerSec  *float64 `json:"read_bytes_per_sec,omitempty"`
	WriteBytesPerSec *float64 `json:"write_bytes_per_sec,omitempty"`
	// MaxIOPS is the declared ceiling of the volume's own QoS policy group, in requests per second. Absent means no declared ceiling — never 0.
	MaxIOPS *float64 `json:"max_iops,omitempty"`
	// MaxBytesPerSec is the declared throughput ceiling of the volume's own QoS policy group, converted from the policy's MB/s figure so it shares the unit of read_bytes_per_sec / write_bytes_per_sec.
	MaxBytesPerSec *float64 `json:"max_bytes_per_sec,omitempty"`
}

// EdgeData is the serialised form of a graph edge.
type EdgeData struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	Source  string            `json:"source"`
	Target  string            `json:"target"`
	Labels  map[string]string `json:"labels"`
	Metrics *EdgeMetricsDTO   `json:"metrics,omitempty"`
}

// round6 rounds v to 6 significant digits via decimal formatting.
// Do NOT use math.Pow/math.Log10-based decimal-place rounding — it double-rounds,
// is off-by-one at exact powers of ten, and is not bit-identical across platforms
// (design D9 of add-service-graph-red-metrics).
func round6(v float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 6, 64), 64)
	return r
}

// metricsDTO merges at most one family into the wire DTO with rounding
// applied. RED wins if both are set (the builder never sets both). Returns
// nil when neither family is present so the metrics key is wholly absent.
func metricsDTO(m *graph.EdgeMetrics, io *graph.IOMetrics) *EdgeMetricsDTO {
	if m != nil {
		rate := round6(m.Rate)
		dto := &EdgeMetricsDTO{Rate: &rate}
		if m.ErrorRate != nil {
			er := round6(*m.ErrorRate)
			dto.ErrorRate = &er
		}
		if m.P90ServerMs != nil {
			p90 := round6(*m.P90ServerMs)
			dto.P90ServerMs = &p90
		}
		return dto
	}
	if io == nil {
		return nil
	}
	dto := &EdgeMetricsDTO{}
	filled := false
	if io.ReadOps != nil {
		v := round6(*io.ReadOps)
		dto.ReadOps = &v
		filled = true
	}
	if io.WriteOps != nil {
		v := round6(*io.WriteOps)
		dto.WriteOps = &v
		filled = true
	}
	if io.ReadLatencyUs != nil {
		v := round6(*io.ReadLatencyUs)
		dto.ReadLatencyUs = &v
		filled = true
	}
	if io.WriteLatencyUs != nil {
		v := round6(*io.WriteLatencyUs)
		dto.WriteLatencyUs = &v
		filled = true
	}
	if io.ReadBytesPerSec != nil {
		v := round6(*io.ReadBytesPerSec)
		dto.ReadBytesPerSec = &v
		filled = true
	}
	if io.WriteBytesPerSec != nil {
		v := round6(*io.WriteBytesPerSec)
		dto.WriteBytesPerSec = &v
		filled = true
	}
	// The ceilings deliberately do NOT set `filled`: the builder can only
	// attach them alongside a measurement (design.md D3 hop C), so they can
	// never be the sole reason a metrics object exists.
	if io.MaxIOPS != nil {
		v := round6(*io.MaxIOPS)
		dto.MaxIOPS = &v
	}
	if io.MaxBytesPerSec != nil {
		v := round6(*io.MaxBytesPerSec)
		dto.MaxBytesPerSec = &v
	}
	if !filled {
		return nil
	}
	return dto
}

func usageDTO(u *graph.UsageBytes) *UsageDTO {
	if u == nil {
		return nil
	}
	if u.UsedBytes == nil && u.CapacityBytes == nil {
		return nil
	}
	return &UsageDTO{UsedBytes: u.UsedBytes, CapacityBytes: u.CapacityBytes}
}

// Synthetic compound group-node types. These exist only in the Cytoscape
// presentation (to satisfy data.parent references); they are not
// graph.GraphNodes. See design.md D6.
const (
	nodeTypeCluster        = "cluster"
	nodeTypeStorageCluster = "storage-cluster"
	nodeTypeNamespace      = "namespace"
	nodeTypeApplication    = "application"
	nodeTypeController     = "controller"
)

// Synthetic group-node id constructors. Each id encodes its full ancestry path
// so the compound tree is unambiguous by construction (a node has exactly one
// parent) and no data.parent can dangle.
func clusterParentID(cluster string) string { return "cluster/" + cluster }

func storageClusterParentID(oc string) string { return "storage-cluster/" + oc }

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
	storageClusterSeen := map[string]struct{}{}
	nsSeen := map[nsKey]struct{}{}
	appSeen := map[appKey]struct{}{}
	ctrlSeen := map[ctrlKey]struct{}{}
	for _, n := range view.Nodes {
		labels := n.Labels()
		c := labels["cluster"]
		if c != "" {
			clusterSeen[c] = struct{}{}
		}
		if oc := labels["ontap_cluster"]; oc != "" {
			storageClusterSeen[oc] = struct{}{}
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
			ns := labels["namespace"]
			if c == "" || ns == "" {
				continue // cluster-less / namespace-less service/pvc: falls back to cluster group
			}
			nsSeen[nsKey{c, ns}] = struct{}{}
			if app := n.Application(); app != "" {
				appSeen[appKey{c, ns, app}] = struct{}{}
			}
		default:
			// node / netapp / external: only cluster / storage-cluster groups
			// (collected above) apply; no workload group is derived from them.
		}
	}

	// The top-level `clusters` describes the RESPONSE: it lists the clusters
	// present in the projected view (including cross-cluster partners re-added by
	// projection), not every cluster in upstream VictoriaMetrics.
	sortedClusters := slices.Sorted(maps.Keys(clusterSeen))
	body.Clusters = append(make([]string, 0, len(sortedClusters)), sortedClusters...)

	body.Elements.Nodes = make([]Node, 0,
		len(view.Nodes)+len(clusterSeen)+len(storageClusterSeen)+len(nsSeen)+len(appSeen)+len(ctrlSeen))

	// Synthetic group nodes in tier order (cluster, storage-cluster, namespace,
	// application, controller), each tier sorted by id, before the real nodes.
	for _, c := range sortedClusters {
		body.Elements.Nodes = append(body.Elements.Nodes, groupNode(
			clusterParentID(c), c, nodeTypeCluster, ""))
	}
	sortedOC := slices.Sorted(maps.Keys(storageClusterSeen))
	for _, oc := range sortedOC {
		body.Elements.Nodes = append(body.Elements.Nodes, groupNode(
			storageClusterParentID(oc), oc, nodeTypeStorageCluster, ""))
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
		body.Elements.Nodes = append(body.Elements.Nodes, Node{
			Data: NodeData{
				ID:           n.ID(),
				Name:         n.Name(),
				Type:         string(n.Type()),
				Parent:       compoundParent(n),
				IPAddress:    n.IPAddress(),
				Owner:        n.Owner(),
				Application:  n.Application(),
				Containers:   n.Containers(),
				ReadyStatus:  n.ReadyStatus(),
				Health:       n.Health(),
				Usage:        usageDTO(n.Usage()),
				StorageClass: n.StorageClass(),
				Labels:       n.Labels(),
			},
		})
	}

	body.Elements.Edges = make([]Edge, 0, len(view.Edges))
	for _, e := range view.Edges {
		body.Elements.Edges = append(body.Elements.Edges, Edge{
			Data: EdgeData{
				ID:      e.ID,
				Type:    string(e.Type),
				Source:  e.Source,
				Target:  e.Target,
				Labels:  e.Labels,
				Metrics: metricsDTO(e.Metrics, e.IO),
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
//	service, pvc → its application group when it has a resolved Application,
//	               else its namespace group (skip-absent-levels:
//	               cluster > namespace > [application >] {service, pvc}); a
//	               cluster-less or namespace-less service/pvc falls back to its
//	               cluster group, else ""
//	node         → its cluster group
//	netapp-node  → storage-cluster/<ontap_cluster>
//	netapp-aggr  → the real netapp-node id netapp/<oc>/<labels.node>,
//	               falling back to storage-cluster/<oc> when no owner resolved
//	external     → "" (no cluster identity)
//
// The pod→node and pvc→netapp-aggr relationships are edges, not nesting.
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
			if app := n.Application(); app != "" {
				return applicationParentID(c, ns, app)
			}
			return namespaceParentID(c, ns)
		}
	case graph.NodeTypeNetAppNode:
		if oc := labels["ontap_cluster"]; oc != "" {
			return storageClusterParentID(oc)
		}
		return ""
	case graph.NodeTypeNetAppAggr:
		oc, node := labels["ontap_cluster"], labels["node"]
		if oc == "" {
			return ""
		}
		if node != "" {
			return graph.NetAppNodeID(oc, node)
		}
		// Owner-less aggregate (the Harvest volume series carried no `node`
		// label), so no controller node exists to parent it. Nest it directly
		// under its storage-cluster group rather than leaving it orphaned at
		// top level next to a group that was synthesised for it anyway.
		return storageClusterParentID(oc)
	default:
		// node / external: cluster-group fallback below.
	}
	if c != "" {
		return clusterParentID(c)
	}
	return ""
}
