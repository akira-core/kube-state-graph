package build

import (
	"log/slog"
	"maps"
	"slices"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// clusterResolver turns a series' raw `cluster` label into the cluster IDENTITY
// every cluster-scoped structure is keyed on.
//
// A Kubernetes cluster is identified by `<az>-<env>-<cluster>`: the raw name is
// reused across zones and environments (`c1` in us/dev and again in eu/prod), so
// keying on it alone merges two unrelated clusters into one id space — one
// `c1/worker-0` node, one `cluster/c1` compound group, one ServicesByNameNS
// bucket. The composition happens HERE, at the single point where the label is
// read, so ids, labels, join keys, indexes, the cluster-family key, cross-cluster
// status and `clusters[]` all inherit it without any downstream consumer being
// taught about zones.
//
// Every cluster name the reader meets — on a topology or kubelet series, on a
// service-graph series' `cluster` label, or arriving from the route store —
// passes through one ladder:
//
//  1. COMPOSE  — the series carries both configured labels non-empty.
//  2. ADOPT    — it does not, but the raw name maps to exactly one identity
//     composed elsewhere in this build.
//  3. VERBATIM — neither; the raw name stands as its own cluster and the sample
//     is tallied for the aggregated cluster_identity_unresolved warning.
//
// Step 2 keeps a partially-stamped estate whole (a kubelet or owner family that
// lacks the pair still joins its cluster whenever the raw name is unambiguous).
// Step 3 is the honest failure: two identities behind one raw name cannot be
// told apart, so nothing is guessed — the series becomes its own visible cluster
// and the warning names the metric.
//
// The resolver also carries the pre-existing per-metric tally of samples whose
// `cluster` label is absent entirely (bucketed to promql.ClusterUnknownValue).
type clusterResolver struct {
	keys promql.LabelKeys

	// identities maps a composed identity to its three components. Populated
	// by observe during the first pass and handed to the built graph, where
	// Graph.ClusterRawName reads the raw component for the projection-level
	// `cluster` filter.
	identities map[string]graph.ClusterIdentity
	// byRaw maps a raw cluster name to every identity composed from it. A
	// single entry is what makes step 2 unambiguous.
	byRaw map[string]map[string]struct{}

	missing    map[promql.Query]int
	unresolved map[promql.Query]int
}

func newClusterResolver(keys promql.LabelKeys) *clusterResolver {
	return &clusterResolver{
		keys:       keys.OrDefault(),
		identities: map[string]graph.ClusterIdentity{},
		byRaw:      map[string]map[string]struct{}{},
		missing:    map[promql.Query]int{},
		unresolved: map[promql.Query]int{},
	}
}

// compose applies step 1 only: the identity a metric's own labels spell out, or
// "" when it carries less than the full pair. The raw name is already bucketed,
// so a cluster-less series with the pair composes to `<az>-<env>-unknown` — the
// bucket keeps its raw component and stays addressable as `?cluster=unknown`.
func (r *clusterResolver) compose(m model.Metric) (identity string, ci graph.ClusterIdentity, ok bool) {
	az, env := string(m[model.LabelName(r.keys.AZ)]), string(m[model.LabelName(r.keys.Env)])
	if az == "" || env == "" {
		return "", graph.ClusterIdentity{}, false
	}
	raw := bucketCluster(string(m["cluster"]))
	ci = graph.ClusterIdentity{AZ: az, Env: env, Name: raw}
	return az + "-" + env + "-" + raw, ci, true
}

// observe records a step-1 composition into the identity table. It is the FIRST
// pass of parseTopology and runs only over the four families that mint
// cluster-labelled entities (pods, nodes, services, PVC bindings): every other
// family resolves THROUGH the table and must not be able to invent a cluster
// that holds no entity.
func (r *clusterResolver) observe(m model.Metric) {
	identity, ci, ok := r.compose(m)
	if !ok {
		return
	}
	r.identities[identity] = ci
	if r.byRaw[ci.Name] == nil {
		r.byRaw[ci.Name] = map[string]struct{}{}
	}
	r.byRaw[ci.Name][identity] = struct{}{}
}

// identify runs the full ladder over one series' labels WITHOUT tallying. It is
// the resolution the route prescan uses: the prescan re-reads the same vector
// the parse reads, so tallying there would double every count.
func (r *clusterResolver) identify(m model.Metric) string {
	if identity, _, ok := r.compose(m); ok {
		return identity
	}
	raw := bucketCluster(string(m["cluster"]))
	if identity, ok := r.adopt(raw); ok {
		return identity
	}
	return raw
}

// bucket resolves one sample's cluster identity, tallying a missing `cluster`
// label (pre-existing behaviour) and an unresolvable name (step 3).
func (r *clusterResolver) bucket(metric promql.Query, m model.Metric) string {
	raw := string(m["cluster"])
	if raw == "" {
		r.missing[metric]++
	}
	identity := r.identify(m)
	// Only a build that composed SOMETHING can meaningfully call a name
	// unresolved: with no identity table at all every series is verbatim by
	// construction (the wholly unstamped estate), which is the unchanged
	// pre-identity behaviour and not a condition to warn about.
	if identity == bucketCluster(raw) && len(r.identities) > 0 {
		r.unresolved[metric]++
	}
	return identity
}

// adopt applies step 2 to a raw name: the sole identity composed from it, if
// exactly one exists.
func (r *clusterResolver) adopt(raw string) (string, bool) {
	ids := r.byRaw[raw]
	if len(ids) != 1 {
		return "", false
	}
	for identity := range ids {
		return identity, true
	}
	return "", false
}

// resolveForeign resolves a cluster name that arrives with no labels of its own
// — a route-store destination or ingress cluster. Steps 2 and 3 only: there is
// nothing to compose from, so a name is adopted when unambiguous and otherwise
// stands verbatim (and then simply fails the topology lookup its caller makes,
// degrading through that caller's existing miss path). No tally: this is not a
// metric sample, and its caller logs its own outcome.
func (r *clusterResolver) resolveForeign(raw string) string {
	if raw == "" {
		return raw
	}
	if _, known := r.identities[raw]; known {
		return raw
	}
	if identity, ok := r.adopt(raw); ok {
		return identity
	}
	return raw
}

// snapshot returns the identity table for the built graph. Nil when nothing
// composed, so a graph built from an unstamped estate carries no table and
// Graph.ClusterRawName degrades every value to itself.
func (r *clusterResolver) snapshot() map[string]graph.ClusterIdentity {
	if len(r.identities) == 0 {
		return nil
	}
	return maps.Clone(r.identities)
}

// warn emits one aggregated warning per affected metric — not one per sample, so
// a whole cluster missing a label cannot flood the log — letting operators spot
// which exporter feeds the "unknown" cluster and which one cannot be placed.
// Sorted iteration keeps the log order deterministic.
func (r *clusterResolver) warn() {
	for _, q := range slices.Sorted(maps.Keys(r.missing)) {
		slog.Warn(`samples missing cluster label; bucketed into "unknown" cluster`,
			"metric", string(q),
			"samples", r.missing[q],
		)
	}
	for _, q := range slices.Sorted(maps.Keys(r.unresolved)) {
		slog.Warn("cluster_identity_unresolved",
			"metric", string(q),
			"samples", r.unresolved[q],
			"detail", "series carries no az/env pair and its cluster name maps to no single identity; it stands as its own cluster and joins nothing",
		)
	}
}

// bucketCluster returns the missing-cluster bucket name when the upstream
// cluster label is absent. The name is promql.ClusterUnknownValue rather than a
// local literal because the query layer renders the SAME value as a request
// matcher: rename it on one side only and `?cluster=<bucket>` silently stops
// addressing the bucket it names. The bucket is the identity's RAW component,
// so it composes like any other name (`us-dev-unknown`) and stays addressable.
func bucketCluster(c string) string {
	if c == "" {
		return promql.ClusterUnknownValue
	}
	return c
}

// topologyClusterResolver returns the resolver a Topology was parsed with, or a
// fresh empty one when the Topology was constructed by hand (a unit test, an
// embedder assembling a fixture). An empty resolver composes from a series' own
// labels and otherwise stands every name verbatim, which is exactly the
// pre-identity behaviour — so a hand-built Topology is never a nil dereference
// and never a silent behaviour change.
func topologyClusterResolver(t Topology) *clusterResolver {
	if t.clusters != nil {
		return t.clusters
	}
	return newClusterResolver(promql.LabelKeys{})
}
