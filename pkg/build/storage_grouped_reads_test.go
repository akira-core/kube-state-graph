package build

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// manyNamespaceEstate is one Kubernetes node running a mounting pod in each of
// n namespaces — the shape of a shared node or a large filer root, where the
// storage build's reads of known pods and claims span hundreds of namespaces.
// Every pod and claim is named the same in every namespace, so a read that
// lost the namespace half of a pair would load another namespace's object.
func manyNamespaceEstate(n int) map[promql.Query]model.Vector {
	fx := map[promql.Query]model.Vector{}
	add := func(q promql.Query, s *model.Sample) { fx[q] = append(fx[q], s) }
	add(promql.QNodeInfo, planKSM("node", "worker-1"))
	for i := range n {
		ns := fmt.Sprintf("tenant-%03d", i)
		uid := fmt.Sprintf("uid-%03d", i)
		pv := fmt.Sprintf("pvc-%03d", i)
		add(promql.QPodInfo, planKSM("namespace", ns, "pod", "db-0", "uid", uid, "node", "worker-1"))
		add(promql.QPodOwner, planKSM("namespace", ns, "pod", "db-0",
			"owner_kind", "StatefulSet", "owner_name", "db", "owner_is_controller", "true"))
		add(promql.QPVCBindings, planKSM("namespace", ns, "pod", "db-0", "persistentvolumeclaim", "data"))
		add(promql.QPVCInfo, planKSM("namespace", ns, "persistentvolumeclaim", "data", "volumename", pv))
		add(promql.QKubeletVolumeUsedBytes, withValue(planKSM("namespace", ns, "persistentvolumeclaim", "data"), 10))
		add(promql.QKubeletVolumeCapacityBytes, withValue(planKSM("namespace", ns, "persistentvolumeclaim", "data"), 100))
	}
	return fx
}

// P-8: a storage build reads its known pods and claims with one grouped
// restriction per chunk — a namespace alternation beside a name alternation —
// not one query per namespace. A node root whose pods span 40 namespaces
// issues each pod- and claim-keyed family in a single chunk (a per-namespace
// read issued 281 such queries); one spanning 250 namespaces splits each
// family's namespace alternation at 100 values into 3 chunks, merged in chunk
// order, and still builds every pod and claim.
func TestBuildStorage_ManyNamespaceRootGroupsItsReads(t *testing.T) {
	for _, tc := range []struct{ namespaces, chunks int }{
		{namespaces: 40, chunks: 1},
		{namespaces: 250, chunks: 3},
	} {
		t.Run(fmt.Sprintf("namespaces=%d", tc.namespaces), func(t *testing.T) {
			q := promqlfake.New(manyNamespaceEstate(tc.namespaces))
			scope := vlrScope(t, graph.StorageRootNode, []string{"worker-1"})

			g, err := New(q, Options{}, nil, nil).BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, scope.Roots)
			require.NoError(t, err)

			counts := map[promql.Query]int{}
			for _, is := range q.Issued() {
				counts[promql.Query(is.Name)]++
			}
			// kube_pod_info: the node seed, then incarnation completion and the
			// pod wave, each one query per chunk.
			assert.Equal(t, 1+2*tc.chunks, counts[promql.QPodInfo], "seed + incarnation + pod wave")
			assert.Equal(t, tc.chunks, counts[promql.QPodOwner])
			// Bindings: the seed's by-pod read and mounter completion's
			// by-claim read.
			assert.Equal(t, 2*tc.chunks, counts[promql.QPVCBindings])
			for _, fam := range []promql.Query{
				promql.QPVCInfo, promql.QPVCAnnotations,
				promql.QKubeletVolumeUsedBytes, promql.QKubeletVolumeCapacityBytes,
			} {
				assert.Equal(t, tc.chunks, counts[fam], fam)
			}
			for _, nss := range q.ScopeValues(promql.QPodOwner, "namespace") {
				assert.LessOrEqual(t, len(nss), promql.MaxAlternationValues,
					"each namespace alternation stays at the upstream's lookup limit")
			}

			// Every namespace's pod and claim is still built.
			for i := range tc.namespaces {
				ns := fmt.Sprintf("tenant-%03d", i)
				assert.Contains(t, g.NodesByID, graph.PodID("zone-a-prod-c1", fmt.Sprintf("uid-%03d", i)), "pod of %s", ns)
				assert.Contains(t, g.NodesByID, graph.PVCID("zone-a-prod-c1", ns, "data"), "claim of %s", ns)
			}
			assert.Len(t, g.NodesByID, 2*tc.namespaces+1, "the pods, their claims and the root node")
		})
	}
}

// A grouped read's two alternations are independent, so it can return a
// (namespace, name) pair nobody asked for. Such a row must never reach a
// slot: here `tenant-b/db-0` exists and binds a claim, but only `tenant-a/db-0`
// and `tenant-b/web-0` are known pods, so the grouped read
// `namespace=~"tenant-a|tenant-b",pod=~"db-0|web-0"` returns it.
func TestIssuePodFamilies_DropsCrossPairs(t *testing.T) {
	fx := map[promql.Query]model.Vector{
		promql.QPodInfo: {
			planKSM("namespace", "tenant-a", "pod", "db-0", "uid", "a-db"),
			planKSM("namespace", "tenant-b", "pod", "web-0", "uid", "b-web"),
			planKSM("namespace", "tenant-b", "pod", "db-0", "uid", "b-db"),
		},
	}
	q := promqlfake.New(fx)
	var v topologyVectors
	var mu sync.Mutex
	out, err := queryPodsByPair(t.Context(), q, time.Minute, vlrEnd, Options{}, vlrSel, &v, &mu, promql.QPodInfo,
		[]graph.PodRef{{Namespace: "tenant-a", Name: "db-0"}, {Namespace: "tenant-b", Name: "web-0"}})
	require.NoError(t, err)

	got := make([]string, 0, len(out))
	for _, s := range out {
		got = append(got, string(s.Metric["uid"]))
	}
	assert.ElementsMatch(t, []string{"a-db", "b-web"}, got, "the cross pair tenant-b/db-0 is dropped")
	require.Len(t, q.QueriesFor(promql.QPodInfo), 1, "one grouped query for both namespaces")
	assert.Equal(t, 1, v.ExtraSeriesCount[promql.QPodInfo], "the dropped row is still tallied as read")
}
