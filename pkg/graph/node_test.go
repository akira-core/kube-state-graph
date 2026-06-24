package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStorageClass_OnlyPVCsCarryIt — the StorageClass() accessor returns the
// resolved value for a PVC and "" for every other node kind (and a class-less
// PVC). It is consumed only by the Cytoscape serialiser for compound grouping
// and is never a label or serialised attribute.
func TestStorageClass_OnlyPVCsCarryIt(t *testing.T) {
	pvc := &PVCNode{IDValue: "c/n/claim", NameValue: "claim", StorageClassValue: "gp3"}
	assert.Equal(t, "gp3", pvc.StorageClass())

	classless := &PVCNode{IDValue: "c/n/claim2", NameValue: "claim2"}
	assert.Empty(t, classless.StorageClass(), "PVC with no resolved StorageClass returns empty")

	others := []GraphNode{
		&PodNode{IDValue: "c/u"},
		&K8sNode{IDValue: "c/w"},
		&ServiceNode{IDValue: "c/n/s"},
		&ExternalNode{IDValue: "external/x"},
	}
	for _, n := range others {
		assert.Emptyf(t, n.StorageClass(), "%T must return empty StorageClass", n)
	}
}

// TestApplicationAndContainers_OnlyPodsCarryThem — the Application() and
// Containers() accessors return a pod's resolved values and the zero value
// ("" / nil) for every other node kind (and an unenriched pod). Both are typed
// attributes consumed by the serialiser, never labels.
func TestApplicationAndContainers_OnlyPodsCarryThem(t *testing.T) {
	pod := &PodNode{
		IDValue:          "c/u",
		NameValue:        "checkout",
		ApplicationValue: "checkout",
		ContainersValue:  []Container{{Name: "app", Image: "reg/app:1.2"}},
	}
	assert.Equal(t, "checkout", pod.Application())
	assert.Equal(t, []Container{{Name: "app", Image: "reg/app:1.2"}}, pod.Containers())

	bare := &PodNode{IDValue: "c/u2", NameValue: "bare"}
	assert.Empty(t, bare.Application(), "pod with no Application returns empty")
	assert.Nil(t, bare.Containers(), "pod with no containers returns nil")

	others := []GraphNode{
		&K8sNode{IDValue: "c/w"},
		&PVCNode{IDValue: "c/n/claim"},
		&ServiceNode{IDValue: "c/n/s"},
		&ExternalNode{IDValue: "external/x"},
	}
	for _, n := range others {
		assert.Emptyf(t, n.Application(), "%T must return empty Application", n)
		assert.Nilf(t, n.Containers(), "%T must return nil Containers", n)
	}
}

// TestReadyStatus_OnlyK8sNodesCarryIt — the ReadyStatus() accessor returns the
// node's resolved Ready-condition status for a K8sNode and "" for every other
// node kind (and a node with no observed Ready-condition data). It is a typed
// attribute consumed by the serialiser, never a label.
func TestReadyStatus_OnlyK8sNodesCarryIt(t *testing.T) {
	ready := &K8sNode{IDValue: "c/w", NameValue: "w", ReadyStatusValue: ReadyStatusReady}
	assert.Equal(t, ReadyStatusReady, ready.ReadyStatus())

	for _, want := range []string{ReadyStatusReady, ReadyStatusNotReady, ReadyStatusUnknown} {
		n := &K8sNode{IDValue: "c/w", ReadyStatusValue: want}
		assert.Equal(t, want, n.ReadyStatus())
	}

	bare := &K8sNode{IDValue: "c/w2", NameValue: "w2"}
	assert.Empty(t, bare.ReadyStatus(), "node with no Ready-condition data returns empty")

	others := []GraphNode{
		&PodNode{IDValue: "c/u"},
		&PVCNode{IDValue: "c/n/claim"},
		&ServiceNode{IDValue: "c/n/s"},
		&ExternalNode{IDValue: "external/x"},
	}
	for _, n := range others {
		assert.Emptyf(t, n.ReadyStatus(), "%T must return empty ReadyStatus", n)
	}
}
