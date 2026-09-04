package graph

import (
	"testing"
)

func TestEdgeTypePodCallsService_MayCrossCluster(t *testing.T) {
	var def *EdgeTypeDefinition
	for i := range EdgeTypes {
		if EdgeTypes[i].Type == EdgeTypePodCallsService {
			def = &EdgeTypes[i]
			break
		}
	}
	if def == nil {
		t.Fatal("EdgeTypePodCallsService is not registered in EdgeTypes")
	}
	if !def.MayCrossCluster {
		t.Error("pod-calls-service may_cross_cluster must be true (route-engine ingress-cluster anchoring)")
	}
	if !ValidEdgeType(EdgeTypePodCallsService) {
		t.Error("ValidEdgeType must accept pod-calls-service")
	}
}

// The storage-flow entry describes the whole fixed tier chain in ONE registry
// row: its source and target sets are the union of the chain's five hops, so a
// consumer validating an edge against the catalogue accepts every tier. It is
// never cross-cluster — the NetApp tiers belong to no Kubernetes cluster and
// both Kubernetes hops are intra-cluster by construction.
func TestEdgeTypeStorageFlow_RegistryEntry(t *testing.T) {
	var def *EdgeTypeDefinition
	for i := range EdgeTypes {
		if EdgeTypes[i].Type == EdgeTypeStorageFlow {
			def = &EdgeTypes[i]
			break
		}
	}
	if def == nil {
		t.Fatal("EdgeTypeStorageFlow is not registered in EdgeTypes")
	}
	if !ValidEdgeType(EdgeTypeStorageFlow) {
		t.Fatal("ValidEdgeType must accept storage-flow")
	}
	if !def.Directed {
		t.Error("storage-flow is directed, storage → workload")
	}
	if def.MayCrossCluster {
		t.Error("storage-flow may_cross_cluster must be false")
	}

	wantSource := []NodeType{NodeTypeNetAppNode, NodeTypeNetAppAggr, NodeTypeNetAppSVM, NodeTypePVC, NodeTypePod}
	wantTarget := []NodeType{NodeTypeNetAppAggr, NodeTypeNetAppSVM, NodeTypePVC, NodeTypePod, NodeTypeK8sNode}
	if len(def.SourceType) != len(wantSource) {
		t.Fatalf("source_type = %v, want %v", def.SourceType, wantSource)
	}
	for i, want := range wantSource {
		if def.SourceType[i] != want {
			t.Errorf("source_type[%d] = %q, want %q", i, def.SourceType[i], want)
		}
	}
	for i, want := range wantTarget {
		if def.TargetType[i] != want {
			t.Errorf("target_type[%d] = %q, want %q", i, def.TargetType[i], want)
		}
	}

	// The chain is a path: every tier's source type must be some tier's target
	// type or the chain head, and vice versa. Concretely, sources are the chain
	// minus its last node and targets the chain minus its first.
	labels := map[string]bool{}
	for _, l := range def.Labels {
		labels[l.Name] = true
		if l.ValueType != "string" {
			t.Errorf("label %q value_type = %q, want string", l.Name, l.ValueType)
		}
	}
	for _, want := range []string{"tier", "attribution"} {
		if !labels[want] {
			t.Errorf("storage-flow must enumerate a %q label", want)
		}
	}
}

// The tier constants and the ordered StorageTiers list must not drift apart:
// the assembler, the projection and the registry description all read the same
// five values in the same chain order.
func TestStorageTiers_ChainOrder(t *testing.T) {
	want := []string{"node-aggr", "aggr-svm", "svm-pvc", "pvc-pod", "pod-node"}
	if len(StorageTiers) != len(want) {
		t.Fatalf("StorageTiers = %v, want %v", StorageTiers, want)
	}
	for i, w := range want {
		if StorageTiers[i] != w {
			t.Errorf("StorageTiers[%d] = %q, want %q", i, StorageTiers[i], w)
		}
	}
	if StorageTierNodeAggr != want[0] || StorageTierAggrSVM != want[1] ||
		StorageTierSVMPVC != want[2] || StorageTierPVCPod != want[3] ||
		StorageTierPodNode != want[4] {
		t.Error("a tier constant disagrees with StorageTiers")
	}
	if AttributionSplit != "split" {
		t.Errorf("AttributionSplit = %q, want \"split\"", AttributionSplit)
	}
}
