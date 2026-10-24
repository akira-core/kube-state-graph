package build

import (
	"math/rand/v2"
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
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// unscopedVectors fills a topologyVectors the way /v1/graph's first wave does:
// every topology leg once, unrestricted, over a fake of fx.
func unscopedVectors(t *testing.T, fx map[promql.Query]model.Vector, opts Options) topologyVectors {
	t.Helper()
	opts = opts.withAppMetadataDefaults()
	var v topologyVectors
	v.VolumeKey = opts.volumeKey()
	v.AppMetadataSource = opts.AppMetadata
	q := promqlfake.New(fx)
	for _, l := range topologyLegs(&v, opts.AppMetadata) {
		out, err := q.Instant(t.Context(), string(l.query),
			renderTopologyQuery(l.query, opts.AppMetadata, time.Minute, opts.LabelKeys, promql.Selector{}), vlrEnd)
		require.NoError(t, err)
		*l.dst = out
	}
	return v
}

// reorderedSlots are the topologyVectors slots a Kubernetes-only build fills
// from SEVERAL reads — a root read, then a co-mounter delta, then a controller
// fallback — so the order rows reach a slot in is not the order a single
// unrestricted read returns them in. Each carries the grain at which that order
// can differ: a pod, a claim, or a bare controller / node name, because the
// last two are read by name alone and a name lands in one chunk of one round.
func reorderedSlots(v *topologyVectors) map[string]struct {
	rows *model.Vector
	key  func(model.Metric) string
} {
	byPod := func(m model.Metric) string { return string(m["namespace"]) + "/" + string(m[promql.PodLabel]) }
	byClaim := func(m model.Metric) string { return string(m["namespace"]) + "/" + bindingClaim(m) }
	byName := func(label string) func(model.Metric) string {
		return func(m model.Metric) string { return string(m[model.LabelName(label)]) }
	}
	type slot = struct {
		rows *model.Vector
		key  func(model.Metric) string
	}
	return map[string]slot{
		"kube_pod_info":                         {&v.Pod, byPod},
		"kube_pod_owner":                        {&v.PodOwner, byPod},
		"claim bindings":                        {&v.PVC, byClaim},
		"kube_persistentvolumeclaim_info":       {&v.PVCInfo, byClaim},
		"kube_persistentvolumeclaim_annotation": {&v.PVCAnnotations, byClaim},
		"kubelet used":                          {&v.KubeletVolumeUsed, byClaim},
		"kubelet capacity":                      {&v.KubeletVolumeCapacity, byClaim},
		"kube_node_info":                        {&v.Node, byName("node")},
		"kube_node_status_addresses":            {&v.Addr, byName("node")},
		"kube_node_labels":                      {&v.NodeLabels, byName("node")},
		"kube_node_status_condition":            {&v.NodeStatus, byName("node")},
		"kube_replicaset_owner":                 {&v.ReplicaSetOwner, byName("replicaset")},
		"kube_replicaset_annotations":           {&v.ReplicaSetAnnotations, byName("replicaset")},
		"kube_job_owner":                        {&v.JobOwner, byName("job_name")},
		"kube_job_annotations":                  {&v.JobAnnotations, byName("job_name")},
		"kube_deployment_annotations":           {&v.DeploymentAnnotations, byName("deployment")},
		"kube_statefulset_annotations":          {&v.StatefulSetAnnotations, byName("statefulset")},
		"kube_daemonset_annotations":            {&v.DaemonSetAnnotations, byName("daemonset")},
		"kube_cronjob_annotations":              {&v.CronJobAnnotations, byName("cronjob")},
		"app_metadata":                          {&v.AppMetadata, byName("namespace")},
	}
}

// shuffleAcrossKeys groups rows by key, keeps each group's rows in their input
// order, and returns the groups in a random order. It never mutates rows.
func shuffleAcrossKeys(rows model.Vector, key func(model.Metric) string, rng *rand.Rand) model.Vector {
	var order []string
	groups := map[string]model.Vector{}
	for _, s := range rows {
		k := key(s.Metric)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], s)
	}
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	out := make(model.Vector, 0, len(rows))
	for _, k := range order {
		out = append(out, groups[k]...)
	}
	return out
}

// A Kubernetes-only build appends rows to a slot in round order, so the parse
// must not depend on the order of rows of DIFFERENT keys. Shuffling every such
// slot across its keys, with each key's own rows kept in order (they come from
// one query), must not move a byte of the body.
func TestParseTopology_IsOrderInsensitiveAcrossKeys(t *testing.T) {
	src := defaultAppMetadata()
	opts := Options{AppMetadata: &src}
	fx := planEstate()
	fx[promql.QAppMetadata] = model.Vector{
		appMetaRow("shop", "orders-0", "shop-v2", "team-a", "alice"),
		appMetaRow("shop", "web-0", "storefront-v2", "team-a", "bob"),
		appMetaRow("platform", "redis-0", "redis-app", "team-p", "carol"),
	}
	b := New(promqlfake.New(fx), opts, nil, nil)

	body := func(v topologyVectors) string {
		topology := parseTopology(v, opts.LabelKeys)
		nodes, edges := assemble(topology, ServiceGraphResult{})
		g := b.freeze(t.Context(), topology, nodes, edges)
		return planBodyJSON(t, cytoscape.Serialise(g, graph.Project(g, graph.Scope{Inventory: true})))
	}

	base := unscopedVectors(t, fx, opts)
	reference := body(base)
	require.Contains(t, reference, `"storefront-v2"`, "a vacuous body would prove nothing")

	for seed := uint64(1); seed <= 12; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*7919))
		shuffled := base
		var moved []string
		for name, slot := range reorderedSlots(&shuffled) {
			before := slices.Clone(*slot.rows)
			*slot.rows = shuffleAcrossKeys(before, slot.key, rng)
			if !slices.Equal(before, *slot.rows) {
				moved = append(moved, name)
			}
		}
		if seed == 1 {
			assert.Greater(t, len(moved), 5, "the shuffle must actually reorder slots: %s", strings.Join(moved, ", "))
		}
		if got := body(shuffled); got != reference {
			at := firstDifference(reference, got)
			t.Errorf("seed %d: body differs at byte %d:\n want ...%s\n  got ...%s", seed, at,
				excerpt(reference, at), excerpt(got, at))
		}
	}
}

// firstDifference is the index of the first byte at which a and b differ.
func firstDifference(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// excerpt is a short window of s around at, for a failure message that would
// otherwise print a whole body.
func excerpt(s string, at int) string {
	return s[max(at-80, 0):min(at+120, len(s))]
}
