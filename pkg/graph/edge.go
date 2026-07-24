package graph

import (
	"sort"

	"github.com/google/uuid"
)

// EdgeType identifies the semantic relationship between two nodes.
type EdgeType string

const (
	EdgeTypePodMountsPVC      EdgeType = "pod-mounts-pvc"
	EdgeTypePodCallsPod       EdgeType = "pod-calls-pod"
	EdgeTypePodCallsService   EdgeType = "pod-calls-service"
	EdgeTypeServiceSelectsPod EdgeType = "service-selects-pod"
	EdgeTypePodToNode         EdgeType = "pod-to-node"
	EdgeTypePVCToStorageClass EdgeType = "pvc-to-storageclass"
)

// edgeNamespace is the fixed UUID namespace under which all edge IDs are
// derived (UUIDv5). Bumping this value invalidates every existing edge ID and
// MUST be treated as a v2 break.
var edgeNamespace = uuid.MustParse("4f6a3f9c-9d7e-5d8b-9b14-3a3f0a9e2c11")

// Edge is the canonical edge value carried over the wire.
type Edge struct {
	ID     string
	Type   EdgeType
	Source string
	Target string
	Labels map[string]string
}

// NewEdge constructs an Edge with a deterministic UUIDv5 id derived from
// (type | source | target).
func NewEdge(t EdgeType, source, target string, labels map[string]string) *Edge {
	if labels == nil {
		labels = map[string]string{}
	}
	id := uuid.NewSHA1(edgeNamespace, []byte(string(t)+"|"+source+"|"+target))
	return &Edge{
		ID:     id.String(),
		Type:   t,
		Source: source,
		Target: target,
		Labels: labels,
	}
}

// SortEdges orders edges deterministically by ID for stable output.
func SortEdges(edges []*Edge) {
	sort.SliceStable(edges, func(i, j int) bool {
		return edges[i].ID < edges[j].ID
	})
}
