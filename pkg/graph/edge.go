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
	EdgeTypePVCToNetAppAggr   EdgeType = "pvc-to-netapp-aggr"
)

// edgeNamespace is the fixed UUID namespace under which all edge IDs are
// derived (UUIDv5). Bumping this value invalidates every existing edge ID and
// MUST be treated as a v2 break.
var edgeNamespace = uuid.MustParse("4f6a3f9c-9d7e-5d8b-9b14-3a3f0a9e2c11")

// EdgeMetrics holds the RED measurements attached to a trace-derived,
// UID-resolved pod-to-pod edge. Numeric values NEVER enter Labels — same
// precedent as node ipaddress / owner / ready_status.
//
// Absent-vs-zero contract (design D2):
//   - Rate is required and always > 0 when the struct is present (a zero-rate
//     series never produces an edge).
//   - ErrorRate is nil when the failure counter could not be read (query
//     error / metric absent); a non-nil 0 means the counter was read and
//     reported no failures. Consumers MUST NOT treat a missing key as 0.
//   - P90ServerMs is nil when no usable classic histogram was available.
type EdgeMetrics struct {
	Rate        float64  // req/s, always > 0 when the struct is present
	ErrorRate   *float64 // nil = unreadable; 0 = read, no failures
	P90ServerMs *float64 // nil = no usable classic histogram
}

// Edge is the canonical edge value carried over the wire.
type Edge struct {
	ID      string
	Type    EdgeType
	Source  string
	Target  string
	Labels  map[string]string
	Metrics *EdgeMetrics // nil = no RED measurements for this edge
	IO      *IOMetrics   // nil = no I/O measurements for this edge
}

// IOMetrics holds the Harvest storage measurements attached to a
// pvc-to-netapp-aggr edge: six measured I/O figures from the QoS workload
// families plus the volume's declared throughput ceiling, from the QoS
// fixed-policy families (its own QoS policy group's figures — never another
// group's from the same SVM). Numeric values NEVER enter Labels. Each
// field is a pointer so a missing family is omitted (distinct from 0) — for the
// ceiling, absence means "no declared ceiling" and must never surface as a
// number. The RED EdgeMetrics invariant is untouched; a single edge carries at
// most one family (the builder never sets both).
//
// MaxIOPS / MaxBytesPerSec can only be set for an edge that already carries at
// least one measurement. The ceiling is keyed on hop A's (ontap cluster, svm)
// pair, so it does NOT ride on a workload series any more (design.md D9); the
// invariant is upheld structurally instead — the builder attaches the ceiling
// only inside its matched-workload branch (design.md D3 hop C).
// MaxBytesPerSec is the one converted value in the struct — the policy's
// megabytes-per-second figure scaled to bytes per second so it shares the unit
// of ReadBytesPerSec / WriteBytesPerSec.
type IOMetrics struct {
	ReadOps          *float64
	WriteOps         *float64
	ReadLatencyUs    *float64
	WriteLatencyUs   *float64
	ReadBytesPerSec  *float64
	WriteBytesPerSec *float64
	MaxIOPS          *float64
	MaxBytesPerSec   *float64
}

// NewEdge constructs an Edge with a deterministic UUIDv5 id derived from
// (type | source | target). Metrics is always nil; attach via WithMetrics.
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

// WithMetrics returns a shallow copy of e carrying the given metrics.
// ID, Type, Source, Target, and Labels are unchanged — edge identity is
// derived solely from (type|source|target) and metrics never perturb it.
// The original *Edge is left untouched (immutability).
func (e *Edge) WithMetrics(m EdgeMetrics) *Edge {
	if e == nil {
		return nil
	}
	cp := *e
	mm := m
	cp.Metrics = &mm
	return &cp
}

// WithIO returns a shallow copy of e carrying the given I/O measurements.
// Identity fields are unchanged; the original *Edge is left untouched.
func (e *Edge) WithIO(m IOMetrics) *Edge {
	if e == nil {
		return nil
	}
	cp := *e
	mm := m
	cp.IO = &mm
	return &cp
}

// SortEdges orders edges deterministically by ID for stable output.
func SortEdges(edges []*Edge) {
	sort.SliceStable(edges, func(i, j int) bool {
		return edges[i].ID < edges[j].ID
	})
}
