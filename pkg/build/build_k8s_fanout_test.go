package build

import (
	"cmp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// miniPod is one pod of miniEstate. owner is the controller named by
// kube_pod_owner — a StatefulSet unless ownerKind says otherwise; claim, when
// set, is a claim of the pod's namespace it mounts.
type miniPod struct {
	ns, name, node, owner, claim string
	ownerKind                    string
}

// miniEstate is a fixture of exactly the pods it is given, in cluster c1 of
// zone-a / prod: kube_pod_info, kube_pod_owner, the bindings and claim info of
// the claims they mount, and the node families of the nodes they run on. A test
// reads the queries a build issued over it, so its shape is chosen to make
// those queries exact; planEstate() is for the shapes a body must survive.
func miniEstate(pods ...miniPod) map[promql.Query]model.Vector {
	fx := map[promql.Query]model.Vector{}
	add := func(q promql.Query, s *model.Sample) { fx[q] = append(fx[q], s) }
	nodes := map[string]bool{}
	claims := map[[2]string]bool{}
	for _, p := range pods {
		add(promql.QPodInfo, planKSM("namespace", p.ns, "pod", p.name, "uid", "uid-"+p.ns+"-"+p.name, "node", p.node))
		if p.owner != "" {
			kind := cmp.Or(p.ownerKind, "StatefulSet")
			add(promql.QPodOwner, planKSM("namespace", p.ns, "pod", p.name,
				"owner_kind", kind, "owner_name", p.owner, "owner_is_controller", "true"))
		}
		if p.node != "" && !nodes[p.node] {
			nodes[p.node] = true
			add(promql.QNodeInfo, planKSM("node", p.node))
			add(promql.QNodeStatusCondition, planKSM("node", p.node, "condition", "Ready", "status", "true"))
		}
		if p.claim != "" {
			add(promql.QPVCBindings, planKSM("namespace", p.ns, "pod", p.name,
				"persistentvolumeclaim", p.claim, "volume", "data"))
			if key := [2]string{p.ns, p.claim}; !claims[key] {
				claims[key] = true
				add(promql.QPVCInfo, planKSM("namespace", p.ns, "persistentvolumeclaim", p.claim,
					"volumename", "pvc-"+p.ns+"-"+p.claim, "storageclass", "netapp-nas"))
				add(promql.QKubeletVolumeUsedBytes, withValue(planKSM("namespace", p.ns, "persistentvolumeclaim", p.claim), 1))
				add(promql.QKubeletVolumeCapacityBytes, withValue(planKSM("namespace", p.ns, "persistentvolumeclaim", p.claim), 2))
			}
		}
	}
	return fx
}

const k8sQuerySelector = `az="zone-a",env="prod"`

func k8sQuery(name promql.Query, matchers string) string {
	return "last_over_time(" + string(name) + "{" + k8sQuerySelector + "," + matchers + "}[1m])"
}

// Spec: "Reads by reference, Kubernetes families only" / "Node root query set".
func TestBuildK8s_NodeRootQuerySet(t *testing.T) {
	fx := miniEstate(miniPod{ns: "shop", name: "web-0", node: "worker-1", owner: "web"})
	_, q := buildK8sOver(t, fx, Options{}, k8sRootsOf(t, graph.StorageRootNode, "worker-1"))

	assert.ElementsMatch(t, []string{
		k8sQuery(promql.QPodInfo, `node="worker-1"`),
		k8sQuery(promql.QPodInfo, `namespace="shop",pod="web-0"`),
	}, q.QueriesFor(promql.QPodInfo), "the seed, then incarnation completion")
	assert.Equal(t, []string{k8sQuery(promql.QPVCBindings, `namespace="shop",pod="web-0"`)},
		q.QueriesFor(promql.QPVCBindings), "the claim-binding read is on the seed's pair")
	assert.Equal(t, []string{k8sQuery(promql.QPodOwner, `namespace="shop",pod="web-0"`)},
		q.QueriesFor(promql.QPodOwner), "so is the owner read")

	assertOnlyKubernetesFamilies(t, q)
}

// assertOnlyKubernetesFamilies requires that a k8s build issued no Harvest,
// service-side, container, service-graph or up{} query, and did read ALERTS.
func assertOnlyKubernetesFamilies(t *testing.T, q *promqlfake.Querier) {
	t.Helper()
	seen := map[string]bool{}
	for _, is := range q.Issued() {
		seen[is.Name] = true
		assert.False(t, harvestFamily(is.Name), "%s: no Harvest query", is.Name)
	}
	for _, forbidden := range []promql.Query{
		promql.QServiceInfo, promql.QEndpointSliceEndpoints, promql.QEndpointSliceLabels,
		promql.QServiceAnnotations, promql.QPodContainerInfo,
		promql.QServiceGraphTotal, promql.QServiceGraphFailedTotal, promql.QServiceGraphServerSecondsBucket,
		promql.QUpProbe,
	} {
		assert.False(t, seen[string(forbidden)], "%s must not be issued", forbidden)
	}
	assert.True(t, seen[string(promql.QAlerts)], "the alert overlay is read")
}

// Every root shape reads nothing outside the Kubernetes families, on the whole
// planEstate() with the application-metadata metric configured.
func TestBuildK8s_ReadsOnlyKubernetesFamilies(t *testing.T) {
	src := defaultAppMetadata()
	for name, roots := range map[string]graph.StorageRoots{
		"node root":        k8sRootsOf(t, graph.StorageRootNode, "worker-1"),
		"podless node":     k8sRootsOf(t, graph.StorageRootNode, "worker-3"),
		"pod root":         k8sRootsOf(t, graph.StorageRootPod, "shop/orders-0"),
		"unmounting pod":   k8sRootsOf(t, graph.StorageRootPod, "shop/web-0"),
		"pods in two ns":   k8sRootsOf(t, graph.StorageRootPod, "shop/orders-0", "platform/redis-0"),
		"unscheduled root": k8sRootsOf(t, graph.StorageRootPod, "shop/unscheduled-0"),
	} {
		t.Run(name, func(t *testing.T) {
			fx := planEstate()
			fx[promql.QAppMetadata] = model.Vector{appMetaRow("shop", "orders-0", "shop-v2", "team-a", "alice")}
			_, q := buildK8sOver(t, fx, Options{AppMetadata: &src}, roots)
			assertOnlyKubernetesFamilies(t, q)
		})
	}
}

// Spec: "Node root reads its claimless pods". The owner read names web-0, which
// mounts nothing and which a /v1/storage-graph request for the same node does
// not, and kube_pod_info is read for it once, by incarnation completion.
func TestBuildK8s_NodeRootReadsItsClaimlessPods(t *testing.T) {
	roots := k8sRootsOf(t, graph.StorageRootNode, "worker-1")
	_, k8sQ := buildK8sOver(t, planEstate(), Options{}, roots)
	storageQ := promqlfake.New(planEstate())
	_, err := New(storageQ, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, roots)
	require.NoError(t, err)

	k8sOwners := flatten(k8sQ.ScopeValues(promql.QPodOwner, "pod"))
	storageOwners := flatten(storageQ.ScopeValues(promql.QPodOwner, "pod"))
	for _, claimless := range []string{"web-0", "daemon-0", "ownerless-0", "job-annotated-0"} {
		assert.Contains(t, k8sOwners, claimless, "the k8s build reads the owner of %s", claimless)
		assert.NotContains(t, storageOwners, claimless, "the storage build never loads %s", claimless)
	}

	var reads int
	for _, scope := range k8sQ.ScopeValues(promql.QPodInfo, "pod") {
		if slices.Contains(scope, "web-0") {
			reads++
		}
	}
	assert.Equal(t, 1, reads, "kube_pod_info is read once for a root-node pod")
}

// severalNamespacesEstate: three pods on worker-1 in two namespaces, and a
// same-named pod of a third on worker-2 — platform/web-0 — that the independent
// alternations of the incarnation read also match.
func severalNamespacesEstate() map[promql.Query]model.Vector {
	return miniEstate(
		miniPod{ns: "shop", name: "orders-0", node: "worker-1", owner: "orders"},
		miniPod{ns: "shop", name: "web-0", node: "worker-1", owner: "web"},
		miniPod{ns: "platform", name: "redis-0", node: "worker-1", owner: "redis"},
		miniPod{ns: "platform", name: "web-0", node: "worker-2", owner: "platform-only-web"},
	)
}

// Spec: "Several namespaces in one query".
func TestBuildK8s_SeveralNamespacesInOneQuery(t *testing.T) {
	_, q := buildK8sOver(t, severalNamespacesEstate(), Options{}, k8sRootsOf(t, graph.StorageRootNode, "worker-1"))

	grouped := `namespace=~"platform|shop",pod=~"orders-0|redis-0|web-0"`
	assert.ElementsMatch(t, []string{
		k8sQuery(promql.QPodInfo, `node="worker-1"`),
		k8sQuery(promql.QPodInfo, grouped),
	}, q.QueriesFor(promql.QPodInfo), "incarnation completion is one query, not one per namespace")
	assert.Equal(t, []string{k8sQuery(promql.QPodOwner, grouped)}, q.QueriesFor(promql.QPodOwner))
	assert.Equal(t, []string{k8sQuery(promql.QPVCBindings, grouped)}, q.QueriesFor(promql.QPVCBindings))
}

// Spec: "A cross pair is discarded". platform/web-0 matches the grouped query and
// is dropped before anything is scoped from it: its node and its controller are
// never read, and the body is the one an unrestricted read produces.
func TestBuildK8s_ACrossPairIsDiscarded(t *testing.T) {
	fx := severalNamespacesEstate()
	scope := vlrScope(t, graph.StorageRootNode, []string{"worker-1"})
	g := requireK8sOutputPreserved(t, fx, Options{}, vlrSel, scope)
	assert.Nil(t, g.NodesByID[graph.PodID(k8sCluster, "uid-platform-web-0")], "the cross pair is not loaded")
	assert.NotNil(t, g.NodesByID[graph.PodID(k8sCluster, "uid-shop-web-0")])

	_, q := buildK8sOver(t, fx, Options{}, scope.Roots)
	assert.NotContains(t, flatten(q.ScopeValues(promql.QStatefulSetAnnotations, "statefulset")), "platform-only-web")
	assert.Equal(t, [][]string{{"worker-1"}}, q.ScopeValues(promql.QNodeInfo, "node"),
		"worker-2 runs only the cross pair, so no node read names it")
}

// Spec: "Claims are grouped the same way". shop/data and platform/queue are
// tracked; shop/queue and platform/data are the cross pairs the independent
// alternations also match, each mounted by a pod that must never be loaded.
func TestBuildK8s_ClaimsAreGroupedTheSameWay(t *testing.T) {
	fx := miniEstate(
		miniPod{ns: "shop", name: "db-0", node: "worker-1", owner: "db", claim: "data"},
		miniPod{ns: "platform", name: "mq-0", node: "worker-1", owner: "mq", claim: "queue"},
		miniPod{ns: "shop", name: "other-0", node: "worker-2", owner: "other-sts", claim: "queue"},
		miniPod{ns: "platform", name: "other-1", node: "worker-2", owner: "other-sts", claim: "data"},
	)
	scope := vlrScope(t, graph.StorageRootNode, []string{"worker-1"})
	g := requireK8sOutputPreserved(t, fx, Options{}, vlrSel, scope)

	_, q := buildK8sOver(t, fx, Options{}, scope.Roots)
	for _, fam := range []promql.Query{
		promql.QPVCInfo, promql.QPVCAnnotations,
		promql.QKubeletVolumeUsedBytes, promql.QKubeletVolumeCapacityBytes,
	} {
		assert.Equal(t, [][]string{{"platform", "shop"}}, q.ScopeValues(fam, "namespace"), "%s: one query", fam)
		assert.Equal(t, [][]string{{"data", "queue"}}, q.ScopeValues(fam, "persistentvolumeclaim"), fam)
	}
	// The binding family is read by pod once (the seed's pairs) and by claim once.
	byClaim := q.ScopeValues(promql.QPVCBindings, "persistentvolumeclaim")
	assert.Equal(t, [][]string{{"data", "queue"}}, byClaim, "mounter completion is one query")

	assert.Nil(t, g.NodesByID[graph.PVCID(k8sCluster, "shop", "queue")], "a returned shop/queue row is discarded")
	assert.Nil(t, g.NodesByID[graph.PVCID(k8sCluster, "platform", "data")], "so is platform/data")
	assert.Nil(t, g.NodesByID[graph.PodID(k8sCluster, "uid-shop-other-0")], "its mounter is not loaded")
	assert.Nil(t, g.NodesByID[graph.PodID(k8sCluster, "uid-platform-other-1")])
	assert.NotNil(t, g.NodesByID[graph.PVCID(k8sCluster, "shop", "data")])
	assert.NotNil(t, g.NodesByID[graph.PVCID(k8sCluster, "platform", "queue")])
}

// Spec: "A large pair set is chunked". A pod on worker-1 in each of several
// namespaces gives the incarnation read more pairs than a small byte budget
// carries: it splits into several queries, each naming only its own pairs'
// namespaces and names, they merge in chunk order, and a single pair larger
// than the budget is still issued. The body does not change.
func TestBuildK8s_ALargePairSetIsChunked(t *testing.T) {
	pods := make([]miniPod, 0, 12)
	want := make([]string, 0, 12)
	for _, ns := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"} {
		for _, name := range []string{"api-0", "worker-0"} {
			pods = append(pods, miniPod{ns: ns, name: ns + "-" + name, node: "worker-1", owner: ns + "-" + name + "-set"})
			want = append(want, ns+"-"+name)
		}
	}
	fx := miniEstate(pods...)
	scope := vlrScope(t, graph.StorageRootNode, []string{"worker-1"})

	for name, budget := range map[string]int{"a budget of a few pairs": 120, "a budget below any pair": 1} {
		t.Run(name, func(t *testing.T) {
			opts := Options{QoSScopeBatchBytes: budget}
			requireK8sOutputPreserved(t, fx, opts, vlrSel, scope)
			_, q := buildK8sOver(t, fx, opts, scope.Roots)

			incarnation := q.ScopeValues(promql.QPodInfo, "pod")
			// The first QueriesFor entry per chunk also carries the seed's node
			// matcher; ScopeValues reports only queries restricting `pod`.
			require.Greater(t, len(incarnation), 1, "the pair set does not fit one query")
			var covered []string
			for _, names := range incarnation {
				covered = append(covered, names...)
			}
			slices.Sort(covered)
			assert.Equal(t, want, covered, "every pair is read, in exactly one chunk")

			namespaces := q.ScopeValues(promql.QPodInfo, "namespace")
			require.Len(t, namespaces, len(incarnation))
			for i, ns := range namespaces {
				assert.Less(t, len(ns), 6, "chunk %d carries only its own pairs' namespaces: %v", i, ns)
				for _, n := range incarnation[i] {
					assert.Contains(t, ns, strings.SplitN(n, "-", 2)[0], "chunk %d: %s names its own namespace", i, n)
				}
			}
		})
	}
}

