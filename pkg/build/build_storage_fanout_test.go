package build

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/internal/promqlfake"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

var updateFanOut = flag.Bool("update-storage-fanout", false, "rewrite pkg/build/testdata/storage_fanout.golden")

const storageFanOutGolden = "testdata/storage_fanout.golden"

// allFanOut is the sorted multiset of every rendered query a fake answered,
// Harvest included, as `name|query` lines. Arrival order is a property of
// goroutine scheduling, so it is deliberately not part of the pin.
func allFanOut(q *promqlfake.Querier) []string {
	issued := q.Issued()
	out := make([]string, 0, len(issued))
	for _, is := range issued {
		out = append(out, is.Name+"|"+is.Query)
	}
	slices.Sort(out)
	return out
}

// TestBuildStorage_FanOutUnchanged pins the /v1/storage-graph fan-out for pod
// and node roots: every query a build issues over planEstate(), its rendered
// string included, Harvest and the application-metadata read among them. The
// Kubernetes-only read plan is allowed to group, reorder and read ahead;
// the storage plan is not, so any edit that moves one of these lines is an
// edit to /v1/storage-graph and has to be a deliberate one (design D1 of
// shorten-by-reference-read-path). The flag is not the repo's routine `-update`,
// so a golden refresh elsewhere cannot rewrite this guard; refresh with
// `go test ./pkg/build -run TestBuildStorage_FanOutUnchanged -update-storage-fanout`
// only for an INTENDED storage change.
func TestBuildStorage_FanOutUnchanged(t *testing.T) {
	src := defaultAppMetadata()
	cases := []struct {
		name  string
		kind  graph.StorageRootKind
		roots []string
	}{
		{"node root, claimful and claimless pods", graph.StorageRootNode, []string{"worker-1"}},
		{"node root, second node", graph.StorageRootNode, []string{"worker-2"}},
		{"two node roots", graph.StorageRootNode, []string{"worker-1", "worker-2"}},
		{"podless node root", graph.StorageRootNode, []string{"worker-3"}},
		{"mounting pod root", graph.StorageRootPod, []string{"shop/orders-0"}},
		{"claimless pod root", graph.StorageRootPod, []string{"shop/web-0"}},
		{"pod roots in two namespaces", graph.StorageRootPod, []string{"shop/orders-0", "platform/redis-0"}},
		{"shared claim with an undrawn mounter", graph.StorageRootPod, []string{"shop/beta-rwx-0"}},
		{"job resolved through its cronjob", graph.StorageRootPod, []string{"shop/job-cronjob-0"}},
	}

	var sb strings.Builder
	for _, tc := range cases {
		fx := planEstate()
		fx[promql.QAppMetadata] = model.Vector{
			appMetaRow("shop", "orders-0", "shop-v2", "team-a", "alice"),
			appMetaRow("platform", "redis-0", "redis-app", "team-p", "carol"),
		}
		q := promqlfake.New(fx)
		_, err := New(q, Options{AppMetadata: &src}, nil, nil).
			BuildStorage(t.Context(), time.Minute, vlrEnd, vlrSel, vlrScope(t, tc.kind, tc.roots).Roots)
		require.NoError(t, err, tc.name)

		sb.WriteString("## " + tc.name + "\n")
		for _, line := range allFanOut(q) {
			sb.WriteString(line + "\n")
		}
		sb.WriteString("\n")
	}

	if *updateFanOut {
		require.NoError(t, os.MkdirAll(filepath.Dir(storageFanOutGolden), 0o755))
		require.NoError(t, os.WriteFile(storageFanOutGolden, []byte(sb.String()), 0o644))
		return
	}
	want, err := os.ReadFile(storageFanOutGolden)
	require.NoError(t, err, "run with -update-storage-fanout to create the golden file")
	require.Equal(t, string(want), sb.String())
}
