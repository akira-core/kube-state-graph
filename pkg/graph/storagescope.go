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

// ClaimRef names one PersistentVolumeClaim by (namespace, name). It is the
// `pvc=` root key of the storage-flow projection, the PodRef shape for the same
// reason: an operator asking "which filer backs shop/orders-data" knows the
// claim's name, and claim names are unique per namespace within a cluster. When
// the selected estate holds several clusters the name may resolve in more than
// one; the optional `cluster` filter narrows that.
type ClaimRef struct {
	Namespace string
	Name      string
}

// String renders the ref in the wire form the `pvc=` parameter accepts.
func (r ClaimRef) String() string { return r.Namespace + "/" + r.Name }

// ONTAPRef names one aggregate or SVM on one ONTAP cluster. It is the qualified
// form of an `aggr=` / `svm=` root (`<ontap_cluster>/<name>`): aggregate and SVM
// names recur on every filer, so the bare form roots that name everywhere and
// this one roots exactly one component. ONTAP cluster, aggregate and SVM names
// cannot contain "/", which makes the split unambiguous.
type ONTAPRef struct {
	ONTAPCluster string
	Name         string
}

// String renders the ref in the wire form `aggr=` / `svm=` accept.
func (r ONTAPRef) String() string { return r.ONTAPCluster + "/" + r.Name }

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
	StorageRootPVC          StorageRootKind = "pvc"
	StorageRootPV           StorageRootKind = "pv"
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
	StorageRootPVC,
	StorageRootPV,
	StorageRootApplication,
}

// StorageRoots is the resolved root selection of a storage-flow request: one
// kind and its values. A path is retained when it touches any of them.
//
// The values are RAW NAMES, not node ids — resolution to ids happens in
// ProjectStorage against the built graph, because an id needs the ONTAP
// cluster (or the Kubernetes cluster identity) that only the graph knows.
// Pods are PodRef and claims are ClaimRef; every other kind — a PersistentVolume
// (`pv`) included, being cluster-scoped and named bare — stores Names, except
// that an `aggr` / `svm` value of the form `<ontap_cluster>/<name>` is stored in
// Qualified. Each is sorted and de-duplicated, and a kind whose every value was
// empty is the zero value (no root).
type StorageRoots struct {
	Kind StorageRootKind
	// Names is the value set for every kind except pod and pvc; for aggr and svm
	// it holds the BARE values only (a name on every filer).
	Names []string
	// Qualified is the value set of the aggr and svm kinds' qualified values —
	// one aggregate or SVM on one ONTAP cluster. A qualified value whose name is
	// also a bare value is dropped at construction: the bare form subsumes it.
	Qualified []ONTAPRef
	// Pods is the value set when Kind is pod.
	Pods []PodRef
	// Claims is the value set when Kind is pvc.
	Claims []ClaimRef
}

// Any reports whether a root kind with at least one value was requested.
func (r StorageRoots) Any() bool {
	return r.Kind != "" && (len(r.Names) > 0 || len(r.Qualified) > 0 || len(r.Pods) > 0 || len(r.Claims) > 0)
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
// A pod or pvc value must split on exactly one "/" into two non-empty segments;
// anything else is an error, so a bare `?pod=orders-0` is rejected rather than
// silently matching nothing. A pv value is a bare PersistentVolume name. An aggr
// or svm value containing "/" is the qualified `<ontap_cluster>/<name>` form and
// must split the same way; a bare value is unchanged. An unknown kind is an
// error.
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
	switch kind {
	case StorageRootPod:
		pods, err := podRefs(kept)
		if err != nil {
			return StorageRoots{}, err
		}
		return StorageRoots{Kind: kind, Pods: pods}, nil
	case StorageRootPVC:
		claims, err := claimRefs(kept)
		if err != nil {
			return StorageRoots{}, err
		}
		return StorageRoots{Kind: kind, Claims: claims}, nil
	case StorageRootAggr, StorageRootSVM:
		return ontapRoots(kind, kept)
	default:
		// every other kind, a PersistentVolume included, is a bare name
	}
	slices.Sort(kept)
	kept = slices.Compact(kept)
	return StorageRoots{Kind: kind, Names: kept}, nil
}

// ontapRoots splits `aggr=` / `svm=` values into bare names and qualified
// `<ontap_cluster>/<name>` refs. A value with a "/" must split into two
// non-empty segments on exactly one of them, else it is an error naming the
// parameter and the expected shape. Both sets are sorted and de-duplicated, and a
// qualified ref whose name is also bare is dropped: the bare form already roots
// that name on every filer.
func ontapRoots(kind StorageRootKind, values []string) (StorageRoots, error) {
	var bare []string
	var qualified []ONTAPRef
	for _, v := range values {
		if !strings.Contains(v, "/") {
			bare = append(bare, v)
			continue
		}
		oc, name, _ := strings.Cut(v, "/")
		if oc == "" || name == "" || strings.Contains(name, "/") {
			return StorageRoots{}, fmt.Errorf("invalid %s root %q: expected <name> or <ontap_cluster>/<name>", kind, v)
		}
		qualified = append(qualified, ONTAPRef{ONTAPCluster: oc, Name: name})
	}
	slices.Sort(bare)
	bare = slices.Compact(bare)
	slices.SortFunc(qualified, func(a, b ONTAPRef) int {
		return cmp.Or(cmp.Compare(a.ONTAPCluster, b.ONTAPCluster), cmp.Compare(a.Name, b.Name))
	})
	qualified = slices.Compact(qualified)
	qualified = slices.DeleteFunc(qualified, func(r ONTAPRef) bool { return slices.Contains(bare, r.Name) })
	if len(qualified) == 0 {
		qualified = nil
	}
	return StorageRoots{Kind: kind, Names: bare, Qualified: qualified}, nil
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

// namespacedRef is the unexported parse result shared by every root kind whose
// value is `<namespace>/<name>`. It has the field set of PodRef and ClaimRef so
// each converts to it directly.
type namespacedRef struct {
	Namespace string
	Name      string
}

// namespacedRefs parses `<namespace>/<name>` root values into a sorted,
// de-duplicated slice. Every value is non-empty; one that does not split on
// exactly one "/" into two non-empty segments is an error naming the parameter
// and the expected shape (nameHint), so pod and pvc roots cannot drift apart.
func namespacedRefs(values []string, param StorageRootKind, nameHint string) ([]namespacedRef, error) {
	out := make([]namespacedRef, 0, len(values))
	seen := make(map[namespacedRef]struct{}, len(values))
	for _, v := range values {
		ns, name, ok := strings.Cut(v, "/")
		if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("invalid %s root %q: expected <namespace>/<%s>", param, v, nameHint)
		}
		ref := namespacedRef{Namespace: ns, Name: name}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	slices.SortFunc(out, func(a, b namespacedRef) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

// podRefs parses `pod=` values into a sorted, de-duplicated PodRef slice.
func podRefs(values []string) ([]PodRef, error) {
	refs, err := namespacedRefs(values, StorageRootPod, "pod-name")
	if err != nil {
		return nil, err
	}
	out := make([]PodRef, len(refs))
	for i, r := range refs {
		out[i] = PodRef(r)
	}
	return out, nil
}

// claimRefs parses `pvc=` values into a sorted, de-duplicated ClaimRef slice.
func claimRefs(values []string) ([]ClaimRef, error) {
	refs, err := namespacedRefs(values, StorageRootPVC, "claim-name")
	if err != nil {
		return nil, err
	}
	out := make([]ClaimRef, len(refs))
	for i, r := range refs {
		out[i] = ClaimRef(r)
	}
	return out, nil
}