// Spec: "A root set the cap accepts is never rejected later". A pod root set that
// fills the cap exactly is chunked by preparePodSeed into maxRootedVolumeLabelChunks
// queries; the roots' kube_pod_info and kube_pod_owner are read in exactly that
// split, so the request is not rejected after a query was issued. One more root
// is rejected before any query.
func TestBuildK8s_APodRootSetAtTheCapIsNeverRejectedLater(t *testing.T) {
	// A one-byte budget puts every pod name in its own chunk.
	opts := Options{QoSScopeBatchBytes: 1}
	rootsOf := func(n int) ([]string, map[promql.Query]model.Vector) {
		refs := make([]string, 0, n)
		pods := make([]miniPod, 0, n)
		for i := range n {
			name := "pod-" + itoa(100+i)
			refs = append(refs, "shop/"+name)
			pods = append(pods, miniPod{ns: "shop", name: name, node: "worker-1", owner: "sts"})
		}
		return refs, miniEstate(pods...)
	}

	refs, fx := rootsOf(maxRootedVolumeLabelChunks)
	roots := k8sRootsOf(t, graph.StorageRootPod, refs...)
	g, q := buildK8sOver(t, fx, opts, roots)
	var drawn int
	for _, n := range g.NodesByID {
		if n.Type() == graph.NodeTypePod {
			drawn++
		}
	}
	require.Equal(t, maxRootedVolumeLabelChunks, drawn, "every root pod is drawn")
	for _, fam := range []promql.Query{promql.QPVCBindings, promql.QPodInfo, promql.QPodOwner} {
		assert.Len(t, q.ScopeValues(fam, "pod"), maxRootedVolumeLabelChunks,
			"%s is read in exactly the seed's chunk split", fam)
	}

	refs, fx = rootsOf(maxRootedVolumeLabelChunks + 1)
	over := promqlfake.New(fx)
	_, err := New(over, opts, nil, nil).BuildK8s(t.Context(), time.Minute, vlrEnd, vlrSel,
		k8sRootsOf(t, graph.StorageRootPod, refs...))
	require.Equal(t, ReasonInvalidScope, AsReason(err))
	assert.Empty(t, over.Issued(), "rejected before any query")
}
