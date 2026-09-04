package graph

import (
	"fmt"
	"sort"
	"strings"
)

// PodRef names one pod by (namespace, name) rather than by UID. It is the
// workload-root key of the storage-flow projection: an operator searching for
// "the storage under shop/orders-0" knows the pod's name, never its UID, and
// pod names are unique per namespace within a cluster. When the selected
// estate holds several clusters the name may resolve in more than one; the
// optional `cluster` filter narrows that, exactly as it does for every other
// node.
type PodRef struct {
	Namespace string
	Name      string
}

// String renders the ref in the wire form the `pod=` parameter accepts.
func (r PodRef) String() string { return r.Namespace + "/" + r.Name }

// StorageRoots is the resolved root selection of a storage-flow request: the
// components a path must touch to be retained.
//
// The five sets mirror the five root parameters. They are RAW NAMES, not node
// ids — resolution to ids happens in ProjectStorage against the built graph,
// because an id needs the ONTAP cluster (or the Kubernetes cluster identity)
// that only the graph knows.
//
// Nodes is deliberately ONE set serving BOTH sides of the flow. The `node=`
// parameter is matched against the ONTAP controller name AND the Kubernetes
// node name: an operator searching for a node by name generally does not know
// which kind it is, and a name present on both tiers makes both roots. That is
// why RequestedStorage and RequestedWorkload both consult it.
type StorageRoots struct {
	// ONTAPClusters selects every controller, aggregate and SVM of an ONTAP
	// cluster. Storage side.
	ONTAPClusters map[string]struct{}
	// Nodes selects an ONTAP controller (storage side) or a Kubernetes node
	// (workload side) — or both, when the name exists on both tiers.
	Nodes map[string]struct{}
	// Aggrs selects an ONTAP aggregate. Storage side.
	Aggrs map[string]struct{}
	// SVMs selects an SVM. Storage side.
	SVMs map[string]struct{}
	// Pods selects one pod by (namespace, name). Workload side.
	Pods map[PodRef]struct{}
}

// RequestedStorage reports whether the request carried any root that could
// resolve to a storage-side component. It is a property of the REQUEST, not of
// what resolved: a requested side that resolved to nothing retains nothing (a
// mistyped `?aggr=` returns an empty body, never the whole estate), which is
// only expressible by separating "asked" from "found".
func (r StorageRoots) RequestedStorage() bool {
	return len(r.ONTAPClusters) > 0 || len(r.Aggrs) > 0 || len(r.SVMs) > 0 || len(r.Nodes) > 0
}

// RequestedWorkload reports whether the request carried any root that could
// resolve to a workload-side component. See RequestedStorage for why this is a
// property of the request rather than of the resolution.
func (r StorageRoots) RequestedWorkload() bool {
	return len(r.Pods) > 0 || len(r.Nodes) > 0
}

// Any reports whether any root at all was requested. No root means "the whole
// selected estate", which is a legitimate request rather than an empty one.
func (r StorageRoots) Any() bool {
	return r.RequestedStorage() || r.RequestedWorkload()
}

// StorageScope is the projection filter of GET /v1/storage-graph, the
// storage-flow counterpart of Scope.
//
// Clusters and Namespaces carry the same meaning and the same defence-in-depth
// role they do in Scope: the build already narrowed the topology at the source,
// and the projection re-applies them so a node that reached the graph anyway
// cannot slip into a filtered view. They narrow the Kubernetes side only — a
// storage root is never dropped by them, because a NetApp node belongs to no
// Kubernetes cluster and carries no namespace.
//
// There is deliberately no EdgeTypes field and no Inventory field: the body has
// exactly one edge type, and the storage projection is reachability over the
// tier chain rather than the connectivity prune, so `edge_type` and `prune` are
// ignored by the request parser.
type StorageScope struct {
	Clusters   map[string]struct{} // empty ⇒ no cluster filter
	Namespaces map[string]struct{} // empty ⇒ no namespace filter
	Roots      StorageRoots
}

// NewStorageScope constructs a StorageScope from raw query-parameter values.
//
// Every set drops empty values and de-duplicates, so `?aggr=a&aggr=a&aggr=` and
// `?aggr=a` are indistinguishable — the determinism rule that makes two
// differently-ordered requests produce byte-identical bodies.
//
// pods carries the raw `pod=<namespace>/<name>` values. Each must split on
// exactly one "/" into two non-empty segments; anything else is an error, so a
// bare `?pod=orders-0` is rejected rather than silently matching nothing. The
// self-contained form is what keeps a root unambiguous: `namespace` is already
// an OR-combined narrowing filter, so qualifying a pod root with it would make
// `?namespace=a&namespace=b&pod=x` undecidable.
func NewStorageScope(clusters, namespaces, ontapClusters, nodes, aggrs, svms, pods []string) (StorageScope, error) {
	refs, err := podRefSet(pods)
	if err != nil {
		return StorageScope{}, err
	}
	return StorageScope{
		Clusters:   stringSet(clusters),
		Namespaces: stringSet(namespaces),
		Roots: StorageRoots{
			ONTAPClusters: stringSet(ontapClusters),
			Nodes:         stringSet(nodes),
			Aggrs:         stringSet(aggrs),
			SVMs:          stringSet(svms),
			Pods:          refs,
		},
	}, nil
}

// podRefSet parses the `pod=` values into a de-duplicated PodRef set. An empty
// value is skipped (a bare `?pod=` is a no-op, matching every other set), but a
// NON-empty malformed value is an error — a typo must not degrade into "no root
// on this side", which would silently widen the answer to the whole estate.
func podRefSet(values []string) (map[PodRef]struct{}, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[PodRef]struct{}, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		ns, name, ok := strings.Cut(v, "/")
		if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("invalid pod root %q: expected <namespace>/<pod-name>", v)
		}
		out[PodRef{Namespace: ns, Name: name}] = struct{}{}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// SortedPodRefs returns the refs in deterministic (namespace, name) order.
// Root resolution iterates them in this order so the projection is a pure
// function of the value set rather than of map iteration.
func SortedPodRefs(m map[PodRef]struct{}) []PodRef {
	out := make([]PodRef, 0, len(m))
	for r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}
