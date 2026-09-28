package graph

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// PodRef names one pod by (namespace, name) rather than by UID. It is the
// pod-root key of the storage-flow projection: an operator searching for
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

// StorageRootKind is the one root parameter a storage-flow request carries.
// Values are the wire parameter names.
type StorageRootKind string

const (
	StorageRootONTAPCluster StorageRootKind = "ontap_cluster"
	StorageRootONTAPNode    StorageRootKind = "ontap_node"
	StorageRootAggr         StorageRootKind = "aggr"
	StorageRootSVM          StorageRootKind = "svm"
	StorageRootNode         StorageRootKind = "node"
	StorageRootPod          StorageRootKind = "pod"
	StorageRootApplication  StorageRootKind = "application"
)

// StorageRootKinds is every accepted kind, in parameter order.
var StorageRootKinds = []StorageRootKind{
	StorageRootONTAPCluster,
	StorageRootONTAPNode,
	StorageRootAggr,
	StorageRootSVM,
	StorageRootNode,
	StorageRootPod,
	StorageRootApplication,
}

// StorageRoots is the resolved root selection of a storage-flow request: one
// kind and its values. A path is retained when it touches any of them.
//
// The values are RAW NAMES, not node ids — resolution to ids happens in
// ProjectStorage against the built graph, because an id needs the ONTAP
// cluster (or the Kubernetes cluster identity) that only the graph knows.
// Pods are PodRef; every other kind stores Names. Both are sorted and
// de-duplicated, and a kind whose every value was empty is the zero value
// (no root).
type StorageRoots struct {
	Kind StorageRootKind
	// Names is the value set for every kind except pod.
	Names []string
	// Pods is the value set when Kind is pod.
	Pods []PodRef
}

// Any reports whether a root kind with at least one value was requested.
func (r StorageRoots) Any() bool {
	return r.Kind != "" && (len(r.Names) > 0 || len(r.Pods) > 0)
}

// HasName reports whether name is one of this root's non-pod values.
func (r StorageRoots) HasName(name string) bool {
	return slices.Contains(r.Names, name)
}

// HasPod reports whether ref is one of this root's pod values.
func (r StorageRoots) HasPod(ref PodRef) bool {
	return slices.Contains(r.Pods, ref)
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
// There is deliberately no Inventory field: the storage projection is
// reachability over the tier chain rather than the connectivity prune, so
// `prune` is ignored by the request parser. The body carries exactly one edge
// type, and no scope of either endpoint filters by edge type any more.
type StorageScope struct {
	Clusters   map[string]struct{} // empty ⇒ no cluster filter
	Namespaces map[string]struct{} // empty ⇒ no namespace filter
	Roots      StorageRoots
}

// NewStorageScope constructs a StorageScope from raw query-parameter values.
//
// kind is the one root parameter. values are its raw values: `pod` values are
// `<namespace>/<name>`, every other kind is a bare name. Empty values are
// dropped and the rest are sorted and de-duplicated, so `?aggr=a&aggr=a&aggr=`
// and `?aggr=a` are indistinguishable. A kind whose every value was empty is
// stored as no root.
//
// A pod value must split on exactly one "/" into two non-empty segments;
// anything else is an error, so a bare `?pod=orders-0` is rejected rather than
// silently matching nothing. An unknown kind is an error.
func NewStorageScope(clusters, namespaces []string, kind StorageRootKind, values []string) (StorageScope, error) {
	roots, err := newStorageRoots(kind, values)
	if err != nil {
		return StorageScope{}, err
	}
	return StorageScope{
		Clusters:   stringSet(clusters),
		Namespaces: stringSet(namespaces),
		Roots:      roots,
	}, nil
}

func newStorageRoots(kind StorageRootKind, values []string) (StorageRoots, error) {
	if kind == "" {
		if len(nonEmpty(values)) > 0 {
			return StorageRoots{}, fmt.Errorf("storage root values require a kind")
		}
		return StorageRoots{}, nil
	}
	if !slices.Contains(StorageRootKinds, kind) {
		return StorageRoots{}, fmt.Errorf("unknown storage root kind %q", kind)
	}
	kept := nonEmpty(values)
	if len(kept) == 0 {
		return StorageRoots{}, nil
	}
	if kind == StorageRootPod {
		pods, err := podRefs(kept)
		if err != nil {
			return StorageRoots{}, err
		}
		return StorageRoots{Kind: kind, Pods: pods}, nil
	}
	slices.Sort(kept)
	kept = slices.Compact(kept)
	return StorageRoots{Kind: kind, Names: kept}, nil
}

func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// podRefs parses `pod=` values into a sorted, de-duplicated PodRef slice.
// Every value is non-empty; a malformed one is an error.
func podRefs(values []string) ([]PodRef, error) {
	out := make([]PodRef, 0, len(values))
	seen := make(map[PodRef]struct{}, len(values))
	for _, v := range values {
		ns, name, ok := strings.Cut(v, "/")
		if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("invalid pod root %q: expected <namespace>/<pod-name>", v)
		}
		ref := PodRef{Namespace: ns, Name: name}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	slices.SortFunc(out, func(a, b PodRef) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}
