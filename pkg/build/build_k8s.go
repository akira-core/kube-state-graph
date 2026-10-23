package build

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/common/model"

	"github.com/akira-core/kube-state-graph/pkg/graph"
	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// BuildK8s builds the Kubernetes-only graph served by GET /v1/k8s-graph.
//
// It is buildStorage under k8sPlan: the storage build's topology fan-out for a
// pod or node root, so the seeds, the incarnation and mounter completion, the chunk
// caps, the routing and the fail-closed rule are the storage build's own — but
// no Harvest wave is launched, and the service-side, container and
// service-graph families are never issued. The topology is assembled with
// assemble, which with no Harvest and no service graph emits only
// pod-to-node and pod-mounts-pvc edges; which of those are drawn is
// graph.ProjectK8s's concern.
//
// Only the pod and node kinds are accepted, and only with at least one value:
// any other root reaches a seed whose purpose is a Harvest read. Anything else
// is ReasonInvalidScope before a querier is bound.
//
// Like BuildStorage it issues no up{} probe and classifies nothing as
// outside_retention (an empty result is an empty 200), and it does not write
// the last-build gauges: the adapters reset each vector on write, so a k8s
// build would erase the /v1/graph series.
func (b *Builder) BuildK8s(ctx context.Context, window time.Duration, end time.Time, sel promql.Selector, roots graph.StorageRoots) (*graph.Graph, error) {
	if !k8sRoots(roots) {
		return nil, NewError(ReasonInvalidScope, "k8s graph requires a pod or node root", nil)
	}
	return b.buildStorage(ctx, window, end, sel, k8sPlan(roots))
}

// refuseHarvest wraps the querier a Kubernetes-only build reads through so
// that any Harvest query fails the build instead of reaching a store. The plan
// already launches no Harvest read (topologyPlan.kubernetesOnly); this is the
// one place that guarantee is enforced, so a Harvest reader added to
// readTopology without that gate turns every k8s build red rather than quietly
// querying the harvest backends — and failing closed whenever they are down.
//
// The wrapper keeps the optional promql.FamilyServer upgrade of the querier it
// wraps, and only it: readTopology consults that to decide whether the
// application-metadata family is served.
func refuseHarvest(q promql.Querier) promql.Querier {
	r := harvestRefuser{q}
	if fs, ok := q.(promql.FamilyServer); ok {
		return harvestRefuserServer{r, fs}
	}
	return r
}

type harvestRefuser struct{ q promql.Querier }

func (r harvestRefuser) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	if fam, _ := promql.FamilyOf(promql.Query(name)); fam == promql.FamilyHarvest {
		return nil, fmt.Errorf("harvest query %s issued by a Kubernetes-only build", name)
	}
	return r.q.Instant(ctx, name, query, ts)
}

type harvestRefuserServer struct {
	harvestRefuser
	promql.FamilyServer
}

// k8sRoots reports whether roots names a kind the Kubernetes-only build reads —
// pod or node — with at least one usable value. An empty set would leave the
// plan with no seed and read the inventory across the whole zone.
//
// Every pod ref must carry both a namespace and a name, as the parser's
// `pod=<ns>/<name>` does: a ref with no namespace would read its name across
// every namespace and still match no pod in the projection, so it is rejected
// rather than answered with an empty 200.
func k8sRoots(roots graph.StorageRoots) bool {
	switch roots.Kind {
	case graph.StorageRootPod:
		for _, ref := range roots.Pods {
			if ref.Namespace == "" || ref.Name == "" {
				return false
			}
		}
		return len(roots.Pods) > 0
	case graph.StorageRootNode:
		return len(sortedNames(roots.Names)) > 0
	default:
		return false
	}
}
