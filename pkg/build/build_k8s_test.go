package build

import (
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/cytoscape"
	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/internal/testlog"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

const k8sCluster = "zone-a-prod-c1"

func k8sRootsOf(t *testing.T, kind graph.StorageRootKind, values ...string) graph.StorageRoots {
	t.Helper()
	return vlrScope(t, kind, values).Roots
}

// buildK8sOver runs BuildK8s over a promqlfake of fx and returns the graph and
// the fake, so a test can assert on both the body and the queries behind it.
func buildK8sOver(t *testing.T, fx map[promql.Query]model.Vector, opts Options, roots graph.StorageRoots) (*graph.Graph, *promqlfake.Querier) {
	t.Helper()
	q := promqlfake.New(fx)
	g, err := New(q, opts, nil, nil).BuildK8s(t.Context(), time.Minute, vlrEnd, vlrSel, roots)
	require.NoError(t, err)
	return g, q
}

// queryNames is the sorted, de-duplicated set of non-Harvest family names a
// fake answered.
func queryNames(q *promqlfake.Querier) []string {
	seen := map[string]bool{}
	for _, is := range q.Issued() {
		if !harvestFamily(is.Name) {
			seen[is.Name] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func flatten(groups [][]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// harvestFamily reports whether the query name belongs to the harvest family.
func harvestFamily(name string) bool {
	fam, ok := promql.FamilyOf(promql.Query(name))
	return ok && fam == promql.FamilyHarvest
}

// kubernetesFanOut is the multiset of rendered queries a fake answered, with
// the Harvest family taken out, sorted so two builds compare directly.
func kubernetesFanOut(q *promqlfake.Querier) []string {
	var out []string
	for _, is := range q.Issued() {
		if harvestFamily(is.Name) {
			continue
		}
		out = append(out, is.Name+"|"+is.Query)
	}
	slices.Sort(out)
	return out
}

// Spec: "Reads by reference, Kubernetes families only". A k8s build issues
// the Kubernetes queries the storage build issues for the same root — the
// seeds, completions and chunk caps are shared — and none of the families the
// storage build adds beside them. For a pod root the two sets are identical.
// For a node root the k8s build ALSO reads the root node's claimless pods
// (the storage body draws a pod through its claims; this one draws everything
// that runs on the node), so its pod-scoped reads widen by exactly those pods.
func TestBuildK8s_FanOutIsTheStoragePlansKubernetesHalf(t *testing.T) {
	src := defaultAppMetadata()
	// Pods on a node root that mount no claim, by case name.
	claimlessOnNode := map[string][]string{
		"node root": {"web-0", "daemon-0", "ownerless-0", "job-annotated-0"},
	}
	for name, roots := range map[string]graph.StorageRoots{
		"node root":        k8sRootsOf(t, graph.StorageRootNode, "worker-1"),
		"podless node":     k8sRootsOf(t, graph.StorageRootNode, "worker-3"),
		"pod root":         k8sRootsOf(t, graph.StorageRootPod, "shop/orders-0"),
		"unmounting pod":   k8sRootsOf(t, graph.StorageRootPod, "shop/web-0"),
		"pods in two ns":   k8sRootsOf(t, graph.StorageRootPod, "shop/orders-0", "platform/redis-0"),
		"unscheduled root": k8sRootsOf(t, graph.StorageRootPod, "shop/unscheduled-0"),
	} {
		t.Run(name, func(t *testing.T) {
			opts := Options{AppMetadata: &src}
			fx := planEstate()
			fx[promql.QAppMetadata] = model.Vector{appMetaRow("shop", "orders-0", "shop-v2", "team-a", "alice")}

			storageQ := promqlfake.New(fx)
			_, err := New(storageQ, opts, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, roots)
			require.NoError(t, err)

			_, k8sQ := buildK8sOver(t, fx, opts, roots)

			want := kubernetesFanOut(storageQ)
			require.NotEmpty(t, want)
			if roots.Kind == graph.StorageRootPod || len(claimlessOnNode[name]) == 0 {
				assert.Equal(t, want, kubernetesFanOut(k8sQ), "the Kubernetes queries are the storage build's own")
			} else {
				// More pods mean more controller kinds to resolve, so the k8s
				// build may read families the storage build had no pod for.
				assert.Subset(t, queryNames(k8sQ), queryNames(storageQ))
				// kube_pod_owner is read by the pod wave alone, so its pod scope
				// is the set of pods the build loaded.
				storagePods := flatten(storageQ.ScopeValues(promql.QPodOwner, "pod"))
				k8sPods := flatten(k8sQ.ScopeValues(promql.QPodOwner, "pod"))
				for _, pod := range claimlessOnNode[name] {
					assert.NotContains(t, storagePods, pod, "the storage build never loads %s", pod)
					assert.Contains(t, k8sPods, pod, "the k8s build loads %s", pod)
				}
			}

			for _, is := range k8sQ.Issued() {
				assert.False(t, harvestFamily(is.Name), "%s: no Harvest query", is.Name)
			}
			seen := map[string]bool{}
			for _, is := range k8sQ.Issued() {
				seen[is.Name] = true
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
		})
	}
}

// Spec: "Kubernetes-only element set" — Harvest, service-graph and container
// series upstream do not leak into the body.
func TestBuildK8s_HoldsOnlyKubernetesElements(t *testing.T) {
	g, _ := buildK8sOver(t, planEstate(), Options{}, k8sRootsOf(t, graph.StorageRootNode, "worker-2"))

	require.NotEmpty(t, g.Edges)
	for _, e := range g.Edges {
		assert.Contains(t, []graph.EdgeType{graph.EdgeTypePodToNode, graph.EdgeTypePodMountsPVC}, e.Type)
	}
	for id, n := range g.NodesByID {
		assert.Contains(t, []graph.NodeType{graph.NodeTypePod, graph.NodeTypeK8sNode, graph.NodeTypePVC}, n.Type(), id)
		assert.Empty(t, n.Containers(), "%s carries no container list", id)
		assert.Nil(t, n.QoS(), "%s carries no qos ceiling", id)
		assert.NotContains(t, n.Labels(), "aggr", id)
		assert.NotContains(t, n.Labels(), "svm", id)
	}
	claim := g.NodesByID[graph.PVCID(k8sCluster, "shop", "orders-data")]
	require.NotNil(t, claim)
	assert.Equal(t, "netapp-nas", claim.StorageClass())
	require.NotNil(t, claim.Usage())
}

// Spec: "Kubernetes attributes and grouping match the storage graph".
func TestBuildK8s_Attributes(t *testing.T) {
	src := defaultAppMetadata()
	fx := planEstate()
	fx[promql.QAppMetadata] = model.Vector{
		appMetaRow("shop", "orders-0", "shop-v2", "team-a", "alice"),
		appMetaRow("shop", "orders-1", "shop-v2", "team-a", "alice"),
	}
	// Both nodes are roots, so every pod on worker-1 and worker-2 is drawn,
	// claim-mounting or not.
	g, _ := buildK8sOver(t, fx, Options{AppMetadata: &src}, k8sRootsOf(t, graph.StorageRootNode, "worker-1", "worker-2"))
	byID := func(id string) graph.GraphNode {
		n := g.NodesByID[id]
		require.NotNil(t, n, id)
		return n
	}
	uid := func(u string) string { return graph.PodID(k8sCluster, u) }

	t.Run("owner and application via Deployment", func(t *testing.T) {
		pod := byID(uid("uid-w0"))
		require.NotNil(t, pod.Owner())
		assert.Equal(t, graph.Owner{Kind: "Deployment", Name: "web"}, *pod.Owner())
		assert.Equal(t, "storefront", pod.Application())
	})
	t.Run("job pod resolves through its CronJob", func(t *testing.T) {
		pod := byID(uid("uid-jc0"))
		require.NotNil(t, pod.Owner())
		assert.Equal(t, graph.Owner{Kind: "Job", Name: "nightly-28901"}, *pod.Owner())
		assert.Equal(t, "reports", pod.Application())
	})
	t.Run("metric wins over the controller chain", func(t *testing.T) {
		assert.Equal(t, "shop-v2", byID(uid("uid-o0")).Application())
	})
	t.Run("node attributes", func(t *testing.T) {
		node := byID(graph.K8sNodeID(k8sCluster, "worker-1"))
		assert.Equal(t, "Ready", node.ReadyStatus())
		assert.Equal(t, []string{"192.168.0.1"}, node.IPAddress())
	})
	t.Run("alerts and status", func(t *testing.T) {
		pod := byID(uid("uid-o0"))
		assert.NotEmpty(t, pod.Alerts())
		assert.Equal(t, graph.StatusWarning, pod.Status())
		for id, n := range g.NodesByID {
			assert.NotEmpty(t, n.Status(), "%s carries data.status", id)
		}
	})
	t.Run("group metadata", func(t *testing.T) {
		assert.Equal(t, "team-a", g.NamespaceOwners[graph.NamespaceRef{Cluster: k8sCluster, Namespace: "shop"}])
		assert.Equal(t, "alice", g.ApplicationDeployers[graph.ApplicationRef{Cluster: k8sCluster, Namespace: "shop", Application: "shop-v2"}])
	})
}

// Spec: "Claim inherits from every mounter, drawn or not". A pod root beta-rwx-0
// loads shared-beta's other mounter (alpha-share-0, on worker-2) by mounter
// completion, so the claim inherits the lexically-smallest of the two.
func TestBuildK8s_ClaimInheritsFromUndrawnMounter(t *testing.T) {
	g, _ := buildK8sOver(t, planEstate(), Options{}, k8sRootsOf(t, graph.StorageRootPod, "shop/beta-rwx-0"))
	claim := g.NodesByID[graph.PVCID(k8sCluster, "shop", "shared-beta")]
	require.NotNil(t, claim)
	assert.Equal(t, "alpha", claim.Application(), "alpha-share-0 is not a root, yet decides the claim's Application")
}

// Spec: "Fails closed on any upstream error except ALERTS", and "No NetApp
// coverage warning".
func TestBuildK8s_FailsClosed(t *testing.T) {
	src := defaultAppMetadata()
	roots := k8sRootsOf(t, graph.StorageRootPod, "shop/orders-0", "shop/web-0")
	fx := planEstate()
	fx[promql.QAppMetadata] = model.Vector{appMetaRow("shop", "orders-0", "shop-v2", "team-a", "alice")}

	for _, failing := range []promql.Query{
		promql.QKubeletVolumeUsedBytes,
		promql.QReplicaSetAnnotations,
		promql.QAppMetadata,
		promql.QPodInfo,
		promql.QNodeInfo,
	} {
		t.Run(string(failing), func(t *testing.T) {
			q := promqlfake.New(fx)
			q.Fail = func(name, _ string) error {
				if name == string(failing) {
					return errors.New(`Post "http://vm-internal:8428/api/v1/query": 503`)
				}
				return nil
			}
			_, err := New(q, Options{AppMetadata: &src}, nil, nil).BuildK8s(t.Context(), time.Minute, vlrEnd, vlrSel, roots)
			require.Error(t, err)
			be, ok := errors.AsType[*Error](err)
			require.True(t, ok)
			assert.Equal(t, ReasonUpstream, be.Reason)
			assert.Equal(t, string(failing), be.Query)
		})
	}

	t.Run("ALERTS degrades", func(t *testing.T) {
		q := promqlfake.New(fx)
		q.Fail = func(name, _ string) error {
			if name == string(promql.QAlerts) {
				return errors.New("alert store down")
			}
			return nil
		}
		g, err := New(q, Options{AppMetadata: &src}, nil, nil).BuildK8s(t.Context(), time.Minute, vlrEnd, vlrSel, roots)
		require.NoError(t, err)
		require.NotEmpty(t, g.NodesByID)
		for id, n := range g.NodesByID {
			assert.Empty(t, n.Alerts(), id)
		}
	})

	t.Run("empty families do not fail", func(t *testing.T) {
		g, _ := buildK8sOver(t, nil, Options{}, roots)
		assert.Empty(t, g.NodesByID)
	})

	t.Run("no NetApp coverage warning", func(t *testing.T) {
		buf := testlog.CaptureLevel(t, slog.LevelDebug)
		g, _ := buildK8sOver(t, fx, Options{AppMetadata: &src}, roots)
		require.NotEmpty(t, g.NodesByID)
		assert.NotContains(t, buf.String(), "netapp_volume_join_miss")
		assert.NotContains(t, buf.String(), "netapp_qos_join_miss")
	})
}

// Spec: "Bounded root sets" and the root-kind contract of the build itself.
func TestBuildK8s_RejectsBeforeAnyQuery(t *testing.T) {
	tooMany := make([]string, maxRootedVolumeLabelChunks+1)
	for i := range tooMany {
		tooMany[i] = "n" + strings.Repeat("x", 8) + itoa(i)
	}
	cases := map[string]graph.StorageRoots{
		"too many node roots":   k8sRootsOf(t, graph.StorageRootNode, tooMany...),
		"storage-side kind":     k8sRootsOf(t, graph.StorageRootAggr, "aggr1"),
		"application kind":      k8sRootsOf(t, graph.StorageRootApplication, "shop"),
		"claim kind":            k8sRootsOf(t, graph.StorageRootPVC, "shop/orders-data"),
		"node kind, no values":  {Kind: graph.StorageRootNode},
		"pod kind, no values":   {Kind: graph.StorageRootPod},
		"pod with no namespace": {Kind: graph.StorageRootPod, Pods: []graph.PodRef{{Name: "orders-0"}}},
		"valid pod beside one with no namespace": {Kind: graph.StorageRootPod,
			Pods: []graph.PodRef{{Namespace: "shop", Name: "orders-0"}, {Name: "web-0"}}},
		"pod with no name": {Kind: graph.StorageRootPod, Pods: []graph.PodRef{{Namespace: "shop"}}},
		"no kind":          {},
	}
	for name, roots := range cases {
		t.Run(name, func(t *testing.T) {
			q := promqlfake.New(planEstate())
			_, err := New(q, Options{QoSScopeBatchBytes: 1}, nil, nil).BuildK8s(t.Context(), time.Minute, vlrEnd, vlrSel, roots)
			require.Equal(t, ReasonInvalidScope, AsReason(err))
			assert.Empty(t, q.Issued())
		})
	}
}

// Spec: "Zone routing and matchers" — a zone-a request never reaches zone-b's
// store, and every Kubernetes query carries the matchers its family accepts.
func TestBuildK8s_StaysInTheRequestZone(t *testing.T) {
	k8sA, k8sB, netappA, netappB := hubZonesStores()
	f := newHubZonesFixtureFrom(t, k8sA, k8sB, netappA, netappB)
	sel := promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Cluster: []string{"c1"}, Namespace: []string{"shop"}}

	g, err := New(f.router, Options{}, nil, nil).BuildK8s(t.Context(), time.Minute, vlrEnd, sel,
		k8sRootsOf(t, graph.StorageRootNode, "worker-1"))
	require.NoError(t, err)
	require.NotEmpty(t, g.NodesByID)

	assert.Empty(t, f.k8sB.Issued(), "no query reaches zone-b's ksm store")
	assert.Empty(t, f.netappA.Issued(), "no Harvest query is issued at all")
	assert.Empty(t, f.netappB.Issued())
	require.NotEmpty(t, f.k8sA.Issued())
	for _, is := range f.k8sA.Issued() {
		assert.Contains(t, is.Query, `az="zone-a",env="prod"`, is.Name)
		switch promql.Query(is.Name) {
		case promql.QAlerts:
			assert.Contains(t, is.Query, `namespace=~"shop|"`)
			assert.NotContains(t, is.Query, `cluster=`, "ALERTS accepts no cluster matcher")
		default:
			if fam, _ := promql.FamilyOf(promql.Query(is.Name)); fam == promql.FamilyKSM {
				assert.Contains(t, is.Query, `cluster="c1"`, is.Name)
			}
		}
	}
}

// Spec: "Node root". Everything that runs on the root node is loaded — pods
// that mount no claim included — while a storage build of the same root keeps
// only the claim-mounting ones.
func TestBuildK8s_NodeRootLoadsClaimlessPods(t *testing.T) {
	roots := k8sRootsOf(t, graph.StorageRootNode, "worker-1")
	g, _ := buildK8sOver(t, planEstate(), Options{}, roots)
	for _, uid := range []string{"uid-w0", "uid-d0", "uid-oe0", "uid-ja0", "uid-o0"} {
		assert.NotNil(t, g.NodesByID[graph.PodID(k8sCluster, uid)], uid)
	}
	assert.Nil(t, g.NodesByID[graph.PodID(k8sCluster, "uid-us0")], "the unscheduled pod runs on no node")

	storageG, err := New(promqlfake.New(planEstate()), Options{}, nil, nil).
		BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, roots)
	require.NoError(t, err)
	assert.Nil(t, storageG.NodesByID[graph.PodID(k8sCluster, "uid-w0")], "the storage build is unchanged")
}

// The querier a k8s build reads through refuses every Harvest query before it
// reaches a store, passes everything else, and keeps FamilyServer exactly when
// the wrapped querier has it.
func TestRefuseHarvest(t *testing.T) {
	inner := promqlfake.New(planEstate())
	q := refuseHarvest(inner)

	_, err := q.Instant(t.Context(), string(promql.QVolumeLabels), "volume_labels", vlrEnd)
	require.ErrorContains(t, err, "harvest query volume_labels")
	assert.Empty(t, inner.Issued(), "a refused query never reaches the store")

	_, err = q.Instant(t.Context(), string(promql.QPodInfo), "kube_pod_info", vlrEnd)
	require.NoError(t, err)

	_, ok := q.(promql.FamilyServer)
	assert.False(t, ok, "no FamilyServer is invented")
	fs, ok := refuseHarvest(appMetaUnserved{inner}).(promql.FamilyServer)
	require.True(t, ok, "the wrapped querier's FamilyServer is kept")
	assert.False(t, fs.Serves(promql.FamilyAppMeta))
}

// An unserved appmeta family is not read: the Harvest guard keeps the
// FamilyServer readTopology asks.
func TestBuildK8s_UnservedAppMetadataIsNotRead(t *testing.T) {
	src := defaultAppMetadata()
	fx := planEstate()
	fx[promql.QAppMetadata] = model.Vector{appMetaRow("shop", "orders-0", "shop-v2", "team-a", "alice")}
	inner := promqlfake.New(fx)
	_, err := New(appMetaUnserved{inner}, Options{AppMetadata: &src}, nil, nil).
		BuildK8s(t.Context(), time.Minute, vlrEnd, vlrSel, k8sRootsOf(t, graph.StorageRootNode, "worker-1"))
	require.NoError(t, err)
	assert.Empty(t, inner.QueriesFor(promql.QAppMetadata))
}

// k8sSkippedLegs is the storage skip set plus exactly the Harvest first-wave
// legs, so a Harvest leg added to topologyLegs is skipped without an edit.
func TestK8sSkippedLegs_AreTheStorageSkipsPlusEveryHarvestLeg(t *testing.T) {
	want := maps.Clone(storageSkippedLegs)
	var harvest int
	for _, l := range topologyLegs(&topologyVectors{}, nil) {
		if harvestFamily(string(l.query)) {
			want[l.query] = true
			harvest++
		}
	}
	require.NotZero(t, harvest)
	assert.Equal(t, want, k8sSkippedLegs)
	assert.True(t, k8sSkippedLegs[promql.QVolumeLabels])
	assert.True(t, k8sSkippedLegs[promql.QNetAppNodeTotalData])
}

// A node root's pod wave is keyed by cluster identity, not by (namespace, pod)
// alone. A same-named pod of another selected cluster, running on another
// node, is returned by the per-namespace read but never loaded, so neither its
// controller nor its cluster reaches the graph. And kube_pod_info is not read a
// second time for the root-node pods: the incarnation completion's rows are
// the pod wave's own.
func TestBuildK8s_NodeRootPodWaveIsClusterKeyed(t *testing.T) {
	fx := planEstate()
	fx[promql.QPodInfo] = append(slices.Clone(fx[promql.QPodInfo]),
		planKSM("cluster", "c2", "namespace", "shop", "pod", "web-0", "uid", "uid-c2-w0", "node", "worker-9"))
	fx[promql.QPodOwner] = append(slices.Clone(fx[promql.QPodOwner]),
		planKSM("cluster", "c2", "namespace", "shop", "pod", "web-0",
			"owner_kind", "ReplicaSet", "owner_name", "c2-web-rs", "owner_is_controller", "true"))

	g, q := buildK8sOver(t, fx, Options{}, k8sRootsOf(t, graph.StorageRootNode, "worker-1"))

	require.NotNil(t, g.NodesByID[graph.PodID(k8sCluster, "uid-w0")], "c1's web-0 runs on the root")
	assert.Nil(t, g.NodesByID[graph.PodID("zone-a-prod-c2", "uid-c2-w0")], "c2's web-0 is not loaded")
	assert.NotContains(t, g.ClusterIdentities, "zone-a-prod-c2", "no c2 row reaches the parse")
	assert.NotContains(t, flatten(q.ScopeValues(promql.QReplicaSetOwner, "replicaset")), "c2-web-rs",
		"the controller wave never scopes from c2's pod")

	// web-0 mounts nothing, so only incarnation completion names it.
	var reads int
	for _, scope := range q.ScopeValues(promql.QPodInfo, "pod") {
		if slices.Contains(scope, "web-0") {
			reads++
		}
	}
	assert.Equal(t, 1, reads, "kube_pod_info is read once for a root-node pod")
}

// Spec: "Output preservation". Reading by reference changes what is fetched,
// never what is drawn: for every root shape the body equals the one projected
// from an UNSCOPED Kubernetes read of the whole zone — the same topology
// fan-out with no root, no Harvest and no container list — which is what a
// by-reference scope is an optimisation of.
func TestBuildK8s_PlanIsOutputPreserving(t *testing.T) {
	src := defaultAppMetadata()
	cases := map[string]struct {
		scope      func() graph.StorageScope
		namespaces []string
		appMeta    bool
	}{
		"node root with claimful and claimless pods": {scope: func() graph.StorageScope {
			return vlrScope(t, graph.StorageRootNode, []string{"worker-1"})
		}},
		"node root, second node": {scope: func() graph.StorageScope {
			return vlrScope(t, graph.StorageRootNode, []string{"worker-2"})
		}},
		"two node roots": {scope: func() graph.StorageScope {
			return vlrScope(t, graph.StorageRootNode, []string{"worker-1", "worker-2"})
		}},
		"podless node root": {scope: func() graph.StorageScope {
			return vlrScope(t, graph.StorageRootNode, []string{"worker-3"})
		}},
		"claimless pod root": {scope: func() graph.StorageScope {
			return vlrScope(t, graph.StorageRootPod, []string{"shop/web-0"})
		}},
		"mounting pod root": {scope: func() graph.StorageScope {
			return vlrScope(t, graph.StorageRootPod, []string{"shop/orders-0"})
		}},
		"unscheduled pod root": {scope: func() graph.StorageScope {
			return vlrScope(t, graph.StorageRootPod, []string{"shop/unscheduled-0"})
		}},
		"job pod resolved through its cronjob": {scope: func() graph.StorageScope {
			return vlrScope(t, graph.StorageRootPod, []string{"shop/job-cronjob-0"})
		}},
		"pod roots in two namespaces": {scope: func() graph.StorageScope {
			return vlrScope(t, graph.StorageRootPod, []string{"shop/orders-0", "platform/redis-0"})
		}},
		"shared claim with an undrawn mounter": {scope: func() graph.StorageScope {
			return vlrScope(t, graph.StorageRootPod, []string{"shop/beta-rwx-0"})
		}},
		"namespace filter on a node root": {namespaces: []string{"shop"}, scope: func() graph.StorageScope {
			scope, err := graph.NewStorageScope(nil, []string{"shop"}, graph.StorageRootNode, []string{"worker-1"})
			require.NoError(t, err)
			return scope
		}},
		"node root with the application-metadata metric": {appMeta: true, scope: func() graph.StorageScope {
			return vlrScope(t, graph.StorageRootNode, []string{"worker-1"})
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			scope := tc.scope()
			sel := promql.Selector{AZ: []string{"zone-a"}, Env: []string{"prod"}, Namespace: tc.namespaces}
			opts := Options{}
			fx := planEstate()
			skip := maps.Clone(k8sSkippedLegs)
			if tc.appMeta {
				opts.AppMetadata = &src
				fx[promql.QAppMetadata] = model.Vector{
					appMetaRow("shop", "orders-0", "shop-v2", "team-a", "alice"),
					appMetaRow("shop", "web-0", "storefront-v2", "team-a", "bob"),
					appMetaRow("platform", "redis-0", "redis-app", "team-p", "carol"),
				}
				// The reference reads the metric whole, in the first wave.
				delete(skip, promql.QAppMetadata)
			}

			b := New(promqlfake.New(fx), opts, nil, nil)
			scoped, err := b.BuildK8s(t.Context(), time.Minute, vlrEnd, sel, scope.Roots)
			require.NoError(t, err)

			plan := topologyPlan{skip: skip, failClosed: true}
			refTopology, err := readTopology(t.Context(), promqlfake.New(fx), time.Minute, vlrEnd, opts.withAppMetadataDefaults(), sel, plan)
			require.NoError(t, err)
			nodes, edges := assemble(refTopology, ServiceGraphResult{})
			reference := b.freeze(t.Context(), refTopology, nodes, edges)

			scopedBody := cytoscape.Serialise(scoped, graph.ProjectK8s(scoped, scope))
			referenceBody := cytoscape.Serialise(reference, graph.ProjectK8s(reference, scope))
			require.NotEmpty(t, scopedBody.Elements.Nodes, "a vacuous body would prove nothing")
			assert.JSONEq(t, planBodyJSON(t, referenceBody), planBodyJSON(t, scopedBody))
		})
	}
}
