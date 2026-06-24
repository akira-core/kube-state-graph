package graph

import (
	"errors"
	"fmt"
	"sort"
)

// Direction governs traversal direction.
type Direction string

const (
	DirectionIn   Direction = "in"
	DirectionOut  Direction = "out"
	DirectionBoth Direction = "both"
)

// MaxTraversalDepth is the hard upper bound on traversal depth (D7 / spec).
const MaxTraversalDepth = 6

// Scope describes the projection filter applied at response time.
type Scope struct {
	Clusters   map[string]struct{}   // empty ⇒ no cluster filter
	Namespaces map[string]struct{}   // empty ⇒ no namespace filter
	EdgeTypes  map[EdgeType]struct{} // empty ⇒ all edge types
	Names      map[string]struct{}   // empty ⇒ no name filter

	Root      string    // empty ⇒ no traversal
	Depth     int       // 0..MaxTraversalDepth
	Direction Direction // in | out | both
}

// NewScope constructs a Scope from raw query parameter values, validating ranges.
func NewScope(clusters, namespaces, edgeTypes, names []string, root string, depth int, direction string) (Scope, error) {
	if depth < 0 {
		return Scope{}, errors.New("depth must be non-negative")
	}
	if depth > MaxTraversalDepth {
		return Scope{}, errors.New("depth exceeds maximum")
	}
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
	if root != "" {
		if depth == 0 {
			depth = 2
		}
		switch Direction(direction) {
		case "":
			direction = string(DirectionBoth)
		case DirectionIn, DirectionOut, DirectionBoth:
		default:
			return Scope{}, errors.New("invalid direction")
		}
	}
	return Scope{
		Clusters:   stringSet(clusters),
		Namespaces: stringSet(namespaces),
		EdgeTypes:  edgeTypeSet(edgeTypes),
		Names:      stringSet(names),
		Root:       root,
		Depth:      depth,
		Direction:  Direction(direction),
	}, nil
}

// NameFilterActive reports whether the scope restricts to a named node set
// (matched by Name() across every node type).
func (s Scope) NameFilterActive() bool {
	return len(s.Names) > 0
}

// edgeTypeAllowed reports whether an edge of type t is permitted by the
// edge-type filter (an empty filter permits every type). Traversal consults
// this so BFS only crosses in-scope edge types — a node reachable solely via a
// filtered-out edge must not enter the view as an orphan (no incident edge).
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
