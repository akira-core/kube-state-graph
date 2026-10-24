package build

import (
	"slices"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

const trackingIDLabel = "annotation_argocd_argoproj_io_tracking_id"

// withStatefulSetApp adds a StatefulSet carrying an ArgoCD tracking-id to fx.
func withStatefulSetApp(fx map[promql.Query]model.Vector, ns, statefulset, app string) {
	fx[promql.QStatefulSetAnnotations] = append(slices.Clone(fx[promql.QStatefulSetAnnotations]),
		planKSM("namespace", ns, "statefulset", statefulset,
			trackingIDLabel, app+":apps/StatefulSet:"+ns+"/"+statefulset))
}

// drawnPods are the names of the pods a projected k8s body carries.
func drawnPods(t *testing.T, g *graph.Graph, scope graph.StorageScope) []string {
	t.Helper()
	body := cytoscape.Serialise(g, graph.ProjectK8s(g, scope))
	var out []string
	for _, n := range body.Elements.Nodes {
		if n.Data.Type == string(graph.NodeTypePod) {
			out = append(out, n.Data.Name)
		}
	}
	slices.Sort(out)
	return out
}

// Spec: "A shared claim adds the co-mounter's reads". orders-0 on the root node
// mounts RWX claim shop/shared and report-0, on worker-2, mounts it too. Once
// mounter completion names report-0, its pod and owner are read, then its
// controllers and worker-2's node families; the claim inherits its Application
// over BOTH mounters; and neither report-0 nor worker-2 is drawn.
func TestBuildK8s_ASharedClaimAddsTheCoMountersReads(t *testing.T) {
	fx := miniEstate(
		miniPod{ns: "shop", name: "orders-0", node: "worker-1", owner: "orders-sts", claim: "shared"},
		miniPod{ns: "shop", name: "report-0", node: "worker-2", owner: "report-sts", claim: "shared"},
	)
	// The claim inherits the lexically-smallest Application of its mounters, and
	// that one belongs to the mounter that is not drawn.
	withStatefulSetApp(fx, "shop", "orders-sts", "bravo")
	withStatefulSetApp(fx, "shop", "report-sts", "alpha")
	scope := vlrScope(t, graph.StorageRootNode, []string{"worker-1"})

	g := requireK8sOutputPreserved(t, fx, Options{}, vlrSel, scope)
	assert.Equal(t, "alpha", g.NodesByID[graph.PVCID(k8sCluster, "shop", "shared")].Application(),
		"the claim inherits over both mounters")
	assert.Equal(t, []string{"orders-0"}, drawnPods(t, g, scope), "report-0 is not drawn")

	_, q := buildK8sOver(t, fx, Options{}, scope.Roots)
	assert.Contains(t, flatten(q.ScopeValues(promql.QPodInfo, "pod")), "report-0")
	assert.Contains(t, flatten(q.ScopeValues(promql.QPodOwner, "pod")), "report-0")
	assert.Contains(t, flatten(q.ScopeValues(promql.QStatefulSetAnnotations, "statefulset")), "report-sts")
	assert.Contains(t, flatten(q.ScopeValues(promql.QNodeInfo, "node")), "worker-2")

	// And in that order: the co-mounter's pod reads wait for mounter completion,
	// its controllers and node for the pod reads. Five rounds, not three.
	b := runLockstep(t, fx, Options{}, scope.Roots)
	require.Len(t, b.Rounds, 5, "rounds: %v", b.Rounds)
	assert.Equal(t, []string{string(promql.QPodInfo), string(promql.QPodOwner)}, b.Rounds[3],
		"r4 is the co-mounter's pod and owner")
	assert.Contains(t, b.Rounds[4], string(promql.QStatefulSetAnnotations), "r5 reads its controller")
	assert.Contains(t, b.Rounds[4], string(promql.QNodeInfo), "and its node")
}

// Spec: "A name is read once per family". The co-mounter is owned by the same
// StatefulSet as the root-node pod, so no second kube_statefulset_annotations
// query names it, and its pod read adds no controller round at all.
func TestBuildK8s_ANameIsReadOncePerFamily(t *testing.T) {
	fx := miniEstate(
		miniPod{ns: "shop", name: "orders-0", node: "worker-1", owner: "orders-sts", claim: "shared"},
		miniPod{ns: "shop", name: "orders-1", node: "worker-1", owner: "orders-sts"},
		miniPod{ns: "shop", name: "orders-2", node: "worker-2", owner: "orders-sts", claim: "shared"},
	)
	withStatefulSetApp(fx, "shop", "orders-sts", "storefront")
	scope := vlrScope(t, graph.StorageRootNode, []string{"worker-1"})

	requireK8sOutputPreserved(t, fx, Options{}, vlrSel, scope)
	_, q := buildK8sOver(t, fx, Options{}, scope.Roots)
	assert.Equal(t, [][]string{{"orders-sts"}}, q.ScopeValues(promql.QStatefulSetAnnotations, "statefulset"),
		"the StatefulSet is read once, though a root-node pod and a co-mounter both name it")

	assert.Equal(t, [][]string{{"worker-1"}, {"worker-2"}}, sortedScopes(q.ScopeValues(promql.QNodeInfo, "node")),
		"and each node once: the co-mounter's node is read for itself, not with the root's")
}

// sortedScopes orders the per-query value sets by their first value, so a
// comparison does not depend on the order concurrent queries arrived in.
func sortedScopes(scopes [][]string) [][]string {
	out := slices.Clone(scopes)
	slices.SortFunc(out, slices.Compare)
	return out
}
