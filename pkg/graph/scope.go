package graph

import (
	"fmt"
	"sort"
)

// Scope describes the projection filter applied at response time, over the
// freshly built graph.
//
// `cluster` and `namespace` appear here AND as upstream selector dimensions
// (promql.Selector): the build narrows the topology at the source, and the
// projection applies the same two filters again as defence in depth — a node
// that reached the graph anyway (an unlabelled series bucketed to
// cluster="unknown", say) must not slip into a filtered view.
type Scope struct {
	Clusters   map[string]struct{}   // empty ⇒ no cluster filter
	Namespaces map[string]struct{}   // empty ⇒ no namespace filter
	EdgeTypes  map[EdgeType]struct{} // empty ⇒ all edge types

	// Inventory turns the default connectivity prune OFF: every pod is emitted
	// with its pod-to-node / pod-mounts-pvc / pvc-to-netapp-aggr chain
	// regardless of traffic, and an infrastructure node is admitted even when
	// nothing in scope references it (bounded by the filters that CAN exclude
	// it by its own labels — see infraNodePassesFilters).
	//
	// It is the INVERSE of the request's `prune` parameter (`prune=false` ⇒
	// Inventory=true) so the zero Scope keeps today's meaning: prune on.
	Inventory bool
}

// NewScope constructs a Scope from raw query parameter values, validating them.
func NewScope(clusters, namespaces, edgeTypes []string, inventory bool) (Scope, error) {
	// Validate edge types against the single in-code registry (EdgeTypes) so a
	// typo like "pod-calls-pods" is an error, not a scope that silently
	// filters every edge out. Living here (not in the HTTP parser) gives D32
	// embedders constructing scopes directly the same guard. Empty values are
	// skipped — edgeTypeSet drops them, keeping a bare `edge_type=` a no-op.
	for _, et := range edgeTypes {
		if et == "" {
			continue
		}
		if !ValidEdgeType(EdgeType(et)) {
			return Scope{}, fmt.Errorf("unknown edge_type %q", et)
		}
	}
	return Scope{
		Clusters:   stringSet(clusters),
		Namespaces: stringSet(namespaces),
		EdgeTypes:  edgeTypeSet(edgeTypes),
		Inventory:  inventory,
	}, nil
}

// edgeTypeAllowed reports whether an edge of type t is permitted by the
// edge-type filter (an empty filter permits every type). filterEdges is its
// only caller; it stays a method so the "empty means all" convention lives
// with the field it governs.
func (s Scope) edgeTypeAllowed(t EdgeType) bool {
	if len(s.EdgeTypes) == 0 {
		return true
	}
	_, ok := s.EdgeTypes[t]
	return ok
}

func stringSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		if v != "" {
			out[v] = struct{}{}
		}
	}
	return out
}

func edgeTypeSet(values []string) map[EdgeType]struct{} {
	if len(values) == 0 {
		return nil
	}
	out := make(map[EdgeType]struct{}, len(values))
	for _, v := range values {
		if v != "" {
			out[EdgeType(v)] = struct{}{}
		}
	}
	return out
}

// SortedKeys returns keys of a map[string]struct{} in deterministic order.
func SortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
